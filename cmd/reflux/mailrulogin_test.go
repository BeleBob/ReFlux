package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// testCookies is what Firefox copies from a request's cookies, with a
// value that cannot go into a Cookie header (bad) and one net/http would
// not send (mtrc, which the node leaves out).
const testCookies = "Mpop\t\"1:2:owner@mail.ru:\"\nsdcs\t\"S1\"\nmtrc\t\"{\\\"a\\\":1}\"\nbad\t\"x;y\"\n"

// fakeLoginCheck answers check-login as mail.ru would: a login with sdcs
// "S1" is taken, "OLD" is refused, anything else cannot be checked.
func fakeLoginCheck(t *testing.T) *[]string {
	var got []string
	old := checkLoginInEgress
	checkLoginInEgress = func(header string) (loginCheck, error) {
		got = append(got, header)
		switch {
		case strings.Contains(header, "sdcs=S1"):
			return loginCheck{OK: true, Account: "owner@mail.ru"}, nil
		case strings.Contains(header, "sdcs=OLD"):
			return loginCheck{Refused: true, Error: "CSRF token: 403 user"}, nil
		}
		return loginCheck{}, errors.New("the egress could not check the login: not running")
	}
	t.Cleanup(func() { checkLoginInEgress = old })
	return &got
}

// The owner's cookies, in whatever form the browser gave them, reach the
// nodes as one Cookie header once mail.ru takes them; a login mail.ru
// refuses never replaces the one the nodes have.
func TestSaveMailruLogin(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	checks := fakeLoginCheck(t)
	now := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)

	st, err := s.SaveMailruLogin(testCookies, now)
	if err != nil || !st.OK || st.Account != "owner@mail.ru" || !st.Checked.Equal(now) {
		t.Fatalf("good login: %+v %v", st, err)
	}
	b, _ := os.ReadFile(s.mailruCookiesPath())
	if string(b) != "Mpop=1:2:owner@mail.ru:; mtrc={\\\"a\\\":1}; sdcs=S1\n" {
		t.Errorf("cookies file = %q", b)
	}
	if fi, _ := os.Stat(s.mailruCookiesPath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("cookies file mode %v", fi.Mode().Perm())
	}
	if len(*checks) != 1 || !strings.Contains((*checks)[0], "sdcs=S1") {
		t.Errorf("checked %q", *checks)
	}

	// Refused: the good login stays.
	if _, err := s.SaveMailruLogin("Mpop=1:2:owner@mail.ru:; sdcs=OLD", now); !errors.Is(err, errMailruRefused) {
		t.Fatalf("refused login: err = %v", err)
	}
	if b2, _ := os.ReadFile(s.mailruCookiesPath()); string(b2) != string(b) {
		t.Error("a refused login replaced the nodes' one")
	}
	// No login in it: not even checked.
	if _, err := s.SaveMailruLogin("t=x; act=y", now); err == nil || !strings.Contains(err.Error(), "Mpop") {
		t.Fatalf("cookies without a login: err = %v", err)
	}
	if len(*checks) != 2 {
		t.Errorf("cookies without a login were sent to mail.ru")
	}

	// The egress down: saved, but said to be unchecked.
	st, err = s.SaveMailruLogin("Mpop=1:2:owner@mail.ru:; sdcs=NEW", now)
	if err != nil || st.OK || st.Account != "owner@mail.ru" || !strings.Contains(st.Error, "not running") {
		t.Fatalf("unchecked login: %+v %v", st, err)
	}
	if loaded, have, _ := s.mailruLogin(); !have || loaded.OK || loaded.Error == "" {
		t.Errorf("state %+v have=%v", loaded, have)
	}
}

