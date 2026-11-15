package relay

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"
	"github.com/PaddeK/catflap/internal/fsutil"

	"tailscale.com/types/key"
)

// Limits.
const (
	maxSubsPerInvite    = 5
	maxInvitesPerAgent  = 50
	maxInviteTTL        = 90 * 24 * time.Hour
	keepClosedInvite    = 30 * time.Minute // so the client can fetch its result
	maxClientsPerAgent  = 500
	defaultJoinCodeTTL  = time.Hour
	maxPendingJoinCodes = 10
)

// state is persisted to state.json. It holds only hashes of secrets,
// except the relay's own DERP key.
type state struct {
	DERPKey   key.NodePrivate      `json:"derp_key"`
	JoinCodes map[string]*joinCode `json:"join_codes"`
	Agents    map[string]*agent    `json:"agents"`
	Invites   map[string]*invite   `json:"invites"`
}

type joinCode struct {
	Hash    string    `json:"hash"` // sha256 of the secret
	Expires time.Time `json:"expires"`
}

type agent struct {
	Name      string            `json:"name"`
	TokenHash string            `json:"token_hash"`
	NodeKey   key.NodePublic    `json:"nodekey"` // the homelab's own tailcat key
	Created   time.Time         `json:"created"`
	Clients   map[string]string `json:"clients"` // nodekey -> device name
	// RequestsUntil is the end of the homelab's request window (tap to
	// approve); enrollment requests are refused outside it.
	RequestsUntil time.Time `json:"requests_until,omitzero"`
}

type invite struct {
	AgentID  string    `json:"agent_id"`
	Expires  time.Time `json:"expires"`
	Closed   bool      `json:"closed,omitempty"`
	ClosedAt time.Time `json:"closed_at,omitzero"`
	Subs     []*sub    `json:"subs,omitempty"`
}

type sub struct {
	ID       string            `json:"id"`
	Req      api.EnrollRequest `json:"req"`
	Source   string            `json:"source"`
	Received time.Time         `json:"received"`
	Status   string            `json:"status"`
	Reason   string            `json:"reason,omitempty"`
	Sealed   string            `json:"sealed,omitempty"`
}

// store guards state and persists every change.
type store struct {
	path string

	mu       sync.Mutex
	st       state
	allowed  map[key.NodePublic]bool // derived: everything admitted at gate 1
	byToken  map[string]string       // derived: token hash -> agent id
	changed  chan struct{}           // closed and replaced when submissions arrive
	reqs     map[string]*request     // enrollment requests (memory only)
	lastSave error
}

func openStore(path string) (*store, error) {
	s := &store{path: path, changed: make(chan struct{}), reqs: map[string]*request{}}
	err := fsutil.ReadJSON(path, &s.st)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.st.DERPKey = key.NewNode()
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if s.st.DERPKey.IsZero() {
		return nil, fmt.Errorf("%s has no derp_key", path)
	}
	if s.st.JoinCodes == nil {
		s.st.JoinCodes = map[string]*joinCode{}
	}
	if s.st.Agents == nil {
		s.st.Agents = map[string]*agent{}
	}
	if s.st.Invites == nil {
		s.st.Invites = map[string]*invite{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// saveLocked rebuilds derived indexes and writes the state file.
func (s *store) saveLocked() error {
	s.allowed = map[key.NodePublic]bool{}
	s.byToken = map[string]string{}
	for id, a := range s.st.Agents {
		s.byToken[a.TokenHash] = id
		if !a.NodeKey.IsZero() {
			s.allowed[a.NodeKey] = true
		}
		for nk := range a.Clients {
			var k key.NodePublic
			if k.UnmarshalText([]byte(nk)) == nil {
				s.allowed[k] = true
			}
		}
	}
	s.lastSave = fsutil.WriteJSON(s.path, &s.st, 0o600)
	return s.lastSave
}

func (s *store) admitted(k key.NodePublic) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowed[k]
}

// gcLocked drops expired join codes and old invites.
func (s *store) gcLocked(now time.Time) {
	for id, j := range s.st.JoinCodes {
		if now.After(j.Expires) {
			delete(s.st.JoinCodes, id)
		}
	}
	for id, inv := range s.st.Invites {
		if inv.Closed && now.Sub(inv.ClosedAt) > keepClosedInvite {
			delete(s.st.Invites, id)
		} else if !inv.Closed && now.After(inv.Expires) {
			inv.Closed, inv.ClosedAt = true, now
		}
		if _, ok := s.st.Agents[inv.AgentID]; !ok {
			delete(s.st.Invites, id)
		}
	}
}

// newJoinCode creates and stores a join code.
func (s *store) newJoinCode(relayAddr string, ttl time.Duration) (codes.Code, error) {
	c, err := codes.New(codes.KindJoin, relayAddr)
	if err != nil {
		return c, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now())
	if len(s.st.JoinCodes) >= maxPendingJoinCodes {
		return c, errors.New("too many unused join codes; wait for them to expire")
	}
	s.st.JoinCodes[c.ID] = &joinCode{Hash: codes.Hash(c.Secret), Expires: time.Now().Add(ttl)}
	return c, s.saveLocked()
}

// notifyLocked wakes long-polling agents.
func (s *store) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}
