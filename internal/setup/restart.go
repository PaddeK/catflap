package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/homelab"
)

// `catflap restart`: restart the relay or homelab service after an upgrade
// (or a hand edit of its config), without knowing systemd's unit names.

const (
	relayService    = "catflap-relay" // system service
	homelabUnitName = "catflap"       // systemd user service
)

// RestartRelay restarts the relay's system service (needs root).
func RestartRelay(ctx context.Context, out io.Writer) error {
	if err := checkSystemd(); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("restarting the relay needs root: sudo catflap restart")
	}
	if !unitLoaded(false, relayService) {
		return fmt.Errorf("there is no %s service here (was the relay set up with `catflap setup relay`?)", relayService)
	}
	fmt.Fprintln(out, "restarting the relay; connected devices reconnect within a few seconds ...")
	if err := run(out, "systemctl", "restart", relayService); err != nil {
		return err
	}
	if !waitFor(10*time.Second, func() bool { return isActive(relayService) }) {
		return fmt.Errorf("the relay did not come back up; see: journalctl -u %s -n 50", relayService)
	}
	fmt.Fprintln(out, "relay running again")
	return nil
}

// RestartHomelab restarts the homelab's service.
func RestartHomelab(ctx context.Context, out io.Writer) error {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return errors.New("the homelab runs as your own user: run catflap restart without sudo")
	}
	p, err := homelab.DefaultPaths()
	if err != nil {
		return err
	}
	svc, err := newHomelabService(p)
	if err != nil {
		return err
	}
	if !svc.installed() {
		return errors.New("the homelab doesn't run as a service here (set up with --no-service?); restart `catflap homelab run` the way you started it")
	}
	if ViaCatflap(os.Getenv("SSH_CONNECTION")) {
		// Restarting cuts this very session. Don't wait for the restart
		// (we'd die mid-command); the service manager finishes it on its own.
		fmt.Fprintln(out, "You are connected through catflap, so this session drops now.")
		fmt.Fprintln(out, "Reconnect in a few seconds; `catflap status` then shows the running version.")
		return svc.restart(out, true)
	}
	fmt.Fprintln(out, "restarting the homelab; open sessions through catflap drop and devices reconnect ...")
	began := time.Now().Add(-time.Second)
	if err := svc.restart(out, false); err != nil {
		return err
	}
	s, up := waitHomelabUp(ctx, p, began, 20*time.Second)
	if !up {
		return fmt.Errorf("the homelab did not come back up; see: %s", svc.logs())
	}
	fmt.Fprintf(out, "homelab %q running catflap %s again: %d device(s), forwarding %s\n", s.Name, s.Version, s.Devices, strings.Join(s.Forwards, ", "))
	return nil
}

// ViaCatflap reports whether an SSH session (from $SSH_CONNECTION) arrived
// through the catflap tunnel: the homelab connects to sshd from localhost.
func ViaCatflap(sshConnection string) bool {
	f := strings.Fields(sshConnection)
	return len(f) > 0 && (f[0] == "127.0.0.1" || f[0] == "::1")
}

func checkSystemd() error {
	if runtime.GOOS != "linux" {
		return errors.New("the relay runs on Linux only")
	}
	if !hasCmd("systemctl") {
		return errors.New("systemctl not found: the relay needs systemd")
	}
	return nil
}

// unitLoaded reports whether systemd knows the unit (installed, enabled or not).
func unitLoaded(user bool, unit string) bool {
	args := []string{"show", "--property=LoadState", "--value", unit}
	if user {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.Command("systemctl", args...).Output()
	return err == nil && strings.TrimSpace(string(out)) == "loaded"
}

func waitFor(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}
