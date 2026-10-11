package main

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

type fakeOfferSession struct {
	mu   sync.Mutex
	peer [32]byte
	sent []control.ConfigOffer
}

func (f *fakeOfferSession) PeerID() [32]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peer
}

func (f *fakeOfferSession) SendControl(sub control.Subtype, p []byte) error {
	if sub != control.SubtypeConfigOffer {
		return nil
	}
	o, err := control.DecodeConfigOffer(p)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.sent = append(f.sent, o)
	f.mu.Unlock()
	return nil
}

func (f *fakeOfferSession) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeOfferSession) setPeer(b byte) {
	f.mu.Lock()
	f.peer = [32]byte{b}
	f.mu.Unlock()
}

// The exit tells a client its configuration once per handshake, again when
// the file changes, and not while nobody is connected.
func TestConfigOffer(t *testing.T) {
	defer func(p, r time.Duration) { configOfferPoll, configOfferRepeat = p, r }(configOfferPoll, configOfferRepeat)
	configOfferPoll, configOfferRepeat = 10*time.Millisecond, time.Hour

	path := filepath.Join(t.TempDir(), "offer")
	write := func(v int, link string) {
		os.WriteFile(path, []byte(`{"link":"`+link+`","version":`+strconv.Itoa(v)+`}`), 0o600)
		// A rewrite within the file system's timestamp granularity must
		// still be seen.
		at := time.Now().Add(time.Duration(v) * time.Second)
		os.Chtimes(path, at, at)
	}
	write(1, "openflux://v1")
	f := &fakeOfferSession{}
	stop, done := make(chan struct{}), make(chan struct{})
	defer func() { close(stop); <-done }()
	go func() { runConfigOffer(f, path, stop); close(done) }()

	time.Sleep(60 * time.Millisecond)
	if n := f.count(); n != 0 {
		t.Fatalf("%d offers with no client", n)
	}
	f.setPeer(1)
	waitFor(t, "the offer to the new client", func() bool { return f.count() == 1 })
	time.Sleep(60 * time.Millisecond)
	if n := f.count(); n != 1 {
		t.Fatalf("offered %d times to one client", n)
	}
	write(2, "openflux://v2")
	waitFor(t, "the changed offer", func() bool { return f.count() == 2 })
	f.setPeer(2) // the client reconnected
	waitFor(t, "the offer after a reconnect", func() bool { return f.count() == 3 })
	f.mu.Lock()
	last := f.sent[2]
	f.mu.Unlock()
	if last.Link != "openflux://v2" || last.Version != 2 {
		t.Errorf("last offer %+v", last)
	}
	write(3, "https://not-a-link")
	time.Sleep(60 * time.Millisecond)
	if n := f.count(); n != 3 {
		t.Errorf("a bad file was offered (%d offers)", n)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
