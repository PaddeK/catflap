package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// enrollByRequest is `catflap enroll --request homelab@relay`: no invite;
// the user compares a pairing code and approves on the phone or homelab.
func enrollByRequest(ctx context.Context, o EnrollOptions) error {
	out := o.Out
	homelabName, relay, ok := strings.Cut(o.Request, "@")
	if !ok || homelabName == "" {
		return errors.New("--request wants HOMELAB@RELAY, e.g. homelab@relay.example.com")
	}
	if err := codes.ValidateRelay(relay); err != nil {
		return err
	}
	if reservedAlias(o.Alias) {
		return fmt.Errorf("--alias %q is reserved (it is a `catflap forward` subcommand); pick another name", o.Alias)
	}
	host, _ := os.Hostname()
	if len(host) > 64 {
		host = host[:64]
	}
	name := o.Name
	if name == "" {
		name = deviceNameFrom(host)
	}
	if !deviceNameRE.MatchString(name) {
		return fmt.Errorf("invalid device name %q: use lowercase letters, digits, '.', '-' or '_' (--name)", name)
	}
	st, path, err := Load()
	if err != nil {
		return err
	}
	sshKey, identity := "", ""
	if !o.NoSSHKey {
		if sshKey, identity, err = pickSSHKey(o.SSHKeyFile, out); err != nil {
			return err
		}
	}
	devKey := key.NewNode()
	boxPub, boxPriv, err := codes.NewBoxKey()
	if err != nil {
		return err
	}
	reveal := codes.RandomReveal()
	req := api.RequestCreate{
		Homelab: homelabName, Name: name, NodeKey: devKey.Public().String(), BoxKey: codes.EncodeBoxKey(boxPub),
		SSHKey: sshKey, Hostname: host, Commit: codes.RequestCommit(reveal),
	}
	rc := api.NewClient(relay, "")
	var created api.RequestCreated
	if err := rc.Do(ctx, "POST", "/v1/request", req, &created); err != nil {
		var ae *api.Error
		if errors.As(err, &ae) {
			return errors.New(ae.Msg)
		}
		return fmt.Errorf("contacting relay %s: %w", relay, err)
	}
	fmt.Fprintf(out, "Asked homelab %q to let this device in as %q (via %s).\n", homelabName, name, relay)

	revealed := false
	deadline := time.Now().Add(11 * time.Minute)
	for {
		var status api.RequestStatus
		if err := rc.Do(ctx, "GET", "/v1/request/"+created.ID, nil, &status); err != nil {
			var ae *api.Error
			if errors.As(err, &ae) && ae.Status == 404 {
				return errors.New("the request expired without an answer; ask again")
			}
			return err
		}
		switch status.Status {
		case api.StatusReveal:
			if revealed {
				break
			}
			nonce, err := codes.DecodeBytes(status.Nonce)
			if err != nil {
				return errors.New("bad answer from the relay")
			}
			if err := rc.Do(ctx, "POST", "/v1/request/"+created.ID+"/reveal", api.RequestReveal{Reveal: codes.EncodeBytes(reveal)}, nil); err != nil {
				return err
			}
			code := codes.PairingCode(codes.PairingFields{
				HomelabKey: created.HomelabKey, Homelab: homelabName, Name: name,
				NodeKey: req.NodeKey, BoxKey: req.BoxKey, SSHKey: req.SSHKey, Hostname: host,
			}, reveal, nonce)
			fmt.Fprintf(out, `
  Code:  %s

Approve the request on your phone (in the homelab's Telegram bot) or on the
homelab (catflap approve %s), but only if it shows exactly this code.
Waiting up to 10 minutes...
`, codes.FormatPairingCode(code), code)
			revealed = true
		case api.StatusApproved:
			msg, err := codes.OpenSealed(boxPub, boxPriv, status.Sealed)
			if err != nil {
				return err
			}
			var reply api.SealedReply
			if err := json.Unmarshal(msg, &reply); err != nil {
				return fmt.Errorf("bad reply from homelab: %w", err)
			}
			// The address must belong to the homelab the code was computed for.
			ci, err := tailcat.ParseAddr(tailcat.Addr(reply.Addr))
			if err != nil {
				return fmt.Errorf("homelab sent an invalid address: %w", err)
			}
			if ci.ServerPublic.String() != created.HomelabKey {
				return errors.New("the reply doesn't come from the homelab the code was made for; nothing was saved. Tell the homelab's owner")
			}
			return finishEnroll(ctx, o, st, path, devKey, relay, identity, reply)
		case api.StatusRejected:
			return fmt.Errorf("request denied: %s", status.Reason)
		}
		if time.Now().After(deadline) {
			return errors.New("no answer within 10 minutes; ask again")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

var deviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// deviceNameFrom turns a hostname into a device name ("Paddes-MacBook.local"
// -> "paddes-macbook").
func deviceNameFrom(host string) string {
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	b := []byte(host)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			b[i] = '-'
		}
	}
	s := strings.Trim(string(b), "-_")
	if len(s) > 63 {
		s = s[:63]
	}
	if s == "" {
		s = "device"
	}
	return s
}
