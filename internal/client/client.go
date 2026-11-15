// Package client implements the client role: `catflap enroll` and
// `catflap proxy` (the ssh ProxyCommand that tunnels to the homelab).
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/api"
	"github.com/PaddeK/catflap/internal/codes"
	"github.com/PaddeK/catflap/internal/fsutil"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// State is the client's file: its identity and the homelabs it can reach.
type State struct {
	// Key is the device key of enrollments made before v0.3; newer ones
	// have their own key per homelab (Server.Key).
	Key     key.NodePrivate    `json:"key,omitzero"`
	Servers map[string]*Server `json:"servers"`
}

// Server is one enrolled homelab, stored under its ssh alias.
type Server struct {
	Addr string `json:"addr"` // tailcat address (secret: contains the PSK)
	// Key is this device's key for this homelab. One key per enrollment:
	// the relay allows one connection per key, so homelabs sharing a relay
	// need different keys to be used at the same time.
	Key          key.NodePrivate `json:"key,omitzero"`
	Relay        string          `json:"relay"`
	Homelab      string          `json:"homelab"`
	DeviceName   string          `json:"device_name"`
	SSHUser      string          `json:"ssh_user"`
	IdentityFile string          `json:"identity_file,omitempty"`
	Enrolled     time.Time       `json:"enrolled"`
}

// KeyFor returns the key to use for s (its own, or the old shared one).
func (st *State) KeyFor(s *Server) key.NodePrivate {
	if !s.Key.IsZero() {
		return s.Key
	}
	return st.Key
}

// StatePath returns the client state file location.
func StatePath() (string, error) {
	if d := os.Getenv("CATFLAP_DIR"); d != "" {
		return filepath.Join(d, "client.json"), nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "catflap", "client.json"), nil
}

// Load reads the client state; a missing file gives an empty state.
func Load() (*State, string, error) {
	path, err := StatePath()
	if err != nil {
		return nil, "", err
	}
	st := &State{}
	if err := fsutil.ReadJSON(path, st); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, path, fmt.Errorf("reading %s: %w", path, err)
	}
	if st.Servers == nil {
		st.Servers = map[string]*Server{}
	}
	return st, path, nil
}

// Save writes the client state (mode 600: it holds the private key).
func Save(path string, st *State) error {
	return fsutil.WriteJSON(path, st, 0o600)
}

// Aliases returns the enrolled aliases, sorted.
func (st *State) Aliases() []string {
	var out []string
	for a := range st.Servers {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// EnrollOptions are the inputs of `catflap enroll`.
type EnrollOptions struct {
	Invite      string
	Alias       string // ssh alias; default: the homelab's name
	SSHKeyFile  string // public key to authorize; default: id_ed25519.pub or a new key
	NoSSHKey    bool   // don't send an SSH key at all
	NoSSHConfig bool   // don't touch ~/.ssh/config
	// Request (HOMELAB) asks for approval instead of using an invite;
	// Name is the wanted device name (default: from the hostname).
	Request string
	Name    string
	Out     io.Writer
}

// Enroll redeems an invite.
func Enroll(ctx context.Context, o EnrollOptions) error {
	if o.Request != "" {
		return enrollByRequest(ctx, o)
	}
	out := o.Out
	code, err := codes.Parse(o.Invite, codes.KindInvite)
	if err != nil {
		return err
	}
	if reservedAlias(o.Alias) {
		return fmt.Errorf("--alias %q is reserved (it is a `catflap forward` subcommand); pick another name", o.Alias)
	}
	keys, err := codes.DeriveKeys(code.Secret)
	if err != nil {
		return err
	}
	st, path, err := Load()
	if err != nil {
		return err
	}
	devKey := key.NewNode() // this enrollment's own key

	sshKey, identity := "", ""
	if !o.NoSSHKey {
		if sshKey, identity, err = pickSSHKey(o.SSHKeyFile, out); err != nil {
			return err
		}
	}
	host, _ := os.Hostname()
	if len(host) > 64 {
		host = host[:64]
	}
	f := codes.EnrollFields{InviteID: code.ID, NodeKey: devKey.Public().String(), SSHKey: sshKey, Hostname: host, Nonce: codes.RandomNonce()}
	req := api.EnrollRequest{InviteID: f.InviteID, NodeKey: f.NodeKey, SSHKey: f.SSHKey, Hostname: f.Hostname, Nonce: f.Nonce, MAC: codes.EnrollMAC(keys, f)}

	rc := api.NewClient(code.Relay, "")
	var er api.EnrollResponse
	if err := rc.Do(ctx, "POST", "/v1/enroll", req, &er); err != nil {
		return fmt.Errorf("sending enrollment to %s: %w", code.Relay, err)
	}
	fmt.Fprintf(out, "Request sent to %s, waiting for the homelab to confirm...\n", code.Relay)

	var status api.EnrollStatus
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := rc.Do(ctx, "GET", "/v1/enroll/"+code.ID+"/"+er.SubmissionID, nil, &status); err != nil {
			return err
		}
		if status.Status != api.StatusPending {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("no answer from the homelab within 2 minutes; is `catflap homelab` running? (the invite stays valid until it expires, just run enroll again)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if status.Status != api.StatusApproved {
		return fmt.Errorf("enrollment rejected: %s", status.Reason)
	}
	pt, err := codes.Open(keys, codes.SealAAD(code.ID, er.SubmissionID, f.NodeKey), status.Sealed)
	if err != nil {
		return err
	}
	var reply api.SealedReply
	if err := json.Unmarshal(pt, &reply); err != nil {
		return fmt.Errorf("bad reply from homelab: %w", err)
	}
	return finishEnroll(ctx, o, st, path, devKey, code.Relay, identity, reply)
}

// pickSSHKey returns the public key line to authorize and the identity file
// to put in the ssh config.
func pickSSHKey(file string, out io.Writer) (pub, identity string, err error) {
	home, _ := os.UserHomeDir()
	if file == "" {
		def := filepath.Join(home, ".ssh", "id_ed25519.pub")
		own := filepath.Join(home, ".ssh", "catflap_ed25519.pub")
		switch {
		case fsutil.Exists(def):
			file = def
		case fsutil.Exists(own):
			file = own
		default:
			if _, err := exec.LookPath("ssh-keygen"); err != nil {
				return "", "", errors.New("no SSH key found and ssh-keygen is not installed; pass --ssh-key FILE.pub or --no-ssh-key")
			}
			fmt.Fprintf(out, "No SSH key found, creating %s (choose a passphrase):\n", strings.TrimSuffix(own, ".pub"))
			if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
				return "", "", err
			}
			host, _ := os.Hostname()
			cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-f", strings.TrimSuffix(own, ".pub"), "-C", "catflap@"+host)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return "", "", fmt.Errorf("ssh-keygen: %w", err)
			}
			file = own
		}
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", "", err
	}
	if strings.Contains(string(b), "PRIVATE KEY") {
		return "", "", fmt.Errorf("%s is a private key; pass the .pub file", file)
	}
	pk, comment, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		return "", "", fmt.Errorf("%s: not an SSH public key: %v", file, err)
	}
	_ = comment
	pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	identity = file
	if priv := strings.TrimSuffix(file, ".pub"); priv != file && fsutil.Exists(priv) {
		identity = priv
	}
	fmt.Fprintf(out, "Using SSH key %s (%s)\n", file, ssh.FingerprintSHA256(pk))
	return pub, identity, nil
}

