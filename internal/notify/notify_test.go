package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a fake service that records requests.
type recorder struct {
	mu   sync.Mutex
	reqs []recorded
	resp func(r *http.Request, body []byte) (int, string)
}

type recorded struct {
	Path, Auth, Type string
	Body             []byte
}

func newRecorder(t *testing.T, resp func(r *http.Request, body []byte) (int, string)) (*recorder, *httptest.Server) {
	rec := &recorder{resp: resp}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, recorded{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), b})
		rec.mu.Unlock()
		code, body := 200, `{"ok":true,"result":{"message_id":7},"status":1}`
		if rec.resp != nil {
			code, body = rec.resp(r, b)
		}
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

func TestNtfy(t *testing.T) {
	rec, srv := newRecorder(t, nil)
	ch := Channel{Name: "n", Type: TypeNtfy, Server: srv.URL, Topic: "catflap-x", Token: "tk_secret"}
	if err := ch.Send(context.Background(), Message{Title: "new device: ä", Body: "b", Priority: High}); err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	var got map[string]any
	json.Unmarshal(r.Body, &got)
	if r.Path != "/" || r.Auth != "Bearer tk_secret" || got["topic"] != "catflap-x" || got["title"] != "new device: ä" || got["priority"] != float64(4) {
		t.Fatalf("unexpected request: %s %q %v", r.Path, r.Auth, got)
	}
	// No token, no Authorization header; HTTP errors are reported.
	rec2, srv2 := newRecorder(t, func(*http.Request, []byte) (int, string) { return 403, "forbidden" })
	ch = Channel{Name: "n", Type: TypeNtfy, Server: srv2.URL, Topic: "t"}
	if err := ch.Send(context.Background(), Message{Title: "x"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want HTTP 403 error, got %v", err)
	}
	if rec2.all()[0].Auth != "" {
		t.Fatal("sent an Authorization header without a token")
	}
}

func TestPushover(t *testing.T) {
	rec, srv := newRecorder(t, nil)
	ch := Channel{Name: "p", Type: TypePushover, Token: "apptoken", User: "userkey", APIURL: srv.URL}
	if err := ch.Send(context.Background(), Message{Title: "t", Body: "b", Priority: High}); err != nil {
		t.Fatal(err)
	}
	f, _ := url.ParseQuery(string(rec.all()[0].Body))
	if f.Get("token") != "apptoken" || f.Get("user") != "userkey" || f.Get("priority") != "1" || f.Get("title") != "t" {
		t.Fatalf("unexpected form: %v", f)
	}
	_, bad := newRecorder(t, func(*http.Request, []byte) (int, string) {
		return 400, `{"status":0,"errors":["application token is invalid"]}`
	})
	ch.APIURL = bad.URL
	if err := ch.Send(context.Background(), Message{Title: "t"}); err == nil || !strings.Contains(err.Error(), "application token is invalid") {
		t.Fatalf("want Pushover's error text, got %v", err)
	}
}

func TestTelegram(t *testing.T) {
	rec, srv := newRecorder(t, nil)
	ch := Channel{Name: "t", Type: TypeTelegram, Token: "123:SECRET", ChatID: 42, APIURL: srv.URL}
	if err := ch.Send(context.Background(), Message{Title: "a<b", Body: "c&d", Priority: High}); err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	var got map[string]any
	json.Unmarshal(r.Body, &got)
	if r.Path != "/bot123:SECRET/sendMessage" || got["chat_id"] != float64(42) || got["parse_mode"] != "HTML" ||
		got["text"] != "⚠️ <b>a&lt;b</b>\nc&amp;d" {
		t.Fatalf("unexpected request: %s %v", r.Path, got)
	}

	// API errors keep their code (409 = someone else polls the bot) and
	// never leak the token, not even when the connection fails.
	_, conflict := newRecorder(t, func(*http.Request, []byte) (int, string) {
		return 409, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`
	})
	tg := &Telegram{Token: "123:SECRET", APIURL: conflict.URL}
	_, err := tg.GetUpdates(context.Background(), 0, 0)
	var te *TelegramError
	if !errors.As(err, &te) || !te.Conflict() {
		t.Fatalf("want a conflict error, got %v", err)
	}
	tg.APIURL = "http://127.0.0.1:1"
	if _, err := tg.GetMe(context.Background()); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error must not contain the token: %v", err)
	}
}

func TestCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	out := filepath.Join(t.TempDir(), "out")
	ch := Channel{Name: "c", Type: TypeCommand, Command: []string{"sh", "-c", `{ echo "$CATFLAP_EVENT|$CATFLAP_PRIORITY|$CATFLAP_TITLE"; cat; } > "$0"`, out}}
	if err := ch.Send(context.Background(), Message{Event: "device.enrolled", Title: "new device: x", Body: "body", Priority: High}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if want := "device.enrolled|high|new device: x\nnew device: x\n\nbody\n"; string(b) != want {
		t.Fatalf("got %q, want %q", b, want)
	}
	fail := Channel{Name: "c", Type: TypeCommand, Command: []string{"sh", "-c", "echo nope >&2; exit 3"}}
	if err := fail.Send(context.Background(), Message{}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want the command's output in the error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	good := []Channel{
		{Name: "ntfy", Type: TypeNtfy, Topic: "catflap-abc"},
		{Name: "phone-2", Type: TypePushover, Token: "a", User: "u"},
		{Name: "tg", Type: TypeTelegram, Token: "1:x", ChatID: 5, Bot: true},
		{Name: "mail", Type: TypeCommand, Command: []string{"mail"}},
	}
	for _, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := []Channel{
		{Name: "Bad", Type: TypeNtfy, Topic: "t"},
		{Name: "n", Type: TypeNtfy},
		{Name: "n", Type: TypeNtfy, Topic: "a/b"},
		{Name: "n", Type: TypeNtfy, Topic: "t", Server: "ftp://x"},
		{Name: "p", Type: TypePushover, Token: "a"},
		{Name: "t", Type: TypeTelegram, Token: "1:x"},
		{Name: "c", Type: TypeCommand},
		{Name: "x", Type: "sms"},
		{Name: "n", Type: TypeNtfy, Topic: "t", Bot: true},
	}
	for _, c := range bad {
		if c.Validate() == nil {
			t.Errorf("%+v: accepted", c)
		}
	}
	if d := (Channel{Type: TypePushover, User: "uQiRzpo4DXghDmr9QzzfQu27cmVRsG", Token: "secret"}).Describe(); strings.Contains(d, "secret") || strings.Contains(d, "QzzfQu") {
		t.Errorf("Describe leaks secrets: %q", d)
	}
}

func TestDispatcher(t *testing.T) {
	retryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	var mu sync.Mutex
	fails := 2 // the first two attempts fail, the third gets through
	rec, srv := newRecorder(t, func(*http.Request, []byte) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return 500, "down"
		}
		return 200, "{}"
	})
	other, srv2 := newRecorder(t, nil)
	d := NewDispatcher(t.Logf)
	defer d.Close()
	d.Set([]Channel{
		{Name: "a", Type: TypeNtfy, Server: srv.URL, Topic: "t"},
		{Name: "bot", Type: TypeNtfy, Server: srv2.URL, Topic: "t"},
	})
	d.Send(Message{Title: "x"}, "bot") // came from "bot": skip it
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.all()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(rec.all()); n != 3 {
		t.Fatalf("want 3 attempts on channel a, got %d", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(other.all()); n != 0 {
		t.Fatalf("the origin channel got %d message(s)", n)
	}
}

func TestLoadSave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notify.json")
	c, err := Load(p)
	if err != nil || len(c.Channels) != 0 {
		t.Fatalf("missing file: %v %v", c, err)
	}
	c.Channels = []Channel{{Name: "n", Type: TypeNtfy, Topic: "t", Token: "secret"}}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 { // no Unix modes on Windows
		t.Fatalf("mode %v, want 600", fi.Mode().Perm())
	}
	if c2, err := Load(p); err != nil || c2.Channels[0].Token != "secret" {
		t.Fatalf("reload: %v %v", c2, err)
	}
}
