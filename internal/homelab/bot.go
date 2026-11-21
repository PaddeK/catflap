package homelab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"
	"github.com/PaddeK/catflap/internal/notify"
)

// The Telegram invite bot: lets you manage devices from your phone when no
// enrolled device is at hand. It long-polls Telegram from the homelab (no
// open port, nothing on the relay) and answers exactly one chat, the one
// linked by `catflap notify add telegram`. Everyone else is ignored, and
// the first message from each stranger per hour triggers an alert.

const (
	botInviteTTL  = 15 * time.Minute // invites through Telegram are short-lived
	botMaxAge     = 2 * time.Minute  // older commands arrived while offline
	botConfirmTTL = 5 * time.Minute  // how long a /remove button works
)

type bot struct {
	d  *Daemon
	ch notify.Channel
	tg *notify.Telegram

	mu        sync.Mutex
	pending   map[string]pendingRemove // confirm-button nonce -> device
	strangers map[int64]time.Time      // last alert per unknown sender
}

type pendingRemove struct {
	name    string
	expires time.Time
}

func newBot(d *Daemon, ch notify.Channel) *bot {
	return &bot{d: d, ch: ch, tg: ch.Telegram(), pending: map[string]pendingRemove{}, strangers: map[int64]time.Time{}}
}

func esc(s string) string { return html.EscapeString(s) }

func (b *bot) run(ctx context.Context) {
	b.d.logf("bot: answering Telegram chat %d", b.ch.ChatID)
	b.setCommands(ctx)
	var offset int64
	backoff, lastErr := 5*time.Second, ""
	for ctx.Err() == nil {
		ups, err := b.tg.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			msg := err.Error()
			var te *notify.TelegramError
			if errors.As(err, &te) && te.Conflict() {
				msg = "another program is polling this bot (is its token used elsewhere?)"
			}
			if msg != lastErr { // don't repeat the same error every retry
				b.d.logf("bot: %s (retrying)", msg)
				lastErr = msg
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 2*time.Minute)
			continue
		}
		if lastErr != "" {
			b.d.logf("bot: Telegram reachable again")
		}
		backoff, lastErr = 5*time.Second, ""
		for _, u := range ups {
			offset = u.UpdateID + 1
			switch {
			case u.Message != nil:
				b.onMessage(ctx, u.Message)
			case u.CallbackQuery != nil:
				b.onCallback(ctx, u.CallbackQuery)
			}
		}
	}
}

// setCommands fills the command menu in the Telegram app.
func (b *bot) setCommands(ctx context.Context) {
	cmds := []map[string]string{
		{"command": "invite", "description": "one-time invite for a new device"},
		{"command": "invites", "description": "open invites"},
		{"command": "revoke", "description": "revoke an open invite"},
		{"command": "list", "description": "enrolled devices"},
		{"command": "remove", "description": "remove a device"},
		{"command": "requests", "description": "let new devices ask to join: on [1h] | off"},
		{"command": "status", "description": "homelab status"},
		{"command": "help", "description": "what this bot does"},
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.tg.SetCommands(cctx, cmds); err != nil {
		b.d.logf("bot: setting the command menu: %v", err)
	}
}

func (b *bot) send(ctx context.Context, text string, kb *notify.Keyboard) int64 {
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := b.tg.SendMessage(sctx, b.ch.ChatID, text, kb)
	if err != nil {
		b.d.logf("bot: sending: %v", err)
	}
	return id
}

func (b *bot) edit(ctx context.Context, msg int64, text string) {
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.tg.EditMessage(sctx, b.ch.ChatID, msg, text); err != nil {
		b.d.logf("bot: editing a message: %v", err)
	}
}

func (b *bot) help() string {
	return fmt.Sprintf(`<b>catflap bot for homelab %s</b>
/invite &lt;name&gt; – one-time invite for a new device (valid %d min)
/invites – open invites
/revoke &lt;name&gt; – revoke an open invite
/list – enrolled devices
/remove &lt;name&gt; – remove a device (asks first)
/requests on [1h] | off – let new devices ask to join (tap to approve)
/status – homelab status`, esc(b.d.cfg.Name), int(botInviteTTL.Minutes()))
}

func (b *bot) onMessage(ctx context.Context, m *notify.TMessage) {
	if m.Chat.ID != b.ch.ChatID || m.From == nil || m.From.IsBot {
		b.stranger(m.From, m.Text)
		return
	}
	text := strings.TrimSpace(m.Text)
	f := strings.Fields(text)
	if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
		b.send(ctx, b.help(), nil)
		return
	}
	cmd, _, _ := strings.Cut(strings.ToLower(f[0]), "@") // "/list@mybot" in menus
	args := f[1:]
	if sent := time.Unix(m.Date, 0); time.Since(sent) > botMaxAge {
		b.send(ctx, fmt.Sprintf("Ignored <code>%s</code> from %s: it arrived while the homelab was offline. Send it again if you still want it.",
			esc(text), sent.Local().Format("15:04")), nil)
		return
	}
	one := func(usage string) (string, bool) {
		if len(args) != 1 {
			b.send(ctx, "Usage: <code>"+esc(usage)+"</code>", nil)
			return "", false
		}
		return strings.ToLower(args[0]), true
	}
	switch cmd {
	case "/start", "/help":
		b.send(ctx, b.help(), nil)
	case "/invite":
		if name, ok := one("/invite <name>"); ok {
			b.invite(ctx, name)
		}
	case "/invites":
		b.listInvites(ctx)
	case "/revoke":
		if name, ok := one("/revoke <name>"); ok {
			if err := b.d.revokeInvite(ctx, name); err != nil {
				b.send(ctx, esc(err.Error()), nil)
			} else {
				b.send(ctx, "Revoked the invite for <b>"+esc(name)+"</b>.", nil)
			}
		}
	case "/list", "/devices":
		b.listDevices(ctx)
	case "/remove":
		if name, ok := one("/remove <name>"); ok {
			b.askRemove(ctx, name)
		}
	case "/status":
		b.status(ctx)
	case "/requests":
		b.requests(ctx, args)
	default:
		b.send(ctx, "Unknown command.\n\n"+b.help(), nil)
	}
}

