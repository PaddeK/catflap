// Package osutil holds the few things that differ between operating systems
// outside the service managers: user names, admin rights, file ACLs.
package osutil

import (
	"os"
	"os/user"
	"runtime"
	"strings"
)

// SSHUsername is the name to log in as with ssh: the current user's name,
// on Windows without the computer name in front (DOMAIN\user becomes
// user@domain for domain accounts).
func SSHUsername() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return sshUsername(runtime.GOOS, u.Username, os.Getenv("COMPUTERNAME"))
}

func sshUsername(goos, name, computer string) string {
	if goos != "windows" {
		return name
	}
	dom, n, ok := strings.Cut(name, `\`)
	if !ok {
		return name
	}
	if computer == "" || strings.EqualFold(dom, computer) {
		return n
	}
	return n + "@" + dom
}
