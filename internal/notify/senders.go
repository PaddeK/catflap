package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 70 * time.Second} // > Telegram's 50 s long poll

// post sends a request and returns the response body. Errors never contain
// the URL, because the Telegram URL holds the bot token.
func post(ctx context.Context, service, u, contentType string, body []byte, hdr map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("%s: bad URL", service)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "catflap")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, 0, fmt.Errorf("%s: %w", service, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ntfy: JSON publishing, which handles UTF-8 titles (headers don't).
func sendNtfy(ctx context.Context, c Channel, m Message) error {
	prio, tags := 3, []string{"lock"}
	if m.Priority == High {
		prio, tags = 4, []string{"warning"}
	}
	body, _ := json.Marshal(map[string]any{
		"topic": c.Topic, "title": m.Title, "message": m.Body, "priority": prio, "tags": tags,
	})
	hdr := map[string]string{}
	if c.Token != "" {
		hdr["Authorization"] = "Bearer " + c.Token
	}
	b, code, err := post(ctx, "ntfy", strings.TrimSuffix(c.server(), "/")+"/", "application/json", body, hdr)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("ntfy: HTTP %d: %s", code, snippet(b))
	}
	return nil
}

func sendPushover(ctx context.Context, c Channel, m Message) error {
	u := c.APIURL
	if u == "" {
		u = "https://api.pushover.net/1/messages.json"
	}
	prio := "0"
	if m.Priority == High {
		prio = "1"
	}
	form := url.Values{"token": {c.Token}, "user": {c.User}, "title": {m.Title}, "message": {m.Body}, "priority": {prio}}
	b, code, err := post(ctx, "Pushover", u, "application/x-www-form-urlencoded", []byte(form.Encode()), nil)
	if err != nil {
		return err
	}
	var r struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	json.Unmarshal(b, &r)
	if code/100 != 2 || r.Status != 1 {
		if len(r.Errors) > 0 {
			return fmt.Errorf("Pushover: %s", strings.Join(r.Errors, "; "))
		}
		return fmt.Errorf("Pushover: HTTP %d: %s", code, snippet(b))
	}
	return nil
}

func runCommand(ctx context.Context, c Channel, m Message) error {
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	prio := "normal"
	if m.Priority == High {
		prio = "high"
	}
	cmd.Env = append(os.Environ(),
		"CATFLAP_EVENT="+m.Event, "CATFLAP_TITLE="+m.Title, "CATFLAP_MESSAGE="+m.Body, "CATFLAP_PRIORITY="+prio)
	cmd.Stdin = strings.NewReader(m.Title + "\n\n" + m.Body + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if s := snippet(out); s != "" {
			return fmt.Errorf("command: %v: %s", err, s)
		}
		return fmt.Errorf("command: %v", err)
	}
	return nil
}

// FormatHTML renders a message for Telegram's HTML parse mode.
func FormatHTML(m Message) string {
	t := "<b>" + html.EscapeString(m.Title) + "</b>"
	if m.Priority == High {
		t = "⚠️ " + t
	}
	if m.Body == "" {
		return t
	}
	return t + "\n" + html.EscapeString(m.Body)
}
