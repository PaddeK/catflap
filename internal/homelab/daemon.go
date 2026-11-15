package homelab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"
	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/notify"
	"github.com/PaddeK/catflap/internal/osutil"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// Daemon is the running homelab.
type Daemon struct {
	cfg   Config
	paths Paths
	relay *api.Client
	logf  logger.Logf

	mu    sync.Mutex
	st    State
	allow tailcat.KeySet
	srv   *tailcat.Server
	addr  tailcat.Addr
	wake  chan struct{} // nudges the agent loop (e.g. pending sync)

	fwdMu sync.Mutex
	fwds  map[int]*forward // live forwards by tunnel port

	disp     *notify.Dispatcher // alerts to the channels in notify.json
	notifyMu sync.Mutex
	ncfg     notify.Config
	bot      *bot // Telegram invite bot, if a channel runs it
	botStop  context.CancelFunc

	reqMu    sync.Mutex
	reqs     map[string]*pendingRequest // enrollment requests (tap to approve)
	reqUntil time.Time                  // request window open until

	shutdown context.CancelFunc // ends Run (control route POST /shutdown)
	started  time.Time
}

// Run starts the homelab daemon and blocks until ctx is done.
func Run(ctx context.Context, p Paths) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg, err := LoadConfig(p)
	if err != nil {
		return err
	}
	var st State
	if err := fsutil.ReadJSON(p.State, &st); err != nil {
		return fmt.Errorf("reading %s: %w", p.State, err)
	}
	if st.ServerKey.IsZero() || st.AgentToken == "" {
		return errors.New("homelab state is incomplete; run `catflap setup homelab` again")
	}
	d := &Daemon{
		cfg:   cfg,
		paths: p,
		st:    st,
		relay: api.NewClient(cfg.Relay, st.AgentToken),
		logf:  log.Printf,
		wake:  make(chan struct{}, 1),

		shutdown: cancel,
		started:  time.Now(),
	}
	d.disp = notify.NewDispatcher(d.logf)
	d.reqs = map[string]*pendingRequest{}
	defer d.disp.Close()
	for _, dev := range st.Devices {
		var k key.NodePublic
		if err := k.UnmarshalText([]byte(dev.NodeKey)); err == nil {
			d.allow.Add(k)
		}
	}

	tlog := logger.Discard
	if os.Getenv("CATFLAP_DEBUG") != "" {
		tlog = logger.WithPrefix(log.Printf, "tailcat: ")
	}
	d.srv = &tailcat.Server{
		Key:          st.ServerKey,
		PresharedKey: st.PSK,
		Region:       Region(cfg.Relay, st.STUNPort),
		AllowClient:  d.allowClient,
		// Only the forwards have listeners (see forward.go). Other ports
		// are answered with a TCP reset (OnTCP is nil), so a client knows
		// at once that a port isn't forwarded, and silence always means
		// the tunnel itself is stuck. Non-enrolled devices can't send
		// anything at all (AllowClient).
		Logf: tlog,
	}
	if err := d.srv.StartContext(ctx); err != nil {
		return fmt.Errorf("starting tailcat server: %w", err)
	}
	defer d.srv.Close()
	d.addr = d.srv.TailcatAddr()
	d.startForwards()
	d.logf("homelab %q up: relay %s, %d device(s), forwarding %s", cfg.Name, cfg.Relay, len(st.Devices), forwardsString(cfg.Forwards))

	if err := d.serveControl(ctx); err != nil {
		return err
	}
	if err := d.loadNotify(ctx); err != nil {
		// Run without alerts rather than not at all, but say so loudly.
		d.logf("notify: %v; NOTIFICATIONS ARE OFF until this is fixed", err)
	}
	d.agentLoop(ctx)

	// Shutting down (e.g. `catflap restart`): close open connections first and
	// give the closing packets a moment to reach the clients.
	d.closeForwards()
	time.Sleep(500 * time.Millisecond)
	return nil
}

