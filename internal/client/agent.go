package client

// The client agent keeps ONE tunnel engine per homelab for all of this
// device's connections.
//
// Without it, every `ssh`, `scp` or forwarded connection would start its own
// tailcat engine with the same device key. The relay and the homelab then
// see one device hopping between connections: a new session freezes the
// running ones, and a reconnect right after a session ended stalls for ~10 s
// (the homelab keeps sending to the previous process's UDP port for 6.5 s).
//
// `catflap proxy`, `catflap forward` and the enrollment check therefore talk
// to a background `catflap agent` over a unix socket next to client.json
// (a directory only this user can read). The agent is started on demand
// and exits after agentIdle without connections. If it can't be used, the
// callers fall back to an in-process engine.
//
// Protocol: the caller sends one JSON line (agentRequest), the agent answers
// with one JSON line (agentResponse); after an ok answer to "dial" the
// connection carries the raw TCP stream to the homelab port. "probe" pings
// the homelab through the tunnel (`catflap ping`).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PaddeK/catflap/internal/api"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

const (
	agentIdle        = 10 * time.Minute
	agentStartWait   = 5 * time.Second
	agentDialTimeout = 60 * time.Second
	maxSocketPath    = 100 // unix socket paths are limited to ~104 bytes (macOS)
)

type agentRequest struct {
	Op       string `json:"op"` // "dial", "probe", "ping" or "stop"
	Alias    string `json:"alias,omitempty"`
	Port     uint16 `json:"port,omitempty"`
	Identity string `json:"identity"` // build identity of the caller
}

type agentResponse struct {
	OK      bool        `json:"ok"`
	Error   string      `json:"error,omitempty"`
	Restart bool        `json:"restart,omitempty"` // agent is from another build and retires
	PID     int         `json:"pid,omitempty"`
	Probe   *PingResult `json:"probe,omitempty"`
}

// AgentSocket returns the agent's socket path.
func AgentSocket() (string, error) {
	p, err := StatePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "agent.sock"), nil
}

// buildIdentity identifies the running binary, so a caller never talks to
// an agent left over from an older catflap.
func buildIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return api.Version
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return api.Version + "|" + exe
	}
	return fmt.Sprintf("%s|%s|%d|%d", api.Version, exe, fi.Size(), fi.ModTime().UnixNano())
}

// ---- caller side ----

// dialHomelab connects to a port on the homelab behind alias, through the
// agent if possible.
func dialHomelab(ctx context.Context, alias string, port uint16) (net.Conn, error) {
	if os.Getenv("CATFLAP_NO_AGENT") == "" {
		c, err := dialViaAgent(ctx, alias, port)
		if err == nil {
			return c, nil
		}
		var re *remoteError
		if errors.As(err, &re) {
			return nil, err // the agent is fine; the homelab said no
		}
		if os.Getenv("CATFLAP_DEBUG") != "" {
			log.Printf("agent unavailable, connecting directly: %v", err)
		}
	}
	return dialInProcess(ctx, alias, port)
}

// remoteError is an error reported by a working agent (e.g. port not
// reachable), as opposed to the agent itself being unavailable.
type remoteError struct{ msg string }

func (e *remoteError) Error() string { return e.msg }

func dialViaAgent(ctx context.Context, alias string, port uint16) (net.Conn, error) {
	c, _, err := callAgent(ctx, agentRequest{Op: "dial", Alias: alias, Port: port})
	return c, err
}

