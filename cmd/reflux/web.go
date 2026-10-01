package main

import (
	"bytes"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"openflux/share"
)

// The web panel: the bot's screens as pages, for the home network. It
// serves on the host's LAN address only, answers private addresses only,
// and lets in the owner's trusted addresses and browsers signed in with a
// one-time link (see webauth.go).

//go:embed web/panel.html
var webFiles embed.FS

const sessionCookie = "reflux_session"

type webServer struct {
	s    Store
	cfg  webConfig
	tmpl *template.Template
	// mu runs the handlers one at a time: they run docker and swap
	// dockerStderr, like the bot's commands.
	mu    sync.Mutex
	stats *sampler
}

func newWebServer(s Store, cfg webConfig) (*webServer, error) {
	w := &webServer{s: s, cfg: cfg}
	w.stats = newSampler(s, &w.mu)
	t, err := template.New("panel").Funcs(webFuncs(langRU)).ParseFS(webFiles, "web/panel.html")
	if err != nil {
		return nil, err
	}
	w.tmpl = t
	return w, nil
}

// webFuncs are the templates' helpers in l.
func webFuncs(l lang) template.FuncMap {
	return template.FuncMap{
		"t":     func(id string, args ...any) string { return tr(l, id, args...) },
		"dur":   func(d time.Duration) string { return durationIn(l, d) },
		"mbit":  func(v float64) string { return mbit(l, v) },
		"ago":   func(t time.Time) string { return ago(l, t) },
		"bytes": humanBytes,
		"spark": spark,
		"pct":   pct,
		"bar":   barLevel,
		"f0":    func(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) },
		"f1":    func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
		"f2":    func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
		"hhmm":  func(t time.Time) string { return t.Local().Format("02.01 15:04:05") },
		"etext": func(e event) string { return e.Text(l) },
		"add":   func(a, b float64) float64 { return a + b },
		"sub": func(a, b uint64) uint64 {
			if b > a {
				return 0
			}
			return a - b
		},
	}
}

// lang is the owner's language: the bot's setting, Russian by default.
func (w *webServer) lang() lang {
	if c, err := w.s.loadBotConfig(); err == nil && c.Lang == string(langEN) {
		return langEN
	}
	return langRU
}

func (w *webServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", w.login)
	mux.HandleFunc("GET /{$}", w.page(w.home))
	mux.HandleFunc("GET /server", w.page(w.server))
	mux.HandleFunc("GET /events", w.page(w.events))
	mux.HandleFunc("GET /c/{name}", w.page(w.client))
	mux.HandleFunc("POST /c/{name}/show", w.page(w.show))
	mux.HandleFunc("POST /c/{name}/{action}", w.action(w.clientAction))
	mux.HandleFunc("GET /logs/{name}", w.page(w.logs))
	mux.HandleFunc("GET /add", w.page(w.addForm))
	mux.HandleFunc("POST /add", w.action(w.add))
	mux.HandleFunc("GET /doctor", w.page(w.doctor))
	mux.HandleFunc("GET /gateway", w.page(w.gateway))
	mux.HandleFunc("POST /gateway/{what}", w.action(w.gatewayAction))
	mux.HandleFunc("GET /speed", w.page(w.speedForm))
	mux.HandleFunc("POST /speed", w.page(w.speed))
	mux.HandleFunc("POST /logout", w.logout)
	return w.guard(mux)
}

// privateAddr: loopback or a private LAN address.
func privateAddr(a net.IP) bool {
	return a != nil && (a.IsLoopback() || a.IsPrivate())
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// guard lets in private addresses only, and only requests from the
// panel's own pages change anything: a POST must come from its origin.
func (w *webServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !privateAddr(remoteIP(r)) {
			http.Error(rw, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(rw, "forbidden: cross-site request", http.StatusForbidden)
			return
		}
		rw.Header().Set("X-Frame-Options", "DENY")
		rw.Header().Set("Referrer-Policy", "no-referrer")
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'")
		next.ServeHTTP(rw, r)
	})
}

// sameOrigin: the Origin (or, without it, the Referer) names this host.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	u, err := url.Parse(src)
	return err == nil && src != "" && u.Host == r.Host
}

