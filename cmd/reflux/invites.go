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
)

// Invites: a one-time link to the client bot that gives access at once,
// without a request to approve. The owner makes one (the owner bot, the
// panel, reflux invite) with the access time it gives; the person opens
// t.me/<client bot>?start=<code> and gets their channel. A link works
// once, for inviteFor; unused ones can be revoked.

const inviteFor = 7 * 24 * time.Hour

type invite struct {
	Code    string    `json:"code"`
	How     string    `json:"how"` // +30, +90 or never: what approving gives
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
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return invite{}, err
	}
	note = strings.Join(strings.Fields(note), " ")
	if len([]rune(note)) > maxNote {
		return invite{}, fmt.Errorf("the note is too long: %d characters at most", maxNote)
	}
	inv := invite{Code: strings.ToLower(inviteEncoder.EncodeToString(b)), How: how, Note: note,
		Created: now.UTC().Truncate(time.Second), Expires: now.Add(inviteFor).UTC().Truncate(time.Second)}
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
func (s Store) useInvite(code string, u tgUser, now time.Time) (accessRequest, Client, invite, error) {
	var inv invite
	if !inviteCodeRe.MatchString(code) {
		return accessRequest{}, Client{}, inv, errNoInvite
	}
	b, err := os.ReadFile(s.invitePath(code))
	if err != nil || json.Unmarshal(b, &inv) != nil || !now.Before(inv.Expires) {
		os.Remove(s.invitePath(code))
		return accessRequest{}, Client{}, inv, errNoInvite
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

// inviteLink is the link to the client bot for an invite, or the bare
// code when the client bot is not set up.
func (s Store) inviteLink(code string) string {
	c, ok, _ := s.loadClientBot()
	if !ok {
		return code
	}
	if c.Username == "" {
		if me, err := newTelegram(c.Token).getMe(); err == nil {
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
			fmt.Fprintf(stdout, "%s  %-6s until %s  %s\n", inv.Code, inv.How, inv.Expires.Local().Format("2006-01-02 15:04"), inv.Note)
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("invite: reflux invite [--expires +30|never] [--note <who>] | list | revoke <code>")
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
