// Package notify sends the homelab's notifications (security alerts and the
// invite bot's replies) to the channels configured with `catflap notify`:
// ntfy, Pushover, Telegram, or a command of your choice. It also holds the
// small Telegram Bot API client the homelab's invite bot uses.
//
// Channels are compiled in; "plugin" means a channel type picked in the
// config, not code loaded at runtime. The config, including tokens, lives in
// notify.json next to the homelab state (mode 600).
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PaddeK/catflap/internal/fsutil"
)

// Priority of a message. High maps to ntfy priority 4, Pushover priority 1
// and a warning sign on Telegram.
type Priority int

const (
	Normal Priority = iota
	High
)

// Message is one notification.
type Message struct {
	Event    string // machine-readable kind, e.g. "device.enrolled"
	Title    string
	Body     string
	Priority Priority
}

// Channel types.
const (
	TypeNtfy     = "ntfy"
	TypePushover = "pushover"
	TypeTelegram = "telegram"
	TypeCommand  = "command"
)

// Channel is one configured destination. Which fields apply depends on Type.
type Channel struct {
	Name string `json:"name"`
	Type string `json:"type"`

	// ntfy: server (default https://ntfy.sh) and topic.
	Server string `json:"server,omitempty"`
	Topic  string `json:"topic,omitempty"`
	// Token is the ntfy access token (optional), the Pushover application
	// token, or the Telegram bot token.
	Token string `json:"token,omitempty"`
	// User is the Pushover user (or group) key.
	User string `json:"user,omitempty"`
	// ChatID is the one Telegram chat catflap talks to; everyone else is
	// ignored. Bot makes the homelab answer /invite, /list, /remove there.
	ChatID  int64  `json:"chat_id,omitempty"`
	Bot     bool   `json:"bot,omitempty"`
	BotName string `json:"bot_name,omitempty"`
	// Command is run (no shell) with the message on stdin and the details
	// in CATFLAP_EVENT, CATFLAP_TITLE, CATFLAP_MESSAGE, CATFLAP_PRIORITY.
	Command []string `json:"command,omitempty"`
	// APIURL replaces the Pushover or Telegram API URL (a self-hosted
	// Telegram Bot API server, tests).
	APIURL string `json:"api_url,omitempty"`
}

// Config is the content of notify.json.
type Config struct {
	Channels []Channel `json:"channels"`
}

// Load reads the config; a missing file is an empty config.
func Load(path string) (Config, error) {
	var c Config
	if err := fsutil.ReadJSON(path, &c); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("reading %s: %w", path, err)
	}
	for _, ch := range c.Channels {
		if err := ch.Validate(); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	return c, nil
}

// Save writes the config (mode 600: it holds tokens).
func Save(path string, c Config) error {
	if c.Channels == nil {
		c.Channels = []Channel{}
	}
	return fsutil.WriteJSON(path, c, 0o600)
}

// Validate checks a channel's settings.
func (c Channel) Validate() error {
	if !validName(c.Name) {
		return fmt.Errorf("invalid channel name %q: use lowercase letters, digits, '.', '-' or '_'", c.Name)
	}
	for _, u := range []string{c.Server, c.APIURL} {
		if u == "" {
			continue
		}
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			return fmt.Errorf("channel %q: %q is not an http(s) URL", c.Name, u)
		}
	}
	switch c.Type {
	case TypeNtfy:
		if c.Topic == "" || strings.ContainsAny(c.Topic, "/?#") {
			return fmt.Errorf("channel %q: ntfy needs a topic (letters, digits, '-', '_')", c.Name)
		}
	case TypePushover:
		if c.Token == "" || c.User == "" {
			return fmt.Errorf("channel %q: Pushover needs an application token and a user key", c.Name)
		}
	case TypeTelegram:
		if c.Token == "" || c.ChatID == 0 {
			return fmt.Errorf("channel %q: Telegram needs a bot token and a chat", c.Name)
		}
	case TypeCommand:
		if len(c.Command) == 0 || c.Command[0] == "" {
			return fmt.Errorf("channel %q: command is empty", c.Name)
		}
	default:
		return fmt.Errorf("channel %q: unknown type %q (want ntfy, pushover, telegram or command)", c.Name, c.Type)
	}
	if c.Bot && c.Type != TypeTelegram {
		return fmt.Errorf("channel %q: only Telegram channels can run the bot", c.Name)
	}
	return nil
}

