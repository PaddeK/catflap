package homelab

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"
	"github.com/PaddeK/catflap/internal/fsutil"
)

// `catflap homelab move`: move the homelab to another relay (a new hostname
// or port). The homelab keeps its keys and its device list; because the
// relay's name is part of the address every client stores, each device gets
// a re-enroll invite that replaces its old entry.

// MoveResult reports a move.
type MoveResult struct {
	OldRelay, NewRelay string
	OldRelayErr        error          // de-registering at the old relay failed
	Invites            []InviteResult // one re-enroll invite per device
}

// MoveTarget parses the join code and checks it's for another relay.
func MoveTarget(p Paths, joinCode string) (string, error) {
	code, err := codes.Parse(joinCode, codes.KindJoin)
	if err != nil {
		return "", err
	}
	var cfg Config
	if err := fsutil.ReadJSON(p.Config, &cfg); err != nil {
		return "", fmt.Errorf("reading %s: %w (is a homelab set up here?)", p.Config, err)
	}
	if code.Relay == cfg.Relay {
		return "", fmt.Errorf("the homelab uses relay %s already", cfg.Relay)
	}
	return code.Relay, nil
}

// Move registers the homelab with the relay of joinCode, creates a
// re-enroll invite (valid ttl) for every device, switches config and state
// to the new relay and removes the homelab from the old one. The daemon must
// not be running: it would overwrite the state.
func Move(ctx context.Context, p Paths, joinCode string, ttl time.Duration) (MoveResult, error) {
	var res MoveResult
	code, err := codes.Parse(joinCode, codes.KindJoin)
	if err != nil {
		return res, err
	}
	var cfg Config
	if err := fsutil.ReadJSON(p.Config, &cfg); err != nil {
		return res, fmt.Errorf("reading %s: %w", p.Config, err)
	}
	var st State
	if err := fsutil.ReadJSON(p.State, &st); err != nil {
		return res, fmt.Errorf("reading %s: %w", p.State, err)
	}
	if st.ServerKey.IsZero() || st.AgentToken == "" {
		return res, errors.New("homelab state is incomplete")
	}
	if code.Relay == cfg.Relay {
		return res, fmt.Errorf("the homelab uses relay %s already", cfg.Relay)
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	res.OldRelay, res.NewRelay = cfg.Relay, code.Relay
	name := cfg.Name
	if name == "" {
		name = "homelab"
	}

	// 1. Register with the new relay, keeping the homelab's own key.
	rc := api.NewClient(code.Relay, "")
	var jr api.JoinResponse
	err = rc.Do(ctx, "POST", "/v1/agent/join", api.JoinRequest{
		JoinID:  code.ID,
		Secret:  strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(code.Secret)),
		Name:    name,
		NodeKey: st.ServerKey.Public().String(),
	}, &jr)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status == 403 {
			return res, errors.New("the new relay refused the join code (wrong, already used or expired); create a new one there with `sudo catflap relay join-code`")
		}
		return res, fmt.Errorf("contacting relay %s: %w", code.Relay, err)
	}
	undo := func() { Deregister(context.Background(), code.Relay, jr.Token) } // nothing changed locally yet

	// 2. One re-enroll invite per device, registered with the new relay.
	nc := api.NewClient(code.Relay, jr.Token)
	exp := time.Now().Add(ttl).Truncate(time.Second)
	var invites []Invite
	for _, dev := range st.Devices {
		c, err := codes.New(codes.KindInvite, code.Relay)
		if err != nil {
			undo()
			return res, err
		}
		if err := nc.Do(ctx, "POST", "/v1/agent/invites", api.InviteRegistration{InviteID: c.ID, Expires: exp}, nil); err != nil {
			undo()
			return res, fmt.Errorf("registering invites with the new relay: %w", err)
		}
		invites = append(invites, Invite{ID: c.ID, Name: dev.Name, Secret: c.Secret, Created: time.Now(), Expires: exp, Replace: true})
		res.Invites = append(res.Invites, InviteResult{Code: c.String(), Name: dev.Name, Expires: exp})
	}

	// 3. Switch to the new relay: state (open invites for the old relay are
	// useless now) and the config's relay, keeping everything else in it.
	old := st
	st.AgentID, st.AgentToken, st.STUNPort = jr.AgentID, jr.Token, jr.STUNPort
	st.Invites, st.NeedsSync = invites, true
	if err := setConfigRelay(p.Config, code.Relay); err != nil {
		undo()
		return res, err
	}
	if err := fsutil.WriteJSON(p.State, &st, 0o600); err != nil {
		setConfigRelay(p.Config, cfg.Relay)
		undo()
		return res, err
	}

	// 4. Leave the old relay.
	res.OldRelayErr = Deregister(ctx, cfg.Relay, old.AgentToken)
	return res, nil
}

// setConfigRelay rewrites only the "relay" key of homelab.json.
func setConfigRelay(path, relay string) error {
	var raw map[string]json.RawMessage
	if err := fsutil.ReadJSON(path, &raw); err != nil {
		return err
	}
	v, _ := json.Marshal(relay)
	raw["relay"] = v
	return fsutil.WriteJSON(path, raw, 0o644)
}
