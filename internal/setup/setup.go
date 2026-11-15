// Package setup installs catflap as a service: the relay as a hardened
// system service, the homelab as a systemd user service with lingering.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/relay"
)

const relayUnit = `[Unit]
Description=catflap relay (DERP + STUN + enrollment API)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s relay run --config %s
User=catflap
Group=catflap
StateDirectory=catflap
StateDirectoryMode=0700
RuntimeDirectory=catflap
RuntimeDirectoryMode=0700
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
UMask=0077
Restart=always
RestartSec=2
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`

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

// RelayOptions are the inputs of `catflap setup relay`.
type RelayOptions struct {
	Config     relay.Config
	NoFirewall bool
	// Restore sets the relay up from a backup (`--restore`): its config and
	// state replace Config, and no join code is created.
	Restore *relay.Backup
	Force   bool // restore over an existing relay state
	Out     io.Writer
}

// Relay installs and starts the relay service and returns a join code
// (none for a restore).
func Relay(ctx context.Context, o RelayOptions) (string, error) {
	out := o.Out
	if runtime.GOOS != "linux" {
		return "", errors.New("the relay runs on Linux only")
	}
	if os.Geteuid() != 0 {
		return "", errors.New("setup relay needs root (run it with sudo)")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "", errors.New("setup relay needs systemd")
	}
	cfg := o.Config
	if o.Restore != nil {
		cfg = o.Restore.Config
		if cfg.CertMode == "manual" { // the certificate comes from the backup
			cfg.CertFile = filepath.Join(filepath.Dir(relay.DefaultConfigPath), "cert.pem")
			cfg.KeyFile = filepath.Join(filepath.Dir(relay.DefaultConfigPath), "key.pem")
		}
		if fsutil.Exists(cfg.StatePath()) && !o.Force {
			return "", fmt.Errorf("a relay is already set up here (%s); restoring replaces it and every homelab registered with it. Add --force to do that", cfg.StatePath())
		}
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	step := func(s string) { fmt.Fprintf(out, "==> %s\n", s) }

	step("checking DNS and ports")
	if addrs, err := net.DefaultResolver.LookupHost(ctx, cfg.Hostname); err != nil || len(addrs) == 0 {
		fmt.Fprintf(out, "    warning: %s does not resolve yet; Let's Encrypt needs a DNS record pointing to this server\n", cfg.Hostname)
	} else {
		fmt.Fprintf(out, "    %s -> %s\n", cfg.Hostname, strings.Join(addrs, ", "))
	}
	active := isActive("catflap-relay")
	if !active {
		if err := portFree("tcp", cfg.Listen); err != nil {
			return "", fmt.Errorf("%v\n    another program (an old derper?) is using it; stop it first", err)
		}
	}

	step("installing the binary")
	bin, err := installSelf("/usr/local/bin/catflap")
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "    %s\n", bin)

	step("creating the service user")
	if _, err := user.Lookup("catflap"); err != nil {
		nologin := "/usr/sbin/nologin"
		if !fsutil.Exists(nologin) {
			nologin = "/sbin/nologin"
		}
		if err := run(out, "useradd", "--system", "--user-group", "--home-dir", "/var/lib/catflap", "--no-create-home", "--shell", nologin, "catflap"); err != nil {
			return "", err
		}
	}

	if b := o.Restore; b != nil {
		step("restoring the relay state from the backup")
		if err := restoreFiles(b, cfg); err != nil {
			return "", err
		}
		fmt.Fprintf(out, "    %d homelab(s): %s\n", len(b.Homelabs()), strings.Join(b.Homelabs(), ", "))
	}

	step("writing " + relay.DefaultConfigPath)
	if err := os.MkdirAll(filepath.Dir(relay.DefaultConfigPath), 0o755); err != nil {
		return "", err
	}
	if err := fsutil.WriteJSON(relay.DefaultConfigPath, cfg, 0o644); err != nil {
		return "", err
	}
	unitPath := "/etc/systemd/system/catflap-relay.service"
	if err := fsutil.WriteFile(unitPath, fmt.Appendf(nil, relayUnit, bin, relay.DefaultConfigPath), 0o644); err != nil {
		return "", err
	}

	if !o.NoFirewall {
		step("firewall")
		openFirewall(out, cfg)
	}

	step("starting catflap-relay")
	if err := run(out, "systemctl", "daemon-reload"); err != nil {
		return "", err
	}
	if active {
		err = run(out, "systemctl", "restart", "catflap-relay")
	} else {
		err = run(out, "systemctl", "enable", "--now", "catflap-relay")
	}
	if err != nil {
		return "", err
	}

	// Wait for the admin socket, then ask the daemon for a join code.
	adm := relay.AdminClient(cfg)
	if o.Restore != nil {
		if !waitFor(20*time.Second, func() bool { return adm.Do(ctx, "GET", "/status", nil, nil) == nil }) {
			return "", errors.New("relay did not come up (see: journalctl -u catflap-relay)")
		}
		return "", nil
	}
	var resp struct{ Code string }
	deadline := time.Now().Add(20 * time.Second)
	for {
		err = adm.Do(ctx, "POST", "/join-code", nil, &resp)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return "", fmt.Errorf("relay did not come up: %v (see: journalctl -u catflap-relay)", err)
	}
	return resp.Code, nil
}

