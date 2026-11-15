package homelab

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/fsutil"
)

// Info describes an existing homelab setup (for uninstall's plan).
type Info struct {
	Name, Relay string
	Devices     []string
	AuthKeys    string // authorized_keys file catflap manages ("" = off)
}

// Inspect reads the homelab setup at p, if there is one.
func Inspect(p Paths) (*Info, bool) {
	var cfg Config
	if fsutil.ReadJSON(p.Config, &cfg) != nil {
		return nil, false
	}
	info := &Info{Name: cfg.Name, Relay: cfg.Relay}
	if info.Name == "" {
		info.Name = "homelab"
	}
	var st State
	if fsutil.ReadJSON(p.State, &st) == nil {
		for _, d := range st.Devices {
			info.Devices = append(info.Devices, d.Name)
		}
	}
	if path, ok := cfg.authorizedKeysPath(); ok {
		info.AuthKeys = path
	}
	return info, true
}

// removeDeviceKeys removes the SSH keys catflap added for devs and returns
// how many lines went.
func removeDeviceKeys(cfg Config, devs []Device) (int, error) {
	path, ok := cfg.authorizedKeysPath()
	if !ok {
		return 0, nil
	}
	n := 0
	for _, d := range devs {
		removed, err := removeAuthorizedKey(path, d.Name)
		if err != nil {
			return n, fmt.Errorf("%s: %w", path, err)
		}
		if removed {
			n++
		}
	}
	return n, nil
}

// Deregister asks the relay to forget this homelab: its record, its devices
// and its open invites. Relays before v0.3 don't support it.
func Deregister(ctx context.Context, relay, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := api.NewClient(relay, token).Do(ctx, "DELETE", "/v1/agent", nil, nil)
	var ae *api.Error
	if errors.As(err, &ae) && (ae.Status == 404 || ae.Status == 405) {
		return errors.New("the relay is older than v0.3 and can't do this")
	}
	if errors.As(err, &ae) && ae.Status == 401 {
		return nil // the relay doesn't know this homelab (anymore)
	}
	return err
}

// UninstallResult reports what Uninstall did.
type UninstallResult struct {
	RelayErr     error    // de-registering failed (remove it on the relay by hand)
	KeysRemoved  int      // authorized_keys lines removed
	KeysErr      error    // removing them failed (remove them by hand)
	FilesRemoved []string // files and directories deleted
}

// Uninstall removes the homelab's data: it de-registers from the relay,
// removes the SSH keys catflap added, and deletes config, state,
// notification channels and the service log. The service must be stopped
// before. keysDone skips the keys (the daemon removed them already, see
// POST /uninstall/keys).
func Uninstall(ctx context.Context, p Paths, keysDone bool) (UninstallResult, error) {
	var res UninstallResult
	var cfg Config
	fsutil.ReadJSON(p.Config, &cfg)
	var st State
	if err := fsutil.ReadJSON(p.State, &st); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("reading %s: %w", p.State, err)
	}
	if cfg.Relay != "" && st.AgentToken != "" {
		res.RelayErr = Deregister(ctx, cfg.Relay, st.AgentToken)
	}
	if !keysDone {
		res.KeysRemoved, res.KeysErr = removeDeviceKeys(cfg, st.Devices)
	}
	for _, f := range []string{p.State, p.Notify, p.Socket, p.Config} {
		if err := os.Remove(f); err == nil {
			res.FilesRemoved = append(res.FilesRemoved, f)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
	}
	// The service log (macOS, Windows): best effort. On Windows the stopped
	// process may hold it open a moment longer.
	for _, f := range []string{p.Log, p.Log + ".1"} {
		if removeSoon(f, 5*time.Second) {
			res.FilesRemoved = append(res.FilesRemoved, f)
		}
	}
	// Remove the directories if catflap's files were all that was in them
	// (on Linux the client's config lives next to homelab.json).
	for _, dir := range []string{filepath.Dir(p.State), filepath.Dir(p.Config)} {
		if os.Remove(dir) == nil {
			res.FilesRemoved = append(res.FilesRemoved, dir)
		}
	}
	return res, nil
}

// removeSoon removes a file, retrying while it is in use, and reports
// whether it was there and is gone.
func removeSoon(f string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		err := os.Remove(f)
		if err == nil {
			return true
		}
		if errors.Is(err, fs.ErrNotExist) || time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}
