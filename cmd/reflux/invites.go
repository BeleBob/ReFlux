package main

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

// Invites: a one-time link to the client bot that gives access at once,
// without a request to approve. The owner makes one (the owner bot, the
// panel, reflux invite) with the access time it gives; the person opens
// t.me/<client bot>?start=<code> and gets their channel. A link works
// once, for inviteFor; unused ones can be revoked.
//
// A link invite (Client set) gives an existing client, made without an
// account, to whoever opens it first: the owner adds a client now and
// links it to a person later.

const inviteFor = 7 * 24 * time.Hour

type invite struct {
	Code    string    `json:"code"`
	How     string    `json:"how"` // +30, +90 or never: what approving gives
	// Client: a link invite's client, which keeps its own access.
	Client string `json:"client,omitempty"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

var (
	inviteCodeRe  = regexp.MustCompile(`^[a-z2-7]{16}$`)
	errNoInvite   = errors.New("no such invite: used, revoked or expired")
	inviteEncoder = base32.StdEncoding.WithPadding(base32.NoPadding)
)

func (s Store) invitesDir() string { return filepath.Join(s.Root, "invites") }

func (s Store) invitePath(code string) string {
	return filepath.Join(s.invitesDir(), code+".json")
}

// newInvite makes an invite: a random code of 16 base32 letters (80 bits).
func (s Store) newInvite(how, note string, now time.Time) (invite, error) {
	if _, err := expiryFrom(how, "", time.Time{}, now); err != nil {
		return invite{}, err
	}
	note = strings.Join(strings.Fields(note), " ")
	if len([]rune(note)) > maxNote {
		return invite{}, fmt.Errorf("the note is too long: %d characters at most", maxNote)
	}
	return s.saveNewInvite(invite{How: how, Note: note}, now)
}

// newLinkInvite makes a link invite for a client no account has yet.
func (s Store) newLinkInvite(client string, now time.Time) (invite, error) {
	c, err := s.Get(client)
	if err != nil {
		return invite{}, err
	}
	if c.Telegram != nil {
		return invite{}, fmt.Errorf("%s is linked to %s already", c.Name, c.Telegram)
	}
	return s.saveNewInvite(invite{Client: c.Name}, now)
}

// saveNewInvite gives inv a random code of 16 base32 letters (80 bits) and
// its dates, and saves it.
func (s Store) saveNewInvite(inv invite, now time.Time) (invite, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return invite{}, err
	}
	inv.Code = strings.ToLower(inviteEncoder.EncodeToString(b))
	inv.Created = now.UTC().Truncate(time.Second)
	inv.Expires = now.Add(inviteFor).UTC().Truncate(time.Second)
	if err := os.MkdirAll(s.invitesDir(), 0o700); err != nil {
		return inv, err
	}
	return inv, writeJSON(s.invitePath(inv.Code), inv)
}

// listInvites returns the invites that still work, the newest first, and
// removes the expired ones.
func (s Store) listInvites(now time.Time) []invite {
	paths, _ := filepath.Glob(filepath.Join(s.invitesDir(), "*.json"))
	var out []invite
	for _, p := range paths {
		var inv invite
		b, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(b, &inv) != nil {
			continue
		}
		if !now.Before(inv.Expires) {
			os.Remove(p)
			continue
		}
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (s Store) revokeInvite(code string) error {
	if !inviteCodeRe.MatchString(code) {
		return errNoInvite
	}
	if err := os.Remove(s.invitePath(code)); errors.Is(err, os.ErrNotExist) {
		return errNoInvite
	} else {
		return err
	}
}

// useInvite spends an invite on a Telegram account: a request made and
// approved at once, as the owner would. An account that cannot use it
// (blocked, with a channel or a request already) leaves it for the one
// it was meant for. The caller holds the lock and starts the node.
func (s Store) useInvite(code string, u telegram.User, now time.Time) (accessRequest, Client, invite, error) {
	var inv invite
	if !inviteCodeRe.MatchString(code) {
		return accessRequest{}, Client{}, inv, errNoInvite
	}
	b, err := os.ReadFile(s.invitePath(code))
	if err != nil || json.Unmarshal(b, &inv) != nil || !now.Before(inv.Expires) {
		os.Remove(s.invitePath(code))
		return accessRequest{}, Client{}, inv, errNoInvite
	}
	if inv.Client != "" {
		return s.useLinkInvite(inv, u, now)
	}
	text := inv.Note
	if text == "" {
		text = "invite"
	}
	r, err := s.newRequest(u, reqAccess, text, now)
	if err != nil {
		return r, Client{}, inv, err
	}
	// The invite is spent once a request stands for it.
	os.Remove(s.invitePath(code))
	r, c, err := s.approveRequest(u.ID, inv.How, now)
	if err != nil {
		// The pool ran dry: the request waits for the owner instead.
		return r, c, inv, err
	}
	return r, c, inv, nil
}

// useLinkInvite links the invite's client to the account: a request made
// and approved for that client, so the client bot sends the welcome and
// the link. An account with a channel of its own keeps the invite unspent;
// a client someone was linked to meanwhile spends it.
func (s Store) useLinkInvite(inv invite, u telegram.User, now time.Time) (accessRequest, Client, invite, error) {
	c, err := s.Get(inv.Client)
	if err != nil || c.Telegram != nil {
		os.Remove(s.invitePath(inv.Code))
		return accessRequest{}, Client{}, inv, errNoInvite
	}
	if _, ok := s.clientOf(u.ID); ok {
		return accessRequest{}, Client{}, inv, errWaiting
	}
	r, err := s.newRequest(u, reqAccess, "invite: "+c.Name, now)
	if err != nil {
		return r, Client{}, inv, err
	}
	os.Remove(s.invitePath(inv.Code))
	c.Telegram = &TGAccount{ID: u.ID, Username: u.Username, Name: u.FirstName}
	if err := s.Save(c); err != nil {
		return r, c, inv, err
	}
	r.State, r.Client, r.Decided = reqApproved, c.Name, now.UTC().Truncate(time.Second)
	return r, c, inv, s.saveRequest(r)
}

// inviteLink is the link to the client bot for an invite, or the bare
// code when the client bot is not set up.
func (s Store) inviteLink(code string) string {
	c, ok, _ := s.loadClientBot()
	if !ok {
		return code
	}
	if c.Username == "" {
		if me, err := telegram.New(c.Token).GetMe(); err == nil {
			c.Username = me.Username
			writeJSON(s.clientBotPath(), c)
		}
	}
	if c.Username == "" {
		return code
	}
	return "https://t.me/" + c.Username + "?start=" + code
}

// cmdInvite makes an invite, lists them, or revokes one.
func cmdInvite(s Store, args []string, stdout io.Writer) error {
	now := time.Now()
	if len(args) > 0 && args[0] == "list" {
		invs := s.listInvites(now)
		if len(invs) == 0 {
			fmt.Fprintln(stdout, "No invites. Make one: reflux invite [--expires +30|+90|never] [--note <who>]")
		}
		for _, inv := range invs {
			how := inv.How
			if inv.Client != "" {
				how = "link " + inv.Client
			}
			fmt.Fprintf(stdout, "%s  %-6s until %s  %s\n", inv.Code, how, inv.Expires.Local().Format("2006-01-02 15:04"), inv.Note)
		}
		return nil
	}
	if len(args) == 2 && args[0] == "revoke" {
		if err := s.revokeInvite(args[1]); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Revoked.")
		return nil
	}
	fs := newFlagSet("invite")
	how := fs.String("expires", "+30", "the access it gives: +N days or never")
	note := fs.String("note", "", "who it is for, for you")
	client := fs.String("client", "", "link this existing client to whoever opens the invite")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *client != "" {
		inv, err := s.newLinkInvite(*client, now)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Link invite for %s (works once, until %s):\n%s\n", inv.Client, inv.Expires.Local().Format("2006-01-02 15:04"), s.inviteLink(inv.Code))
		return nil
	}
	if fs.NArg() > 0 {
		return errors.New("invite: reflux invite [--expires +30|never] [--note <who>] | --client <name> | list | revoke <code>")
	}
	h := *how
	if h != "never" && !strings.HasPrefix(h, "+") {
		h = "+" + strings.TrimSuffix(h, "d")
	}
	inv, err := s.newInvite(h, *note, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Invite (works once, until %s), access %s:\n%s\n", inv.Expires.Local().Format("2006-01-02 15:04"), inv.How, s.inviteLink(inv.Code))
	if _, ok, _ := s.loadClientBot(); !ok {
		fmt.Fprintln(stdout, "The client bot is not set up: reflux bot client")
	}
	return nil
}
