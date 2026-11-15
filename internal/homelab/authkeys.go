package homelab

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/PaddeK/catflap/internal/fsutil"
	"github.com/PaddeK/catflap/internal/osutil"

	"golang.org/x/crypto/ssh"
)

// Lines catflap adds to authorized_keys end with this tag plus the device
// name, so they can be found and removed again.
const keyTag = "catflap:"

// normalizeSSHKey parses a public key line and returns "type base64".
// Options and comments are dropped: catflap sets its own.
func normalizeSSHKey(line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return "", errors.New("SSH key must be a single line")
	}
	pk, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "", fmt.Errorf("invalid SSH public key: %v", err)
	}
	if len(opts) > 0 {
		return "", errors.New("SSH key must not carry options")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))), nil
}

// addAuthorizedKey appends the key for a device unless the same key is
// already present.
func addAuthorizedKey(path, sshKey, name string, localOnly bool) error {
	norm, err := normalizeSSHKey(sshKey)
	if err != nil {
		return err
	}
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if strings.Contains(l, norm) {
			return nil // already authorized (by catflap or by hand)
		}
	}
	entry := norm + " " + keyTag + name
	if localOnly {
		entry = `from="127.0.0.1,::1" ` + entry
	}
	lines = append(lines, entry)
	return writeLines(path, lines)
}

// hasAuthorizedKey reports whether the key appears in the file.
func hasAuthorizedKey(path, sshKey string) bool {
	norm, err := normalizeSSHKey(sshKey)
	if err != nil {
		return false
	}
	lines, err := readLines(path)
	if err != nil {
		return false
	}
	for _, l := range lines {
		if strings.Contains(l, norm) {
			return true
		}
	}
	return false
}

// removeAuthorizedKey removes the lines catflap added for a device and
// reports whether there were any. Keys added by hand are left alone.
func removeAuthorizedKey(path, name string) (bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return false, err
	}
	out := lines[:0]
	removed := false
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) > 0 && f[len(f)-1] == keyTag+name {
			removed = true
			continue
		}
		out = append(out, l)
	}
	if !removed {
		return false, nil
	}
	return true, writeLines(path, out)
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil, nil
	}
	return strings.Split(s, "\n"), nil
}

func writeLines(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data := strings.Join(lines, "\n")
	if data != "" {
		data += "\n"
	}
	if err := fsutil.WriteFile(path, []byte(data), 0o600); err != nil {
		return err
	}
	if isAdminKeysFile(path) { // sshd ignores the file unless only admins can access it
		return osutil.RestrictToAdmins(path)
	}
	return nil
}
