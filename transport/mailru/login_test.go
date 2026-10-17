package mailru

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

const testMpop = "1792230406:6d0e40:someone@mail.ru:"

// Every way a person gets the cookies out of a browser reads the same.
func TestParseCookies(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	past := time.Now().Add(-time.Hour).Unix()
	netscape := strings.Join([]string{
		"# Netscape HTTP Cookie File",
		".mail.ru\tTRUE\t/\tTRUE\t" + itoa(future) + "\tMpop\t" + testMpop,
		"#HttpOnly_cloud.mail.ru\tFALSE\t/\tTRUE\t0\tsdcs\tS1",
		".mail.ru\tTRUE\t/\tTRUE\t" + itoa(past) + "\told\tgone",
		"e.mail.ru\tFALSE\t/\tTRUE\t0\tmailonly\tx",
		"mail.ru\tFALSE\t/\tTRUE\t0\thostonly\tx",
		".yandex.ru\tTRUE\t/\tTRUE\t0\tSession_id\ty",
	}, "\n")
	for _, c := range []struct{ name, text string }{
		{"header", "Mpop=" + testMpop + "; sdcs=S1"},
		{"header with name", "Cookie: Mpop=" + testMpop + "; sdcs=S1\n"},
		{"json", `{"Mpop":"` + testMpop + `","sdcs":"S1"}`},
		{"devtools json", `{"Куки запроса":{"Mpop":"` + testMpop + `","sdcs":"S1"}}`},
		{"devtools lines", "Mpop\t\"" + testMpop + "\"\nsdcs\t\"S1\"\n"},
		{"netscape", netscape},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCookies(c.text)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"Mpop": testMpop, "sdcs": "S1"}
			if len(got) != len(want) || got["Mpop"] != want["Mpop"] || got["sdcs"] != want["sdcs"] {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
	for _, bad := range []string{"", "   ", "{}", "a\tb\tc"} {
		if _, err := parseCookies(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// cloudServer is the editor API of a cloud that, like Mail.ru's since
// 2026-10-15, opens documents for signed-in accounts only.
type cloudServer struct {
	*httptest.Server
	csrfCalls  atomic.Int32
	editCalls  atomic.Int32
	rejectNext atomic.Bool // answer the next edit with 403 "token"
	mu         sync.Mutex
	lastEdit   *http.Request
	lastBody   map[string]string
}

func newCloudServer(t *testing.T) *cloudServer {
	cs := &cloudServer{}
	signed := func(r *http.Request) bool {
		mpop, err1 := r.Cookie("Mpop")
		sdcs, err2 := r.Cookie("sdcs")
		return err1 == nil && err2 == nil && mpop.Value == testMpop && sdcs.Value == "S1"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/tokens/csrf", func(w http.ResponseWriter, r *http.Request) {
		cs.csrfCalls.Add(1)
		if !signed(r) {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "nosdc"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "CSRF1"})
	})
	mux.HandleFunc("/api/v4/r7/edit", func(w http.ResponseWriter, r *http.Request) {
		cs.editCalls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		cs.mu.Lock()
		cs.lastEdit, cs.lastBody = r, body
		cs.mu.Unlock()
		if !signed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "FORBIDDEN/ANONYMOUSE/ACCESS", "error_description": "public is available with auth only"})
			return
		}
		if r.Header.Get("X-CSRF-Token") != "CSRF1" || cs.rejectNext.Swap(false) {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "token"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token": "tok", "api": "http://api",
			"document": map[string]any{
				"key": "KEY1", "fileType": "docx", "url": "http://doc", "title": "t.docx",
				"permissions": map[string]any{"edit": true},
			},
			"editorConfig": map[string]any{"callbackUrl": "http://cb", "user": map[string]any{"id": "uid1"}},
		})
	})
	cs.Server = httptest.NewServer(mux)
	t.Cleanup(cs.Close)
	old := editAPIURL
	editAPIURL = cs.URL + "/api/v4/r7/edit"
	t.Cleanup(func() { editAPIURL = old })
	return cs
}

