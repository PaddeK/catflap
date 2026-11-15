package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/homelab"
)

// homelabService is the homelab's background service on this OS: a systemd
// user service (Linux), a launchd LaunchAgent (macOS) or a scheduled task
// that starts at boot (Windows).
type homelabService interface {
	// binPath is where setup installs the binary the service runs.
	binPath() (string, error)
	installed() bool
	// install writes the service definition for bin and (re)starts it.
	install(bin string, out io.Writer) error
	start(out io.Writer) error
	stop(out io.Writer) error // returns once the homelab has stopped
	// restart returns once the restart is underway: with noBlock, without
	// waiting for anything that needs this session (it may drop).
	restart(out io.Writer, noBlock bool) error
	remove(out io.Writer) error // stop it and delete the definition
	// logs says where the homelab's log is.
	logs() string
}

// newHomelabService returns the service for this OS (see service_*.go).
func newHomelabService(p homelab.Paths) (homelabService, error) {
	return platformService(p)
}

// usesService reports whether the homelab here runs as the service, and
// returns it. Under CATFLAP_DIR (tests, extra instances) it never does.
func usesService(p homelab.Paths) (homelabService, bool) {
	if os.Getenv("CATFLAP_DIR") != "" {
		return nil, false
	}
	svc, err := newHomelabService(p)
	if err != nil || !svc.installed() {
		return nil, false
	}
	return svc, true
}

// HomelabService installs and starts the homelab as this OS's service and
// waits until it runs.
func HomelabService(ctx context.Context, out io.Writer) error {
	p, err := homelab.DefaultPaths()
	if err != nil {
		return err
	}
	svc, err := newHomelabService(p)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return errors.New("run setup homelab as the user who should own the homelab, not as root")
	}
	dst, err := svc.binPath()
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" && homelabRunning(p) {
		// Windows can't replace a running program: stop the old homelab first.
		if err := svc.stop(out); err != nil {
			return err
		}
	}
	bin, err := installSelf(dst)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "==> installed %s\n", bin)
	if err := svc.install(bin, out); err != nil {
		return err
	}
	if _, ok := waitHomelabUp(ctx, p, time.Time{}, 20*time.Second); !ok {
		return fmt.Errorf("the homelab service did not come up; see %s", svc.logs())
	}
	return nil
}

// HomelabLogs says where the homelab's log is on this OS.
func HomelabLogs() string {
	p, err := homelab.DefaultPaths()
	if err != nil {
		return "the homelab's log"
	}
	if os.Getenv("CATFLAP_DIR") != "" {
		return "the output of catflap homelab run"
	}
	svc, err := newHomelabService(p)
	if err != nil {
		return "the output of catflap homelab run"
	}
	return svc.logs()
}

// HomelabServiceInstalled reports whether the homelab runs as a service here.
func HomelabServiceInstalled() bool {
	p, err := homelab.DefaultPaths()
	if err != nil {
		return false
	}
	_, ok := usesService(p)
	return ok
}

// CheckForwardTargets warns about forwards nobody listens on, e.g. port 22
// without an SSH server, with a hint how to turn one on.
func CheckForwardTargets(out io.Writer, cfg homelab.Config) {
	for port, target := range cfg.Forwards {
		c, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err == nil {
			c.Close()
			continue
		}
		fmt.Fprintf(out, "    note: nothing answers on %s (forwarded as port %s) yet\n", target, port)
		if port == "22" && strings.HasPrefix(target, "127.0.0.1:") {
			fmt.Fprintf(out, "    %s\n", sshServerHint())
		}
	}
}

func sshServerHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "turn on the SSH server: System Settings > General > Sharing > Remote Login"
	case "windows":
		return "install the SSH server: Settings > System > Optional features > OpenSSH Server, then in an admin PowerShell:\n      Start-Service sshd; Set-Service sshd -StartupType Automatic"
	}
	return "install and start an SSH server, e.g. sudo apt install openssh-server"
}

// homelabRunning reports whether a homelab answers on the control socket.
func homelabRunning(p homelab.Paths) bool {
	c, err := net.DialTimeout("unix", p.Socket, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// waitHomelabUp waits until the homelab answers, started at or after since.
func waitHomelabUp(ctx context.Context, p homelab.Paths, since time.Time, d time.Duration) (homelab.StatusInfo, bool) {
	var s homelab.StatusInfo
	ok := waitFor(d, func() bool {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		s = homelab.StatusInfo{}
		return homelab.Control(p).Do(cctx, "GET", "/status", nil, &s) == nil && !s.Started.Before(since)
	})
	return s, ok
}

// shutdownHomelab asks the homelab to exit and waits until it has.
func shutdownHomelab(p homelab.Paths, d time.Duration) error {
	if !homelabRunning(p) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := homelab.Control(p).Do(ctx, "POST", "/shutdown", nil, nil); err != nil && homelabRunning(p) {
		return fmt.Errorf("asking the homelab to stop: %v", err)
	}
	if !waitFor(d, func() bool { return !homelabRunning(p) }) {
		return errors.New("the homelab did not stop")
	}
	return nil
}
