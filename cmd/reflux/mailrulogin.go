package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
)

// Since 2026-10-15 mail.ru opens a document for a signed-in account only,
// so the nodes sign in with the owner's account:
//
//	mailru/cookies     its cookies, one Cookie header (0600). The directory
//	                   is mounted read-only into every node at /mailru, and
//	                   the node reads the file (MailruCookiesFile) at start
//	                   and again whenever mail.ru refuses its login, so a
//	                   new login needs no restart.
//	mailru/login.json  the last check of it (MailruLogin)
//
// The owner gives the cookies with `reflux mailru login` or on the panel;
// never through the bot, whose chat would keep them. The check runs in the
// egress namespace, where mail.ru sees the address the nodes come from,
// and the cookies go there on stdin, never as arguments.

const (
	mailruDirName = "mailru"
	// mailruNodeFile is where a node finds the cookies (node.conf).
	mailruNodeFile = "/mailru/cookies"
)

func (s Store) mailruDir() string         { return filepath.Join(s.Root, mailruDirName) }
func (s Store) mailruCookiesPath() string { return filepath.Join(s.mailruDir(), "cookies") }
func (s Store) mailruStatePath() string   { return filepath.Join(s.mailruDir(), "login.json") }

// MailruLogin is what the last check of the account said.
type MailruLogin struct {
	Account string    `json:"account,omitempty"`
	Saved   time.Time `json:"saved,omitzero"`
	Checked time.Time `json:"checked,omitzero"`
	OK      bool      `json:"ok,omitempty"`
	// Refused: mail.ru no longer takes the login. Otherwise a failed
	// check (Error) says nothing about the login.
	Refused bool   `json:"refused,omitempty"`
	Error   string `json:"error,omitempty"`
}

// mailruLogin returns the last check and whether the nodes have cookies.
func (s Store) mailruLogin() (MailruLogin, bool, error) {
	var st MailruLogin
	if _, err := os.Stat(s.mailruCookiesPath()); errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	} else if err != nil {
		return st, false, err
	}
	b, err := os.ReadFile(s.mailruStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return st, true, nil
	}
	if err != nil {
		return st, true, err
	}
	return st, true, json.Unmarshal(b, &st)
}

