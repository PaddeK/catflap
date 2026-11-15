package setup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"

	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/homelab"
)

const homelabUnit = `[Unit]
Description=catflap homelab (tailcat server + relay agent)
After=network-online.target

[Service]
ExecStart=%s homelab run
Restart=always
RestartSec=3
NoNewPrivileges=yes

[Install]
WantedBy=default.target
`

// systemdService is the homelab as a systemd user service with lingering.
type systemdService struct{}

func platformService(homelab.Paths) (homelabService, error) {
	if !hasCmd("systemctl") {
		return nil, errors.New("the homelab service needs systemd; run `catflap homelab run` yourself (e.g. from your init system)")
	}
	return systemdService{}, nil
}

func (systemdService) binPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", "catflap"), nil
}

func unitPath() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		dir = filepath.Join(x, "systemd", "user")
	}
	return filepath.Join(dir, homelabUnitName+".service")
}

func (systemdService) installed() bool { return unitLoaded(true, homelabUnitName) }

func (systemdService) install(bin string, out io.Writer) error {
	if err := fsutil.WriteFile(unitPath(), fmt.Appendf(nil, homelabUnit, bin), 0o644); err != nil {
		return err
	}
	u, _ := user.Current()
	if err := run(io.Discard, "loginctl", "enable-linger", u.Username); err != nil {
		fmt.Fprintf(out, "    could not enable lingering (%v); run: sudo loginctl enable-linger %s\n    (without it the homelab stops when you log out)\n", err, u.Username)
	}
	if err := run(out, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if isActiveUser(homelabUnitName) {
		return run(out, "systemctl", "--user", "restart", homelabUnitName)
	}
	return run(out, "systemctl", "--user", "enable", "--now", homelabUnitName)
}

func (systemdService) start(out io.Writer) error {
	return run(io.Discard, "systemctl", "--user", "start", homelabUnitName)
}

func (systemdService) stop(out io.Writer) error {
	return run(io.Discard, "systemctl", "--user", "stop", homelabUnitName)
}

func (systemdService) restart(out io.Writer, noBlock bool) error {
	if noBlock {
		return run(out, "systemctl", "--user", "restart", "--no-block", homelabUnitName)
	}
	return run(out, "systemctl", "--user", "restart", homelabUnitName)
}

func (systemdService) remove(out io.Writer) error {
	if err := run(io.Discard, "systemctl", "--user", "disable", "--now", homelabUnitName); err != nil {
		return err
	}
	os.Remove(unitPath())
	run(io.Discard, "systemctl", "--user", "daemon-reload")
	fmt.Fprintln(out, "    stopped and removed the catflap user service")
	if u, err := user.Current(); err == nil {
		fmt.Fprintf(out, "    (lingering stays on for user %s; other user services may need it.\n     Turn it off with: sudo loginctl disable-linger %s)\n", u.Username, u.Username)
	}
	return nil
}

func (systemdService) logs() string { return "journalctl --user -u catflap -f" }
