package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Access requests: people who asked the client bot for a channel, or for
// more time on theirs. Each waits in requests/<telegram id>.json until the
// owner approves (a client is made, or its access extended), rejects or
// blocks it — from the owner bot, the panel or the CLI. An approved or
// rejected request stays until the client bot has told the person; a
// blocked one stays for good.

const (
	maxPending   = 20             // requests waiting at once
	askAgainFrom = 24 * time.Hour // after a rejection
)

// Request states and kinds.
const (
	reqPending  = "pending"
	reqApproved = "approved"
	reqRejected = "rejected"
	reqBlocked  = "blocked"
	reqAccess   = "access" // a new channel
	reqExtend   = "extend" // more time for the channel the person has
)

type accessRequest struct {
	ID       int64     `json:"id"` // the Telegram account
	Username string    `json:"username,omitempty"`
	Name     string    `json:"name,omitempty"`
	Lang     string    `json:"lang,omitempty"` // the person's, for the client bot
	Kind     string    `json:"kind"`
	Text     string    `json:"text,omitempty"` // what they wrote
	At       time.Time `json:"at"`
	State    string    `json:"state"`
	Client   string    `json:"client,omitempty"`   // the channel made or extended
	Notified bool      `json:"notified,omitempty"` // the owner heard of it
	Told     bool      `json:"told,omitempty"`     // the person heard of a rejection
	Decided  time.Time `json:"decided,omitzero"`
}

// Who is how the request names the person: the name and @username, or
// with no username the name and the id (a name alone may say nothing).
func (r accessRequest) Who() string {
	who := TGAccount{ID: r.ID, Username: r.Username, Name: r.Name}.String()
	if r.Username == "" && r.Name != "" {
		who += fmt.Sprintf(" (id %d)", r.ID)
	}
	return who
}

var (
	errBlocked    = errors.New("blocked")
	errHasChannel = errors.New("already has a channel")
	errWaiting    = errors.New("a request already waits")
	errTooSoon    = errors.New("rejected less than a day ago")
	errTooMany    = errors.New("too many requests wait")
)

func (s Store) requestsDir() string { return filepath.Join(s.Root, "requests") }
func (s Store) requestPath(id int64) string {
	return filepath.Join(s.requestsDir(), strconv.FormatInt(id, 10)+".json")
}
func (s Store) removeRequest(id int64) error { return ignoreMissing(os.Remove(s.requestPath(id))) }