// mailruCookies is the Cookie header the nodes sign in with, "" when none.
func (s Store) mailruCookies() (string, error) {
	b, err := os.ReadFile(s.mailruCookiesPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

func (s Store) saveMailruState(st MailruLogin) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.mailruStatePath(), append(b, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// cookieHeader is values as one Cookie header, in a stable order. A
// cookie whose name or value would break the header (a ';', a line
// break) is left out: no browser sends one.
func cookieHeader(values map[string]string) string {
	names := make([]string, 0, len(values))
	for k, v := range values {
		if !strings.ContainsAny(k+v, ";\r\n") && !strings.ContainsAny(k, "= ") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k + "=" + values[k]
	}
	return strings.Join(parts, "; ")
}

// loginCheck is the egress's answer (reflux-egress check-login).
type loginCheck struct {
	OK      bool   `json:"ok"`
	Account string `json:"account"`
	Refused bool   `json:"refused"`
	Error   string `json:"error"`
}

// checkLoginInEgress asks mail.ru from the egress namespace whether it
// takes the login in header; tests replace it.
var checkLoginInEgress = func(header string) (loginCheck, error) {
	var out strings.Builder
	if err := runDockerIn(strings.NewReader(header), &out, "exec", "-i", "reflux-egress", "reflux-egress", "check-login"); err != nil {
		return loginCheck{}, fmt.Errorf("the egress could not check the login: %w", err)
	}
	var r loginCheck
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &r); err != nil {
		return loginCheck{}, fmt.Errorf("the egress's answer: %w", err)
	}
	return r, nil
}

// errMailruRefused: mail.ru does not take the login given.
var errMailruRefused = errors.New("mail.ru does not take this login")

// SaveMailruLogin checks the cookies the owner gave (any form a browser
// gives them in, see mailru.ParseCookies) and, unless mail.ru refuses
// them, hands them to the nodes. A check that could not be made (the
// egress is down) does not stop them: the state says so.
func (s Store) SaveMailruLogin(text string, now time.Time) (MailruLogin, error) {
	values, err := mailru.ParseCookies(text)
	if err == nil {
		err = mailru.LoginCookies(values)
	}
	if err != nil {
		return MailruLogin{}, fmt.Errorf("the cookies: %w", err)
	}
	header := cookieHeader(values)
	st := MailruLogin{Saved: now}
	r, err := checkLoginInEgress(header)
	switch {
	case err != nil:
		st.Error = err.Error()
	case r.Refused:
		return MailruLogin{}, fmt.Errorf("%w: %s", errMailruRefused, r.Error)
	default:
		st.Checked, st.OK, st.Account, st.Error = now, r.OK, r.Account, r.Error
	}
	if st.Account == "" {
		st.Account = mailruAccount(values)
	}
	if err := writeFileAtomic(s.mailruCookiesPath(), []byte(header+"\n")); err != nil {
		return MailruLogin{}, err
	}
	return st, s.saveMailruState(st)
}

// mailruAccount is the address the Mpop cookie names ("time:hash:email:").
func mailruAccount(values map[string]string) string {
	parts := strings.Split(values["Mpop"], ":")
	if len(parts) > 2 && strings.Contains(parts[2], "@") {
		return parts[2]
	}
	return ""
}

// CheckMailruLogin checks the nodes' login again (the bot every
// docCheckEvery, `reflux mailru check`). Without cookies it does nothing.
func (s Store) CheckMailruLogin(now time.Time) (MailruLogin, error) {
	st, have, err := s.mailruLogin()
	if err != nil || !have {
		return st, err
	}
	header, err := s.mailruCookies()
	if err != nil {
		return st, err
	}
	r, err := checkLoginInEgress(header)
	if err != nil {
		// Not about the login: the last answer stands.
		return st, err
	}
	st.Checked, st.OK, st.Refused, st.Error = now, r.OK, r.Refused, r.Error
	if r.Account != "" {
		st.Account = r.Account
	}
	return st, s.saveMailruState(st)
}

// usesMailru: some active client has a mail.ru document.
func usesMailru(clients []Client, now time.Time) bool {
	for _, c := range activeClients(clients, now) {
		for _, d := range c.Docs() {
			if d.Transport == "mailru" {
				return true
			}
		}
	}
	return false
}

// mailru checks the nodes' mail.ru login.
func (d *doctor) mailru(s Store) {
	st, have, err := s.mailruLogin()
	if err != nil {
		d.warn("mailru", "mailru.bad", err)
		return
	}
	if !have {
		if clients, err := s.List(); err == nil && usesMailru(clients, time.Now()) {
			d.fail("mailru", "mailru.none")
		}
		return
	}
	switch {
	case st.Refused:
		d.fail("mailru", "mailru.refused", st.Account, st.Checked.Local().Format("02.01 15:04"))
	case !st.OK:
		// Shown, but no alert: the check failed, not the login.
		d.silent(levelWarn, "mailru", "mailru.unchecked", st.Account, st.Error)
	default:
		d.ok("mailru", "mailru.ok", st.Account, durationPhrase(time.Since(st.Checked)))
	}
}

// cmdMailru is `reflux mailru [status | login [file] | check]`.
func cmdMailru(s Store, args []string, stdin io.Reader, stdout io.Writer) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status":
		st, have, err := s.mailruLogin()
		if err != nil {
			return err
		}
		if !have {
			fmt.Fprintln(stdout, "No mail.ru login: mail.ru opens documents for a signed-in account only. Give the cookies of cloud.mail.ru: reflux mailru login <file>")
			return nil
		}
		printMailruLogin(stdout, st)
		return nil
	case "login":
		in := stdin
		if len(args) > 1 && args[1] != "-" {
			f, err := os.Open(args[1])
			if err != nil {
				return err
			}
			defer f.Close()
			in = f
		} else {
			fmt.Fprintln(stdout, "Paste the cookies of cloud.mail.ru (a Cookie header, cookies.txt or the browser's copy), then Ctrl+D:")
		}
		text, err := io.ReadAll(io.LimitReader(in, 1<<20))
		if err != nil {
			return err
		}
		if err := s.Init(); err != nil {
			return err
		}
		st, err := s.SaveMailruLogin(string(text), time.Now())
		if err != nil {
			return err
		}
		printMailruLogin(stdout, st)
		fmt.Fprintln(stdout, "The nodes take the new login at their next attempt; a node set up before this version needs `reflux apply` once.")
		return nil
	case "check":
		st, err := s.CheckMailruLogin(time.Now())
		if err != nil {
			return err
		}
		printMailruLogin(stdout, st)
		if st.Refused {
			return errMailruRefused
		}
		return nil
	}
	return fmt.Errorf("usage: reflux mailru [status | login [file] | check]")
}

func printMailruLogin(stdout io.Writer, st MailruLogin) {
	account := st.Account
	if account == "" {
		account = "(account not named)"
	}
	switch {
	case st.Refused:
		fmt.Fprintf(stdout, "mail.ru: %s — REFUSED at %s: %s\n", account, st.Checked.Local().Format("02.01 15:04"), st.Error)
	case st.OK:
		fmt.Fprintf(stdout, "mail.ru: signed in as %s (checked %s)\n", account, st.Checked.Local().Format("02.01 15:04"))
	default:
		fmt.Fprintf(stdout, "mail.ru: %s, not checked: %s\n", account, st.Error)
	}
}