// callAgent sends req to the agent, starting one if needed. The returned
// conn is open after an ok answer.
func callAgent(ctx context.Context, req agentRequest) (net.Conn, agentResponse, error) {
	sock, err := AgentSocket()
	if err != nil {
		return nil, agentResponse{}, err
	}
	if len(sock) > maxSocketPath {
		return nil, agentResponse{}, fmt.Errorf("agent socket path too long: %s", sock)
	}
	req.Identity = buildIdentity()

	started := false
	deadline := time.Now().Add(agentStartWait)
	for {
		c, resp, err := agentRoundTrip(ctx, sock, req)
		switch {
		case err == nil && resp.OK:
			return c, resp, nil
		case err == nil && resp.Restart:
			// An agent from another build is retiring; start ours.
			started = false
		case err == nil:
			return nil, resp, &remoteError{resp.Error}
		}
		if ctx.Err() != nil {
			return nil, resp, ctx.Err()
		}
		if !started {
			if err := startAgent(sock); err != nil {
				return nil, resp, err
			}
			started = true
		}
		if time.Now().After(deadline) {
			return nil, resp, fmt.Errorf("agent did not start (see %s): %v", agentLogPath(sock), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// agentRoundTrip connects, sends req and reads the answer. The returned
// conn is only meaningful (and open) if resp.OK.
func agentRoundTrip(ctx context.Context, sock string, req agentRequest) (net.Conn, agentResponse, error) {
	var resp agentResponse
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, resp, err
	}
	if err := writeLine(c, req); err != nil {
		c.Close()
		return nil, resp, err
	}
	// The dial on the agent side can take a while (up to agentDialTimeout).
	stop := context.AfterFunc(ctx, func() { c.SetReadDeadline(time.Now()) })
	err = readLine(c, &resp)
	stop()
	if err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, resp, ctx.Err()
		}
		return nil, resp, err
	}
	if !resp.OK {
		c.Close()
		return nil, resp, nil
	}
	return c, resp, nil
}

func agentLogPath(sock string) string { return filepath.Join(filepath.Dir(sock), "agent.log") }

// startAgent starts `catflap agent` in the background, detached from the
// caller (ssh kills its ProxyCommand when the session ends).
func startAgent(sock string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(agentLogPath(sock), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "agent")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = os.Environ()
	if err := startDetached(cmd); err != nil {
		return fmt.Errorf("starting the agent: %w", err)
	}
	return cmd.Process.Release()
}

// ---- in-process fallback ----

var (
	inprocMu      sync.Mutex
	inprocClients = map[string]*tailcat.Client{}
)

// dialInProcess dials with an engine inside this process, shared by all
// dials of the process (e.g. one `catflap forward`).
func dialInProcess(ctx context.Context, alias string, port uint16) (net.Conn, error) {
	cl, err := inprocClient(alias)
	if err != nil {
		return nil, err
	}
	return cl.DialTCPPort(ctx, port)
}

func inprocClient(alias string) (*tailcat.Client, error) {
	inprocMu.Lock()
	defer inprocMu.Unlock()
	if cl := inprocClients[alias]; cl != nil {
		return cl, nil
	}
	st, _, err := Load()
	if err != nil {
		return nil, err
	}
	cl, err := newTailcatClient(st, alias)
	if err != nil {
		return nil, err
	}
	inprocClients[alias] = cl
	return cl, nil
}

// ---- agent side ----

// RunAgent runs the client agent until it has been idle for agentIdle.
func RunAgent(ctx context.Context) error {
	sock, err := AgentSocket()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	ln, err := listenAgent(sock)
	if err != nil {
		return err
	}
	a := &agent{
		identity: buildIdentity(),
		clients:  map[string]*agentClient{},
		ln:       ln,
		sock:     sock,
		lastUsed: time.Now(),
	}
	log.Printf("agent: listening on %s (pid %d)", sock, os.Getpid())
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.idleLoop(ctx, cancel)
	go func() {
		<-ctx.Done()
		a.retire()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		go a.serve(ctx, c)
	}
	// Listener closed (idle or retired): let open connections finish,
	// unless we were asked to stop.
	for a.active.Load() > 0 && parent.Err() == nil && !a.stopping.Load() {
		time.Sleep(time.Second)
	}
	a.mu.Lock()
	for _, c := range a.clients {
		c.cl.Close()
	}
	a.mu.Unlock()
	log.Printf("agent: exit")
	return nil
}

// StopAgent stops this user's running agent (if any) and waits until it is
// gone, so its files can be deleted. Open sessions through it end.
func StopAgent() error {
	sock, err := AgentSocket()
	if err != nil {
		return err
	}
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return nil // not running
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	writeLine(c, agentRequest{Op: "stop", Identity: buildIdentity()})
	var resp agentResponse
	readLine(c, &resp) // older agents answer "restart" and retire as well
	c.Close()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err != nil {
			return nil
		} else {
			c.Close()
		}
	}
	return errors.New("the client agent did not stop")
}

// listenAgent listens on sock, replacing a stale socket file but never a
// live agent.
func listenAgent(sock string) (net.Listener, error) {
	ln, err := net.Listen("unix", sock)
	if err == nil {
		return ln, nil
	}
	if c, derr := net.DialTimeout("unix", sock, time.Second); derr == nil {
		c.Close()
		return nil, errors.New("another agent is already running")
	}
	os.Remove(sock)
	return net.Listen("unix", sock)
}

type agent struct {
	identity string
	sock     string
	ln       net.Listener
	active   atomic.Int32

	mu       sync.Mutex
	clients  map[string]*agentClient // by alias
	lastUsed time.Time
	retired  bool
	stopping atomic.Bool // `catflap uninstall`: exit without waiting for open connections
}

type agentClient struct {
	cl   *tailcat.Client
	addr string
	key  key.NodePrivate
}

func (a *agent) idleLoop(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			idle := a.active.Load() == 0 && time.Since(a.lastUsed) > agentIdle
			a.mu.Unlock()
			if idle {
				log.Printf("agent: idle, exiting")
				cancel()
				return
			}
		}
	}
}

// retire stops accepting new connections and removes the socket, so a new
// agent can take over; open connections keep running.
func (a *agent) retire() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retired {
		return
	}
	a.retired = true
	a.ln.Close()
	// Remove the socket only if it is still ours (a successor may own it).
	if fi, err := os.Stat(a.sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if c, err := net.DialTimeout("unix", a.sock, 200*time.Millisecond); err != nil {
			os.Remove(a.sock)
		} else {
			c.Close()
		}
	}
}

