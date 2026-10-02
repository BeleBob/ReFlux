package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// reflux setup: a server's install as a wizard, safe to run again. Each
// step looks first and changes only what is missing; it asks before
// anything that needs root or reaches outside the data directory, and
// --check only says what it would do. The steps: Docker, the AmneziaWG
// module, user services without a login, the route around a host VPN,
// the tunnels' configs, heal from cron, the egress started, then what the
// owner wants of the bots, the panel, backups and a first client.

type setupUI struct {
	in      *bufio.Reader
	out     io.Writer
	check   bool // report only
	missing int  // steps that found something missing
}

// say prints a line; one starting with ✗ is a step with work to do.
func (u *setupUI) say(format string, a ...any) {
	if strings.HasPrefix(format, "✗") {
		u.missing++
	}
	fmt.Fprintf(u.out, format+"\n", a...)
}

func (u *setupUI) line(q string) string {
	fmt.Fprint(u.out, q+" ")
	s, _ := u.in.ReadString('\n')
	return strings.TrimSpace(s)
}

// ask is a yes/no question; an empty answer takes def.
func (u *setupUI) ask(q string, def bool) bool {
	hint := " [y/N]"
	if def {
		hint = " [Y/n]"
	}
	switch strings.ToLower(u.line(q + hint)) {
	case "y", "yes", "д", "да":
		return true
	case "n", "no", "н", "нет":
		return false
	}
	return def
}

// errStop: the wizard cannot go on until the owner does something it
// said (log in again, install a module); setup can then run again.
var errStop = errors.New("stopped")

// setupStep is one step: it reports, and fixes what it may.
type setupStep struct {
	name string
	run  func(u *setupUI, s Store) error
}

var setupSteps = []setupStep{
	{"user", setupUser},
	{"docker", setupDocker},
	{"module", setupModule},
	{"linger", setupLinger},
	{"route", setupRoute},
	{"configs", setupConfigs},
	{"cron", setupCron},
	{"start", setupStart},
	{"extras", setupExtras},
}

