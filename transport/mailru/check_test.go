package mailru

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A document is dead for good when the editor API refuses it or the link
// cannot edit it; a bad moment of the service is not a verdict.
func TestCheckDocument(t *testing.T) {
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
		{"bad link", answer(http.StatusBadRequest, true), true, true},
		{"read-only link", answer(http.StatusOK, false), true, true},
		{"service trouble", answer(http.StatusBadGateway, true), false, true},
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
		})
	}

	// Unreachable: not dead either.
	defer func(u string) { editAPIURL = u }(editAPIURL)
	editAPIURL = "http://127.0.0.1:1/edit"
	if dead, err := CheckDocument("AbCdEfGh1/IjKlMnOp2"); dead || err == nil {
		t.Errorf("unreachable API: dead=%v err=%v", dead, err)
	}
}
