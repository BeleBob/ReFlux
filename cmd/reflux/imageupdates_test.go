package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRegistry serves tokens and manifest digests like ghcr.io; digests
// maps a repository to its tag's digest.
func fakeRegistry(t *testing.T, digests map[string]string) *sync.Mutex {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"token":"t"}`)
			return
		}
		repo, _, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
		if !ok || r.Method != http.MethodHead || r.Header.Get("Authorization") != "Bearer t" || digests[repo] == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", digests[repo])
	}))
	old := registryBase
	registryBase = srv.URL
	t.Cleanup(func() { registryBase = old; srv.Close() })
	return &mu
}

// fakeImages answers docker image inspect with local digests and records
// the other docker calls.
func fakeImages(t *testing.T, local map[string]string) *[]string {
	t.Helper()
	calls := fakeDocker(t, "")
	inner := runDocker
	runDocker = func(stdout io.Writer, args ...string) error {
		if len(args) == 5 && args[0] == "image" && args[1] == "inspect" {
			img := args[4]
			repo, _, _ := strings.Cut(img, ":")
			if d := local[img]; d != "" {
				fmt.Fprintf(stdout, `["%s@%s"]`+"\n", repo, d)
			}
			return nil
		}
		return inner(stdout, args...)
	}
	return calls
}

const (
	nodeImg   = "ghcr.io/belebob/reflux-node:main"
	egressImg = "ghcr.io/belebob/reflux-egress:main"
)

func TestImageDigests(t *testing.T) {
	remote := map[string]string{"belebob/reflux-node": "sha256:new1", "belebob/reflux-egress": "sha256:e1"}
	fakeRegistry(t, remote)
	fakeImages(t, map[string]string{nodeImg: "sha256:old1", egressImg: "sha256:e1"})
	if d, err := remoteDigest(nodeImg); err != nil || d != "sha256:new1" {
		t.Errorf("remote %q %v", d, err)
	}
	if d, err := remoteDigest("docker.io/library/alpine:3"); err != nil || d != "" {
		t.Errorf("another registry: %q %v", d, err)
	}
	if _, err := remoteDigest("ghcr.io/belebob/nothing:main"); err == nil {
		t.Error("a missing image")
	}
	states, err := checkImages()
	if err != nil || len(states) != 2 || !states[0].newer() || states[1].newer() || !anyNewer(states) {
		t.Errorf("states %+v %v", states, err)
	}
	if states[0].Local != "sha256:old1" {
		t.Errorf("local %q", states[0].Local)
	}
}

func TestUpdatesFromTheBot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	remote := map[string]string{"belebob/reflux-node": "sha256:n2", "belebob/reflux-egress": "sha256:e1"}
	mu := fakeRegistry(t, remote)
	calls := fakeImages(t, map[string]string{nodeImg: "sha256:n1", egressImg: "sha256:e1"})
	s := Store{Root: home}
	s.Init()
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	day := time.Date(2026, 10, 2, 14, 0, 0, 0, time.Local)

	// Offered once with a button; not again for the same images.
	b.checkUpdates(day, false)
	m := f.to(42)
	if len(m) != 1 || !strings.Contains(m[0].Text, "Доступно обновление ReFlux") || !strings.Contains(m[0].Buttons.String(), "upd!:") {
		t.Fatalf("offer %+v", m)
	}
	b.checkUpdates(day.Add(time.Minute), false)
	b.checkUpdates(day.Add(7*time.Hour), false)
	if len(f.to(42)) != 1 {
		t.Error("offered again for the same images")
	}
	// A newer one: offered again.
	mu.Lock()
	remote["belebob/reflux-node"] = "sha256:n3"
	mu.Unlock()
	b.checkUpdates(day.Add(14*time.Hour), false)
	if len(f.to(42)) != 2 {
		t.Error("a newer image not offered")
	}
	// The button updates; a stale one does not.
	*calls = nil
	b.handle(press(1, 42, "upd!:"+fmt.Sprint(time.Now().Add(-time.Hour).Unix()), time.Now()))
	if strings.Contains(strings.Join(*calls, "\n"), "pull") {
		t.Error("a stale button updated")
	}
	b.handle(press(2, 42, "upd!:"+stamp(), time.Now()))
	if !strings.Contains(strings.Join(*calls, "\n"), "pull --quiet "+nodeImg) || !strings.Contains(f.lastEditText(), "Обновлено") {
		t.Errorf("update: %q\n%s", *calls, f.lastEditText())
	}
	// The screen and the night switch.
	b.handle(press(3, 42, "upd?", time.Now()))
	if !strings.Contains(f.lastEditText(), "reflux-node") && !strings.Contains(f.lastEditText(), "node:") {
		t.Errorf("screen:\n%s", f.lastEditText())
	}
	b.handle(press(4, 42, "updauto", time.Now()))
	if c, _ := s.loadBotConfig(); !c.AutoUpdate || c.Token != testToken {
		t.Errorf("auto %+v", c)
	}
	if !strings.Contains(f.lastEditText(), "Автообновление включено") {
		t.Errorf("screen after the switch:\n%s", f.lastEditText())
	}
}

func TestNightUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeRegistry(t, map[string]string{"belebob/reflux-node": "sha256:n2", "belebob/reflux-egress": "sha256:e1"})
	calls := fakeImages(t, map[string]string{nodeImg: "sha256:n1", egressImg: "sha256:e1"})
	s := Store{Root: home}
	s.Init()
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42, AutoUpdate: true})
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	evening := time.Date(2026, 10, 2, 22, 0, 0, 0, time.Local)
	b.checkUpdates(evening, true) // offered: night is later
	if strings.Contains(strings.Join(*calls, "\n"), "pull") {
		t.Fatal("updated in the evening")
	}
	night := time.Date(2026, 10, 3, 4, 20, 0, 0, time.Local)
	b.checkUpdates(night, true)
	pulls := strings.Count(strings.Join(*calls, "\n"), "pull --quiet")
	if pulls != 2 || !strings.Contains(f.to(42)[len(f.to(42))-1].Text, "обновлён ночью") {
		t.Errorf("night: %d pulls, %+v", pulls, f.to(42))
	}
	b.checkUpdates(night.Add(10*time.Minute), true)
	if strings.Count(strings.Join(*calls, "\n"), "pull --quiet") != 2 {
		t.Error("updated twice in a night")
	}
}

// After an update the bot ran, what the restart breaks is not sent; the
// same changes without the quiet are.
func TestQuietAfterAnUpdate(t *testing.T) {
	_, _, owner, _ := bots(t)
	owner.check(true)
	owner.mon.confirm = 1
	broken := func() {
		owner.mon.reported = map[string]finding{}
		for _, f := range runChecks(owner.s) {
			owner.mon.reported[f.Key] = finding{Key: f.Key, Level: levelFail, Sig: "down"}
		}
	}
	broken()
	owner.quietUntil = time.Now().Add(time.Minute)
	if news := owner.check(false); len(news) != 0 {
		t.Errorf("sent while quiet: %q", news)
	}
	broken()
	owner.quietUntil = time.Time{}
	if news := owner.check(false); len(news) == 0 {
		t.Error("the control sends nothing either: the test proves nothing")
	}
}
