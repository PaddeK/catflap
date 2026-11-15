package client

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Enrollment is one homelab this client is enrolled in.
type Enrollment struct {
	Alias, Homelab, Relay, Device string
}

// Enrollments lists the homelabs of this client (nil if it has none).
func Enrollments() []Enrollment {
	st, _, err := Load()
	if err != nil {
		return nil
	}
	var out []Enrollment
	for alias, s := range st.Servers {
		out = append(out, Enrollment{alias, s.Homelab, s.Relay, s.DeviceName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// IsSetUp reports whether this machine has client state.
func IsSetUp() bool {
	p, err := StatePath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Uninstall stops the agent, removes catflap's ssh config blocks and
// deletes the client's files. It returns the deleted paths.
func Uninstall() ([]string, error) {
	if err := StopAgent(); err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range Enrollments() {
		if err := RemoveSSHConfig(e.Alias); err != nil {
			return removed, err
		}
	}
	p, err := StatePath()
	if err != nil {
		return removed, err
	}
	sock, _ := AgentSocket()
	for _, f := range []string{p, agentLogPath(sock), sock} {
		if err := os.Remove(f); err == nil {
			removed = append(removed, f)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
	}
	if os.Remove(filepath.Dir(p)) == nil { // only if nothing else is in it
		removed = append(removed, filepath.Dir(p))
	}
	return removed, nil
}
