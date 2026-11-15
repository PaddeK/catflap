// Command catflap sets up and runs every part of a self-hosted tailcat
// setup: the relay, the homelab and its clients.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/client"
	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/homelab"
	"github.com/PaddeK/catflap/internal/relay"
	"github.com/PaddeK/catflap/internal/setup"
)

// Set by the release build (-ldflags "-X main.version=...").
var version = "dev"

const usage = `catflap - reach your homelab from anywhere, on your own relay

Setup (once per machine):
  catflap setup relay --hostname derp.example.com     on the VPS (as root)
  catflap setup homelab --join <join-code>            on the homelab
  catflap enroll <invite>                             on each client

Homelab administration:
  catflap invite <name> [--ttl 15m]    create a one-time invite for a device
  catflap invites [--revoke <name>]    list or revoke open invites
  catflap list                         list enrolled devices
  catflap remove <name>                remove a device (takes effect immediately)
  catflap forward add <port>[=host:port]   make a port reachable through the tunnel
  catflap forward remove <port>        close it again (cuts open connections)
  catflap forward list                 show reachable ports (default: 22 only)
  catflap notify add telegram|ntfy|pushover|command
                                       security alerts on your phone; Telegram adds
                                       the invite bot (/invite, /list, /remove)
  catflap notify list | remove <name> | test [name]

Client:
  catflap forward <alias> [local:]<port>   open localhost:<port> to a forwarded homelab port
  catflap proxy <alias> [port]         ssh ProxyCommand (written to ~/.ssh/config by enroll)
  catflap forget <alias>               delete a homelab entry from this client

Relay administration (as root):
  catflap relay join-code [--ttl 1h]   create a join code for another homelab
  catflap relay status                 show registered homelabs and devices

Other:
  catflap status                       show what is set up on this machine
  catflap restart [relay|homelab]      restart the service, e.g. after an upgrade
  catflap version

Daemons (started by systemd):
  catflap relay run [--config FILE]
  catflap homelab run
  catflap agent                        client tunnel shared by all connections
                                       (started on demand, exits when idle)
`

func main() {
	api.Version = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "catflap:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		return cmdVersion()
	case "setup":
		if len(args) == 0 {
			return errors.New("usage: catflap setup relay|homelab ...")
		}
		switch args[0] {
		case "relay":
			return cmdSetupRelay(ctx, args[1:])
		case "homelab":
			return cmdSetupHomelab(ctx, args[1:])
		}
		return fmt.Errorf("unknown setup target %q (want relay or homelab)", args[0])
	case "relay":
		return cmdRelay(ctx, args)
	case "homelab":
		if len(args) > 0 && args[0] == "run" {
			p, err := homelab.DefaultPaths()
			if err != nil {
				return err
			}
			return homelab.Run(ctx, p)
		}
		return errors.New("usage: catflap homelab run")
	case "invite":
		return cmdInvite(ctx, args)
	case "invites":
		return cmdInvites(ctx, args)
	case "list", "ls":
		return cmdList(ctx)
	case "remove", "rm":
		return cmdRemove(ctx, args)
	case "enroll":
		return cmdEnroll(ctx, args)
	case "proxy":
		return cmdProxy(ctx, args)
	case "agent":
		if len(args) != 0 {
			return errors.New("usage: catflap agent (started automatically by proxy/forward)")
		}
		return client.RunAgent(ctx)
	case "forward", "fwd":
		return cmdForward(ctx, args)
	case "notify":
		return cmdNotify(ctx, args)
	case "forget":
		return cmdForget(args)
	case "status":
		return cmdStatus(ctx)
	case "restart":
		return cmdRestart(ctx, args)
	}
	return fmt.Errorf("unknown command %q (see `catflap help`)", cmd)
}

// parse parses flags that may appear before, between or after positional
// arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("catflap "+name, flag.ContinueOnError)
	return fs
}

// ---- setup ----

