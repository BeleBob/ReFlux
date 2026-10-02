// Command reflux manages the ReFlux exit nodes on one Docker host: one
// channel (a document, a key and an exit node container) per client, all
// leaving through a shared egress container.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/p1neappleXpress/OpenFlux/share"
)

const usage = `reflux — manage ReFlux exit nodes (one per client) on this host.

USAGE
  reflux add <name> --transport <type> --url <document-url> [--expires <when>] [--note <text>]
  reflux list
  reflux show <name> [--png <file>]
  reflux pause <name>      stop the node, keep the key and the document
  reflux resume <name>
  reflux expire <name> <when>
                           when: never, a date (through that day), 30d, 2w, 12h;
                           heal stops expired nodes
  reflux rename <name> <new-name>
                           the link and the key stay; the node restarts
  reflux docs <name> [add <url|pool> | remove <n>]
                           a client's documents: backups make its node a
                           Session exit (the app imports the new link)
  reflux requests [approve|reject|block|forget <id|@user>] [--expires +30|never]
                           access requests from the client bot
  reflux invite [--expires +30|+90|never] [--note <who>] | list | revoke <code>
                           a one-time client bot link that gives access at once
  reflux pool [add <url>...]
                           prepared documents (docs-pool.txt): free and taken
  reflux revoke <name> [--yes]
  reflux apply [--dry-run] render compose.yml and start/stop containers
  reflux update            pull new images, apply, remove old ReFlux images
  reflux restart [name]    recreate egress and all nodes, or one client's node
  reflux heal              recreate nodes stranded by an egress restart
                           (quiet; for cron: * * * * * reflux heal)
  reflux status            egress tunnels, nodes, who is online
  reflux doctor            check the host, egress and every node; says what to fix
  reflux speedtest         download speed through the egress: Russia and the world
  reflux gateway [world-N.conf|auto|russia <fallback|tunnel|direct>]
                           choose the world server, or how Russia leaves
  reflux bot <setup|install|test|run>
                           Telegram bot: alerts when a doctor check changes,
                           /status, /doctor, /pause, /resume, /expire
  reflux web <install|login|run>
                           web panel for the home network (same as the bot)
  reflux logs <name|egress> [--follow]
  reflux backup [list]     back up the data directory now / list the backups
  reflux backup install [--dir <dir>] [--keep <n>]
                           a backup a day (default ~/reflux-backups, 14 kept);
                           backups stay on this server
  reflux restore <backup> [--yes]
                           put a backup back (the current state is backed up
                           first) and recreate egress and every node

ENVIRONMENT
  REFLUX_HOME          data directory (default: ~/reflux)
  REFLUX_NODE_IMAGE    exit node image (default: ghcr.io/belebob/reflux-node:main)
  REFLUX_EGRESS_IMAGE  egress image (default: ghcr.io/belebob/reflux-egress:main)

Transports: %s. The link and the key printed by add/show give access to
the channel: pass them to its client only.
`

// runDocker runs the docker CLI; tests replace it.
var runDocker = func(stdout io.Writer, args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = dockerStderr
	return cmd.Run()
}

