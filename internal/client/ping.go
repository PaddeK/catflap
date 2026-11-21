package client

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/tailscale/tailcat"
)

// probeTimeout bounds one ping, including starting the tunnel.
const probeTimeout = 15 * time.Second

// PingResult is one ping to the homelab through the tunnel.
type PingResult struct {
	Latency time.Duration `json:"latency"`
	// Direct is set when the ping went straight to the homelab, not
	// through the relay.
	Direct bool `json:"direct"`
	// Endpoint is the homelab's address on a direct path (IP:port).
	Endpoint string `json:"endpoint,omitempty"`
}

// Ping pings the homelab behind alias once, through the agent's tunnel if
// possible: the one ssh and forwards use, so the result shows their path.
func Ping(ctx context.Context, alias string) (PingResult, error) {
	if os.Getenv("CATFLAP_NO_AGENT") == "" {
		c, resp, err := callAgent(ctx, agentRequest{Op: "probe", Alias: alias})
		if err == nil {
			c.Close()
			if resp.Probe == nil {
				return PingResult{}, errors.New("the client agent sent no result")
			}
			return *resp.Probe, nil
		}
		var re *remoteError
		if errors.As(err, &re) || ctx.Err() != nil {
			return PingResult{}, err
		}
	}
	cl, err := inprocClient(alias)
	if err != nil {
		return PingResult{}, err
	}
	return discoPing(ctx, cl)
}

func discoPing(ctx context.Context, cl *tailcat.Client) (PingResult, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	r, err := cl.DiscoPing(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return PingResult{}, errors.New("no answer from the homelab")
		}
		return PingResult{}, err
	}
	return PingResult{
		Latency:  time.Duration(r.LatencySeconds * float64(time.Second)),
		Direct:   r.Endpoint != "" || r.PeerRelay != "",
		Endpoint: r.Endpoint,
	}, nil
}