func (b *bot) invite(ctx context.Context, name string) {
	res, err := b.d.createInvite(ctx, name, botInviteTTL, b.ch.Name, false)
	if err != nil {
		b.send(ctx, "Could not create the invite: "+esc(err.Error()), nil)
		return
	}
	id := b.send(ctx, fmt.Sprintf(`Invite for <b>%s</b>, one use, valid until %s:

<code>catflap enroll %s</code>

Tap the line to copy it, then run it on the new device. Telegram can read this chat, so the invite is short-lived, and this message goes away once it's used or expired.`,
		esc(name), when(res.Expires), esc(res.Code)), nil)
	if id == 0 {
		// Couldn't deliver it: nobody can use it, so don't leave it open.
		b.d.revokeInvite(ctx, name)
		return
	}
	b.d.setInviteMessage(name, id)
}

// postReenroll posts a re-enroll invite (from `homelab move`) to the chat.
func (b *bot) postReenroll(ctx context.Context, inv Invite, code string) int64 {
	return b.send(ctx, fmt.Sprintf(`Re-enroll invite for <b>%s</b> (the homelab moved to relay %s), one use, valid until %s:

<code>catflap enroll %s</code>

Run it on that device. This message goes away once it's used or expired.`,
		esc(inv.Name), esc(b.d.cfg.Relay), when(inv.Expires), esc(code)), nil)
}

// inviteUsed replaces the code with a note once a device enrolled.
func (b *bot) inviteUsed(msg int64, name, hostname string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := ""
	if hostname != "" {
		host = " (hostname " + esc(hostname) + ")"
	}
	b.edit(ctx, msg, fmt.Sprintf("✓ The invite for <b>%s</b> was used: the device%s is enrolled.", esc(name), host))
}

// inviteGone removes the code once the invite expired or was revoked.
func (b *bot) inviteGone(msg int64, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.tg.DeleteMessage(ctx, b.ch.ChatID, msg); err != nil {
		// Bots can delete only for 48 hours; at least remove the code.
		b.edit(ctx, msg, "The invite for <b>"+esc(name)+"</b> expired or was revoked.")
	}
}

func (b *bot) listInvites(ctx context.Context) {
	b.d.mu.Lock()
	var lines []string
	for _, inv := range b.d.st.Invites {
		if inv.Used == nil && time.Now().Before(inv.Expires) {
			lines = append(lines, fmt.Sprintf("• %s, until %s", esc(inv.Name), inv.Expires.Local().Format("2006-01-02 15:04")))
		}
	}
	b.d.mu.Unlock()
	if len(lines) == 0 {
		b.send(ctx, "No open invites.", nil)
		return
	}
	b.send(ctx, "Open invites:\n"+strings.Join(lines, "\n"), nil)
}

