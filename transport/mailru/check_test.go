package mailru

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A document is dead for good only when the editor API says it or its
// owner's account is gone or blocked, or the link cannot edit it. A bad
// moment of the service is not a verdict: on 2026-10-13 the API answered
// 408 to a live document, and a check that hit it twice would have taken
// the client's working document away.
func TestCheckDocument(t *testing.T) {
	refuse := func(status int, reason string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			if reason != "" {
				json.NewEncoder(w).Encode(map[string]string{"error": reason, "error_description": "test"})
			}
		}
	}
	answer := func(status int, edit bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"token": "tok",
				"api":   "http://api",
				"document": map[string]any{
					"key": "KEY1", "fileType": "docx", "url": "http://doc", "title": "t.docx",
					"permissions": map[string]any{"edit": edit},
				},
				"editorConfig": map[string]any{"callbackUrl": "http://cb", "user": map[string]any{"id": "anon1"}},
			})
		}
	}
	for _, c := range []struct {
		name   string
		h      http.HandlerFunc
		dead   bool
		hasErr bool
	}{
		{"alive", answer(http.StatusOK, true), false, false},
		{"deleted", answer(http.StatusNotFound, true), true, true},
		{"link not found", refuse(http.StatusBadRequest, "BAD/WEBLINK/URL"), true, true},
		{"gone", refuse(http.StatusGone, ""), true, true},
		{"withheld", refuse(http.StatusUnavailableForLegalReasons, ""), true, true},
		{"blocked", refuse(http.StatusForbidden, "USER/BLOCKED"), true, true},
		{"read-only link", answer(http.StatusOK, false), true, true},
		{"bad request", refuse(http.StatusBadRequest, "BAD/REQUEST"), false, true},
		{"forbidden without a reason", refuse(http.StatusForbidden, ""), false, true},
		{"timeout", refuse(http.StatusRequestTimeout, ""), false, true},
		{"conflict", refuse(http.StatusConflict, ""), false, true},
		{"service trouble", answer(http.StatusBadGateway, true), false, true},
		{"gateway timeout", refuse(http.StatusGatewayTimeout, ""), false, true},
		{"rate limited", answer(http.StatusTooManyRequests, true), false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.h)
			defer srv.Close()
			defer func(u string) { editAPIURL = u }(editAPIURL)
			editAPIURL = srv.URL

			dead, err := CheckDocument("AbCdEfGh1/IjKlMnOp2")
			if dead != c.dead || (err != nil) != c.hasErr {
				t.Fatalf("dead=%v err=%v, want dead=%v error=%v", dead, err, c.dead, c.hasErr)
			}
			if c.name == "read-only link" && !errors.Is(err, ErrReadOnly) {
				t.Errorf("err = %v, want ErrReadOnly", err)
			}
			// The reason is what the owner sees in the dead list.
			if c.name == "link not found" && (err == nil || !strings.Contains(err.Error(), "BAD/WEBLINK/URL")) {
				t.Errorf("err = %v, want the API's reason in it", err)
			}
		})
	}

	// Unreachable: not dead either.
	defer func(u string) { editAPIURL = u }(editAPIURL)
	editAPIURL = "http://127.0.0.1:1/edit"
	if dead, err := CheckDocument("AbCdEfGh1/IjKlMnOp2"); dead || err == nil {
		t.Errorf("unreachable API: dead=%v err=%v", dead, err)
	}
}
