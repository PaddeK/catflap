package homelab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/notify"
)

// fakeService records JSON requests by path (fake Telegram, relay, ntfy).
type fakeService struct {
	mu   sync.Mutex
	reqs []fakeReq
	srv  *httptest.Server
}

type fakeReq struct {
	Path string
	Body map[string]any
}

func newFake(t *testing.T) *fakeService {
	f := &fakeService{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeReq{r.URL.Path, m})
		f.mu.Unlock()
		io.WriteString(w, `{"ok":true,"result":{"message_id":7}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// find returns the requests whose path ends with suffix.
func (f *fakeService) find(suffix string) []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReq
	for _, r := range f.reqs {
		if strings.HasSuffix(r.Path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

// waitFind waits for an async request (alerts go through the dispatcher).
func (f *fakeService) waitFind(t *testing.T, suffix, contains string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range f.find(suffix) {
			b, _ := json.Marshal(r.Body)
			if strings.Contains(string(b), contains) {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (f *fakeService) lastText(t *testing.T) string {
	t.Helper()
	msgs := f.find("/sendMessage")
	if len(msgs) == 0 {
		t.Fatal("the bot sent nothing")
	}
	s, _ := msgs[len(msgs)-1].Body["text"].(string)
	return s
}

func testBot(t *testing.T) (*bot, *fakeService, *fakeService, *fakeService) {
	tg, relay, ntfy := newFake(t), newFake(t), newFake(t)
	rc := api.NewClient("relay.example.com", "token")
	rc.Base = relay.srv.URL
	d := &Daemon{
		cfg:   Config{Name: "lab", Relay: "relay.example.com"},
		paths: Paths{State: filepath.Join(t.TempDir(), "state.json")},
		relay: rc,
		logf:  t.Logf,
		disp:  notify.NewDispatcher(t.Logf),
	}
	t.Cleanup(d.disp.Close)
	tgc := notify.Channel{Name: "tg", Type: notify.TypeTelegram, Token: "1:x", ChatID: 42, Bot: true, BotName: "labbot", APIURL: tg.srv.URL}
	d.disp.Set([]notify.Channel{tgc, {Name: "phone", Type: notify.TypeNtfy, Server: ntfy.srv.URL, Topic: "t"}})
	b := newBot(d, tgc)
	d.bot = b
	return b, tg, relay, ntfy
}

func msg(chat int64, text string, age time.Duration) *notify.TMessage {
	return &notify.TMessage{MessageID: 1, From: &notify.TUser{ID: chat, Username: "u"}, Chat: notify.TChat{ID: chat, Type: "private"},
		Date: time.Now().Add(-age).Unix(), Text: text}
}

func TestBotInvite(t *testing.T) {
	b, tg, relay, ntfy := testBot(t)
	ctx := context.Background()
	b.onMessage(ctx, msg(42, "/invite laptop", 0))

	if len(relay.find("/v1/agent/invites")) != 1 {
		t.Fatal("invite was not registered with the relay")
	}
	if txt := b.d.st.Invites; len(txt) != 1 || txt[0].Name != "laptop" || txt[0].TelegramMessage != 7 {
		t.Fatalf("invite state: %+v", b.d.st.Invites)
	}
	if exp := time.Until(b.d.st.Invites[0].Expires); exp > botInviteTTL || exp < botInviteTTL-time.Minute {
		t.Fatalf("bot invite valid for %v, want %v", exp, botInviteTTL)
	}
	if s := tg.lastText(t); !strings.Contains(s, "<code>catflap enroll cfi1-") {
		t.Fatalf("reply has no invite: %q", s)
	}
	// The alert goes to the other channels, not back to Telegram.
	if !ntfy.waitFind(t, "/", "invite created for laptop") {
		t.Fatal("no alert on the ntfy channel")
	}
	time.Sleep(50 * time.Millisecond)
	for _, r := range tg.find("/sendMessage") {
		if s, _ := r.Body["text"].(string); strings.Contains(s, "invite created") {
			t.Fatal("the alert was echoed to the Telegram chat the request came from")
		}
	}

	// A second invite for the same name is refused with a reason.
	b.onMessage(ctx, msg(42, "/invite laptop", 0))
	if s := tg.lastText(t); !strings.Contains(s, "Could not create the invite") {
		t.Fatalf("want a refusal, got %q", s)
	}
	// /revoke deletes the message with the code.
	b.onMessage(ctx, msg(42, "/revoke laptop", 0))
	if len(b.d.st.Invites) != 0 || !tg.waitFind(t, "/deleteMessage", `"message_id":7`) {
		t.Fatal("revoke did not remove the invite and its message")
	}
}

func TestBotIgnoresStrangers(t *testing.T) {
	b, tg, relay, ntfy := testBot(t)
	ctx := context.Background()
	b.onMessage(ctx, msg(99, "/invite evil", 0))
	b.onMessage(ctx, msg(99, "/list", 0))
	if len(relay.find("/v1/agent/invites")) != 0 || len(b.d.st.Invites) != 0 {
		t.Fatal("a stranger created an invite")
	}
	for _, r := range tg.find("/sendMessage") {
		if r.Body["chat_id"] == float64(99) {
			t.Fatal("the bot answered a stranger")
		}
	}
	if !ntfy.waitFind(t, "/", "someone else wrote to your Telegram bot") {
		t.Fatal("no alert about the stranger")
	}
	time.Sleep(50 * time.Millisecond)
	n := 0
	for _, r := range ntfy.find("/") {
		if s, _ := r.Body["title"].(string); strings.Contains(s, "someone else") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one alert per stranger and hour, got %d", n)
	}
	// Buttons from elsewhere don't work either.
	b.onCallback(ctx, &notify.TCallback{ID: "c", From: notify.TUser{ID: 99}, Message: msg(99, "", 0), Data: "rm:abc"})
	if len(tg.find("/answerCallbackQuery")) != 1 {
		t.Fatal("callback not answered")
	}
}

func TestBotCommands(t *testing.T) {
	b, tg, _, _ := testBot(t)
	ctx := context.Background()
	b.d.st.Devices = []Device{{Name: "phone", Hostname: "iphone", Added: time.Now()}}

	b.onMessage(ctx, msg(42, "/invite laptop", 5*time.Minute))
	if s := tg.lastText(t); !strings.Contains(s, "arrived while the homelab was offline") || len(b.d.st.Invites) != 0 {
		t.Fatalf("an old command was executed: %q", s)
	}
	b.onMessage(ctx, msg(42, "/list@labbot", 0))
	if s := tg.lastText(t); !strings.Contains(s, "<b>phone</b>, hostname iphone") {
		t.Fatalf("list: %q", s)
	}
	b.onMessage(ctx, msg(42, "/remove ghost", 0))
	if s := tg.lastText(t); !strings.Contains(s, "No device named") {
		t.Fatalf("remove unknown: %q", s)
	}
	b.onMessage(ctx, msg(42, "/remove phone", 0))
	last := tg.find("/sendMessage")
	kb, _ := json.Marshal(last[len(last)-1].Body["reply_markup"])
	if !strings.Contains(string(kb), `"callback_data":"rm:`) || !strings.Contains(string(kb), `"callback_data":"no:`) {
		t.Fatalf("no confirm buttons: %s", kb)
	}
	// Cancel keeps the device; a reused or unknown button has expired.
	var nonce string
	for k := range b.pending {
		nonce = k
	}
	b.onCallback(ctx, &notify.TCallback{ID: "c1", From: notify.TUser{ID: 42}, Message: msg(42, "", 0), Data: "no:" + nonce})
	if len(b.d.st.Devices) != 1 || !tg.waitFind(t, "/editMessageText", "Kept") {
		t.Fatal("cancel did not keep the device")
	}
	b.onCallback(ctx, &notify.TCallback{ID: "c2", From: notify.TUser{ID: 42}, Message: msg(42, "", 0), Data: "rm:" + nonce})
	if len(b.d.st.Devices) != 1 || !tg.waitFind(t, "/answerCallbackQuery", "expired") {
		t.Fatal("a used button still worked")
	}
	b.onMessage(ctx, msg(42, "hello", 0))
	if s := tg.lastText(t); !strings.Contains(s, "/invite &lt;name&gt;") {
		t.Fatalf("no help for plain text: %q", s)
	}
}
