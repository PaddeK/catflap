// Command catflap sets up and runs every part of a self-hosted tailcat
// setup: the relay, the homelab and its clients.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/client"
	"github.com/PaddeK/catflap/internal/codes"
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
  catflap enroll --request HOMELAB             ... or without invite: tap to approve

Homelab administration:
  catflap invite <name> [--ttl 15m]    create a one-time invite for a device
                                       (--replace: re-enroll an existing device)
  catflap invites [--revoke <name>]    list or revoke open invites
  catflap list                         list enrolled devices
  catflap requests [on [--for 1h] | off]   let new devices ask to join (tap to approve)
  catflap approve|deny <code>          decide a pending request
  catflap remove <name>                remove a device (takes effect immediately)
  catflap forward add <port>[=host:port]   make a port reachable through the tunnel
  catflap forward remove <port>        close it again (cuts open connections)
  catflap forward list                 show reachable ports (default: 22 only)
  catflap homelab move --join <code>   move to another relay (new hostname/port);
                                       prints a re-enroll invite per device
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
  catflap relay remove <homelab>       forget a homelab that's gone (uninstall does this itself)
  catflap relay backup FILE            back up the relay (move it with setup relay --restore FILE)

Other:
  catflap status                       show what is set up on this machine
  catflap restart [relay|homelab]      restart the service, e.g. after an upgrade
  catflap uninstall [relay|homelab|client]   remove catflap from this machine (asks first)
  catflap version

