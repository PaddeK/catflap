package codes

import (
	"testing"
)

func TestPairing(t *testing.T) {
	reveal, nonce := RandomReveal(), RandomReveal()
	commit := RequestCommit(reveal)
	if !CheckCommit(commit, reveal) || CheckCommit(commit, RandomReveal()) {
		t.Fatal("commitment check broken")
	}
	f := PairingFields{HomelabKey: "nodekey:aa", Homelab: "lab", Name: "laptop", NodeKey: "nodekey:bb", BoxKey: "box", SSHKey: "ssh-ed25519 AAAA", Hostname: "host"}
	c := PairingCode(f, reveal, nonce)
	if len(c) != 6 || PairingCode(f, reveal, nonce) != c {
		t.Fatalf("code %q not stable", c)
	}
	// Any change on either side changes the code (with overwhelming probability).
	for _, g := range []PairingFields{
		{HomelabKey: "nodekey:ac", Homelab: f.Homelab, Name: f.Name, NodeKey: f.NodeKey, BoxKey: f.BoxKey, SSHKey: f.SSHKey, Hostname: f.Hostname},
		{HomelabKey: f.HomelabKey, Homelab: f.Homelab, Name: f.Name, NodeKey: "nodekey:bc", BoxKey: f.BoxKey, SSHKey: f.SSHKey, Hostname: f.Hostname},
		{HomelabKey: f.HomelabKey, Homelab: f.Homelab, Name: f.Name, NodeKey: f.NodeKey, BoxKey: "box2", SSHKey: f.SSHKey, Hostname: f.Hostname},
		{HomelabKey: f.HomelabKey, Homelab: f.Homelab, Name: f.Name, NodeKey: f.NodeKey, BoxKey: f.BoxKey, SSHKey: "ssh-ed25519 BBBB", Hostname: f.Hostname},
		// field boundaries are unambiguous
		{HomelabKey: f.HomelabKey, Homelab: f.Homelab + "l", Name: "aptop", NodeKey: f.NodeKey, BoxKey: f.BoxKey, SSHKey: f.SSHKey, Hostname: f.Hostname},
	} {
		if PairingCode(g, reveal, nonce) == c {
			t.Errorf("code unchanged for %+v", g)
		}
	}
	if PairingCode(f, reveal, RandomReveal()) == c && PairingCode(f, reveal, RandomReveal()) == c {
		t.Error("the homelab's nonce doesn't change the code")
	}
	if FormatPairingCode("482913") != "482 913" || NormalizePairingCode(" 482-913 ") != "482913" {
		t.Error("formatting")
	}
}

func TestSealTo(t *testing.T) {
	pub, priv, err := NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := DecodeBoxKey(EncodeBoxKey(pub))
	if err != nil || *k != *pub {
		t.Fatal("box key round trip")
	}
	s, err := SealTo(pub, []byte("address"))
	if err != nil {
		t.Fatal(err)
	}
	if msg, err := OpenSealed(pub, priv, s); err != nil || string(msg) != "address" {
		t.Fatalf("open: %q %v", msg, err)
	}
	otherPub, otherPriv, _ := NewBoxKey()
	if _, err := OpenSealed(otherPub, otherPriv, s); err == nil {
		t.Fatal("another key opened the reply")
	}
}