// Describe summarizes a channel without its secrets.
func (c Channel) Describe() string {
	switch c.Type {
	case TypeNtfy:
		auth := ""
		if c.Token != "" {
			auth = " (with access token)"
		}
		return strings.TrimSuffix(c.server(), "/") + "/" + c.Topic + auth
	case TypePushover:
		return "user " + mask(c.User)
	case TypeTelegram:
		s := fmt.Sprintf("chat %d", c.ChatID)
		if c.BotName != "" {
			s = "@" + c.BotName + ", " + s
		}
		if c.Bot {
			s += ", answers /invite /list /remove"
		}
		return s
	case TypeCommand:
		return strings.Join(c.Command, " ")
	}
	return ""
}

func (c Channel) server() string {
	if c.Server == "" {
		return "https://ntfy.sh"
	}
	return c.Server
}

func mask(s string) string {
	if len(s) <= 6 {
		return "…"
	}
	return s[:4] + "…"
}

// Send delivers m on this channel.
func (c Channel) Send(ctx context.Context, m Message) error {
	switch c.Type {
	case TypeNtfy:
		return sendNtfy(ctx, c, m)
	case TypePushover:
		return sendPushover(ctx, c, m)
	case TypeTelegram:
		_, err := c.Telegram().SendMessage(ctx, c.ChatID, FormatHTML(m), nil)
		return err
	case TypeCommand:
		return runCommand(ctx, c, m)
	}
	return fmt.Errorf("unknown channel type %q", c.Type)
}

// Telegram returns a Bot API client for a Telegram channel.
func (c Channel) Telegram() *Telegram {
	return &Telegram{Token: c.Token, APIURL: c.APIURL}
}

func validName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || i > 0 && (r == '.' || r == '-' || r == '_')
		if !ok {
			return false
		}
	}
	return true
}

// ---- dispatcher ----

// Dispatcher sends messages to all channels in the background, with retries,
// so a slow or broken channel never holds up the homelab.
type Dispatcher struct {
	logf func(string, ...any)

	mu      sync.Mutex
	workers map[string]*worker
}

type worker struct {
	ch   Channel
	q    chan Message
	done chan struct{}
}

// NewDispatcher returns an empty dispatcher.
func NewDispatcher(logf func(string, ...any)) *Dispatcher {
	return &Dispatcher{logf: logf, workers: map[string]*worker{}}
}

// Set replaces the channels. Unchanged channels keep their queue.
func (d *Dispatcher) Set(chs []Channel) {
	d.mu.Lock()
	defer d.mu.Unlock()
	keep := map[string]bool{}
	for _, ch := range chs {
		keep[ch.Name] = true
		if w, ok := d.workers[ch.Name]; ok {
			if sameChannel(w.ch, ch) {
				continue
			}
			close(w.done)
		}
		w := &worker{ch: ch, q: make(chan Message, 64), done: make(chan struct{})}
		d.workers[ch.Name] = w
		go d.run(w)
	}
	for name, w := range d.workers {
		if !keep[name] {
			close(w.done)
			delete(d.workers, name)
		}
	}
}

func sameChannel(a, b Channel) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Close stops all workers. Queued messages are dropped.
func (d *Dispatcher) Close() { d.Set(nil) }

// Send queues m for every channel except the one named skip (the channel a
// request came from already got its answer there).
func (d *Dispatcher) Send(m Message, skip string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, w := range d.workers {
		if name == skip {
			continue
		}
		select {
		case w.q <- m:
		default:
			d.logf("notify: %s: queue full, dropped %q", name, m.Title)
		}
	}
}

func (d *Dispatcher) run(w *worker) {
	for {
		select {
		case <-w.done:
			return
		case m := <-w.q:
			d.deliver(w, m)
		}
	}
}

// retryDelays are the pauses between the three attempts per message.
var retryDelays = []time.Duration{5 * time.Second, 30 * time.Second}

// deliver tries a message three times (now, after 5 s, after 30 s).
func (d *Dispatcher) deliver(w *worker, m Message) {
	delays := retryDelays
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := w.ch.Send(ctx, m)
		cancel()
		if err == nil {
			return
		}
		if attempt == len(delays) {
			d.logf("notify: %s: giving up on %q: %v", w.ch.Name, m.Title, err)
			return
		}
		d.logf("notify: %s: %v (retrying)", w.ch.Name, err)
		select {
		case <-w.done:
			return
		case <-time.After(delays[attempt]):
		}
	}
}