Daemons (started by the service manager):
  catflap relay run [--config FILE]
  catflap homelab run [--service]     --service: log to a file, start again after errors
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
		if len(args) > 0 && args[0] == "move" {
			return cmdHomelabMove(ctx, args[1:])
		}
		if len(args) > 0 && args[0] == "run" {
			fs := newFlags("homelab run")
			service := fs.Bool("service", false, "run as the macOS/Windows service: log to a file, start again after errors")
			if _, err := parse(fs, args[1:]); err != nil {
				return err
			}
			p, err := homelab.DefaultPaths()
			if err != nil {
				return err
			}
			if *service {
				return homelab.RunService(ctx, p)
			}
			return homelab.Run(ctx, p)
		}
		return errors.New("usage: catflap homelab run|move")
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
	case "requests":
		return cmdRequests(ctx, args)
	case "approve", "deny":
		return cmdDecide(ctx, cmd, args)
	case "forget":
		return cmdForget(args)
	case "status":
		return cmdStatus(ctx)
	case "restart":
		return cmdRestart(ctx, args)
	case "uninstall":
		return cmdUninstall(ctx, args)
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
	restore := fs.String("restore", "", "set the relay up from a backup file (made with: catflap relay backup), e.g. on a new server")
	force := fs.Bool("force", false, "with --restore: replace a relay that is set up here already")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	var backup *relay.Backup
	if *restore != "" {
		b, err := relay.ReadBackup(*restore)
		if err != nil {
			return err
		}
		if cfg.Hostname != "" && cfg.Hostname != b.Config.Hostname {
			return fmt.Errorf("the backup is for %s; a restore keeps the hostname, because homelabs and devices know the relay by it", b.Config.Hostname)
		}
		backup = b
	} else if cfg.Hostname == "" {
		return errors.New("--hostname is required, e.g. catflap setup relay --hostname derp.example.com")
	}
	code, err := setup.Relay(ctx, setup.RelayOptions{Config: cfg, NoFirewall: *noFW, Restore: backup, Force: *force, Out: os.Stdout})
	if err != nil {
		return err
	}
	if backup != nil {
		fmt.Printf(`
Relay %s is restored and running (backup from %s).  Logs: journalctl -u catflap-relay -f

Now point the DNS records of %s to this server. Homelabs and devices
reconnect on their own once DNS has updated; then remove the old relay
(sudo catflap uninstall on the old server).

`, backup.Config.Hostname, backup.Created.Local().Format("2006-01-02 15:04"), backup.Config.Hostname)
		return nil
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
	noService := fs.Bool("no-service", false, "don't install the background service (systemd user service, LaunchAgent or scheduled task)")
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
	} else if err := setup.HomelabService(ctx, os.Stdout); err != nil {
		return err
	}
	setup.CheckForwardTargets(os.Stdout, cfg)
	if *noService {
		return nil
	}
	fmt.Printf(`
Homelab is running.  Logs: %s

Add your first device:

  catflap invite my-laptop

`, setup.HomelabLogs())
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
		return errors.New("usage: catflap relay run|join-code|status|remove|backup")
	}
	fs := newFlags("relay " + args[0])
	cfgPath := fs.String("config", relay.DefaultConfigPath, "relay config file")
	ttl := fs.Duration("ttl", time.Hour, "join code lifetime (join-code)")
	yes := fs.Bool("yes", false, "don't ask for confirmation (remove)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	cfg, err := relay.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "backup":
		if len(pos) != 1 {
			return errors.New("usage: sudo catflap relay backup FILE")
		}
		b, err := relay.WriteBackup(cfg, pos[0])
		if err != nil {
			return adminErr(err)
		}
		fmt.Printf(`Backup of relay %s written to %s: %d homelab(s).

It contains the relay's private key: keep it as private as the server.
Restore it on a new server with:  sudo catflap setup relay --restore FILE
Homelabs set up after this backup aren't in it; make a new backup then.
`, cfg.Hostname, pos[0], len(b.Homelabs()))
		return nil
	case "remove", "rm":
		if len(pos) != 1 {
			return errors.New("usage: sudo catflap relay remove <homelab name or id> [--yes]")
		}
		return cmdRelayRemove(ctx, cfg, pos[0], *yes)
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

// cmdRelayRemove removes a homelab from the relay: for homelabs that are gone
// without `catflap uninstall` (which de-registers by itself).
func cmdRelayRemove(ctx context.Context, cfg relay.Config, which string, yes bool) error {
	var st struct {
		Agents []struct {
			ID, Name string
			Created  time.Time
			Clients  map[string]string
		}
	}
	if err := relay.AdminClient(cfg).Do(ctx, "GET", "/status", nil, &st); err != nil {
		return adminErr(err)
	}
	type match = struct {
		ID, Name string
		Created  time.Time
		Clients  map[string]string
	}
	var found []match
	for _, a := range st.Agents {
		if a.ID == which || a.Name == which {
			found = append(found, match(a))
		}
	}
	switch len(found) {
	case 0:
		return fmt.Errorf("no homelab %q on this relay (see: sudo catflap relay status)", which)
	case 1:
	default:
		var ids []string
		for _, f := range found {
			ids = append(ids, fmt.Sprintf("%s (since %s)", f.ID, f.Created.Format("2006-01-02")))
		}
		return fmt.Errorf("%d homelabs are called %q; pick one by id: %s", len(found), which, strings.Join(ids, ", "))
	}
	h := found[0]
	fmt.Printf("homelab %q (id %s, since %s) with %d device(s)\n", h.Name, h.ID, h.Created.Format("2006-01-02"), len(h.Clients))
	fmt.Println("Removing it from the relay stops it and its devices from using this relay.")
	if !yes && !confirm("Remove it?") {
		return errors.New("cancelled")
	}
	if err := relay.AdminClient(cfg).Do(ctx, "DELETE", "/agents/"+h.ID, nil, nil); err != nil {
		return adminErr(err)
	}
	fmt.Printf("removed homelab %q from the relay.\nConnections it still has end when they reconnect; `sudo catflap restart` ends them now.\n", h.Name)
	return nil
}

// confirm asks a yes/no question on the terminal; without one, it's a no.
func confirm(question string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Println("(no terminal to confirm on; add --yes)")
		return false
	}
	return newPrompter().yes(question, false)
}

