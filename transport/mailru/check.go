package mailru

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

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

// CheckDocument opens a public document the way the transport does, as an
// anonymous visitor, without joining its co-authoring session. dead is true
// when the document cannot carry a tunnel at all: the editor API says it or
// its owner's account is gone or blocked (StatusError.blocked), or the link
// may not edit it. Other errors (the network, the service having a bad
// moment, a timeout) leave dead false.
func CheckDocument(weblink string) (dead bool, err error) {
	t := NewMailruDocsTransport(weblink, transport.DefaultConfig())
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
