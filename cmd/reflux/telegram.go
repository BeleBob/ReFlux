package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgMessage struct {
	From *tgUser `json:"from"`
	Chat tgChat  `json:"chat"`
	Text string  `json:"text"`
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
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
		"allowed_updates": []string{"message"},
	}, wait+20*time.Second, &ups)
	return ups, err
}

// send posts an HTML message to chat. The Bot API takes up to 4096
// characters; callers cut long text before marking it up (see pre).
func (t *telegram) send(chat int64, html string) error {
	return t.call("sendMessage", map[string]any{
		"chat_id":                  chat,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}, 20*time.Second, nil)
}