// cmdUninstall removes what catflap set up on this machine.
func cmdUninstall(ctx context.Context, args []string) error {
	fs := newFlags("uninstall")
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	keep := fs.Bool("keep-binary", false, "keep the catflap program itself")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	role := ""
	switch len(pos) {
	case 0:
	case 1:
		role = pos[0]
	default:
		return errors.New("usage: catflap uninstall [relay|homelab|client] [--yes] [--keep-binary]")
	}
	plan, err := setup.PlanUninstall(role, *keep)
	if err != nil {
		return err
	}
	if plan.Empty() {
		msg := "nothing of catflap is set up here"
		if plan.RelaySkipped {
			msg += " for this user; the relay needs root: sudo catflap uninstall relay"
		}
		if plan.BinaryKeptNote != "" {
			msg += "\n" + plan.BinaryKeptNote
		}
		fmt.Println(msg)
		return nil
	}
	plan.Describe(os.Stdout)
	fmt.Println()
	if !*yes && !confirm("This can't be undone. Continue?") {
		return errors.New("cancelled, nothing was changed")
	}
	return setup.Uninstall(ctx, plan, os.Stdout)
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
	if notRunning(err) {
		return fmt.Errorf("the homelab is not running here (%s)", startHint())
	}
	return err
}

// notRunning reports whether err means the control socket isn't there.
func notRunning(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

func startHint() string {
	if setup.HomelabServiceInstalled() {
		return "start it with: catflap restart; logs: " + setup.HomelabLogs()
	}
	return "start it with: catflap homelab run"
}

func cmdInvite(ctx context.Context, args []string) error {
	fs := newFlags("invite")
	ttl := fs.Duration("ttl", 15*time.Minute, "how long the invite stays valid (max 2160h)")
	replace := fs.Bool("replace", false, "re-enroll an existing device: its old entry and SSH key are replaced")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: catflap invite <device-name> [--ttl 15m] [--replace]")
	}
	c, err := control()
	if err != nil {
		return err
	}
	var res homelab.InviteResult
	if err := c.Do(ctx, "POST", "/invite", map[string]any{"name": pos[0], "ttl": *ttl, "replace": *replace}, &res); err != nil {
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
	fs.StringVar(&o.Request, "request", "", "no invite: ask HOMELAB to approve this device (tap to approve)")
	fs.StringVar(&o.Name, "name", "", "device name to ask for, with --request (default: from the hostname)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case o.Request != "" && len(pos) == 0:
	case o.Request == "" && len(pos) == 1 && o.Name == "":
		o.Invite = pos[0]
	default:
		return errors.New("usage: catflap enroll <invite> [--alias NAME] [--ssh-key FILE.pub]\n       catflap enroll --request HOMELAB [--name DEVICE] [--alias NAME]")
	}
	o.Out = os.Stdout
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
				why := err.Error()
				if notRunning(err) {
					why = startHint()
				}
				fmt.Printf("homelab: %q via %s - NOT RUNNING (%s)\n", cfg.Name, cfg.Relay, why)
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

// cmdHomelabMove moves the homelab to another relay (new hostname or port).
func cmdHomelabMove(ctx context.Context, args []string) error {
	fs := newFlags("homelab move")
	join := fs.String("join", "", "join code from the new relay (sudo catflap relay join-code there)")
	ttl := fs.Duration("ttl", 24*time.Hour, "how long the re-enroll invites stay valid")
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *join == "" {
		return errors.New("usage: catflap homelab move --join <code from the new relay> [--ttl 24h] [--yes]")
	}
	p, err := homelab.DefaultPaths()
	if err != nil {
		return err
	}
	info, ok := homelab.Inspect(p)
	if !ok {
		return errors.New("there is no homelab set up here (for this user)")
	}
	target, err := homelab.MoveTarget(p, *join)
	if err != nil {
		return err
	}
	fmt.Printf("This moves homelab %q from relay %s to relay %s:\n", info.Name, info.Relay, target)
	fmt.Println("  - registers it with the new relay (it keeps its keys and devices) and removes it from the old one")
	fmt.Println("  - restarts the homelab")
	if len(info.Devices) > 0 {
		fmt.Printf("  - creates a re-enroll invite for each device, valid %s: %s\n", humanDuration(*ttl), strings.Join(info.Devices, ", "))
		fmt.Println("Devices can't connect until they ran `catflap enroll` with their new invite.")
	}
	fmt.Println()
	if !*yes && !confirm("Move the homelab?") {
		return errors.New("cancelled, nothing was changed")
	}
	res, sent, err := setup.MoveHomelab(ctx, p, *join, *ttl, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("==> homelab %q now uses relay %s\n", info.Name, res.NewRelay)
	if res.OldRelayErr != nil {
		fmt.Printf("    could not remove it from the old relay (%v);\n    there: sudo catflap relay remove %s\n", res.OldRelayErr, info.Name)
	}
	if os.Getenv("CATFLAP_DIR") != "" || !setup.HomelabServiceInstalled() {
		fmt.Println("    start it again with: catflap homelab run")
	}
	if len(res.Invites) == 0 {
		return nil
	}
	fmt.Printf("\nRun on each device (one use, valid until %s; treat them like passwords):\n\n", res.Invites[0].Expires.Local().Format("2006-01-02 15:04"))
	for _, inv := range res.Invites {
		fmt.Printf("  %s:\n    catflap enroll %s\n", inv.Name, inv.Code)
	}
	if sent > 0 {
		fmt.Printf("\nThe invites were also posted to your Telegram bot (%d).\n", sent)
	}
	fmt.Println("\nEach device keeps its name and ssh alias; `catflap invites` shows the ones not used yet.")
	return nil
}

// humanDuration formats e.g. 24h as "24 hours".
func humanDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0 && d >= 48*time.Hour:
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d hours", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}

// cmdRequests shows or opens/closes the enrollment request window.
func cmdRequests(ctx context.Context, args []string) error {
	fs := newFlags("requests")
	dur := fs.Duration("for", homelab.DefaultRequestWindow, "how long the window stays open (on)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	c, err := control()
	if err != nil {
		return err
	}
	if len(pos) == 1 && (pos[0] == "on" || pos[0] == "off") {
		d := *dur
		if pos[0] == "off" {
			d = 0
		} else if d <= 0 || d > 7*24*time.Hour {
			return errors.New("--for must be between 1s and 168h")
		}
		var res homelab.RequestsInfo
		if err := c.Do(ctx, "POST", "/requests/window", map[string]any{"for": d}, &res); err != nil {
			return ctlErr(err)
		}
		if d == 0 {
			fmt.Println("enrollment requests are closed")
			return nil
		}
		var s homelab.StatusInfo
		c.Do(ctx, "GET", "/status", nil, &s)
		fmt.Printf("Enrollment requests are open until %s. On the new device run:\n\n  catflap enroll --request %s@%s\n\nThen approve it in Telegram or here with: catflap approve <code>\n", res.Until.Local().Format("15:04"), s.Name, s.Relay)
		return nil
	}
	if len(pos) != 0 {
		return errors.New("usage: catflap requests [on [--for 1h] | off]")
	}
	var info homelab.RequestsInfo
	if err := c.Do(ctx, "GET", "/requests", nil, &info); err != nil {
		return ctlErr(err)
	}
	if time.Now().Before(info.Until) {
		fmt.Printf("enrollment requests: open until %s\n", info.Until.Local().Format("2006-01-02 15:04"))
	} else {
		fmt.Println("enrollment requests: closed (open with: catflap requests on)")
	}
	if len(info.Pending) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nNAME\tHOSTNAME\tFROM\tCODE\tASKED")
	for _, p := range info.Pending {
		code := "(waiting)"
		if p.Code != "" {
			code = codes.FormatPairingCode(p.Code)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Hostname, p.Source, code, p.Received.Local().Format("15:04:05"))
	}
	return tw.Flush()
}

// cmdDecide approves or denies an enrollment request by code or name.
func cmdDecide(ctx context.Context, action string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: catflap %s <code or device name>", action)
	}
	c, err := control()
	if err != nil {
		return err
	}
	var res struct{ Name string }
	if err := c.Do(ctx, "POST", "/requests/"+action, map[string]string{"code": args[0]}, &res); err != nil {
		return ctlErr(err)
	}
	if action == "approve" {
		fmt.Printf("approved %q: the device is enrolled\n", res.Name)
	} else {
		fmt.Printf("denied the request from %q\n", res.Name)
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
		case hasRelay && hasHomelab && os.Geteuid() != 0:
			role = "homelab" // the relay needs root anyway
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
