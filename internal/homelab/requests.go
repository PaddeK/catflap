package homelab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"
	"github.com/PaddeK/catflap/internal/notify"

	"tailscale.com/types/key"
)

// Enrollment requests (tap to approve): a new device asks by name, the user
// compares a 6-digit code and approves in Telegram or with `catflap
// approve`. Requests are only possible while the request window is open
// (`catflap requests on`, `/requests on`), 1 hour by default. Approval never
// goes through the relay or a notification service: only the homelab
// decides. See codes/pairing.go for the protocol.

const (
	DefaultRequestWindow = time.Hour
	requestPendingTTL    = 10 * time.Minute
)

type pendingRequest struct {
	ID       string
	Req      api.RequestCreate
	Source   string
	Received time.Time
	Nonce    []byte
	Code     string // pairing code, once the device revealed its secret

	nk       key.NodePublic
	box      *[32]byte
	sshKey   string
	hostname string
	tgMsg    int64 // the bot's message with the buttons
}

// RequestInfo is a pending request as shown by `catflap requests`.
type RequestInfo struct {
	Name     string    `json:"name"`
	Hostname string    `json:"hostname"`
	Source   string    `json:"source"`
	Code     string    `json:"code"` // empty until the device revealed its secret
	Received time.Time `json:"received"`
}

// RequestsInfo is returned by GET /requests.
type RequestsInfo struct {
	Until   time.Time     `json:"until"` // window open until (zero or past: closed)
	Pending []RequestInfo `json:"pending"`
}

func (d *Daemon) windowOpen() bool {
	d.reqMu.Lock()
	defer d.reqMu.Unlock()
	return time.Now().Before(d.reqUntil)
}

// setRequestWindow opens the window for dur (0 closes it) and tells the relay.
func (d *Daemon) setRequestWindow(ctx context.Context, dur time.Duration, origin string) (time.Time, error) {
	var until time.Time
	if dur > 0 {
		until = time.Now().Add(dur).Truncate(time.Second)
	}
	if err := d.relay.Do(ctx, "POST", "/v1/agent/requests", api.RequestWindow{Until: until}, nil); err != nil {
		return until, fmt.Errorf("telling the relay: %w", err)
	}
	d.reqMu.Lock()
	d.reqUntil = until
	d.reqMu.Unlock()
	if dur > 0 {
		d.logf("requests: window open until %s", until.Format(time.RFC3339))
		d.alert(notify.Message{Event: "requests.open", Title: "enrollment requests open",
			Body: fmt.Sprintf("New devices can ask to join until %s (opened %s). Close it early with: catflap requests off", when(until), via(origin))}, origin)
	} else {
		d.logf("requests: window closed")
	}
	return until, nil
}

// announceWindow repeats the window to the relay (start, after outages).
func (d *Daemon) announceWindow(ctx context.Context) {
	d.reqMu.Lock()
	until := d.reqUntil
	d.reqMu.Unlock()
	if err := d.relay.Do(ctx, "POST", "/v1/agent/requests", api.RequestWindow{Until: until}, nil); err != nil {
		d.logf("requests: telling the relay about the window: %v", err)
	}
}

func (d *Daemon) requestAction(ctx context.Context, a api.RequestAction) error {
	return d.relay.Do(ctx, "POST", "/v1/agent/request", a, nil)
}

func (d *Daemon) denyRequestID(ctx context.Context, id, reason string) {
	if err := d.requestAction(ctx, api.RequestAction{ID: id, Action: "deny", Reason: reason}); err != nil {
		d.logf("requests: denying: %v", err)
	}
}

// nameTaken reports whether a device, open invite or other request uses name.
func (d *Daemon) nameTaken(name, exceptID string) bool {
	d.mu.Lock()
	taken := slices.ContainsFunc(d.st.Devices, func(dev Device) bool { return dev.Name == name }) ||
		slices.ContainsFunc(d.st.Invites, func(inv Invite) bool { return inv.Name == name && inv.Used == nil })
	d.mu.Unlock()
	if taken {
		return true
	}
	d.reqMu.Lock()
	defer d.reqMu.Unlock()
	for id, p := range d.reqs {
		if id != exceptID && p.Req.Name == name {
			return true
		}
	}
	return false
}

