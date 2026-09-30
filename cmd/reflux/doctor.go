package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
type finding struct {
	Key   string
	Level level
	Text  string
	Sig   string
}

// doctor runs the checks of `reflux doctor` and the bot: everything a
// working channel depends on, from the host to each node, with what to do
// when one fails.
type doctor struct {
	findings []finding
}

func (d *doctor) add(lv level, key, sig, format string, a ...any) {
	if sig == "" {
		sig = lv.String()
	}
	d.findings = append(d.findings, finding{Key: key, Level: lv, Text: fmt.Sprintf(format, a...), Sig: sig})
}

func (d *doctor) ok(key, format string, a ...any)   { d.add(levelOK, key, "", format, a...) }
func (d *doctor) warn(key, format string, a ...any) { d.add(levelWarn, key, "", format, a...) }
func (d *doctor) fail(key, format string, a ...any) { d.add(levelFail, key, "", format, a...) }

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
		fmt.Fprintf(&b, "%-6s%s\n", f.Level, f.Text)
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
		d.fail("module", "amneziawg kernel module not loaded: sudo modprobe amneziawg (after a kernel update: sudo dkms autoinstall)")
	} else {
		d.ok("module", "amneziawg kernel module loaded")
	}
	if err := checkHost(); err != nil {
		d.fail("host-rule", "%v", err)
	} else {
		d.ok("host-rule", "host routing: egress tunnels bypass the host's own VPN")
	}

	dir := filepath.Join(s.Root, "egress")
	world, _ := filepath.Glob(filepath.Join(dir, "world-*.conf"))
	ru, _ := filepath.Glob(filepath.Join(dir, "ru-*.conf"))
	_, directErr := os.Stat(filepath.Join(dir, "ru-direct"))
	russia := "tunnel " + strings.Join(baseNames(ru), ", ")
	if directErr == nil {
		russia = "direct"
	}
	switch {
	case len(world) == 0:
		d.fail("egress-configs", "no world-*.conf in %s: the egress has no way out", dir)
	case directErr != nil && len(ru) == 0:
		d.fail("egress-configs", "no ru-*.conf in %s, and no ru-direct file", dir)
	default:
		d.ok("egress-configs", "egress configs: world %s; russia %s", strings.Join(baseNames(world), ", "), russia)
	}
	for _, f := range append(world, ru...) {
		if st, err := os.Stat(f); err == nil && st.Mode().Perm()&0o077 != 0 {
			d.warn("perm:"+f, "%s is readable by others (%v): chmod 600 %s", filepath.Base(f), st.Mode().Perm(), f)
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
		d.fail("docker", "docker does not answer: is it running, and is this user in the docker group?")
		return false
	}
	if err := quiet(&compose, "compose", "version", "--short"); err != nil {
		d.fail("docker", "docker compose plugin missing: sudo apt install docker-compose-plugin")
		return false
	}
	d.ok("docker", "docker %s, compose %s", strings.TrimSpace(server.String()), strings.TrimSpace(compose.String()))
	o := options()
	for _, img := range []string{o.NodeImage, o.EgressImage} {
		var b strings.Builder
		err := quiet(&b, "image", "inspect", "--format",
			`{{index .Config.Labels "org.opencontainers.image.revision"}} {{.Created}}`, img)
		f := strings.Fields(b.String())
		switch {
		case err != nil:
			d.warn("image:"+img, "image %s not pulled: reflux update", img)
		case len(f) == 2 && len(f[0]) >= 7:
			// The commit is the signature: the bot reports each update.
			d.add(levelOK, "image:"+img, "ok "+f[0], "image %s: commit %s, built %s", img, f[0][:7], dateOf(f[1]))
		default:
			d.ok("image:"+img, "image %s (local build)", img)
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

func (d *doctor) egress() {
	var b strings.Builder
	quiet(&b, "inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", "reflux-egress")
	state := strings.TrimSpace(b.String())
	if !strings.HasPrefix(state, "running") {
		if state == "" {
			state = "missing"
		}
		d.fail("egress", "egress container %s: reflux apply", state)
		return
	}
	var st strings.Builder
	if err := quiet(&st, "exec", "reflux-egress", "reflux-egress", "status"); err != nil {
		d.fail("egress", "egress container %s, but has no status yet (starting?): reflux logs egress", state)
		return
	}
	lines := map[string]string{}
	for _, l := range strings.Split(st.String(), "\n") {
		if k, _, ok := strings.Cut(l, " "); ok {
			lines[k] = strings.Join(strings.Fields(l), " ")
		}
	}
	d.ok("egress", "egress container %s", state)
	for _, k := range []string{"world", "russia"} {
		// "world up via world-3.conf (since ...)": the server in use is
		// part of the signature, so the bot reports a failover.
		f := strings.Fields(lines[k])
		sig := ""
		if len(f) > 3 && f[2] == "via" {
			sig = f[1] + " " + f[3]
		}
		if strings.HasPrefix(lines[k], k+" up ") {
			d.add(levelOK, k, sig, "%s", lines[k])
		} else {
			d.add(levelFail, k, sig, "%s: reflux logs egress", lines[k])
		}
	}
	if e := lines["error:"]; e != "" {
		d.warn("egress-error", "egress reports %s", e)
	}
	if k := lines["kill"]; k != "" {
		d.ok("kill-switch", "%s", k)
	}
}

// docTroubleRe matches the node log lines of a carrier that cannot keep
// its document: logged at most once a minute each, while it retries.
var docTroubleRe = regexp.MustCompile(`connection to the document dropped|cannot open the document|failed to start|still not up`)

func (d *doctor) nodes(s Store) {
	clients, err := s.List()
	if err != nil {
		d.fail("clients", "clients: %v", err)
		return
	}
	if len(clients) == 0 {
		d.warn("clients", "no clients yet: reflux add <name> --url <document-url>")
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
				d.warn("node:"+c.Name, "node %s runs although access is %s: reflux heal", c.Name, accessText(c, now))
			} else {
				d.add(levelOK, "node:"+c.Name, accessText(c, now), "node %s: %s, not running", c.Name, accessText(c, now))
			}
			continue
		}
		switch {
		case !running:
			d.fail("node:"+c.Name, "node %s not running: reflux apply", c.Name)
			continue
		case egressUp && t.Before(egress):
			d.fail("node:"+c.Name, "node %s started before egress and has no network: reflux heal", c.Name)
			continue
		}
		if n := d.docTrouble(c.Name); n >= 3 {
			d.warn("doc:"+c.Name, "node %s lost its document %d times in 5 minutes: reflux logs %s", c.Name, n, c.Name)
		}
		st, ok := live[c.Name]
		if !ok {
			d.warn("node:"+c.Name, "node %s gives no status (starting, or set up by an older reflux: reflux update)", c.Name)
			continue
		}
		d.ok("node:"+c.Name, "node %s: up %s, client %s, %s down / %s up", c.Name,
			humanDuration(time.Duration(st.UptimeMs)*time.Millisecond), onlineText(st),
			humanBytes(st.BytesOut), humanBytes(st.BytesIn))
	}
}

func onlineText(st ipc.StatusPayload) string {
	if st.Connected {
		return "online"
	}
	return "offline"
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
		d.ok("cron", "cron runs reflux heal")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "reflux"
	}
	d.warn("cron", "cron does not run reflux heal: add with crontab -e:\n      * * * * * %s heal >> %s 2>&1", exe, "$HOME/reflux/heal.log")
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
		d.warn("disk", "disk: df failed: %v", err)
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
			d.fail("disk:"+m[2], "disk %s %d%% full: free space (docker image prune)", m[2], used)
		case used >= 90:
			d.warn("disk:"+m[2], "disk %s %d%% full", m[2], used)
		default:
			d.ok("disk:"+m[2], "disk %s %d%% used", m[2], used)
		}
	}
}