func (b *bot) listDevices(ctx context.Context) {
	devs := b.d.deviceInfos()
	if len(devs) == 0 {
		b.send(ctx, "No devices yet. Add one with /invite &lt;name&gt;.", nil)
		return
	}
	lines := []string{fmt.Sprintf("Devices of homelab <b>%s</b>:", esc(b.d.cfg.Name))}
	for _, dev := range devs {
		host := ""
		if dev.Hostname != "" {
			host = ", hostname " + esc(dev.Hostname)
		}
		seen := "last seen " + dev.LastSeenText(time.Now())
		if dev.Active {
			seen = "connected " + dev.LastSeenText(time.Now())
		}
		lines = append(lines, fmt.Sprintf("• <b>%s</b>%s, %s, enrolled %s", esc(dev.Name), host, seen, dev.Added.Local().Format("2006-01-02")))
	}
	b.send(ctx, strings.Join(lines, "\n"), nil)
}

func (b *bot) status(ctx context.Context) {
	connected := len(b.d.refreshActivity())
	b.d.mu.Lock()
	devs, invs, sync, latest := len(b.d.st.Devices), 0, b.d.st.NeedsSync, b.d.latest
	for _, inv := range b.d.st.Invites {
		if inv.Used == nil {
			invs++
		}
	}
	b.d.mu.Unlock()
	var fwd []string
	for _, f := range b.d.forwardList() {
		fwd = append(fwd, fmt.Sprintf("%d→%s", f.Port, f.Target))
	}
	relay := "in sync"
	if sync {
		relay = "sync pending (relay unreachable?)"
	}
	update := ""
	if latest != "" {
		update = fmt.Sprintf("\nUpdate available: catflap %s (running %s)", esc(latest), esc(api.Version))
	}
	b.send(ctx, fmt.Sprintf("Homelab <b>%s</b> via %s\n%d device(s), %d connected now, %d open invite(s)\nForwarding: %s\nRelay: %s%s",
		esc(b.d.cfg.Name), esc(b.d.cfg.Relay), devs, connected, invs, esc(strings.Join(fwd, ", ")), relay, update), nil)
}

func (b *bot) askRemove(ctx context.Context, name string) {
	b.d.mu.Lock()
	ok := slices.ContainsFunc(b.d.st.Devices, func(dev Device) bool { return dev.Name == name })
	b.d.mu.Unlock()
	if !ok {
		b.send(ctx, fmt.Sprintf("No device named <b>%s</b>. See /list.", esc(name)), nil)
		return
	}
	var r [8]byte
	rand.Read(r[:])
	nonce := hex.EncodeToString(r[:])
	b.mu.Lock()
	for k, p := range b.pending {
		if time.Now().After(p.expires) {
			delete(b.pending, k)
		}
	}
	b.pending[nonce] = pendingRemove{name: name, expires: time.Now().Add(botConfirmTTL)}
	b.mu.Unlock()
	b.send(ctx, fmt.Sprintf("Remove <b>%s</b>? It loses access immediately.", esc(name)), &notify.Keyboard{Rows: [][]notify.Button{{
		{Text: "Remove " + name, Data: "rm:" + nonce},
		{Text: "Cancel", Data: "no:" + nonce},
	}}})
}

func (b *bot) onCallback(ctx context.Context, cb *notify.TCallback) {
	answer := func(text string) {
		actx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		b.tg.AnswerCallback(actx, cb.ID, text)
	}
	if cb.Message == nil || cb.Message.Chat.ID != b.ch.ChatID || cb.From.IsBot {
		b.stranger(&cb.From, "(tapped a button)")
		answer("")
		return
	}
	kind, nonce, _ := strings.Cut(cb.Data, ":")
	if kind == "ra" || kind == "rd" { // enrollment request: approve / deny
		b.onRequestButton(ctx, cb, kind == "ra", nonce, answer)
		return
	}
	b.mu.Lock()
	p, ok := b.pending[nonce]
	delete(b.pending, nonce)
	b.mu.Unlock()
	switch {
	case !ok || time.Now().After(p.expires):
		answer("This button has expired.")
		b.edit(ctx, cb.Message.MessageID, "Expired. Send /remove again if you still want to.")
	case kind == "no":
		answer("")
		b.edit(ctx, cb.Message.MessageID, "Kept <b>"+esc(p.name)+"</b>.")
	case kind == "rm":
		res, err := b.d.removeDevice(ctx, p.name, b.ch.Name)
		if err != nil {
			answer("Failed")
			b.edit(ctx, cb.Message.MessageID, "Could not remove <b>"+esc(p.name)+"</b>: "+esc(err.Error()))
			return
		}
		answer("Removed")
		s := "Removed <b>" + esc(p.name) + "</b>: disconnected"
		if res.SSHKeyRemoved {
			s += ", SSH key removed"
		}
		if res.RelayUpdated {
			s += ", relay updated."
		} else {
			s += "; the relay is unreachable right now, the homelab keeps retrying."
		}
		b.edit(ctx, cb.Message.MessageID, s)
	default:
		answer("")
	}
}

