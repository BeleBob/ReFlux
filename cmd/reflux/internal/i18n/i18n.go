// Package i18n holds the texts of the ReFlux bots, panel and CLI in
// English and Russian, and renders them.
package i18n

import "fmt"

// Lang is a language of the texts. The CLI speaks English.
type Lang string

const (
	EN Lang = "en"
	RU Lang = "ru"
)

// Phrase is a message used as an argument of another one, translated
// along with it: "node %s: %s" with the access state as a Phrase.
type Phrase struct {
	ID   string
	Args []any
}

// P is a phrase: message id with its arguments.
func P(id string, args ...any) Phrase { return Phrase{id, args} }

// T renders message id in l, English when l has no text for it.
func T(l Lang, id string, args ...any) string {
	m, ok := Messages[id]
	if !ok {
		return id
	}
	format := m[0]
	if l == RU && m[1] != "" {
		format = m[1]
	}
	out := make([]any, len(args))
	for i, a := range args {
		if p, ok := a.(Phrase); ok {
			a = T(l, p.ID, p.Args...)
		}
		out[i] = a
	}
	return fmt.Sprintf(format, out...)
}
