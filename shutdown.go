package main

import (
	"log"
	"os"
	"time"

	"openflux/transport"
)

// stopOnSignal blocks until the process is asked to stop (SIGINT, SIGTERM,
// SIGHUP: what Docker, systemd and a closed terminal send), then runs the
// given stops and stops trans before exiting. Stopping the transports lets
// document carriers leave their documents cleanly: a participant that just
// vanishes keeps its place on the co-authoring server, which then turns the
// peer that replaces it away for minutes.
func stopOnSignal(trans transport.Transport, stops ...func() error) {
	sigCh := make(chan os.Signal, 1)
	notifySignals(sigCh)
	sig := <-sigCh
	log.Printf("%v: shutting down", sig)
	done := make(chan struct{})
	go func() {
		for _, stop := range stops {
			_ = stop()
		}
		_ = trans.Stop()
		close(done)
	}()
	// Docker sends SIGKILL 10 s after SIGTERM: stay well inside that.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Printf("shutdown timed out; exiting anyway")
	}
	os.Exit(0)
}
