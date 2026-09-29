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

	"openflux/share"
)

const usage = `reflux — manage ReFlux exit nodes (one per client) on this host.

USAGE
  reflux add <name> --transport <type> --url <document-url>
  reflux list
  reflux show <name> [--png <file>]
  reflux revoke <name> [--yes]
  reflux apply [--dry-run] render compose.yml and start/stop containers
  reflux update            pull new images, then apply
  reflux restart           recreate egress and all nodes
  reflux heal              recreate nodes stranded by an egress restart
                           (quiet; for cron: * * * * * reflux heal)
  reflux status            containers and tunnels
  reflux logs <name|egress> [--follow]

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
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

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
	switch cmd {
	case "add":
		return cmdAdd(s, rest, stdout)
	case "list":
		return cmdList(s, stdout)
	case "show":
		return cmdShow(s, rest, stdout)
	case "revoke":
		return cmdRevoke(s, rest, stdin, stdout)
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
		if err := runDocker(stdout, composeArgs(s, "pull")...); err != nil {
			return err
		}
		return apply(s, stdout)
	case "status":
		if err := runDocker(stdout, composeArgs(s, "ps", "--all")...); err != nil {
			return err
		}
		fmt.Fprintln(stdout)
		if err := runDocker(stdout, "exec", "reflux-egress", "reflux-egress", "status"); err != nil {
			fmt.Fprintln(stdout, "egress: no status (not running?)")
		}
		if err := checkHost(); err != nil {
			fmt.Fprintln(stdout, "WARNING:", err)
		}
		return nil
	case "restart":
		// Nodes live in the egress container's network namespace; when
		// egress restarts, they must be recreated to join the new one.
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
	case "logs":
		return cmdLogs(rest, stdout)
	case "heal":
		return heal(s, stdout)
	}
	return fmt.Errorf("unknown command %q (see reflux --help)", cmd)
}

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
	name, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := s.Add(name, *transport, *url)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Added %s (%s).\n", c.Name, c.Transport)
	if !*noApply {
		if err := apply(s, stdout); err != nil {
			return fmt.Errorf("added %s, but starting it failed: %w", c.Name, err)
		}
	}
	return printAccess(s, c, stdout, "")
}

func cmdList(s Store, stdout io.Writer) error {
	clients, err := s.List()
	if err != nil {
		return err
	}
	if len(clients) == 0 {
		fmt.Fprintln(stdout, "No clients. Add one: reflux add <name> --transport mailru --url <document-url>")
		return nil
	}
	states := containerStates()
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTRANSPORT\tCREATED\tNODE")
	for _, c := range clients {
		st := states["reflux-node-"+c.Name]
		if st == "" {
			st = "not running"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Name, c.Transport, c.Created.Format("2006-01-02"), st)
	}
	return w.Flush()
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
	fmt.Fprintf(stdout, `
Client %s — gives access to the channel, pass it to its owner only.

Manual entry:
  Transport       %s
  Document URL    %s
  Encryption key  %s
  Legacy codec    off

Link (OpenFlux apps: scan or paste):
%s
%s`, c.Name, c.Transport, c.URL, key, link, qr)
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

// render writes compose.yml for the current clients.
func render(s Store) error {
	clients, err := s.List()
	if err != nil {
		return err
	}
	b, err := composeYAML(s.Root, clients, options())
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
	return runDocker(stdout, composeArgs(s, "up", "--detach", "--remove-orphans")...)
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

// heal recreates the nodes that started before the running egress: they
// share its network namespace, and when egress restarts (a crash, a Docker
// update) they are left in the old one, which has no way out. Only those
// nodes are recreated; egress is left alone. It prints nothing when all is
// well, so it can run from cron.
func heal(s Store, stdout io.Writer) error {
	clients, err := s.List()
	if err != nil || len(clients) == 0 {
		return err
	}
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