// requests handles /requests [on [duration] | off].
func (b *bot) requests(ctx context.Context, args []string) {
	switch {
	case len(args) == 0:
		info := b.d.requestsInfo()
		s := "Enrollment requests are <b>off</b>. Open them with /requests on (1 hour) or /requests on 30m."
		if time.Now().Before(info.Until) {
			s = fmt.Sprintf("Enrollment requests are <b>open</b> until %s. New devices can run:\n<code>catflap enroll --request %s@%s</code>\nClose with /requests off.", when(info.Until), esc(b.d.cfg.Name), esc(b.d.cfg.Relay))
		}
		for _, p := range info.Pending {
			s += fmt.Sprintf("\n• %s from %s", esc(p.Name), esc(p.Source))
		}
		b.send(ctx, s, nil)
	case args[0] == "off":
		if _, err := b.d.setRequestWindow(ctx, 0, b.ch.Name); err != nil {
			b.send(ctx, esc(err.Error()), nil)
			return
		}
		b.send(ctx, "Enrollment requests are closed.", nil)
	case args[0] == "on":
		dur := DefaultRequestWindow
		if len(args) > 1 {
			v, err := ParseDuration(args[1])
			if err != nil || v <= 0 || v > 7*24*time.Hour {
				b.send(ctx, "Usage: <code>/requests on 30m</code> (up to 7d)", nil)
				return
			}
			dur = v
		}
		until, err := b.d.setRequestWindow(ctx, dur, b.ch.Name)
		if err != nil {
			b.send(ctx, esc(err.Error()), nil)
			return
		}
		b.send(ctx, fmt.Sprintf("Enrollment requests are open until %s. On the new device run:\n<code>catflap enroll --request %s@%s</code>\nYou'll get a message here to approve it.", when(until), esc(b.d.cfg.Name), esc(b.d.cfg.Relay)), nil)
	default:
		b.send(ctx, "Usage: <code>/requests</code>, <code>/requests on [30m]</code> or <code>/requests off</code>", nil)
	}
}

// postRequest asks the user to approve a request; returns the message id.
func (b *bot) postRequest(ctx context.Context, p *pendingRequest) int64 {
	host := ""
	if p.hostname != "" {
		host = " (hostname " + esc(p.hostname) + ")"
	}
	return b.send(ctx, fmt.Sprintf("<b>%s</b>%s from %s asks to join homelab %s.\n\nCode: <b>%s</b>\n\nApprove only if the new device shows exactly this code.",
		esc(p.Req.Name), host, esc(p.Source), esc(b.d.cfg.Name), codes.FormatPairingCode(p.Code)),
		&notify.Keyboard{Rows: [][]notify.Button{{
			{Text: "Approve " + p.Req.Name, Data: "ra:" + p.ID},
			{Text: "Deny", Data: "rd:" + p.ID},
		}}})
}

func (b *bot) onRequestButton(ctx context.Context, cb *notify.TCallback, approve bool, id string, answer func(string)) {
	var name string
	var err error
	if approve {
		name, err = b.d.approveRequest(ctx, id, b.ch.Name)
	} else {
		name, err = b.d.denyRequest(ctx, id, b.ch.Name)
	}
	if err != nil {
		answer("Failed")
		b.edit(ctx, cb.Message.MessageID, esc(err.Error()))
		return
	}
	if approve {
		answer("Approved " + name)
	} else {
		answer("Denied")
	}
}

// requestDecided replaces the request message (approved, denied, expired).
func (b *bot) requestDecided(msg int64, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b.edit(ctx, msg, text)
}

// stranger alerts (once per sender per hour) about someone else using the bot.
func (b *bot) stranger(from *notify.TUser, text string) {
	var id int64
	if from != nil {
		id = from.ID
	}
	b.mu.Lock()
	if time.Since(b.strangers[id]) < time.Hour {
		b.mu.Unlock()
		return
	}
	b.strangers[id] = time.Now()
	b.mu.Unlock()
	if len(text) > 100 {
		text = text[:100] + "…"
	}
	b.d.logf("bot: ignored a message from %s", from.Label())
	b.d.alert(notify.Message{
		Event:    "bot.stranger",
		Title:    "someone else wrote to your Telegram bot",
		Body:     fmt.Sprintf("%s sent %q to @%s. catflap ignored it: the bot only answers your chat.", from.Label(), text, b.ch.BotName),
		Priority: notify.High,
	}, "")
}
