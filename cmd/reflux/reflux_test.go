package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"openflux/share"
	"openflux/transport"
)

const testURL = "https://cloud.mail.ru/public/AbCdEfGh1/IjKlMnOp2"

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "phone", "phone-2", "laptop1", strings.Repeat("a", 32)} {
		if err := validName(ok); err != nil {
			t.Errorf("validName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "Phone", "a_b", "a.b", "../x", strings.Repeat("a", 33)} {
		if validName(bad) == nil {
			t.Errorf("validName(%q) = nil, want an error", bad)
		}
	}
}

func TestValidURLRejectsWhatTheConfParserCuts(t *testing.T) {
	for _, ok := range []string{testURL, "AbCdEfGh1/IjKlMnOp2", "https://docs.yandex.ru/edit/d/abc?x=1"} {
		if err := validURL(ok); err != nil {
			t.Errorf("validURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "https://x/#frag", "https://x/a;b", "https://x/a b",
		"https://x/a\nMode = l3", "https://x/a\x00"} {
		if validURL(bad) == nil {
			t.Errorf("validURL(%q) = nil, want an error", bad)
		}
	}
}

func TestAddWritesPrivateFilesAndAFreshKey(t *testing.T) {
	s := Store{Root: t.TempDir()}
	c, err := s.Add("phone", "mailru", testURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"client.json", "key", "node.conf"} {
		st, err := os.Stat(filepath.Join(s.clientDir("phone"), f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, st.Mode().Perm())
		}
	}
	for _, d := range []string{s.Root, s.clientDir("phone"), s.stateDir("phone")} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %v, want 0700", d, st.Mode().Perm())
		}
	}
	key, err := s.Key("phone")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) {
		t.Errorf("key %q is not 64 hex characters", key)
	}
	got, err := s.Get("phone")
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Errorf("Get = %+v, want %+v", got, c)
	}

	other, err := s.Add("laptop", "mailru", testURL+"x")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _ := s.Key(other.Name)
	if otherKey == key {
		t.Error("two clients got the same key")
	}
}

func TestAddRefusesDuplicatesAndBadInput(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	key, _ := s.Key("phone")
	if _, err := s.Add("phone", "mailru", testURL); err == nil {
		t.Error("second add of phone succeeded")
	}
	if again, _ := s.Key("phone"); again != key {
		t.Error("a refused add changed the existing key")
	}
	if _, err := s.Add("x", "oneme", testURL); err == nil {
		t.Error("unsupported transport accepted")
	}
	if _, err := s.Add("y", "mailru", "https://x/#a"); err == nil {
		t.Error("URL with '#' accepted")
	}
}

func TestListIsSortedByName(t *testing.T) {
	s := Store{Root: t.TempDir()}
	for _, n := range []string{"tablet", "laptop", "phone"} {
		if _, err := s.Add(n, "mailru", testURL); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range list {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "laptop,phone,tablet" {
		t.Errorf("List = %v", names)
	}
}

func TestRevokeDeletesKeyConfigAndState(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(s.stateDir("phone"), "cookies.json"), []byte("{}"), 0o600)
	dst, err := s.Revoke("phone", time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dst) != "phone-20260929-120000" {
		t.Errorf("record at %s", dst)
	}
	if _, err := os.Stat(filepath.Join(dst, "client.json")); err != nil {
		t.Errorf("record missing: %v", err)
	}
	for _, gone := range []string{filepath.Join(dst, "key"), filepath.Join(dst, "node.conf"),
		s.clientDir("phone"), s.stateDir("phone")} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists", gone)
		}
	}
	if _, err := s.Get("phone"); err == nil {
		t.Error("revoked client still listed")
	}
}

func TestNodeConfIsAClassicExitWithAKey(t *testing.T) {
	conf := nodeConf(Client{Name: "phone", Transport: "mailru", URL: testURL})
	for _, want := range []string{"[Interface]", "Role = exit", "Mode = l4", "Transport = mailru",
		"URL = " + testURL, "EncryptionKeyFile = /config/key", "CookieStore = /state/cookies.json"} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("node.conf lacks %q:\n%s", want, conf)
		}
	}
	// [Transport] sections would make a Session-only exit that classic
	// apps cannot reach.
	if strings.Contains(conf, "[Transport") {
		t.Errorf("node.conf has a [Transport] section:\n%s", conf)
	}
}

