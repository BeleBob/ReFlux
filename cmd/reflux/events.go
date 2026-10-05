package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/host"
)

// The event log: what the bot reported (a tunnel down or back, a
// failover, an update, the bot starting), in both languages, for the web
// panel. One JSON object a line in events.jsonl, the newest last.

type event struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"` // ok, warn, FAIL or info
	RU    string    `json:"ru"`
	EN    string    `json:"en"`
}

// Text is the event in l.
func (e event) Text(l lang) string {
	if l == langEN {
		return e.EN
	}
	return e.RU
}

const (
	eventsKeep = 1000 // lines kept when the log is trimmed
	eventsMax  = 2000 // lines that trigger a trim
)

func (s Store) eventsPath() string { return filepath.Join(s.Root, "events.jsonl") }

// logEvents appends events, trimming the log to eventsKeep lines once it
// has eventsMax.
func (s Store) logEvents(evs []event) error {
	if len(evs) == 0 {
		return nil
	}
	unlock, err := lockFile(s.eventsPath()+".lock", 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	var buf bytes.Buffer
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(append(b, '\n'))
	}
	f, err := os.OpenFile(s.eventsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	f.Close()
	if err != nil {
		return err
	}
	lines, _ := host.ReadLines(s.eventsPath())
	if len(lines) >= eventsMax {
		keep := []byte{}
		for _, l := range lines[len(lines)-eventsKeep:] {
			keep = append(keep, l+"\n"...)
		}
		tmp := s.eventsPath() + ".tmp"
		if err := os.WriteFile(tmp, keep, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, s.eventsPath())
	}
	return nil
}

// readEvents returns up to n events, the newest first.
func (s Store) readEvents(n int) []event {
	f, err := os.Open(s.eventsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	var out []event
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out
}
