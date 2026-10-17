package mailru

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// StatusError is the editor API answering a document with an HTTP error.
// Reason is the error code in its body ("BAD/WEBLINK/URL"), when it has one.
type StatusError struct {
	Code   int
	Reason string
}

func (e *StatusError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("API returned status %d (%s)", e.Code, e.Reason)
	}
	return fmt.Sprintf("API returned status %d", e.Code)
}

// blocked says whether the editor API's error means the document or its
// owner's account is gone or blocked for good: the link is not found
// (deleted, unpublished, blocked: 400 BAD/WEBLINK/URL, as checked on
// 2026-10-13), gone or withheld (404, 410, 451), or refused with a block
// named in the reason. Anything else, timeouts (408) and refusals without a
// reason among them, may pass and says nothing about the document.
func (e *StatusError) blocked() bool {
	switch e.Code {
	case http.StatusNotFound, http.StatusGone, http.StatusUnavailableForLegalReasons:
		return true
	case http.StatusBadRequest:
		return e.Reason == "BAD/WEBLINK/URL"
	case http.StatusForbidden:
		return strings.Contains(strings.ToUpper(e.Reason), "BLOCK")
	}
	return false
}

// ErrReadOnly: the link opens the document for viewing only.
var ErrReadOnly = errors.New("the link opens the document read-only")

// CheckDocument opens a public document the way the transport does, as the
// account cookies hold (nil: anonymously, which Mail.ru refuses since
// 2026-10-15), without joining its co-authoring session. dead is true when
// the document cannot carry a tunnel at all: the editor API says it or its
// owner's account is gone or blocked (StatusError.blocked), or the link may
// not edit it. Other errors (the network, the service having a bad moment,
// a timeout, a login it does not take) leave dead false.
func CheckDocument(weblink string, cookies map[string]string) (dead bool, err error) {
	t := NewMailruDocsTransport(weblink, transport.DefaultConfig())
	if len(cookies) > 0 {
		t.cookieJar = newJar(t.cloudURL(), cookies)
	}
	info, err := t.fetchDocInfo(t.weblink)
	var se *StatusError
	switch {
	case errors.As(err, &se):
		return se.blocked(), err
	case err != nil:
		return false, err
	case !canEdit(info.Permissions):
		return true, ErrReadOnly
	}
	return false, nil
}

// CheckLogin says whether Mail.ru takes the account cookies hold: it asks
// the cloud for the CSRF token the transport takes before it opens a
// document. account is the address the login names ("" when it names
// none). A login that is missing or over is ErrLoginRequired.
func CheckLogin(cookies map[string]string) (account string, err error) {
	if err := LoginCookies(cookies); err != nil {
		return "", fmt.Errorf("%w: %v", ErrLoginRequired, err)
	}
	t := NewMailruDocsTransport("", transport.DefaultConfig())
	client := &http.Client{Jar: newJar(t.cloudURL(), cookies), Timeout: 15 * time.Second}
	if _, err := fetchCSRF(client, t.cloudURL()); err != nil {
		return "", err
	}
	return accountEmail(cookies[loginCookie]), nil
}