// The link must name the context the exit derives from its .conf, or the
// client's packets never decrypt.
func TestLinkContextMatchesTheExit(t *testing.T) {
	c := Client{Name: "phone", Transport: "mailru", URL: testURL}
	key := strings.Repeat("ab", 32)
	link, err := share.MakeLink(shareConfig(c, key))
	if err != nil {
		t.Fatal(err)
	}
	r := share.Read(link)
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	// The exit: single-transport mode, --url from URL, no --session-context.
	exitContext, _ := transport.KDFContexts("", c.URL, []transport.ContextSource{
		{Type: c.Transport, URL: c.URL, Priority: 100},
	})
	if r.Context != exitContext {
		t.Errorf("link context %q, exit derives %q", r.Context, exitContext)
	}
	got := r.Config
	if got.Secret != key || got.Negotiate || len(got.Transports) != 1 ||
		got.Transports[0].Type != "mailru" || got.Transports[0].URL != testURL {
		t.Errorf("link config = %+v", got)
	}
}

func TestComposeKeepsNodesBehindEgress(t *testing.T) {
	root := "/home/user/reflux"
	clients := []Client{{Name: "phone", Transport: "mailru", URL: testURL}}
	b, err := composeYAML(root, clients, Options{NodeImage: "node:x", EgressImage: "egress:x", UID: 1000, GID: 1000})
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := strings.Cut(string(b), "\n")
	if !strings.HasPrefix(head, "# ") {
		t.Errorf("no header comment: %q", head)
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatalf("compose body is not JSON: %v", err)
	}
	services := f["services"].(map[string]any)
	node := services["node-phone"].(map[string]any)
	if node["network_mode"] != "service:egress" {
		t.Errorf("node network_mode = %v", node["network_mode"])
	}
	if node["user"] != "1000:1000" || node["read_only"] != true {
		t.Errorf("node user/read_only = %v/%v", node["user"], node["read_only"])
	}
	if caps, _ := node["cap_drop"].([]any); len(caps) != 1 || caps[0] != "ALL" {
		t.Errorf("node cap_drop = %v", node["cap_drop"])
	}
	vols := node["volumes"].([]any)
	if vols[0] != root+"/clients/phone:/config:ro" || vols[1] != root+"/state/phone:/state" {
		t.Errorf("node volumes = %v", vols)
	}
	for name, svc := range services {
		svc := svc.(map[string]any)
		if _, ok := svc["ports"]; ok {
			t.Errorf("%s publishes ports", name)
		}
		env, _ := svc["environment"].(map[string]any)
		if v, ok := env["HTTPS_PROXY"]; !ok || v != "" {
			t.Errorf("%s does not blank HTTPS_PROXY: %v", name, env)
		}
	}
	if _, ok := services["egress"]; !ok {
		t.Error("no egress service")
	}
	if !strings.Contains(body, EgressSubnet) {
		t.Error("egress network lacks its fixed subnet")
	}
}

func TestParseArgsTakesFlagsAroundTheName(t *testing.T) {
	for _, args := range [][]string{{"phone", "--png", "x"}, {"--png", "x", "phone"}} {
		name, png, err := parseShow(args)
		if err != nil || name != "phone" || png != "x" {
			t.Errorf("%v: name=%q png=%q err=%v", args, name, png, err)
		}
	}
	if _, _, err := parseShow([]string{"a", "b"}); err == nil {
		t.Error("two names accepted")
	}
}

func parseShow(args []string) (string, string, error) {
	fs := newFlagSet("show")
	png := fs.String("png", "", "")
	name, err := parseArgs(fs, args)
	return name, *png, err
}

// fakeDocker records docker calls; fail makes calls starting with it fail.
func fakeDocker(t *testing.T, fail string) *[]string {
	var calls []string
	old := runDocker
	runDocker = func(stdout io.Writer, args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if fail != "" && strings.HasPrefix(call, fail) {
			return errors.New("docker failed")
		}
		return nil
	}
	t.Cleanup(func() { runDocker = old })
	return &calls
}

func TestRevokeStopsTheNodeBeforeDeletingItsKey(t *testing.T) {
	t.Setenv("REFLUX_HOME", t.TempDir())
	calls := fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"revoke", "phone", "--yes"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(*calls) < 2 || (*calls)[0] != "rm --force reflux-node-phone" ||
		!strings.Contains((*calls)[len(*calls)-1], "up --detach --remove-orphans") {
		t.Errorf("docker calls = %q", *calls)
	}
}

func TestRevokeKeepsEverythingWhenTheNodeCannotBeStopped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "rm")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"revoke", "phone", "--yes"}, nil, io.Discard); err == nil {
		t.Fatal("revoke succeeded although the node could not be stopped")
	}
	if _, err := (Store{Root: home}).Key("phone"); err != nil {
		t.Errorf("key gone after a failed revoke: %v", err)
	}
}

func TestRevokeNeedsTheNameTyped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	calls := fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"revoke", "phone"}, strings.NewReader("y\n"), io.Discard); err == nil {
		t.Error("revoke went ahead on a plain y")
	}
	if len(*calls) != 0 {
		t.Errorf("docker called: %q", *calls)
	}
	if err := run([]string{"revoke", "phone"}, strings.NewReader("phone\n"), io.Discard); err != nil {
		t.Errorf("revoke with the name typed: %v", err)
	}
}
