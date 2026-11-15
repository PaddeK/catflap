package relay

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

const maxBody = 64 << 10

type apiServer struct {
	cfg   Config
	store *store
	logf  func(string, ...any)

	enrollLimit  *ipLimiter
	joinLimit    *ipLimiter
	requestLimit *ipLimiter
}

func newAPIServer(cfg Config, st *store, logf func(string, ...any)) *apiServer {
	return &apiServer{
		cfg:          cfg,
		store:        st,
		logf:         logf,
		enrollLimit:  newIPLimiter(10, 10*time.Minute),
		joinLimit:    newIPLimiter(10, 10*time.Minute),
		requestLimit: newIPLimiter(10, time.Hour),
	}
}

func (a *apiServer) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/enroll", a.handleEnroll)
	mux.HandleFunc("GET /v1/enroll/{invite}/{sub}", a.handleEnrollStatus)
	mux.HandleFunc("POST /v1/agent/join", a.handleJoin)
	mux.HandleFunc("POST /v1/agent/invites", a.agentOnly(a.handleInviteRegister))
	mux.HandleFunc("DELETE /v1/agent/invites/{invite}", a.agentOnly(a.handleInviteRevoke))
	mux.HandleFunc("GET /v1/agent/events", a.agentOnly(a.handleEvents))
	mux.HandleFunc("POST /v1/agent/result", a.agentOnly(a.handleResult))
	mux.HandleFunc("POST /v1/agent/sync", a.agentOnly(a.handleSync))
	mux.HandleFunc("GET /v1/agent/status", a.agentOnly(a.handleStatus))
	mux.HandleFunc("DELETE /v1/agent", a.agentOnly(a.handleLeave))
	mux.HandleFunc("POST /v1/request", a.handleRequestCreate)
	mux.HandleFunc("GET /v1/request/{id}", a.handleRequestStatus)
	mux.HandleFunc("POST /v1/request/{id}/reveal", a.handleRequestReveal)
	mux.HandleFunc("POST /v1/agent/requests", a.agentOnly(a.handleRequestWindow))
	mux.HandleFunc("POST /v1/agent/request", a.agentOnly(a.handleRequestAction))
}

// admitHandler answers derpserver's --verify-client-url style requests.
func (a *apiServer) admitHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req tailcfg.DERPAdmitClientRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ok := a.store.admitted(req.NodePublic)
		verdict := "DENY "
		if ok {
			verdict = "ALLOW"
		}
		a.logf("admit: %s %s from %s", verdict, req.NodePublic.ShortString(), req.Source)
		writeJSON(w, http.StatusOK, tailcfg.DERPAdmitClientResponse{Allow: ok})
	})
}

// ---- public enrollment ----

func (a *apiServer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !a.enrollLimit.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many requests, try again later")
		return
	}
	var req api.EnrollRequest
	if !readJSON(w, r, &req) {
		return
	}
	var nk key.NodePublic
	switch {
	case !codes.ValidID(req.InviteID):
		writeErr(w, http.StatusBadRequest, "bad invite id")
		return
	case nk.UnmarshalText([]byte(req.NodeKey)) != nil || nk.IsZero():
		writeErr(w, http.StatusBadRequest, "bad node key")
		return
	case len(req.SSHKey) > 16<<10, len(req.Hostname) > 64, len(req.Nonce) > 64, req.Nonce == "", len(req.MAC) > 64, req.MAC == "":
		writeErr(w, http.StatusBadRequest, "bad request fields")
		return
	}

	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now())
	inv, ok := s.st.Invites[req.InviteID]
	if !ok || inv.Closed {
		writeErr(w, http.StatusNotFound, "unknown, used or expired invite")
		return
	}
	if len(inv.Subs) >= maxSubsPerInvite {
		writeErr(w, http.StatusTooManyRequests, "too many attempts for this invite")
		return
	}
	sb := &sub{ID: codes.RandomID(), Req: req, Source: ip, Received: time.Now(), Status: api.StatusPending}
	inv.Subs = append(inv.Subs, sb)
	if err := s.saveLocked(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relay storage error")
		return
	}
	s.notifyLocked()
	a.logf("enroll: submission %s for invite %s from %s (%s)", sb.ID, req.InviteID, ip, nk.ShortString())
	writeJSON(w, http.StatusOK, api.EnrollResponse{SubmissionID: sb.ID})
}

func (a *apiServer) handleEnrollStatus(w http.ResponseWriter, r *http.Request) {
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if inv, ok := s.st.Invites[r.PathValue("invite")]; ok {
		for _, sb := range inv.Subs {
			if sb.ID == r.PathValue("sub") {
				writeJSON(w, http.StatusOK, api.EnrollStatus{Status: sb.Status, Reason: sb.Reason, Sealed: sb.Sealed})
				return
			}
		}
	}
	writeErr(w, http.StatusNotFound, "unknown submission")
}

