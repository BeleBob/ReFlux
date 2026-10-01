package main

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

// tgAPI is the Telegram Bot API; tests point it at a fake server.
var tgAPI = "https://api.telegram.org"

// telegram is a minimal Bot API client: what the ReFlux bot needs.
type telegram struct {
	token string
	http  *http.Client
}

func newTelegram(token string) *telegram {
	return &telegram{token: token, http: &http.Client{}}
}

type tgUser struct {
	ID           int64  `json:"id"`
	FirstName    string `json:"first_name"`
	Username     string `json:"username"`
	LanguageCode string `json:"language_code,omitempty"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgMessage struct {
	MessageID int64   `json:"message_id"`
	Date      int64   `json:"date"`
	From      *tgUser `json:"from"`
	Chat      tgChat  `json:"chat"`
	Text      string  `json:"text"`
}

// tgCallback is a press on an inline button.
type tgCallback struct {
	ID      string     `json:"id"`
	From    *tgUser    `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUpdate struct {
	UpdateID int64       `json:"update_id"`
	Message  *tgMessage  `json:"message"`
	Callback *tgCallback `json:"callback_query"`
}

// tgButton is an inline button; its data comes back in a tgCallback.
type tgButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// tgError is an error the API answered with.
type tgError struct {
	Code       int
	Desc       string
	RetryAfter time.Duration
}

func (e *tgError) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Desc) }

// call runs one API method. Errors never contain the token: the HTTP
// client puts the request URL, which holds it, into its errors.
func (t *telegram) call(method string, params any, timeout time.Duration, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", tgAPI+"/bot"+t.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return t.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return t.do(req, method, timeout, result)
}

// do sends an API request and decodes the answer into result.
func (t *telegram) do(req *http.Request, method string, timeout time.Duration, result any) error {
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
		return &tgError{Code: r.ErrorCode, Desc: r.Description, RetryAfter: time.Duration(r.Parameters.RetryAfter) * time.Second}
	}
	if result != nil {
		return json.Unmarshal(r.Result, result)
	}
	return nil
}

func (t *telegram) redact(err error) error {
	if t.token == "" || !strings.Contains(err.Error(), t.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "<token>"))
}

func (t *telegram) getMe() (tgUser, error) {
	var me tgUser
	err := t.call("getMe", struct{}{}, 20*time.Second, &me)
	return me, err
}

// getUpdates long-polls for new messages for up to wait.
func (t *telegram) getUpdates(offset int64, wait time.Duration) ([]tgUpdate, error) {
	var ups []tgUpdate
	err := t.call("getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(wait.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}, wait+20*time.Second, &ups)
	return ups, err
}

// send posts an HTML message to chat and returns its id. The Bot API
// takes up to 4096 characters; callers cut long text before marking it up
// (see pre).
func (t *telegram) send(chat int64, html string) (int64, error) {
	return t.sendKeyboard(chat, html, nil)
}

// sendKeyboard posts an HTML message with rows of inline buttons under it.
func (t *telegram) sendKeyboard(chat int64, html string, kb [][]tgButton) (int64, error) {
	params := map[string]any{
		"chat_id":                  chat,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if len(kb) > 0 {
		params["reply_markup"] = map[string]any{"inline_keyboard": kb}
	}
	var m tgMessage
	err := t.call("sendMessage", params, 20*time.Second, &m)
	return m.MessageID, err
}

// sendPhoto posts a PNG with an HTML caption (up to 1024 characters) and
// returns the message id.
func (t *telegram) sendPhoto(chat int64, png []byte, caption string) (int64, error) {
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
	req, err := http.NewRequest("POST", tgAPI+"/bot"+t.token+"/sendPhoto", &body)
	if err != nil {
		return 0, t.redact(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var m tgMessage
	err = t.do(req, "sendPhoto", 60*time.Second, &m)
	return m.MessageID, err
}

func (t *telegram) deleteMessage(chat, id int64) error {
	return t.call("deleteMessage", map[string]any{"chat_id": chat, "message_id": id}, 20*time.Second, nil)
}

// editKeyboard replaces a message's text and buttons.
func (t *telegram) editKeyboard(chat, id int64, html string, kb [][]tgButton) error {
	params := map[string]any{
		"chat_id": chat, "message_id": id, "text": html, "parse_mode": "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             map[string]any{"inline_keyboard": append([][]tgButton{}, kb...)},
	}
	return t.call("editMessageText", params, 20*time.Second, nil)
}

// answer acknowledges a button press, or the client shows a spinner.
func (t *telegram) answer(callbackID, text string) error {
	return t.call("answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text}, 20*time.Second, nil)
}

// setCommands fills the command menu of the chat input.
func (t *telegram) setCommands(cmds [][2]string) error {
	var list []map[string]string
	for _, c := range cmds {
		list = append(list, map[string]string{"command": c[0], "description": c[1]})
	}
	return t.call("setMyCommands", map[string]any{"commands": list}, 20*time.Second, nil)
}
