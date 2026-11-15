package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Telegram is a minimal Bot API client (https://core.telegram.org/bots/api):
// just what catflap's notifications and invite bot use. The homelab only
// makes outgoing requests (long polling); no webhook, no open port.
type Telegram struct {
	Token  string
	APIURL string // default https://api.telegram.org
}

// TelegramError is an error answer from the Bot API.
type TelegramError struct {
	Code        int
	Description string
}

func (e *TelegramError) Error() string {
	return fmt.Sprintf("Telegram: %s (%d)", e.Description, e.Code)
}

// Conflict reports whether another program polls the same bot.
func (e *TelegramError) Conflict() bool { return e.Code == 409 }

// TUser is a Telegram user.
type TUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// Label names a user for alerts.
func (u *TUser) Label() string {
	if u == nil {
		return "unknown"
	}
	if u.Username != "" {
		return fmt.Sprintf("@%s (id %d)", u.Username, u.ID)
	}
	return fmt.Sprintf("%s (id %d)", u.FirstName, u.ID)
}

// TChat is a chat.
type TChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private" for a one-to-one chat with the bot
}

// TMessage is a message.
type TMessage struct {
	MessageID int64  `json:"message_id"`
	From      *TUser `json:"from"`
	Chat      TChat  `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
}

// TCallback is a tap on an inline button.
type TCallback struct {
	ID      string    `json:"id"`
	From    TUser     `json:"from"`
	Message *TMessage `json:"message"`
	Data    string    `json:"data"`
}

// TUpdate is one incoming event.
type TUpdate struct {
	UpdateID      int64      `json:"update_id"`
	Message       *TMessage  `json:"message"`
	CallbackQuery *TCallback `json:"callback_query"`
}

// Keyboard is an inline keyboard under a message.
type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

// Button is an inline button.
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

func (t *Telegram) call(ctx context.Context, method string, in, out any) error {
	base := t.APIURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	body, _ := json.Marshal(in)
	b, code, err := post(ctx, "Telegram", strings.TrimSuffix(base, "/")+"/bot"+t.Token+"/"+method, "application/json", body, nil)
	if err != nil {
		return err
	}
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
	}
	if json.Unmarshal(b, &r) != nil {
		return fmt.Errorf("Telegram: HTTP %d: %s", code, snippet(b))
	}
	if !r.OK {
		if r.ErrorCode == 0 {
			r.ErrorCode = code
		}
		return &TelegramError{Code: r.ErrorCode, Description: r.Description}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// GetMe checks the token and returns the bot's user.
func (t *Telegram) GetMe(ctx context.Context) (TUser, error) {
	var u TUser
	err := t.call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

// GetUpdates long-polls for updates after offset (waiting up to timeout seconds).
func (t *Telegram) GetUpdates(ctx context.Context, offset int64, timeout int) ([]TUpdate, error) {
	var ups []TUpdate
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

// SendMessage sends an HTML message and returns its id.
func (t *Telegram) SendMessage(ctx context.Context, chat int64, htmlText string, kb *Keyboard) (int64, error) {
	in := map[string]any{"chat_id": chat, "text": htmlText, "parse_mode": "HTML", "link_preview_options": map[string]bool{"is_disabled": true}}
	if kb != nil {
		in["reply_markup"] = kb
	}
	var m TMessage
	err := t.call(ctx, "sendMessage", in, &m)
	return m.MessageID, err
}

// EditMessage replaces a message's text (and removes its buttons).
func (t *Telegram) EditMessage(ctx context.Context, chat, msg int64, htmlText string) error {
	return t.call(ctx, "editMessageText", map[string]any{
		"chat_id": chat, "message_id": msg, "text": htmlText, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	}, nil)
}

// DeleteMessage deletes a message (bots can delete their own for 48 hours).
func (t *Telegram) DeleteMessage(ctx context.Context, chat, msg int64) error {
	return t.call(ctx, "deleteMessage", map[string]any{"chat_id": chat, "message_id": msg}, nil)
}

// SetCommands sets the bot's command menu ([{command, description}]).
func (t *Telegram) SetCommands(ctx context.Context, cmds []map[string]string) error {
	return t.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// AnswerCallback acknowledges a button tap, optionally with a short notice.
func (t *Telegram) AnswerCallback(ctx context.Context, id, text string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