// signedIn: a trusted address, or a valid session cookie.
func (w *webServer) signedIn(r *http.Request) bool {
	if ip := remoteIP(r); ip != nil && slices.Contains(w.cfg.Trusted, ip.String()) {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && w.s.validSession(c.Value)
}

func (w *webServer) login(rw http.ResponseWriter, r *http.Request) {
	if !w.s.useLogin(r.URL.Query().Get("t")) {
		w.render(rw, http.StatusForbidden, "signin", pageData{Error: tr(w.lang(), "web.login.bad")})
		return
	}
	id, err := w.s.newSession()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(rw, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionFor.Seconds()),
	})
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

func (w *webServer) logout(rw http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		w.s.endSession(c.Value)
	}
	http.SetCookie(rw, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	w.render(rw, http.StatusOK, "signin", pageData{Notice: tr(w.lang(), "web.logout.done")})
}

// pageData is what every page gets.
type pageData struct {
	Title   string
	Active  string // the menu item of the page
	Version string
	Notice  string
	Error   string
	Now     string
	// Refresh reloads the page after this many seconds (0: never).
	Refresh int
	// Back is the page an error came from.
	Back string
	Body any
}

// page wraps a handler that renders: sign-in, the lock, the language.
func (w *webServer) page(fn func(*http.Request) (string, pageData, error)) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !w.signedIn(r) {
			w.render(rw, http.StatusUnauthorized, "signin", pageData{})
			return
		}
		w.mu.Lock()
		name, data, err := fn(r)
		w.mu.Unlock()
		if err != nil {
			data.Error = err.Error()
		}
		if n := r.URL.Query().Get("ok"); n != "" && data.Notice == "" {
			data.Notice = tr(w.lang(), "web.ok."+n)
		}
		rw.Header().Set("Cache-Control", "no-store")
		w.render(rw, http.StatusOK, name, data)
	}
}

// action wraps a handler that changes something and redirects (so a
// reload does not repeat it); an error shows the page it came from.
func (w *webServer) action(fn func(*http.Request) (string, error)) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !w.signedIn(r) {
			w.render(rw, http.StatusUnauthorized, "signin", pageData{})
			return
		}
		w.mu.Lock()
		to, err := fn(r)
		w.mu.Unlock()
		if err != nil {
			back := ""
			if u, perr := url.Parse(r.Header.Get("Referer")); perr == nil && u.Host == r.Host {
				back = u.RequestURI()
			}
			w.render(rw, http.StatusBadRequest, "error", pageData{Error: err.Error(), Back: back})
			return
		}
		http.Redirect(rw, r, to, http.StatusSeeOther)
	}
}