// dockerStderr receives docker's error output.
var dockerStderr io.Writer = os.Stderr

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "reflux:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintf(stdout, usage, strings.Join(supportedTransports(), ", "))
		return nil
	}
	root, err := dataDir()
	if err != nil {
		return err
	}
	s := Store{Root: root}
	cmd, rest := args[0], args[1:]
	if changes[cmd] {
		wait := lockWait
		if cmd == "heal" {
			wait = 0 // from cron: skip this minute rather than pile up
		}
		unlock, err := s.Lock(wait)
		if errors.Is(err, errBusy) && cmd == "heal" {
			return nil
		}
		if err != nil {
			return err
		}
		defer unlock()
	}
	switch cmd {
	case "add":
		return cmdAdd(s, rest, stdout)
	case "list":
		return cmdList(s, stdout)
	case "show":
		return cmdShow(s, rest, stdout)
	case "revoke":
		return cmdRevoke(s, rest, stdin, stdout)
	case "rename":
		if len(rest) != 2 {
			return errors.New("usage: reflux rename <name> <new-name>")
		}
		return cmdRename(s, rest[0], rest[1], stdout)
	case "pause":
		return cmdPause(s, rest, stdout)
	case "resume":
		return cmdResume(s, rest, stdout)
	case "expire":
		return cmdExpire(s, rest, stdout)
	case "apply":
		fs := newFlagSet("apply")
		dryRun := fs.Bool("dry-run", false, "only write compose.yml; do not touch containers")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *dryRun {
			if err := s.Init(); err != nil {
				return err
			}
			if err := render(s); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Wrote %s; containers unchanged.\n", composePath(s))
			return nil
		}
		return apply(s, stdout)
	case "update":
		if err := s.Init(); err != nil {
			return err
		}
		if err := render(s); err != nil {
			return err
		}
		// compose pull only pulls the services in compose.yml: with no
		// clients yet the node image was never pulled, and the next add
		// started a node from a stale cached image. Pull both by name.
		o := options()
		if err := runDocker(stdout, "pull", "--quiet", o.NodeImage); err != nil {
			return err
		}
		if err := runDocker(stdout, "pull", "--quiet", o.EgressImage); err != nil {
			return err
		}
		if err := apply(s, stdout); err != nil {
			return err
		}
		// Every pull of :main leaves the previous image dangling, 40 MB a
		// time. Remove those of ReFlux only; other images stay.
		if err := runDocker(io.Discard, "image", "prune", "--force", "--filter", "label="+imageSourceLabel); err != nil {
			fmt.Fprintln(stdout, "warning: removing old ReFlux images failed:", err)
		}
		return nil
	case "status":
		states := containerStates()
		egress := states["reflux-egress"]
		if egress == "" {
			egress = "not running"
		}
		h := measureHost(measureWindow)
		fmt.Fprintf(stdout, "host: CPU %.0f%% (load %.2f, %d CPUs), memory %.0f%% (%s of %s)",
			h.Point.CPU, h.Load[0], h.CPUs, h.Point.Mem, humanBytes(h.Mem.Used()), humanBytes(h.Mem.Total))
		if len(h.Temps) > 0 {
			fmt.Fprintf(stdout, ", %.0f °C", h.Temps[0].C)
		}
		fmt.Fprintf(stdout, ", up %s\n", humanDurationEN(h.Uptime))
		fmt.Fprintf(stdout, "egress container: %s\n", egress)
		if err := runDocker(stdout, "exec", "reflux-egress", "reflux-egress", "status"); err != nil {
			fmt.Fprintln(stdout, "egress: no status (not running?)")
		}
		fmt.Fprintln(stdout)
		if err := printClients(s, stdout, states); err != nil {
			return err
		}
		if err := checkHost(); err != nil {
			fmt.Fprintln(stdout, "WARNING:", err)
		}
		return nil
	case "restart":
		if len(rest) == 1 {
			return restartNode(s, rest[0], stdout)
		}
		return cmdRestart(s, stdout)
	case "logs":
		return cmdLogs(rest, stdout)
	case "requests":
		return cmdRequests(s, rest, stdout)
	case "invite":
		return cmdInvite(s, rest, stdout)
	case "docs":
		return cmdDocs(s, rest, stdout)
	case "pool":
		return cmdPool(s, rest, stdout)
	case "heal":
		return heal(s, stdout)
	case "doctor":
		return cmdDoctor(s, stdout)
	case "speedtest":
		return cmdSpeedtest(stdout)
	case "gateway":
		return cmdGateway(s, rest, stdout)
	case "bot":
		return cmdBot(s, rest, stdin, stdout)
	case "web":
		return cmdWeb(s, rest, stdout)
	case "backup":
		return cmdBackup(s, rest, stdout)
	case "restore":
		return cmdRestore(s, rest, stdin, stdout)
	}
	return fmt.Errorf("unknown command %q (see reflux --help)", cmd)
}

// imageSourceLabel marks the images built from this repository.
const imageSourceLabel = "org.opencontainers.image.source=https://github.com/BeleBob/ReFlux"

// changes are the commands that change the data directory or the
// containers; they run one at a time (Store.Lock).
var changes = map[string]bool{
	"docs": true, "pool": true, "requests": true,
	"add": true, "pause": true, "resume": true, "expire": true, "revoke": true, "rename": true, "gateway": true,
	"apply": true, "update": true, "restart": true, "heal": true, "backup": true, "restore": true,
}

// lockWait is how long a command waits for another one to finish.
var lockWait = 2 * time.Minute

// cmdRestart recreates egress and every node. Nodes live in the egress
// container's network namespace; when egress restarts, they must be
// recreated to join the new one.
func cmdRestart(s Store, stdout io.Writer) error {
	if err := s.Init(); err != nil {
		return err
	}
	if err := render(s); err != nil {
		return err
	}
	if err := checkHost(); err != nil {
		return err
	}
	return runDocker(stdout, composeArgs(s, "up", "--detach", "--remove-orphans", "--force-recreate")...)
}

