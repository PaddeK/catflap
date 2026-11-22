package homelab

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestForwardOnly(t *testing.T) {
	f := &forward{port: 1880, target: "127.0.0.1:1880", only: normalizeOnly([]string{"wsl", "phone", "wsl"}), conns: map[net.Conn]string{}}
	if got := f.info().String(); got != "1880->127.0.0.1:1880 (only phone, wsl)" {
		t.Fatalf("String: %q", got)
	}
	if !f.allows("phone") || f.allows("laptop") || f.allows("unknown") {
		t.Fatal("allows")
	}
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	if !f.track("phone", a1) || !f.track("wsl", b1) || f.track("laptop", b1) {
		t.Fatal("track")
	}
	// Taking access away cuts that device's open connections, not the others.
	f.setOnly([]string{"wsl"})
	if _, err := a2.Write([]byte("x")); err == nil {
		t.Fatal("phone's connection is still open")
	}
	go b2.Write([]byte("x"))
	buf := make([]byte, 1)
	if _, err := b1.Read(buf); err != nil {
		t.Fatalf("wsl's connection was cut: %v", err)
	}
	f.setOnly(nil)
	if !f.allows("laptop") || f.info().String() != "1880->127.0.0.1:1880" {
		t.Fatal("nil should open the port to all devices")
	}
}

func TestLoadConfigForwardOnly(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Config: filepath.Join(dir, "homelab.json")}
	load := func(js string) error {
		os.WriteFile(p.Config, []byte(js), 0o644)
		_, err := LoadConfig(p)
		return err
	}
	if err := DeviceNameError("any"); err == nil {
		t.Fatal(`"any" accepted as a device name`)
	}
	if err := load(`{"relay":"r","forwards":{"22":"127.0.0.1:22","1880":"127.0.0.1:1880"},"forward_only":{"1880":["phone"]}}`); err != nil {
		t.Fatal(err)
	}
	for js, want := range map[string]string{
		`{"relay":"r","forwards":{"22":"127.0.0.1:22"},"forward_only":{"1880":["phone"]}}`:  "not forwarded",
		`{"relay":"r","forwards":{"22":"127.0.0.1:22"},"forward_only":{"22":[]}}`:           "has no devices",
		`{"relay":"r","forwards":{"22":"127.0.0.1:22"},"forward_only":{"22":["Bad Name"]}}`: "bad device name",
		`{"relay":"r","forwards":{"22":"127.0.0.1:22"},"forward_only":{"22":["any"]}}`:      "bad device name",
	} {
		if err := load(js); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", js, err, want)
		}
	}
}
