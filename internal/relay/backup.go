package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/PaddeK/catflap/internal/fsutil"
)

// Backup is everything needed to run the relay on another server under the
// same hostname: its config, its state (DERP key, registered homelabs and
// their devices, all secrets hashed except the DERP key) and, in manual
// cert mode, the certificate. Let's Encrypt certificates aren't included;
// the new server gets its own.
type Backup struct {
	Format  int             `json:"catflap_relay_backup"`
	Created time.Time       `json:"created"`
	Config  Config          `json:"config"`
	State   json.RawMessage `json:"state"`
	CertPEM string          `json:"cert_pem,omitempty"`
	KeyPEM  string          `json:"key_pem,omitempty"`
}

const backupFormat = 1

// StatePath is the relay's state file.
func (c Config) StatePath() string { return c.statePath() }

// WriteBackup writes a backup of the relay described by cfg to path (mode
// 600; it holds the relay's private key). It refuses to overwrite a file.
func WriteBackup(cfg Config, path string) (*Backup, error) {
	st, err := os.ReadFile(cfg.statePath())
	if err != nil {
		return nil, fmt.Errorf("reading the relay state: %w", err)
	}
	if !json.Valid(st) {
		return nil, fmt.Errorf("%s is not valid JSON", cfg.statePath())
	}
	b := &Backup{Format: backupFormat, Created: time.Now().UTC().Truncate(time.Second), Config: cfg, State: st}
	if cfg.CertMode == "manual" {
		cert, err := os.ReadFile(cfg.CertFile)
		if err != nil {
			return nil, err
		}
		key, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		b.CertPEM, b.KeyPEM = string(cert), string(key)
	}
	if fsutil.Exists(path) {
		return nil, fmt.Errorf("%s exists already; pick another file name", path)
	}
	return b, fsutil.WriteJSON(path, b, 0o600)
}

// ReadBackup reads and checks a backup file.
func ReadBackup(path string) (*Backup, error) {
	var b Backup
	if err := fsutil.ReadJSON(path, &b); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if b.Format != backupFormat {
		return nil, errors.New("not a catflap relay backup (or from an unknown version)")
	}
	if err := b.Config.Validate(); err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(b.State, &st); err != nil || st.DERPKey.IsZero() {
		return nil, errors.New("the backup has no usable relay state")
	}
	if b.Config.CertMode == "manual" && (b.CertPEM == "" || b.KeyPEM == "") {
		return nil, errors.New("the backup uses its own certificate but doesn't contain it")
	}
	return &b, nil
}

// Homelabs lists the names of the homelabs registered in the backup.
func (b *Backup) Homelabs() []string {
	var st state
	json.Unmarshal(b.State, &st)
	var out []string
	for _, a := range st.Agents {
		out = append(out, a.Name)
	}
	return out
}
