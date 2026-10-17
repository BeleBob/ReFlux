package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
	"github.com/p1neappleXpress/OpenFlux/transport"
)

// cups.online rooms. Next to its mail.ru documents a client gets a list of
// cups.online rooms: a Doc of type cupsonline whose URL is the packed list.
// The egress makes the rooms (reflux-egress create-rooms), from the address
// the nodes come from; the node joins them and the link carries the list.
// Rooms carry many times what a mail.ru document does, so they get the
// higher priority (docPriority) and the documents stand by: when the rooms
// fail, the channel goes on over mail.ru, which also brings the app the
// new rooms (the node's ConfigOffer).
//
// The document check (doccheck.go) looks at the rooms too: a list with a
// room gone gets new rooms in its place, never pool documents, and every
// active client without rooms gets them, one client per check (cups.online
// refuses rooms made too fast), unless cups.json turns rooms off or the
// owner took a client's rooms away (Client.NoCups). When cups.online makes
// no rooms, it is asked again after cupsRetry and the doctor says so.

const (
	cupsType = "cupsonline"
	// cupsRooms is how many rooms a channel gets: each room is a lane of
	// its own, and 8 carried no more than 4 (measured 2026-10-17).
	cupsRooms = 4
	// cupsPriority is the rooms' priority, docsPriority the documents'
	// next to them; without rooms the documents keep 100.
	cupsPriority = 100
	docsPriority = 50
	cupsRetry    = time.Hour
)

func isCups(d Doc) bool { return d.Transport == cupsType }

// cupsIndex is the position of c's rooms among its documents, -1 if none.
func (c Client) cupsIndex() int {
	for i, d := range c.Docs() {
		if isCups(d) {
			return i
		}
	}
	return -1
}

// roomCount is how many rooms a list holds, 0 when it is not one.
func roomCount(list string) int {
	ids, _, err := transport.UnpackRooms(list)
	if err != nil {
		return 0
	}
	return len(ids)
}

// cupsSettings is cups.json; without it rooms are on.
type cupsSettings struct {
	Off bool `json:"off,omitempty"`
}

func (s Store) cupsSettingsPath() string { return filepath.Join(s.Root, "cups.json") }

func (s Store) cupsOff() bool {
	var cs cupsSettings
	b, err := os.ReadFile(s.cupsSettingsPath())
	if err != nil {
		return false
	}
	return json.Unmarshal(b, &cs) == nil && cs.Off
}

func (s Store) setCupsOff(off bool) error {
	return writeJSON(s.cupsSettingsPath(), cupsSettings{Off: off})
}

// cupsState is how making rooms went (docs-state.json).
type cupsState struct {
	Error  string    `json:"error,omitempty"` // the last failure; "" once rooms were made
	Since  time.Time `json:"since,omitzero"`  // failing since
	Failed time.Time `json:"failed,omitzero"` // the last failure
	Made   time.Time `json:"made,omitzero"`   // the last rooms made
}

var errCupsWait = errors.New("cups.online made no rooms a while ago; asked again after an hour")

