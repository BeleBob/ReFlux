package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The states of documents that may not be handed out (docs-state.json):
// dead ones, which no longer open (doccheck.go), until the owner has them
// checked again and they pass; and ones in quarantine for a day after they
// left a client, so the old app stops reaching for them before someone
// else gets them.

const (
	stateDead       = "dead"
	stateQuarantine = "quarantine"
)

// docQuarantine is how long a document waits after leaving a client.
const docQuarantine = 24 * time.Hour

// docState is one document that is dead or in quarantine.
type docState struct {
	Transport string    `json:"transport"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until,omitempty"`  // the end of a quarantine
	Reason    string    `json:"reason,omitempty"` // why it is dead
	Client    string    `json:"client,omitempty"` // whose it was
	Checked   time.Time `json:"checked,omitempty"`
}

// docsState is docs-state.json.
type docsState struct {
	Docs map[string]*docState `json:"docs,omitempty"` // by URL
	// Suspect counts the dead verdicts in a row of documents not yet
	// declared dead: one bad answer is not enough.
	Suspect map[string]int `json:"suspect,omitempty"`
	Checked time.Time      `json:"checked,omitempty"` // the last automatic check
}

func (s Store) docsStatePath() string { return filepath.Join(s.Root, "docs-state.json") }

func (s Store) loadDocsState() (docsState, error) {
	st := docsState{Docs: map[string]*docState{}, Suspect: map[string]int{}}
	b, err := os.ReadFile(s.docsStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Docs == nil {
		st.Docs = map[string]*docState{}
	}
	if st.Suspect == nil {
		st.Suspect = map[string]int{}
	}
	return st, nil
}

func (s Store) saveDocsState(st docsState) error {
	return writeJSON(s.docsStatePath(), st)
}

// blocked: the document may not be handed out now (dead, or quarantined
// until after now). An expired quarantine is forgotten.
func (st docsState) blocked(url string, now time.Time) bool {
	d := st.Docs[url]
	switch {
	case d == nil:
		return false
	case d.State == stateQuarantine && !now.Before(d.Until):
		delete(st.Docs, url)
		return false
	}
	return true
}

// quarantine puts documents that just left a client aside for a day and
// back in the pool after it, so another client gets them then.
func (s Store) quarantine(docs []Doc, client string, now time.Time) error {
	if len(docs) == 0 {
		return nil
	}
	st, err := s.loadDocsState()
	if err != nil {
		return err
	}
	for _, d := range docs {
		if cur := st.Docs[d.URL]; cur != nil && cur.State == stateDead {
			continue // dead stays dead until it passes a check
		}
		st.Docs[d.URL] = &docState{Transport: d.Transport, State: stateQuarantine, Since: now, Until: now.Add(docQuarantine), Client: client}
		delete(st.Suspect, d.URL)
	}
	if err := s.saveDocsState(st); err != nil {
		return err
	}
	_, err = s.addToPool(docs)
	return err
}

// markDead moves a document to the dead list.
func (st docsState) markDead(d Doc, client, reason string, now time.Time) {
	st.Docs[d.URL] = &docState{Transport: d.Transport, State: stateDead, Since: now, Reason: reason, Client: client, Checked: now}
	delete(st.Suspect, d.URL)
}

// deadDoc is a dead document with its URL, for lists.
type deadDoc struct {
	URL string
	docState
}

// deadDocs lists the dead documents, the most recent first.
func (st docsState) deadDocs() []deadDoc {
	var out []deadDoc
	for url, d := range st.Docs {
		if d.State == stateDead {
			out = append(out, deadDoc{URL: url, docState: *d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.After(out[j].Since)
		}
		return out[i].URL < out[j].URL
	})
	return out
}

// revokedAt reads when a client was revoked from its record's directory
// name (revoked/<name>-20060102-150405, UTC).
func revokedAt(dir string) (time.Time, bool) {
	i := strings.LastIndexByte(dir, '-')
	if i < 0 {
		return time.Time{}, false
	}
	j := strings.LastIndexByte(dir[:i], '-')
	if j < 0 {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102-150405", dir[j+1:])
	return t, err == nil
}
