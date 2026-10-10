package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Dead documents are found and replaced on their own: every
// docCheckEvery the bot has the egress open each client's documents and
// the pool's free ones (reflux-egress check-docs). A document that fails
// docDeadAfter checks in a row goes to the dead list (docstate.go); its
// client gets a free pool document in its place, the node restarts, and
// the client bot sends the new link. A dead document comes back only when
// the owner has it checked again and it passes, after a quarantine.

const (
	docCheckEvery = 15 * time.Minute
	docDeadAfter  = 2
)

// runDockerIn runs the docker CLI with stdin; tests replace it.
var runDockerIn = func(stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = dockerStderr
	return cmd.Run()
}

// docVerdict is the egress's answer for one document.
type docVerdict struct {
	State string `json:"state"` // ok, dead, unknown
	Error string `json:"error"`
}

// checkDocsInEgress checks documents from the egress namespace; the answers
// come in the documents' order. The links go on stdin, never as arguments.
func checkDocsInEgress(docs []Doc) ([]docVerdict, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	var in strings.Builder
	for _, d := range docs {
		fmt.Fprintf(&in, "%s %s\n", d.Transport, d.URL)
	}
	var out strings.Builder
	if err := runDockerIn(strings.NewReader(in.String()), &out, "exec", "-i", "reflux-egress", "reflux-egress", "check-docs"); err != nil {
		return nil, fmt.Errorf("the egress could not check documents: %w", err)
	}
	verdicts := make([]docVerdict, len(docs))
	for i := range verdicts {
		verdicts[i].State = "unknown"
	}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var v struct {
			Line int `json:"line"`
			docVerdict
		}
		if json.Unmarshal(sc.Bytes(), &v) == nil && v.Line >= 1 && v.Line <= len(docs) {
			verdicts[v.Line-1] = v.docVerdict
		}
	}
	return verdicts, nil
}

// docChange is what an automatic check did about one dead document.
type docChange struct {
	Doc    Doc
	Reason string // the check's answer
	Client string // "" for a pool document
	New    *Doc   // the replacement, nil when none was free
	Err    error  // the client could not be changed
}

// checkDocs checks the clients' documents and the pool's free ones once,
// declares the ones that failed docDeadAfter times in a row dead and
// replaces the clients'. It returns the changes and whether nodes need an
// apply. The caller holds the store lock.
func (s Store) checkDocs(now time.Time) ([]docChange, bool, error) {
	clients, err := s.List()
	if err != nil {
		return nil, false, err
	}
	st, err := s.loadDocsState()
	if err != nil {
		return nil, false, err
	}
	type target struct {
		doc    Doc
		client string
	}
	var targets []target
	for _, c := range clients {
		for _, d := range c.Docs() {
			targets = append(targets, target{d, c.Name})
		}
	}
	pool, err := s.poolStatus()
	if err != nil {
		return nil, false, err
	}
	for _, p := range pool {
		if p.free() {
			targets = append(targets, target{p.Doc, ""})
		}
	}
	docs := make([]Doc, len(targets))
	for i, t := range targets {
		docs[i] = t.doc
	}
	verdicts, err := checkDocsInEgress(docs)
	if err != nil {
		return nil, false, err
	}
	var changes []docChange
	changed := false
	for i, v := range verdicts {
		t := targets[i]
		switch v.State {
		case "ok":
			delete(st.Suspect, t.doc.URL)
			continue
		case "dead":
		default:
			continue
		}
		st.Suspect[t.doc.URL]++
		if st.Suspect[t.doc.URL] < docDeadAfter {
			continue
		}
		st.markDead(t.doc, t.client, v.Error, now)
		ch := docChange{Doc: t.doc, Reason: v.Error, Client: t.client}
		if t.client != "" {
			// The state goes to disk first, so the replacement is not
			// the document that just died.
			if err := s.saveDocsState(st); err != nil {
				return changes, changed, err
			}
			ch.New, ch.Err = s.replaceDoc(t.client, t.doc)
			changed = changed || ch.Err == nil
		}
		changes = append(changes, ch)
	}
	st.Checked = now
	return changes, changed, s.saveDocsState(st)
}

// replaceDoc gives a client a free pool document in place of a dead one.
// Without a free document the dead one stays when it is the client's only
// one (the node keeps trying it), and goes when the client has others.
func (s Store) replaceDoc(name string, dead Doc) (*Doc, error) {
	c, err := s.Get(name)
	if err != nil {
		return nil, err
	}
	index := func(c Client) int {
		for i, d := range c.Docs() {
			if d.URL == dead.URL {
				return i
			}
		}
		return -1
	}
	fresh, ferr := s.freeDoc(c)
	if ferr != nil {
		if i := index(c); i >= 0 && len(c.Docs()) > 1 {
			_, err := s.RemoveDoc(name, i)
			return nil, err
		}
		return nil, ferr
	}
	if len(c.Docs()) >= maxDocs {
		if c, err = s.RemoveDoc(name, index(c)); err != nil {
			return nil, err
		}
	}
	if c, err = s.AddDoc(name, fresh); err != nil {
		return nil, err
	}
	if i := index(c); i >= 0 {
		if _, err := s.RemoveDoc(name, i); err != nil {
			return nil, err
		}
	}
	return &fresh, nil
}

// recheckDead checks dead documents again (all of them when urls is
// empty): a document that opens goes to quarantine and then back to the
// pool. It returns how many came back and how many are still dead.
func (s Store) recheckDead(urls []string, now time.Time) (back, still int, err error) {
	st, err := s.loadDocsState()
	if err != nil {
		return 0, 0, err
	}
	var docs []Doc
	for _, d := range st.deadDocs() {
		if len(urls) == 0 || contains(urls, d.URL) {
			docs = append(docs, Doc{Transport: d.Transport, URL: d.URL})
		}
	}
	if len(docs) == 0 {
		return 0, 0, errors.New("no such dead document")
	}
	verdicts, err := checkDocsInEgress(docs)
	if err != nil {
		return 0, 0, err
	}
	var revived []Doc
	for i, v := range verdicts {
		d := st.Docs[docs[i].URL]
		d.Checked = now
		if v.State == "ok" {
			revived = append(revived, docs[i])
			continue
		}
		if v.Error != "" {
			d.Reason = v.Error
		}
		still++
	}
	for _, d := range revived {
		delete(st.Docs, d.URL)
	}
	if err := s.saveDocsState(st); err != nil {
		return 0, still, err
	}
	return len(revived), still, s.quarantine(revived, "", now)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