func cmdSetupRelay(ctx context.Context, args []string) error {
	fs := newFlags("setup relay")
	cfg := relay.DefaultConfig()
	fs.StringVar(&cfg.Hostname, "hostname", "", "public DNS name of this server (required)")
	fs.StringVar(&cfg.Email, "email", "", "optional contact address for Let's Encrypt")
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "TLS listen address")
	fs.IntVar(&cfg.HTTPPort, "http-port", cfg.HTTPPort, "HTTP port for ACME and redirects (0 = off)")
	fs.IntVar(&cfg.STUNPort, "stun-port", cfg.STUNPort, "UDP STUN port (0 = off)")
	fs.StringVar(&cfg.CertMode, "cert-mode", cfg.CertMode, "letsencrypt or manual")
	fs.StringVar(&cfg.CertFile, "cert", "", "certificate file (cert-mode manual)")
	fs.StringVar(&cfg.KeyFile, "key", "", "key file (cert-mode manual)")
	noFW := fs.Bool("no-firewall", false, "don't touch ufw/firewalld")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if cfg.Hostname == "" {
		return errors.New("--hostname is required, e.g. catflap setup relay --hostname derp.example.com")
	}
	code, err := setup.Relay(ctx, setup.RelayOptions{Config: cfg, NoFirewall: *noFW, Out: os.Stdout})
	if err != nil {
		return err
	}
	fmt.Printf(`
Relay is running.  Logs: journalctl -u catflap-relay -f

Next, on your homelab (as your normal user, valid for 1 hour, one use):

  catflap setup homelab --join %s

`, code)
	return nil
}

func cmdSetupHomelab(ctx context.Context, args []string) error {
	fs := newFlags("setup homelab")
	join := fs.String("join", "", "join code printed by `catflap setup relay` (required)")
	name := fs.String("name", "homelab", "name of this homelab (default ssh alias on clients)")
	force := fs.Bool("force", false, "replace an existing homelab setup")
	noService := fs.Bool("no-service", false, "don't install the systemd user service")
	var forwards forwardFlag
	fs.Var(&forwards, "forward", "tunnel port to expose, PORT or PORT=HOST:PORT (repeatable; default 22)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *join == "" {
		return errors.New("--join is required (the code comes from `catflap setup relay`)")
	}
	p, err := homelab.DefaultPaths()
	if err != nil {
		return err
	}
	cfg, err := homelab.Join(ctx, p, homelab.JoinOptions{JoinCode: *join, Name: *name, Forwards: forwards.m, Force: *force})
	if err != nil {
		return err
	}
	fmt.Printf("==> registered homelab %q with relay %s\n", cfg.Name, cfg.Relay)
	if *noService {
		fmt.Println("Start it with: catflap homelab run")
	} else if err := setup.HomelabService(os.Stdout); err != nil {
		return err
	}
	fmt.Printf(`
Homelab is running.  Logs: journalctl --user -u catflap -f

Add your first device:

  catflap invite my-laptop

`)
	return nil
}

type forwardFlag struct{ m map[string]string }

func (f *forwardFlag) String() string { return fmt.Sprint(f.m) }
func (f *forwardFlag) Set(s string) error {
	port, target, err := homelab.ParseForward(s)
	if err != nil {
		return err
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[strconv.Itoa(port)] = target
	return nil
}

// ---- relay ----

func cmdRelay(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: catflap relay run|join-code|status")
	}
	fs := newFlags("relay " + args[0])
	cfgPath := fs.String("config", relay.DefaultConfigPath, "relay config file")
	ttl := fs.Duration("ttl", time.Hour, "join code lifetime (join-code)")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	cfg, err := relay.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "run":
		return relay.Run(ctx, cfg)
	case "join-code":
		var resp struct{ Code, Expires string }
		if err := relay.AdminClient(cfg).Do(ctx, "POST", "/join-code?ttl="+ttl.String(), nil, &resp); err != nil {
			return adminErr(err)
		}
		fmt.Printf("On the homelab (valid until %s, one use):\n\n  catflap setup homelab --join %s\n\n", resp.Expires, resp.Code)
		return nil
	case "status":
		var st struct {
			Relay  string
			Agents []struct {
				ID, Name, NodeKey string
				Created           time.Time
				Clients           map[string]string
			}
			OpenInvites, JoinCodes int
		}
		if err := relay.AdminClient(cfg).Do(ctx, "GET", "/status", nil, &st); err != nil {
			return adminErr(err)
		}
		fmt.Printf("relay %s: %d homelab(s), %d open invite(s), %d unused join code(s)\n", st.Relay, len(st.Agents), st.OpenInvites, st.JoinCodes)
		for _, a := range st.Agents {
			fmt.Printf("\n  homelab %q (id %s, since %s)\n", a.Name, a.ID, a.Created.Format("2006-01-02"))
			names := make([]string, 0, len(a.Clients))
			for _, n := range a.Clients {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Printf("    device %s\n", n)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown relay command %q", args[0])
}

func adminErr(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return err // the relay answered; its message is the error
	}
	if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
		return errors.New("permission denied: run this as root (sudo)")
	}
	if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "connection refused") {
		return errors.New("the relay is not running (systemctl status catflap-relay)")
	}
	return err
}

