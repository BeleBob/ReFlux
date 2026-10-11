package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/provision"
)

// nodeProvisioner creates a pool's exit node channel on this host. The heavy
// lifting is node-install.sh's plan/apply (as root); a cupsonline transport
// with an empty url means "make the rooms yourself". See docs/plans.
type nodeProvisioner interface {
	Ensure(ctx context.Context, p Pool) error
}

// scriptProvisioner runs node-install.sh on this machine, the way the owner
// does from the CLI, so a missing channel is made without their hands.
type scriptProvisioner struct {
	cfg  *Config
	logf func(format string, args ...any)
	// exec runs the script; runCmd runs it for real. Tests replace it and
	// return node-install.sh answers without a shell.
	exec func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error)
	// rooms makes cups.online rooms; cupsonline.CreateRoomList for real.
	rooms func(ctx context.Context) (string, error)
}

// scriptResult is node-install.sh's last JSON line.
type scriptResult struct {
	OK    bool   `json:"ok"`
	Port  int    `json:"port"`
	Error string `json:"error"`
}

func runCmd(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	return c.CombinedOutput()
}

// channelTransports fills a cupsonline transport with empty url with fresh
// cups.online rooms, and makes rooms for a pool with no transports at all, so
// a new channel always has a document transport for readChannel.
func (p *scriptProvisioner) channelTransports(ctx context.Context, ts []provision.ChannelTransport) ([]provision.ChannelTransport, error) {
	out := make([]provision.ChannelTransport, 0, len(ts)+1)
	for _, t := range ts {
		t.URL = strings.TrimSpace(t.URL)
		if t.Type == "cupsonline" && t.URL == "" {
			rooms, err := p.rooms(ctx)
			if err != nil {
				return nil, fmt.Errorf("cups.online: %w", err)
			}
			t.URL = rooms
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		rooms, err := p.rooms(ctx)
		if err != nil {
			return nil, fmt.Errorf("cups.online: %w", err)
		}
		out = append(out, provision.ChannelTransport{Type: "cupsonline", URL: rooms})
	}
	return provision.CheckTransports(out)
}

// Ensure creates the pool's channel on this host: node-install.sh plan picks a
// port, apply installs, configures and starts the openflux-node@channel unit.
func (p *scriptProvisioner) Ensure(ctx context.Context, pool Pool) error {
	ts, err := p.channelTransports(ctx, pool.Transports)
	if err != nil {
		return err
	}
	mode, _ := provision.CheckMode(pool.Mode) // checked in Config.Check already
	key, err := provision.NewKey()
	if err != nil {
		return err
	}
	cfg := "channel=" + pool.Channel + "\n" + transportLines(ts)
	if mode == provision.ModeL3 {
		cfg += "mode=l3\n"
	}
	cfg += "autoupdate=no\n"

	r, err := p.runScript(ctx, "plan", cfg)
	if err != nil {
		return err
	}
	if r.Port <= 0 || r.Port > 65535 {
		return fmt.Errorf("план ноды не дал порт (port=%d)", r.Port)
	}
	cfg += "key=" + key + "\n"
	cfg += "port=" + strconv.Itoa(r.Port) + "\n"
	if _, err := p.runScript(ctx, "apply", cfg); err != nil {
		return err
	}
	p.logf("pool %s: channel %s created on this host, port %d", pool.ID, pool.Channel, r.Port)
	return nil
}

// runScript feeds the config to the script the way its commands want it:
// plan reads stdin, apply a config file argument it deletes after itself.
func (p *scriptProvisioner) runScript(ctx context.Context, cmd, cfg string) (scriptResult, error) {
	var out []byte
	var err error
	if cmd == "plan" {
		out, err = p.exec(ctx, cfg, "sh", p.cfg.NodeScript, cmd)
	} else {
		f, err2 := os.CreateTemp("", "opensub-node-*.conf")
		if err2 != nil {
			return scriptResult{}, err2
		}
		defer os.Remove(f.Name())
		if err2 := os.WriteFile(f.Name(), []byte(cfg), 0o600); err2 != nil {
			return scriptResult{}, err2
		}
		out, err = p.exec(ctx, "", "sh", p.cfg.NodeScript, cmd, f.Name())
	}
	if err != nil {
		msg := firstLine(out)
		if msg == "" {
			msg = err.Error()
		}
		return scriptResult{}, fmt.Errorf("%s: %s", cmd, msg)
	}
	r, err := lastScriptJSON(out)
	if err != nil {
		return scriptResult{}, fmt.Errorf("%s: %v", cmd, err)
	}
	if !r.OK {
		return scriptResult{}, errors.New(r.Error)
	}
	return r, nil
}

// lastScriptJSON finds node-install.sh's last JSON line, like provision.lastJSON.
func lastScriptJSON(out []byte) (scriptResult, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			var r scriptResult
			if err := json.Unmarshal([]byte(l), &r); err != nil {
				return scriptResult{}, err
			}
			return r, nil
		}
	}
	return scriptResult{}, errors.New("node-install.sh не ответил")
}

// transportLines is the carriers' part of node-install.sh's config (the same
// as provision's, which is a package away).
func transportLines(ts []provision.ChannelTransport) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(t.Type + "=" + t.URL + "\n")
	}
	return b.String()
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}