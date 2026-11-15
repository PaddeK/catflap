package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/client"
	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/homelab"
	"github.com/PaddeK/catflap/internal/relay"
)

// `catflap uninstall`: removes what catflap set up on this machine.

// UninstallPlan is what an uninstall would remove here.
type UninstallPlan struct {
	Relay          *relay.Config // relay to remove (we are root)
	RelaySkipped   bool          // a relay is set up, but needs root (not removed)
	Homelab        *homelab.Info
	HomelabPaths   homelab.Paths
	Client         bool
	Enrollments    []client.Enrollment
	Binary         string // binary to delete ("" = keep)
	BinaryKeptNote string
}

// Empty reports whether there is nothing to do.
func (p *UninstallPlan) Empty() bool {
	return p.Relay == nil && p.Homelab == nil && !p.Client && p.Binary == ""
}

// PlanUninstall works out what to remove. role is "", "relay", "homelab"
// or "client"; "" means everything set up here.
func PlanUninstall(role string, keepBinary bool) (*UninstallPlan, error) {
	switch role {
	case "", "relay", "homelab", "client":
	default:
		return nil, fmt.Errorf("unknown role %q (want relay, homelab or client)", role)
	}
	p := &UninstallPlan{}
	remaining := 0      // roles of this user that stay (they use this user's binary)
	relayStays := false // the relay stays (it uses /usr/local/bin/catflap)

	if fsutil.Exists(relay.DefaultConfigPath) {
		switch {
		case role != "" && role != "relay":
			relayStays = true
		case os.Geteuid() != 0:
			if role == "relay" {
				return nil, errors.New("removing the relay needs root: sudo catflap uninstall relay")
			}
			p.RelaySkipped = true
			relayStays = true
		default:
			cfg, err := relay.LoadConfig(relay.DefaultConfigPath)
			if err != nil {
				return nil, err
			}
			p.Relay = &cfg
		}
	} else if role == "relay" {
		return nil, errors.New("there is no relay set up here")
	}

	hp, err := homelab.DefaultPaths()
	if err != nil {
		return nil, err
	}
	if info, ok := homelab.Inspect(hp); ok {
		if role == "" || role == "homelab" {
			if os.Geteuid() == 0 && os.Getenv("CATFLAP_DIR") == "" && role == "homelab" {
				return nil, errors.New("the homelab belongs to your normal user: run catflap uninstall without sudo")
			}
			p.Homelab, p.HomelabPaths = info, hp
		} else {
			remaining++
		}
	} else if role == "homelab" {
		return nil, errors.New("there is no homelab set up here (for this user)")
	}

	if client.IsSetUp() {
		if role == "" || role == "client" {
			p.Client, p.Enrollments = true, client.Enrollments()
		} else {
			remaining++
		}
	} else if role == "client" {
		return nil, errors.New("this machine is not set up as a client (for this user)")
	}

	exe, err := os.Executable()
	if err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
	}
	switch {
	case keepBinary || err != nil:
	case remaining > 0, relayStays && exe == "/usr/local/bin/catflap":
		p.BinaryKeptNote = "kept the catflap binary: something else on this machine still uses it"
	case !standardLocation(exe):
		p.BinaryKeptNote = "kept " + exe + " (not where the install scripts put catflap; delete it yourself if you like)"
	default:
		p.Binary = exe
	}
	return p, nil
}

// standardLocation reports whether exe is where install.sh/install.ps1 or
// `catflap setup` put catflap.
func standardLocation(exe string) bool {
	home, _ := os.UserHomeDir()
	switch {
	case runtime.GOOS == "windows":
		return strings.EqualFold(exe, filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "catflap", "catflap.exe"))
	case exe == "/usr/local/bin/catflap":
		return true
	case home != "" && exe == filepath.Join(home, ".local", "bin", "catflap"):
		return true
	}
	return false
}

