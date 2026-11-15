// Package homelab implements the homelab role: the embedded tailcat server
// that clients tunnel into, the agent that talks to the relay, and the
// local control socket used by `catflap invite/list/remove`.
package homelab

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/osutil"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// Config holds the homelab settings (no secrets). Written by `setup
// homelab`, safe to edit; read at daemon start. `catflap forward` updates
// the "forwards" key of the file while the daemon runs.
type Config struct {
	Relay   string `json:"relay"`    // relay host[:port]
	Name    string `json:"name"`     // this homelab's name, the default ssh alias on clients
	SSHUser string `json:"ssh_user"` // user clients log in as
	// Forwards maps tunnel ports to targets (HOST:PORT, on the homelab or
	// in its LAN). Only these ports are reachable through the tunnel.
	// Default: {"22": "127.0.0.1:22"}. Managed with `catflap forward`.
	Forwards map[string]string `json:"forwards"`
	// AuthorizedKeys is the authorized_keys file catflap manages (gate 3).
	// Empty means ~/.ssh/authorized_keys; "off" disables SSH key management.
	AuthorizedKeys string `json:"authorized_keys,omitempty"`
	// AllowSSHKeysFromAnywhere drops the from="127.0.0.1,::1" restriction
	// on added SSH keys. By default an enrolled device's SSH key only works
	// through the catflap tunnel, not directly over the LAN.
	AllowSSHKeysFromAnywhere bool `json:"allow_ssh_keys_from_anywhere,omitempty"`
}

// State is managed by catflap (do not edit while the daemon runs).
type State struct {
	ServerKey  key.NodePrivate      `json:"server_key"`
	PSK        tailcat.PresharedKey `json:"psk"`
	AgentID    string               `json:"agent_id"`
	AgentToken string               `json:"agent_token"`
	STUNPort   int                  `json:"stun_port"`
	Devices    []Device             `json:"devices"`
	Invites    []Invite             `json:"invites"`
	NeedsSync  bool                 `json:"needs_sync,omitempty"`
}

// Device is an enrolled client.
type Device struct {
	Name     string    `json:"name"`
	NodeKey  string    `json:"nodekey"`
	SSHKey   string    `json:"ssh_key,omitempty"`
	Hostname string    `json:"hostname,omitempty"` // as reported by the device
	Added    time.Time `json:"added"`
}