// ---- agent API ----

type agentHandler func(w http.ResponseWriter, r *http.Request, agentID string)

func (a *apiServer) agentOnly(h agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing agent token")
			return
		}
		a.store.mu.Lock()
		id, found := a.store.byToken[codes.Hash([]byte(tok))]
		a.store.mu.Unlock()
		if !found {
			writeErr(w, http.StatusUnauthorized, "invalid agent token")
			return
		}
		h(w, r, id)
	}
}

func (a *apiServer) handleJoin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !a.joinLimit.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many requests, try again later")
		return
	}
	var req api.JoinRequest
	if !readJSON(w, r, &req) {
		return
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(req.Secret))
	var nk key.NodePublic
	if err != nil || nk.UnmarshalText([]byte(req.NodeKey)) != nil || nk.IsZero() || len(req.Name) > 64 {
		writeErr(w, http.StatusBadRequest, "bad join request")
		return
	}
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now())
	jc, ok := s.st.JoinCodes[req.JoinID]
	if !ok || !codes.HashEqual(jc.Hash, codes.Hash(secret)) {
		a.logf("join: rejected attempt from %s", ip)
		writeErr(w, http.StatusForbidden, "invalid or expired join code")
		return
	}
	delete(s.st.JoinCodes, req.JoinID)
	id := codes.RandomID()
	tok := codes.RandomToken("cft1_")
	name := req.Name
	if name == "" {
		name = "homelab"
	}
	s.st.Agents[id] = &agent{Name: name, TokenHash: codes.Hash([]byte(tok)), NodeKey: nk, Created: time.Now(), Clients: map[string]string{}}
	if err := s.saveLocked(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relay storage error")
		return
	}
	a.logf("join: new homelab %q (agent %s, %s) from %s", name, id, nk.ShortString(), ip)
	writeJSON(w, http.StatusOK, api.JoinResponse{AgentID: id, Token: tok, STUNPort: a.cfg.STUNPort})
}

