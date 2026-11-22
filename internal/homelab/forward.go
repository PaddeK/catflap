package homelab

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PaddeK/catflap/internal/fsutil"

	"github.com/tailscale/tailcat"
)

// Forwards are the only ports reachable through the tunnel. Each one is a
// tailcat listener: tailcat's packet filter admits exactly the ports with
// an open listener, so adding or removing a forward takes effect at once,
// without restarting the tunnel. A forward can be limited to some devices
// (`forward add PORT --only DEVICE`): the filter can't tell devices apart,
// so the forward itself closes connections from the others at once.

// ForwardInfo is one forward as shown by `catflap forward list`.
type ForwardInfo struct {
	Port   int    `json:"port"`
	Target string `json:"target"`
	// Only lists the devices the port is open to; empty means all.
	Only []string `json:"only,omitempty"`
}

// String formats the forward as e.g. "1880->127.0.0.1:1880 (only phone)".
func (f ForwardInfo) String() string {
	s := fmt.Sprintf("%d->%s", f.Port, f.Target)
	if len(f.Only) > 0 {
		s += " (only " + strings.Join(f.Only, ", ") + ")"
	}
	return s
}

// normalizeOnly sorts and dedupes a device list; empty becomes nil (all).
func normalizeOnly(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := slices.Clone(names)
	slices.Sort(out)
	return slices.Compact(out)
}

// ParseForward parses "PORT" (meaning 127.0.0.1:PORT on the homelab) or
// "PORT=HOST:PORT".
func ParseForward(s string) (port int, target string, err error) {
	ps, target, hasTarget := strings.Cut(s, "=")
	port, err = parsePort(ps)
	if err != nil {
		return 0, "", fmt.Errorf("bad forward %q: want PORT or PORT=HOST:PORT, e.g. 1880 or 8080=192.168.1.50:80", s)
	}
	if !hasTarget {
		target = net.JoinHostPort("127.0.0.1", ps)
	}
	if err := validateTarget(target); err != nil {
		return 0, "", err
	}
	return port, target, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("bad port %q", s)
	}
	return n, nil
}

func validateTarget(t string) error {
	host, port, err := net.SplitHostPort(t)
	if err != nil || host == "" {
		return fmt.Errorf("bad forward target %q: want HOST:PORT, e.g. 127.0.0.1:1880", t)
	}
	if _, err := parsePort(port); err != nil {
		return fmt.Errorf("bad forward target %q: %v", t, err)
	}
	return nil
}

// forward is one live tunnel port.
type forward struct {
	port   int
	target string
	ln     net.Listener

	mu     sync.Mutex
	closed bool
	only   []string            // devices allowed (sorted); nil: all
	conns  map[net.Conn]string // open connections -> device name
}

func (d *Daemon) startForward(port int, target string, only []string) (*forward, error) {
	ln, err := d.srv.Listen(context.Background(), "tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	f := &forward{port: port, target: target, ln: ln, only: only, conns: map[net.Conn]string{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go f.handle(d, c)
		}
	}()
	return f, nil
}

func (f *forward) allowsLocked(who string) bool {
	return f.only == nil || slices.Contains(f.only, who)
}

func (f *forward) allows(who string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allowsLocked(who)
}

// track registers connections of device who. It reports false if the
// forward is closed or not open to who (anymore).
func (f *forward) track(who string, cs ...net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || !f.allowsLocked(who) {
		return false
	}
	for _, c := range cs {
		f.conns[c] = who
	}
	return true
}

// setOnly changes the devices the forward is open to and cuts the open
// connections of devices that lost access.
func (f *forward) setOnly(only []string) {
	f.mu.Lock()
	f.only = only
	var cut []net.Conn
	for c, who := range f.conns {
		if !f.allowsLocked(who) {
			cut = append(cut, c)
			delete(f.conns, c)
		}
	}
	f.mu.Unlock()
	for _, c := range cut {
		c.Close()
	}
}

func (f *forward) info() ForwardInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ForwardInfo{Port: f.port, Target: f.target, Only: slices.Clone(f.only)}
}

func (f *forward) untrack(cs ...net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range cs {
		delete(f.conns, c)
	}
}

