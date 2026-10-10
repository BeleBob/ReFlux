package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Several documents per client. The client's own document stays the main
// one; backups are further documents the node runs at the same time. With
// backups the node is a Session exit: every document is a carrier, traffic
// goes over the highest-priority one that reaches the client and fails
// over to the next. A Session exit serves Session apps only (a classic
// exit serves both, see nodeConf), so the client imports the new link.
//
// The encryption context is the client's first document for good: when
// the main document is removed, a backup takes its place and the context
// stays (Client.Context), so the app's profile goes on working over the
// backups without a new link.
//
// The owner prepares documents in a pool, docs-pool.txt ("type url" or
// "url" per line); the bot and the panel take backups from it.

const maxDocs = 5

// Doc is one document of a channel.
type Doc struct {
	Transport string `json:"transport"`
	URL       string `json:"url"`
}

// Docs are c's documents, the main one first.
func (c Client) Docs() []Doc {
	return append([]Doc{{Transport: c.Transport, URL: c.URL}}, c.Backups...)
}

// session: c's node runs as a Session exit — it has backups, or its main
// document changed and the context is no longer its URL.
func (c Client) session() bool { return len(c.Backups) > 0 || c.Context != "" }

// context is the channel's encryption context.
func (c Client) context() string {
	if c.Context != "" {
		return c.Context
	}
	return c.URL
}

// carrierNames name the documents' carriers for the node and the app:
// the type, numbered from the second document of a type on (mailru,
// mailru-2). A peer that names its carriers otherwise still matches
// them by type and document (PROTOCOL_NEGOTIATION.md).
func carrierNames(docs []Doc) []string {
	seen := map[string]int{}
	out := make([]string, len(docs))
	for i, d := range docs {
		seen[d.Transport]++
		out[i] = d.Transport
		if n := seen[d.Transport]; n > 1 {
			out[i] = fmt.Sprintf("%s-%d", d.Transport, n)
		}
	}
	return out
}

// docPriority is the i-th document's priority: the main one 100, each
// backup 10 less. Distinct priorities make a failover order; equal ones
// would share flows, which measured slower on mail.ru.
func docPriority(i int) int { return 100 - 10*i }

// docIndex is the position of the carrier named active among c's
// documents, -1 for none.
func (c Client) docIndex(active string) int {
	for i, n := range carrierNames(c.Docs()) {
		if n == active {
			return i
		}
	}
	return -1
}

// docUsers maps every document URL to the client using it, and to clients
// revoked less than docQuarantine ago: an old app may still reach for its
// document for a while, so it is not handed to anyone else that soon.
func (s Store) docUsers() (map[string]string, error) {
	users := map[string]string{}
	clients, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		for _, d := range c.Docs() {
			users[d.URL] = c.Name
		}
	}
	revoked, _ := filepath.Glob(filepath.Join(s.Root, "revoked", "*", "client.json"))
	for _, p := range revoked {
		if at, ok := revokedAt(filepath.Base(filepath.Dir(p))); ok && time.Since(at) >= docQuarantine {
			continue
		}
		var c Client
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &c) == nil {
			for _, d := range c.Docs() {
				if _, ok := users[d.URL]; !ok {
					users[d.URL] = filepath.Base(filepath.Dir(p))
				}
			}
		}
	}
	return users, nil
}

// checkDoc: d is a document a client can be given — a known transport, a
// URL the node's .conf keeps intact, no other client's.
func (s Store) checkDoc(d Doc) error {
	if !transports[d.Transport] {
		return fmt.Errorf("unsupported transport %q (supported: %s)", d.Transport, strings.Join(supportedTransports(), ", "))
	}
	if err := validURL(d.URL); err != nil {
		return err
	}
	clients, err := s.List()
	if err != nil {
		return err
	}
	for _, c := range clients {
		for _, o := range c.Docs() {
			if o.URL == d.URL {
				return fmt.Errorf("the document is already %s's: a document serves one channel", c.Name)
			}
		}
	}
	return nil
}

// AddDoc gives a client one more document, a backup after the others.
func (s Store) AddDoc(name string, d Doc) (Client, error) {
	c, err := s.Get(name)
	if err != nil {
		return Client{}, err
	}
	if len(c.Docs()) >= maxDocs {
		return Client{}, fmt.Errorf("%s has %d documents already, the most a client gets", name, maxDocs)
	}
	if err := s.checkDoc(d); err != nil {
		return Client{}, err
	}
	c.Backups = append(c.Backups, d)
	return c, s.Save(c)
}

