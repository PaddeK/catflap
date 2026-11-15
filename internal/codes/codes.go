// Package codes implements catflap's one-time codes (invites, join codes),
// the key derivation from their secrets, and the enrollment MAC and sealed
// replies built on those keys.
//
// A code looks like
//
//	cfi1-<id>-<secret>@relay.example.com[:port]
//
// The id is public (the relay sees it). The secret never leaves the two
// endpoints: whoever created the code and whoever redeems it.
package codes

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Code kinds.
const (
	KindInvite = "cfi1" // homelab -> client: enroll one device
	KindJoin   = "cfj1" // relay -> homelab: register a homelab with the relay
)

const (
	idBytes     = 8  // 13 base32 chars
	secretBytes = 16 // 128 bits, 26 base32 chars
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Code is a parsed invite or join code.
type Code struct {
	Kind   string
	ID     string
	Secret []byte
	Relay  string // host or host:port of the relay
}

// New creates a code of the given kind with a fresh id and secret.
func New(kind, relay string) (Code, error) {
	if err := ValidateRelay(relay); err != nil {
		return Code{}, err
	}
	sec := make([]byte, secretBytes)
	rand.Read(sec)
	return Code{Kind: kind, ID: RandomID(), Secret: sec, Relay: relay}, nil
}

// String formats the code for humans and copy/paste.
func (c Code) String() string {
	return fmt.Sprintf("%s-%s-%s@%s", c.Kind, c.ID, strings.ToLower(b32.EncodeToString(c.Secret)), c.Relay)
}

// Parse parses a code and checks that it has the wanted kind.
func Parse(s, wantKind string) (Code, error) {
	s = strings.TrimSpace(s)
	body, relay, ok := strings.Cut(s, "@")
	if !ok {
		return Code{}, errors.New("malformed code: missing @relay")
	}
	parts := strings.Split(body, "-")
	if len(parts) != 3 {
		return Code{}, errors.New("malformed code")
	}
	kind := strings.ToLower(parts[0])
	if kind != wantKind {
		switch kind {
		case KindInvite:
			return Code{}, errors.New("this is an invite code (for `catflap enroll`), not a join code")
		case KindJoin:
			return Code{}, errors.New("this is a join code (for `catflap setup homelab`), not an invite")
		}
		return Code{}, fmt.Errorf("unknown code type %q", parts[0])
	}
	id := strings.ToLower(parts[1])
	if raw, err := b32.DecodeString(strings.ToUpper(id)); err != nil || len(raw) != idBytes {
		return Code{}, errors.New("malformed code: bad id")
	}
	sec, err := b32.DecodeString(strings.ToUpper(parts[2]))
	if err != nil || len(sec) != secretBytes {
		return Code{}, errors.New("malformed code: bad secret")
	}
	if err := ValidateRelay(relay); err != nil {
		return Code{}, err
	}
	return Code{Kind: kind, ID: id, Secret: sec, Relay: strings.ToLower(relay)}, nil
}

// ValidateRelay checks that s is a bare host or host:port.
func ValidateRelay(s string) error {
	if s == "" || strings.ContainsAny(s, "/@ \t") || strings.Contains(s, "://") {
		return fmt.Errorf("invalid relay address %q (want host or host:port)", s)
	}
	if strings.Contains(s, ":") {
		_, port, err := net.SplitHostPort(s)
		if err != nil {
			return fmt.Errorf("invalid relay address %q: %v", s, err)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid relay port in %q", s)
		}
	}
	return nil
}

// ValidID reports whether s looks like an id produced by RandomID.
func ValidID(s string) bool {
	raw, err := b32.DecodeString(strings.ToUpper(s))
	return err == nil && len(raw) == idBytes && s == strings.ToLower(s)
}

// RandomID returns a random public identifier.
func RandomID() string {
	b := make([]byte, idBytes)
	rand.Read(b)
	return strings.ToLower(b32.EncodeToString(b))
}

// RandomToken returns a random 256-bit bearer token with a prefix.
func RandomToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + strings.ToLower(b32.EncodeToString(b))
}

// Hash returns the hex SHA-256 of b. Used to store secrets and tokens.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// HashEqual compares two hex hashes in constant time.
func HashEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// Keys are the per-code keys derived from a code's secret.
type Keys struct {
	MAC  []byte
	Seal []byte
}

// DeriveKeys derives the MAC and seal keys from a code secret.
func DeriveKeys(secret []byte) (Keys, error) {
	m, err := hkdf.Key(sha256.New, secret, []byte("catflap-invite-v1"), "mac", 32)
	if err != nil {
		return Keys{}, err
	}
	s, err := hkdf.Key(sha256.New, secret, []byte("catflap-invite-v1"), "seal", 32)
	if err != nil {
		return Keys{}, err
	}
	return Keys{MAC: m, Seal: s}, nil
}

// EnrollFields are the values an enrollment MAC covers.
type EnrollFields struct {
	InviteID, NodeKey, SSHKey, Hostname, Nonce string
}

func (f EnrollFields) message() []byte {
	return []byte(strings.Join([]string{"catflap-enroll-v1", f.InviteID, f.NodeKey, f.SSHKey, f.Hostname, f.Nonce}, "\n"))
}

// EnrollMAC returns the MAC over f.
func EnrollMAC(k Keys, f EnrollFields) string {
	m := hmac.New(sha256.New, k.MAC)
	m.Write(f.message())
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// VerifyEnrollMAC checks mac against f in constant time.
func VerifyEnrollMAC(k Keys, f EnrollFields, mac string) bool {
	got, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, k.MAC)
	m.Write(f.message())
	return hmac.Equal(got, m.Sum(nil))
}

// SealAAD is the associated data binding a sealed reply to one submission.
func SealAAD(inviteID, submissionID, nodeKey string) []byte {
	return []byte("catflap-seal-v1\n" + inviteID + "\n" + submissionID + "\n" + nodeKey)
}

// Seal encrypts and authenticates plaintext with AES-256-GCM.
func Seal(k Keys, aad, plaintext []byte) (string, error) {
	aead, err := newGCM(k.Seal)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	out := aead.Seal(nonce, nonce, plaintext, aad)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// Open reverses Seal. It fails if anything was tampered with.
func Open(k Keys, aad []byte, sealed string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return nil, errors.New("malformed sealed reply")
	}
	aead, err := newGCM(k.Seal)
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("malformed sealed reply")
	}
	pt, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("sealed reply failed authentication (wrong invite or tampered)")
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// RandomNonce returns a random value for EnrollFields.Nonce.
func RandomNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