func cmdSetup(s Store, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := newFlagSet("setup")
	check := fs.Bool("check", false, "only say what is missing; change nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u := &setupUI{in: bufio.NewReader(stdin), out: stdout, check: *check}
	u.say("ReFlux setup %s: every step checks first and changes only what is missing.", version())
	for _, st := range setupSteps {
		if *check && st.name == "extras" {
			break
		}
		if err := st.run(u, s); err != nil {
			if errors.Is(err, errStop) {
				u.say("\nRun  reflux setup  again once that is done: it goes on from there.")
				return nil
			}
			return fmt.Errorf("%s: %w", st.name, err)
		}
	}
	if *check {
		if u.missing == 0 {
			u.say("\nNothing is missing.")
		} else {
			u.say("\n%d step(s) to do: reflux setup does them, asking first.", u.missing)
		}
		return nil
	}
	u.say("\nChecking the whole server (reflux doctor):")
	fs2 := runChecks(s)
	io.WriteString(stdout, formatFindings(fs2))
	warns, fails := count(fs2)
	u.say("\n%d problem(s), %d warning(s). Setup is done; it can run again any time.", fails, warns)
	return nil
}

func setupUser(u *setupUI, s Store) error {
	if os.Geteuid() == 0 {
		return errors.New("run reflux setup as the user who will manage ReFlux, not as root: it asks for sudo where it needs it")
	}
	return nil
}

// ---- Docker ----

func setupDocker(u *setupUI, s Store) error {
	var ver, compose strings.Builder
	if quiet(&ver, "version", "--format", "{{.Server.Version}}") == nil {
		if quiet(&compose, "compose", "version", "--short") == nil {
			u.say("✓ Docker %s, compose %s", strings.TrimSpace(ver.String()), strings.TrimSpace(compose.String()))
			return nil
		}
		u.say("✗ Docker without its compose plugin.")
		if u.check {
			return nil
		}
		if u.ask("Install the compose plugin (sudo apt-get install docker-compose-plugin)?", true) {
			if err := runCmd(u.out, "sudo", "apt-get", "install", "-y", "docker-compose-plugin"); err != nil {
				return fmt.Errorf("installing the compose plugin: %w", err)
			}
			return setupDocker(u, s)
		}
		return errStop
	}
	if _, err := lookPath("docker"); err == nil {
		return setupDockerAccess(u, s)
	}
	u.say("✗ Docker is not installed.")
	if u.check {
		return nil
	}
	if !u.ask("Install Docker with its official script (curl https://get.docker.com | sudo sh)?", true) {
		u.say("Install Docker with the compose plugin, then run reflux setup again.")
		return errStop
	}
	if err := runCmd(u.out, "sh", "-c", "curl -fsSL https://get.docker.com | sudo sh"); err != nil {
		return fmt.Errorf("installing Docker: %w", err)
	}
	name := currentUser()
	if err := runCmd(u.out, "sudo", "usermod", "-aG", "docker", name); err != nil {
		return err
	}
	u.say("Docker is installed and %s is in the docker group: log out and in again (or reboot).", name)
	return errStop
}

// setupDockerAccess: Docker is installed, but this user cannot talk to it.
func setupDockerAccess(u *setupUI, s Store) error {
	name := currentUser()
	switch {
	case inGroup(""):
		// In the group already: the daemon is not running.
		u.say("✗ Docker is installed, but its daemon does not answer.")
		if u.check {
			return nil
		}
		if !u.ask("Start it now and at every boot (sudo systemctl enable --now docker)?", true) {
			return errStop
		}
		if err := runCmd(u.out, "sudo", "systemctl", "enable", "--now", "docker"); err != nil {
			return err
		}
		if quiet(io.Discard, "version") != nil {
			return errors.New("the Docker daemon still does not answer: sudo journalctl -u docker")
		}
		return setupDocker(u, s)
	case inGroup(name):
		u.say("✗ %s was added to the docker group after this login.", name)
		if !u.check {
			u.say("Log out and in again (or reboot) for it to take effect.")
			return errStop
		}
		return nil
	}
	u.say("✗ Docker is installed, but %s may not use it.", name)
	if u.check {
		return nil
	}
	if u.ask(fmt.Sprintf("Add %s to the docker group (sudo usermod -aG docker %s)?", name, name), true) {
		if err := runCmd(u.out, "sudo", "usermod", "-aG", "docker", name); err != nil {
			return err
		}
		u.say("Done. Log out and in again (or reboot) for it to take effect.")
	}
	return errStop
}

// inGroup: whether this process (name "") or the account name, as it
// will be at its next login, is in the docker group.
func inGroup(name string) bool {
	var b strings.Builder
	args := []string{"-nG"}
	if name != "" {
		args = append(args, name)
	}
	if runCmd(&b, "id", args...) != nil {
		return false
	}
	for _, g := range strings.Fields(b.String()) {
		if g == "docker" {
			return true
		}
	}
	return false
}

// lookPath and currentUser are replaced in tests.
var lookPath = exec.LookPath

var currentUser = func() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// ---- the AmneziaWG module ----

func setupModule(u *setupUI, s Store) error {
	if _, err := os.Stat(amneziawgModule); err == nil {
		u.say("✓ The amneziawg kernel module is loaded")
		return nil
	}
	if runCmd(io.Discard, sbin("modinfo"), "amneziawg") == nil {
		u.say("✗ The amneziawg module is installed but not loaded.")
		if u.check {
			return nil
		}
		if !u.ask("Load it now and at every boot (sudo modprobe; /etc/modules-load.d/amneziawg.conf)?", true) {
			return errStop
		}
		if err := runCmd(u.out, "sudo", "modprobe", "amneziawg"); err != nil {
			return err
		}
		if err := runCmd(u.out, "sh", "-c", "echo amneziawg | sudo tee /etc/modules-load.d/amneziawg.conf >/dev/null"); err != nil {
			return err
		}
		u.say("✓ The amneziawg module is loaded")
		return nil
	}
	u.say(`✗ The amneziawg kernel module is not installed: the egress needs it for its tunnels.
  Install it (DKMS, built for each kernel) as its README says for your system:
    https://github.com/amnezia-vpn/amneziawg-linux-kernel-module
  Ubuntu, for one: sudo add-apt-repository ppa:amnezia/ppa && sudo apt-get install amneziawg`)
	if u.check {
		return nil
	}
	return errStop
}

// sbin finds an administrator's tool that a user's PATH may lack
// (Debian keeps /usr/sbin out of it).
func sbin(name string) string {
	if p, err := lookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/sbin", "/sbin"} {
		if p := filepath.Join(dir, name); fileExists(p) {
			return p
		}
	}
	return name
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---- user services without a login ----

// lingerDir is where systemd-logind marks the users whose services run
// without a login.
var lingerDir = "/var/lib/systemd/linger"

func setupLinger(u *setupUI, s Store) error {
	name := currentUser()
	if fileExists(filepath.Join(lingerDir, name)) {
		u.say("✓ %s's services run without a login (linger)", name)
		return nil
	}
	u.say("✗ %s's services stop when they log out: the bot and the panel need linger.", name)
	if u.check {
		return nil
	}
	if u.ask(fmt.Sprintf("Turn it on (sudo loginctl enable-linger %s)?", name), true) {
		return runCmd(u.out, "sudo", "loginctl", "enable-linger", name)
	}
	return nil
}

// ---- the route around a host VPN ----

// routeUnit is deploy/reflux/host/reflux-egress-route.service for a host
// VPN unit (%[1]s); TestRouteUnitMatchesTheDeployFile keeps them alike.
const routeUnit = `# ReFlux: send the egress container's tunnels out of the plain uplink.
# Written by reflux setup; see deploy/reflux/host/reflux-egress-route.service.

[Unit]
Description=ReFlux: route the egress subnet around the host VPN
After=%[1]s
PartOf=%[1]s
Before=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=-/usr/sbin/ip rule del from 172.31.250.0/24 lookup main priority 100
ExecStart=/usr/sbin/ip rule add from 172.31.250.0/24 lookup main priority 100
ExecStop=/usr/sbin/ip rule del from 172.31.250.0/24 lookup main priority 100

[Install]
WantedBy=multi-user.target %[1]s
`

var vpnIfaceRe = regexp.MustCompile(`^\d+:\s+((?:awg|wg)\w*)[:@]`)

// guessVPNUnit finds the systemd unit of a host VPN interface (awg0,
// wg0): the first of its usual unit names that is active.
func guessVPNUnit() string {
	var links strings.Builder
	runCmd(&links, "ip", "-o", "link", "show")
	for _, l := range strings.Split(links.String(), "\n") {
		m := vpnIfaceRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		for _, unit := range []string{m[1] + ".service", "awg-quick@" + m[1] + ".service", "wg-quick@" + m[1] + ".service"} {
			if runCmd(io.Discard, "systemctl", "is-active", "--quiet", unit) == nil {
				return unit
			}
		}
	}
	return ""
}

func setupRoute(u *setupUI, s Store) error {
	rules, err := readRules()
	if err != nil {
		return fmt.Errorf("reading the host's routing rules: %w", err)
	}
	if hostRuleProblem(rules) == "" {
		u.say("✓ The egress tunnels reach their servers directly, past any host VPN")
		return nil
	}
	u.say("✗ This host's own traffic goes through a VPN: the egress tunnels need a rule to go around it.")
	if u.check {
		return nil
	}
	unit := guessVPNUnit()
	if a := u.line(fmt.Sprintf("The host VPN's systemd unit [%s]:", orDefault(unit, "awg0.service"))); a != "" {
		unit = a
	}
	unit = orDefault(unit, "awg0.service")
	if !u.ask("Install /etc/systemd/system/reflux-egress-route.service for it (sudo)?", true) {
		return errStop
	}
	tmp, err := os.CreateTemp("", "reflux-route-*.service")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	fmt.Fprintf(tmp, routeUnit, unit)
	tmp.Close()
	for _, c := range [][]string{
		{"sudo", "install", "-m", "0644", tmp.Name(), "/etc/systemd/system/reflux-egress-route.service"},
		{"sudo", "systemctl", "daemon-reload"},
		{"sudo", "systemctl", "enable", "--now", "reflux-egress-route.service"},
	} {
		if err := runCmd(u.out, c[0], c[1:]...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
		}
	}
	if rules, _ := readRules(); hostRuleProblem(rules) != "" {
		return fmt.Errorf("the rule is still missing: %s", hostRuleProblem(rules))
	}
	u.say("✓ The egress tunnels go around the host VPN")
	return nil
}

// ---- the tunnels' configs ----

// awgConfError says what an AmneziaWG config lacks, or "".
func awgConfError(text string) string {
	for _, need := range []string{"[Interface]", "PrivateKey", "[Peer]", "PublicKey", "Endpoint"} {
		if !strings.Contains(text, need) {
			return "it has no " + need
		}
	}
	return ""
}

// copyConf puts an AmneziaWG config into the egress directory as name.
func (u *setupUI) copyConf(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if msg := awgConfError(string(b)); msg != "" {
		return fmt.Errorf("%s is not an AmneziaWG config: %s", src, msg)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		return err
	}
	u.say("  %s → %s", src, dst)
	return nil
}

func setupConfigs(u *setupUI, s Store) error {
	if !u.check {
		if err := s.Init(); err != nil {
			return err
		}
	}
	dir := filepath.Join(s.Root, "egress")
	world, _ := filepath.Glob(filepath.Join(dir, "world-*.conf"))
	ru, _ := filepath.Glob(filepath.Join(dir, "ru-*.conf"))
	_, directErr := os.Stat(filepath.Join(dir, ruDirectFile))
	if len(world) > 0 && (len(ru) > 0 || directErr == nil) {
		u.say("✓ Tunnel configs: %s; Russia: %s", strings.Join(baseNames(world), ", "),
			orDefault(strings.Join(baseNames(ru), ", "), "directly"))
		return nil
	}
	if len(world) == 0 {
		u.say("✗ No world tunnel configs in %s (world-1.conf, world-2.conf, …: the rest of the world, in failover order).", dir)
		if u.check {
			u.say("  The wizard asks for the AmneziaWG .conf files and copies them in.")
		} else {
			u.say("AmneziaWG configs of servers abroad, each issued for this server only. Paths one per line, an empty line to end:")
			for n := 1; ; n++ {
				p := u.line(fmt.Sprintf("  world-%d.conf from:", n))
				if p == "" {
					break
				}
				if err := u.copyConf(p, filepath.Join(dir, fmt.Sprintf("world-%d.conf", n))); err != nil {
					u.say("  %v", err)
					n--
				}
			}
			if world, _ = filepath.Glob(filepath.Join(dir, "world-*.conf")); len(world) == 0 {
				u.say("The egress needs at least one world config: add them and run reflux setup again.")
				return errStop
			}
		}
	}
	if len(ru) == 0 && directErr != nil {
		u.say("✗ No Russian tunnel config (ru-1.conf).")
		if u.check {
			return nil
		}
		p := u.line("AmneziaWG config of a server in Russia (empty: send Russian traffic directly from this server):")
		switch {
		case p == "":
			if !u.ask("Russian sites and services then see this server's own address. Go on that way?", false) {
				return errStop
			}
			if err := os.WriteFile(filepath.Join(dir, ruDirectFile), nil, 0o600); err != nil {
				return err
			}
		default:
			if err := u.copyConf(p, filepath.Join(dir, "ru-1.conf")); err != nil {
				return err
			}
			if u.ask("When the Russian tunnel fails, send Russia directly from this server's address until it is back?", false) {
				if err := os.WriteFile(filepath.Join(dir, ruFallbackFile), nil, 0o600); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ---- heal from cron ----

func setupCron(u *setupUI, s Store) error {
	var b strings.Builder
	runCmd(&b, "crontab", "-l")
	if strings.Contains(b.String(), "reflux heal") {
		u.say("✓ cron runs reflux heal every minute")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "reflux"
	}
	line := fmt.Sprintf("* * * * * %s heal >> %s 2>&1", exe, filepath.Join(s.Root, "heal.log"))
	u.say("✗ cron does not run reflux heal: nodes left without a network after an egress restart would stay so.")
	if u.check {
		return nil
	}
	if _, err := lookPath("crontab"); err != nil {
		u.say("  cron is not installed: sudo apt-get install cron, then run reflux setup again.")
		return errStop
	}
	if !u.ask("Add it to your crontab?", true) {
		return nil
	}
	return runCmd(u.out, "sh", "-c", fmt.Sprintf("(crontab -l 2>/dev/null; echo '%s') | crontab -", line))
}

// ---- the egress ----

// egressWait is how long setup waits for the egress tunnels.
var egressWait = 2 * time.Minute

func setupStart(u *setupUI, s Store) error {
	if st, err := readEgressStatus(); err == nil && st.WorldOK {
		u.say("✓ The egress runs: world via %s", st.World)
		return nil
	}
	u.say("✗ The egress is not running yet.")
	if u.check {
		return nil
	}
	if !u.ask("Start it now (reflux apply: pulls the images, starts the egress)?", true) {
		return errStop
	}
	if err := locked(s, func() error { return apply(s, u.out) }); err != nil {
		return err
	}
	u.say("Waiting for the tunnels (up to %s)…", egressWait)
	for deadline := time.Now().Add(egressWait); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		if st, err := readEgressStatus(); err == nil && st.WorldOK {
			u.say("✓ The egress runs: world via %s, Russia %s", st.World, ruModeName(st))
			return nil
		}
	}
	u.say("The tunnels are not up yet: reflux logs egress tells why.")
	return nil
}

// locked runs f under the data directory's lock, as the commands that
// change clients or containers do; setup itself waits for people and
// installs, so it holds the lock only for such a change.
func locked(s Store, f func() error) error {
	unlock, err := s.Lock(lockWait)
	if err != nil {
		return err
	}
	defer unlock()
	return f()
}

func ruModeName(st egressStatus) string {
	return tr(langEN, ruMode(st).id)
}

// ---- what else the owner wants ----

// lanAddress is the host's first private IPv4 address outside Docker's
// networks: where the panel listens.
var lanAddress = func() string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 ||
			strings.HasPrefix(i.Name, "docker") || strings.HasPrefix(i.Name, "br-") || strings.HasPrefix(i.Name, "reflux") ||
			strings.HasPrefix(i.Name, "veth") || strings.HasPrefix(i.Name, "awg") || strings.HasPrefix(i.Name, "wg") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil && ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	return ""
}

func setupExtras(u *setupUI, s Store) error {
	if _, err := s.loadBotConfig(); err != nil {
		if u.ask("\nSet up the Telegram bot: alerts and managing clients from your phone?", true) {
			if err := botSetup(s, u.in, u.out, 5*time.Minute); err != nil {
				u.say("The bot is not set up: %v (later: reflux bot setup)", err)
			} else if err := botInstall(s, u.out); err != nil {
				u.say("The bot does not run: %v", err)
			}
		}
	} else {
		u.say("✓ The Telegram bot is set up")
	}
	if _, ok, _ := s.loadClientBot(); !ok {
		if _, err := s.loadBotConfig(); err == nil && u.ask("Set up the client bot, where people ask for access?", false) {
			if err := clientBotSetup(s, nil, u.in, u.out); err != nil {
				u.say("The client bot is not set up: %v (later: reflux bot client)", err)
			} else {
				botInstall(s, io.Discard)
			}
		}
	}
	if _, err := s.loadWebConfig(); err != nil {
		if ip := lanAddress(); ip != "" && u.ask(fmt.Sprintf("Run the web panel for your home network at http://%s:8686?", ip), true) {
			if err := webInstall(s, ip+":8686", "", u.out); err != nil {
				u.say("The panel does not run: %v (later: reflux web install)", err)
			} else {
				u.say("Sign in with a one-time link: reflux web login (or /web in the bot).")
			}
		}
	} else {
		u.say("✓ The web panel is set up")
	}
	if _, installed, _ := s.loadBackupConfig(); !installed {
		if u.ask("Back the data directory up every day (kept on this server)?", true) {
			dir := s.defaultBackupDir()
			if a := u.line(fmt.Sprintf("Where [%s]:", dir)); a != "" {
				dir = a
			}
			if err := backupInstall(s, dir, backupKeep, u.out); err != nil {
				u.say("Backups are not set up: %v (later: reflux backup install)", err)
			}
		}
	} else {
		u.say("✓ Backups are set up")
	}
	if clients, _ := s.List(); len(clients) == 0 && u.ask("Add the first client now?", true) {
		name := u.line("Name (latin letters, digits, dashes):")
		url := u.line("Link to a new mail.ru document (anyone with the link can edit):")
		if name != "" && url != "" {
			var c Client
			err := locked(s, func() error {
				var err error
				if c, err = s.Add(name, transportOf(url), url); err != nil {
					return err
				}
				return apply(s, io.Discard)
			})
			if err != nil {
				u.say("Not added: %v (later: reflux add)", err)
			} else if err := printAccess(s, c, u.out, ""); err != nil {
				u.say("%v", err)
			}
		}
	}
	return nil
}
