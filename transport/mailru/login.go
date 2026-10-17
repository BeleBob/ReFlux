package mailru

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Since 2026-10-15 Mail.ru opens a public document only for a signed-in
// account: the editor API answers an anonymous visitor 401
// FORBIDDEN/ANONYMOUSE/ACCESS ("public is available with auth only"), for
// viewing as well as editing. A signed-in browser sends the account's
// cookies, of which the API needs two: Mpop, the mail.ru login, and sdcs,
// the cloud's own session ("nosdc" without it). It also sends a CSRF token
// it takes from POST /api/v3/tokens/csrf (403 "token" without one).

// ErrLoginRequired: the document needs a signed-in mail.ru account and the
// transport has none, or its login has expired.
var ErrLoginRequired = errors.New("mailru: the document needs a signed-in mail.ru account")

// Cookies a signed-in session cannot do without.
const (
	loginCookie   = "Mpop"
	sessionCookie = "sdcs"
)

// cloudURL is where the account's cookies belong: the host of the editor
// API (cloud.mail.ru; a test server in tests).
func (t *MailruDocsTransport) cloudURL() *url.URL {
	u, err := url.Parse(t.apiURL)
	if err != nil || u.Host == "" {
		return &url.URL{Scheme: "https", Host: "cloud.mail.ru", Path: "/"}
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}
}

// loginPageURL is where a person signs in to give the transport cookies:
// mail.ru's login page, which returns to the document afterwards.
func loginPageURL(weblink string) string {
	return "https://account.mail.ru/login?page=" + url.QueryEscape("https://cloud.mail.ru/public/"+weblink)
}

// newJar is a jar holding values as cloud.mail.ru's cookies. A value Go's
// HTTP client cannot send as it is (mail.ru's mtrc holds quotes) is left
// out: the client would send it changed, and log so on every request.
func newJar(cloud *url.URL, values map[string]string) *cookiejar.Jar {
	jar, _ := cookiejar.New(nil)
	cookies := make([]*http.Cookie, 0, len(values))
	for k, v := range values {
		if !sendable(v) {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: k, Value: v, Path: "/"})
	}
	jar.SetCookies(cloud, cookies)
	return jar
}

// sendable: net/http sends v as a cookie value unchanged (RFC 6265
// cookie-octets, plus the space and comma it quotes).
func sendable(v string) bool {
	for i := 0; i < len(v); i++ {
		if b := v[i]; b < 0x20 || b >= 0x7f || b == '"' || b == ';' || b == '\\' {
			return false
		}
	}
	return true
}

// jarValues are the cookies jar sends to cloud.mail.ru.
func jarValues(cloud *url.URL, jar http.CookieJar) map[string]string {
	out := make(map[string]string)
	if jar == nil {
		return out
	}
	for _, c := range jar.Cookies(cloud) {
		out[c.Name] = c.Value
	}
	return out
}

// accountEmail is the address the Mpop cookie names ("time:hash:email:"),
// "" when it names none.
func accountEmail(mpop string) string {
	parts := strings.Split(mpop, ":")
	if len(parts) > 2 && strings.Contains(parts[2], "@") {
		return parts[2]
	}
	return ""
}

// signIn returns what the editor API request carries for the account in
// jar: the x-email field and the CSRF token. A jar without a login opens
// the document anonymously ("anonym", no token), which Mail.ru refuses
// now; that refusal is what asks for a login.
func (t *MailruDocsTransport) signIn(client *http.Client) (email, csrf string, err error) {
	values := jarValues(t.cloudURL(), client.Jar)
	mpop, ok := values[loginCookie]
	if !ok {
		return "anonym", "", nil
	}
	email = accountEmail(mpop)
	if email == "" {
		email = "anonym" // the API takes the account from the cookies
	}
	t.jarMu.RLock()
	csrf = t.csrf
	t.jarMu.RUnlock()
	if csrf != "" {
		return email, csrf, nil
	}
	if csrf, err = fetchCSRF(client, t.cloudURL()); err != nil {
		return "", "", err
	}
	t.jarMu.Lock()
	t.csrf = csrf
	t.jarMu.Unlock()
	return email, csrf, nil
}

// fetchCSRF asks the cloud for the CSRF token its API wants from a
// signed-in account.
func fetchCSRF(client *http.Client, cloud *url.URL) (string, error) {
	req, _ := http.NewRequest("POST", cloud.Scheme+"://"+cloud.Host+"/api/v3/tokens/csrf", nil)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", mailruUserAgent)
	req.Header.Set("Origin", "https://cloud.mail.ru")
	req.Header.Set("Referer", "https://cloud.mail.ru/home/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mailru: CSRF token: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusOK && body.Token != "":
		return body.Token, nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		// "nosdc": the cloud session is missing or over; or the login is.
		return "", fmt.Errorf("%w (CSRF token: %w)", ErrLoginRequired, &StatusError{Code: resp.StatusCode, Reason: body.Error})
	}
	return "", fmt.Errorf("mailru: CSRF token: %w", &StatusError{Code: resp.StatusCode, Reason: body.Error})
}