// Describe prints the plan.
func (p *UninstallPlan) Describe(w io.Writer) {
	fmt.Fprintln(w, "This removes from this machine:")
	if p.Relay != nil {
		fmt.Fprintf(w, "\n  relay %s\n", p.Relay.PublicAddr())
		fmt.Fprintf(w, "    stops and removes the catflap-relay service and the catflap user,\n")
		fmt.Fprintf(w, "    deletes %s and %s (keys, certificates).\n", filepath.Dir(relay.DefaultConfigPath), p.Relay.StateDir)
		fmt.Fprintf(w, "    Every homelab and device using this relay stops working.\n")
	}
	if h := p.Homelab; h != nil {
		fmt.Fprintf(w, "\n  homelab %q (relay %s)\n", h.Name, h.Relay)
		fmt.Fprintf(w, "    stops and removes the service, removes it from the relay,\n")
		if len(h.Devices) > 0 && h.AuthKeys != "" {
			fmt.Fprintf(w, "    removes the SSH keys of %d device(s) (%s) from %s,\n", len(h.Devices), strings.Join(h.Devices, ", "), h.AuthKeys)
		}
		fmt.Fprintf(w, "    deletes its config, state and notification channels.\n")
		if len(h.Devices) > 0 {
			fmt.Fprintf(w, "    Its devices can't connect anymore.\n")
		}
	}
	if p.Client {
		fmt.Fprintf(w, "\n  client\n")
		for _, e := range p.Enrollments {
			fmt.Fprintf(w, "    forgets homelab %q (ssh alias %q, this device is %q there)\n", e.Homelab, e.Alias, e.Device)
		}
		fmt.Fprintf(w, "    stops the background agent, deletes this device's keys and settings.\n")
	}
	if p.Binary != "" {
		fmt.Fprintf(w, "\n  the catflap program: %s\n", p.Binary)
	}
	if p.RelaySkipped {
		fmt.Fprintf(w, "\nNot included: the relay set up here (needs root: sudo catflap uninstall relay).\n")
	}
}

// Uninstall carries out the plan.
func Uninstall(ctx context.Context, p *UninstallPlan, out io.Writer) error {
	step := func(s string) { fmt.Fprintf(out, "==> %s\n", s) }
	if h := p.Homelab; h != nil {
		step("removing homelab " + h.Name)
		if err := stopHomelabService(out, p.HomelabPaths); err != nil {
			return err
		}
		res, err := homelab.Uninstall(ctx, p.HomelabPaths)
		if err != nil {
			return err
		}
		if res.RelayErr != nil {
			fmt.Fprintf(out, "    could not remove it from the relay (%v);\n    on the relay run: sudo catflap relay remove %s\n", res.RelayErr, h.Name)
		} else {
			fmt.Fprintf(out, "    removed from relay %s\n", h.Relay)
		}
		if res.KeysRemoved > 0 {
			fmt.Fprintf(out, "    removed %d SSH key(s) from %s\n", res.KeysRemoved, h.AuthKeys)
		}
		for _, f := range res.FilesRemoved {
			fmt.Fprintf(out, "    deleted %s\n", f)
		}
		if os.Getenv("CATFLAP_DIR") == "" {
			if u, err := user.Current(); err == nil {
				fmt.Fprintf(out, "    (lingering stays on for user %s; other user services may need it.\n     Turn it off with: sudo loginctl disable-linger %s)\n", u.Username, u.Username)
			}
		}
	}
	if p.Client {
		step("removing client")
		removed, err := client.Uninstall()
		if err != nil {
			return err
		}
		for _, e := range p.Enrollments {
			fmt.Fprintf(out, "    removed ssh alias %q\n", e.Alias)
		}
		for _, f := range removed {
			fmt.Fprintf(out, "    deleted %s\n", f)
		}
		for _, e := range p.Enrollments {
			fmt.Fprintf(out, "    on homelab %q, remove this device: catflap remove %s (or /remove %s in its Telegram bot, if you use it)\n", e.Homelab, e.Device, e.Device)
		}
		if home, _ := os.UserHomeDir(); fsutil.Exists(filepath.Join(home, ".ssh", "catflap_ed25519")) {
			fmt.Fprintf(out, "    kept your SSH key %s; delete it if you don't use it elsewhere\n", filepath.Join(home, ".ssh", "catflap_ed25519"))
		}
	}
	if p.Relay != nil {
		step("removing relay " + p.Relay.PublicAddr())
		if err := uninstallRelay(out, *p.Relay); err != nil {
			return err
		}
	}
	if p.Binary != "" {
		step("removing " + p.Binary)
		if err := removeBinary(p.Binary, out); err != nil {
			fmt.Fprintf(out, "    could not remove it (%v); delete it yourself\n", err)
		}
	} else if p.BinaryKeptNote != "" {
		fmt.Fprintf(out, "    %s\n", p.BinaryKeptNote)
	}
	fmt.Fprintln(out, "done")
	return nil
}

