//go:build windows

package osutil

import (
	"golang.org/x/sys/windows"
)

// IsAdmin reports whether the current user belongs to the Administrators
// group, also when UAC filtered it out of this process (not elevated).
func IsAdmin() bool {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	t := windows.GetCurrentProcessToken()
	groups, err := t.GetTokenGroups()
	if err != nil {
		return false
	}
	for _, g := range groups.AllGroups() {
		if g.Sid.Equals(admins) {
			return true
		}
	}
	// Elevated tokens list the group as enabled; filtered ones as
	// deny-only. Both appear above. A linked token covers the rest.
	if lt, err := t.GetLinkedToken(); err == nil {
		defer lt.Close()
		if ok, err := lt.IsMember(admins); err == nil && ok {
			return true
		}
	}
	return false
}

// IsElevated reports whether this process runs with full admin rights.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// UserSID returns the current user's SID as a string.
func UserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// RestrictToAdmins gives path an ACL of SYSTEM and Administrators only,
// without inherited entries: what Windows' OpenSSH server demands of
// administrators_authorized_keys.
func RestrictToAdmins(path string) error {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
		admins, nil, dacl, nil)
}

// AllowMediumIntegrity labels path medium integrity, so that a process
// that isn't elevated can open a file (here: connect to a Unix socket) an
// elevated process created. Without elevation there is nothing to do.
func AllowMediumIntegrity(path string) error {
	if !IsElevated() {
		return nil
	}
	sd, err := windows.SecurityDescriptorFromString("S:(ML;;NW;;;ME)")
	if err != nil {
		return err
	}
	sacl, _, err := sd.SACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, sacl)
}
