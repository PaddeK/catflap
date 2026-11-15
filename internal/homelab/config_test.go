package homelab

import (
	"path/filepath"
	"testing"
)

func TestAdminKeysFile(t *testing.T) {
	const pd = `C:\ProgramData`
	def := "# This is the sshd server system-wide configuration file.\n#PubkeyAuthentication yes\nAuthorizedKeysFile\t.ssh/authorized_keys\nSubsystem\tsftp\tsftp-server.exe\n\nMatch Group administrators\n       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys\n"
	if p, ok := adminKeysFile(def, pd); !ok || p != filepath.FromSlash(pd+"/ssh/administrators_authorized_keys") {
		t.Errorf("default config: got %q, %v", p, ok)
	}
	commented := "AuthorizedKeysFile .ssh/authorized_keys\n#Match Group administrators\n#       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys\n"
	if p, ok := adminKeysFile(commented, pd); ok {
		t.Errorf("commented out: got %q", p)
	}
	other := "Match Group administrators\n  PasswordAuthentication no\nMatch User anna\n  AuthorizedKeysFile C:/keys/anna\n"
	if p, ok := adminKeysFile(other, pd); ok {
		t.Errorf("keys file in another block: got %q", p)
	}
	list := "Match Group users,Administrators\n AuthorizedKeysFile D:/ssh/admin_keys\n"
	if p, ok := adminKeysFile(list, pd); !ok || p != filepath.FromSlash("D:/ssh/admin_keys") {
		t.Errorf("group list: got %q, %v", p, ok)
	}
}
