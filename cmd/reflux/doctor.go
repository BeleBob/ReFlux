package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"openflux/transport/ipc"
)

// runCmd runs a host command other than docker; tests replace it.
var runCmd = func(stdout io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = stdout
	return cmd.Run()
}

// amneziawgModule is where the kernel lists the loaded module.
var amneziawgModule = "/sys/module/amneziawg"

type level int

const (
	levelOK level = iota
	levelWarn
	levelFail
)

func (l level) String() string { return [...]string{"ok", "warn", "FAIL"}[l] }

// finding is the result of one check. Key names the check from one run to
// the next; Sig is what the bot compares between runs, so a change in it
// is news (by default the level: a node going offline and back is not).
// Msg and Args are its text, rendered in the reader's language.
type finding struct {
	Key   string
	Level level
	Msg   string
	Args  []any
	Sig   string
}

// text renders the finding in l.
func (f finding) text(l lang) string { return tr(l, f.Msg, f.Args...) }

// doctor runs the checks of `reflux doctor` and the bot: everything a
// working channel depends on, from the host to each node, with what to do
// when one fails.
type doctor struct {
	findings []finding
}

func (d *doctor) add(lv level, key, sig, msg string, a ...any) {
	if sig == "" {
		sig = lv.String()
	}
	d.findings = append(d.findings, finding{Key: key, Level: lv, Msg: msg, Args: a, Sig: sig})
}

func (d *doctor) ok(key, msg string, a ...any)   { d.add(levelOK, key, "", msg, a...) }
func (d *doctor) warn(key, msg string, a ...any) { d.add(levelWarn, key, "", msg, a...) }
func (d *doctor) fail(key, msg string, a ...any) { d.add(levelFail, key, "", msg, a...) }

// quiet runs docker with its error output discarded: a failed check says
// what went wrong in its own words.
func quiet(stdout io.Writer, args ...string) error {
	old := dockerStderr
	dockerStderr = io.Discard
	defer func() { dockerStderr = old }()
	return runDocker(stdout, args...)
}

// runChecks runs every check once.
func runChecks(s Store) []finding {
	d := &doctor{}
	d.host(s)
	if d.docker() {
		d.egress()
		d.nodes(s)
	}
	d.cron()
	d.disk(s)
	return d.findings
}

func cmdDoctor(s Store, stdout io.Writer) error {
	fs := runChecks(s)
	io.WriteString(stdout, formatFindings(fs))
	warns, fails := count(fs)
	fmt.Fprintf(stdout, "\n%d problem(s), %d warning(s)\n", fails, warns)
	if fails > 0 {
		return fmt.Errorf("%d problem(s) found", fails)
	}
	return nil
}

func formatFindings(fs []finding) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "%-6s%s\n", f.Level, f.text(langEN))
	}
	return b.String()
}

func count(fs []finding) (warns, fails int) {
	for _, f := range fs {
		switch f.Level {
		case levelWarn:
			warns++
		case levelFail:
			fails++
		}
	}
	return warns, fails
}

func (d *doctor) host(s Store) {
	if _, err := os.Stat(amneziawgModule); err != nil {
		d.fail("module", "module.fail")
	} else {
		d.ok("module", "module.ok")
	}
	if err := checkHost(); err != nil {
		d.fail("host-rule", "hostrule.fail", err.Error())
	} else {
		d.ok("host-rule", "hostrule.ok")
	}

	dir := filepath.Join(s.Root, "egress")
	world, _ := filepath.Glob(filepath.Join(dir, "world-*.conf"))
	ru, _ := filepath.Glob(filepath.Join(dir, "ru-*.conf"))
	_, directErr := os.Stat(filepath.Join(dir, ruDirectFile))
	_, fallbackErr := os.Stat(filepath.Join(dir, ruFallbackFile))
	russia := ph("ru.tunnelconf", strings.Join(baseNames(ru), ", "))
	switch {
	case directErr == nil:
		russia = ph("ru.direct")
	case fallbackErr == nil:
		russia = ph("ru.tunnelfallback", strings.Join(baseNames(ru), ", "))
	}
	switch {
	case len(world) == 0:
		d.fail("egress-configs", "configs.noworld", dir)
	case directErr != nil && len(ru) == 0:
		d.fail("egress-configs", "configs.noru", dir)
	default:
		d.ok("egress-configs", "configs.ok", strings.Join(baseNames(world), ", "), russia)
	}
	for _, f := range append(world, ru...) {
		if st, err := os.Stat(f); err == nil && st.Mode().Perm()&0o077 != 0 {
			d.warn("perm:"+f, "perm.warn", filepath.Base(f), st.Mode().Perm(), f)
		}
	}
}

func baseNames(paths []string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}

