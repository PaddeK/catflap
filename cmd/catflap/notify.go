package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PaddeK/catflap/internal/homelab"
	"github.com/PaddeK/catflap/internal/notify"

	"golang.org/x/term"
)

const notifyUsage = `usage (on the homelab):
  catflap notify add telegram [--name N]     alerts + the invite bot (prompts for the bot token)
  catflap notify add ntfy [--name N]         alerts via ntfy (ntfy.sh or your own server)
  catflap notify add pushover [--name N]     alerts via Pushover
  catflap notify add command [--name N] -- CMD [ARGS...]
                                             run CMD per alert (message on stdin, CATFLAP_* env)
  catflap notify list
  catflap notify remove <name>
  catflap notify test [name]`

func cmdNotify(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(notifyUsage)
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "add":
		return cmdNotifyAdd(ctx, args)
	case "list", "ls":
		return cmdNotifyList(ctx)
	case "remove", "rm":
		if len(args) != 1 {
			return errors.New("usage: catflap notify remove <name>")
		}
		c, err := control()
		if err != nil {
			return err
		}
		if err := c.Do(ctx, "DELETE", "/notify/"+args[0], nil, nil); err != nil {
			return ctlErr(err)
		}
		fmt.Printf("removed channel %q\n", args[0])
		return nil
	case "test":
		return cmdNotifyTest(ctx, args)
	}
	return errors.New(notifyUsage)
}

func cmdNotifyList(ctx context.Context) error {
	c, err := control()
	if err != nil {
		return err
	}
	var chs []homelab.ChannelInfo
	if err := c.Do(ctx, "GET", "/notify", nil, &chs); err != nil {
		return ctlErr(err)
	}
	if len(chs) == 0 {
		fmt.Println("no notification channels; add one with: catflap notify add telegram|ntfy|pushover|command")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tDETAILS")
	for _, ch := range chs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", ch.Name, ch.Type, ch.Detail)
	}
	return tw.Flush()
}

