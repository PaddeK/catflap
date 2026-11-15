//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// startDetached starts cmd in its own session, so it survives the caller
// (and the terminal or ssh process that started the caller).
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