// docker checks the daemon and the images; the checks after it need it.
func (d *doctor) docker() bool {
	var server, compose strings.Builder
	if err := quiet(&server, "version", "--format", "{{.Server.Version}}"); err != nil {
		d.fail("docker", "docker.down")
		return false
	}
	if err := quiet(&compose, "compose", "version", "--short"); err != nil {
		d.fail("docker", "compose.missing")
		return false
	}
	d.ok("docker", "docker.ok", strings.TrimSpace(server.String()), strings.TrimSpace(compose.String()))
	o := options()
	for _, img := range []string{o.NodeImage, o.EgressImage} {
		var b strings.Builder
		err := quiet(&b, "image", "inspect", "--format",
			`{{index .Config.Labels "org.opencontainers.image.revision"}} {{.Created}}`, img)
		f := strings.Fields(b.String())
		switch {
		case err != nil:
			d.warn("image:"+img, "image.missing", img)
		case len(f) == 2 && len(f[0]) >= 7:
			// The commit is the signature: the bot reports each update.
			d.add(levelOK, "image:"+img, "ok "+f[0], "image.ok", img, f[0][:7], dateOf(f[1]))
		default:
			d.ok("image:"+img, "image.local", img)
		}
	}
	return true
}

func dateOf(rfc3339 string) string {
	if t, err := time.Parse(time.RFC3339Nano, rfc3339); err == nil {
		return t.Local().Format(time.DateOnly)
	}
	return rfc3339
}

// egressStatus is the egress controller's status.json (see reflux-egress).
type egressStatus struct {
	Updated       time.Time `json:"updated"`
	World         string    `json:"world"`
	WorldOK       bool      `json:"world_ok"`
	WorldSince    time.Time `json:"world_since"`
	Selected      string    `json:"world_selected"`
	RUOK          bool      `json:"ru_ok"`
	RUMode        string    `json:"ru_mode"` // "tunnel" or "direct": the way in use
	RUFallback    bool      `json:"ru_fallback"`
	CarrierDirect bool      `json:"carrier_direct"`
	Carriers      []string  `json:"carrier_addrs"`
	RUPrefixes    int       `json:"ru_prefixes"`
	RUListAt      time.Time `json:"ru_list_updated"`
	Dropped       int64     `json:"killswitch_dropped"`
	Error         string    `json:"error"`
}

// readEgressStatus asks the running egress for its status.
func readEgressStatus() (egressStatus, error) {
	var st egressStatus
	var b strings.Builder
	if err := quiet(&b, "exec", "reflux-egress", "cat", "/run/reflux-egress/status.json"); err != nil {
		return st, err
	}
	err := json.Unmarshal([]byte(b.String()), &st)
	return st, err
}

func ruMode(st egressStatus) phrase {
	if st.RUFallback {
		return ph("ru.fallback")
	}
	if st.RUMode == "direct" {
		return ph("ru.direct")
	}
	return ph("ru.tunnel")
}

func (d *doctor) egress() {
	var b strings.Builder
	quiet(&b, "inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", "reflux-egress")
	state := strings.TrimSpace(b.String())
	if !strings.HasPrefix(state, "running") {
		if state == "" {
			state = "missing"
		}
		d.fail("egress", "egress.down", state)
		return
	}
	st, err := readEgressStatus()
	if err != nil {
		d.fail("egress", "egress.nostatus", state)
		return
	}
	d.ok("egress", "egress.ok", state)
	// The server in use is part of the signature: the bot reports a
	// failover.
	if st.WorldOK {
		d.add(levelOK, "world", "up "+st.World, "world.up", st.World, st.WorldSince.Local().Format(time.DateTime))
	} else {
		d.add(levelFail, "world", "down "+st.World, "world.down", st.World)
	}
	switch {
	case st.RUOK && st.RUFallback:
		d.add(levelWarn, "russia", "fallback", "russia.fallback")
	case st.RUOK:
		d.add(levelOK, "russia", "up "+st.RUMode, "russia.up", ruMode(st), st.RUPrefixes, st.RUListAt.Local().Format(time.DateOnly))
	default:
		d.add(levelFail, "russia", "down "+st.RUMode, "russia.down", ruMode(st))
	}
	if st.CarrierDirect {
		d.ok("carrier", "carrier.direct", len(st.Carriers))
	}
	if st.Error != "" {
		d.warn("egress-error", "egress.error", st.Error)
	}
	d.ok("kill-switch", "killswitch", st.Dropped)
}

// docTroubleRe matches the node log lines of a carrier that cannot keep
// its document: logged at most once a minute each, while it retries.
var docTroubleRe = regexp.MustCompile(`connection to the document dropped|cannot open the document|failed to start|still not up`)

// expiryWarn is how long before a client's access ends the owner hears
// of it.
const expiryWarn = 3 * 24 * time.Hour

