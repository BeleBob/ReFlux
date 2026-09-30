package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Telegram bot tells the owner when a check of `reflux doctor` changes
// (a tunnel down, a node that lost its document, a failover, an update)
// and answers a few commands. It talks to the Bot API from the host, like
// docker pulls do: an alert about the egress must not depend on it.

const botUsage = `usage:
  reflux bot setup     link a Telegram bot (token from @BotFather) to your account
  reflux bot install   run the bot as a systemd user service (again after an update)
  reflux bot test      send a test message
  reflux bot run       run in the foreground (what the service does)`

// botConfig is telegram.json in the data directory (0600): the token is a
// secret, and chat is the only chat the bot talks to.
type botConfig struct {
	Token string `json:"token"`
	Chat  int64  `json:"chat"`
}

func (s Store) botConfigPath() string { return filepath.Join(s.Root, "telegram.json") }

func (s Store) loadBotConfig() (botConfig, error) {
	var c botConfig
	b, err := os.ReadFile(s.botConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, errors.New("no Telegram bot yet: run reflux bot setup")
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", s.botConfigPath(), err)
	}
	if c.Token == "" || c.Chat == 0 {
		return c, fmt.Errorf("%s is incomplete: run reflux bot setup", s.botConfigPath())
	}
	return c, nil
}

func (s Store) saveBotConfig(c botConfig) error {
	if err := s.Init(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.botConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.botConfigPath())
}

func cmdBot(s Store, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New(botUsage)
	}
	switch args[0] {
	case "setup":
		return botSetup(s, stdin, stdout, 5*time.Minute)
	case "install":
		return botInstall(s, stdout)
	case "test":
		c, err := s.loadBotConfig()
		if err != nil {
			return err
		}
		if err := newTelegram(c.Token).send(c.Chat, "ReFlux bot test: messages reach you."); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Sent.")
		return nil
	case "run":
		c, err := s.loadBotConfig()
		if err != nil {
			return err
		}
		return newBot(s, c).run()
	}
	return errors.New(botUsage)
}

var tokenRe = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]{30,}$`)

// botSetup links a bot to the owner's chat: it takes the token on stdin
// (never an argument: those show up in ps and the shell history) and the
// chat from the first private message the bot gets.
func botSetup(s Store, stdin io.Reader, stdout io.Writer, wait time.Duration) error {
	in := bufio.NewReader(stdin)
	fmt.Fprint(stdout, `1. In Telegram, open @BotFather, send /newbot and follow its questions.
2. Paste the token it gives you here (it is kept in `+s.botConfigPath()+`):
> `)
	line, _ := in.ReadString('\n')
	token := strings.TrimSpace(line)
	if !tokenRe.MatchString(token) {
		return errors.New("that does not look like a bot token (digits:letters)")
	}
	t := newTelegram(token)
	me, err := t.getMe()
	if err != nil {
		return fmt.Errorf("the token does not work: %w", err)
	}
	// Whatever was sent to the bot before now is not the owner's answer.
	offset, err := skipPending(t)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "3. Open https://t.me/%s in Telegram and press Start. Waiting...\n", me.Username)
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		ups, err := t.getUpdates(offset, min(30*time.Second, time.Until(deadline).Round(time.Second)))
		if err != nil {
			return err
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.Chat.Type != "private" || m.From == nil {
				continue
			}
			fmt.Fprintf(stdout, "Message from %s (@%s, id %d). Link the bot to this account? Type yes: ",
				m.From.FirstName, m.From.Username, m.Chat.ID)
			answer, _ := in.ReadString('\n')
			if strings.TrimSpace(answer) != "yes" {
				return errors.New("not linked; run reflux bot setup again")
			}
			// Mark it read, or the running bot would see it again.
			t.getUpdates(offset, 0)
			if err := s.saveBotConfig(botConfig{Token: token, Chat: m.Chat.ID}); err != nil {
				return err
			}
			t.send(m.Chat.ID, "ReFlux bot linked. It will write when something breaks or changes. /help lists the commands.")
			fmt.Fprintln(stdout, "Linked. Next: reflux bot install")
			return nil
		}
	}
	return errors.New("no message arrived; run reflux bot setup again")
}

// skipPending marks every update waiting for the bot as read and returns
// the offset after them.
func skipPending(t *telegram) (int64, error) {
	ups, err := t.getUpdates(-1, 0)
	if err != nil || len(ups) == 0 {
		return 0, err
	}
	return ups[len(ups)-1].UpdateID + 1, nil
}

// botUnit is the systemd user service. REFLUX_HOME pins the data
// directory the bot was set up with.
const botUnit = `# Generated by reflux bot install.
[Unit]
Description=ReFlux Telegram bot (alerts and commands)