func newTailcatClient(st *State, alias string) (*tailcat.Client, error) {
	srv, ok := st.Servers[alias]
	if !ok {
		if len(st.Servers) == 0 {
			return nil, errors.New("this device is not enrolled anywhere; run `catflap enroll <invite>`")
		}
		return nil, fmt.Errorf("unknown homelab %q (known: %s)", alias, strings.Join(st.Aliases(), ", "))
	}
	tlog := logger.Discard
	if os.Getenv("CATFLAP_DEBUG") != "" {
		tlog = logger.WithPrefix(log.Printf, "tailcat: ")
	}
	return &tailcat.Client{Server: tailcat.Addr(srv.Addr), Key: st.KeyFor(srv), Logf: tlog}, nil
}

// Probe dials a port through the tunnel and returns the first line the
// server sends (the SSH banner for port 22).
func Probe(ctx context.Context, alias string, port uint16) (string, error) {
	c, err := dialHomelab(ctx, alias, port)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetReadDeadline(dl)
	}
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if n == 0 && err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.SplitN(string(buf[:n]), "\n", 2)[0]), nil
}

// Proxy connects stdin/stdout to a port on the homelab. Used as ssh
// ProxyCommand.
func Proxy(ctx context.Context, alias string, port uint16) error {
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c, err := dialHomelab(dctx, alias, port)
	if err != nil {
		return fmt.Errorf("cannot reach %s port %d: %w", alias, port, err)
	}
	defer c.Close()
	// stdin -> homelab. At EOF, half-close so the server sees the end of
	// input but can still answer.
	go func() {
		io.Copy(c, os.Stdin)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			c.Close()
		}
	}()
	// homelab -> stdout. The session ends when the server closes.
	done := make(chan struct{})
	go func() {
		io.Copy(os.Stdout, c)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// finishEnroll stores an approved enrollment (invite or request), writes
// the ssh config and checks the tunnel.
func finishEnroll(ctx context.Context, o EnrollOptions, st *State, path string, devKey key.NodePrivate, relay, identity string, reply api.SealedReply) error {
	out := o.Out
	if _, err := tailcat.ParseAddr(tailcat.Addr(reply.Addr)); err != nil {
		return fmt.Errorf("homelab sent an invalid address: %w", err)
	}
	alias := o.Alias
	if alias == "" {
		alias = reply.Homelab
	}
	if alias == "" || reservedAlias(alias) {
		alias = "homelab"
	}
	if _, exists := st.Servers[alias]; exists {
		fmt.Fprintf(out, "Note: replacing the existing entry %q.\n", alias)
	}
	st.Servers[alias] = &Server{
		Addr: reply.Addr, Key: devKey, Relay: relay, Homelab: reply.Homelab, DeviceName: reply.Name,
		SSHUser: reply.SSHUser, IdentityFile: identity, Enrolled: time.Now(),
	}
	if err := fsutil.WriteJSON(path, st, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "Approved: this device is %q on homelab %q.\n", reply.Name, reply.Homelab)

	if !o.NoSSHConfig {
		cfgPath, warn, err := WriteSSHConfig(alias, st.Servers[alias])
		if err != nil {
			fmt.Fprintf(out, "Could not update the ssh config: %v\n", err)
		} else {
			fmt.Fprintf(out, "Added Host %q to %s\n", alias, cfgPath)
			if warn != "" {
				fmt.Fprintln(out, "Warning:", warn)
			}
		}
	}

	fmt.Fprint(out, "Checking the tunnel... ")
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	banner, err := Probe(cctx, alias, 22)
	if err != nil {
		fmt.Fprintf(out, "not reachable yet (%v)\n", err)
	} else {
		fmt.Fprintf(out, "ok (%s)\n", banner)
	}
	if !o.NoSSHConfig {
		fmt.Fprintf(out, "\nConnect with:  ssh %s\n", alias)
	} else {
		fmt.Fprintf(out, "\nConnect with:  ssh -o ProxyCommand='catflap proxy %s' %s@%s\n", alias, reply.SSHUser, alias)
	}
	return nil
}
