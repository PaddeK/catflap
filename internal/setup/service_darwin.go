package setup

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/homelab"
)

// launchdLabel names the homelab's LaunchAgent.
const launchdLabel = "catflap.homelab"

const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>homelab</string>
		<string>run</string>
		<string>--service</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>5</integer>
</dict>
</plist>
`

// launchdService is the homelab as a LaunchAgent: it runs as the user, in
// the user's login session (FileVault's unlock at boot or automatic login
// starts one).
type launchdService struct{ p homelab.Paths }

func platformService(p homelab.Paths) (homelabService, error) {
	return launchdService{p}, nil
}

func (launchdService) binPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", "catflap"), nil
}

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func guiDomain() string     { return fmt.Sprintf("gui/%d", os.Getuid()) }
func launchdTarget() string { return guiDomain() + "/" + launchdLabel }

func (launchdService) installed() bool { return fsutil.Exists(plistPath()) }

// loaded reports whether launchd has the job (running or waiting to restart).
func (launchdService) loaded() bool {
	return exec.Command("launchctl", "print", launchdTarget()).Run() == nil
}

func (s launchdService) install(bin string, out io.Writer) error {
	var esc bytes.Buffer
	xml.EscapeText(&esc, []byte(bin))
	if err := fsutil.WriteFile(plistPath(), fmt.Appendf(nil, launchdPlist, launchdLabel, esc.String()), 0o644); err != nil {
		return err
	}
	if s.loaded() { // reload, so launchd reads the new definition
		if err := s.stop(out); err != nil {
			return err
		}
	}
	run(io.Discard, "launchctl", "enable", launchdTarget())
	return s.start(out)
}

func (s launchdService) start(out io.Writer) error {
	if s.loaded() {
		return run(io.Discard, "launchctl", "kickstart", launchdTarget())
	}
	if msg, err := exec.Command("launchctl", "bootstrap", guiDomain(), plistPath()).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s\n    launchd runs the homelab in your login session: log in on the Mac itself\n    (or turn on automatic login), then run this again", err, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (s launchdService) stop(out io.Writer) error {
	if !s.loaded() {
		return nil
	}
	if err := run(io.Discard, "launchctl", "bootout", launchdTarget()); err != nil && s.loaded() {
		return err
	}
	if !waitFor(15*time.Second, func() bool { return !s.loaded() && !homelabRunning(s.p) }) {
		return fmt.Errorf("the homelab did not stop; see %s", s.logs())
	}
	return nil
}

func (s launchdService) restart(out io.Writer, noBlock bool) error {
	if !s.loaded() {
		return s.start(out)
	}
	// kickstart -k: launchd kills and starts it itself, also if this
	// session drops in between.
	return run(out, "launchctl", "kickstart", "-k", launchdTarget())
}

func (s launchdService) remove(out io.Writer) error {
	if err := s.stop(out); err != nil {
		return err
	}
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(out, "    stopped and removed the LaunchAgent %s\n", launchdLabel)
	return nil
}

func (s launchdService) logs() string { return "tail -f " + s.p.Log }