// restartNode recreates one client's node; egress and the others stay.
func restartNode(s Store, name string, stdout io.Writer) error {
	c, err := s.Get(name)
	if err != nil {
		return err
	}
	if !c.Active(time.Now()) {
		return fmt.Errorf("%s is %s: its node does not run", name, accessText(c, time.Now()))
	}
	if err := render(s); err != nil {
		return err
	}
	return runDocker(stdout, composeArgs(s, "up", "--detach", "--no-deps", "--force-recreate", "node-"+name)...)
}

func humanDurationEN(d time.Duration) string { return durationIn(langEN, d) }

func dataDir() (string, error) {
	if d := os.Getenv("REFLUX_HOME"); d != "" {
		return filepath.Abs(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "reflux"), nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func options() Options {
	return Options{
		NodeImage:   env("REFLUX_NODE_IMAGE", "ghcr.io/belebob/reflux-node:main"),
		EgressImage: env("REFLUX_EGRESS_IMAGE", "ghcr.io/belebob/reflux-egress:main"),
		UID:         os.Getuid(),
		GID:         os.Getgid(),
	}
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// parseArgs splits a command's arguments into its one positional name and
// its flags, in any order ("show phone --png x" and "show --png x phone").
func parseArgs(fs *flag.FlagSet, args []string) (string, error) {
	var names []string
	for {
		if err := fs.Parse(args); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			break
		}
		names = append(names, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(names) != 1 {
		return "", fmt.Errorf("%s: expected one client name", fs.Name())
	}
	return names[0], nil
}

func cmdAdd(s Store, args []string, stdout io.Writer) error {
	fs := newFlagSet("add")
	transport := fs.String("transport", "mailru", "carrier type")
	url := fs.String("url", "", "document URL (a new document for every client)")
	noApply := fs.Bool("no-apply", false, "do not start the node")
	expires := fs.String("expires", "never", "when access ends: never, a date, 30d, 2w, 12h")
	note := fs.String("note", "", "who or what the client is, for you")
	name, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	when, err := parseExpiry(*expires, time.Now())
	if err != nil {
		return err
	}
	c, err := s.Add(name, *transport, *url)
	if err != nil {
		return err
	}
	if !when.IsZero() || *note != "" {
		c.Expires, c.Note = when, strings.Join(strings.Fields(*note), " ")
		if err := s.Save(c); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "Added %s (%s), access %s.\n", c.Name, c.Transport, accessText(c, time.Now()))
	if !*noApply {
		if err := apply(s, stdout); err != nil {
			return fmt.Errorf("added %s, but starting it failed: %w", c.Name, err)
		}
	}
	return printAccess(s, c, stdout, "")
}

func cmdList(s Store, stdout io.Writer) error {
	return printClients(s, stdout, containerStates())
}

// printClients prints one line per channel: its node's container state
// and, from the node itself, whether the client is on the channel now and
// the traffic since the node started.
func printClients(s Store, stdout io.Writer, states map[string]string) error {
	clients, err := s.List()
	if err != nil {
		return err
	}
	if len(clients) == 0 {
		fmt.Fprintln(stdout, "No clients. Add one: reflux add <name> --transport mailru --url <document-url>")
		return nil
	}
	live := nodeStatuses(s, clients)
	seen := s.lastSeen()
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTRANSPORT\tACCESS\tNODE\tCLIENT\tDOWN\tUP\tNOTE")
	now := time.Now()
	for _, c := range clients {
		node := states["reflux-node-"+c.Name]
		if node == "" {
			node = "not running"
		}
		client, down, up := "-", "-", "-"
		if st, ok := live[c.Name]; ok {
			client = "offline"
			if t := seen[c.Name]; !t.IsZero() {
				client = "offline, " + durationIn(langEN, now.Sub(t)) + " ago"
			}
			if st.Connected {
				client = "online"
			}
			// The node's view: what it sends goes down to the client.
			down, up = humanBytes(st.BytesOut), humanBytes(st.BytesIn)
		}
		transport := c.Transport
		if n := len(c.Backups); n > 0 {
			transport += fmt.Sprintf("+%d", n) // backup documents
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, transport, accessText(c, now), node, client, down, up, c.Note)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "DOWN/UP: channel traffic to/from the client since the node started; +N: backup documents.")
	return nil
}

// containerStates maps container names to their docker status line. It
// returns an empty map when docker is unavailable: list still works.
func containerStates() map[string]string {
	var out strings.Builder
	states := map[string]string{}
	if err := runDocker(&out, "ps", "--all", "--filter", "name=^reflux-",
		"--format", "{{.Names}}\t{{.Status}}"); err != nil {
		return states
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if name, status, ok := strings.Cut(line, "\t"); ok {
			states[name] = status
		}
	}
	return states
}

func cmdShow(s Store, args []string, stdout io.Writer) error {
	fs := newFlagSet("show")
	png := fs.String("png", "", "also write the QR code to this PNG file (0600)")
	name, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := s.Get(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Access: %s\n", accessText(c, time.Now()))
	return printAccess(s, c, stdout, *png)
}

// printAccess prints what a client needs: the fields for manual entry
// (apps without link import) and the openflux:// link with its QR code.
func printAccess(s Store, c Client, stdout io.Writer, pngPath string) error {
	key, err := s.Key(c.Name)
	if err != nil {
		return err
	}
	link, err := share.MakeLink(shareConfig(c, key))
	if err != nil {
		return err
	}
	qr, err := share.Terminal(link)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nClient %s — gives access to the channel, pass it to its owner only.\n\nManual entry:\n", c.Name)
	if c.session() {
		fmt.Fprintf(stdout, "  Mode            Session (several documents, in this order)\n")
		names := carrierNames(c.Docs())
		for i, d := range c.Docs() {
			fmt.Fprintf(stdout, "  %-15s %s, priority %d: %s\n", names[i], d.Transport, docPriority(i), d.URL)
		}
		fmt.Fprintf(stdout, "  Context         %s\n", c.context())
	} else {
		fmt.Fprintf(stdout, "  Transport       %s\n  Document URL    %s\n", c.Transport, c.URL)
	}
	fmt.Fprintf(stdout, `  Encryption key  %s
  Legacy codec    off

Link (OpenFlux apps: scan or paste):
%s
%s`, key, link, qr)
	if pngPath != "" {
		img, err := share.PNG(link, 512)
		if err != nil {
			return err
		}
		if err := os.WriteFile(pngPath, img, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "QR code written to %s\n", pngPath)
	}
	return nil
}

func cmdRevoke(s Store, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := newFlagSet("revoke")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	name, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if _, err := s.Get(name); err != nil {
		return err
	}
	if !*yes {
		fmt.Fprintf(stdout, "Revoke %s? Its node stops and its key is deleted for good. Type the name to confirm: ", name)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(answer) != name {
			return errors.New("not confirmed; nothing changed")
		}
	}
	// Stop the node before its key goes: a running node keeps the key in
	// memory and would go on serving the channel.
	if err := runDocker(io.Discard, "rm", "--force", "reflux-node-"+name); err != nil {
		return fmt.Errorf("stopping reflux-node-%s failed, nothing revoked: %w", name, err)
	}
	dst, err := s.Revoke(name, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Revoked %s; record kept in %s.\n", name, dst)
	return apply(s, stdout)
}

// cmdRename renames a client. Its node stops first, so the old and the
// new one never serve the channel at once; apply starts the new one. The
// link and the key stay the same.
func cmdRename(s Store, old, name string, stdout io.Writer) error {
	if _, err := s.Get(old); err != nil {
		return err
	}
	if err := validName(name); err != nil {
		return err
	}
	if _, err := s.Get(name); err == nil {
		return fmt.Errorf("client %q already exists", name)
	}
	if err := runDocker(io.Discard, "rm", "--force", "reflux-node-"+old); err != nil {
		return fmt.Errorf("stopping reflux-node-%s failed, nothing renamed: %w", old, err)
	}
	if _, err := s.Rename(old, name); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Renamed %s to %s; the link and the key stay the same.\n", old, name)
	return apply(s, stdout)
}

func cmdLogs(args []string, stdout io.Writer) error {
	fs := newFlagSet("logs")
	follow := fs.Bool("follow", false, "keep printing new lines")
	name, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	container := "reflux-egress"
	if name != "egress" {
		if err := validName(name); err != nil {
			return err
		}
		container = "reflux-node-" + name
	}
	dockerArgs := []string{"logs", "--tail", "100"}
	if *follow {
		dockerArgs = append(dockerArgs, "--follow")
	}
	return runDocker(stdout, append(dockerArgs, container)...)
}

func composePath(s Store) string { return filepath.Join(s.Root, "compose.yml") }

func composeArgs(s Store, args ...string) []string {
	return append([]string{"compose", "--file", composePath(s), "--project-name", "reflux"}, args...)
}

// render writes compose.yml for the current clients and brings their
// node.conf files up to date.
func render(s Store) error {
	clients, err := s.List()
	if err != nil {
		return err
	}
	for _, c := range clients {
		if err := s.SyncConf(c); err != nil {
			return fmt.Errorf("%s: node.conf: %w", c.Name, err)
		}
	}
	b, err := composeYAML(s.Root, activeClients(clients, time.Now()), options())
	if err != nil {
		return err
	}
	tmp := composePath(s) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, composePath(s))
}

// apply renders compose.yml and brings the containers in line with it,
// removing the nodes of revoked clients. It refuses while the host would
// route the egress tunnels through a VPN of its own.
func apply(s Store, stdout io.Writer) error {
	if err := s.Init(); err != nil {
		return err
	}
	if err := render(s); err != nil {
		return err
	}
	if err := checkHost(); err != nil {
		return err
	}
	if err := runDocker(stdout, composeArgs(s, "up", "--detach", "--remove-orphans")...); err != nil {
		return err
	}
	return recreateStale(s, stdout)
}

// recreateStale recreates the nodes started before their node.conf last
// changed: compose sees the file only as a mounted directory and would
// leave them running on the old config.
func recreateStale(s Store, stdout io.Writer) error {
	all, err := s.List()
	if err != nil {
		return err
	}
	clients := activeClients(all, time.Now())
	if len(clients) == 0 {
		return nil
	}
	started := startTimes(clients)
	var stale []string
	for _, c := range clients {
		t, ok := started["reflux-node-"+c.Name]
		if !ok {
			continue
		}
		st, err := os.Stat(filepath.Join(s.clientDir(c.Name), "node.conf"))
		if err == nil && st.ModTime().After(t) {
			stale = append(stale, "node-"+c.Name)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	fmt.Fprintf(stdout, "Config changed; recreating %s\n", strings.Join(stale, ", "))
	return runDocker(stdout, composeArgs(s, append([]string{"up", "--detach", "--no-deps", "--force-recreate"}, stale...)...)...)
}

// startTimes maps the running reflux containers (egress and the clients'
// nodes) to when they started.
func startTimes(clients []Client) map[string]time.Time {
	names := []string{"reflux-egress"}
	for _, c := range clients {
		names = append(names, "reflux-node-"+c.Name)
	}
	var out strings.Builder
	args := append([]string{"inspect", "--format", "{{.Name}} {{.State.Running}} {{.State.StartedAt}}"}, names...)
	// inspect fails when a container is missing; what it printed still counts.
	runDocker(&out, args...)
	started := map[string]time.Time{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "true" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, f[2])
		if err == nil {
			started[strings.TrimPrefix(f[0], "/")] = t
		}
	}
	return started
}

func checkHost() error {
	rules, err := readRules()
	if err != nil {
		return fmt.Errorf("cannot read the host's routing rules (ip -4 rule show): %w", err)
	}
	if p := hostRuleProblem(rules); p != "" {
		return errors.New(p)
	}
	return nil
}

// heal stops the nodes whose access expired and recreates the nodes that
// started before the running egress: they share its network namespace,
// and when egress restarts (a crash, a Docker update) they are left in the
// old one, which has no way out. Only those nodes are recreated; egress is
// left alone. It prints nothing when all is well, so it can run from cron.
func heal(s Store, stdout io.Writer) error {
	clients, err := s.List()
	if err != nil || len(clients) == 0 {
		return err
	}
	started := startTimes(clients)
	now := time.Now()
	var ended []string
	for _, c := range clients {
		if _, running := started["reflux-node-"+c.Name]; running && !c.Active(now) {
			ended = append(ended, c.Name)
		}
	}
	if len(ended) > 0 {
		fmt.Fprintf(stdout, "%s: access ended for %s; stopping\n", now.Format(time.DateTime), strings.Join(ended, ", "))
		return apply(s, stdout)
	}
	egress, ok := started["reflux-egress"]
	if !ok {
		return nil // egress is down: nothing to rejoin yet
	}
	var stranded []string
	for _, c := range clients {
		if t, ok := started["reflux-node-"+c.Name]; ok && t.Before(egress) {
			stranded = append(stranded, "node-"+c.Name)
		}
	}
	if len(stranded) == 0 {
		return nil
	}
	fmt.Fprintf(stdout, "%s: egress restarted; recreating %s\n", time.Now().Format(time.DateTime), strings.Join(stranded, ", "))
	if err := render(s); err != nil {
		return err
	}
	return runDocker(stdout, composeArgs(s, append([]string{"up", "--detach", "--no-deps", "--force-recreate"}, stranded...)...)...)
}
