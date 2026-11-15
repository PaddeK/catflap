package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/tailscale/tailcat"
)

// ForwardOptions are the inputs of `catflap forward <alias> [LOCAL:]PORT`.
type ForwardOptions struct {
	Alias     string
	Port      uint16 // port on the homelab (must be forwarded there)
	LocalPort uint16 // port on this machine; 0 means the same as Port
	Out       io.Writer
}

// Forward listens on localhost and tunnels every connection to a port on
// the homelab, until ctx is done. It only binds to the loopback address:
// anyone who can reach the listener uses this device's access.
func Forward(ctx context.Context, o ForwardOptions) error {
	st, _, err := Load()
	if err != nil {
		return err
	}
	if _, ok := st.Servers[o.Alias]; !ok {
		_, err := newTailcatClient(st, o.Alias) // for its "unknown homelab" error
		return err
	}

	local := o.LocalPort
	if local == 0 {
		local = o.Port
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(local))))
	if err != nil {
		return fmt.Errorf("cannot listen on localhost:%d: %v (pick another local port: catflap forward %s OTHER:%d)", local, err, o.Alias, o.Port)
	}
	defer ln.Close()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	fmt.Fprintf(o.Out, "Forwarding localhost:%d -> %s port %d. Press Ctrl-C to stop.\n", local, o.Alias, o.Port)

	// Check once up front so a missing forward shows up right away instead
	// of as a hanging browser tab.
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		c, err := dialHomelab(cctx, o.Alias, o.Port)
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(o.Out, "Warning: %s port %d does not answer (%v).\n  Is it forwarded on the homelab? There: catflap forward add %d\n", o.Alias, o.Port, err, o.Port)
			}
			return
		}
		c.Close()
		fmt.Fprintf(o.Out, "Tunnel ok: open http://localhost:%d (if it is a web app)\n", local)
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go func() {
			dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			r, err := dialHomelab(dctx, o.Alias, o.Port)
			cancel()
			if err != nil {
				fmt.Fprintf(o.Out, "connection to %s port %d failed: %v\n", o.Alias, o.Port, err)
				c.Close()
				return
			}
			tailcat.ProxyConns(c, r)
		}()
	}
}

// reservedAlias reports whether an alias would clash with the homelab-side
// `catflap forward add|remove|list` subcommands.
func reservedAlias(a string) bool {
	switch a {
	case "add", "remove", "rm", "list", "ls":
		return true
	}
	return false
}
