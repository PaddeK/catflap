package main

// Shell completion: `catflap completion bash|zsh|fish|powershell` prints a
// small script that asks catflap itself for the candidates:
//
//	catflap __complete N WORD...
//
// N is the number of finished words after "catflap"; a word after them is
// the one being completed (none: an empty word). The candidates come out one
// per line, already filtered by that word. Device names, aliases, channels
// and ports are looked up live (homelab control socket, client state).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PaddeK/catflap/internal/client"
	"github.com/PaddeK/catflap/internal/homelab"
)

// spec describes a command for completion.
type spec struct {
	sub    map[string]*spec // subcommands
	hidden []string         // subcommand names not offered (short forms)
	// flags of the command; a trailing "=" marks a flag that takes a value.
	flags []string
	// values offers values for flags that take one.
	values map[string]func(context.Context) []string
	// args offers the n-th positional argument (subcommands excluded).
	args func(ctx context.Context, n int, done []string) []string
}

var completionTree = buildTree()

func buildTree() *spec {
	devices := &spec{args: first(deviceNames)}
	forward := &spec{
		sub: map[string]*spec{
			"add":    {},
			"remove": {args: first(forwardPorts)},
			"list":   {},
		},
		hidden: []string{"rm", "ls"},
		args:   first(aliases),
	}
	forward.sub["rm"], forward.sub["ls"] = forward.sub["remove"], forward.sub["list"]
	channels := &spec{args: first(channelNames)}
	notifyAdd := &spec{
		flags: []string{"--name=", "--api-url="},
		args: func(_ context.Context, n int, _ []string) []string {
			if n == 0 {
				return []string{"telegram", "ntfy", "pushover", "command"}
			}
			return nil
		},
	}
	relayFlags := []string{"--config="}
	root := &spec{sub: map[string]*spec{
		"setup": {sub: map[string]*spec{
			"relay": {flags: []string{"--hostname=", "--email=", "--listen=", "--http-port=", "--stun-port=",
				"--cert-mode=", "--cert=", "--key=", "--no-firewall", "--restore=", "--force"},
				values: map[string]func(context.Context) []string{"--cert-mode": fixed("letsencrypt", "manual")}},
			"homelab": {flags: []string{"--join=", "--name=", "--force", "--no-service", "--forward="}},
		}},
		"enroll": {flags: []string{"--alias=", "--ssh-key=", "--no-ssh-key", "--no-ssh-config", "--request=", "--name="}},
		"invite": {flags: []string{"--ttl=", "--replace"},
			args: func(ctx context.Context, n int, done []string) []string {
				if n == 0 && slices.ContainsFunc(done, isFlag("replace")) {
					return deviceNames(ctx)
				}
				return nil
			}},
		"invites": {flags: []string{"--revoke="},
			values: map[string]func(context.Context) []string{"--revoke": inviteNames}},
		"list": {},
		"requests": {flags: []string{"--for="},
			args: func(_ context.Context, n int, _ []string) []string {
				if n == 0 {
					return []string{"on", "off"}
				}
				return nil
			}},
		"approve": {args: first(pendingNames)},
		"deny":    {args: first(pendingNames)},
		"remove":  devices,
		"forward": forward,
		"homelab": {sub: map[string]*spec{
			"move": {flags: []string{"--join=", "--ttl=", "--yes"}},
			"run":  {flags: []string{"--service"}},
		}},
		"notify": {sub: map[string]*spec{
			"add":    notifyAdd,
			"list":   {},
			"remove": channels,
			"test":   channels,
			"ls":     {},
			"rm":     channels,
		}, hidden: []string{"ls", "rm"}},
		"ping":   {flags: []string{"--count="}, args: first(aliases)},
		"proxy":  {args: first(aliases)},
		"forget": {args: first(aliases)},
		"relay": {sub: map[string]*spec{
			"join-code": {flags: append([]string{"--ttl="}, relayFlags...)},
			"status":    {flags: relayFlags},
			"remove":    {flags: append([]string{"--yes"}, relayFlags...)},
			"backup":    {flags: relayFlags},
			"run":       {flags: relayFlags},
		}},
		"status":     {},
		"restart":    {args: first(fixed("relay", "homelab"))},
		"uninstall":  {flags: []string{"--yes", "--keep-binary"}, args: first(fixed("relay", "homelab", "client"))},
		"version":    {},
		"help":       {},
		"completion": {args: first(fixed("bash", "zsh", "fish", "powershell"))},
	}, hidden: []string{"ls", "rm", "fwd"}}
	root.sub["ls"], root.sub["rm"], root.sub["fwd"] = root.sub["list"], root.sub["remove"], root.sub["forward"]
	return root
}

// complete returns the candidates for cur, after the finished words done.
func complete(ctx context.Context, done []string, cur string) []string {
	s := completionTree
	var pos []string // positional words of the current command
	expectValue := ""
	for _, w := range done {
		if expectValue != "" {
			expectValue = ""
			continue
		}
		if strings.HasPrefix(w, "-") {
			if f := s.flag(w); f != "" && !strings.Contains(w, "=") {
				expectValue = f
			}
			continue
		}
		if sub := s.sub[w]; sub != nil && len(pos) == 0 {
			s = sub
			continue
		}
		pos = append(pos, w)
	}
	var out []string
	switch {
	case expectValue != "":
		if v := s.values[expectValue]; v != nil {
			out = v(ctx)
		}
	case strings.HasPrefix(cur, "-"):
		for _, f := range s.flags {
			out = append(out, strings.TrimSuffix(f, "="))
		}
	default:
		if len(pos) == 0 {
			for name := range s.sub {
				if !slices.Contains(s.hidden, name) {
					out = append(out, name)
				}
			}
		}
		if s.args != nil {
			out = append(out, s.args(ctx, len(pos), done)...)
		}
	}
	out = slices.DeleteFunc(out, func(c string) bool { return !strings.HasPrefix(c, cur) })
	slices.Sort(out)
	return slices.Compact(out)
}

