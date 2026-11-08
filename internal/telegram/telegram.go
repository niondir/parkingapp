// Package telegram is a minimal client for the Telegram Bot API (long polling).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	token string
	http  *http.Client
}

func New(token string) *Client {
	return &Client{token: token, http: &http.Client{Timeout: 70 * time.Second}}
}

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type InlineButton struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}

type InlineKeyboard struct {
	InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// APIError is returned for responses with ok=false.
type APIError struct {
	Code        int    `json:"error_code"`
	Description string `json:"description"`
}

func (e *APIError) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Description) }

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+c.token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL contains the token, don't leak it into logs.
		if ue, ok := err.(interface{ Unwrap() error }); ok && ue.Unwrap() != nil {
			return fmt.Errorf("telegram %s: %w", method, ue.Unwrap())
		}
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var r struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		APIError
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d: decode: %w", method, resp.StatusCode, err)
	}
	if !r.OK {
		return &r.APIError
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var u []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}, &u)
	return u, err
}

// Send sends an HTML formatted message. kb may be nil.
func (c *Client) Send(ctx context.Context, chatID int64, html string, kb *InlineKeyboard) error {
	return c.Push(ctx, chatID, html, kb, false)
}

// Push is Send with the option to deliver without sound.
func (c *Client) Push(ctx context.Context, chatID int64, html string, kb *InlineKeyboard, silent bool) error {
	p := map[string]any{
		"chat_id":              chatID,
		"text":                 html,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
		"disable_notification": silent,
	}
	if kb != nil {
		p["reply_markup"] = kb
	}
	return c.call(ctx, "sendMessage", p, nil)
}

// Edit replaces text and keyboard of a message sent by the bot.
func (c *Client) Edit(ctx context.Context, chatID, messageID int64, html string, kb *InlineKeyboard) error {
	p := map[string]any{
		"chat_id":              chatID,
		"message_id":           messageID,
		"text":                 html,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if kb != nil {
		p["reply_markup"] = kb
	}
	return c.call(ctx, "editMessageText", p, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

// SetCommands sets the command menu, for all private chats if chatID is 0.
func (c *Client) SetCommands(ctx context.Context, cmds []BotCommand, chatID int64) error {
	p := map[string]any{"commands": cmds}
	if chatID != 0 {
		p["scope"] = map[string]any{"type": "chat", "chat_id": chatID}
	}
	return c.call(ctx, "setMyCommands", p, nil)
}