// restoreFiles writes the backup's state (and certificate) where the relay
// service expects them, owned by its user.
func restoreFiles(b *relay.Backup, cfg relay.Config) error {
	u, err := user.Lookup("catflap")
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	os.Chown(cfg.StateDir, uid, gid)
	if err := fsutil.WriteFile(cfg.StatePath(), b.State, 0o600); err != nil {
		return err
	}
	os.Chown(cfg.StatePath(), uid, gid)
	if cfg.CertMode == "manual" {
		if err := os.MkdirAll(filepath.Dir(cfg.CertFile), 0o755); err != nil {
			return err
		}
		for path, data := range map[string]string{cfg.CertFile: b.CertPEM, cfg.KeyFile: b.KeyPEM} {
			if err := fsutil.WriteFile(path, []byte(data), 0o640); err != nil {
				return err
			}
			os.Chown(path, 0, gid) // readable by the relay's group only
		}
	}
	return nil
}

func openFirewall(out io.Writer, cfg relay.Config) {
	_, port, _ := net.SplitHostPort(cfg.Listen)
	rules := []string{port + "/tcp"}
	if cfg.HTTPPort > 0 {
		rules = append(rules, fmt.Sprint(cfg.HTTPPort)+"/tcp")
	}
	if cfg.STUNPort > 0 {
		rules = append(rules, fmt.Sprint(cfg.STUNPort)+"/udp")
	}
	switch {
	case hasCmd("ufw"):
		o, _ := exec.Command("ufw", "status").Output()
		if !strings.Contains(string(o), "Status: active") {
			fmt.Fprintf(out, "    ufw is installed but inactive; not touching it. If you enable it, allow: 22/tcp %s\n", strings.Join(rules, " "))
			return
		}
		for _, r := range rules {
			run(io.Discard, "ufw", "allow", r)
		}
		fmt.Fprintf(out, "    ufw: allowed %s\n", strings.Join(rules, " "))
	case hasCmd("firewall-cmd") && exec.Command("firewall-cmd", "--state").Run() == nil:
		for _, r := range rules {
			run(io.Discard, "firewall-cmd", "--permanent", "--add-port="+r)
		}
		run(io.Discard, "firewall-cmd", "--reload")
		fmt.Fprintf(out, "    firewalld: allowed %s\n", strings.Join(rules, " "))
	default:
		fmt.Fprintf(out, "    no active ufw/firewalld found; make sure %s are reachable (also in your provider's firewall)\n", strings.Join(rules, " "))
	}
}

// HomelabService installs and starts the homelab as a systemd user service.
func HomelabService(out io.Writer) error {
	if runtime.GOOS != "linux" {
		return errors.New("the homelab service currently supports Linux (systemd) only; run `catflap homelab run` yourself")
	}
	if os.Geteuid() == 0 {
		return errors.New("run setup homelab as the user who should own the homelab, not as root")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	bin, err := installSelf(filepath.Join(home, ".local", "bin", "catflap"))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "==> installed %s\n", bin)
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		unitDir = filepath.Join(x, "systemd", "user")
	}
	if err := fsutil.WriteFile(filepath.Join(unitDir, "catflap.service"), fmt.Appendf(nil, homelabUnit, bin), 0o644); err != nil {
		return err
	}
	u, _ := user.Current()
	if err := run(io.Discard, "loginctl", "enable-linger", u.Username); err != nil {
		fmt.Fprintf(out, "    could not enable lingering (%v); run: sudo loginctl enable-linger %s\n    (without it the homelab stops when you log out)\n", err, u.Username)
	}
	if err := run(out, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if isActiveUser("catflap") {
		return run(out, "systemctl", "--user", "restart", "catflap")
	}
	return run(out, "systemctl", "--user", "enable", "--now", "catflap")
}

// installSelf copies the running binary to dst unless it already is dst.
func installSelf(dst string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if exe == dst {
		return dst, nil
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	// Write next to dst and rename, so a running binary is never truncated.
	if err := fsutil.WriteFile(dst, data, 0o755); err != nil {
		return "", err
	}
	return dst, nil
}

func portFree(network, addr string) error {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("port %s is not free: %v", addr, err)
	}
	return ln.Close()
}

func isActive(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

func isActiveUser(unit string) bool {
	return exec.Command("systemctl", "--user", "is-active", "--quiet", unit).Run() == nil
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func run(out io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
