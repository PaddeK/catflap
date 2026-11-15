// Package api defines the relay's HTTP API (shared by relay, homelab and
// client) and a small JSON client for it.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Version is sent as the User-Agent.
var Version = "dev"

// ---- public enrollment API (no auth) ----

// EnrollRequest is POSTed by a new device to /v1/enroll.
type EnrollRequest struct {
	InviteID string `json:"invite_id"`
	NodeKey  string `json:"nodekey"`  // tailcat client node key, "nodekey:<hex>"
	SSHKey   string `json:"ssh_key"`  // SSH public key line, may be empty
	Hostname string `json:"hostname"` // informational
	Nonce    string `json:"nonce"`
	MAC      string `json:"mac"` // codes.EnrollMAC over the fields above
}

// EnrollResponse identifies the submission for polling.
type EnrollResponse struct {
	SubmissionID string `json:"submission_id"`
}

// Submission statuses.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// EnrollStatus is returned by GET /v1/enroll/{invite}/{submission}.
type EnrollStatus struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"` // for rejected
	Sealed string `json:"sealed,omitempty"` // for approved: codes.Seal(SealedReply)
}

// SealedReply is what the homelab seals for an approved device.
type SealedReply struct {
	Name    string `json:"name"`     // device name chosen by the homelab admin
	Homelab string `json:"homelab"`  // homelab name, suggested ssh alias
	Addr    string `json:"addr"`     // tailcat address of the homelab (secret)
	SSHUser string `json:"ssh_user"` // user to log in as
}

// ---- agent API (homelab, bearer token) ----

// JoinRequest registers a homelab (agent) with the relay.
type JoinRequest struct {
	JoinID  string `json:"join_id"`
	Secret  string `json:"secret"` // base32 secret from the join code
	Name    string `json:"name"`
	NodeKey string `json:"nodekey"` // the homelab's tailcat server key
}

// JoinResponse returns the long-lived agent credentials.
type JoinResponse struct {
	AgentID  string `json:"agent_id"`
	Token    string `json:"token"`
	STUNPort int    `json:"stun_port"` // 0 = STUN disabled on the relay
}

// InviteRegistration announces an invite id so the relay accepts
// enrollments for it. The relay never learns the invite secret.
type InviteRegistration struct {
	InviteID string    `json:"invite_id"`
	Expires  time.Time `json:"expires"`
}

// Submission is a queued enrollment delivered to the agent.
type Submission struct {
	InviteID     string        `json:"invite_id"`
	SubmissionID string        `json:"submission_id"`
	Request      EnrollRequest `json:"request"`
	Source       string        `json:"source"` // client IP, informational
	Received     time.Time     `json:"received"`
}

// EventsResponse is the answer to the agent's long poll.
type EventsResponse struct {
	Submissions []Submission `json:"submissions"`
}

// ResultRequest answers one submission. Approving admits NodeKey at the
// relay, closes the invite and stores Sealed for the client, atomically.
type ResultRequest struct {
	InviteID     string `json:"invite_id"`
	SubmissionID string `json:"submission_id"`
	Approve      bool   `json:"approve"`
	Reason       string `json:"reason,omitempty"`
	CloseInvite  bool   `json:"close_invite,omitempty"` // reject and close (expired/used)
	NodeKey      string `json:"nodekey,omitempty"`
	Name         string `json:"name,omitempty"`
	Sealed       string `json:"sealed,omitempty"`
}

// SyncRequest sets the complete list of client keys this agent wants
// admitted. The homelab is the source of truth; the relay mirrors it.
type SyncRequest struct {
	Clients map[string]string `json:"clients"` // nodekey -> name
}

// AgentStatus is returned by GET /v1/agent/status.
type AgentStatus struct {
	AgentID     string            `json:"agent_id"`
	Name        string            `json:"name"`
	Clients     map[string]string `json:"clients"`
	OpenInvites []string          `json:"open_invites"`
}

// ErrorResponse is the body of any non-2xx answer.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Error is returned by Client for non-2xx answers.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("relay: %s (HTTP %d)", e.Msg, e.Status) }

// Client talks to a relay.
type Client struct {
	Base  string // https://host[:port]
	Token string // agent token, optional
	HTTP  *http.Client
}

// NewClient returns a client for relay (host or host:port).
func NewClient(relay, token string) *Client {
	return &Client{
		Base:  "https://" + relay,
		Token: token,
		HTTP:  &http.Client{Timeout: 45 * time.Second},
	}
}

// Do sends in as JSON (if non-nil) and decodes the answer into out (if non-nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "catflap/"+Version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var er ErrorResponse
		if json.Unmarshal(data, &er) != nil || er.Error == "" {
			er.Error = strings.TrimSpace(string(data))
			if len(er.Error) > 200 {
				er.Error = er.Error[:200]
			}
		}
		return &Error{Status: resp.StatusCode, Msg: er.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
