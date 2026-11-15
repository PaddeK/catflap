package homelab

import "testing"

func TestParseForward(t *testing.T) {
	good := []struct {
		in, target string
		port       int
	}{
		{"1880", "127.0.0.1:1880", 1880},
		{"22=127.0.0.1:22", "127.0.0.1:22", 22},
		{"8080=192.168.1.50:80", "192.168.1.50:80", 8080},
		{"8443=nas.lan:443", "nas.lan:443", 8443},
		{"9000=[::1]:9000", "[::1]:9000", 9000},
	}
	for _, g := range good {
		port, target, err := ParseForward(g.in)
		if err != nil || port != g.port || target != g.target {
			t.Errorf("ParseForward(%q) = %d, %q, %v; want %d, %q", g.in, port, target, err, g.port, g.target)
		}
	}
	for _, bad := range []string{"", "0", "65536", "abc", "22=", "22=host", "22=:22", "22=host:0", "22=host:x", "-1=host:22"} {
		if _, _, err := ParseForward(bad); err == nil {
			t.Errorf("ParseForward(%q) accepted", bad)
		}
	}
}