// The bot checks the login every round; a refusal is kept for the doctor,
// a check that could not be made leaves the last answer.
func TestCheckMailruLogin(t *testing.T) {
	s := Store{Root: t.TempDir()}
	s.Init()
	fakeLoginCheck(t)
	if st, err := s.CheckMailruLogin(time.Now()); err != nil || st != (MailruLogin{}) {
		t.Fatalf("no cookies: %+v %v", st, err)
	}
	if _, err := s.SaveMailruLogin("Mpop=1:2:owner@mail.ru:; sdcs=S1", time.Now()); err != nil {
		t.Fatal(err)
	}
	writeFileAtomic(s.mailruCookiesPath(), []byte("Mpop=1:2:owner@mail.ru:; sdcs=OLD\n"))
	if st, err := s.CheckMailruLogin(time.Now()); err != nil || !st.Refused || st.OK {
		t.Fatalf("expired login: %+v %v", st, err)
	}
	writeFileAtomic(s.mailruCookiesPath(), []byte("Mpop=1:2:owner@mail.ru:; sdcs=SLOW\n"))
	if _, err := s.CheckMailruLogin(time.Now()); err == nil {
		t.Fatal("a check that could not be made said nothing")
	}
	if st, _, _ := s.mailruLogin(); !st.Refused {
		t.Errorf("the last answer was lost: %+v", st)
	}
}

// The document checks open the documents as the nodes' account: the
// cookies go first on stdin, and the answers still land on their
// documents.
func TestCheckDocsSignedIn(t *testing.T) {
	s := Store{Root: t.TempDir()}
	s.Init()
	writeFileAtomic(s.mailruCookiesPath(), []byte("Mpop=m; sdcs=s\n"))
	var stdin string
	old := runDockerIn
	runDockerIn = func(in io.Reader, out io.Writer, args ...string) error {
		b, _ := io.ReadAll(in)
		stdin = string(b)
		if strings.Contains(strings.Join(args, " "), "sdcs") {
			t.Errorf("cookies in the docker arguments: %v", args)
		}
		// As reflux-egress answers: the cookies line counts, unanswered.
		json.NewEncoder(out).Encode(map[string]any{"line": 2, "state": "ok"})
		json.NewEncoder(out).Encode(map[string]any{"line": 3, "state": "dead", "error": "API returned status 404"})
		return nil
	}
	t.Cleanup(func() { runDockerIn = old })
	v, err := s.checkDocsInEgress([]Doc{{"mailru", testURL}, {"mailru", docB}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdin, "cookies Mpop=m; sdcs=s\nmailru "+testURL+"\n") {
		t.Errorf("stdin = %q", stdin)
	}
	if v[0].State != "ok" || v[1].State != "dead" {
		t.Errorf("verdicts %+v", v)
	}
}

// Every node sees the account's directory, read-only; the egress, which
// takes the cookies on stdin, does not.
func TestComposeMountsMailru(t *testing.T) {
	b, err := composeYAML("/home/u/reflux", []Client{{Name: "phone", Transport: "mailru", URL: testURL}}, Options{NodeImage: "n", EgressImage: "e", UID: 1000, GID: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var f composeFile
	if err := json.Unmarshal(b[strings.IndexByte(string(b), '{'):], &f); err != nil {
		t.Fatal(err)
	}
	want := "/home/u/reflux/mailru:/mailru:ro"
	if !contains(f.Services["node-phone"].Volumes, want) {
		t.Errorf("node volumes %v lack %s", f.Services["node-phone"].Volumes, want)
	}
	for _, v := range f.Services["egress"].Volumes {
		if strings.Contains(v, "mailru") {
			t.Errorf("egress mounts %s", v)
		}
	}
	if conf := nodeConf(Client{Name: "phone", Transport: "mailru", URL: testURL}); !strings.Contains(conf, "\nMailruCookiesFile = /mailru/cookies\n") {
		t.Errorf("node.conf:\n%s", conf)
	}
}

// `reflux mailru login` takes the cookies pasted on stdin.
func TestMailruCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeLoginCheck(t)
	var out strings.Builder
	if err := run([]string{"mailru"}, nil, &out); err != nil || !strings.Contains(out.String(), "No mail.ru login") {
		t.Fatalf("status without a login: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := run([]string{"mailru", "login"}, strings.NewReader(testCookies), &out); err != nil || !strings.Contains(out.String(), "signed in as owner@mail.ru") {
		t.Fatalf("login: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "S1") {
		t.Errorf("a cookie in the output:\n%s", out.String())
	}
	out.Reset()
	if err := run([]string{"mailru", "status"}, nil, &out); err != nil || !strings.Contains(out.String(), "signed in as owner@mail.ru") {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
}