// refused turns the editor API's refusal into the error connect sees: a
// refusal for want of a login is ErrLoginRequired (and still the
// StatusError), and a refused CSRF token is forgotten so the next attempt
// takes a new one.
func (t *MailruDocsTransport) refused(se *StatusError) error {
	switch {
	case se.Code == http.StatusUnauthorized || strings.HasPrefix(se.Reason, "FORBIDDEN/ANONYM"):
		return fmt.Errorf("%w: %w", ErrLoginRequired, se)
	case se.Code == http.StatusForbidden && (se.Reason == "token" || se.Reason == "nosdc"):
		t.jarMu.Lock()
		t.csrf = ""
		t.jarMu.Unlock()
		if se.Reason == "nosdc" {
			return fmt.Errorf("%w: %w", ErrLoginRequired, se)
		}
	}
	return se
}

// SetErrorNotifier implements transport.ErrorNotifier: a document that
// needs a login asks for one (reason "login") with the page to sign in on.
// Called once by the manager.
func (t *MailruDocsTransport) SetErrorNotifier(fn func(err error, transportName, url, html, reason string)) {
	t.errMu.Lock()
	t.errNotifier = fn
	t.errMu.Unlock()
}

// askLogin reports that the document needs a login: in the log once, and
// to the notifier until cookies arrive (ApplyCookies), at most once a
// minute.
func (t *MailruDocsTransport) askLogin(err error) {
	if utils.Throttled("m-docs.login."+t.weblink, time.Hour) {
		utils.Infof("[M-DOCS] mail.ru opens the document only for a signed-in account: %v. "+
			"Give the exit a cookies file (--mailru-cookies-file) or sign in in the app", err)
	}
	t.errMu.Lock()
	fn := t.errNotifier
	due := time.Since(t.loginAsked) >= time.Minute
	if fn != nil && due {
		t.loginAsked = time.Now()
	}
	t.errMu.Unlock()
	if fn != nil && due {
		fn(err, "mailru", loginPageURL(t.weblink), "", "login")
	}
}

// LoadCookieFile signs the transport in with the account a cookies file
// holds; they win over cookies applied later (a saved jar, a peer's
// offer), which may be older. Accepted formats: a Netscape cookies.txt,
// a Cookie header ("a=1; b=2", "Cookie:" optional), a JSON object of
// names to values, or the "name<TAB>value" lines a browser's developer
// tools copy. Cookie values never appear in its errors.
func (t *MailruDocsTransport) LoadCookieFile(path string) error {
	values, err := readCookieFile(path)
	if err != nil {
		return err
	}
	for _, name := range []string{loginCookie, sessionCookie} {
		if values[name] == "" {
			return fmt.Errorf("mail.ru cookie file %s has no %s cookie: export the cookies of cloud.mail.ru while signed in", path, name)
		}
	}
	t.jarMu.Lock()
	t.fileCookies = values
	t.cookieJar = newJar(t.cloudURL(), values)
	t.csrf = ""
	t.jarMu.Unlock()
	utils.Infof("[M-DOCS] signed in with the mail.ru account from %s", path)
	return nil
}

func readCookieFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open mail.ru cookie file: %w", err)
	}
	values, err := parseCookies(string(raw))
	if err != nil {
		return nil, fmt.Errorf("mail.ru cookie file %s: %w", path, err)
	}
	return values, nil
}

// parseCookies reads the cookies a browser sends to cloud.mail.ru from
// any of LoadCookieFile's formats.
func parseCookies(text string) (map[string]string, error) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return nil, errors.New("empty")
	}
	if strings.HasPrefix(text, "{") {
		return parseCookieJSON(text)
	}
	if strings.Contains(text, "\t") {
		return parseCookieLines(text)
	}
	text = strings.TrimSpace(strings.TrimPrefix(text, "Cookie:"))
	out := make(map[string]string)
	for _, part := range strings.Split(text, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name != "" {
			out[name] = value
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no cookies in it")
	}
	return out, nil
}

// parseCookieJSON reads {"name": "value"}, also wrapped in one object (a
// browser's "copy all": {"Request Cookies": {...}}).
func parseCookieJSON(text string) (map[string]string, error) {
	var top map[string]any
	if err := json.Unmarshal([]byte(text), &top); err != nil {
		return nil, errors.New("not valid JSON")
	}
	if len(top) == 1 {
		for _, v := range top {
			if inner, ok := v.(map[string]any); ok {
				top = inner
			}
		}
	}
	out := make(map[string]string)
	for k, v := range top {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no cookies in it")
	}
	return out, nil
}

// parseCookieLines reads a Netscape cookies.txt (seven tab-separated
// fields; only what a browser sends to cloud.mail.ru, unexpired) or
// "name<TAB>value" lines, the value possibly in quotes.
func parseCookieLines(text string) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 4096), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" || (strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_")) {
			continue
		}
		f := strings.Split(line, "\t")
		switch len(f) {
		case 7:
			domain := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(f[0], "#HttpOnly_"), "."))
			if domain != "cloud.mail.ru" && !(domain == "mail.ru" && f[1] == "TRUE") {
				continue
			}
			if exp, err := strconv.ParseInt(f[4], 10, 64); err != nil {
				return nil, fmt.Errorf("line %d: invalid expiry", n)
			} else if exp != 0 && exp <= time.Now().Unix() {
				continue
			}
			if f[5] != "" {
				out[f[5]] = f[6]
			}
		case 2:
			name := strings.TrimSpace(f[0])
			value := strings.TrimSpace(f[1])
			if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
				value = value[1 : len(value)-1]
			}
			if name != "" {
				out[name] = value
			}
		default:
			return nil, fmt.Errorf("line %d: expected a cookies.txt line (7 tab-separated fields) or name<TAB>value", n)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no unexpired cloud.mail.ru cookies in it")
	}
	return out, nil
}
