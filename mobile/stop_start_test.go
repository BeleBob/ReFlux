package mobile

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// slowStart is a transport whose Start waits for release, like carriers
// that are still opening their documents.
type slowStart struct {
	entered chan struct{}
	release chan struct{}
	stopped atomic.Bool
}

func newSlowStart() *slowStart {
	return &slowStart{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *slowStart) Start() error {
	close(s.entered)
	<-s.release
	return nil
}
func (s *slowStart) Stop() error                     { s.stopped.Store(true); return nil }
func (s *slowStart) Send([]byte) error               { return nil }
func (s *slowStart) Receive(func([]byte))            {}
func (s *slowStart) IsConnected() bool               { return false }
func (s *slowStart) Stats() transport.TransportStats { return transport.TransportStats{} }

// A Stop that comes while a start is still in Start (the app reconnecting
// while the tunnel is connecting) must stop that start's transport too: it
// used to finish after the next start, take its place and keep running
// with the old settings.
func TestStopDuringStartStopsThatStart(t *testing.T) {
	Stop()
	t.Cleanup(Stop)

	old := newSlowStart()
	oldDone := make(chan string, 1)
	go func() {
		oldDone <- startPacket(func() (transport.Transport, error) { return old, nil })
	}()
	<-old.entered

	Stop()

	next := newSlowStart()
	close(next.release)
	if msg := startPacket(func() (transport.Transport, error) { return next, nil }); msg != "" {
		t.Fatalf("next start: %q", msg)
	}

	close(old.release)
	select {
	case <-oldDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped start did not return")
	}

	if !old.stopped.Load() {
		t.Error("the transport of the stopped start keeps running")
	}
	if next.stopped.Load() {
		t.Error("the next start's transport was stopped")
	}
	client.mu.Lock()
	current, running := client.transport, client.running
	client.mu.Unlock()
	if current != transport.Transport(next) || !running {
		t.Errorf("client runs %p (running %v), want the next start's %p", current, running, next)
	}
}

// The same for the SOCKS proxy: the stopped start must not keep its
// transport (or the port) once its carriers are up.
func TestStopProxyDuringStartStopsThatStart(t *testing.T) {
	StopProxy()
	t.Cleanup(StopProxy)

	old := newSlowStart()
	oldDone := make(chan string, 1)
	go func() {
		oldDone <- startProxyWith(func() (transport.Transport, error) { return old, nil }, "127.0.0.1:0", "", "", "")
	}()
	<-old.entered

	StopProxy()

	next := newSlowStart()
	close(next.release)
	if msg := startProxyWith(func() (transport.Transport, error) { return next, nil }, "127.0.0.1:0", "", "", ""); msg != "" {
		t.Fatalf("next start: %q", msg)
	}

	close(old.release)
	select {
	case <-oldDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped start did not return")
	}

	if !old.stopped.Load() {
		t.Error("the transport of the stopped start keeps running")
	}
	proxy.mu.Lock()
	current, running := proxy.transport, proxy.running
	proxy.mu.Unlock()
	if current != transport.Transport(next) || !running {
		t.Errorf("proxy runs %p (running %v), want the next start's %p", current, running, next)
	}
}

// And for the phone as an exit node.
func TestStopExitDuringStartStopsThatStart(t *testing.T) {
	StopExit()
	t.Cleanup(StopExit)

	old := newSlowStart()
	oldDone := make(chan string, 1)
	go func() {
		oldDone <- startExitWith(func() (transport.Transport, error) { return old, nil }, nil)
	}()
	<-old.entered

	StopExit()

	next := newSlowStart()
	close(next.release)
	if msg := startExitWith(func() (transport.Transport, error) { return next, nil }, nil); msg != "" {
		t.Fatalf("next start: %q", msg)
	}

	close(old.release)
	select {
	case <-oldDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped start did not return")
	}

	if !old.stopped.Load() {
		t.Error("the transport of the stopped start keeps running")
	}
	exitNode.mu.Lock()
	current, running := exitNode.transport, exitNode.running
	exitNode.mu.Unlock()
	if current != transport.Transport(next) || !running {
		t.Errorf("exit runs %p (running %v), want the next start's %p", current, running, next)
	}
}
