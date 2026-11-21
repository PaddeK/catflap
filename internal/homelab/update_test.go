package homelab

import (
	"path/filepath"
	"testing"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/notify"
	"github.com/PaddeK/catflap/internal/update"
)

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

	d.updateFound(update.Release{Version: "0.5.1", URL: "u"})
	if d.latest != "" {
		t.Errorf("latest = %q for the running version", d.latest)
	}
	d.updateFound(update.Release{Version: "0.6.0", URL: "https://github.com/PaddeK/catflap/releases/tag/v0.6.0"})
	if !ntfy.waitFind(t, "/", "catflap 0.6.0 is available") {
		t.Fatal("no update alert")
	}
	if d.latest != "0.6.0" || d.st.UpdateNotified != "0.6.0" {
		t.Errorf("latest %q, notified %q", d.latest, d.st.UpdateNotified)
	}
	d.updateFound(update.Release{Version: "0.6.0", URL: "u"}) // the next day
	d.updateFound(update.Release{Version: "0.6.1", URL: "u"})
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
