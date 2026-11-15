package osutil

import "testing"

func TestSSHUsername(t *testing.T) {
	for _, c := range []struct{ goos, name, computer, want string }{
		{"linux", "padde", "", "padde"},
		{"darwin", "anna", "", "anna"},
		{"windows", `DESKTOP-1\anna`, "DESKTOP-1", "anna"},
		{"windows", `desktop-1\anna`, "DESKTOP-1", "anna"},
		{"windows", `CORP\anna`, "DESKTOP-1", "anna@CORP"},
		{"windows", "anna", "DESKTOP-1", "anna"},
	} {
		if got := sshUsername(c.goos, c.name, c.computer); got != c.want {
			t.Errorf("sshUsername(%q, %q, %q) = %q, want %q", c.goos, c.name, c.computer, got, c.want)
		}
	}
}