// handleRequest acts on a request event from the relay.
func (d *Daemon) handleRequest(ctx context.Context, ev api.RequestEvent) {
	switch ev.Stage {
	case "new":
		d.requestNew(ctx, ev)
	case "revealed":
		d.requestRevealed(ctx, ev)
	}
}

func (d *Daemon) requestNew(ctx context.Context, ev api.RequestEvent) {
	d.reqMu.Lock()
	if p, ok := d.reqs[ev.ID]; ok { // delivered again: repeat our nonce
		nonce := p.Nonce
		d.reqMu.Unlock()
		d.requestAction(ctx, api.RequestAction{ID: ev.ID, Action: "nonce", Nonce: codes.EncodeBytes(nonce)})
		return
	}
	d.reqMu.Unlock()
	r := ev.Request
	deny := func(reason string) {
		d.logf("requests: refused %q from %s: %s", r.Name, ev.Source, reason)
		d.denyRequestID(ctx, ev.ID, reason)
	}
	if !d.windowOpen() {
		deny("the homelab doesn't accept requests right now")
		return
	}
	p := &pendingRequest{ID: ev.ID, Req: r, Source: ev.Source, Received: time.Now(), Nonce: codes.RandomReveal(), hostname: sanitize(r.Hostname)}
	var err error
	switch {
	case r.Homelab != d.cfg.Name:
		deny("wrong homelab")
		return
	case DeviceNameError(r.Name) != nil:
		deny(DeviceNameError(r.Name).Error() + "; ask again with another --name")
		return
	case d.nameTaken(r.Name, ev.ID):
		deny(fmt.Sprintf("the name %q is taken on this homelab; ask again with another --name", r.Name))
		return
	case p.nk.UnmarshalText([]byte(r.NodeKey)) != nil || p.nk.IsZero():
		deny("invalid node key")
		return
	}
	if p.box, err = codes.DecodeBoxKey(r.BoxKey); err != nil {
		deny("invalid box key")
		return
	}
	if r.SSHKey != "" {
		if p.sshKey, err = normalizeSSHKey(r.SSHKey); err != nil {
			deny(err.Error())
			return
		}
	}
	d.reqMu.Lock()
	d.reqs[ev.ID] = p
	d.reqMu.Unlock()
	if err := d.requestAction(ctx, api.RequestAction{ID: ev.ID, Action: "nonce", Nonce: codes.EncodeBytes(p.Nonce)}); err != nil {
		d.logf("requests: sending nonce: %v", err)
	}
}

func (d *Daemon) requestRevealed(ctx context.Context, ev api.RequestEvent) {
	d.reqMu.Lock()
	p, ok := d.reqs[ev.ID]
	d.reqMu.Unlock()
	if !ok {
		d.denyRequestID(ctx, ev.ID, "the homelab doesn't know this request anymore (restarted?); ask again")
		return
	}
	reveal, err := codes.DecodeBytes(ev.Reveal)
	if err != nil || !codes.CheckCommit(p.Req.Commit, reveal) {
		d.forgetRequest(ev.ID)
		d.denyRequestID(ctx, ev.ID, "the reveal doesn't match the commitment")
		return
	}
	code := codes.PairingCode(codes.PairingFields{
		HomelabKey: d.st.ServerKey.Public().String(), Homelab: d.cfg.Name, Name: p.Req.Name,
		NodeKey: p.Req.NodeKey, BoxKey: p.Req.BoxKey, SSHKey: p.Req.SSHKey, Hostname: p.Req.Hostname,
	}, reveal, p.Nonce)
	d.reqMu.Lock()
	first := p.Code == ""
	p.Code = code
	d.reqMu.Unlock()
	if err := d.requestAction(ctx, api.RequestAction{ID: ev.ID, Action: "shown"}); err != nil {
		d.logf("requests: %v", err)
	}
	if !first {
		return
	}
	host := ""
	if p.hostname != "" {
		host = " (hostname " + p.hostname + ")"
	}
	d.logf("requests: %q%s from %s asks to join, code %s", p.Req.Name, host, p.Source, code)
	skip := ""
	if b := d.currentBot(); b != nil {
		skip = b.ch.Name
		if id := b.postRequest(ctx, p); id != 0 {
			d.reqMu.Lock()
			p.tgMsg = id
			d.reqMu.Unlock()
		}
	}
	d.alert(notify.Message{Event: "request.new", Title: "enrollment request: " + p.Req.Name, Priority: notify.High,
		Body: fmt.Sprintf("%s%s from %s asks to join, code %s. Approve only if the new device shows the same code: in the Telegram bot, or on the homelab with: catflap approve %s",
			p.Req.Name, host, p.Source, codes.FormatPairingCode(code), code)}, skip)
}

