// Package telegram is a minimal Telegram Bot API client: what the ReFlux
// bots need.
package telegram

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// API is the Telegram Bot API; tests point it at a fake server.
var API = "https://api.telegram.org"

// Client is a minimal Bot API client: what the ReFlux bot needs.
type Client struct {
	token string
	http  *http.Client
}

// New returns a client for the bot with this token.
func New(token string) *Client {
	return &Client{token: token, http: &http.Client{}}
}

type User struct {
	ID           int64  `json:"id"`
	FirstName    string `json:"first_name"`
	Username     string `json:"username"`
	LanguageCode string `json:"language_code,omitempty"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

// Callback is a press on an inline button.
type Callback struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Update struct {
	UpdateID int64     `json:"update_id"`
	Message  *Message  `json:"message"`
	Callback *Callback `json:"callback_query"`
}

// Button is an inline button; its data comes back in a tgCallback.
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// Error is an error the API answered with.
type Error struct {
	Code       int
	Desc       string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Desc) }

// call runs one API method. Errors never contain the token: the HTTP
// client puts the request URL, which holds it, into its errors.
func (t *Client) call(method string, params any, timeout time.Duration, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", API+"/bot"+t.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return t.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return t.do(req, method, timeout, result)
}

// do sends an API request and decodes the answer into result.
func (t *Client) do(req *http.Request, method string, timeout time.Duration, result any) error {
	client := *t.http
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		return t.redact(err)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: http %d: %w", method, resp.StatusCode, t.redact(err))
	}
	if !r.OK {
		return &Error{Code: r.ErrorCode, Desc: r.Description, RetryAfter: time.Duration(r.Parameters.RetryAfter) * time.Second}
	}
	if result != nil {
		return json.Unmarshal(r.Result, result)
	}
	return nil
}

func (t *Client) redact(err error) error {
	if t.token == "" || !strings.Contains(err.Error(), t.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "<token>"))
}

func (t *Client) GetMe() (User, error) {
	var me User
	err := t.call("getMe", struct{}{}, 20*time.Second, &me)
	return me, err
}

// GetUpdates long-polls for new messages for up to wait.
func (t *Client) GetUpdates(offset int64, wait time.Duration) ([]Update, error) {
	var ups []Update
	err := t.call("getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(wait.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}, wait+20*time.Second, &ups)
	return ups, err
}

// Send posts an HTML message to chat and returns its id. The Bot API
// takes up to 4096 characters; callers cut long text before marking it up
// (see pre).
func (t *Client) Send(chat int64, html string) (int64, error) {
	return t.SendKeyboard(chat, html, nil)
}

// SendKeyboard posts an HTML message with rows of inline buttons under it.
func (t *Client) SendKeyboard(chat int64, html string, kb [][]Button) (int64, error) {
	params := map[string]any{
		"chat_id":                  chat,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if len(kb) > 0 {
		params["reply_markup"] = map[string]any{"inline_keyboard": kb}
	}
	var m Message
	err := t.call("sendMessage", params, 20*time.Second, &m)
	return m.MessageID, err
}

// SendPhoto posts a PNG with an HTML caption (up to 1024 characters) and
// returns the message id.
func (t *Client) SendPhoto(chat int64, png []byte, caption string) (int64, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("chat_id", strconv.FormatInt(chat, 10))
	w.WriteField("caption", caption)
	w.WriteField("parse_mode", "HTML")
	part, err := w.CreateFormFile("photo", "qr.png")
	if err != nil {
		return 0, err
	}
	part.Write(png)
	if err := w.Close(); err != nil {
		return 0, err
	}
	req, err := http.NewRequest("POST", API+"/bot"+t.token+"/sendPhoto", &body)
	if err != nil {
		return 0, t.redact(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var m Message
	err = t.do(req, "sendPhoto", 60*time.Second, &m)
	return m.MessageID, err
}

func (t *Client) DeleteMessage(chat, id int64) error {
	return t.call("deleteMessage", map[string]any{"chat_id": chat, "message_id": id}, 20*time.Second, nil)
}

// EditKeyboard replaces a message's text and buttons.
func (t *Client) EditKeyboard(chat, id int64, html string, kb [][]Button) error {
	params := map[string]any{
		"chat_id": chat, "message_id": id, "text": html, "parse_mode": "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             map[string]any{"inline_keyboard": append([][]Button{}, kb...)},
	}
	return t.call("editMessageText", params, 20*time.Second, nil)
}

// Answer acknowledges a button press, or the client shows a spinner.
func (t *Client) Answer(callbackID, text string) error {
	return t.call("answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text}, 20*time.Second, nil)
}

// SetCommands fills the command menu of the chat input.
func (t *Client) SetCommands(cmds [][2]string) error {
	var list []map[string]string
	for _, c := range cmds {
		list = append(list, map[string]string{"command": c[0], "description": c[1]})
	}
	return t.call("setMyCommands", map[string]any{"commands": list}, 20*time.Second, nil)
}