func writeCookies(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mailru.cookies")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Mail.ru answers an anonymous visitor 401 since 2026-10-15; a transport
// with an account's cookies opens the document as that account, with the
// CSRF token the cloud gives it, and takes the token once.
func TestFetchDocInfoSignedIn(t *testing.T) {
	cs := newCloudServer(t)
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	// mtrc is the value the browser copies with quotes in it.
	if err := tr.LoadCookieFile(writeCookies(t, "Mpop\t\""+testMpop+"\"\nsdcs\t\"S1\"\nt\t\"x\"\nmtrc\t\"{\"a\":1}\"\n")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		info, err := tr.fetchDocInfo(tr.weblink)
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		if info.EditorUserID != "uid1" {
			t.Fatalf("editor user = %q", info.EditorUserID)
		}
	}
	cs.mu.Lock()
	email, hdr := cs.lastBody["x-email"], cs.lastEdit.Header.Get("X-Email")
	_, mtrcErr := cs.lastEdit.Cookie("mtrc")
	cs.mu.Unlock()
	if mtrcErr == nil {
		t.Error("sent mtrc, whose value net/http cannot send unchanged")
	}
	if email != "someone@mail.ru" || hdr != "someone@mail.ru" {
		t.Errorf("x-email = %q, X-Email = %q, want the account's address", email, hdr)
	}
	if n := cs.csrfCalls.Load(); n != 1 {
		t.Errorf("CSRF token taken %d times for two opens, want once", n)
	}

	// A refused token is taken anew on the next attempt.
	cs.rejectNext.Store(true)
	if _, err := tr.fetchDocInfo(tr.weblink); err == nil || errors.Is(err, ErrLoginRequired) {
		t.Fatalf("refused token: err = %v, want a plain refusal", err)
	}
	if _, err := tr.fetchDocInfo(tr.weblink); err != nil {
		t.Fatalf("after a refused token: %v", err)
	}
	if n := cs.csrfCalls.Load(); n != 2 {
		t.Errorf("CSRF token taken %d times, want once more after the refusal", n)
	}
}

// Without an account the transport asks for one: the document's error is
// ErrLoginRequired (and still the API's StatusError, which the document
// check reads), and the notifier gets the page to sign in on.
func TestAnonymousAsksForLogin(t *testing.T) {
	newCloudServer(t)
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	_, err := tr.fetchDocInfo(tr.weblink)
	var se *StatusError
	if !errors.Is(err, ErrLoginRequired) || !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("err = %v, want ErrLoginRequired with the 401", err)
	}

	asked := make(chan [2]string, 4)
	tr.SetErrorNotifier(func(_ error, name, url, _, reason string) {
		asked <- [2]string{url, reason}
		_ = name
	})
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()
	select {
	case a := <-asked:
		if a[1] != "login" || !strings.HasPrefix(a[0], "https://account.mail.ru/login?") || !strings.Contains(a[0], "AbCdEfGh1%2FIjKlMnOp2") {
			t.Fatalf("asked %v, want a login on mail.ru's page that returns to the document", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no login asked")
	}
	// Retries do not ask again within the minute.
	select {
	case a := <-asked:
		t.Fatalf("asked again at once: %v", a)
	case <-time.After(1500 * time.Millisecond):
	}
}

// The account of a cookies file stays when other cookies are applied (a
// saved jar, a peer's offer): they may be older or someone else's.
func TestCookieFileWinsOverApplied(t *testing.T) {
	newCloudServer(t)
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	if err := tr.LoadCookieFile(writeCookies(t, "Mpop="+testMpop+"; sdcs=S1")); err != nil {
		t.Fatal(err)
	}
	if err := tr.ApplyCookies(map[string]string{"Mpop": "1:2:other@mail.ru:", "sdcs": "OLD", "extra": "e"}); err != nil {
		t.Fatal(err)
	}
	jar, _ := tr.FetchCookies()
	if jar["Mpop"] != testMpop || jar["sdcs"] != "S1" || jar["extra"] != "e" {
		t.Fatalf("jar = %v, want the file's login plus the extra cookie", jar)
	}
	if _, err := tr.fetchDocInfo(tr.weblink); err != nil {
		t.Fatalf("signed in after ApplyCookies: %v", err)
	}
}

// A file without the login says what is missing and never what is in it.
func TestLoadCookieFileNeedsLogin(t *testing.T) {
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	err := tr.LoadCookieFile(writeCookies(t, "Mpop=SECRETVALUE; t=x"))
	if err == nil || !strings.Contains(err.Error(), "sdcs") {
		t.Fatalf("err = %v, want the missing sdcs named", err)
	}
	if strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("the error shows a cookie value: %v", err)
	}
	if err := tr.LoadCookieFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing file loaded")
	}
}