// stopHomelabService stops and removes the user service. Under CATFLAP_DIR
// (tests, extra instances) there is no service; the daemon must be stopped.
func stopHomelabService(out io.Writer, hp homelab.Paths) error {
	if os.Getenv("CATFLAP_DIR") == "" && runtime.GOOS == "linux" && hasCmd("systemctl") && unitLoaded(true, homelabService) {
		if err := run(io.Discard, "systemctl", "--user", "disable", "--now", homelabService); err != nil {
			return err
		}
		home, _ := os.UserHomeDir()
		unitDir := filepath.Join(home, ".config", "systemd", "user")
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			unitDir = filepath.Join(x, "systemd", "user")
		}
		os.Remove(filepath.Join(unitDir, "catflap.service"))
		run(io.Discard, "systemctl", "--user", "daemon-reload")
		fmt.Fprintln(out, "    stopped and removed the catflap user service")
	}
	// Still running (started by hand with `catflap homelab run`)?
	if c, err := net.DialTimeout("unix", hp.Socket, time.Second); err == nil {
		c.Close()
		return errors.New("the homelab is still running (started without the service?); stop `catflap homelab run` first, then run uninstall again")
	}
	return nil
}

func uninstallRelay(out io.Writer, cfg relay.Config) error {
	if unitLoaded(false, relayService) {
		if err := run(io.Discard, "systemctl", "disable", "--now", relayService); err != nil {
			return err
		}
	}
	os.Remove("/etc/systemd/system/catflap-relay.service")
	run(io.Discard, "systemctl", "daemon-reload")
	fmt.Fprintln(out, "    stopped and removed the catflap-relay service")
	for _, dir := range []string{cfg.StateDir, filepath.Dir(relay.DefaultConfigPath), filepath.Dir(cfg.AdminSocketPath())} {
		if !strings.Contains(dir, "catflap") || dir == "/" { // never anything but catflap's own directories
			fmt.Fprintf(out, "    left %s alone (not a catflap directory)\n", dir)
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		fmt.Fprintf(out, "    deleted %s\n", dir)
	}
	if _, err := user.Lookup("catflap"); err == nil {
		if err := run(io.Discard, "userdel", "catflap"); err != nil {
			fmt.Fprintf(out, "    could not remove the catflap user: %v\n", err)
		} else {
			fmt.Fprintln(out, "    removed the catflap user")
		}
	}
	_, port, _ := net.SplitHostPort(cfg.Listen)
	ports := []string{port + "/tcp"}
	if cfg.HTTPPort > 0 {
		ports = append(ports, fmt.Sprint(cfg.HTTPPort)+"/tcp")
	}
	if cfg.STUNPort > 0 {
		ports = append(ports, fmt.Sprint(cfg.STUNPort)+"/udp")
	}
	fmt.Fprintf(out, "    firewall: setup opened %s; close them if nothing else needs them\n", strings.Join(ports, " "))
	if hasCmd("ufw") {
		fmt.Fprintf(out, "      e.g. ufw delete allow %s\n", strings.Join(ports, "; ufw delete allow "))
	}
	return nil
}
