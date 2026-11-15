package client

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/PaddeK/catflap/internal/fsutil"
)

func blockMarkers(alias string) (begin, end string) {
	return "# >>> catflap " + alias + " (managed by catflap, do not edit)", "# <<< catflap " + alias
}

// sshConfigPath is ~/.ssh/config (CATFLAP_SSH_CONFIG overrides, for tests).
func sshConfigPath() (string, error) {
	if p := os.Getenv("CATFLAP_SSH_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// WriteSSHConfig adds or replaces the managed Host block for alias. The
// block is appended at the end so it never captures global options at the
// top of the file. It returns a warning if another Host line for the
// same alias exists (ssh uses the first match).
func WriteSSHConfig(alias string, srv *Server) (path, warning string, err error) {
	path, err = sshConfigPath()
	if err != nil {
		return "", "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if strings.Contains(exe, os.TempDir()) || strings.Contains(exe, "go-build") {
		warning = fmt.Sprintf("catflap runs from a temporary location (%s); install it permanently and run enroll again, or the ssh config will break", exe)
	}

	begin, end := blockMarkers(alias)
	var b strings.Builder
	fmt.Fprintln(&b, begin)
	fmt.Fprintf(&b, "Host %s\n", alias)
	if srv.SSHUser != "" {
		fmt.Fprintf(&b, "  User %s\n", srv.SSHUser)
	}
	fmt.Fprintf(&b, "  HostKeyAlias catflap-%s\n", alias)
	// '%' is a token character in ssh_config; escape it in the path.
	fmt.Fprintf(&b, "  ProxyCommand \"%s\" proxy %s\n", strings.ReplaceAll(exe, "%", "%%"), alias)
	if srv.IdentityFile != "" {
		fmt.Fprintf(&b, "  IdentityFile \"%s\"\n", strings.ReplaceAll(srv.IdentityFile, "%", "%%"))
		fmt.Fprintln(&b, "  IdentitiesOnly yes")
	}
	fmt.Fprintln(&b, "  ServerAliveInterval 30")
	fmt.Fprintln(&b, end)

	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return path, "", err
	}
	content := removeBlock(string(old), begin, end)
	for _, l := range strings.Split(content, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && strings.EqualFold(f[0], "Host") {
			for _, h := range f[1:] {
				if h == alias {
					warning = fmt.Sprintf("%s already has a \"Host %s\" entry; ssh uses the first match, so remove that entry or enroll with --alias", path, alias)
				}
			}
		}
	}
	content = strings.TrimRight(content, "\n")
	if content != "" {
		content += "\n\n"
	}
	content += b.String()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, warning, err
	}
	return path, warning, fsutil.WriteFile(path, []byte(content), 0o600)
}

// RemoveSSHConfig removes the managed block for alias.
func RemoveSSHConfig(alias string) error {
	path, err := sshConfigPath()
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	begin, end := blockMarkers(alias)
	nw := removeBlock(string(old), begin, end)
	if nw == string(old) {
		return nil
	}
	return fsutil.WriteFile(path, []byte(strings.TrimRight(nw, "\n")+"\n"), 0o600)
}

func removeBlock(s, begin, end string) string {
	lines := strings.Split(s, "\n")
	var out []string
	in := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case t == begin:
			in = true
		case in && t == end:
			in = false
		case !in:
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