// Region returns the DERP region for a relay address.
func Region(relay string, stunPort int) *tailcfg.DERPRegion {
	host, port := relay, 443
	if h, p, err := net.SplitHostPort(relay); err == nil {
		host = h
		port, _ = strconv.Atoi(p)
	}
	n := &tailcfg.DERPNode{Name: "1", RegionID: 900, HostName: host}
	if port != 443 {
		n.DERPPort = port
	}
	switch stunPort {
	case 0:
		n.STUNPort = -1 // relay has STUN disabled
	case 3478:
	default:
		n.STUNPort = stunPort
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			n.IPv4, n.IPv6 = host, "none"
		} else {
			n.IPv6, n.IPv4 = host, "none"
		}
	}
	return &tailcfg.DERPRegion{RegionID: 900, RegionCode: "catflap", RegionName: "catflap", Nodes: []*tailcfg.DERPNode{n}}
}

func (d *Daemon) allowClient(k key.NodePublic) bool {
	ok := d.allow.Contains(k)
	if !ok && os.Getenv("CATFLAP_DEBUG") != "" {
		d.logf("tunnel: refused unknown key %s", k.ShortString())
	}
	return ok
}

func (d *Daemon) deviceName(remote net.Addr) string {
	k, ok := d.srv.PeerKey(remote)
	if !ok {
		return "unknown"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, dev := range d.st.Devices {
		if dev.NodeKey == k.String() {
			return dev.Name
		}
	}
	return k.ShortString()
}

func (d *Daemon) saveLocked() error {
	return fsutil.WriteJSON(d.paths.State, &d.st, 0o600)
}

func (d *Daemon) nudge() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// ---- agent loop: relay events ----

func (d *Daemon) agentLoop(ctx context.Context) {
	d.registerOpenInvites(ctx)
	d.announceWindow(ctx) // after a restart the window is closed
	d.mu.Lock()
	d.st.NeedsSync = true // reconcile with the relay at every start
	d.mu.Unlock()

	backoff := time.Second
	var down time.Time // first failure of the current outage
	alerted := false
	for ctx.Err() == nil {
		d.pruneInvites()
		d.pruneRequests()
		d.mu.Lock()
		needSync := d.st.NeedsSync
		d.mu.Unlock()
		if needSync {
			if err := d.sync(ctx); err != nil {
				d.logf("agent: sync with relay failed: %v", err)
			}
		}
		var ev api.EventsResponse
		err := d.relay.Do(ctx, "GET", "/v1/agent/events?wait=25", nil, &ev)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.logf("agent: relay: %v (retrying in %v)", err, backoff)
			if down.IsZero() {
				down = time.Now()
			}
			if !alerted && time.Since(down) >= relayDownAlert {
				alerted = true
				d.alert(notify.Message{Event: "relay.down", Title: "relay unreachable", Priority: notify.High,
					Body: fmt.Sprintf("The homelab has not reached the relay %s for %d minutes, so devices can't connect. Last error: %v", d.cfg.Relay, int(relayDownAlert.Minutes()), err)}, "")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			case <-d.wake:
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		if alerted {
			d.alert(notify.Message{Event: "relay.up", Title: "relay reachable again",
				Body: fmt.Sprintf("The relay %s is reachable again after %s.", d.cfg.Relay, time.Since(down).Round(time.Minute))}, "")
		}
		if !down.IsZero() {
			// Back after an outage: the relay may have lost recent changes (e.g.
			// restored from a backup), so send the device list and invites again.
			d.mu.Lock()
			d.st.NeedsSync = true
			d.mu.Unlock()
			d.registerOpenInvites(ctx)
			d.announceWindow(ctx)
		}
		down, alerted = time.Time{}, false
		backoff = time.Second
		for _, s := range ev.Submissions {
			d.handleSubmission(ctx, s)
		}
		for _, rq := range ev.Requests {
			d.handleRequest(ctx, rq)
		}
	}
}

func (d *Daemon) registerOpenInvites(ctx context.Context) {
	d.mu.Lock()
	invs := slices.Clone(d.st.Invites)
	d.mu.Unlock()
	for _, inv := range invs {
		if inv.Used != nil || time.Now().After(inv.Expires) {
			continue
		}
		err := d.relay.Do(ctx, "POST", "/v1/agent/invites", api.InviteRegistration{InviteID: inv.ID, Expires: inv.Expires}, nil)
		if err != nil {
			d.logf("agent: re-registering invite for %q: %v", inv.Name, err)
		}
	}
}

func (d *Daemon) sync(ctx context.Context) error {
	d.mu.Lock()
	req := api.SyncRequest{Clients: map[string]string{}}
	for _, dev := range d.st.Devices {
		req.Clients[dev.NodeKey] = dev.Name
	}
	d.mu.Unlock()
	if err := d.relay.Do(ctx, "POST", "/v1/agent/sync", req, nil); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st.NeedsSync = false
	return d.saveLocked()
}

func (d *Daemon) pruneInvites() {
	d.mu.Lock()
	var gone []Invite
	n := len(d.st.Invites)
	d.st.Invites = slices.DeleteFunc(d.st.Invites, func(inv Invite) bool {
		expired := inv.Used == nil && time.Now().After(inv.Expires)
		if expired && inv.TelegramMessage != 0 {
			gone = append(gone, inv)
		}
		return expired
	})
	if len(d.st.Invites) != n {
		d.saveLocked()
	}
	d.mu.Unlock()
	if b := d.currentBot(); b != nil {
		for _, inv := range gone {
			go b.inviteGone(inv.TelegramMessage, inv.Name)
		}
	}
}

func (d *Daemon) reject(ctx context.Context, s api.Submission, reason string, closeInvite bool) {
	d.logf("enroll: rejected submission from %s: %s", s.Source, reason)
	d.mu.Lock()
	name := "an unknown invite"
	for _, inv := range d.st.Invites {
		if inv.ID == s.InviteID {
			name = fmt.Sprintf("the invite for %q", inv.Name)
		}
	}
	d.mu.Unlock()
	d.alert(notify.Message{Event: "enroll.failed", Title: "failed enrollment", Priority: notify.High,
		Body: fmt.Sprintf("A device at %s tried to enroll with %s and was refused: %s.", s.Source, name, reason)}, "")
	err := d.relay.Do(ctx, "POST", "/v1/agent/result", api.ResultRequest{
		InviteID: s.InviteID, SubmissionID: s.SubmissionID, Reason: reason, CloseInvite: closeInvite,
	}, nil)
	if err != nil {
		d.logf("enroll: reporting rejection: %v", err)
	}
}

// handleSubmission verifies one enrollment and, if valid, adds the device.
func (d *Daemon) handleSubmission(ctx context.Context, s api.Submission) {
	d.mu.Lock()
	i := slices.IndexFunc(d.st.Invites, func(inv Invite) bool { return inv.ID == s.InviteID })
	if i < 0 {
		d.mu.Unlock()
		d.reject(ctx, s, "unknown invite", true)
		return
	}
	inv := d.st.Invites[i]

	// Already redeemed: re-send the stored result, or refuse a second device.
	if inv.Used != nil {
		d.mu.Unlock()
		if inv.Used.SubmissionID == s.SubmissionID {
			d.sendApproval(ctx, inv, s)
		} else {
			d.reject(ctx, s, "invite already used", false)
		}
		return
	}
	if time.Now().After(inv.Expires) {
		d.mu.Unlock()
		d.reject(ctx, s, "invite expired", true)
		return
	}
	keys, err := codes.DeriveKeys(inv.Secret)
	if err != nil {
		d.mu.Unlock()
		d.logf("enroll: %v", err)
		return
	}
	r := s.Request
	if !codes.VerifyEnrollMAC(keys, codes.EnrollFields{InviteID: s.InviteID, NodeKey: r.NodeKey, SSHKey: r.SSHKey, Hostname: r.Hostname, Nonce: r.Nonce}, r.MAC) {
		d.mu.Unlock()
		d.reject(ctx, s, "invalid invite code (verification failed)", false)
		return
	}
	var nk key.NodePublic
	if err := nk.UnmarshalText([]byte(r.NodeKey)); err != nil || nk.IsZero() {
		d.mu.Unlock()
		d.reject(ctx, s, "invalid node key", false)
		return
	}
	sshKey := ""
	if r.SSHKey != "" {
		if sshKey, err = normalizeSSHKey(r.SSHKey); err != nil {
			d.mu.Unlock()
			d.reject(ctx, s, err.Error(), false)
			return
		}
	}
	replaced := -1 // index of the device a re-enroll invite replaces
	for i, dev := range d.st.Devices {
		switch {
		case dev.NodeKey == nk.String():
			d.mu.Unlock()
			d.reject(ctx, s, fmt.Sprintf("this device is already enrolled as %q", dev.Name), false)
			return
		case dev.Name == inv.Name && inv.Replace:
			replaced = i
		case dev.Name == inv.Name:
			d.mu.Unlock()
			d.reject(ctx, s, fmt.Sprintf("a device named %q exists already", inv.Name), true)
			return
		}
	}

	// Gate 3 first: if writing authorized_keys fails, nothing else changed.
	// A re-enrollment drops the old device's line first.
	if path, ok := d.cfg.authorizedKeysPath(); ok && replaced >= 0 {
		if _, err := removeAuthorizedKey(path, inv.Name); err != nil {
			d.mu.Unlock()
			d.logf("enroll: authorized_keys: %v", err)
			d.reject(ctx, s, "homelab could not update the SSH keys", false)
			return
		}
	}
	if path, ok := d.cfg.authorizedKeysPath(); ok && sshKey != "" {
		if err := addAuthorizedKey(path, sshKey, inv.Name, !d.cfg.AllowSSHKeysFromAnywhere); err != nil {
			d.mu.Unlock()
			d.logf("enroll: authorized_keys: %v", err)
			d.reject(ctx, s, "homelab could not store the SSH key", false)
			return
		}
	}
	// Gate 2: live allowlist. A replaced device loses its old key.
	var oldKey key.NodePublic
	if replaced >= 0 {
		oldKey.UnmarshalText([]byte(d.st.Devices[replaced].NodeKey))
		d.allow.Remove(oldKey)
		d.st.Devices = slices.Delete(d.st.Devices, replaced, replaced+1)
		d.st.NeedsSync = true // the relay drops the old key too
	}
	d.st.Devices = append(d.st.Devices, Device{Name: inv.Name, NodeKey: nk.String(), SSHKey: sshKey, Hostname: sanitize(r.Hostname), Added: time.Now()})
	d.allow.Add(nk)

	reply, _ := json.Marshal(api.SealedReply{Name: inv.Name, Homelab: d.cfg.Name, Addr: string(d.addr), SSHUser: d.cfg.SSHUser})
	sealed, err := codes.Seal(keys, codes.SealAAD(s.InviteID, s.SubmissionID, r.NodeKey), reply)
	if err != nil {
		d.mu.Unlock()
		d.logf("enroll: seal: %v", err)
		return
	}
	d.st.Invites[i].Used = &UsedInvite{SubmissionID: s.SubmissionID, NodeKey: nk.String(), Sealed: sealed}
	inv = d.st.Invites[i]
	if err := d.saveLocked(); err != nil {
		d.logf("enroll: saving state: %v", err)
	}
	d.mu.Unlock()
	if !oldKey.IsZero() {
		d.srv.DisconnectClient(oldKey)
		d.nudge()
	}
	d.logf("enroll: added device %q (%s, from %s)", inv.Name, nk.ShortString(), s.Source)
	host := ""
	if h := sanitize(r.Hostname); h != "" {
		host = " (hostname " + h + ")"
	}
	title := "new device: " + inv.Name
	if replaced >= 0 {
		title = "device re-enrolled: " + inv.Name
	}
	d.alert(notify.Message{Event: "device.enrolled", Title: title, Priority: notify.High,
		Body: fmt.Sprintf("%s%s enrolled from %s. If that wasn't you, remove it: %s", inv.Name, host, s.Source, d.removeHint(inv.Name))}, "")
	if inv.TelegramMessage != 0 {
		if b := d.currentBot(); b != nil {
			go b.inviteUsed(inv.TelegramMessage, inv.Name, sanitize(r.Hostname))
		}
	}
	d.sendApproval(ctx, inv, s)
}

// sendApproval tells the relay (gate 1) and hands it the sealed reply.
func (d *Daemon) sendApproval(ctx context.Context, inv Invite, s api.Submission) {
	err := d.relay.Do(ctx, "POST", "/v1/agent/result", api.ResultRequest{
		InviteID: inv.ID, SubmissionID: s.SubmissionID, Approve: true,
		NodeKey: inv.Used.NodeKey, Name: inv.Name, Sealed: inv.Used.Sealed,
	}, nil)
	if err != nil {
		d.logf("enroll: relay did not take the approval yet (will retry): %v", err)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st.Invites = slices.DeleteFunc(d.st.Invites, func(x Invite) bool { return x.ID == inv.ID })
	d.saveLocked()
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func forwardsString(f map[string]string) string {
	var parts []string
	for p, t := range f {
		parts = append(parts, p+"->"+t)
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

// ---- control socket ----

// InviteResult is returned by the invite command.
type InviteResult struct {
	Code    string    `json:"code"`
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`
}

// InviteInfo is a listed open invite.
type InviteInfo struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`
}

// StatusInfo is returned by the status command.
type StatusInfo struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"` // of the running daemon
	Relay     string    `json:"relay"`
	Addr      string    `json:"addr"`
	Devices   int       `json:"devices"`
	Invites   int       `json:"invites"`
	Forwards  []string  `json:"forwards"`
	NeedsSync bool      `json:"needs_sync"`
	Started   time.Time `json:"started"`
}

func (d *Daemon) serveControl(ctx context.Context) error {
	os.Remove(d.paths.Socket)
	if err := os.MkdirAll(dirOf(d.paths.Socket), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("unix", d.paths.Socket)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	os.Chmod(d.paths.Socket, 0o600)
	// Elevated on Windows (admin accounts), the socket would otherwise be
	// out of reach of catflap commands that aren't.
	if err := osutil.AllowMediumIntegrity(d.paths.Socket); err != nil {
		d.logf("control socket: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /invite", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name    string        `json:"name"`
			TTL     time.Duration `json:"ttl"`
			Replace bool          `json:"replace"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			ctlErr(w, http.StatusBadRequest, "bad request")
			return
		}
		res, err := d.createInvite(r.Context(), req.Name, req.TTL, "", req.Replace)
		if err != nil {
			ctlErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctlJSON(w, res)
	})
	mux.HandleFunc("GET /invites", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		out := []InviteInfo{}
		for _, inv := range d.st.Invites {
			if inv.Used == nil {
				out = append(out, InviteInfo{inv.ID, inv.Name, inv.Expires})
			}
		}
		d.mu.Unlock()
		ctlJSON(w, out)
	})
	mux.HandleFunc("DELETE /invites/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.revokeInvite(r.Context(), r.PathValue("id")); err != nil {
			ctlErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctlJSON(w, struct{}{})
	})
	// Post open re-enroll invites (from `homelab move`) to the Telegram bot.
	mux.HandleFunc("POST /invites/telegram", func(w http.ResponseWriter, r *http.Request) {
		sent := 0
		if b := d.currentBot(); b != nil {
			d.mu.Lock()
			var todo []Invite
			for _, inv := range d.st.Invites {
				if inv.Replace && inv.Used == nil && inv.TelegramMessage == 0 && time.Now().Before(inv.Expires) {
					todo = append(todo, inv)
				}
			}
			d.mu.Unlock()
			for _, inv := range todo {
				code := codes.Code{Kind: codes.KindInvite, ID: inv.ID, Secret: inv.Secret, Relay: d.cfg.Relay}.String()
				if id := b.postReenroll(r.Context(), inv, code); id != 0 {
					d.setInviteMessage(inv.Name, id)
					sent++
				}
			}
		}
		ctlJSON(w, map[string]int{"sent": sent})
	})
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		out := slices.Clone(d.st.Devices)
		d.mu.Unlock()
		if out == nil {
			out = []Device{}
		}
		ctlJSON(w, out)
	})
	mux.HandleFunc("DELETE /devices/{name}", func(w http.ResponseWriter, r *http.Request) {
		res, err := d.removeDevice(r.Context(), r.PathValue("name"), "")
		if err != nil {
			ctlErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctlJSON(w, res)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		s := StatusInfo{Name: d.cfg.Name, Version: api.Version, Relay: d.cfg.Relay, Addr: string(d.addr), Devices: len(d.st.Devices), NeedsSync: d.st.NeedsSync, Started: d.started}
		for _, inv := range d.st.Invites {
			if inv.Used == nil {
				s.Invites++
			}
		}
		d.mu.Unlock()
		s.Forwards = []string{}
		for _, f := range d.forwardList() {
			s.Forwards = append(s.Forwards, fmt.Sprintf("%d->%s", f.Port, f.Target))
		}
		ctlJSON(w, s)
	})
	mux.HandleFunc("GET /forwards", func(w http.ResponseWriter, r *http.Request) {
		ctlJSON(w, d.forwardList())
	})
	mux.HandleFunc("PUT /forwards/{port}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target string `json:"target"`
		}
		port, err := parsePort(r.PathValue("port"))
		if err != nil || json.NewDecoder(r.Body).Decode(&req) != nil {
			ctlErr(w, http.StatusBadRequest, "bad request")
			return
		}
		changed, err := d.setForward(port, req.Target)
		if err != nil {
			ctlErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctlJSON(w, map[string]bool{"changed": changed})
	})
	mux.HandleFunc("DELETE /forwards/{port}", func(w http.ResponseWriter, r *http.Request) {
		port, err := parsePort(r.PathValue("port"))
		if err != nil {
			ctlErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := d.removeForward(port); err != nil {
			ctlErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctlJSON(w, struct{}{})
	})
	// Stop the daemon: the service managers without a stop command of
	// their own that works for the user (Windows) use this.
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) {
		d.logf("shutting down (asked on the control socket)")
		ctlJSON(w, struct{}{})
		d.shutdown()
	})
	// Remove the SSH keys catflap added (uninstall, before stopping): the
	// daemon may have rights the command doesn't (Windows admin accounts).
	mux.HandleFunc("POST /uninstall/keys", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		devs := slices.Clone(d.st.Devices)
		d.mu.Unlock()
		n, err := removeDeviceKeys(d.cfg, devs)
		if err != nil {
			ctlErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		ctlJSON(w, map[string]int{"removed": n})
	})
	d.notifyRoutes(ctx, mux)
	d.requestRoutes(mux)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
		os.Remove(d.paths.Socket)
	}()
	go srv.Serve(ln)
	return nil
}

// createInvite makes a one-time invite. origin is the channel the request
// came from ("" = command line).
func (d *Daemon) createInvite(ctx context.Context, name string, ttl time.Duration, origin string, replace bool) (InviteResult, error) {
	if !ValidName(name) {
		return InviteResult{}, fmt.Errorf("invalid name %q: use lowercase letters, digits, '.', '-' or '_'", name)
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if ttl > 90*24*time.Hour {
		return InviteResult{}, errors.New("ttl is limited to 90 days")
	}
	d.mu.Lock()
	exists := slices.ContainsFunc(d.st.Devices, func(dev Device) bool { return dev.Name == name })
	switch {
	case exists && !replace:
		d.mu.Unlock()
		return InviteResult{}, fmt.Errorf("a device named %q exists already (remove it first, or create a re-enroll invite with --replace)", name)
	case !exists && replace:
		d.mu.Unlock()
		return InviteResult{}, fmt.Errorf("there is no device named %q to replace", name)
	}
	for _, inv := range d.st.Invites {
		if inv.Name == name && time.Now().Before(inv.Expires) {
			d.mu.Unlock()
			return InviteResult{}, fmt.Errorf("there is already an open invite for %q (see `catflap invites`)", name)
		}
	}
	d.mu.Unlock()

	c, err := codes.New(codes.KindInvite, d.cfg.Relay)
	if err != nil {
		return InviteResult{}, err
	}
	exp := time.Now().Add(ttl).Truncate(time.Second)
	if err := d.relay.Do(ctx, "POST", "/v1/agent/invites", api.InviteRegistration{InviteID: c.ID, Expires: exp}, nil); err != nil {
		return InviteResult{}, fmt.Errorf("registering the invite with the relay: %w", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st.Invites = append(d.st.Invites, Invite{ID: c.ID, Name: name, Secret: c.Secret, Created: time.Now(), Expires: exp, Replace: replace})
	if err := d.saveLocked(); err != nil {
		return InviteResult{}, err
	}
	d.logf("invite: created for %q, valid until %s", name, exp.Format(time.RFC3339))
	kind := "One-time invite"
	if replace {
		kind = "Re-enroll invite (replaces the existing device)"
	}
	d.alert(notify.Message{Event: "invite.created", Title: "invite created for " + name,
		Body: fmt.Sprintf("%s for %q created %s, valid until %s.", kind, name, via(origin), when(exp))}, origin)
	return InviteResult{Code: c.String(), Name: name, Expires: exp}, nil
}

func (d *Daemon) revokeInvite(ctx context.Context, idOrName string) error {
	d.mu.Lock()
	i := slices.IndexFunc(d.st.Invites, func(inv Invite) bool { return inv.ID == idOrName || inv.Name == idOrName })
	if i < 0 {
		d.mu.Unlock()
		return fmt.Errorf("no open invite %q", idOrName)
	}
	inv := d.st.Invites[i]
	id := inv.ID
	d.st.Invites = slices.Delete(d.st.Invites, i, i+1)
	err := d.saveLocked()
	d.mu.Unlock()
	if err != nil {
		return err
	}
	if b := d.currentBot(); b != nil && inv.TelegramMessage != 0 {
		go b.inviteGone(inv.TelegramMessage, inv.Name)
	}
	// Even if this fails, the homelab no longer accepts the invite.
	if err := d.relay.Do(ctx, "DELETE", "/v1/agent/invites/"+id, nil, nil); err != nil {
		d.logf("invite: revoking at relay: %v", err)
	}
	return nil
}

// RemoveResult is returned by the remove command.
type RemoveResult struct {
	// RelayUpdated is false if the relay was unreachable; the agent keeps
	// retrying in the background.
	RelayUpdated bool `json:"relay_updated"`
	// SSHKeyRemoved is false if catflap had not added an SSH key for the
	// device (none sent, or the key was already authorized by hand).
	SSHKeyRemoved bool `json:"ssh_key_removed"`
	// SSHKeyKept is set when the device's SSH key is still authorized
	// because it was added by hand.
	SSHKeyKept bool `json:"ssh_key_kept"`
}

// removeDevice closes all three gates for a device. origin is the channel
// the request came from ("" = command line).
func (d *Daemon) removeDevice(ctx context.Context, name string, origin string) (RemoveResult, error) {
	var res RemoveResult
	d.mu.Lock()
	i := slices.IndexFunc(d.st.Devices, func(dev Device) bool { return dev.Name == name })
	if i < 0 {
		d.mu.Unlock()
		return res, fmt.Errorf("no device named %q", name)
	}
	dev := d.st.Devices[i]
	d.st.Devices = slices.Delete(d.st.Devices, i, i+1)
	var k key.NodePublic
	if k.UnmarshalText([]byte(dev.NodeKey)) == nil {
		d.allow.Remove(k)
	}
	d.st.NeedsSync = true
	err := d.saveLocked()
	d.mu.Unlock()
	if err != nil {
		return res, err
	}
	d.srv.DisconnectClient(k)
	if path, ok := d.cfg.authorizedKeysPath(); ok {
		removed, err := removeAuthorizedKey(path, name)
		if err != nil {
			d.logf("remove: authorized_keys: %v", err)
		}
		res.SSHKeyRemoved = removed
		res.SSHKeyKept = !removed && dev.SSHKey != "" && hasAuthorizedKey(path, dev.SSHKey)
	}
	d.logf("remove: device %q removed", name)
	d.alert(notify.Message{Event: "device.removed", Title: "device removed: " + name,
		Body: fmt.Sprintf("%s was removed %s and disconnected.", name, via(origin))}, origin)
	if err := d.sync(ctx); err != nil {
		d.logf("remove: relay sync failed, will retry: %v", err)
		d.nudge()
		return res, nil
	}
	res.RelayUpdated = true
	return res, nil
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[:i]
	}
	return "."
}

func ctlJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func ctlErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(api.ErrorResponse{Error: msg})
}

// Control returns a client for the running daemon's control socket.
func Control(p Paths) *api.Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dl net.Dialer
		return dl.DialContext(ctx, "unix", p.Socket)
	}}
	c := api.NewClient("homelab", "")
	c.Base = "http://homelab"
	c.HTTP = &http.Client{Transport: tr, Timeout: 60 * time.Second}
	return c
}

// relayDownAlert is how long the relay must be unreachable before an alert.
const relayDownAlert = 10 * time.Minute

// setInviteMessage remembers the bot message that shows an open invite.
func (d *Daemon) setInviteMessage(name string, msg int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.st.Invites {
		if d.st.Invites[i].Name == name && d.st.Invites[i].Used == nil {
			d.st.Invites[i].TelegramMessage = msg
			d.saveLocked()
			return
		}
	}
}
