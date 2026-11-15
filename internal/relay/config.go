// Package relay implements `catflap relay run`: an embedded DERP server
// with STUN, Let's Encrypt and catflap's admission + enrollment API, all
// behind one TLS port.
package relay

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"

	"github.com/PaddeK/catflap/internal/codes"
	"github.com/PaddeK/catflap/internal/fsutil"
)

// DefaultConfigPath is where `setup relay` writes the config.
const DefaultConfigPath = "/etc/catflap/relay.json"

// Config holds the relay settings (no secrets).
type Config struct {
	Hostname string `json:"hostname"`             // public DNS name, also the TLS name
	Listen   string `json:"listen"`               // TLS listen address, default ":443"
	HTTPPort int    `json:"http_port"`            // ACME HTTP-01 + redirect; 0 disables
	STUNPort int    `json:"stun_port"`            // UDP STUN; 0 disables
	CertMode string `json:"cert_mode"`            // "letsencrypt" or "manual"
	CertFile string `json:"cert_file,omitempty"`  // manual mode
	KeyFile  string `json:"key_file,omitempty"`   // manual mode
	Email    string `json:"acme_email,omitempty"` // optional Let's Encrypt contact
	StateDir string `json:"state_dir"`            // default /var/lib/catflap
	// AdminSocket is the local admin API (join codes, status). Default
	// /run/catflap/admin.sock.
	AdminSocket string `json:"admin_socket,omitempty"`
}

func (c Config) adminSocket() string {
	if c.AdminSocket != "" {
		return c.AdminSocket
	}
	return "/run/catflap/admin.sock"
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{
		Listen:   ":443",
		HTTPPort: 80,
		STUNPort: 3478,
		CertMode: "letsencrypt",
		StateDir: "/var/lib/catflap",
	}
}

// LoadConfig reads a config file and fills in defaults.
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	if err := fsutil.ReadJSON(path, &c); err != nil {
		return c, fmt.Errorf("reading %s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate checks the config.
func (c Config) Validate() error {
	if c.Hostname == "" {
		return errors.New("relay config: hostname is required")
	}
	if err := codes.ValidateRelay(c.Hostname); err != nil || net.ParseIP(c.Hostname) != nil && c.CertMode == "letsencrypt" {
		return fmt.Errorf("relay config: hostname must be a DNS name, got %q", c.Hostname)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("relay config: bad listen address %q: %v", c.Listen, err)
	}
	switch c.CertMode {
	case "letsencrypt":
	case "manual":
		if c.CertFile == "" || c.KeyFile == "" {
			return errors.New("relay config: cert_mode manual needs cert_file and key_file")
		}
	default:
		return fmt.Errorf("relay config: unknown cert_mode %q", c.CertMode)
	}
	if c.StateDir == "" {
		return errors.New("relay config: state_dir is required")
	}
	return nil
}

// PublicAddr is the relay address clients use: hostname, plus the port if
// it isn't 443.
func (c Config) PublicAddr() string {
	_, port, _ := net.SplitHostPort(c.Listen)
	if port == "443" || port == "" {
		return c.Hostname
	}
	return net.JoinHostPort(c.Hostname, port)
}

func (c Config) statePath() string { return filepath.Join(c.StateDir, "state.json") }
func (c Config) certDir() string   { return filepath.Join(c.StateDir, "certs") }
