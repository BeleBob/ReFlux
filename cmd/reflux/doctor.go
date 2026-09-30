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

// doctor runs the checks of `reflux doctor`: everything a working channel
// depends on, from the host to each node, with what to do when one fails.
type doctor struct {
	w            io.Writer
	warns, fails int
}

func (d *doctor) ok(format string, a ...any) { fmt.Fprintf(d.w, "ok    "+format+"\n", a...) }

func (d *doctor) warn(format string, a ...any) {
	d.warns++
	fmt.Fprintf(d.w, "warn  "+format+"\n", a...)
}

func (d *doctor) fail(format string, a ...any) {
	d.fails++
	fmt.Fprintf(d.w, "FAIL  "+format+"\n", a...)
}

// quiet runs docker with its error output discarded: a failed check says
// what went wrong in its own words.
func quiet(stdout io.Writer, args ...string) error {
	old := dockerStderr
	dockerStderr = io.Discard
	defer func() { dockerStderr = old }()
	return runDocker(stdout, args...)
}

func cmdDoctor(s Store, stdout io.Writer) error {
	d := &doctor{w: stdout}
	d.host(s)
	if d.docker() {
		d.egress()
		d.nodes(s)
	}
	d.cron()
	d.disk(s)
	fmt.Fprintf(stdout, "\n%d problem(s), %d warning(s)\n", d.fails, d.warns)
	if d.fails > 0 {
		return fmt.Errorf("%d problem(s) found", d.fails)
	}
	return nil
}

func (d *doctor) host(s Store) {
	if _, err := os.Stat(amneziawgModule); err != nil {
		d.fail("amneziawg kernel module not loaded: sudo modprobe amneziawg (after a kernel update: sudo dkms autoinstall)")
	} else {
		d.ok("amneziawg kernel module loaded")
	}
	if err := checkHost(); err != nil {
		d.fail("%v", err)
	} else {
		d.ok("host routing: egress tunnels bypass the host's own VPN")
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
		d.fail("no world-*.conf in %s: the egress has no way out", dir)
	case directErr != nil && len(ru) == 0:
		d.fail("no ru-*.conf in %s, and no ru-direct file", dir)
	default:
		d.ok("egress configs: world %s; russia %s", strings.Join(baseNames(world), ", "), russia)
	}
	for _, f := range append(world, ru...) {
		if st, err := os.Stat(f); err == nil && st.Mode().Perm()&0o077 != 0 {
			d.warn("%s is readable by others (%v): chmod 600 %s", filepath.Base(f), st.Mode().Perm(), f)
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
		d.fail("docker does not answer: is it running, and is this user in the docker group?")
		return false
	}
	if err := quiet(&compose, "compose", "version", "--short"); err != nil {
		d.fail("docker compose plugin missing: sudo apt install docker-compose-plugin")
		return false
	}
	d.ok("docker %s, compose %s", strings.TrimSpace(server.String()), strings.TrimSpace(compose.String()))
	o := options()
	for _, img := range []string{o.NodeImage, o.EgressImage} {
		var b strings.Builder
		err := quiet(&b, "image", "inspect", "--format",
			`{{index .Config.Labels "org.opencontainers.image.revision"}} {{.Created}}`, img)
		f := strings.Fields(b.String())
		switch {
		case err != nil:
			d.warn("image %s not pulled: reflux update", img)
		case len(f) == 2 && len(f[0]) >= 7:
			d.ok("image %s: commit %s, built %s", img, f[0][:7], dateOf(f[1]))
		default:
			d.ok("image %s (local build)", img)
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
		d.fail("egress container %s: reflux apply", state)
		return
	}
	var st strings.Builder
	if err := quiet(&st, "exec", "reflux-egress", "reflux-egress", "status"); err != nil {
		d.fail("egress container %s, but has no status yet (starting?): reflux logs egress", state)
		return
	}
	lines := map[string]string{}
	for _, l := range strings.Split(st.String(), "\n") {
		if k, _, ok := strings.Cut(l, " "); ok {
			lines[k] = strings.Join(strings.Fields(l), " ")
		}
	}
	for _, k := range []string{"world", "russia"} {
		if strings.HasPrefix(lines[k], k+" up ") {
			d.ok("%s", lines[k])
		} else {
			d.fail("%s: reflux logs egress", lines[k])
		}
	}
	if e := lines["error:"]; e != "" {
		d.warn("egress reports %s", e)
	}
	if k := lines["kill"]; k != "" {
		d.ok("%s", k)
	}
}

// docTroubleRe matches the node log lines of a carrier that cannot keep
// its document: logged at most once a minute each, while it retries.
var docTroubleRe = regexp.MustCompile(`connection to the document dropped|cannot open the document|failed to start|still not up`)

func (d *doctor) nodes(s Store) {
	clients, err := s.List()
	if err != nil {
		d.fail("clients: %v", err)
		return
	}
	if len(clients) == 0 {
		d.warn("no clients yet: reflux add <name> --url <document-url>")
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
				d.warn("node %s runs although access is %s: reflux heal", c.Name, accessText(c, now))
			} else {
				d.ok("node %s: %s, not running", c.Name, accessText(c, now))
			}
			continue
		}
		switch {
		case !running:
			d.fail("node %s not running: reflux apply", c.Name)
			continue
		case egressUp && t.Before(egress):
			d.fail("node %s started before egress and has no network: reflux heal", c.Name)
			continue
		}
		if n := d.docTrouble(c.Name); n >= 3 {
			d.warn("node %s lost its document %d times in 5 minutes: reflux logs %s", c.Name, n, c.Name)
		}
		st, ok := live[c.Name]
		if !ok {
			d.warn("node %s gives no status (starting, or set up by an older reflux: reflux update)", c.Name)
			continue
		}
		d.ok("node %s: up %s, client %s, %s down / %s up", c.Name,
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

func (d *doctor) cron() {
	var b strings.Builder
	runCmd(&b, "crontab", "-l")
	if strings.Contains(b.String(), "reflux heal") {
		d.ok("cron runs reflux heal")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "reflux"
	}
	d.warn("cron does not run reflux heal: add with crontab -e:\n      * * * * * %s heal >> %s 2>&1", exe, "$HOME/reflux/heal.log")
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
		d.warn("disk: df failed: %v", err)
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
			d.fail("disk %s %d%% full: free space (docker image prune)", m[2], used)
		case used >= 90:
			d.warn("disk %s %d%% full", m[2], used)
		default:
			d.ok("disk %s %d%% used", m[2], used)
		}
	}
}
