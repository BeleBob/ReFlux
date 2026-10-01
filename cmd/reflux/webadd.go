package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The panel's add page: a name and a note; the document from the pool or
// by link, with backups from the pool; the access time and whether the
// node starts now; the Telegram account the client belongs to.

const maxNote = 100 // characters

type poolChoice struct {
	URL, Label, Transport string
}

type tgChoice struct {
	ID    int64
	Label string
}

type addData struct {
	Free     []poolChoice // the pool's free documents
	Backups  []int        // how many backups the pool allows: 0, 1, ...
	Accounts []tgChoice   // Telegram accounts clients are linked to
	Suggest  string       // a free name
	Pool     string       // the pool file
}

// shortDoc keeps a document link's end: …/public/AbCd/EfGh.
func shortDoc(u string) string {
	parts := strings.Split(strings.TrimRight(u, "/"), "/")
	if len(parts) <= 3 {
		return u
	}
	return "…/" + strings.Join(parts[len(parts)-2:], "/")
}

// knownAccounts are the Telegram accounts clients are linked to, once each.
func knownAccounts(clients []Client) []TGAccount {
	seen := map[int64]bool{}
	var out []TGAccount
	for _, c := range clients {
		if c.Telegram != nil && !seen[c.Telegram.ID] {
			seen[c.Telegram.ID] = true
			out = append(out, *c.Telegram)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func (w *webServer) addForm(r *http.Request) (string, pageData, error) {
	d := addData{Pool: w.s.poolPath(), Suggest: "client1"}
	docs, err := w.s.poolStatus()
	if err != nil {
		return "add", pageData{Title: tr(w.lang(), "web.add.title"), Active: "add", Body: d}, err
	}
	for _, p := range docs {
		if p.User == "" && transports[p.Transport] && validURL(p.URL) == nil {
			d.Free = append(d.Free, poolChoice{URL: p.URL, Label: p.Transport + " · " + shortDoc(p.URL), Transport: p.Transport})
		}
	}
	// mail.ru first: the pool's Yandex documents may hit SmartCaptcha.
	sort.SliceStable(d.Free, func(i, j int) bool { return d.Free[i].Transport == "mailru" && d.Free[j].Transport != "mailru" })
	for n := 0; n < maxDocs && n <= len(d.Free); n++ {
		d.Backups = append(d.Backups, n)
	}
	clients, _ := w.s.List()
	taken := map[string]bool{}
	for _, c := range clients {
		taken[c.Name] = true
	}
	for i := 1; taken[d.Suggest]; i++ {
		d.Suggest = "client" + strconv.Itoa(i+1)
	}
	for _, a := range knownAccounts(clients) {
		d.Accounts = append(d.Accounts, tgChoice{ID: a.ID, Label: a.String()})
	}
	return "add", pageData{Title: tr(w.lang(), "web.add.title"), Active: "add", Body: d}, nil
}

// add creates a client from the add page's form.
func (w *webServer) add(r *http.Request) (string, error) {
	l := w.lang()
	now := time.Now()
	name := strings.TrimSpace(r.FormValue("name"))
	note := strings.Join(strings.Fields(r.FormValue("note")), " ")
	if len([]rune(note)) > maxNote {
		return "", errors.New(tr(l, "web.add.note.long", maxNote))
	}
	// The access time: a choice and a date, or (an older form) a phrase.
	var when time.Time
	var err error
	if phrase := strings.TrimSpace(r.FormValue("expires")); phrase != "" {
		when, err = parseExpiry(phrase, now)
	} else {
		when, err = expiryFrom(orDefault(r.FormValue("how"), "never"), r.FormValue("date"), time.Time{}, now)
	}
	if err != nil {
		return "", err
	}
	backups, err := strconv.Atoi(orDefault(r.FormValue("backups"), "0"))
	if err != nil || backups < 0 || backups >= maxDocs {
		return "", fmt.Errorf("bad number of backup documents %q", r.FormValue("backups"))
	}
	unlock, err := w.s.Lock(lockWait)
	if err != nil {
		return "", err
	}
	defer unlock()

	// The main document, then enough free ones for the backups.
	docs, err := w.s.poolStatus()
	if err != nil {
		return "", err
	}
	// A link typed in wins over the pool's choice: it is what was meant.
	var main Doc
	if r.FormValue("src") == "pool" && strings.TrimSpace(r.FormValue("url")) == "" {
		for _, p := range docs {
			if p.URL == r.FormValue("pooldoc") && p.User == "" {
				main = p.Doc
			}
		}
		if main.URL == "" {
			return "", errors.New(tr(l, "web.add.notfree"))
		}
	} else {
		main.URL = strings.TrimSpace(r.FormValue("url"))
		main.Transport = r.FormValue("type")
		if main.Transport == "" || main.Transport == "auto" {
			main.Transport = transportOf(main.URL)
		}
	}
	free := 0
	for _, p := range docs {
		if p.User == "" && p.URL != main.URL && transports[p.Transport] {
			free++
		}
	}
	if free < backups {
		return "", errors.New(tr(l, "web.add.nofree", backups, free))
	}
	var account *TGAccount
	if id := r.FormValue("tg"); id != "" {
		clients, _ := w.s.List()
		for _, a := range knownAccounts(clients) {
			if strconv.FormatInt(a.ID, 10) == id {
				account = &a
			}
		}
		if account == nil {
			return "", fmt.Errorf("unknown Telegram account %q", id)
		}
	}

	c, err := w.s.Add(name, main.Transport, main.URL)
	if err != nil {
		return "", err
	}
	c.Note, c.Expires, c.Telegram = note, when, account
	c.Paused = r.FormValue("start") == ""
	if err := w.s.Save(c); err != nil {
		return "", err
	}
	for i := 0; i < backups; i++ {
		d, err := w.s.freeDoc(c)
		if err == nil {
			c, err = w.s.AddDoc(c.Name, d)
		}
		if err != nil {
			return "", fmt.Errorf("%s added, but not its backups: %w", c.Name, err)
		}
	}
	if err := apply(w.s, io.Discard); err != nil {
		return "", fmt.Errorf("%s: %w", tr(l, "ui.add.nostart", c.Name), err)
	}
	return "/c/" + url.PathEscape(c.Name) + "?ok=added", nil
}

// setNote changes a client's note (the client page's form).
func setNote(s Store, name, note string, l lang) error {
	note = strings.Join(strings.Fields(note), " ")
	if len([]rune(note)) > maxNote {
		return errors.New(tr(l, "web.add.note.long", maxNote))
	}
	c, err := s.Get(name)
	if err != nil {
		return err
	}
	c.Note = note
	return s.Save(c)
}