// Invite is an open invite. The secret stays on the homelab.
type Invite struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Secret  []byte    `json:"secret"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	// Used is set once a device redeemed the invite but the relay hasn't
	// confirmed the result yet; the result is re-sent until it has.
	Used *UsedInvite `json:"used,omitempty"`
	// TelegramMessage is the invite bot's message showing the code, edited
	// once the invite is used and deleted when it expires or is revoked.
	TelegramMessage int64 `json:"telegram_message,omitempty"`
	// Replace lets the invite re-enroll an existing device of the same name
	// (`invite --replace`, `homelab move`): its old key and SSH key go.
	Replace bool `json:"replace,omitempty"`
}

// UsedInvite remembers an approval until the relay has it.
type UsedInvite struct {
	SubmissionID string `json:"submission_id"`
	NodeKey      string `json:"nodekey"`
	Sealed       string `json:"sealed"`
}

// Paths are the homelab's files.
type Paths struct {
	Config, State, Socket string
	Notify                string // notification channels incl. tokens (600)
	Log                   string // log file of `homelab run --service` (macOS, Windows)
}

// DefaultPaths returns the file locations. CATFLAP_DIR puts everything in
// one directory (used by tests and for running several instances).
func DefaultPaths() (Paths, error) {
	if d := os.Getenv("CATFLAP_DIR"); d != "" {
		return dirPaths(d, d), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	if runtime.GOOS == "windows" { // everything in %LOCALAPPDATA%\catflap
		local, err := os.UserCacheDir()
		if err != nil {
			local = filepath.Join(home, "AppData", "Local")
		}
		d := filepath.Join(local, "catflap")
		return dirPaths(d, d), nil
	}
	cfg := filepath.Join(home, ".config")
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		cfg = x
	}
	st := filepath.Join(home, ".local", "state")
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		st = x
	}
	return dirPaths(filepath.Join(cfg, "catflap"), filepath.Join(st, "catflap")), nil
}

func dirPaths(cfgDir, stateDir string) Paths {
	return Paths{
		Config: filepath.Join(cfgDir, "homelab.json"),
		State:  filepath.Join(stateDir, "homelab-state.json"),
		Socket: filepath.Join(stateDir, "control.sock"),
		Notify: filepath.Join(stateDir, "notify.json"),
		Log:    filepath.Join(stateDir, "homelab.log"),
	}
}

// LoadConfig reads and validates the config.
func LoadConfig(p Paths) (Config, error) {
	var c Config
	if err := fsutil.ReadJSON(p.Config, &c); err != nil {
		return c, fmt.Errorf("reading %s: %w (did you run `catflap setup homelab`?)", p.Config, err)
	}
	if c.Relay == "" {
		return c, errors.New("homelab config: relay is required")
	}
	if c.Name == "" {
		c.Name = "homelab"
	}
	if c.SSHUser == "" {
		c.SSHUser = osutil.SSHUsername()
	}
	if c.Forwards == nil { // missing; an explicit {} means "no forwards"
		c.Forwards = map[string]string{"22": "127.0.0.1:22"}
	}
	for p, t := range c.Forwards {
		if _, err := parsePort(p); err != nil {
			return c, fmt.Errorf("homelab config: bad forward port %q", p)
		}
		if err := validateTarget(t); err != nil {
			return c, fmt.Errorf("homelab config: %v", err)
		}
	}
	return c, nil
}

func (c Config) authorizedKeysPath() (string, bool) {
	switch c.AuthorizedKeys {
	case "off":
		return "", false
	case "":
		p := defaultAuthorizedKeys()
		return p, p != ""
	}
	return c.AuthorizedKeys, true
}

// defaultAuthorizedKeys is ~/.ssh/authorized_keys, except for Windows
// administrators: their keys are in %ProgramData%\ssh\administrators_authorized_keys
// as long as sshd_config says so (it does by default).
func defaultAuthorizedKeys() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	user := filepath.Join(home, ".ssh", "authorized_keys")
	if runtime.GOOS != "windows" || !osutil.IsAdmin() {
		return user
	}
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	conf, err := os.ReadFile(filepath.Join(pd, "ssh", "sshd_config"))
	if err != nil { // no OpenSSH server (yet): its default config uses the admin file
		return filepath.Join(pd, "ssh", "administrators_authorized_keys")
	}
	if p, ok := adminKeysFile(string(conf), pd); ok {
		return p
	}
	return user
}

// adminKeysFile finds the AuthorizedKeysFile of a "Match Group
// administrators" block in a Windows sshd_config.
func adminKeysFile(conf, programData string) (string, bool) {
	in := false
	for _, line := range strings.Split(conf, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch strings.ToLower(f[0]) {
		case "match":
			in = false
			for i := 1; i+1 < len(f); i += 2 {
				if strings.EqualFold(f[i], "group") {
					for _, g := range strings.Split(f[i+1], ",") {
						if strings.EqualFold(g, "administrators") {
							in = true
						}
					}
				}
			}
		case "authorizedkeysfile":
			if in && len(f) > 1 {
				p := strings.ReplaceAll(f[1], "__PROGRAMDATA__", programData)
				return filepath.FromSlash(p), true
			}
		}
	}
	return "", false
}

// isAdminKeysFile reports whether path is Windows' administrators_authorized_keys,
// which must be readable by SYSTEM and Administrators only.
func isAdminKeysFile(path string) bool {
	return runtime.GOOS == "windows" && strings.EqualFold(filepath.Base(path), "administrators_authorized_keys")
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidName reports whether s is a valid device or homelab name.
func ValidName(s string) bool { return nameRE.MatchString(s) }
