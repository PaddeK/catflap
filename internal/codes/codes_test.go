package codes

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, relay := range []string{"derp.example.com", "localhost:18443", "[::1]:443"} {
		c, err := New(KindInvite, relay)
		if err != nil {
			t.Fatal(err)
		}
		s := c.String()
		got, err := Parse(strings.ToUpper(s[:5])+s[5:], KindInvite) // kind is case-insensitive
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if got.ID != c.ID || string(got.Secret) != string(c.Secret) || got.Relay != c.Relay {
			t.Fatalf("round trip mismatch: %+v vs %+v", got, c)
		}
	}
}

func TestParseRejects(t *testing.T) {
	c, _ := New(KindJoin, "derp.example.com")
	if _, err := Parse(c.String(), KindInvite); err == nil || !strings.Contains(err.Error(), "join code") {
		t.Fatalf("want join-code error, got %v", err)
	}
	bad := []string{
		"",
		"cfi1-abc-def",
		"cfi1-aaaaaaaaaaaaa-short@derp.example.com",
		"cfi1-" + c.ID + "-" + strings.Repeat("a", 26) + "@https://x",
		"cfi1-" + c.ID + "-" + strings.Repeat("a", 26) + "@host:99999",
	}
	for _, s := range bad {
		if _, err := Parse(s, KindInvite); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", s)
		}
	}
}

func TestMACAndSeal(t *testing.T) {
	c, _ := New(KindInvite, "derp.example.com")
	k, err := DeriveKeys(c.Secret)
	if err != nil {
		t.Fatal(err)
	}
	f := EnrollFields{InviteID: c.ID, NodeKey: "nodekey:aa", SSHKey: "ssh-ed25519 AAAA", Hostname: "laptop", Nonce: RandomNonce()}
	mac := EnrollMAC(k, f)
	if !VerifyEnrollMAC(k, f, mac) {
		t.Fatal("valid MAC rejected")
	}
	swapped := f
	swapped.NodeKey = "nodekey:bb"
	if VerifyEnrollMAC(k, swapped, mac) {
		t.Fatal("MAC accepted for swapped node key")
	}
	other, _ := New(KindInvite, "derp.example.com")
	k2, _ := DeriveKeys(other.Secret)
	if VerifyEnrollMAC(k2, f, mac) {
		t.Fatal("MAC accepted with another invite's key")
	}

	aad := SealAAD(c.ID, "sub1", f.NodeKey)
	sealed, err := Seal(k, aad, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Open(k, aad, sealed)
	if err != nil || string(pt) != "hello" {
		t.Fatalf("Open = %q, %v", pt, err)
	}
	if _, err := Open(k, SealAAD(c.ID, "sub1", "nodekey:bb"), sealed); err == nil {
		t.Fatal("Open accepted different AAD")
	}
	if _, err := Open(k2, aad, sealed); err == nil {
		t.Fatal("Open accepted wrong key")
	}
}
