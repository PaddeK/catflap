package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/PaddeK/catflap/internal/api"

	"golang.org/x/crypto/acme/autocert"
	"tailscale.com/derp/derpserver"
	"tailscale.com/net/ktimeout"
	"tailscale.com/net/stunserver"
)

// Run serves the relay until ctx is done.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	logf := log.Printf
	st, err := openStore(cfg.statePath())
	if err != nil {
		return err
	}
	apiSrv := newAPIServer(cfg, st, logf)
	if err := serveAdmin(ctx, cfg, st); err != nil {
		return err
	}

	// Admission controller. derpserver only supports an HTTP hook, so we
	// serve it on a loopback port inside this process and fail closed.
	admitLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer admitLn.Close()
	go (&http.Server{Handler: apiSrv.admitHandler(), ReadHeaderTimeout: 5 * time.Second}).Serve(admitLn)

	d := derpserver.New(st.st.DERPKey, logf)
	defer d.Close()
	d.SetVerifyClientURL("http://" + admitLn.Addr().String() + "/admit")
	d.SetVerifyClientURLFailOpen(false)

	host, _, _ := net.SplitHostPort(cfg.Listen)
	if cfg.STUNPort > 0 {
		ss := stunserver.New(ctx)
		addr := net.JoinHostPort(host, fmt.Sprint(cfg.STUNPort))
		if err := ss.Listen(addr); err != nil {
			return fmt.Errorf("stun: %w", err)
		}
		go ss.Serve()
		logf("relay: STUN on udp %s", addr)
	}

	mux := http.NewServeMux()
	mux.Handle("/derp", derpserver.AddWebSocketSupport(d, derpserver.Handler(d)))
	mux.HandleFunc("/derp/probe", derpserver.ProbeHandler)
	mux.HandleFunc("/derp/latency-check", derpserver.ProbeHandler)
	mux.HandleFunc("/generate_204", derpserver.ServeNoContent)
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "catflap relay\n")
	})
	apiSrv.register(mux)

	tlsCfg, port80, err := tlsConfig(cfg, mux)
	if err != nil {
		return err
	}
	d.ModifyTLSConfigToAddMetaCert(tlsCfg)
	tlsCfg.MinVersion = tls.VersionTLS12

	// Settings copied from upstream cmd/derper: the read/write timeouts
	// only affect TLS setup and plain HTTP requests, because derpserver
	// clears deadlines when it hijacks a connection.
	quiet := log.New(httpLogFilter{}, "", 0)
	srv := &http.Server{
		Handler:      mux,
		TLSConfig:    tlsCfg,
		ErrorLog:     quiet,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	lc := net.ListenConfig{KeepAlive: 10 * time.Minute, Control: ktimeout.UserTimeout(15 * time.Second)}
	lc.SetMultipathTCP(false)
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	errc := make(chan error, 2)
	if cfg.HTTPPort > 0 {
		srv80 := &http.Server{
			Addr:         net.JoinHostPort(host, fmt.Sprint(cfg.HTTPPort)),
			Handler:      port80,
			ErrorLog:     quiet,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		}
		wg.Go(func() {
			if err := srv80.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("http port: %w", err)
			}
		})
		defer srv80.Close()
		logf("relay: HTTP (ACME + redirect) on %s", srv80.Addr)
	}
	wg.Go(func() {
		if err := srv.Serve(tls.NewListener(ln, tlsCfg)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	})
	logf("relay: serving DERP + API for %s on %s (TLS, %s)", cfg.PublicAddr(), cfg.Listen, cfg.CertMode)
	go gcLoop(ctx, st)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	return err
}

// tlsConfig returns the TLS config for the main listener and the handler
// for the plain-HTTP port.
func tlsConfig(cfg Config, main http.Handler) (*tls.Config, http.Handler, error) {
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/generate_204" {
			derpserver.ServeNoContent(w, r)
			return
		}
		http.Redirect(w, r, "https://"+cfg.PublicAddr()+r.URL.RequestURI(), http.StatusFound)
	})
	switch cfg.CertMode {
	case "letsencrypt":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Hostname),
			Cache:      autocert.DirCache(cfg.certDir()),
			Email:      cfg.Email,
		}
		return m.TLSConfig(), m.HTTPHandler(redirect), nil
	case "manual":
		mc := &manualCert{certFile: cfg.CertFile, keyFile: cfg.KeyFile}
		if _, err := mc.get(nil); err != nil {
			return nil, nil, err
		}
		return &tls.Config{NextProtos: []string{"http/1.1"}, GetCertificate: mc.get}, redirect, nil
	}
	return nil, nil, fmt.Errorf("unknown cert mode %q", cfg.CertMode)
}

// manualCert serves a certificate from files and reloads it when they change.
type manualCert struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
}

func (m *manualCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	fi, err := os.Stat(m.certFile)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cert != nil && fi.ModTime().Equal(m.modTime) {
		return m.cert, nil
	}
	c, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
	if err != nil {
		if m.cert != nil {
			return m.cert, nil // keep serving the old one during a renewal
		}
		return nil, err
	}
	m.cert, m.modTime = &c, fi.ModTime()
	return m.cert, nil
}

func gcLoop(ctx context.Context, st *store) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st.mu.Lock()
			st.gcLocked(time.Now())
			st.saveLocked()
			st.mu.Unlock()
		}
	}
}

// httpLogFilter drops the noisy TLS handshake errors that every internet
// facing server sees (scanners, bots) and passes everything else on.
type httpLogFilter struct{}

func (httpLogFilter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte("TLS handshake error")) {
		log.Printf("http: %s", bytes.TrimSpace(p))
	}
	return len(p), nil
}

// serveAdmin serves the local admin API on a unix socket. Only root and the
// relay's service user can reach it (directory permissions).
func serveAdmin(ctx context.Context, cfg Config, st *store) error {
	path := cfg.adminSocket()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	os.Chmod(path, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /join-code", func(w http.ResponseWriter, r *http.Request) {
		ttl, err := time.ParseDuration(r.URL.Query().Get("ttl"))
		if err != nil || ttl <= 0 {
			ttl = defaultJoinCodeTTL
		}
		ttl = min(ttl, 7*24*time.Hour)
		c, err := st.newJoinCode(cfg.PublicAddr(), ttl)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"code": c.String(), "expires": time.Now().Add(ttl).Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		type agentInfo struct {
			ID, Name, NodeKey string
			Created           time.Time
			Clients           map[string]string
		}
		var out struct {
			Relay       string
			Agents      []agentInfo
			OpenInvites int
			JoinCodes   int
		}
		out.Relay = cfg.PublicAddr()
		for id, a := range st.st.Agents {
			out.Agents = append(out.Agents, agentInfo{id, a.Name, a.NodeKey.String(), a.Created, a.Clients})
		}
		for _, inv := range st.st.Invites {
			if !inv.Closed {
				out.OpenInvites++
			}
		}
		out.JoinCodes = len(st.st.JoinCodes)
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if _, ok := st.st.Agents[r.PathValue("id")]; !ok {
			writeErr(w, http.StatusNotFound, "no such homelab")
			return
		}
		delete(st.st.Agents, r.PathValue("id"))
		st.gcLocked(time.Now())
		if err := st.saveLocked(); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go srv.Serve(ln)
	return nil
}

// AdminClient returns an HTTP client that talks to the relay's admin socket.
func AdminClient(cfg Config) *api.Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", cfg.adminSocket())
	}}
	c := api.NewClient("relay-admin", "")
	c.Base = "http://relay-admin"
	c.HTTP = &http.Client{Transport: tr, Timeout: 10 * time.Second}
	return c
}