func ignoreMissing(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s Store) getRequest(id int64) (accessRequest, error) {
	var r accessRequest
	b, err := os.ReadFile(s.requestPath(id))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

func (s Store) saveRequest(r accessRequest) error {
	if err := os.MkdirAll(s.requestsDir(), 0o700); err != nil {
		return err
	}
	return writeJSON(s.requestPath(r.ID), r)
}

// listRequests returns every request, the oldest first.
func (s Store) listRequests() ([]accessRequest, error) {
	paths, err := filepath.Glob(filepath.Join(s.requestsDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []accessRequest
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r accessRequest
		if json.Unmarshal(b, &r) == nil && r.ID != 0 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func (s Store) pendingRequests() []accessRequest {
	all, _ := s.listRequests()
	var out []accessRequest
	for _, r := range all {
		if r.State == reqPending {
			out = append(out, r)
		}
	}
	return out
}

// clientOf is the channel linked to a Telegram account.
func (s Store) clientOf(id int64) (Client, bool) {
	clients, _ := s.List()
	for _, c := range clients {
		if c.Telegram != nil && c.Telegram.ID == id {
			return c, true
		}
	}
	return Client{}, false
}

// newRequest records a request from a Telegram account. A new channel is
// refused to a blocked account, one with a channel, one already waiting,
// one rejected within a day, and when too many wait; more time only to
// one with a channel.
func (s Store) newRequest(u tgUser, kind, text string, now time.Time) (accessRequest, error) {
	if old, err := s.getRequest(u.ID); err == nil {
		switch {
		case old.State == reqBlocked:
			return old, errBlocked
		case old.State == reqPending, old.State == reqApproved:
			// An approved one waits for the client bot to send the link.
			return old, errWaiting
		case old.State == reqRejected && now.Sub(old.Decided) < askAgainFrom:
			return old, errTooSoon
		}
	}
	r := accessRequest{ID: u.ID, Username: u.Username, Name: u.FirstName, Lang: u.LanguageCode,
		Kind: kind, Text: strings.Join(strings.Fields(text), " "), At: now.UTC().Truncate(time.Second), State: reqPending}
	if len([]rune(r.Text)) > 300 {
		r.Text = string([]rune(r.Text)[:300]) + "…"
	}
	c, has := s.clientOf(u.ID)
	switch {
	case kind == reqAccess && has:
		return r, errHasChannel
	case kind == reqExtend && !has:
		return r, fmt.Errorf("no channel to extend")
	case kind == reqExtend:
		r.Client = c.Name
	}
	if len(s.pendingRequests()) >= maxPending {
		return r, errTooMany
	}
	return r, s.saveRequest(r)
}

// clientNameFor makes a free client name from a Telegram account: its
// username in lowercase, else tg<id>.
func (s Store) clientNameFor(r accessRequest) string {
	var b strings.Builder
	for _, c := range strings.ToLower(r.Username) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if len(base) > 28 {
		base = strings.Trim(base[:28], "-")
	}
	if validName(base) != nil {
		base = "tg" + strconv.FormatInt(r.ID, 10)
		if len(base) > 28 {
			base = base[:28]
		}
	}
	name := base
	for i := 2; ; i++ {
		if _, err := os.Stat(s.clientDir(name)); errors.Is(err, os.ErrNotExist) {
			return name
		}
		name = base + "-" + strconv.Itoa(i)
	}
}

// approveRequest grants a request: a new channel — named after the
// account, a document from the pool, linked to the account, the note
// saying it came from the bot — or more time on the person's channel.
// The caller holds the lock and starts the node (apply).
func (s Store) approveRequest(id int64, how string, now time.Time) (accessRequest, Client, error) {
	r, err := s.getRequest(id)
	if err != nil {
		return r, Client{}, fmt.Errorf("no request from %d", id)
	}
	if r.State != reqPending {
		return r, Client{}, fmt.Errorf("the request of %s is %s already", r.Who(), r.State)
	}
	var c Client
	if r.Kind == reqExtend {
		if c, err = s.Get(r.Client); err != nil {
			return r, c, err
		}
		if c.Expires, err = expiryFrom(how, "", c.Expires, now); err != nil {
			return r, c, err
		}
		c.Paused = false
	} else {
		when, err := expiryFrom(how, "", time.Time{}, now)
		if err != nil {
			return r, c, err
		}
		d, err := s.freeDoc(Client{Transport: "mailru"})
		if err != nil {
			return r, c, err
		}
		if c, err = s.Add(s.clientNameFor(r), d.Transport, d.URL); err != nil {
			return r, c, err
		}
		c.Expires = when
		c.Telegram = &TGAccount{ID: r.ID, Username: r.Username, Name: r.Name}
		c.Note = r.Text // what they wrote about themselves, if anything
		if len([]rune(c.Note)) > maxNote {
			c.Note = string([]rune(c.Note)[:maxNote-1]) + "…"
		}
	}
	if err := s.Save(c); err != nil {
		return r, c, err
	}
	r.State, r.Client, r.Decided = reqApproved, c.Name, now.UTC().Truncate(time.Second)
	return r, c, s.saveRequest(r)
}

// decideRequest rejects or blocks a request; blocking works on any
// account the store has heard of.
func (s Store) decideRequest(id int64, state string, now time.Time) (accessRequest, error) {
	r, err := s.getRequest(id)
	if err != nil {
		return r, fmt.Errorf("no request from %d", id)
	}
	if state != reqBlocked && r.State != reqPending {
		return r, fmt.Errorf("the request of %s is %s already", r.Who(), r.State)
	}
	r.State, r.Decided = state, now.UTC().Truncate(time.Second)
	return r, s.saveRequest(r)
}

// findRequest finds a request by Telegram id or @username.
func (s Store) findRequest(who string) (accessRequest, error) {
	all, err := s.listRequests()
	if err != nil {
		return accessRequest{}, err
	}
	for _, r := range all {
		if strconv.FormatInt(r.ID, 10) == who || (r.Username != "" && strings.EqualFold("@"+r.Username, who)) ||
			strings.EqualFold(r.Username, who) {
			return r, nil
		}
	}
	return accessRequest{}, fmt.Errorf("no request from %s (reflux requests lists them)", who)
}

// ---- the CLI ----

// cmdRequests lists the requests, or approves, rejects, blocks or forgets
// one.
func cmdRequests(s Store, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		all, err := s.listRequests()
		if err != nil {
			return err
		}
		if len(all) == 0 {
			fmt.Fprintln(stdout, "No requests. People ask the client bot for access.")
			return nil
		}
		for _, r := range all {
			fmt.Fprintf(stdout, "%-9s %-7s %-12d %-20s %s  %s\n", r.State, r.Kind, r.ID, r.Who(), r.At.Local().Format("2006-01-02 15:04"), r.Text)
		}
		return nil
	}
	fs := newFlagSet("requests " + args[0])
	expires := fs.String("expires", "+30", "approve: +N days or never")
	who, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	r, err := s.findRequest(who)
	if err != nil {
		return err
	}
	now := time.Now()
	switch args[0] {
	case "approve":
		how := *expires
		if how != "never" && !strings.HasPrefix(how, "+") {
			how = "+" + strings.TrimSuffix(how, "d")
		}
		r, c, err := s.approveRequest(r.ID, how, now)
		if err != nil {
			return err
		}
		if err := apply(s, stdout); err != nil {
			return fmt.Errorf("approved, but starting %s failed: %w", c.Name, err)
		}
		fmt.Fprintf(stdout, "Approved %s: client %s, access %s. The client bot sends them the link and QR.\n", r.Who(), c.Name, accessText(c, now))
	case "reject", "block":
		state := reqRejected
		if args[0] == "block" {
			state = reqBlocked
		}
		if r, err = s.decideRequest(r.ID, state, now); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: %s.\n", r.Who(), r.State)
	case "forget":
		if err := s.removeRequest(r.ID); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Forgot %s's request: they may ask again.\n", r.Who())
	default:
		return errors.New("requests: reflux requests [approve|reject|block|forget <id|@user>]")
	}
	return nil
}