// findRequest finds a pending request by id, pairing code or device name.
func (d *Daemon) findRequest(key string) (*pendingRequest, error) {
	d.reqMu.Lock()
	defer d.reqMu.Unlock()
	norm := codes.NormalizePairingCode(key)
	for _, p := range d.reqs {
		if p.ID == key || (p.Code != "" && p.Code == norm) || p.Req.Name == key {
			if p.Code == "" {
				return nil, fmt.Errorf("the request from %q isn't ready yet (no code); wait a few seconds", p.Req.Name)
			}
			return p, nil
		}
	}
	return nil, fmt.Errorf("no pending request %q (see: catflap requests)", key)
}

func (d *Daemon) forgetRequest(id string) {
	d.reqMu.Lock()
	delete(d.reqs, id)
	d.reqMu.Unlock()
}

// approveRequest adds the requesting device and sends it the sealed reply.
// deviceTTL > 0 makes it a temporary device.
func (d *Daemon) approveRequest(ctx context.Context, key, origin string, deviceTTL time.Duration) (string, error) {
	if deviceTTL < 0 || deviceTTL > 0 && deviceTTL < time.Minute {
		return "", fmt.Errorf("expires: use at least a minute, e.g. 2h or 7d")
	}
	p, err := d.findRequest(key)
	if err != nil {
		return "", err
	}
	name := p.Req.Name
	if d.nameTaken(name, p.ID) {
		return "", fmt.Errorf("the name %q was taken meanwhile; deny the request and let the device ask again with another --name", name)
	}
	// Gate 3, then gate 2, as for invites.
	path, keysOn := d.cfg.authorizedKeysPath()
	if keysOn && p.sshKey != "" {
		if err := addAuthorizedKey(path, p.sshKey, name, !d.cfg.AllowSSHKeysFromAnywhere); err != nil {
			return "", fmt.Errorf("authorized_keys: %w", err)
		}
	}
	d.mu.Lock()
	var expires time.Time
	if deviceTTL > 0 {
		expires = time.Now().Add(deviceTTL).Truncate(time.Second)
	}
	d.st.Devices = append(d.st.Devices, Device{Name: name, NodeKey: p.nk.String(), SSHKey: p.sshKey, Hostname: p.hostname, Added: time.Now(), Expires: expires})
	d.allow.Add(p.nk)
	reply, _ := json.Marshal(api.SealedReply{Name: name, Homelab: d.cfg.Name, Addr: string(d.addr), SSHUser: d.cfg.SSHUser})
	d.saveLocked()
	d.mu.Unlock()
	rollback := func() {
		d.mu.Lock()
		d.st.Devices = slices.DeleteFunc(d.st.Devices, func(dev Device) bool { return dev.NodeKey == p.nk.String() })
		d.allow.Remove(p.nk)
		d.saveLocked()
		d.mu.Unlock()
		if keysOn && p.sshKey != "" {
			removeAuthorizedKey(path, name)
		}
	}
	sealed, err := codes.SealTo(p.box, reply)
	if err == nil {
		err = d.requestAction(ctx, api.RequestAction{ID: p.ID, Action: "approve", Name: name, Sealed: sealed})
	}
	if err != nil {
		rollback()
		return "", fmt.Errorf("approving at the relay: %w", err)
	}
	d.forgetRequest(p.ID)
	d.logf("requests: approved %q (%s) %s", name, p.nk.ShortString(), via(origin))
	if b := d.currentBot(); b != nil && p.tgMsg != 0 {
		go b.requestDecided(p.tgMsg, fmt.Sprintf("✓ Approved: <b>%s</b> is enrolled.", esc(name)))
	}
	d.alert(notify.Message{Event: "device.enrolled", Title: "new device: " + name, Priority: notify.High,
		Body: fmt.Sprintf("%s (hostname %s) was approved %s and enrolled from %s.%s If that wasn't you, remove it: %s", name, p.hostname, via(origin), p.Source, expiresNote(expires), d.removeHint(name))}, origin)
	return name, nil
}

