//go:build windows

package setup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// Runs against a scratch key, never the real user PATH.
func TestRemoveFromPath(t *testing.T) {
	const key = `Software\catflap-test-path`
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.DeleteKey(registry.CURRENT_USER, key)
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "catflap")
	k.SetExpandStringValue("Path", `%USERPROFILE%\bin;`+dir+`\;C:\Tools`)
	k.Close()

	removed, err := removeFromPath(registry.CURRENT_USER, key, dir)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	k, _ = registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	defer k.Close()
	got, typ, _ := k.GetStringValue("Path")
	if got != `%USERPROFILE%\bin;C:\Tools` || typ != registry.EXPAND_SZ {
		t.Fatalf("Path = %q (type %d), want the other entries unexpanded and REG_EXPAND_SZ", got, typ)
	}
	if removed, _ := removeFromPath(registry.CURRENT_USER, key, dir); removed {
		t.Fatal("removed it twice")
	}
}

func TestDeleteLater(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catflap")
	os.MkdirAll(dir, 0o755)
	exe := filepath.Join(dir, "catflap.exe")
	os.WriteFile(exe, []byte("x"), 0o755)
	os.WriteFile(filepath.Join(dir, "catflap.exe.old-1234abcd"), []byte("x"), 0o755)
	if err := deleteLater(exe); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
	}
	t.Fatal("catflap.exe and its folder were not deleted")
}