func (d *doctor) nodes(s Store) {
	clients, err := s.List()
	if err != nil {
		d.fail("clients", "clients.err", err)
		return
	}
	if len(clients) == 0 {
		d.warn("clients", "clients.none")
		return
	}
	now := time.Now()
	started := startTimes(clients)
	live := nodeStatuses(s, activeClients(clients, now))
	egress, egressUp := started["reflux-egress"]
	for _, c := range clients {
		t, running := started["reflux-node-"+c.Name]
		if !c.Active(now) {
			if running {
				d.warn("node:"+c.Name, "node.inactive.running", c.Name, accessPhrase(c, now))
			} else {
				d.add(levelOK, "node:"+c.Name, accessText(c, now), "node.inactive", c.Name, accessPhrase(c, now))
			}
			continue
		}
		if left := c.Expires.Sub(now); !c.Expires.IsZero() && left < expiryWarn {
			d.warn("expiry:"+c.Name, "node.expiring", c.Name, c.Expires.Local().Format("02.01 15:04"), durationPhrase(left), c.Name)
		}
		switch {
		case !running:
			d.fail("node:"+c.Name, "node.down", c.Name)
			continue
		case egressUp && t.Before(egress):
			d.fail("node:"+c.Name, "node.stranded", c.Name)
			continue
		}
		if n := d.docTrouble(c.Name); n >= 3 {
			d.warn("doc:"+c.Name, "node.doc", c.Name, n, c.Name)
		}
		st, ok := live[c.Name]
		if !ok {
			d.warn("node:"+c.Name, "node.nostatus", c.Name)
			continue
		}
		d.ok("node:"+c.Name, "node.ok", c.Name,
			durationPhrase(time.Duration(st.UptimeMs)*time.Millisecond), onlinePhrase(st),
			humanBytes(st.BytesOut), humanBytes(st.BytesIn))
		d.docInUse(c, st)
	}
}

// docSeen remembers which document each Session client was last seen on:
// while the client is away (a phone asleep), the check keeps saying it,
// so an alert is not resolved and raised again with every nap.
var docSeen = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

// docInUse checks which document a Session client's traffic goes over: a
// backup means the main document does not reach the client.
func (d *doctor) docInUse(c Client, st ipc.StatusPayload) {
	if !c.session() {
		return
	}
	docSeen.Lock()
	defer docSeen.Unlock()
	i, known := docSeen.m[c.Name]
	if at := c.docIndex(st.Active); st.Connected && at >= 0 {
		i, known = at, true
		docSeen.m[c.Name] = at
	}
	switch {
	case !known:
	case i > 0:
		d.warn("docs:"+c.Name, "node.onbackup", c.Name, i+1, c.Name)
	default:
		d.ok("docs:"+c.Name, "node.onmain", c.Name, len(c.Docs()))
	}
}

func onlinePhrase(st ipc.StatusPayload) phrase {
	if st.Connected {
		return ph("online")
	}
	return ph("offline")
}

// docTrouble counts a node's recent complaints about its document.
func (d *doctor) docTrouble(name string) int {
	var b strings.Builder
	old := dockerStderr
	dockerStderr = &b // the core logs to stderr
	runDocker(&b, "logs", "--since", "5m", "reflux-node-"+name)
	dockerStderr = old
	n := 0
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		if docTroubleRe.MatchString(sc.Text()) {
			n++
		}
	}
	return n
}

// cronEvery, when set, reuses the cron check for that long: the bot runs
// the checks every minute, and every crontab -l writes a line to the
// system log. The entry hardly ever changes.
var (
	cronEvery time.Duration
	cronLast  struct {
		at time.Time
		f  finding
	}
)

func (d *doctor) cron() {
	if cronEvery > 0 && time.Since(cronLast.at) < cronEvery {
		d.findings = append(d.findings, cronLast.f)
		return
	}
	d.checkCron()
	if cronEvery > 0 {
		cronLast.at, cronLast.f = time.Now(), d.findings[len(d.findings)-1]
	}
}

func (d *doctor) checkCron() {
	var b strings.Builder
	runCmd(&b, "crontab", "-l")
	if strings.Contains(b.String(), "reflux heal") {
		d.ok("cron", "cron.ok")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "reflux"
	}
	d.warn("cron", "cron.missing", exe, "$HOME/reflux/heal.log")
}

var dfLineRe = regexp.MustCompile(`\s([0-9]+)%\s+(\S+)$`)

// disk checks the filesystems of the data directory and of Docker.
func (d *doctor) disk(s Store) {
	paths := []string{s.Root}
	var root strings.Builder
	if quiet(&root, "info", "--format", "{{.DockerRootDir}}") == nil {
		if p := strings.TrimSpace(root.String()); p != "" {
			paths = append(paths, p)
		}
	}
	var b strings.Builder
	if err := runCmd(&b, "df", append([]string{"-P"}, paths...)...); err != nil && b.Len() == 0 {
		d.warn("disk", "disk.dffail", err)
		return
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(b.String(), "\n") {
		m := dfLineRe.FindStringSubmatch(strings.TrimSpace(" " + l))
		if m == nil || seen[m[2]] {
			continue
		}
		seen[m[2]] = true
		used, _ := strconv.Atoi(m[1])
		switch {
		case used >= 97:
			d.fail("disk:"+m[2], "disk.full", m[2], used)
		case used >= 90:
			d.warn("disk:"+m[2], "disk.warn", m[2], used)
		default:
			d.ok("disk:"+m[2], "disk.ok", m[2], used)
		}
	}
}