// denyRequest refuses a pending request.
func (d *Daemon) denyRequest(ctx context.Context, key, origin string) (string, error) {
	p, err := d.findRequest(key)
	if err != nil {
		return "", err
	}
	if err := d.requestAction(ctx, api.RequestAction{ID: p.ID, Action: "deny", Reason: "the homelab's owner said no"}); err != nil {
		return "", err
	}
	d.forgetRequest(p.ID)
	d.logf("requests: denied %q %s", p.Req.Name, via(origin))
	if b := d.currentBot(); b != nil && p.tgMsg != 0 {
		go b.requestDecided(p.tgMsg, fmt.Sprintf("Denied the request from <b>%s</b>.", esc(p.Req.Name)))
	}
	return p.Req.Name, nil
}

// pruneRequests drops requests older than their lifetime.
func (d *Daemon) pruneRequests() {
	var gone []*pendingRequest
	d.reqMu.Lock()
	for id, p := range d.reqs {
		if time.Since(p.Received) > requestPendingTTL {
			gone = append(gone, p)
			delete(d.reqs, id)
		}
	}
	d.reqMu.Unlock()
	if b := d.currentBot(); b != nil {
		for _, p := range gone {
			if p.tgMsg != 0 {
				go b.requestDecided(p.tgMsg, fmt.Sprintf("The request from <b>%s</b> expired.", esc(p.Req.Name)))
			}
		}
	}
}

func (d *Daemon) requestsInfo() RequestsInfo {
	d.reqMu.Lock()
	defer d.reqMu.Unlock()
	info := RequestsInfo{Until: d.reqUntil, Pending: []RequestInfo{}}
	for _, p := range d.reqs {
		info.Pending = append(info.Pending, RequestInfo{p.Req.Name, p.hostname, p.Source, p.Code, p.Received})
	}
	sort.Slice(info.Pending, func(i, j int) bool { return info.Pending[i].Received.Before(info.Pending[j].Received) })
	return info
}

func (d *Daemon) requestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /requests", func(w http.ResponseWriter, r *http.Request) {
		ctlJSON(w, d.requestsInfo())
	})
	mux.HandleFunc("POST /requests/window", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			For time.Duration `json:"for"` // 0 closes the window
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.For < 0 {
			ctlErr(w, http.StatusBadRequest, "bad request")
			return
		}
		until, err := d.setRequestWindow(r.Context(), req.For, "")
		if err != nil {
			ctlErr(w, http.StatusBadGateway, err.Error())
			return
		}
		ctlJSON(w, RequestsInfo{Until: until, Pending: []RequestInfo{}})
	})
	for _, action := range []string{"approve", "deny"} {
		mux.HandleFunc("POST /requests/"+action, func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Code    string        `json:"code"`
				Expires time.Duration `json:"expires"` // approve: temporary device
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Code == "" {
				ctlErr(w, http.StatusBadRequest, "bad request")
				return
			}
			var name string
			var err error
			if action == "deny" {
				name, err = d.denyRequest(r.Context(), req.Code, "")
			} else {
				name, err = d.approveRequest(r.Context(), req.Code, "", req.Expires)
			}
			if err != nil {
				ctlErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ctlJSON(w, map[string]string{"name": name})
		})
	}
}