func (f *forward) handle(d *Daemon, c net.Conn) {
	who := d.deviceName(c.RemoteAddr())
	if !f.allows(who) {
		d.logf("tunnel: %s -> port %d refused: the port is only open to %s", who, f.port, strings.Join(f.info().Only, ", "))
		c.Close()
		return
	}
	if !f.track(who, c) {
		c.Close()
		return
	}
	t, err := net.DialTimeout("tcp", f.target, 10*time.Second)
	if err != nil {
		d.logf("tunnel: %s -> port %d: %v", who, f.port, err)
		f.untrack(c)
		c.Close()
		return
	}
	if !f.track(who, t) {
		f.untrack(c)
		c.Close()
		t.Close()
		return
	}
	d.logf("tunnel: %s -> port %d (%s)", who, f.port, f.target)
	tailcat.ProxyConns(c, t)
	f.untrack(c, t)
}

// close stops accepting and cuts the connections that are still open.
func (f *forward) close() {
	f.ln.Close()
	f.mu.Lock()
	f.closed = true
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

// startForwards opens the configured forwards at daemon start.
func (d *Daemon) startForwards() {
	d.fwds = map[int]*forward{}
	for ps, target := range d.cfg.Forwards {
		port, _ := strconv.Atoi(ps) // validated by LoadConfig
		f, err := d.startForward(port, target, normalizeOnly(d.cfg.ForwardOnly[ps]))
		if err != nil {
			d.logf("forward %d: %v", port, err)
			continue
		}
		d.fwds[port] = f
	}
}

// setForward adds a forward or changes its target or devices (only: the
// devices it is open to; empty: all).
func (d *Daemon) setForward(port int, target string, only []string) (changed bool, err error) {
	if _, err := parsePort(strconv.Itoa(port)); err != nil {
		return false, err
	}
	if err := validateTarget(target); err != nil {
		return false, err
	}
	only = normalizeOnly(only)
	for _, n := range only {
		if err := DeviceNameError(n); err != nil {
			return false, err
		}
	}
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	old := d.fwds[port]
	switch {
	case old != nil && old.target == target && slices.Equal(old.info().Only, only):
		return false, nil
	case old != nil && old.target == target:
		old.setOnly(only) // open connections stay, unless their device lost access
	default:
		if old != nil {
			old.close() // the port must be free before it can be listened on again
			delete(d.fwds, port)
		}
		f, err := d.startForward(port, target, only)
		if err != nil {
			return false, err
		}
		d.fwds[port] = f
	}
	if err := d.saveForwardsLocked(); err != nil {
		return true, fmt.Errorf("forward is active but saving %s failed: %v", d.paths.Config, err)
	}
	d.logf("forward: %s", d.fwds[port].info())
	return true, nil
}

// removeForward closes a forward, including its open connections.
func (d *Daemon) removeForward(port int) error {
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	f := d.fwds[port]
	if f == nil {
		return fmt.Errorf("port %d is not forwarded", port)
	}
	f.close()
	delete(d.fwds, port)
	if err := d.saveForwardsLocked(); err != nil {
		return fmt.Errorf("forward is closed but saving %s failed: %v", d.paths.Config, err)
	}
	d.logf("forward: port %d removed", port)
	return nil
}

func (d *Daemon) forwardList() []ForwardInfo {
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	out := []ForwardInfo{}
	for _, f := range d.fwds {
		out = append(out, f.info())
	}
	slices.SortFunc(out, func(a, b ForwardInfo) int { return a.Port - b.Port })
	return out
}

// saveForwardsLocked writes the current forwards to homelab.json. It only
// replaces the "forwards" and "forward_only" keys, so other hand-made edits
// survive.
func (d *Daemon) saveForwardsLocked() error {
	m := map[string]string{}
	only := map[string][]string{}
	for p, f := range d.fwds {
		i := f.info()
		m[strconv.Itoa(p)] = i.Target
		if len(i.Only) > 0 {
			only[strconv.Itoa(p)] = i.Only
		}
	}
	raw := map[string]json.RawMessage{}
	if err := fsutil.ReadJSON(d.paths.Config, &raw); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	raw["forwards"] = b
	delete(raw, "forward_only")
	if len(only) > 0 {
		if raw["forward_only"], err = json.Marshal(only); err != nil {
			return err
		}
	} else {
		only = nil
	}
	if err := fsutil.WriteJSON(d.paths.Config, raw, 0o644); err != nil {
		return err
	}
	d.cfg.Forwards, d.cfg.ForwardOnly = m, only
	return nil
}

// closeForwards ends every listener and open connection, so clients see
// their sessions close at once (ssh exits) when the homelab shuts down,
// instead of hanging until a keepalive timeout.
func (d *Daemon) closeForwards() {
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	for port, f := range d.fwds {
		f.close()
		delete(d.fwds, port)
	}
}
