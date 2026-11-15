package relay

import (
	"fmt"
	"net/http"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"

	"tailscale.com/types/key"
)

// Enrollment requests (tap to approve): a device without an invite asks a
// homelab by name. Only while the homelab has its request window open; the
// homelab decides (its user compares a pairing code and approves in
// Telegram or on the homelab). Requests live in memory only: they're short,
// and only the resulting admission (gate 1) is persisted.

const (
	requestTTL          = 10 * time.Minute
	keepDecidedRequest  = 5 * time.Minute // so the device can fetch the result
	maxRequestsPerAgent = 3
	maxRequestWindow    = 7 * 24 * time.Hour
)

// request stages
const (
	rqNew      = "new"      // waiting for the homelab's nonce
	rqNonce    = "nonce"    // waiting for the device's reveal
	rqRevealed = "revealed" // the homelab has to show the code
	rqShown    = "shown"    // waiting for the user's decision
	rqApproved = "approved"
	rqDenied   = "denied"
)

type request struct {
	ID, AgentID string
	Req         api.RequestCreate
	Source      string
	Created     time.Time
	Decided     time.Time
	Stage       string
	Nonce       string
	Reveal      string
	Sealed      string
	Reason      string
}

func (rq *request) decided() bool { return rq.Stage == rqApproved || rq.Stage == rqDenied }

// reqGCLocked drops expired requests and those of removed homelabs.
func (s *store) reqGCLocked(now time.Time) {
	for id, rq := range s.reqs {
		_, agentOK := s.st.Agents[rq.AgentID]
		switch {
		case !agentOK:
			delete(s.reqs, id)
		case rq.decided() && now.Sub(rq.Decided) > keepDecidedRequest:
			delete(s.reqs, id)
		case !rq.decided() && now.Sub(rq.Created) > requestTTL:
			delete(s.reqs, id)
		}
	}
}

func (a *apiServer) handleRequestCreate(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !a.requestLimit.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many requests, try again later")
		return
	}
	var req api.RequestCreate
	if !readJSON(w, r, &req) {
		return
	}
	var nk key.NodePublic
	_, boxErr := codes.DecodeBoxKey(req.BoxKey)
	switch {
	case req.Homelab == "" || len(req.Homelab) > 64 || req.Name == "" || len(req.Name) > 64:
		writeErr(w, http.StatusBadRequest, "bad homelab or device name")
		return
	case nk.UnmarshalText([]byte(req.NodeKey)) != nil || nk.IsZero() || boxErr != nil:
		writeErr(w, http.StatusBadRequest, "bad keys")
		return
	case len(req.SSHKey) > 16<<10, len(req.Hostname) > 64, len(req.Commit) != 43:
		writeErr(w, http.StatusBadRequest, "bad request fields")
		return
	}
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.reqGCLocked(now)
	var match []string
	for id, ag := range s.st.Agents {
		if ag.Name == req.Homelab && now.Before(ag.RequestsUntil) {
			match = append(match, id)
		}
	}
	switch len(match) {
	case 0:
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no homelab %q accepts enrollment requests right now; open a window on it first (catflap requests on, or /requests on in its Telegram bot)", req.Homelab))
		return
	case 1:
	default:
		writeErr(w, http.StatusConflict, fmt.Sprintf("several homelabs are called %q on this relay; use an invite instead", req.Homelab))
		return
	}
	agentID := match[0]
	open := 0
	for _, rq := range s.reqs {
		if rq.AgentID == agentID && !rq.decided() {
			open++
		}
	}
	if open >= maxRequestsPerAgent {
		writeErr(w, http.StatusTooManyRequests, "too many open requests for this homelab, try again later")
		return
	}
	rq := &request{ID: codes.RandomID() + codes.RandomID(), AgentID: agentID, Req: req, Source: ip, Created: now, Stage: rqNew}
	s.reqs[rq.ID] = rq
	s.notifyLocked()
	a.logf("request: %q asks homelab %q from %s (%s)", req.Name, req.Homelab, ip, nk.ShortString())
	writeJSON(w, http.StatusOK, api.RequestCreated{ID: rq.ID, HomelabKey: s.st.Agents[agentID].NodeKey.String()})
}