// ---- homelab admin ----

func control() (*api.Client, error) {
	p, err := homelab.DefaultPaths()
	if err != nil {
		return nil, err
	}
	return homelab.Control(p), nil
}

func ctlErr(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return errors.New(ae.Msg) // the homelab answered; show just its message
	}
	// Only failures to reach the socket mean the daemon isn't running.
	if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "connection refused") {
		return errors.New("the homelab is not running here (systemctl --user status catflap)")
	}
	return err
}

func cmdInvite(ctx context.Context, args []string) error {
	fs := newFlags("invite")
	ttl := fs.Duration("ttl", 15*time.Minute, "how long the invite stays valid (max 2160h)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: catflap invite <device-name> [--ttl 15m]")
	}
	c, err := control()
	if err != nil {
		return err
	}
	var res homelab.InviteResult
	if err := c.Do(ctx, "POST", "/invite", map[string]any{"name": pos[0], "ttl": *ttl}, &res); err != nil {
		return ctlErr(err)
	}
	fmt.Printf(`Invite for %q (one use, valid until %s):

  catflap enroll %s

Run that on the device. Treat the code like a password until it is used.
`, res.Name, res.Expires.Local().Format("2006-01-02 15:04"), res.Code)
	return nil
}

func cmdInvites(ctx context.Context, args []string) error {
	fs := newFlags("invites")
	revoke := fs.String("revoke", "", "revoke the open invite for this device name")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := control()
	if err != nil {
		return err
	}
	if *revoke != "" {
		if err := c.Do(ctx, "DELETE", "/invites/"+*revoke, nil, nil); err != nil {
			return ctlErr(err)
		}
		fmt.Printf("revoked the invite for %q\n", *revoke)
		return nil
	}
	var invs []homelab.InviteInfo
	if err := c.Do(ctx, "GET", "/invites", nil, &invs); err != nil {
		return ctlErr(err)
	}
	if len(invs) == 0 {
		fmt.Println("no open invites")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVALID UNTIL")
	for _, i := range invs {
		fmt.Fprintf(tw, "%s\t%s\n", i.Name, i.Expires.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

func cmdList(ctx context.Context) error {
	c, err := control()
	if err != nil {
		return err
	}
	var devs []homelab.Device
	if err := c.Do(ctx, "GET", "/devices", nil, &devs); err != nil {
		return ctlErr(err)
	}
	if len(devs) == 0 {
		fmt.Println("no devices yet; add one with: catflap invite <name>")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tHOSTNAME\tADDED\tSSH KEY\tNODE KEY")
	for _, d := range devs {
		ssh := "-"
		if d.SSHKey != "" {
			ssh = strings.Fields(d.SSHKey)[0]
		}
		nk := d.NodeKey
		if len(nk) > 20 {
			nk = nk[:20] + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Name, d.Hostname, d.Added.Local().Format("2006-01-02"), ssh, nk)
	}
	return tw.Flush()
}

func cmdRemove(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: catflap remove <device-name>")
	}
	c, err := control()
	if err != nil {
		return err
	}
	var res homelab.RemoveResult
	if err := c.Do(ctx, "DELETE", "/devices/"+args[0], nil, &res); err != nil {
		return ctlErr(err)
	}
	fmt.Printf("removed %q: disconnected", args[0])
	if res.SSHKeyRemoved {
		fmt.Print(", SSH key removed")
	}
	if res.RelayUpdated {
		fmt.Println(", relay updated")
	} else {
		fmt.Println("; relay not reachable right now, the homelab keeps retrying")
	}
	if res.SSHKeyKept {
		fmt.Println("note: its SSH key was in authorized_keys before catflap and stays there")
	}
	return nil
}

// ---- forwards ----

const forwardUsage = `usage:
  on the homelab:  catflap forward add <port>[=host:port] | remove <port> | list
  on a client:     catflap forward <alias> [local:]<port>`

func cmdForward(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(forwardUsage)
	}
	switch args[0] {
	case "add", "remove", "rm", "list", "ls":
		return cmdForwardAdmin(ctx, args[0], args[1:])
	}
	if len(args) != 2 {
		return errors.New(forwardUsage)
	}
	// [local:]port
	lp, rp, hasLocal := strings.Cut(args[1], ":")
	if !hasLocal {
		lp, rp = "", lp
	}
	port, err := strconv.ParseUint(rp, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("bad port %q", args[1])
	}
	var local uint64
	if hasLocal {
		if local, err = strconv.ParseUint(lp, 10, 16); err != nil || local == 0 {
			return fmt.Errorf("bad local port %q", lp)
		}
	}
	return client.Forward(ctx, client.ForwardOptions{Alias: args[0], Port: uint16(port), LocalPort: uint16(local), Out: os.Stdout})
}

func cmdForwardAdmin(ctx context.Context, sub string, args []string) error {
	c, err := control()
	if err != nil {
		return err
	}
	switch sub {
	case "add":
		if len(args) == 0 {
			return errors.New("usage: catflap forward add <port>[=host:port] ...  e.g. 1880 or 8080=192.168.1.50:80")
		}
		for _, a := range args {
			port, target, err := homelab.ParseForward(a)
			if err != nil {
				return err
			}
			var res map[string]bool
			if err := c.Do(ctx, "PUT", fmt.Sprintf("/forwards/%d", port), map[string]string{"target": target}, &res); err != nil {
				return ctlErr(err)
			}
			if res["changed"] {
				fmt.Printf("port %d -> %s is reachable through the tunnel now\n", port, target)
			} else {
				fmt.Printf("port %d -> %s was already forwarded\n", port, target)
			}
			fmt.Printf("  on a client: catflap forward <alias> %d\n", port)
		}
		return nil
	case "remove", "rm":
		if len(args) == 0 {
			return errors.New("usage: catflap forward remove <port> ...")
		}
		for _, a := range args {
			if err := c.Do(ctx, "DELETE", "/forwards/"+a, nil, nil); err != nil {
				return ctlErr(err)
			}
			fmt.Printf("port %s is closed\n", a)
			if a == "22" {
				fmt.Println("note: ssh through catflap needs port 22; add it back with: catflap forward add 22")
			}
		}
		return nil
	}
	var fw []homelab.ForwardInfo
	if err := c.Do(ctx, "GET", "/forwards", nil, &fw); err != nil {
		return ctlErr(err)
	}
	if len(fw) == 0 {
		fmt.Println("no ports are forwarded; add ssh with: catflap forward add 22")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tTARGET")
	for _, f := range fw {
		fmt.Fprintf(tw, "%d\t%s\n", f.Port, f.Target)
	}
	return tw.Flush()
}

// ---- client ----

func cmdEnroll(ctx context.Context, args []string) error {
	fs := newFlags("enroll")
	var o client.EnrollOptions
	fs.StringVar(&o.Alias, "alias", "", "ssh alias for the homelab (default: the homelab's name)")
	fs.StringVar(&o.SSHKeyFile, "ssh-key", "", "SSH public key (.pub) to authorize (default: ~/.ssh/id_ed25519.pub or a new key)")
	fs.BoolVar(&o.NoSSHKey, "no-ssh-key", false, "don't send an SSH key")
	fs.BoolVar(&o.NoSSHConfig, "no-ssh-config", false, "don't write ~/.ssh/config")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: catflap enroll <invite> [--alias NAME] [--ssh-key FILE.pub]")
	}
	o.Invite, o.Out = pos[0], os.Stdout
	return client.Enroll(ctx, o)
}

func cmdProxy(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: catflap proxy <alias> [port]")
	}
	port := uint64(22)
	if len(args) == 2 {
		var err error
		if port, err = strconv.ParseUint(args[1], 10, 16); err != nil || port == 0 {
			return fmt.Errorf("bad port %q", args[1])
		}
	}
	return client.Proxy(ctx, args[0], uint16(port))
}

func cmdForget(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: catflap forget <alias>")
	}
	st, path, err := client.Load()
	if err != nil {
		return err
	}
	if _, ok := st.Servers[args[0]]; !ok {
		return fmt.Errorf("no homelab %q on this client", args[0])
	}
	delete(st.Servers, args[0])
	if err := client.Save(path, st); err != nil {
		return err
	}
	if err := client.RemoveSSHConfig(args[0]); err != nil {
		return err
	}
	fmt.Printf("forgot %q (the homelab still lists this device until you run `catflap remove` there)\n", args[0])
	return nil
}

// ---- status / version ----

func cmdStatus(ctx context.Context) error {
	found := false
	if cfg, err := relay.LoadConfig(relay.DefaultConfigPath); err == nil {
		found = true
		fmt.Printf("relay:   %s (config %s; details: sudo catflap relay status)\n", cfg.PublicAddr(), relay.DefaultConfigPath)
	}
	if p, err := homelab.DefaultPaths(); err == nil {
		if cfg, err := homelab.LoadConfig(p); err == nil {
			found = true
			var s homelab.StatusInfo
			if err := homelab.Control(p).Do(ctx, "GET", "/status", nil, &s); err != nil {
				fmt.Printf("homelab: %q via %s - NOT RUNNING (%v)\n", cfg.Name, cfg.Relay, ctlErr(err))
			} else {
				ver := s.Version
				if ver == "" {
					ver = "(an older version)"
				}
				sync := ""
				if s.NeedsSync {
					sync = ", relay sync pending"
				}
				fmt.Printf("homelab: %q via %s - running catflap %s, %d device(s), %d open invite(s), forwarding %s%s\n", s.Name, s.Relay, ver, s.Devices, s.Invites, strings.Join(s.Forwards, ", "), sync)
			}
		}
	}
	if st, _, err := client.Load(); err == nil && len(st.Servers) > 0 {
		found = true
		for _, a := range st.Aliases() {
			s := st.Servers[a]
			fmt.Printf("client:  %s -> homelab %q via %s (this device is %q; ssh %s)\n", a, s.Homelab, s.Relay, s.DeviceName, a)
		}
	}
	if !found {
		fmt.Println("nothing set up on this machine yet (see `catflap help`)")
	}
	return nil
}

// cmdRestart restarts the relay or homelab service (whichever is set up here).
func cmdRestart(ctx context.Context, args []string) error {
	role := ""
	switch len(args) {
	case 0:
	case 1:
		role = args[0]
	default:
		return errors.New("usage: catflap restart [relay|homelab]")
	}
	if role == "" {
		hasRelay := fsutil.Exists(relay.DefaultConfigPath)
		hasHomelab := false
		if p, err := homelab.DefaultPaths(); err == nil {
			hasHomelab = fsutil.Exists(p.Config)
		}
		switch {
		case hasRelay && hasHomelab:
			return errors.New("both a relay and a homelab are set up here: catflap restart relay|homelab")
		case hasRelay:
			role = "relay"
		case hasHomelab:
			role = "homelab"
		default:
			return errors.New("nothing to restart here: no relay or homelab is set up.\n(On a client there is no service: its agent switches to a new version by itself.)")
		}
	}
	switch role {
	case "relay":
		return setup.RestartRelay(ctx, os.Stdout)
	case "homelab":
		return setup.RestartHomelab(ctx, os.Stdout)
	}
	return fmt.Errorf("unknown role %q (want relay or homelab)", role)
}

func cmdVersion() error {
	fmt.Printf("catflap %s\n", version)
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, modified string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					modified = " (modified)"
				}
			}
		}
		if rev != "" {
			fmt.Printf("  commit %s%s\n", rev, modified)
		}
		for _, d := range bi.Deps {
			if d.Path == "github.com/tailscale/tailcat" || d.Path == "tailscale.com" {
				fmt.Printf("  %s %s\n", d.Path, d.Version)
			}
		}
		fmt.Printf("  %s\n", bi.GoVersion)
	}
	return nil
}