func (a *agent) serve(ctx context.Context, c net.Conn) {
	a.active.Add(1)
	defer func() {
		a.active.Add(-1)
		a.mu.Lock()
		a.lastUsed = time.Now()
		a.mu.Unlock()
	}()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var req agentRequest
	if err := readLine(c, &req); err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})
	if req.Op == "stop" { // `catflap uninstall`; the socket is private to this user
		log.Printf("agent: asked to stop")
		writeLine(c, agentResponse{OK: true})
		c.Close()
		a.stopping.Store(true)
		a.retire()
		return
	}
	if req.Identity != a.identity {
		log.Printf("agent: caller is another catflap build, retiring")
		writeLine(c, agentResponse{Restart: true})
		c.Close()
		a.retire()
		return
	}
	switch req.Op {
	case "ping":
		writeLine(c, agentResponse{OK: true, PID: os.Getpid()})
		c.Close()
		return
	case "probe":
		res, err := a.probe(ctx, req.Alias)
		if err != nil {
			writeLine(c, agentResponse{Error: err.Error()})
		} else {
			writeLine(c, agentResponse{OK: true, Probe: &res})
		}
		c.Close()
		return
	case "dial":
	default:
		writeLine(c, agentResponse{Error: "unknown op " + req.Op})
		c.Close()
		return
	}

	r, err := a.dial(ctx, req.Alias, req.Port)
	if err != nil {
		writeLine(c, agentResponse{Error: err.Error()})
		c.Close()
		return
	}
	if err := writeLine(c, agentResponse{OK: true}); err != nil {
		c.Close()
		r.Close()
		return
	}
	tailcat.ProxyConns(c, r)
}

// staleAfter is how long a dial on an existing engine may go unanswered
// before the engine is rebuilt. The homelab answers closed ports with a
// reset, so silence means the tunnel itself is stuck (typically: the
// homelab restarted and its new process has no WireGuard session with us;
// WireGuard alone would only retry after ~15 s).
const staleAfter = 6 * time.Second

// dial opens a tunneled connection to port on the homelab behind alias.
func (a *agent) dial(ctx context.Context, alias string, port uint16) (net.Conn, error) {
	cl, fresh, err := a.client(alias)
	if err != nil {
		return nil, err
	}
	first := agentDialTimeout
	if !fresh {
		// tailcat registers with the server ("meow") only on first use; a
		// restarted homelab has forgotten us. Re-registering is one
		// packet and harmless otherwise.
		cl.Ping(ctx)
		first = staleAfter
	}
	dctx, cancel := context.WithTimeout(ctx, first)
	r, err := cl.DialTCPPort(dctx, port)
	cancel()
	if err == nil || fresh || ctx.Err() != nil || !isTimeout(err) {
		return r, err
	}
	log.Printf("agent: %s port %d did not answer, restarting the tunnel", alias, port)
	if cl, err = a.resetClient(alias, cl); err != nil {
		return nil, err
	}
	dctx, cancel = context.WithTimeout(ctx, agentDialTimeout)
	defer cancel()
	return cl.DialTCPPort(dctx, port)
}

// probe pings the homelab behind alias through the agent's engine.
func (a *agent) probe(ctx context.Context, alias string) (PingResult, error) {
	cl, fresh, err := a.client(alias)
	if err != nil {
		return PingResult{}, err
	}
	if !fresh {
		cl.Ping(ctx) // re-register with a restarted homelab, as in dial
	}
	return discoPing(ctx, cl)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

// resetClient replaces the engine for alias, unless another dial already
// replaced old.
func (a *agent) resetClient(alias string, old *tailcat.Client) (*tailcat.Client, error) {
	a.mu.Lock()
	if cur := a.clients[alias]; cur != nil && cur.cl == old {
		old.Close()
		delete(a.clients, alias)
	}
	a.mu.Unlock()
	cl, _, err := a.client(alias)
	return cl, err
}

// client returns the engine for alias, (re)created when the enrollment
// changed since it was made. fresh reports a newly created engine.
func (a *agent) client(alias string) (cl *tailcat.Client, fresh bool, err error) {
	st, _, err := Load()
	if err != nil {
		return nil, false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	srv, ok := st.Servers[alias]
	cur := a.clients[alias]
	if cur != nil && (!ok || cur.addr != srv.Addr || !cur.key.Equal(st.KeyFor(srv))) {
		cur.cl.Close() // re-enrolled or forgotten
		delete(a.clients, alias)
		cur = nil
	}
	if cur == nil {
		cl, err := newTailcatClient(st, alias)
		if err != nil {
			return nil, false, err
		}
		cur = &agentClient{cl: cl, addr: srv.Addr, key: st.KeyFor(srv)}
		a.clients[alias] = cur
		fresh = true
	}
	return cur.cl, fresh, nil
}

// ---- line framing ----

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// readLine reads one JSON line byte by byte, so nothing after the newline
// (the start of the tunneled stream) is consumed.
func readLine(r io.Reader, v any) error {
	var buf []byte
	b := make([]byte, 1)
	for len(buf) < 4096 {
		if _, err := io.ReadFull(r, b); err != nil {
			return err
		}
		if b[0] == '\n' {
			return json.Unmarshal(buf, v)
		}
		buf = append(buf, b[0])
	}
	return errors.New("agent: line too long")
}
