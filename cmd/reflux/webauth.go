package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The web panel has no password: the owner signs in with a one-time link
// the bot (/web) or `reflux web login` hands out, and the browser keeps a
// session cookie. Only hashes of the tokens are kept on disk.

const (
	loginFor   = 10 * time.Minute    // a login link is good for this long
	sessionFor = 30 * 24 * time.Hour // a signed-in browser stays in
)

// webConfig is web.json in the data directory.
type webConfig struct {
	// Listen is the address the panel serves on: the host's LAN address,
	// so it is not reachable through its VPN or Docker networks.
	Listen string `json:"listen"`
	// Trusted are LAN addresses signed in without a link: the owner's own
	// computers. Anyone who takes such an address is signed in too, so
	// they should be reserved for those devices on the router.
	Trusted []string `json:"trusted,omitempty"`
}

func (s Store) webConfigPath() string { return filepath.Join(s.Root, "web.json") }

func (s Store) loadWebConfig() (webConfig, error) {
	var c webConfig
	b, err := os.ReadFile(s.webConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, errors.New("the web panel is not installed: reflux web install --listen <LAN-IP>:8686")
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", s.webConfigPath(), err)
	}
	return c, nil
}

func (s Store) saveWebConfig(c webConfig) error {
	return writeJSON(s.webConfigPath(), c)
}

// writeJSON writes v to path (0600) through a temporary file.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func tokenHash(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// expiring is a set of token hashes and when each expires, kept in a file.
type expiring map[string]time.Time

func (s Store) tokensPath(kind string) string { return filepath.Join(s.Root, "web-"+kind+".json") }

// withTokens runs fn on the token file of kind under a lock, and writes it
// back without the expired ones.
func (s Store) withTokens(kind string, fn func(expiring)) error {
	unlock, err := lockFile(s.tokensPath(kind)+".lock", 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	set := expiring{}
	if b, err := os.ReadFile(s.tokensPath(kind)); err == nil {
		json.Unmarshal(b, &set)
	}
	fn(set)
	now := time.Now()
	for h, until := range set {
		if !now.Before(until) {
			delete(set, h)
		}
	}
	return writeJSON(s.tokensPath(kind), set)
}

// loginLink makes a one-time sign-in link for the web panel.
func (s Store) loginLink() (string, error) {
	c, err := s.loadWebConfig()
	if err != nil {
		return "", err
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	err = s.withTokens("login", func(set expiring) { set[tokenHash(token)] = time.Now().Add(loginFor) })
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(host, port) + "/login?t=" + token, nil
}

// useLogin spends a login token: true once, while it is fresh.
func (s Store) useLogin(token string) bool {
	ok := false
	h := tokenHash(token)
	s.withTokens("login", func(set expiring) {
		if until, found := set[h]; found && time.Now().Before(until) {
			ok = true
		}
		delete(set, h)
	})
	return ok
}

// newSession starts a signed-in browser session and returns its cookie
// value.
func (s Store) newSession() (string, error) {
	id, err := randomToken()
	if err != nil {
		return "", err
	}
	return id, s.withTokens("sessions", func(set expiring) { set[tokenHash(id)] = time.Now().Add(sessionFor) })
}

func (s Store) validSession(id string) bool {
	if id == "" {
		return false
	}
	ok := false
	s.withTokens("sessions", func(set expiring) {
		until, found := set[tokenHash(id)]
		ok = found && time.Now().Before(until)
	})
	return ok
}

func (s Store) endSession(id string) error {
	return s.withTokens("sessions", func(set expiring) { delete(set, tokenHash(id)) })
}
