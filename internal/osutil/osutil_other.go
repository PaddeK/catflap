//go:build !windows

package osutil

import (
	"errors"
	"os"
)

// IsAdmin reports whether the current user is root.
func IsAdmin() bool { return os.Geteuid() == 0 }

// IsElevated reports whether the current user is root.
func IsElevated() bool { return os.Geteuid() == 0 }

// UserSID exists on Windows only.
func UserSID() (string, error) { return "", errors.New("no SIDs outside Windows") }

// RestrictToAdmins is for Windows ACLs; Unix modes are set when writing.
func RestrictToAdmins(string) error { return nil }

// AllowMediumIntegrity is for Windows integrity labels.
func AllowMediumIntegrity(string) error { return nil }
