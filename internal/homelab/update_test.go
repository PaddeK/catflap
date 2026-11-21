package homelab

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/notify"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.6.0", "0.5.1", true},
		{"0.5.10", "0.5.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.5.1", "0.5.1", false},
		{"0.5.0", "0.5.1", false},
		{"v0.6.0", "0.5.1", true},
		{"0.6.0", "dev", false},
		{"0.6.0", "0.6.0-dev", false},
		{"0.6", "0.5.1", false},
		{"0.06.0", "0.5.1", false},
	} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestLatestRelease(t *testing.T) {
	body := `{"tag_name":"v0.6.0","html_url":"https://github.com/PaddeK/catflap/releases/tag/v0.6.0","draft":false,"prerelease":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "catflap/"+api.Version {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()
	rel, err := latestRelease(context.Background(), srv.URL)
	if err != nil || rel.Version != "0.6.0" || rel.URL != "https://github.com/PaddeK/catflap/releases/tag/v0.6.0" {
		t.Fatalf("got %+v, %v", rel, err)
	}
	body = `{"tag_name":"nightly","html_url":"x"}`
	if _, err := latestRelease(context.Background(), srv.URL); err == nil {
		t.Error("accepted a tag that isn't a version")
	}
	body = `{"tag_name":"v0.7.0","html_url":"https://evil.example/"}`
	if rel, err := latestRelease(context.Background(), srv.URL); err != nil || rel.URL != "https://github.com/PaddeK/catflap/releases" {
		t.Errorf("foreign release URL not replaced: %+v, %v", rel, err)
	}
}

func TestUpdateFoundAlertsOnce(t *testing.T) {
	old := api.Version
	api.Version = "0.5.1"
	t.Cleanup(func() { api.Version = old })
	ntfy := newFake(t)
	d := &Daemon{
		cfg:   Config{Name: "lab"},
		paths: Paths{State: filepath.Join(t.TempDir(), "state.json")},
		logf:  t.Logf,
		disp:  notify.NewDispatcher(t.Logf),
	}
	t.Cleanup(d.disp.Close)
	d.disp.Set([]notify.Channel{{Name: "phone", Type: notify.TypeNtfy, Server: ntfy.srv.URL, Topic: "t"}})

	d.updateFound(release{Version: "0.5.1", URL: "u"})
	if d.latest != "" {
		t.Errorf("latest = %q for the running version", d.latest)
	}
	d.updateFound(release{Version: "0.6.0", URL: "https://github.com/PaddeK/catflap/releases/tag/v0.6.0"})
	if !ntfy.waitFind(t, "/", "catflap 0.6.0 is available") {
		t.Fatal("no update alert")
	}
	if d.latest != "0.6.0" || d.st.UpdateNotified != "0.6.0" {
		t.Errorf("latest %q, notified %q", d.latest, d.st.UpdateNotified)
	}
	d.updateFound(release{Version: "0.6.0", URL: "u"}) // the next day
	d.updateFound(release{Version: "0.6.1", URL: "u"})
	if !ntfy.waitFind(t, "/", "catflap 0.6.1 is available") {
		t.Fatal("no alert for the next version")
	}
	n := 0
	for _, r := range ntfy.find("/") {
		if s, _ := r.Body["title"].(string); s == "lab: catflap 0.6.0 is available" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d alerts for 0.6.0, want 1", n)
	}
}
