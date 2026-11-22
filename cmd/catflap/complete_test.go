package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestComplete(t *testing.T) {
	t.Setenv("CATFLAP_DIR", t.TempDir()) // no client state, no homelab running
	ctx := context.Background()
	for _, c := range []struct {
		line string // finished words, then the current one after "|"
		want []string
	}{
		{"|se", []string{"setup"}},
		{"|rel", []string{"relay"}},
		{"setup|", []string{"homelab", "relay"}},
		{"setup relay|--c", []string{"--cert", "--cert-mode"}},
		{"setup relay --cert-mode|", []string{"letsencrypt", "manual"}},
		{"setup relay --cert-mode manual|--f", []string{"--force"}},
		{"notify|", []string{"add", "list", "remove", "test"}},
		{"notify add|", []string{"command", "ntfy", "pushover", "telegram"}},
		{"notify add telegram|", nil},
		{"fwd|", []string{"add", "list", "remove"}}, // short form, no aliases enrolled
		{"requests|o", []string{"off", "on"}},
		{"uninstall|", []string{"client", "homelab", "relay"}},
		{"uninstall homelab|", nil},
		{"completion|", []string{"bash", "fish", "powershell", "zsh"}},
		{"invite --ttl|", nil},
		{"ping|--", []string{"--count"}},
	} {
		done, cur, _ := strings.Cut(c.line, "|")
		got := complete(ctx, strings.Fields(done), cur)
		if !slices.Equal(got, c.want) {
			t.Errorf("%q: got %q, want %q", c.line, got, c.want)
		}
	}
	top := complete(ctx, nil, "")
	for _, cmd := range []string{"setup", "enroll", "ping", "completion", "status"} {
		if !slices.Contains(top, cmd) {
			t.Errorf("top level lacks %q", cmd)
		}
	}
	for _, short := range []string{"ls", "rm", "fwd", "__complete"} {
		if slices.Contains(top, short) {
			t.Errorf("top level offers %q", short)
		}
	}
}

// TestCompletionFlags checks that every flag offered for completion exists:
// `catflap <command> <flag> -h` must stop at -h, not at the flag.
func TestCompletionFlags(t *testing.T) {
	flagOutput = io.Discard
	t.Setenv("CATFLAP_DIR", t.TempDir())
	values := map[string]string{"--forward": "22", "--count": "1", "--http-port": "80", "--stun-port": "3478",
		"--ttl": "1h", "--for": "1h", "--expires": "7d"}
	var walk func(path []string, s *spec)
	walk = func(path []string, s *spec) {
		for _, f := range s.flags {
			arg := f
			if name, ok := strings.CutSuffix(f, "="); ok {
				v := values[name]
				if v == "" {
					v = "x"
				}
				arg = name + "=" + v
			}
			args := append(slices.Clone(path), arg, "-h")
			if err := run(context.Background(), args); !errors.Is(err, flag.ErrHelp) {
				t.Errorf("catflap %s: %v", strings.Join(args, " "), err)
			}
		}
		for name, sub := range s.sub {
			if !slices.Contains(s.hidden, name) {
				walk(append(slices.Clone(path), name), sub)
			}
		}
	}
	walk(nil, completionTree)
}

func TestCompletionScripts(t *testing.T) {
	for name, s := range completionScripts {
		if !strings.Contains(s, "catflap __complete") {
			t.Errorf("%s script doesn't call catflap __complete", name)
		}
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(completionScripts["bash"])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("bash script: %v\n%s", err, out)
	}
}

// TestOwnHelp checks that `catflap CMD -h` shows the command's own options
// for every command that has some (instead of the general usage).
func TestOwnHelp(t *testing.T) {
	flagOutput = io.Discard
	t.Setenv("CATFLAP_DIR", t.TempDir())
	for name, s := range completionTree.sub {
		if len(s.flags) == 0 || slices.Contains(completionTree.hidden, name) {
			continue
		}
		if !slices.Contains(ownHelp, name) {
			t.Errorf("catflap %s has options, but isn't in ownHelp", name)
		}
		if err := run(context.Background(), []string{name, "-h"}); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("catflap %s -h: %v", name, err)
		}
	}
}
