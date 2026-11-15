package relay

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/PaddeK/catflap/internal/fsutil"
	"tailscale.com/types/key"
)

func TestBackupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Hostname = "relay.example.com"
	cfg.StateDir = filepath.Join(dir, "state")
	st := state{DERPKey: key.NewNode(), Agents: map[string]*agent{
		"a1": {Name: "lab", TokenHash: "h", Created: time.Now(), Clients: map[string]string{}},
	}}
	if err := fsutil.WriteJSON(cfg.statePath(), st, 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "backup.json")
	if _, err := WriteBackup(cfg, path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 { // no Unix modes on Windows
		t.Fatalf("backup mode %v, want 600 (it holds the DERP key)", fi.Mode().Perm())
	}
	if _, err := WriteBackup(cfg, path); err == nil {
		t.Fatal("overwrote an existing backup")
	}
	b, err := ReadBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.Config.Hostname != cfg.Hostname || !slices.Equal(b.Homelabs(), []string{"lab"}) {
		t.Fatalf("restored %q with homelabs %v", b.Config.Hostname, b.Homelabs())
	}

	// Manual certificates travel with the backup.
	cfg.CertMode = "manual"
	cfg.CertFile, cfg.KeyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cfg.CertFile, []byte("CERT"), 0o644)
	os.WriteFile(cfg.KeyFile, []byte("KEY"), 0o600)
	path2 := filepath.Join(dir, "backup2.json")
	if _, err := WriteBackup(cfg, path2); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadBackup(path2); err != nil || b.CertPEM != "CERT" || b.KeyPEM != "KEY" {
		t.Fatalf("manual cert: %v %+v", err, b)
	}

	// Anything else is refused.
	other := filepath.Join(dir, "other.json")
	os.WriteFile(other, []byte(`{"hostname":"x"}`), 0o600)
	if _, err := ReadBackup(other); err == nil {
		t.Fatal("accepted a file that isn't a backup")
	}
}