func cmdNotifyTest(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: catflap notify test [name]")
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	c, err := control()
	if err != nil {
		return err
	}
	var res []homelab.TestResult
	if err := c.Do(ctx, "POST", "/notify/test", map[string]string{"name": name}, &res); err != nil {
		return ctlErr(err)
	}
	if len(res) == 0 {
		fmt.Println("no notification channels")
		return nil
	}
	failed := 0
	for _, r := range res {
		if r.Error != "" {
			failed++
			fmt.Printf("%s: FAILED: %s\n", r.Name, r.Error)
		} else {
			fmt.Printf("%s: sent\n", r.Name)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d channel(s) failed", failed)
	}
	return nil
}

func cmdNotifyAdd(ctx context.Context, args []string) error {
	// Everything after "--" is the command of a command channel.
	var cmdArgs []string
	for i, a := range args {
		if a == "--" {
			args, cmdArgs = args[:i], args[i+1:]
			break
		}
	}
	fs := newFlags("notify add")
	name := fs.String("name", "", "channel name (default: the type)")
	apiURL := fs.String("api-url", "", "Telegram/Pushover API URL (self-hosted Bot API server, tests)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(notifyUsage)
	}
	typ := pos[0]
	if *name == "" {
		*name = typ
	}
	c, err := control() // fail early if the homelab isn't running
	if err != nil {
		return err
	}
	in := newPrompter()
	ch := notify.Channel{Name: *name, Type: typ, APIURL: *apiURL}
	switch typ {
	case notify.TypeTelegram:
		if err := setupTelegram(ctx, in, &ch); err != nil {
			return err
		}
	case notify.TypeNtfy:
		fmt.Println("ntfy: get the ntfy app and subscribe to the topic below.")
		ch.Server = in.line("Server", "https://ntfy.sh")
		ch.Topic = in.line("Topic", "catflap-"+randomTopic())
		if strings.TrimSuffix(ch.Server, "/") == "https://ntfy.sh" {
			fmt.Println("  (on ntfy.sh, anyone who knows a topic name can read it: keep the random name private)")
		}
		ch.Token = in.secret("Access token (Enter for none)")
	case notify.TypePushover:
		fmt.Println("Pushover: your user key is on pushover.net; create an application there for the token.")
		ch.User = in.secret("User key")
		ch.Token = in.secret("Application token")
	case notify.TypeCommand:
		if len(cmdArgs) == 0 {
			return errors.New("usage: catflap notify add command [--name N] -- CMD [ARGS...]\n  e.g. -- sh -c 'mail -s \"$CATFLAP_TITLE\" me@example.com'")
		}
		ch.Command = cmdArgs
	default:
		return fmt.Errorf("unknown channel type %q (want telegram, ntfy, pushover or command)", typ)
	}
	if typ != notify.TypeCommand && len(cmdArgs) > 0 {
		return errors.New("only command channels take a command after --")
	}
	if err := ch.Validate(); err != nil {
		return err
	}
	fmt.Println("sending a test message ...")
	var info homelab.ChannelInfo
	if err := c.Do(ctx, "PUT", "/notify/"+ch.Name, ch, &info); err != nil {
		return ctlErr(err)
	}
	fmt.Printf("added channel %q (%s: %s)\n", info.Name, info.Type, info.Detail)
	if info.Bot {
		fmt.Println("The bot answers /invite, /list and /remove in that chat now. Try /help.")
	}
	return nil
}

// setupTelegram checks the token and links the one chat catflap talks to:
// you open the bot with a one-time code, so a stranger can't link instead.
func setupTelegram(ctx context.Context, in *prompter, ch *notify.Channel) error {
	fmt.Println("Telegram: create a bot with @BotFather (/newbot) and paste its token here.")
	ch.Token = in.secret("Bot token")
	tg := ch.Telegram()
	me, err := tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("checking the token: %w", err)
	}
	ch.BotName = me.Username
	code := strings.ToLower(randomTopic()[:10])
	fmt.Printf(`
Now open this link on your phone and tap Start:

  https://t.me/%s?start=%s

(or send "/start %s" to @%s). Waiting up to 5 minutes ...
`, me.Username, code, code, me.Username)
	deadline := time.Now().Add(5 * time.Minute)
	var offset int64
	for ch.ChatID == 0 {
		if time.Now().After(deadline) {
			return errors.New("no /start message with the code arrived; run the command again")
		}
		ups, err := tg.GetUpdates(ctx, offset, 20)
		if err != nil {
			var te *notify.TelegramError
			if errors.As(err, &te) && te.Conflict() {
				return errors.New("another program is polling this bot. If catflap already uses it: catflap notify remove <name> first")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.From == nil || m.Chat.Type != "private" {
				continue
			}
			if strings.TrimSpace(m.Text) == "/start "+code {
				ch.ChatID = m.Chat.ID
				fmt.Printf("linked to %s\n", m.From.Label())
			}
		}
	}
	tg.GetUpdates(ctx, offset, 0) // mark the /start as read
	ch.Bot = in.yes("Answer /invite, /list and /remove from this chat (the invite bot)?", true)
	return nil
}

func randomTopic() string {
	var b [12]byte
	rand.Read(b[:])
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// prompter reads answers from the terminal, or line by line from stdin when
// it isn't one (scripts, tests). Secrets are never echoed.
type prompter struct {
	r   *bufio.Reader
	tty bool
}

func newPrompter() *prompter {
	return &prompter{r: bufio.NewReader(os.Stdin), tty: term.IsTerminal(int(os.Stdin.Fd()))}
}

func (p *prompter) readLine() string {
	s, _ := p.r.ReadString('\n')
	return strings.TrimSpace(s)
}

func (p *prompter) line(prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	s := p.readLine()
	if !p.tty {
		fmt.Println()
	}
	if s == "" {
		return def
	}
	return s
}

func (p *prompter) secret(prompt string) string {
	fmt.Printf("%s (hidden): ", prompt)
	defer fmt.Println()
	if p.tty {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return p.readLine()
}

func (p *prompter) yes(prompt string, def bool) bool {
	d := "Y/n"
	if !def {
		d = "y/N"
	}
	s := strings.ToLower(p.line(prompt+" ["+d+"]", ""))
	switch s {
	case "":
		return def
	case "y", "yes", "j", "ja":
		return true
	}
	return false
}