// RemoveDoc takes the i-th document (0 is the main one) from a client.
// Without its main document, the first backup becomes the main one and
// the context stays.
func (s Store) RemoveDoc(name string, i int) (Client, error) {
	c, err := s.Get(name)
	if err != nil {
		return Client{}, err
	}
	docs := c.Docs()
	switch {
	case i < 0 || i >= len(docs):
		return Client{}, fmt.Errorf("%s has no document %d (it has %d)", name, i+1, len(docs))
	case len(docs) == 1:
		return Client{}, fmt.Errorf("%s has only this document: give it another first, or revoke the client", name)
	}
	ctx := c.context()
	docs = append(docs[:i:i], docs[i+1:]...)
	c.Transport, c.URL = docs[0].Transport, docs[0].URL
	c.Backups = append([]Doc(nil), docs[1:]...)
	if len(c.Backups) == 0 {
		c.Backups = nil
	}
	c.Context = ""
	if ctx != c.URL {
		c.Context = ctx
	}
	return c, s.Save(c)
}

// TakeDoc removes the i-th document at the owner's request (RemoveDoc); it
// goes back to the pool after a quarantine (docstate.go).
func (s Store) TakeDoc(name string, i int) (Client, error) {
	c, err := s.Get(name)
	if err != nil {
		return Client{}, err
	}
	docs := c.Docs()
	if c, err = s.RemoveDoc(name, i); err != nil {
		return c, err
	}
	return c, s.quarantine([]Doc{docs[i]}, name, time.Now())
}

// ---- the pool ----

func (s Store) poolPath() string { return filepath.Join(s.Root, "docs-pool.txt") }

// pool reads the documents the owner prepared, in file order.
func (s Store) pool() ([]Doc, error) {
	f, err := os.Open(s.poolPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Doc
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		d, err := parseDoc(strings.Fields(line))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", s.poolPath(), n, err)
		}
		out = append(out, d)
	}
	return out, sc.Err()
}

// parseDoc reads "type url" or "url" (the type from the link).
func parseDoc(f []string) (Doc, error) {
	switch len(f) {
	case 1:
		return Doc{Transport: transportOf(f[0]), URL: f[0]}, nil
	case 2:
		return Doc{Transport: f[0], URL: f[1]}, nil
	}
	return Doc{}, errors.New(`expected "type url" or "url"`)
}

// poolDoc is a pool document and who uses it ("" when free), or why it
// may not be handed out (State: dead or quarantine, docstate.go).
type poolDoc struct {
	Doc
	User  string
	State *docState
}

// free: nobody uses it and it may be handed out.
func (d poolDoc) free() bool { return d.User == "" && d.State == nil }

func (s Store) poolStatus() ([]poolDoc, error) {
	docs, err := s.pool()
	if err != nil {
		return nil, err
	}
	users, err := s.docUsers()
	if err != nil {
		return nil, err
	}
	st, err := s.loadDocsState()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]poolDoc, len(docs))
	for i, d := range docs {
		out[i] = poolDoc{Doc: d, User: users[d.URL]}
		if st.blocked(d.URL, now) {
			out[i].State = st.Docs[d.URL]
		}
	}
	return out, nil
}

// freeDoc is the first free pool document for c: of its main document's
// type first (the pool's Yandex documents may hit SmartCaptcha).
func (s Store) freeDoc(c Client) (Doc, error) {
	docs, err := s.poolStatus()
	if err != nil {
		return Doc{}, err
	}
	var other *Doc
	for i := range docs {
		d := docs[i]
		if !d.free() || !transports[d.Transport] || validURL(d.URL) != nil {
			continue
		}
		if d.Transport == c.Transport {
			return d.Doc, nil
		}
		if other == nil {
			other = &docs[i].Doc
		}
	}
	if other != nil {
		return *other, nil
	}
	return Doc{}, fmt.Errorf("no free documents in %s: add some (reflux pool add <url>)", s.poolPath())
}

// addToPool appends documents the pool does not have yet; it returns how
// many went in.
func (s Store) addToPool(docs []Doc) (int, error) {
	have, err := s.pool()
	if err != nil {
		return 0, err
	}
	known := map[string]bool{}
	for _, d := range have {
		known[d.URL] = true
	}
	var b strings.Builder
	for _, d := range docs {
		if !transports[d.Transport] {
			return 0, fmt.Errorf("unsupported transport %q for %s", d.Transport, d.URL)
		}
		if err := validURL(d.URL); err != nil {
			return 0, err
		}
		if !known[d.URL] {
			known[d.URL] = true
			fmt.Fprintf(&b, "%s %s\n", d.Transport, d.URL)
		}
	}
	if b.Len() == 0 {
		return 0, nil
	}
	f, err := os.OpenFile(s.poolPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	// Document links give access with a key: the owner's eyes only.
	return strings.Count(b.String(), "\n"), os.Chmod(s.poolPath(), 0o600)
}

// ---- commands ----

// cmdDocs lists, adds and removes a client's documents.
func cmdDocs(s Store, args []string, stdout io.Writer) error {
	fs := newFlagSet("docs")
	transport := fs.String("transport", "", "carrier type of an added document (default: from the link)")
	noApply := fs.Bool("no-apply", false, "do not restart the node")
	var pos []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) == 0 {
		return errors.New("docs: name a client (reflux docs <name> [add <url|pool> | remove <n>])")
	}
	c, err := s.Get(pos[0])
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 1:
		return printDocs(s, c, stdout)
	case pos[1] == "add" && len(pos) == 3:
		d := Doc{Transport: *transport, URL: pos[2]}
		if pos[2] == "pool" {
			if d, err = s.freeDoc(c); err != nil {
				return err
			}
		} else if d.Transport == "" {
			d.Transport = transportOf(d.URL)
		}
		if c, err = s.AddDoc(c.Name, d); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: document %d added (%s, backup).\n", c.Name, len(c.Docs()), d.Transport)
	case pos[1] == "remove" && len(pos) == 3:
		n, err := strconv.Atoi(pos[2])
		if err != nil {
			return fmt.Errorf("docs remove: %q is not a document number", pos[2])
		}
		if c, err = s.TakeDoc(c.Name, n-1); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: document %d removed.\n", c.Name, n)
	default:
		return errors.New("docs: reflux docs <name> [add <url|pool> | remove <n>]")
	}
	if !*noApply && c.Active(time.Now()) {
		if err := apply(s, stdout); err != nil {
			return fmt.Errorf("changed, but restarting the node failed: %w", err)
		}
	}
	if err := printDocs(s, c, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nThe app needs the new link: reflux show %s\n", c.Name)
	return nil
}

