package setup

import "testing"

func TestViaCatflap(t *testing.T) {
	for in, want := range map[string]bool{
		"127.0.0.1 51234 127.0.0.1 22":        true, // through the tunnel (forward to 127.0.0.1:22)
		"::1 51234 ::1 22":                    true,
		"192.168.1.20 51234 192.168.1.139 22": false, // over the LAN
		"":                                    false, // not an SSH session
	} {
		if got := ViaCatflap(in); got != want {
			t.Errorf("ViaCatflap(%q) = %v, want %v", in, got, want)
		}
	}
}
