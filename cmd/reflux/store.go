package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Store is the reflux data directory:
//
//	clients/<name>/client.json  what the channel is (transport, document URL)
//	clients/<name>/key          the channel's encryption secret (0600)
//	clients/<name>/node.conf    the exit node's config, rendered from client.json
//	state/<name>/               the node's writable state (cookie store)
//	egress/                     the egress tunnels' configs (see the egress image)
//	revoked/<name>-<time>/      client.json of revoked channels, without the key
//	compose.yml                 rendered from all of the above
type Store struct {
	Root string
}

// Client is one channel: one client, one document, one key, one exit node.
type Client struct {
	Name      string    `json:"name"`
	Transport string    `json:"transport"`
	URL       string    `json:"url"`
	Created   time.Time `json:"created"`
	// Paused and Expires switch access off without revoking it: the node
	// is not run, while the key, the document and the state are kept.
	Paused  bool      `json:"paused,omitempty"`
	Expires time.Time `json:"expires,omitzero"`
}

// Transports a channel can use. vyandex needs a Yandex login (a cookies
// file) and cupsonline creates its rooms at start: both need more than a
// URL, so they are left out for now.
var transports = map[string]bool{"mailru": true, "yandex": true, "boards": true}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

func validName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("bad client name %q: use 1-32 lowercase letters, digits and dashes", name)
	}
	return nil
}

// validURL rejects what the core's .conf parser would cut or misread: it
// treats '#' and ';' as the start of a comment and a line break as the end
// of the value, so such a URL would silently change (or add a key).
func validURL(u string) error {
	if u == "" {
		return errors.New("empty document URL")
	}
	if strings.ContainsAny(u, "#; \t\r\n") {
		return fmt.Errorf("document URL must not contain '#', ';' or whitespace: %q", u)
	}
	for _, r := range u {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("document URL contains a control character: %q", u)
		}
	}
	return nil
}

func (s Store) clientDir(name string) string { return filepath.Join(s.Root, "clients", name) }
func (s Store) stateDir(name string) string  { return filepath.Join(s.Root, "state", name) }
func (s Store) keyPath(name string) string   { return filepath.Join(s.clientDir(name), "key") }

// Init creates the data directory layout. It is safe to run again.
func (s Store) Init() error {
	dirs := []string{s.Root}
	for _, d := range []string{"clients", "state", "egress", "revoked"} {
		dirs = append(dirs, filepath.Join(s.Root, d))
	}
	for _, p := range dirs {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Add creates a channel with a fresh key and returns it.
func (s Store) Add(name, transport, url string) (Client, error) {
	if err := validName(name); err != nil {
		return Client{}, err
	}
	if !transports[transport] {
		return Client{}, fmt.Errorf("unsupported transport %q (supported: %s)", transport, strings.Join(supportedTransports(), ", "))
	}
	if err := validURL(url); err != nil {
		return Client{}, err
	}
	if err := s.Init(); err != nil {
		return Client{}, err
	}
	dir := s.clientDir(name)
	// Mkdir, not MkdirAll: an existing channel must never be overwritten.
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Client{}, fmt.Errorf("client %q already exists", name)
		}
		return Client{}, err
	}
	c := Client{Name: name, Transport: transport, URL: url, Created: time.Now().UTC().Truncate(time.Second)}
	key, err := newKey()
	if err != nil {
		os.RemoveAll(dir)
		return Client{}, err
	}
	if err := s.write(c, key); err != nil {
		os.RemoveAll(dir)
		return Client{}, err
	}
	if err := os.MkdirAll(s.stateDir(name), 0o700); err != nil {
		return Client{}, err
	}
	return c, nil
}

// newKey returns a channel secret: 32 random bytes as 64 hex characters,
// the same form the node wizard's node-install.sh expects.
func newKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s Store) write(c Client, key string) error {
	meta, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := s.clientDir(c.Name)
	files := map[string][]byte{
		"client.json": append(meta, '\n'),
		"key":         []byte(key),
		"node.conf":   []byte(nodeConf(c)),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// SyncConf rewrites c's node.conf when it differs from what this reflux
// renders, as after an update that adds a setting. A running node read its
// config at start: apply recreates the nodes whose config is newer.
func (s Store) SyncConf(c Client) error {
	path := filepath.Join(s.clientDir(c.Name), "node.conf")
	want := []byte(nodeConf(c))
	have, err := os.ReadFile(path)
	if err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Save rewrites an existing channel's client.json; the key stays.
func (s Store) Save(c Client) error {
	if _, err := s.Get(c.Name); err != nil {
		return err
	}
	meta, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.clientDir(c.Name), "client.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(meta, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// errBusy: another reflux command holds the data directory.
var errBusy = errors.New("another reflux command is changing the clients; try again in a moment")

// Lock serializes the commands that change the data directory or the
// containers: the CLI, heal from cron and the bot may run at once.
func (s Store) Lock(wait time.Duration) (func(), error) {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return nil, err
	}
	return lockFile(filepath.Join(s.Root, ".lock"), wait)
}

// Get reads one channel.
func (s Store) Get(name string) (Client, error) {
	if err := validName(name); err != nil {
		return Client{}, err
	}
	b, err := os.ReadFile(filepath.Join(s.clientDir(name), "client.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Client{}, fmt.Errorf("no client %q", name)
	}
	if err != nil {
		return Client{}, err
	}
	var c Client
	if err := json.Unmarshal(b, &c); err != nil {
		return Client{}, fmt.Errorf("client %q: %w", name, err)
	}
	return c, nil
}

// Key reads a channel's secret.
func (s Store) Key(name string) (string, error) {
	b, err := os.ReadFile(s.keyPath(name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// List returns every channel, sorted by name.
func (s Store) List() ([]Client, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "clients"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Client
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := s.Get(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Revoke deletes a channel's key, config and state and keeps its
// client.json under revoked/ as a record. The caller stops the node first.
func (s Store) Revoke(name string, now time.Time) (string, error) {
	if _, err := s.Get(name); err != nil {
		return "", err
	}
	dir := s.clientDir(name)
	for _, f := range []string{"key", "node.conf"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if err := os.RemoveAll(s.stateDir(name)); err != nil {
		return "", err
	}
	dst := filepath.Join(s.Root, "revoked", name+"-"+now.UTC().Format("20060102-150405"))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(dir, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func supportedTransports() []string {
	var out []string
	for t := range transports {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