[Service]
ExecStart=%s bot run
Environment=REFLUX_HOME=%s
Restart=always
RestartSec=15

[Install]
WantedBy=default.target
`

// botInstall writes and (re)starts the user service. Run it again after
// replacing the reflux binary: the restart picks up the new one.
func botInstall(s Store, stdout io.Writer) error {
	if _, err := s.loadBotConfig(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := filepath.Join(dir, "reflux-bot.service")
	if err := os.WriteFile(unit, []byte(fmt.Sprintf(botUnit, exe, s.Root)), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "reflux-bot.service"},
		{"--user", "restart", "reflux-bot.service"},
	} {
		if err := runCmd(stdout, "systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	fmt.Fprintf(stdout, "Installed %s and started it.\n", unit)
	var linger strings.Builder
	runCmd(&linger, "loginctl", "show-user", fmt.Sprint(os.Getuid()), "--property=Linger")
	if strings.TrimSpace(linger.String()) != "Linger=yes" {
		fmt.Fprintln(stdout, "The bot stops when you log out until user services may run without a login; once:\n  sudo loginctl enable-linger $USER")
	}
	return nil
}

// checkEvery is how often the bot runs the checks; confirmRuns is how many
// runs in a row a change must last before it is reported, so a node that
// restarts or a tunnel that blinks does not wake anyone.
var (
	checkEvery  = time.Minute
	confirmRuns = 2
)

type bot struct {
	s    Store
	t    *telegram
	chat int64
	// mu serializes the checks and the commands: both run docker and
	// swap dockerStderr.
	mu     sync.Mutex
	mon    monitor
	outbox []string // alerts not delivered yet
}

func newBot(s Store, c botConfig) *bot {
	return &bot{s: s, t: newTelegram(c.Token), chat: c.Chat, mon: monitor{confirm: confirmRuns}}
}

func (b *bot) run() error {
	offset, err := skipPending(b.t)
	if err != nil {
		return err
	}
	log.Print("bot: started")
	go b.watch()
	for {
		ups, err := b.t.getUpdates(offset, 50*time.Second)
		if err != nil {
			log.Printf("bot: %v", err)
			wait := 10 * time.Second
			if te, ok := err.(*tgError); ok && te.RetryAfter > 0 {
				wait = te.RetryAfter
			}
			time.Sleep(wait)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			b.handle(u)
		}
	}
}

// watch runs the checks every minute and reports what changed.
func (b *bot) watch() {
	for first := true; ; first = false {
		b.mu.Lock()
		fs := runChecks(b.s)
		b.mu.Unlock()
		news := b.mon.update(fs)
		if first {
			news = []string{startMessage(fs)}
		}
		b.deliver(news)
		time.Sleep(checkEvery)
	}
}

// deliver sends the news of one run as one message, with whatever an
// earlier run could not deliver in front.
func (b *bot) deliver(news []string) {
	if len(news) > 0 {
		b.outbox = append(b.outbox, strings.Join(news, "\n"))
	}
	if len(b.outbox) > 20 {
		b.outbox = b.outbox[len(b.outbox)-20:]
	}
	for len(b.outbox) > 0 {
		if err := b.t.send(b.chat, b.outbox[0]); err != nil {
			log.Printf("bot: alert not sent, retrying next run: %v", err)
			return
		}
		b.outbox = b.outbox[1:]
	}
}

func startMessage(fs []finding) string {
	host, _ := os.Hostname()
	warns, fails := count(fs)
	msg := fmt.Sprintf("ReFlux bot started on %s: %d problem(s), %d warning(s).", html.EscapeString(host), fails, warns)
	for _, f := range fs {
		if f.Level != levelOK {
			msg += "\n" + alertLine(f)
		}
	}
	return msg
}

func alertLine(f finding) string {
	mark := map[level]string{levelOK: "✅", levelWarn: "⚠️", levelFail: "❌"}[f.Level]
	return mark + " " + html.EscapeString(f.Text)
}

// monitor turns runs of the checks into news: a check whose signature
// changed and stayed changed for confirm runs.
type monitor struct {
	confirm  int
	reported map[string]finding // what the owner was told last (first run: the baseline)
	pending  map[string]pendingChange
}

type pendingChange struct {
	sig  string
	runs int
}

func (m *monitor) update(fs []finding) []string {
	cur := map[string]finding{}
	var keys []string
	for _, f := range fs {
		cur[f.Key] = f
		keys = append(keys, f.Key)
	}
	if m.reported == nil {
		m.reported, m.pending = cur, map[string]pendingChange{}
		return nil
	}
	var gone []string
	for k := range m.reported {
		if _, ok := cur[k]; !ok {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	var news []string
	for _, k := range append(keys, gone...) {
		c, now := cur[k]
		r, was := m.reported[k]
		sig, rsig := "gone", "gone"
		if now {
			sig = c.Sig
		}
		if was {
			rsig = r.Sig
		}
		if sig == rsig {
			delete(m.pending, k)
			continue
		}
		p := m.pending[k]
		if p.sig != sig {
			p = pendingChange{sig: sig}
		}
		p.runs++
		if p.runs < m.confirm {
			m.pending[k] = p
			continue
		}
		delete(m.pending, k)
		if !now {
			// A check that stopped running: a revoked client, or the
			// checks after a failing docker, which reports itself.
			delete(m.reported, k)
			continue
		}
		m.reported[k] = c
		switch {
		case c.Level != levelOK:
			news = append(news, alertLine(c))
		case was && r.Level != levelOK:
			news = append(news, alertLine(c)) // resolved
		case was:
			news = append(news, "ℹ️ "+html.EscapeString(c.Text)) // a failover, an update
		}
	}
	return news
}

const botHelp = `ReFlux bot. It writes when a check of reflux doctor changes.
/status  tunnels and clients
/doctor  every check
/pause &lt;name&gt;, /resume &lt;name&gt;
/expire &lt;name&gt; &lt;never|2026-12-31|30d|2w|12h&gt;`

// handle answers a message from the owner's chat; anyone else is ignored.
func (b *bot) handle(u tgUpdate) {
	m := u.Message
	if m == nil || m.Chat.ID != b.chat {
		return
	}
	f := strings.Fields(m.Text)
	if len(f) == 0 {
		return
	}
	cmd, _, _ := strings.Cut(f[0], "@") // /status@SomeBot in groups
	args := f[1:]
	var reply string
	b.mu.Lock()
	switch cmd {
	case "/start", "/help":
		reply = botHelp
	case "/status":
		reply = pre(botStatus(b.s))
	case "/doctor":
		fs := runChecks(b.s)
		warns, fails := count(fs)
		reply = pre(formatFindings(fs) + fmt.Sprintf("\n%d problem(s), %d warning(s)", fails, warns))
	case "/pause", "/resume", "/expire":
		var out strings.Builder
		var err error
		switch cmd {
		case "/pause":
			err = cmdPause(b.s, args, &out)
		case "/resume":
			err = cmdResume(b.s, args, &out)
		default:
			err = cmdExpire(b.s, args, &out)
		}
		text := out.String()
		if err != nil {
			text += "error: " + err.Error()
		}
		reply = pre(text)
	default:
		reply = "Unknown command. /help"
	}
	b.mu.Unlock()
	if err := b.t.send(b.chat, reply); err != nil {
		log.Printf("bot: reply not sent: %v", err)
	}
}

// pre marks text up as preformatted, cut to fit a message.
func pre(text string) string {
	text = html.EscapeString(strings.TrimSpace(text))
	if len(text) > 3900 {
		cut := strings.LastIndex(text[:3900], "\n")
		if cut < 0 {
			cut = 3900
		}
		text = text[:cut] + "\n…"
	}
	return "<pre>" + text + "</pre>"
}

// botStatus is status for a phone screen: one short line per tunnel and
// per client.
func botStatus(s Store) string {
	var b strings.Builder
	var st strings.Builder
	if err := quiet(&st, "exec", "reflux-egress", "reflux-egress", "status"); err != nil {
		b.WriteString("egress: no status (not running?)\n")
	}
	for _, l := range strings.Split(st.String(), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || (f[0] != "world" && f[0] != "russia") {
			continue
		}
		// "world up via world-3.conf (since ...)", "russia up direct, 8652
		// prefixes (...)": up to the first comma or parenthesis.
		var short []string
		for _, w := range f {
			if strings.HasPrefix(w, "(") {
				break
			}
			short = append(short, strings.TrimSuffix(w, ","))
			if strings.HasSuffix(w, ",") {
				break
			}
		}
		fmt.Fprintf(&b, "%-7s%s\n", short[0], strings.Join(short[1:], " "))
	}
	clients, err := s.List()
	if err != nil {
		return b.String() + "clients: " + err.Error()
	}
	now := time.Now()
	states := containerStates()
	live := nodeStatuses(s, activeClients(clients, now))
	for _, c := range clients {
		line := accessText(c, now)
		if c.Active(now) {
			switch st, ok := live[c.Name]; {
			case states["reflux-node-"+c.Name] == "":
				line = "node not running"
			case !ok:
				line = "no status"
			default:
				line = fmt.Sprintf("%-7s %s↓ %s↑", onlineText(st), humanBytes(st.BytesOut), humanBytes(st.BytesIn))
			}
		}
		fmt.Fprintf(&b, "%-7s%s\n", c.Name, line)
	}
	if len(clients) == 0 {
		b.WriteString("no clients\n")
	}
	return b.String()
}
