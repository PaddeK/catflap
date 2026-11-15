package homelab

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveSoon(t *testing.T) {
	f := filepath.Join(t.TempDir(), "homelab.log")
	if removeSoon(f, time.Second) {
		t.Error("a missing file counts as removed")
	}
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !removeSoon(f, time.Second) {
		t.Error("file not removed")
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Errorf("file still there: %v", err)
	}
}
