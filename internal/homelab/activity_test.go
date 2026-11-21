package homelab

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/PaddeK/catflap/internal/fsutil"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

func TestApplyActivity(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	kDirect, kRelay, kIdle, kAway := key.NewNode().Public(), key.NewNode().Public(), key.NewNode().Public(), key.NewNode().Public()
	nk := func(k key.NodePublic) string { s, _ := k.MarshalText(); return string(s) }
	since := now.Add(-30 * 24 * time.Hour)
	d := &Daemon{
		paths: Paths{State: filepath.Join(t.TempDir(), "state.json")},
		logf:  t.Logf,
		st: State{SeenSince: since, Devices: []Device{
			{Name: "laptop", NodeKey: nk(kDirect), Added: since.Add(-time.Hour)},
			{Name: "phone", NodeKey: nk(kRelay), Added: since.Add(-time.Hour)},
			{Name: "tablet", NodeKey: nk(kIdle), Added: since.Add(-time.Hour), LastSeen: now.Add(-48 * time.Hour)},
			{Name: "old", NodeKey: nk(kAway), Added: since.Add(-time.Hour)},
			{Name: "new", NodeKey: nk(key.NewNode().Public()), Added: now.Add(-time.Hour)},
		}},
	}
	st := &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{
		kDirect: {Active: true, CurAddr: "203.0.113.5:41641", LastHandshake: now.Add(-time.Minute), LastWrite: now.Add(-2 * time.Second)},
		kRelay:  {Active: true, Relay: "catflap", LastHandshake: now.Add(-30 * time.Second)},
		kIdle:   {LastHandshake: now.Add(-3 * time.Hour)},
	}}
	act := d.applyActivity(st, now)
	if a := act[nk(kDirect)]; !a.active || a.path != "direct" {
		t.Errorf("laptop: %+v, want active direct", a)
	}
	if a := act[nk(kRelay)]; !a.active || a.path != "relay" {
		t.Errorf("phone: %+v, want active relay", a)
	}
	if _, ok := act[nk(kIdle)]; ok {
		t.Error("tablet is not active")
	}
	if got := d.st.Devices[0].LastSeen; !got.Equal(now.Add(-2 * time.Second)) {
		t.Errorf("laptop LastSeen = %v, want the last write", got)
	}
	if got := d.st.Devices[2].LastSeen; !got.Equal(now.Add(-3 * time.Hour)) {
		t.Errorf("tablet LastSeen = %v, want the handshake", got)
	}

	// The first change is saved; changes within seenSaveEvery are not.
	var saved State
	if err := fsutil.ReadJSON(d.paths.State, &saved); err != nil || saved.Devices[2].LastSeen.IsZero() {
		t.Fatalf("activity not saved: %v", err)
	}
	st.Peer[kIdle].LastHandshake = now.Add(time.Minute)
	d.applyActivity(st, now.Add(time.Minute))
	fsutil.ReadJSON(d.paths.State, &saved)
	if !saved.Devices[2].LastSeen.Equal(now.Add(-3 * time.Hour)) {
		t.Error("saved again within seenSaveEvery")
	}
	d.applyActivity(st, now.Add(seenSaveEvery+time.Minute))
	fsutil.ReadJSON(d.paths.State, &saved)
	if !saved.Devices[2].LastSeen.Equal(now.Add(time.Minute)) {
		t.Error("not saved after seenSaveEvery")
	}

	// What `catflap list` shows.
	infos := map[string]DeviceInfo{}
	d.mu.Lock()
	for _, dev := range d.st.Devices {
		a := act[dev.NodeKey]
		ts := d.st.SeenSince
		if dev.Added.After(ts) {
			ts = dev.Added
		}
		infos[dev.Name] = DeviceInfo{Device: dev, Active: a.active, Path: a.path, TrackedSince: ts}
	}
	d.mu.Unlock()
	later := now.Add(seenSaveEvery + time.Minute)
	for name, want := range map[string]string{
		"laptop": "now (direct)",
		"phone":  "now (relay)",
		"tablet": "10 minutes ago",
		"old":    "not since " + since.Local().Format("2006-01-02"),
		"new":    "never",
	} {
		if got := infos[name].LastSeenText(later); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if got := (DeviceInfo{}).LastSeenText(now); got != "?" {
		t.Errorf("older homelab: %q, want ?", got)
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local)
	for d, want := range map[time.Duration]string{
		30 * time.Second:    "just now",
		5 * time.Minute:     "5 minutes ago",
		61 * time.Minute:    "1 hour ago",
		47 * time.Hour:      "47 hours ago",
		72 * time.Hour:      "3 days ago",
		40 * 24 * time.Hour: "on 2026-10-11",
	} {
		if got := Ago(now.Add(-d), now); got != want {
			t.Errorf("Ago(-%v) = %q, want %q", d, got, want)
		}
	}
}
