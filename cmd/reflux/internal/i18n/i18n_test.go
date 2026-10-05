package i18n

import (
	"regexp"
	"strings"
	"testing"
)

var verbRe = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

// Both languages of a message take the same arguments.
func TestMessagesHaveBothLanguages(t *testing.T) {
	for id, m := range Messages {
		if m[0] == "" || m[1] == "" {
			t.Errorf("%s: missing a language: %q", id, m)
			continue
		}
		en, ru := verbRe.FindAllString(strings.ReplaceAll(m[0], "%%", ""), -1), verbRe.FindAllString(strings.ReplaceAll(m[1], "%%", ""), -1)
		if strings.Join(en, " ") != strings.Join(ru, " ") {
			t.Errorf("%s: arguments differ: en %v, ru %v", id, en, ru)
		}
	}
}