// flag returns the name ("--ttl") of the value-taking flag w refers to.
func (s *spec) flag(w string) string {
	name, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "=")
	for _, f := range s.flags {
		if f == "--"+name+"=" {
			return "--" + name
		}
	}
	return ""
}

func isFlag(name string) func(string) bool {
	return func(w string) bool { return strings.TrimLeft(w, "-") == name }
}

func first(f func(context.Context) []string) func(context.Context, int, []string) []string {
	return func(ctx context.Context, n int, _ []string) []string {
		if n == 0 {
			return f(ctx)
		}
		return nil
	}
}

func fixed(vals ...string) func(context.Context) []string {
	return func(context.Context) []string { return vals }
}

// ---- live values (silently empty when unavailable) ----

func aliases(context.Context) []string {
	st, _, err := client.Load()
	if err != nil {
		return nil
	}
	return st.Aliases()
}

func homelabGet(ctx context.Context, path string, v any) bool {
	c, err := control()
	if err != nil {
		return false
	}
	return c.Do(ctx, "GET", path, nil, v) == nil
}

func deviceNames(ctx context.Context) []string {
	var devs []homelab.Device
	homelabGet(ctx, "/devices", &devs)
	var out []string
	for _, d := range devs {
		out = append(out, d.Name)
	}
	return out
}

func inviteNames(ctx context.Context) []string {
	var invs []homelab.InviteInfo
	homelabGet(ctx, "/invites", &invs)
	var out []string
	for _, i := range invs {
		out = append(out, i.Name)
	}
	return out
}

func forwardPorts(ctx context.Context) []string {
	var fwds []homelab.ForwardInfo
	homelabGet(ctx, "/forwards", &fwds)
	var out []string
	for _, f := range fwds {
		out = append(out, strconv.Itoa(f.Port))
	}
	return out
}

func channelNames(ctx context.Context) []string {
	var chs []homelab.ChannelInfo
	homelabGet(ctx, "/notify", &chs)
	var out []string
	for _, c := range chs {
		out = append(out, c.Name)
	}
	return out
}

func pendingNames(ctx context.Context) []string {
	var info homelab.RequestsInfo
	homelabGet(ctx, "/requests", &info)
	var out []string
	for _, p := range info.Pending {
		out = append(out, p.Name)
	}
	return out
}

// cmdComplete is the hidden `catflap __complete N WORD...`.
func cmdComplete(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: catflap __complete N WORD...")
	}
	n, err := strconv.Atoi(args[0])
	words := args[1:]
	if err != nil || n < 0 || n > len(words) {
		return errors.New("usage: catflap __complete N WORD...")
	}
	cur := ""
	if len(words) > n {
		cur = words[n]
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, c := range complete(ctx, words[:n], cur) {
		fmt.Println(c)
	}
	return nil
}

func cmdCompletion(args []string) error {
	if len(args) == 1 {
		if s, ok := completionScripts[args[0]]; ok {
			fmt.Print(s)
			return nil
		}
	}
	return errors.New("usage: catflap completion bash|zsh|fish|powershell")
}

var completionScripts = map[string]string{
	"bash": `# bash completion for catflap. Load it in ~/.bashrc with:
#   source <(catflap completion bash)
_catflap() {
    local IFS=$'\n'
    COMPREPLY=($(catflap __complete "$((COMP_CWORD - 1))" "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null))
}
complete -o default -F _catflap catflap
`,
	"zsh": `#compdef catflap
# zsh completion for catflap. Load it in ~/.zshrc (after compinit) with:
#   source <(catflap completion zsh)
_catflap() {
    local -a cands
    cands=("${(@f)$(catflap __complete $((CURRENT - 2)) "${(@)words[2,CURRENT]}" 2>/dev/null)}")
    cands=(${cands:#})
    if (( ${#cands} )); then
        compadd -- "${cands[@]}"
    else
        _files
    fi
}
if [ "$funcstack[1]" = "_catflap" ]; then
    _catflap "$@"
else
    compdef _catflap catflap
fi
`,
	"fish": `# fish completion for catflap. Install it with:
#   catflap completion fish > ~/.config/fish/completions/catflap.fish
function __catflap_complete
    set -l done (commandline -opc)[2..-1]
    catflap __complete (count $done) $done (commandline -ct) 2>/dev/null
end
complete -c catflap -f -a '(__catflap_complete)'
`,
	"powershell": `# PowerShell completion for catflap. Load it in your $PROFILE with:
#   catflap completion powershell | Out-String | Invoke-Expression
Register-ArgumentCompleter -Native -CommandName catflap, catflap.exe -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $done = @($commandAst.CommandElements | Select-Object -Skip 1 |
        Where-Object { $_.Extent.EndOffset -lt $cursorPosition } |
        ForEach-Object { $_.Extent.Text })
    $cfArgs = @($done.Count) + $done
    if ($wordToComplete -ne '') { $cfArgs += $wordToComplete }
    catflap __complete @cfArgs 2>$null | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`,
}