func (w *webServer) render(rw http.ResponseWriter, status int, name string, data pageData) {
	l := w.lang()
	t, err := w.tmpl.Clone()
	if err == nil {
		t = t.Funcs(webFuncs(l))
	}
	data.Now = time.Now().Format("15:04:05")
	data.Version = version()
	var buf bytes.Buffer
	if err == nil {
		err = t.ExecuteTemplate(&buf, name, data)
	}
	if err != nil {
		log.Printf("web: %s: %v", name, err)
		http.Error(rw, "template error", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(status)
	rw.Write(buf.Bytes())
}

// ---- pages ----

// docRow is a document on the client page.
type docRow struct {
	Doc
	I, N, Priority int // I counts from 0, N from 1
	Main, Traffic  bool
}

// clientRow is a client on the home page.
type clientRow struct {
	Name, Mark, Owner, State, Access string
	Today                            dayTraffic
	Down, Up                         float64 // bytes per second, lately
}

// tiles are the host's vital signs on the home and server pages.
type tiles struct {
	Now            hostNow
	CPU, Mem, Temp []float64
	Lan, Egress    []float64 // down and up together, bytes per second
	RootDisk       *disk
	Ready          bool // the sampler has two readings
}

func (w *webServer) tiles() tiles {
	now, hist := w.stats.snapshot()
	t := tiles{Now: now, Ready: len(hist) > 0}
	for _, p := range hist {
		t.CPU = append(t.CPU, p.CPU)
		t.Mem = append(t.Mem, p.Mem)
		t.Temp = append(t.Temp, p.TempC)
		t.Lan = append(t.Lan, p.LanRx+p.LanTx)
		t.Egress = append(t.Egress, p.EgRx+p.EgTx)
	}
	for i := range now.Disks {
		if now.Disks[i].Mount == "/" {
			t.RootDisk = &now.Disks[i]
		}
	}
	return t
}

type homeData struct {
	Egress  *egressStatus
	Russia  string
	Clients []clientRow
	Tiles   tiles
	Events  []event
}

func (w *webServer) home(r *http.Request) (string, pageData, error) {
	l := w.lang()
	d := homeData{Tiles: w.tiles(), Events: w.s.readEvents(5)}
	if st, err := readEgressStatus(); err == nil {
		d.Egress = &st
		d.Russia = tr(l, ruMode(st).id)
	}
	clients, err := w.s.List()
	now := time.Now()
	for _, v := range viewClients(w.s, clients) {
		owner := ""
		if v.c.Telegram != nil {
			owner = v.c.Telegram.String()
		}
		p := accessPhrase(v.c, now)
		ct := w.stats.clientTraffic(v.c.Name, now)
		row := clientRow{Name: v.c.Name, Mark: v.mark(), Owner: owner, State: stateText(l, v),
			Access: tr(l, p.id, p.args...), Today: ct.Today}
		// Keepalives are not traffic worth a figure: from 50 kbit/s on.
		if n := len(ct.Rates); n > 0 && ct.Rates[n-1].Down+ct.Rates[n-1].Up >= 6250 {
			row.Down, row.Up = ct.Rates[n-1].Down, ct.Rates[n-1].Up
		}
		d.Clients = append(d.Clients, row)
	}
	return "home", pageData{Title: tr(l, "web.nav.home"), Active: "home", Refresh: 15, Body: d}, err
}

func (w *webServer) events(r *http.Request) (string, pageData, error) {
	return "events", pageData{Title: tr(w.lang(), "web.nav.events"), Active: "events", Refresh: 60, Body: w.s.readEvents(200)}, nil
}

type clientData struct {
	Row     clientRow
	Traffic clientTraffic
	// Days is the traffic of the last days as bars, Rate the speed over
	// the last hour (webbars.go).
	Days, Rate             template.HTML
	DaysLegend, RateLegend []legendEntry
	Node                   string
	Created                string
	Transport              string
	Paused                 bool
	Expires                string // for the date field
	// Shown only on the show page:
	Link, Key, URL string
	QR             template.URL // a data: URL of our own PNG
	// Docs are the channel's documents, Session whether there are several,
	// FreeDocs the pool's free ones (docs.go).
	Docs     []docRow
	Session  bool
	FreeDocs int
	Context  string
	CanAdd   bool
}

func (w *webServer) clientData(name string) (clientData, error) {
	l := w.lang()
	c, err := w.s.Get(name)
	if err != nil {
		return clientData{}, err
	}
	v := viewClients(w.s, []Client{c})[0]
	p := accessPhrase(c, time.Now())
	d := clientData{
		Row:       clientRow{Name: c.Name, Mark: v.mark(), State: stateText(l, v), Access: tr(l, p.id, p.args...)},
		Created:   c.Created.Local().Format(time.DateOnly),
		Transport: c.Transport,
		Paused:    c.Paused,
	}
	if c.Telegram != nil {
		d.Row.Owner = c.Telegram.String()
	}
	d.Traffic = w.stats.clientTraffic(c.Name, time.Now())
	d.Days, d.DaysLegend = w.trafficChart(l, c.Name, time.Now())
	d.Rate, d.RateLegend = rateChart(l, d.Traffic.Rates, time.Now())
	if !c.Expires.IsZero() {
		d.Expires = c.Expires.Local().AddDate(0, 0, -1).Format(time.DateOnly)
	}
	switch {
	case !v.active:
		d.Node = tr(l, "ui.node.stopped")
	case !v.running:
		d.Node = tr(l, "ui.node.notrunning")
	case v.status == nil:
		d.Node = tr(l, "ui.nostatus")
	default:
		d.Node = tr(l, "ui.node.up", durationIn(l, v.status.up))
	}
	d.Session, d.FreeDocs, d.Context = c.session(), w.s.freeCount(), c.context()
	doc := -1
	if v.status != nil && v.status.online {
		doc = v.status.doc
	}
	for i, x := range c.Docs() {
		d.Docs = append(d.Docs, docRow{I: i, N: i + 1, Doc: x, Priority: docPriority(i), Main: i == 0, Traffic: i == doc})
	}
	d.CanAdd = len(d.Docs) < maxDocs
	return d, nil
}

func (w *webServer) client(r *http.Request) (string, pageData, error) {
	d, err := w.clientData(r.PathValue("name"))
	if err != nil {
		return "error", pageData{}, err
	}
	return "client", pageData{Title: d.Row.Name, Active: "home", Body: d}, nil
}

// show is the client page with its access data: a POST, so it never sits
// in the history or a cache.
func (w *webServer) show(r *http.Request) (string, pageData, error) {
	d, err := w.clientData(r.PathValue("name"))
	if err != nil {
		return "error", pageData{}, err
	}
	c, _ := w.s.Get(d.Row.Name)
	key, err := w.s.Key(c.Name)
	if err != nil {
		return "error", pageData{}, err
	}
	link, err := share.MakeLink(shareConfig(c, key))
	if err != nil {
		return "error", pageData{}, err
	}
	png, err := share.PNG(link, 384)
	if err != nil {
		return "error", pageData{}, err
	}
	d.Link, d.Key, d.URL = link, key, c.URL
	d.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
	return "show", pageData{Title: d.Row.Name, Body: d}, nil
}

// clientAction changes a client: pause, resume, expire, rename, revoke.
func (w *webServer) clientAction(r *http.Request) (string, error) {
	name, action := r.PathValue("name"), r.PathValue("action")
	back := "/c/" + url.PathEscape(name)
	if _, err := w.s.Get(name); err != nil {
		return "", err
	}
	change := func(fn func() error) error {
		unlock, err := w.s.Lock(lockWait)
		if err != nil {
			return err
		}
		defer unlock()
		return fn()
	}
	access := func(fn func(*Client) error) error {
		return change(func() error { return setAccess(w.s, name, io.Discard, fn) })
	}
	switch action {
	case "pause":
		return back + "?ok=paused", access(func(c *Client) error { c.Paused = true; return nil })
	case "resume":
		return back + "?ok=resumed", access(func(c *Client) error {
			if !c.Expires.IsZero() && !time.Now().Before(c.Expires) {
				return errors.New(tr(w.lang(), "ui.resume.expired"))
			}
			c.Paused = false
			return nil
		})
	case "expire":
		how := r.FormValue("how")
		return back + "?ok=expiry", access(func(c *Client) error {
			t, err := expiryFrom(how, r.FormValue("date"), c.Expires, time.Now())
			if err == nil {
				c.Expires = t
			}
			return err
		})
	case "rename":
		to := strings.TrimSpace(r.FormValue("to"))
		return "/c/" + url.PathEscape(to) + "?ok=renamed", change(func() error { return cmdRename(w.s, name, to, io.Discard) })
	case "restart":
		return back + "?ok=restarted_node", change(func() error { return restartNode(w.s, name, io.Discard) })
	case "unlink":
		return back + "?ok=unlinked", change(func() error {
			c, err := w.s.Get(name)
			if err == nil {
				c.Telegram = nil
				err = w.s.Save(c)
			}
			return err
		})
	case "docadd":
		return back + "?ok=docadded", change(func() error {
			c, err := w.s.Get(name)
			if err != nil {
				return err
			}
			d := Doc{URL: strings.TrimSpace(r.FormValue("url"))}
			if d.URL == "" {
				d, err = w.s.freeDoc(c)
			} else {
				d.Transport = transportOf(d.URL)
			}
			if err == nil {
				_, err = w.s.AddDoc(name, d)
			}
			if err == nil {
				err = apply(w.s, io.Discard)
			}
			return err
		})
	case "docrm":
		i, err := strconv.Atoi(r.FormValue("i"))
		if err != nil {
			return "", err
		}
		return back + "?ok=docremoved", change(func() error {
			if _, err := w.s.RemoveDoc(name, i); err != nil {
				return err
			}
			return apply(w.s, io.Discard)
		})
	case "revoke":
		if r.FormValue("confirm") != name {
			return "", errors.New(tr(w.lang(), "web.revoke.confirm", name))
		}
		return "/?ok=revoked", change(func() error { return cmdRevoke(w.s, []string{name, "--yes"}, nil, io.Discard) })
	}
	return "", fmt.Errorf("unknown action %q", action)
}

// expiryFrom reads the expiry form: +N days (from the end of the access
// while it lasts, else from now), never, now, or a date (through that day).
func expiryFrom(how, date string, cur, now time.Time) (time.Time, error) {
	now = now.Truncate(time.Second)
	switch {
	case how == "never":
		return time.Time{}, nil
	case how == "now":
		return now, nil
	case how == "date":
		return parseExpiry(date, now)
	case strings.HasPrefix(how, "+"):
		days, err := strconv.Atoi(how[1:])
		if err != nil || days <= 0 || days > 3650 {
			return time.Time{}, fmt.Errorf("bad extension %q", how)
		}
		from := now
		if cur.After(now) {
			from = cur
		}
		return from.AddDate(0, 0, days), nil
	}
	return time.Time{}, fmt.Errorf("bad expiry %q", how)
}

func (w *webServer) logs(r *http.Request) (string, pageData, error) {
	name := r.PathValue("name")
	container := "reflux-egress"
	if name != "egress" {
		if err := validName(name); err != nil {
			return "error", pageData{}, err
		}
		container = "reflux-node-" + name
	}
	var out strings.Builder
	old := dockerStderr
	dockerStderr = &out
	err := runDocker(&out, "logs", "--tail", "200", container)
	dockerStderr = old
	return "logs", pageData{Title: name, Body: map[string]string{"Name": name, "Text": out.String()}}, err
}

func (w *webServer) addForm(r *http.Request) (string, pageData, error) {
	return "add", pageData{Title: tr(w.lang(), "web.add.title"), Active: "add"}, nil
}

func (w *webServer) add(r *http.Request) (string, error) {
	name := strings.TrimSpace(r.FormValue("name"))
	docURL := strings.TrimSpace(r.FormValue("url"))
	transport := transportOf(docURL)
	when, err := parseExpiry(orDefault(strings.TrimSpace(r.FormValue("expires")), "never"), time.Now())
	if err != nil {
		return "", err
	}
	unlock, err := w.s.Lock(lockWait)
	if err != nil {
		return "", err
	}
	defer unlock()
	c, err := w.s.Add(name, transport, docURL)
	if err != nil {
		return "", err
	}
	if !when.IsZero() {
		c.Expires = when
		if err := w.s.Save(c); err != nil {
			return "", err
		}
	}
	if err := apply(w.s, io.Discard); err != nil {
		return "", fmt.Errorf("%s: %w", tr(w.lang(), "ui.add.nostart", c.Name), err)
	}
	return "/c/" + url.PathEscape(c.Name) + "?ok=added", nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// transportOf guesses a document's transport from its link.
func transportOf(docURL string) string {
	switch {
	case strings.Contains(docURL, "yandex.ru/whiteboard"), strings.Contains(docURL, "boards.yandex"):
		return "boards"
	case strings.Contains(docURL, "yandex.ru"):
		return "yandex"
	}
	return "mailru"
}

// checkSection is a group of the checks page.
type checkSection struct {
	Title string
	Lines []checkLine
}

type checkLine struct {
	Level string // ok, warn, FAIL
	Text  string
}

func (w *webServer) doctor(r *http.Request) (string, pageData, error) {
	l := w.lang()
	fs := runChecks(w.s)
	warns, fails := count(fs)
	var secs []checkSection
	for _, sec := range sections {
		cs := checkSection{Title: sec.icon + " " + tr(l, "alerts."+sec.cat)}
		for _, f := range fs {
			if category(f.Key) != sec.cat {
				continue
			}
			text := f.text(l)
			if f.Level == levelOK {
				text = shortText(w.s, l, f)
			}
			cs.Lines = append(cs.Lines, checkLine{Level: f.Level.String(), Text: text})
		}
		if len(cs.Lines) > 0 {
			secs = append(secs, cs)
		}
	}
	summary := tr(l, "ui.doctor.allok")
	if warns+fails > 0 {
		summary = tr(l, "ui.doctor.sum", fails, warns)
	}
	return "doctor", pageData{Title: tr(l, "web.nav.doctor"), Active: "doctor", Refresh: 60,
		Body: map[string]any{"Summary": summary, "Sections": secs}}, nil
}

type gatewayData struct {
	Tunnels  []tunnel
	Egress   *egressStatus
	Russia   string
	Worlds   []string
	Chosen   string
	RuMode   string
	RuModes  []string
	Carrier  bool
	Carriers int
}

func (w *webServer) gateway(r *http.Request) (string, pageData, error) {
	l := w.lang()
	d := gatewayData{Worlds: w.s.worldConfigs(), Chosen: w.s.chosenWorld(), RuMode: w.s.russiaMode(),
		RuModes: russiaModes, Carrier: w.s.carrierDirect(), Tunnels: readTunnels()}
	if st, err := readEgressStatus(); err == nil {
		d.Egress = &st
		d.Russia = tr(l, ruMode(st).id)
		d.Carriers = len(st.Carriers)
	}
	return "gateway", pageData{Title: tr(l, "web.nav.gateway"), Active: "gateway", Refresh: 30, Body: d}, nil
}

func (w *webServer) gatewayAction(r *http.Request) (string, error) {
	unlock, err := w.s.Lock(lockWait)
	if err != nil {
		return "", err
	}
	defer unlock()
	switch r.PathValue("what") {
	case "world":
		return "/gateway?ok=world", w.s.chooseWorld(r.FormValue("name"))
	case "russia", "carrier":
		if r.FormValue("confirm") != "yes" {
			return "", errors.New(tr(w.lang(), "web.gw.confirm"))
		}
		var err error
		if r.PathValue("what") == "russia" {
			err = w.s.setRussiaMode(r.FormValue("mode"))
		} else {
			mode := r.FormValue("mode")
			if mode != "direct" && mode != "tunnel" {
				return "", fmt.Errorf("unknown mode %q", mode)
			}
			err = w.s.setCarrierDirect(mode == "direct")
		}
		if err != nil {
			return "", err
		}
		return "/gateway?ok=restarted", cmdRestart(w.s, io.Discard)
	}
	return "", errors.New("unknown setting")
}

func (w *webServer) speedForm(r *http.Request) (string, pageData, error) {
	return "speed", pageData{Title: tr(w.lang(), "web.nav.speed"), Active: "speed"}, nil
}

func (w *webServer) speed(r *http.Request) (string, pageData, error) {
	return "speed", pageData{Title: tr(w.lang(), "web.nav.speed"), Active: "speed", Body: speedLines(w.lang(), speedTest())}, nil
}

// ---- command ----

const webUsage = `usage:
  reflux web install --listen <LAN-IP>:8686 [--trust <IP>,...]
                     run the panel as a systemd user service (again after an update)
  reflux web login   print a one-time sign-in link (good for 10 minutes)
  reflux web run     serve in the foreground (what the service does)`

func cmdWeb(s Store, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(webUsage)
	}
	switch args[0] {
	case "run":
		cfg, err := s.loadWebConfig()
		if err != nil {
			return err
		}
		w, err := newWebServer(s, cfg)
		if err != nil {
			return err
		}
		log.Printf("web: serving on http://%s", cfg.Listen)
		srv := &http.Server{Addr: cfg.Listen, Handler: w.routes(), ReadHeaderTimeout: 10 * time.Second}
		// On SIGTERM (systemd), the sampler saves the traffic and the
		// history before the process goes.
		stop, sampled := make(chan struct{}), make(chan struct{})
		go func() { w.stats.run(stop); close(sampled) }()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			<-sig
			close(stop)
			select {
			case <-sampled:
			case <-time.After(10 * time.Second):
			}
			srv.Close()
		}()
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case "login":
		link, err := s.loginLink()
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Open within %d minutes, once:\n%s\n", int(loginFor.Minutes()), link)
		return nil
	case "install":
		fs := newFlagSet("web install")
		listen := fs.String("listen", "", "LAN address and port to serve on, e.g. 192.168.1.201:8686")
		trust := fs.String("trust", "", "comma-separated LAN addresses signed in without a link")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return webInstall(s, *listen, *trust, stdout)
	}
	return errors.New(webUsage)
}

// webInstall writes web.json and the user service, and (re)starts it.
func webInstall(s Store, listen, trust string, stdout io.Writer) error {
	cfg, _ := s.loadWebConfig()
	if listen != "" {
		cfg.Listen = listen
	}
	if trust != "" {
		cfg.Trusted = nil
		for _, a := range strings.Split(trust, ",") {
			ip := net.ParseIP(strings.TrimSpace(a))
			if !privateAddr(ip) {
				return fmt.Errorf("--trust takes LAN addresses: %q is not one", a)
			}
			cfg.Trusted = append(cfg.Trusted, ip.String())
		}
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil || !privateAddr(net.ParseIP(host)) {
		return fmt.Errorf("--listen takes the host's LAN address and a port, e.g. 192.168.1.201:8686 (got %q)", cfg.Listen)
	}
	if err := s.Init(); err != nil {
		return err
	}
	if err := s.saveWebConfig(cfg); err != nil {
		return err
	}
	if err := installUnit("reflux-web", "ReFlux web panel (home network)", "web run", s, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "The panel serves on http://%s. Let the home network in once:\n  sudo ufw allow from %s to any port %s proto tcp\n",
		cfg.Listen, lanNet(host), portOf(cfg.Listen))
	return nil
}

func portOf(addr string) string { _, p, _ := net.SplitHostPort(addr); return p }

// lanNet is the /24 of a LAN address, the usual home network.
func lanNet(host string) string {
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return host
	}
	return fmt.Sprintf("%d.%d.%d.0/24", ip[0], ip[1], ip[2])
}
