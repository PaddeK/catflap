//go:build windows

package client

import (
	"os/exec"
	"syscall"
)

const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
	createNoWindow         = 0x08000000
)

// startDetached starts cmd without a console and outside the caller's
// process group, so it survives ssh ending the ProxyCommand. Breaking away
// from the caller's job object is tried first (some terminals and editors
// kill the whole job on exit); jobs that forbid it get a second attempt
// without.
func startDetached(cmd *exec.Cmd) error {
	base := uint32(detachedProcess | createNewProcessGroup | createNoWindow)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: base | createBreakawayFromJob, HideWindow: true}
	if err := cmd.Start(); err == nil {
		return nil
	}
	retry := exec.Command(cmd.Path, cmd.Args[1:]...)
	retry.Stdout, retry.Stderr, retry.Env = cmd.Stdout, cmd.Stderr, cmd.Env
	retry.SysProcAttr = &syscall.SysProcAttr{CreationFlags: base, HideWindow: true}
	if err := retry.Start(); err != nil {
		return err
	}
	*cmd = *retry
	return nil
}
