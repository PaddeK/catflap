package homelab

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"
	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/osutil"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// JoinOptions are the inputs of `catflap setup homelab`.
type JoinOptions struct {
	JoinCode string
	Name     string
	Forwards map[string]string
	Force    bool // overwrite an existing homelab setup
}

// Join registers this machine as a homelab with the relay and writes the
// config and state files. It does not install the service.
func Join(ctx context.Context, p Paths, o JoinOptions) (Config, error) {
	code, err := codes.Parse(o.JoinCode, codes.KindJoin)
	if err != nil {
		return Config{}, err
	}
	var old State
	if fsutil.ReadJSON(p.State, &old) == nil && old.AgentToken != "" && !o.Force {
		return Config{}, fmt.Errorf("this machine is already set up as a homelab (%s); use --force to replace it, which disconnects all enrolled devices", p.State)
	}
	if o.Name == "" {
		o.Name = "homelab"
	}
	if !ValidName(o.Name) {
		return Config{}, fmt.Errorf("invalid homelab name %q", o.Name)
	}
	for port, target := range o.Forwards {
		if !strings.Contains(target, ":") {
			return Config{}, fmt.Errorf("forward %s=%s: target must be host:port", port, target)
		}
	}

	st := State{ServerKey: key.NewNode(), PSK: tailcat.NewPresharedKey()}
	rc := api.NewClient(code.Relay, "")
	var resp api.JoinResponse
	err = rc.Do(ctx, "POST", "/v1/agent/join", api.JoinRequest{
		JoinID:  code.ID,
		Secret:  strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(code.Secret)),
		Name:    o.Name,
		NodeKey: st.ServerKey.Public().String(),
	}, &resp)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status == 403 {
			return Config{}, errors.New("the relay refused the join code (wrong, already used or expired); create a new one with `sudo catflap relay join-code` on the relay")
		}
		return Config{}, fmt.Errorf("contacting relay %s: %w", code.Relay, err)
	}
	st.AgentID, st.AgentToken, st.STUNPort = resp.AgentID, resp.Token, resp.STUNPort

	// --force: the new registration worked, so remove the old one from its
	// relay instead of leaving a stale record there.
	if old.AgentToken != "" {
		var oldCfg Config
		if fsutil.ReadJSON(p.Config, &oldCfg) == nil && oldCfg.Relay != "" {
			if err := Deregister(ctx, oldCfg.Relay, old.AgentToken); err != nil {
				log.Printf("note: could not remove the old homelab record from %s (%v); remove it there with: sudo catflap relay remove", oldCfg.Relay, err)
			}
		}
	}

	cfg := Config{Relay: code.Relay, Name: o.Name, Forwards: o.Forwards}
	if len(cfg.Forwards) == 0 {
		cfg.Forwards = map[string]string{"22": "127.0.0.1:22"}
	}
	cfg.SSHUser = osutil.SSHUsername()
	var oldCfg Config
	if fsutil.ReadJSON(p.Config, &oldCfg) == nil {
		// Keep hand-made settings on --force.
		cfg.AuthorizedKeys, cfg.AllowSSHKeysFromAnywhere = oldCfg.AuthorizedKeys, oldCfg.AllowSSHKeysFromAnywhere
		if len(o.Forwards) == 0 && len(oldCfg.Forwards) > 0 {
			cfg.Forwards = oldCfg.Forwards
		}
	}
	if err := fsutil.WriteJSON(p.State, &st, 0o600); err != nil {
		return Config{}, err
	}
	if err := fsutil.WriteJSON(p.Config, &cfg, 0o644); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