func (a *apiServer) handleInviteRegister(w http.ResponseWriter, r *http.Request, agentID string) {
	var req api.InviteRegistration
	if !readJSON(w, r, &req) {
		return
	}
	now := time.Now()
	if !codes.ValidID(req.InviteID) || !req.Expires.After(now) || req.Expires.After(now.Add(maxInviteTTL+time.Hour)) {
		writeErr(w, http.StatusBadRequest, "bad invite registration")
		return
	}
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	if inv, ok := s.st.Invites[req.InviteID]; ok {
		if inv.AgentID != agentID {
			writeErr(w, http.StatusConflict, "invite id in use")
			return
		}
		writeJSON(w, http.StatusOK, struct{}{}) // idempotent re-registration
		return
	}
	open := 0
	for _, inv := range s.st.Invites {
		if inv.AgentID == agentID && !inv.Closed {
			open++
		}
	}
	if open >= maxInvitesPerAgent {
		writeErr(w, http.StatusTooManyRequests, "too many open invites")
		return
	}
	s.st.Invites[req.InviteID] = &invite{AgentID: agentID, Expires: req.Expires}
	if err := s.saveLocked(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relay storage error")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (a *apiServer) handleInviteRevoke(w http.ResponseWriter, r *http.Request, agentID string) {
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.st.Invites[r.PathValue("invite")]
	if !ok || inv.AgentID != agentID {
		writeJSON(w, http.StatusOK, struct{}{}) // already gone
		return
	}
	closeInviteLocked(inv, "invite was revoked")
	if err := s.saveLocked(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relay storage error")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func closeInviteLocked(inv *invite, reason string) {
	if !inv.Closed {
		inv.Closed, inv.ClosedAt = true, time.Now()
	}
	for _, sb := range inv.Subs {
		if sb.Status == api.StatusPending {
			sb.Status, sb.Reason = api.StatusRejected, reason
		}
	}
}

func (a *apiServer) handleEvents(w http.ResponseWriter, r *http.Request, agentID string) {
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	wait = min(max(wait, 0), 25)
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(wait)*time.Second)
	defer cancel()
	for {
		s := a.store
		s.mu.Lock()
		var out []api.Submission
		for id, inv := range s.st.Invites {
			if inv.AgentID != agentID {
				continue
			}
			for _, sb := range inv.Subs {
				if sb.Status == api.StatusPending {
					out = append(out, api.Submission{InviteID: id, SubmissionID: sb.ID, Request: sb.Req, Source: sb.Source, Received: sb.Received})
				}
			}
		}
		s.reqGCLocked(time.Now())
		reqs := s.requestEventsLocked(agentID)
		changed := s.changed
		s.mu.Unlock()
		if len(out) > 0 || len(reqs) > 0 {
			writeJSON(w, http.StatusOK, api.EventsResponse{Submissions: out, Requests: reqs})
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			writeJSON(w, http.StatusOK, api.EventsResponse{})
			return
		}
	}
}

func (a *apiServer) handleResult(w http.ResponseWriter, r *http.Request, agentID string) {
	var req api.ResultRequest
	if !readJSON(w, r, &req) {
		return
	}
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.st.Invites[req.InviteID]
	if !ok || inv.AgentID != agentID {
		writeErr(w, http.StatusNotFound, "unknown invite")
		return
	}
	var sb *sub
	for _, x := range inv.Subs {
		if x.ID == req.SubmissionID {
			sb = x
		}
	}
	if sb == nil {
		writeErr(w, http.StatusNotFound, "unknown submission")
		return
	}
	if sb.Status != api.StatusPending {
		// Retried result: fine if it matches what we already recorded.
		if (sb.Status == api.StatusApproved) == req.Approve {
			writeJSON(w, http.StatusOK, struct{}{})
		} else {
			writeErr(w, http.StatusConflict, "submission already decided")
		}
		return
	}
	ag := s.st.Agents[agentID]
	if req.Approve {
		if req.NodeKey != sb.Req.NodeKey || req.Sealed == "" || req.Name == "" || len(req.Name) > 64 {
			writeErr(w, http.StatusBadRequest, "bad approval")
			return
		}
		if _, exists := ag.Clients[req.NodeKey]; !exists && len(ag.Clients) >= maxClientsPerAgent {
			writeErr(w, http.StatusTooManyRequests, "too many clients")
			return
		}
		ag.Clients[req.NodeKey] = req.Name
		sb.Status, sb.Sealed = api.StatusApproved, req.Sealed
		closeInviteLocked(inv, "invite already used")
		a.logf("enroll: agent %s approved %q (submission %s)", agentID, req.Name, sb.ID)
	} else {
		sb.Status, sb.Reason = api.StatusRejected, req.Reason
		if sb.Reason == "" {
			sb.Reason = "rejected by the homelab"
		}
		if req.CloseInvite {
			closeInviteLocked(inv, sb.Reason)
		}
		a.logf("enroll: agent %s rejected submission %s: %s", agentID, sb.ID, sb.Reason)
	}
	if err := s.saveLocked(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relay storage error")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (a *apiServer) handleSync(w http.ResponseWriter, r *http.Request, agentID string) {
	var req api.SyncRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Clients) > maxClientsPerAgent {
		writeErr(w, http.StatusBadRequest, "too many clients")
		return
	}
	clean := map[string]string{}
	for nk, name := range req.Clients {
		var k key.NodePublic
		if k.UnmarshalText([]byte(nk)) != nil || k.IsZero() || len(name) > 64 {
			writeErr(w, http.StatusBadRequest, "bad client key "+nk)
			return
		}
		clean[k.String()] = name
	}
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Agents[agentID].Clients = clean
	if err := s.saveLocked(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relay storage error")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// handleLeave removes the calling homelab (on uninstall or a fresh setup):
// its record, its devices at gate 1 and its open invites.
func (a *apiServer) handleLeave(w http.ResponseWriter, r *http.Request, agentID string) {
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	name := s.st.Agents[agentID].Name
	delete(s.st.Agents, agentID)
	s.gcLocked(time.Now())
	if err := s.saveLocked(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relay storage error")
		return
	}
	a.logf("leave: homelab %q (agent %s) removed itself", name, agentID)
	writeJSON(w, http.StatusOK, struct{}{})
}

func (a *apiServer) handleStatus(w http.ResponseWriter, r *http.Request, agentID string) {
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	ag := s.st.Agents[agentID]
	st := api.AgentStatus{AgentID: agentID, Name: ag.Name, Clients: ag.Clients, OpenInvites: []string{}}
	for id, inv := range s.st.Invites {
		if inv.AgentID == agentID && !inv.Closed {
			st.OpenInvites = append(st.OpenInvites, id)
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// ---- helpers ----

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.ErrorResponse{Error: msg})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipLimiter allows n requests per window per IP (fixed window).
type ipLimiter struct {
	n      int
	window time.Duration

	mu   sync.Mutex
	hits map[string]*window
}

type window struct {
	start time.Time
	count int
}

func newIPLimiter(n int, w time.Duration) *ipLimiter {
	return &ipLimiter{n: n, window: w, hits: map[string]*window{}}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > 10000 { // bound memory
		for k, v := range l.hits {
			if now.Sub(v.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	h, ok := l.hits[ip]
	if !ok || now.Sub(h.start) > l.window {
		h = &window{start: now}
		l.hits[ip] = h
	}
	h.count++
	return h.count <= l.n
}
