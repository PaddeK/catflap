package homelab

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestExpireDevices(t *testing.T) {
	b, tg, relay, ntfy := testBot(t)
	d := b.d
	ctx := context.Background()
	now := time.Now()
	d.st.Devices = []Device{
		{Name: "guest", Added: now.Add(-time.Hour), Expires: now.Add(-time.Second)},
		{Name: "later", Added: now, Expires: now.Add(time.Hour)},
		{Name: "phone", Added: now},
	}
	d.expireDevices(ctx)
	if len(d.st.Devices) != 2 || d.st.Devices[0].Name != "later" || d.st.Devices[1].Name != "phone" {
		t.Fatalf("devices after expiry: %+v", d.st.Devices)
	}
	if len(relay.find("/v1/agent/sync")) != 1 {
		t.Fatal("the relay was not told")
	}
	if !ntfy.waitFind(t, "/", "temporary device removed: guest") || !tg.waitFind(t, "/sendMessage", "its time ran out") {
		t.Fatal("no alert about the expired device")
	}

	// catflap expire: permanent again, then temporary.
	if exp, err := d.setExpiry("later", 0, ""); err != nil || !exp.IsZero() || !d.st.Devices[0].Expires.IsZero() {
		t.Fatalf("setExpiry never: %v %v %+v", exp, err, d.st.Devices[0])
	}
	exp, err := d.setExpiry("phone", 48*time.Hour, "")
	if err != nil || time.Until(exp) < 47*time.Hour || !d.st.Devices[1].Expires.Equal(exp) {
		t.Fatalf("setExpiry 48h: %v %v", exp, err)
	}
	if _, err := d.setExpiry("ghost", time.Hour, ""); err == nil {
		t.Fatal("setExpiry accepted an unknown device")
	}
	if _, err := d.setExpiry("phone", time.Second, ""); err == nil {
		t.Fatal("setExpiry accepted a second")
	}
}

func TestInviteDeviceTTL(t *testing.T) {
	b, tg, _, _ := testBot(t)
	ctx := context.Background()
	b.onMessage(ctx, msg(42, "/invite guest 7d", 0))
	if len(b.d.st.Invites) != 1 || b.d.st.Invites[0].DeviceTTL != 7*24*time.Hour {
		t.Fatalf("invite state: %+v", b.d.st.Invites)
	}
	if s := tg.lastText(t); !strings.Contains(s, "removed 7 days after it enrolls") {
		t.Fatalf("reply: %q", s)
	}
	b.onMessage(ctx, msg(42, "/invite other soon", 0))
	if s := tg.lastText(t); !strings.Contains(s, "Usage") || len(b.d.st.Invites) != 1 {
		t.Fatalf("bad duration: %q", s)
	}
	if _, err := b.d.createInvite(ctx, InviteOptions{Name: "x", DeviceTTL: time.Second}); err == nil {
		t.Fatal("a device lifetime of a second was accepted")
	}
}

func TestHumanDuration(t *testing.T) {
	for in, want := range map[time.Duration]string{
		time.Hour: "1 hour", 24 * time.Hour: "24 hours", 7 * 24 * time.Hour: "7 days",
		90 * time.Minute: "90 minutes", time.Minute: "1 minute", 1500 * time.Millisecond: "1.5s",
	} {
		if got := HumanDuration(in); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
