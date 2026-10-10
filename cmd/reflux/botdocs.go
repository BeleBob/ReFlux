package main

import (
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

// The bot's documents screen: a client's documents, which one carries the
// traffic, adding one from the pool or by link, removing one (docs.go).

func (b *bot) docsScreen(name string) screen {
	c, err := b.s.Get(name)
	if err != nil {
		return b.failed(err, b.btn("b.clients", "cls"))
	}
	v := b.clientViews([]Client{c})[0]
	active := -1
	if v.status != nil && v.status.online {
		active = v.status.doc
	}
	docs := c.Docs()
	var t strings.Builder
	t.WriteString(b.tr("ui.docs.title", html.EscapeString(c.Name)) + "\n\n")
	for i, d := range docs {
		role := b.tr("ui.docs.backup")
		if i == 0 {
			role = b.tr("ui.docs.main")
		}
		if i == active {
			role += " · " + b.tr("ui.docs.traffic")
		}
		fmt.Fprintf(&t, "%d. %s · %s\n<code>%s</code>\n", i+1, html.EscapeString(d.Transport), role, html.EscapeString(d.URL))
	}
	if c.session() {
		t.WriteString("\n" + b.tr("ui.docs.session", len(docs)) + "\n")
	} else {
		t.WriteString("\n" + b.tr("ui.docs.classic") + "\n")
	}
	free := b.s.freeCount()
	t.WriteString(b.tr("ui.docs.pool", free))
	var kb keyboard
	if len(docs) < maxDocs {
		var row []telegram.Button
		if free > 0 {
			row = append(row, b.btn("b.docs.pool", "dp:"+name))
		}
		kb = append(kb, append(row, b.btn("b.docs.url", "du:"+name)))
	}
	if len(docs) > 1 {
		var row []telegram.Button
		for i := range docs {
			row = append(row, b.btn("b.docs.remove", "dr:"+name+":"+strconv.Itoa(i), i+1))
		}
		kb = append(kb, row)
	}
	return screen{t.String(), append(kb, []telegram.Button{b.btn("b.qr", "qr:"+name), b.btn("b.back", "c:"+name)})}
}

// changeDocs runs fn on a client's documents under the lock and restarts
// its node; the screen then tells what happened.
func (b *bot) changeDocs(name string, fn func() (string, error)) screen {
	var note string
	err := b.change(func() error {
		var err error
		if note, err = fn(); err != nil {
			return err
		}
		return apply(b.s, io.Discard)
	})
	if err != nil {
		return b.failed(err, b.btn("b.back", "dc:"+name))
	}
	sc := b.docsScreen(name)
	sc.text = note + "\n\n" + sc.text
	return sc
}

func (b *bot) addDoc(name string, d *Doc) screen {
	return b.changeDocs(name, func() (string, error) {
		c, err := b.s.Get(name)
		if err != nil {
			return "", err
		}
		if d == nil {
			doc, err := b.s.freeDoc(c)
			if err != nil {
				return "", err
			}
			d = &doc
		}
		if c, err = b.s.AddDoc(name, *d); err != nil {
			return "", err
		}
		return b.tr("ui.docs.added", len(c.Docs())), nil
	})
}

func (b *bot) removeDocAsk(name string, i int) screen {
	return screen{b.tr("ui.docs.remove.ask", i+1, html.EscapeString(name)), keyboard{
		{b.btn("b.docs.remove.confirm", "dr!:"+name+":"+strconv.Itoa(i)+":"+stamp(), i+1)},
		{b.btn("b.cancel", "dc:"+name)},
	}}
}

func (b *bot) removeDoc(name string, i int) screen {
	return b.changeDocs(name, func() (string, error) {
		if _, err := b.s.TakeDoc(name, i); err != nil {
			return "", err
		}
		return b.tr("ui.docs.removed", i+1), nil
	})
}

// docsPress handles the documents screen's buttons: dc, dp, du, dr, dr!.
func (b *bot) docsPress(kind, arg string) screen {
	switch kind {
	case "dc":
		return b.docsScreen(arg)
	case "dp":
		return b.addDoc(arg, nil)
	case "du":
		b.await = awaiting{kind: "doc", name: arg, at: time.Now()}
		return screen{b.tr("ui.docs.prompt", html.EscapeString(arg)), keyboard{{b.btn("b.cancel", "dc:"+arg)}}}
	case "dr":
		name, i, _ := strings.Cut(arg, ":")
		n, err := strconv.Atoi(i)
		if err != nil {
			return b.docsScreen(name)
		}
		return b.removeDocAsk(name, n)
	case "dr!":
		f := strings.Split(arg, ":")
		if len(f) != 3 {
			return b.home()
		}
		n, err := strconv.Atoi(f[1])
		if err != nil || !fresh(f[2]) {
			sc := b.docsScreen(f[0])
			sc.text = b.tr("ui.expired.button") + "\n\n" + sc.text
			return sc
		}
		return b.removeDoc(f[0], n)
	}
	return b.home()
}

// freeCount is how many pool documents nobody uses.
func (s Store) freeCount() int {
	docs, err := s.poolStatus()
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range docs {
		if d.free() && transports[d.Transport] {
			n++
		}
	}
	return n
}
