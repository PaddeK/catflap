//go:build windows

package setup

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// removeBinary removes the install folder's entry from the user PATH (added
// by install.ps1) and deletes catflap.exe and its folder. A running .exe
// can't delete itself, so a hidden cmd.exe does it a moment after we exit.
func removeBinary(exe string, out io.Writer) error {
	dir := filepath.Dir(exe)
	if removed, err := removeFromPath(registry.CURRENT_USER, "Environment", dir); err != nil {
		fmt.Fprintf(out, "    could not update your PATH: %v\n", err)
	} else if removed {
		fmt.Fprintf(out, "    removed %s from your user PATH (new terminals pick it up)\n", dir)
	}
	if err := deleteLater(exe); err != nil {
		return err
	}
	fmt.Fprintf(out, "    %s is deleted in a few seconds\n", exe)
	return nil
}

// deleteLater deletes exe (plus leftovers of earlier upgrades) and its
// folder from a hidden, detached cmd.exe, after a short pause.
func deleteLater(exe string) error {
	dir := filepath.Dir(exe)
	script := fmt.Sprintf(`ping -n 3 127.0.0.1 >nul & del /f /q "%s" & del /f /q "%s\catflap.exe.old-*" & rmdir "%s"`, exe, dir, dir)
	cmd := exec.Command("cmd.exe")
	const detached, noWindow = 0x00000008, 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Raw command line: Go's argument quoting (\") isn't what cmd.exe
		// understands. /s /c "…" runs everything between the outer quotes.
		CmdLine:       `cmd.exe /d /s /c "` + script + `"`,
		CreationFlags: detached | noWindow | syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// removeFromPath drops dir from the Path value under root\key, keeping the
// value's type (REG_EXPAND_SZ entries like %USERPROFILE% stay unexpanded).
func removeFromPath(root registry.Key, key, dir string) (bool, error) {
	k, err := registry.OpenKey(root, key, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	raw, typ, err := k.GetStringValue("Path")
	if err != nil {
		return false, nil // no user PATH
	}
	var keep []string
	removed := false
	for _, p := range strings.Split(raw, ";") {
		exp, _ := registry.ExpandString(p)
		if p != "" && strings.EqualFold(strings.TrimRight(exp, `\`), strings.TrimRight(dir, `\`)) {
			removed = true
			continue
		}
		keep = append(keep, p)
	}
	if !removed {
		return false, nil
	}
	val := strings.Join(keep, ";")
	if typ == registry.EXPAND_SZ {
		err = k.SetExpandStringValue("Path", val)
	} else {
		err = k.SetStringValue("Path", val)
	}
	return err == nil, err
}
