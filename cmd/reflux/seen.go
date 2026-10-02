package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Last seen: when each client was last connected, as its node tells
// whoever reads it (the bot every minute, the panel every 10 seconds, the
// CLI). Kept in seen.json, written at most once a minute by a process.

const seenSaveEvery = time.Minute

var seenCache = struct {
	sync.Mutex
	m       map[string]time.Time
	savedAt time.Time
}{m: map[string]time.Time{}}

func (s Store) seenPath() string { return filepath.Join(s.Root, "seen.json") }

func (s Store) readSeen() map[string]time.Time {
	m := map[string]time.Time{}
	if b, err := os.ReadFile(s.seenPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// markSeen notes clients connected now.
func (s Store) markSeen(names []string, now time.Time) {
	if len(names) == 0 {
		return
	}
	c := &seenCache
	c.Lock()
	defer c.Unlock()
	for _, n := range names {
		c.m[n] = now.UTC().Truncate(time.Second)
	}
	if now.Sub(c.savedAt) < seenSaveEvery {
		return
	}
	m := s.readSeen()
	for n, t := range c.m {
		if t.After(m[n]) {
			m[n] = t
		}
	}
	if err := writeJSON(s.seenPath(), m); err == nil {
		c.savedAt = now
	}
}

// lastSeen is when each client was last connected, as far as this
// process and the file know.
func (s Store) lastSeen() map[string]time.Time {
	m := s.readSeen()
	c := &seenCache
	c.Lock()
	defer c.Unlock()
	for n, t := range c.m {
		if t.After(m[n]) {
			m[n] = t
		}
	}
	return m
}