// printDocs shows a client's documents and which one carries the traffic.
func printDocs(s Store, c Client, stdout io.Writer) error {
	mode := "classic exit (classic and Session apps)"
	if c.session() {
		mode = "Session exit (Session apps only)"
	}
	active := -1
	if st, ok := nodeStatuses(s, activeClients([]Client{c}, time.Now()))[c.Name]; ok {
		active = c.docIndex(st.Active)
	}
	fmt.Fprintf(stdout, "%s: %d document(s), %s\n", c.Name, len(c.Docs()), mode)
	for i, d := range c.Docs() {
		role, mark := "backup", ""
		if i == 0 {
			role = "main"
		}
		if i == active {
			mark = "  <- traffic"
		}
		fmt.Fprintf(stdout, "  %d  %-7s %-6s %s%s\n", i+1, d.Transport, role, d.URL, mark)
	}
	return nil
}

// cmdPool lists the pool or adds documents to it.
func cmdPool(s Store, args []string, stdout io.Writer) error {
	if len(args) > 0 && args[0] == "add" {
		var docs []Doc
		for _, a := range args[1:] {
			docs = append(docs, Doc{Transport: transportOf(a), URL: a})
		}
		if len(docs) == 0 {
			return errors.New("pool add: give document links")
		}
		n, err := s.addToPool(docs)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%d document(s) added to %s.\n", n, s.poolPath())
		return nil
	}
	if len(args) > 0 && args[0] == "check" {
		changes, changed, err := s.checkDocs(time.Now())
		if changed {
			if err := apply(s, stdout); err != nil {
				return err
			}
		}
		for _, ch := range changes {
			id, a := docChangeMessage(ch)
			fmt.Fprintln(stdout, stripTags(tr(langEN, id, a...)))
		}
		if err == nil && len(changes) == 0 {
			fmt.Fprintln(stdout, "Checked: no document died (one bad answer is not enough; it takes two checks in a row).")
		}
		return err
	}
	if len(args) > 0 && args[0] == "recheck" {
		back, still, err := s.recheckDead(args[1:], time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Back: %d (in the pool after %d h of quarantine), still dead: %d.\n", back, int(docQuarantine.Hours()), still)
		return nil
	}
	if len(args) > 0 {
		return errors.New("pool: reflux pool [add <url>... | check | recheck [<url>...]]")
	}
	docs, err := s.poolStatus()
	if err != nil {
		return err
	}
	st, err := s.loadDocsState()
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		fmt.Fprintf(stdout, "The pool is empty: reflux pool add <url>... (%s)\n", s.poolPath())
	}
	free := 0
	for _, d := range docs {
		user := "free"
		switch {
		case d.User != "":
			user = d.User
		case d.State != nil && d.State.State == stateQuarantine:
			user = "quarantine until " + d.State.Until.Local().Format("02.01 15:04")
		case d.State != nil:
			user = d.State.State
		default:
			free++
		}
		fmt.Fprintf(stdout, "  %-7s %-12s %s\n", d.Transport, user, d.URL)
	}
	if len(docs) > 0 {
		fmt.Fprintf(stdout, "%d of %d free.\n", free, len(docs))
	}
	if dead := st.deadDocs(); len(dead) > 0 {
		fmt.Fprintf(stdout, "\nDead (reflux pool recheck [<url>...] to check them again):\n")
		for _, d := range dead {
			whose := "from the pool"
			if d.Client != "" {
				whose = "was " + d.Client + "'s"
			}
			fmt.Fprintf(stdout, "  %-7s %-16s since %s  %s\n    %s\n", d.Transport, whose, d.Since.Local().Format("02.01 15:04"), d.URL, d.Reason)
		}
	}
	return nil
}

// stripTags drops the HTML tags of a bot message for the terminal.
func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return html.UnescapeString(b.String())
}
