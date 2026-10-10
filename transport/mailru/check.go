package mailru

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// StatusError is the editor API answering a document with an HTTP error.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("API returned status %d", e.Code) }

// ErrReadOnly: the link opens the document for viewing only.
var ErrReadOnly = errors.New("the link opens the document read-only")

// CheckDocument opens a public document the way the transport does, as an
// anonymous visitor, without joining its co-authoring session. dead is true
// when the document cannot carry a tunnel at all: the editor API refuses it
// (deleted, unpublished, a broken link) or the link may not edit it. Other
// errors (the network, the service having a bad moment) leave dead false.
func CheckDocument(weblink string) (dead bool, err error) {
	t := NewMailruDocsTransport(weblink, transport.DefaultConfig())
	info, err := t.fetchDocInfo(t.weblink)
	var se *StatusError
	switch {
	case errors.As(err, &se):
		return se.Code >= 400 && se.Code < 500 && se.Code != http.StatusTooManyRequests, err
	case err != nil:
		return false, err
	case !canEdit(info.Permissions):
		return true, ErrReadOnly
	}
	return false, nil
}
