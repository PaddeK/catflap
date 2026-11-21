package homelab

import (
	"strconv"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

// Device activity: when each device was last seen, and whether it is
// connected right now, directly or through the relay. The tunnel knows this
// only since the daemon started, so LastSeen is kept in the state file too.

// DeviceInfo is a device as `catflap list` shows it.
type DeviceInfo struct {
	Device
	// Active means the device is connected right now.
	Active bool `json:"active,omitempty"`
	// Path is how an active device is connected: "direct" or "relay".
	Path string `json:"path,omitempty"`
	// TrackedSince is when the homelab started recording LastSeen for this
	// device: a zero LastSeen means "not since then". Zero from homelabs
	// that don't record it (older versions).
	TrackedSince time.Time `json:"tracked_since,omitzero"`
}

// seenSaveEvery limits how often activity alone rewrites the state file.
const seenSaveEvery = 10 * time.Minute

type activity struct {
	active bool
	path   string
}

// refreshActivity updates the devices' LastSeen from the tunnel and returns
// the current activity by node key.
func (d *Daemon) refreshActivity() map[string]activity {
	if d.srv == nil { // tests
		return map[string]activity{}
	}
	return d.applyActivity(d.srv.Status(), time.Now())
}

func (d *Daemon) applyActivity(st *ipnstate.Status, now time.Time) map[string]activity {
	out := map[string]activity{}
	if st == nil {
		return out
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for i := range d.st.Devices {
		dev := &d.st.Devices[i]
		var k key.NodePublic
		if k.UnmarshalText([]byte(dev.NodeKey)) != nil {
			continue
		}
		ps := st.Peer[k]
		if ps == nil {
			continue
		}
		seen := ps.LastHandshake
		if ps.LastWrite.After(seen) {
			seen = ps.LastWrite
		}
		if seen.After(now) {
			seen = now
		}
		if seen.After(dev.LastSeen) {
			dev.LastSeen = seen.Truncate(time.Second)
			changed = true
		}
		if ps.Active {
			path := "relay"
			if ps.CurAddr != "" || ps.PeerRelay != "" {
				path = "direct"
			}
			out[dev.NodeKey] = activity{active: true, path: path}
		}
	}
	d.seenDirty = d.seenDirty || changed
	if d.seenDirty && now.Sub(d.seenSaved) >= seenSaveEvery {
		d.seenSaved, d.seenDirty = now, false
		if err := d.saveLocked(); err != nil {
			d.logf("saving state: %v", err)
		}
	}
	return out
}

// deviceInfos returns the devices with their activity.
func (d *Daemon) deviceInfos() []DeviceInfo {
	act := d.refreshActivity()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []DeviceInfo{}
	for _, dev := range d.st.Devices {
		a := act[dev.NodeKey]
		since := d.st.SeenSince
		if dev.Added.After(since) {
			since = dev.Added
		}
		out = append(out, DeviceInfo{Device: dev, Active: a.active, Path: a.path, TrackedSince: since})
	}
	return out
}

// LastSeenText describes when a device was last seen, e.g. "now (direct)",
// "3 hours ago" or "never".
func (i DeviceInfo) LastSeenText(now time.Time) string {
	switch {
	case i.Active:
		return "now (" + i.Path + ")"
	case i.TrackedSince.IsZero():
		return "?" // a homelab that doesn't record it
	case i.LastSeen.IsZero() && !i.Added.Before(i.TrackedSince):
		return "never"
	case i.LastSeen.IsZero():
		return "not since " + i.TrackedSince.Local().Format("2006-01-02")
	}
	return Ago(i.LastSeen, now)
}

// Ago formats how long ago t was, coarsely.
func Ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 2*time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
	return "on " + t.Local().Format("2006-01-02")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