func (a *apiServer) handleRequestStatus(w http.ResponseWriter, r *http.Request) {
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqGCLocked(time.Now())
	rq, ok := s.reqs[r.PathValue("id")]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown or expired request")
		return
	}
	st := api.RequestStatus{}
	switch rq.Stage {
	case rqNew:
		st.Status = api.StatusWaiting
	case rqNonce:
		st.Status, st.Nonce = api.StatusReveal, rq.Nonce
	case rqRevealed, rqShown:
		st.Status = api.StatusPending
	case rqApproved:
		st.Status, st.Sealed = api.StatusApproved, rq.Sealed
	case rqDenied:
		st.Status, st.Reason = api.StatusRejected, rq.Reason
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *apiServer) handleRequestReveal(w http.ResponseWriter, r *http.Request) {
	var req api.RequestReveal
	if !readJSON(w, r, &req) {
		return
	}
	reveal, err := codes.DecodeBytes(req.Reveal)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad reveal")
		return
	}
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	rq, ok := s.reqs[r.PathValue("id")]
	switch {
	case !ok:
		writeErr(w, http.StatusNotFound, "unknown or expired request")
	case rq.Stage != rqNonce:
		if rq.Reveal == req.Reveal { // retried
			writeJSON(w, http.StatusOK, struct{}{})
		} else {
			writeErr(w, http.StatusConflict, "not expecting a reveal now")
		}
	case !codes.CheckCommit(rq.Req.Commit, reveal):
		writeErr(w, http.StatusBadRequest, "the reveal doesn't match the commitment")
	default:
		rq.Reveal, rq.Stage = req.Reveal, rqRevealed
		s.notifyLocked()
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// requestEventsLocked lists what the homelab has to act on.
func (s *store) requestEventsLocked(agentID string) []api.RequestEvent {
	var out []api.RequestEvent
	for _, rq := range s.reqs {
		if rq.AgentID != agentID || (rq.Stage != rqNew && rq.Stage != rqRevealed) {
			continue
		}
		out = append(out, api.RequestEvent{ID: rq.ID, Stage: rq.Stage, Request: rq.Req, Reveal: rq.Reveal, Source: rq.Source, Received: rq.Created})
	}
	return out
}

func (a *apiServer) handleRequestWindow(w http.ResponseWriter, r *http.Request, agentID string) {
	var req api.RequestWindow
	if !readJSON(w, r, &req) {
		return
	}
	if req.Until.After(time.Now().Add(maxRequestWindow)) {
		req.Until = time.Now().Add(maxRequestWindow)
	}
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Agents[agentID].RequestsUntil = req.Until
	if err := s.saveLocked(); err != nil {
		writeErr(w, http.StatusInternalServerError, "relay storage error")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (a *apiServer) handleRequestAction(w http.ResponseWriter, r *http.Request, agentID string) {
	var req api.RequestAction
	if !readJSON(w, r, &req) {
		return
	}
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	rq, ok := s.reqs[req.ID]
	if !ok || rq.AgentID != agentID {
		writeErr(w, http.StatusNotFound, "unknown or expired request")
		return
	}
	bad := func() { writeErr(w, http.StatusConflict, "request is at stage "+rq.Stage) }
	switch req.Action {
	case "nonce":
		if _, err := codes.DecodeBytes(req.Nonce); err != nil {
			writeErr(w, http.StatusBadRequest, "bad nonce")
			return
		}
		if rq.Stage != rqNew {
			bad()
			return
		}
		rq.Nonce, rq.Stage = req.Nonce, rqNonce
	case "shown":
		if rq.Stage != rqRevealed && rq.Stage != rqShown {
			bad()
			return
		}
		rq.Stage = rqShown
	case "approve":
		if rq.Stage != rqRevealed && rq.Stage != rqShown {
			bad()
			return
		}
		if req.Sealed == "" || req.Name == "" || len(req.Name) > 64 {
			writeErr(w, http.StatusBadRequest, "bad approval")
			return
		}
		ag := s.st.Agents[agentID]
		if _, exists := ag.Clients[rq.Req.NodeKey]; !exists && len(ag.Clients) >= maxClientsPerAgent {
			writeErr(w, http.StatusTooManyRequests, "too many clients")
			return
		}
		ag.Clients[rq.Req.NodeKey] = req.Name
		rq.Stage, rq.Sealed, rq.Decided = rqApproved, req.Sealed, time.Now()
		if err := s.saveLocked(); err != nil {
			writeErr(w, http.StatusInternalServerError, "relay storage error")
			return
		}
		a.logf("request: homelab %s approved %q", agentID, req.Name)
	case "deny":
		if rq.decided() {
			bad()
			return
		}
		rq.Stage, rq.Reason, rq.Decided = rqDenied, req.Reason, time.Now()
		if rq.Reason == "" {
			rq.Reason = "denied by the homelab"
		}
		a.logf("request: homelab %s denied %q: %s", agentID, rq.Req.Name, rq.Reason)
	default:
		writeErr(w, http.StatusBadRequest, "unknown action")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
