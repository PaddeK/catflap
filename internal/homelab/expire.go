package homelab

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/PaddeK/catflap/internal/notify"
)

// Temporary devices: a device with an expiry time is removed by the homelab
// itself once that time has passed, as if by `catflap remove`, with an
// alert. `invite --expires` sets it at enrollment, `catflap expire` later.

// expireDevices removes the devices whose time is up.
func (d *Daemon) expireDevices(ctx context.Context) {
	now := time.Now()
	d.mu.Lock()
	var due []string
	for _, dev := range d.st.Devices {
		if !dev.Expires.IsZero() && !now.Before(dev.Expires) {
			due = append(due, dev.Name)
		}
	}
	d.mu.Unlock()
	for _, name := range due {
		if _, err := d.dropDevice(ctx, name, "", true); err != nil {
			d.logf("expire: removing %q: %v", name, err)
		}
	}
}

// setExpiry makes a device temporary (removed in `in` from now) or, with
// in == 0, permanent again. It returns the new expiry time (zero: never).
func (d *Daemon) setExpiry(name string, in time.Duration, origin string) (time.Time, error) {
	if in < 0 || in > 0 && in < time.Minute {
		return time.Time{}, fmt.Errorf("use at least a minute, e.g. 2h or 7d")
	}
	var exp time.Time
	if in > 0 {
		exp = time.Now().Add(in).Truncate(time.Second)
	}
	d.mu.Lock()
	i := slices.IndexFunc(d.st.Devices, func(dev Device) bool { return dev.Name == name })
	if i < 0 {
		d.mu.Unlock()
		return time.Time{}, fmt.Errorf("no device named %q", name)
	}
	d.st.Devices[i].Expires = exp
	err := d.saveLocked()
	d.mu.Unlock()
	if err != nil {
		return time.Time{}, err
	}
	if exp.IsZero() {
		d.logf("expire: %q no longer expires", name)
		d.alert(notify.Message{Event: "device.expiry", Title: "device no longer expires: " + name,
			Body: fmt.Sprintf("%s was made a permanent device %s.", name, via(origin))}, origin)
	} else {
		d.logf("expire: %q expires %s", name, exp.Format(time.RFC3339))
		d.alert(notify.Message{Event: "device.expiry", Title: "device expires: " + name,
			Body: fmt.Sprintf("%s is removed automatically at %s (set %s).", name, when(exp), via(origin))}, origin)
	}
	return exp, nil
}

// knownDevice reports whether name is an enrolled device or has an open
// invite (`forward add --only` may name a device that enrolls later).
func (d *Daemon) knownDevice(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.ContainsFunc(d.st.Devices, func(dev Device) bool { return dev.Name == name }) ||
		slices.ContainsFunc(d.st.Invites, func(inv Invite) bool { return inv.Name == name && inv.Used == nil })
}

// expiresNote is a sentence for alerts about a new temporary device.
func expiresNote(exp time.Time) string {
	if exp.IsZero() {
		return ""
	}
	return " It is a temporary device, removed automatically at " + when(exp) + "."
}
