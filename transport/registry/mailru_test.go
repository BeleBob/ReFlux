package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// A node may start before its owner gives it a mail.ru login: a cookies
// file that is not there yet is read once it appears (the transport opens
// documents without a login meanwhile), while a file without a login is a
// mistake the operator hears about at once.
func TestMailruCookiesFile(t *testing.T) {
	dir := t.TempDir()
	o := Options{Base: transport.DefaultConfig(), MailruCookiesFile: filepath.Join(dir, "missing")}
	if _, err := New("mailru", "AbCdEfGh1/IjKlMnOp2", nil, o); err != nil {
		t.Fatalf("missing cookies file: %v", err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("t=x"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.MailruCookiesFile = bad
	if _, err := New("mailru", "AbCdEfGh1/IjKlMnOp2", nil, o); err == nil {
		t.Fatal("a cookies file without a login was accepted")
	}
}