// createRoomsInEgress has the egress make n rooms.
func (s Store) createRoomsInEgress(n int) (string, int, error) {
	var out strings.Builder
	if err := runDockerIn(strings.NewReader(""), &out, "exec", "reflux-egress", "reflux-egress", "create-rooms", strconv.Itoa(n)); err != nil {
		return "", 0, fmt.Errorf("the egress could not make rooms: %w", err)
	}
	var r struct {
		List  string `json:"list"`
		Rooms int    `json:"rooms"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &r); err != nil {
		return "", 0, fmt.Errorf("the egress's answer: %w", err)
	}
	switch {
	case r.Error != "":
		return "", 0, errors.New(r.Error)
	case r.Rooms == 0 || validURL(r.List) != nil:
		return "", 0, errors.New("the egress made no usable room list")
	}
	return r.List, r.Rooms, nil
}

// makeRooms makes a channel's rooms, unless cups.online refused less than
// cupsRetry ago (force asks anyway); st records how it went.
func (s Store) makeRooms(st *docsState, now time.Time, force bool) (Doc, error) {
	if !force && st.Cups.Error != "" && now.Sub(st.Cups.Failed) < cupsRetry {
		return Doc{}, errCupsWait
	}
	list, _, err := s.createRoomsInEgress(cupsRooms)
	if err != nil {
		if st.Cups.Error == "" {
			st.Cups.Since = now
		}
		st.Cups.Error, st.Cups.Failed = err.Error(), now
		return Doc{}, err
	}
	st.Cups = cupsState{Made: now}
	return Doc{Transport: cupsType, URL: list}, nil
}

// addCups gives a client rooms, after its documents.
func (s Store) addCups(name string, d Doc) (Client, error) {
	c, err := s.Get(name)
	if err != nil {
		return Client{}, err
	}
	switch {
	case c.cupsIndex() >= 0:
		return Client{}, fmt.Errorf("%s has cups.online rooms already", name)
	case len(c.Docs()) >= maxDocs:
		return Client{}, fmt.Errorf("%s has %d documents already, the most a client gets", name, maxDocs)
	}
	if err := validURL(d.URL); err != nil {
		return Client{}, err
	}
	c.Backups = append(c.Backups, d)
	c.NoCups = false
	return c, s.Save(c)
}

// ensureCups gives rooms to the first active client that should have them
// and has none: one per check. Failures go to st, not to the changes: the
// doctor tells the owner while cups.online makes no rooms.
func (s Store) ensureCups(st *docsState, now time.Time) ([]docChange, error) {
	if s.cupsOff() {
		return nil, nil
	}
	clients, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		if !c.Active(now) || c.NoCups || c.cupsIndex() >= 0 || len(c.Docs()) >= maxDocs {
			continue
		}
		d, err := s.makeRooms(st, now, false)
		if err != nil {
			return nil, nil
		}
		_, err = s.addCups(c.Name, d)
		return []docChange{{Cups: true, Client: c.Name, New: &d, Err: err}}, nil
	}
	return nil, nil
}

// replaceCups gives a client new rooms in place of a list with gone rooms.
// When cups.online makes none, the old list goes (the channel stays on
// mail.ru) and a later check brings new ones. changed: client.json changed.
func (s Store) replaceCups(name string, dead Doc, st *docsState, now time.Time) (fresh *Doc, changed bool, err error) {
	c, err := s.Get(name)
	if err != nil {
		return nil, false, err
	}
	i := -1
	for j, d := range c.Docs() {
		if d.URL == dead.URL {
			i = j
		}
	}
	if i < 0 {
		return nil, false, errors.New("the rooms are no longer the client's")
	}
	d, err := s.makeRooms(st, now, true)
	if err != nil {
		if len(c.Docs()) == 1 {
			return nil, false, err
		}
		if _, rerr := s.RemoveDoc(name, i); rerr != nil {
			return nil, false, rerr
		}
		return nil, true, err
	}
	if i == 0 {
		c.URL = d.URL
	} else {
		c.Backups[i-1] = d
	}
	return &d, true, s.Save(c)
}

// ---- the owner's side ----

// cupsChangeMessage tells the owner what happened to a client's rooms.
func cupsChangeMessage(ch docChange) (string, []any) {
	e := html.EscapeString
	switch {
	case ch.Doc.URL == "" && ch.Err != nil:
		return "docs.cups.failed", []any{e(ch.Client), e(ch.Err.Error())}
	case ch.Doc.URL == "":
		return "docs.cups.added", []any{e(ch.Client), roomCount(ch.New.URL)}
	case ch.New == nil && ch.Err != nil:
		return "docs.cups.dropped", []any{e(ch.Client), e(ch.Reason), e(ch.Err.Error())}
	}
	return "docs.cups.replaced", []any{e(ch.Client), e(ch.Reason)}
}

// cups is the doctor's line about making rooms.
func (d *doctor) cups(s Store) {
	st, err := s.loadDocsState()
	if err != nil || s.cupsOff() || st.Cups.Error == "" {
		return
	}
	d.warn("cups", "cups.failing", st.Cups.Since.Local().Format("02.01 15:04"), st.Cups.Error)
}

// cupsLines adds the rooms' switch to the owner bot's settings.
func (b *bot) cupsLines(t *strings.Builder, kb keyboard) keyboard {
	state, button := "ui.cups.on", "b.cups.off"
	if b.s.cupsOff() {
		state, button = "ui.cups.off", "b.cups.on"
	}
	t.WriteString("\n" + b.tr(state) + "\n")
	return append(kb, []telegram.Button{b.btn(button, "cupsw")})
}

// toggleCups is the settings button.
func (b *bot) toggleCups() screen {
	if err := b.s.setCupsOff(!b.s.cupsOff()); err != nil {
		return b.failed(err, b.btn("b.home", "home"))
	}
	return b.settingsScreen()
}

// giveCups is the documents screen's button: rooms for this client now.
func (b *bot) giveCups(name string) screen {
	return b.changeDocs(name, func() (string, error) {
		c, err := cupsNow(b.s, name, time.Now())
		if err != nil {
			return "", err
		}
		return b.tr("ui.docs.cups", roomCount(c.Docs()[c.cupsIndex()].URL)), nil
	})
}

// cupsNow makes rooms for a client at once, whatever the last failure
// was. The caller holds the store lock.
func cupsNow(s Store, name string, now time.Time) (Client, error) {
	st, err := s.loadDocsState()
	if err != nil {
		return Client{}, err
	}
	c, err := s.Get(name)
	if err != nil {
		return Client{}, err
	}
	var d Doc
	if i := c.cupsIndex(); i >= 0 {
		if d, err = s.makeRooms(&st, now, true); err == nil {
			if i == 0 {
				c.URL = d.URL
			} else {
				c.Backups[i-1] = d
			}
			c.NoCups = false
			err = s.Save(c)
		}
	} else if d, err = s.makeRooms(&st, now, true); err == nil {
		c, err = s.addCups(name, d)
	}
	if serr := s.saveDocsState(st); err == nil {
		err = serr
	}
	return c, err
}

// cmdCups is `reflux cups [on | off | add <name> | remove <name>]`.
func cmdCups(s Store, args []string, stdout io.Writer) error {
	usage := errors.New("cups: reflux cups [on | off | add <name> | remove <name>]")
	switch {
	case len(args) == 0:
		return printCups(s, stdout)
	case len(args) == 1 && (args[0] == "on" || args[0] == "off"):
		if err := s.setCupsOff(args[0] == "off"); err != nil {
			return err
		}
		if args[0] == "off" {
			fmt.Fprintln(stdout, "cups.online rooms: off. Clients keep the rooms they have (reflux cups remove <name>); new ones get none.")
		} else {
			fmt.Fprintln(stdout, "cups.online rooms: on. Active clients without rooms get them at the document checks, one per check (every 15 minutes).")
		}
		return nil
	case len(args) == 2 && (args[0] == "add" || args[0] == "remove"):
	default:
		return usage
	}
	name := args[1]
	var c Client
	var err error
	if args[0] == "add" {
		if c, err = cupsNow(s, name, time.Now()); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: %d cups.online rooms (the existing ones replaced, if any).\n", name, roomCount(c.Docs()[c.cupsIndex()].URL))
	} else {
		if c, err = s.Get(name); err != nil {
			return err
		}
		i := c.cupsIndex()
		if i < 0 {
			return fmt.Errorf("%s has no cups.online rooms", name)
		}
		if c, err = s.TakeDoc(name, i); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s: cups.online rooms removed; it gets none until reflux cups add %s.\n", name, name)
	}
	if c.Active(time.Now()) {
		if err := apply(s, stdout); err != nil {
			return fmt.Errorf("changed, but restarting the node failed: %w", err)
		}
	}
	fmt.Fprintln(stdout, "The app picks the change up over the channel by itself (the node's ConfigOffer).")
	return nil
}

func printCups(s Store, stdout io.Writer) error {
	clients, err := s.List()
	if err != nil {
		return err
	}
	state := "on"
	if s.cupsOff() {
		state = "off"
	}
	fmt.Fprintf(stdout, "cups.online rooms: %s (%d per client, priority %d; mail.ru documents %d next to them)\n", state, cupsRooms, cupsPriority, docsPriority)
	if st, err := s.loadDocsState(); err == nil && st.Cups.Error != "" {
		fmt.Fprintf(stdout, "cups.online makes no rooms since %s: %s\n", st.Cups.Since.Local().Format("02.01 15:04"), st.Cups.Error)
	}
	for _, c := range clients {
		what := "no rooms"
		switch i := c.cupsIndex(); {
		case i >= 0:
			what = fmt.Sprintf("%d rooms", roomCount(c.Docs()[i].URL))
		case c.NoCups:
			what = "no rooms (taken away: reflux cups add " + c.Name + ")"
		}
		fmt.Fprintf(stdout, "  %-16s %s\n", c.Name, what)
	}
	return nil
}
