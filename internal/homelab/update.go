package homelab

import (
	"context"
	"fmt"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/notify"
	"github.com/PaddeK/catflap/internal/update"
)

// The update check: once a day the homelab asks GitHub for the latest
// catflap release and sends one alert per new version. Turned off with
// "check_updates": false in the settings file. Builds that aren't releases
// (dev builds) never check.

const (
	updateEvery = 24 * time.Hour
	updateDelay = 2 * time.Minute // after the start, so a restart loop doesn't hammer GitHub
)

func (d *Daemon) updateLoop(ctx context.Context) {
	if !d.cfg.checkUpdates() || !update.IsRelease(api.Version) {
		return
	}
	wait := updateDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = updateEvery
		rel, err := update.Latest(ctx, 30*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				d.logf("update check: %v", err)
			}
			continue
		}
		d.updateFound(rel)
	}
}

// updateFound records the latest release and alerts once per newer version.
func (d *Daemon) updateFound(rel update.Release) {
	if !update.Newer(rel.Version, api.Version) {
		d.mu.Lock()
		d.latest = ""
		d.mu.Unlock()
		return
	}
	d.mu.Lock()
	d.latest = rel.Version
	first := d.st.UpdateNotified != rel.Version
	if first {
		d.st.UpdateNotified = rel.Version
		d.saveLocked()
	}
	d.mu.Unlock()
	if !first {
		return
	}
	d.logf("update check: catflap %s is available (running %s)", rel.Version, api.Version)
	d.alert(notify.Message{Event: "update.available", Title: "catflap " + rel.Version + " is available",
		Body: fmt.Sprintf("This homelab runs catflap %s. What's new: %s\nHow to upgrade: %s", api.Version, rel.URL, update.UpgradeDocURL)}, "")
}
