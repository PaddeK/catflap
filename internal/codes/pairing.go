package codes

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/nacl/box"
)

// Enrollment requests (tap to approve) have no shared secret. Two things
// replace it:
//
//   - A 6-digit pairing code that the user compares on the device and on the
//     phone. It covers both sides' keys, so a relay that swaps any of them
//     shows a different code. Commit-then-reveal keeps the relay from
//     grinding keys until the codes match: the device commits to a random
//     secret first, the homelab then sends its own random value, and only
//     then the device reveals its secret. A relay in the middle has to
//     commit before it sees the homelab's value, so it matches the code with
//     a chance of one in a million per attempt, and every failure shows.
//   - The homelab seals its reply (address with the PSK) to the device's
//     X25519 key (NaCl sealed box), which the pairing code also covers.

// RandomReveal returns a device's secret for the commitment.
func RandomReveal() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}

// RequestCommit commits to reveal.
func RequestCommit(reveal []byte) string {
	h := sha256.Sum256(append([]byte("catflap-request-commit-v1\n"), reveal...))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// CheckCommit reports whether reveal matches commit.
func CheckCommit(commit string, reveal []byte) bool {
	return HashEqual(commit, RequestCommit(reveal))
}

// PairingFields are what the pairing code covers.
type PairingFields struct {
	HomelabKey, Homelab, Name, NodeKey, BoxKey, SSHKey, Hostname string
}

// PairingCode returns the 6-digit code for a request.
func PairingCode(f PairingFields, reveal, nonce []byte) string {
	h := sha256.New()
	for _, s := range []string{"catflap-pairing-v1", f.HomelabKey, f.Homelab, f.Name, f.NodeKey, f.BoxKey, f.SSHKey, f.Hostname} {
		fmt.Fprintf(h, "%d:%s\n", len(s), s) // length-prefixed: no ambiguity between fields
	}
	h.Write(reveal)
	h.Write(nonce)
	n := binary.BigEndian.Uint64(h.Sum(nil)[:8])
	return fmt.Sprintf("%06d", n%1_000_000)
}

// FormatPairingCode shows a code as "482 913".
func FormatPairingCode(c string) string {
	if len(c) == 6 {
		return c[:3] + " " + c[3:]
	}
	return c
}

// NormalizePairingCode accepts "482913", "482 913" or "482-913".
func NormalizePairingCode(s string) string {
	return strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(s))
}

// NewBoxKey returns an X25519 key pair for sealed replies.
func NewBoxKey() (pub, priv *[32]byte, err error) {
	return box.GenerateKey(rand.Reader)
}

// EncodeBoxKey and DecodeBoxKey convert a public key for the API.
func EncodeBoxKey(k *[32]byte) string { return base64.RawURLEncoding.EncodeToString(k[:]) }

// DecodeBoxKey parses a public key from the API.
func DecodeBoxKey(s string) (*[32]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("bad box key")
	}
	var k [32]byte
	copy(k[:], b)
	return &k, nil
}

// SealTo encrypts msg so only the holder of pub's private key can read it.
func SealTo(pub *[32]byte, msg []byte) (string, error) {
	out, err := box.SealAnonymous(nil, msg, pub, rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// OpenSealed decrypts a SealTo message.
func OpenSealed(pub, priv *[32]byte, sealed string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	msg, ok := box.OpenAnonymous(nil, b, pub, priv)
	if !ok {
		return nil, errors.New("the reply could not be decrypted")
	}
	return msg, nil
}

// EncodeBytes and DecodeBytes carry nonces and reveals in the API.
func EncodeBytes(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// DecodeBytes parses EncodeBytes; it wants 32 bytes.
func DecodeBytes(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("bad value")
	}
	return b, nil
}
