package homelab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/PaddeK/catflap/internal/notify"
)

// Notifications: security alerts go to every configured channel (see
// `catflap notify`). A request that came from a channel (the Telegram bot)
// is answered there, so its alert skips that channel.

// alert sends m to every channel except origin ("" = command line).
func (d *Daemon) alert(m notify.Message, origin string) {
	m.Title = d.cfg.Name + ": " + m.Title
	d.disp.Send(m, origin)
}

// via describes where a request came from, for alert texts.
func via(origin string) string {
	if origin == "" {
		return "on the homelab"
	}
	return "via the Telegram bot"
}

// removeHint tells how to undo an unwanted enrollment.
func (d *Daemon) removeHint(name string) string {
	s := "catflap remove " + name
	if d.currentBot() != nil {
		s += " (or /remove " + name + " in Telegram)"
	}
	return s
}

// ---- channel config ----

func (d *Daemon) loadNotify(ctx context.Context) error {
	cfg, err := notify.Load(d.paths.Notify)
	if err != nil {
		return err
	}
	d.notifyMu.Lock()
	defer d.notifyMu.Unlock()
	d.ncfg = cfg
	d.applyNotifyLocked(ctx)
	return nil
}

// applyNotifyLocked (re)starts the dispatcher's channels and the bot.
func (d *Daemon) applyNotifyLocked(ctx context.Context) {
	d.disp.Set(d.ncfg.Channels)
	var want *notify.Channel
	for i := range d.ncfg.Channels {
		if d.ncfg.Channels[i].Bot {
			want = &d.ncfg.Channels[i]
			break
		}
	}
	if d.bot != nil && (want == nil || !sameJSON(d.bot.ch, *want)) {
		d.botStop()
		d.bot, d.botStop = nil, nil
	}
	if d.bot == nil && want != nil {
		bctx, cancel := context.WithCancel(ctx)
		d.bot, d.botStop = newBot(d, *want), cancel
		go d.bot.run(bctx)
	}
}

func (d *Daemon) currentBot() *bot {
	d.notifyMu.Lock()
	defer d.notifyMu.Unlock()
	return d.bot
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// ChannelInfo is a listed channel (no secrets).
type ChannelInfo struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Bot    bool   `json:"bot"`
}

// TestResult is the outcome per channel of `catflap notify test`.
type TestResult struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

func (d *Daemon) notifyRoutes(ctx context.Context, mux *http.ServeMux) {
	mux.HandleFunc("GET /notify", func(w http.ResponseWriter, r *http.Request) {
		d.notifyMu.Lock()
		out := []ChannelInfo{}
		for _, c := range d.ncfg.Channels {
			out = append(out, ChannelInfo{c.Name, c.Type, c.Describe(), c.Bot})
		}
		d.notifyMu.Unlock()
		ctlJSON(w, out)
	})
	mux.HandleFunc("PUT /notify/{name}", func(w http.ResponseWriter, r *http.Request) {
		var c notify.Channel
		if json.NewDecoder(r.Body).Decode(&c) != nil {
			ctlErr(w, http.StatusBadRequest, "bad request")
			return
		}
		c.Name = r.PathValue("name")
		if err := c.Validate(); err != nil {
			ctlErr(w, http.StatusBadRequest, err.Error())
			return
		}
		d.notifyMu.Lock()
		defer d.notifyMu.Unlock()
		for _, x := range d.ncfg.Channels {
			if x.Name == c.Name {
				ctlErr(w, http.StatusConflict, fmt.Sprintf("a channel named %q exists already (remove it first, or pick another --name)", c.Name))
				return
			}
			if c.Bot && x.Bot {
				ctlErr(w, http.StatusConflict, fmt.Sprintf("channel %q runs the bot already; only one channel can", x.Name))
				return
			}
		}
		// A channel only goes live after a test message got through.
		body := fmt.Sprintf("This channel now gets security alerts from homelab %q.", d.cfg.Name)
		if c.Bot {
			body += " Send /help to see what the bot can do."
		}
		tctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		err := c.Send(tctx, notify.Message{Event: "notify.test", Title: d.cfg.Name + ": catflap notifications are on", Body: body})
		cancel()
		if err != nil {
			ctlErr(w, http.StatusBadRequest, "test message failed, channel not added: "+err.Error())
			return
		}
		next := notify.Config{Channels: append(slices.Clone(d.ncfg.Channels), c)}
		if err := notify.Save(d.paths.Notify, next); err != nil {
			ctlErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		d.ncfg = next
		d.applyNotifyLocked(ctx)
		d.logf("notify: added channel %q (%s)", c.Name, c.Type)
		ctlJSON(w, ChannelInfo{c.Name, c.Type, c.Describe(), c.Bot})
	})
	mux.HandleFunc("DELETE /notify/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		d.notifyMu.Lock()
		defer d.notifyMu.Unlock()
		i := slices.IndexFunc(d.ncfg.Channels, func(c notify.Channel) bool { return c.Name == name })
		if i < 0 {
			ctlErr(w, http.StatusBadRequest, fmt.Sprintf("no channel named %q", name))
			return
		}
		next := notify.Config{Channels: slices.Delete(slices.Clone(d.ncfg.Channels), i, i+1)}
		if err := notify.Save(d.paths.Notify, next); err != nil {
			ctlErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		d.ncfg = next
		d.applyNotifyLocked(ctx)
		d.logf("notify: removed channel %q", name)
		ctlJSON(w, struct{}{})
	})
	mux.HandleFunc("POST /notify/test", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		d.notifyMu.Lock()
		chs := slices.Clone(d.ncfg.Channels)
		d.notifyMu.Unlock()
		out := []TestResult{}
		for _, c := range chs {
			if req.Name != "" && c.Name != req.Name {
				continue
			}
			tctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			err := c.Send(tctx, notify.Message{Event: "notify.test", Title: d.cfg.Name + ": catflap test message", Body: "If you can read this, channel " + c.Name + " works."})
			cancel()
			res := TestResult{Name: c.Name}
			if err != nil {
				res.Error = err.Error()
			}
			out = append(out, res)
		}
		if req.Name != "" && len(out) == 0 {
			ctlErr(w, http.StatusBadRequest, fmt.Sprintf("no channel named %q", req.Name))
			return
		}
		ctlJSON(w, out)
	})
}

// when formats a time for messages: just the clock time if it's today.
func when(t time.Time) string {
	t = t.Local()
	if t.Format("2006-01-02") == time.Now().Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}
