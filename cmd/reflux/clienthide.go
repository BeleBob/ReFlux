package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

// Hiding the client bot from strangers. While client-bot.json says hidden,
// the bot answers only the people it already deals with — a client's person,
// a request waiting or approved — and a valid invite's /start; anyone else
// gets no answer at all, as if the bot did not exist. The owner bot's
// settings, the panel's requests page and `reflux bot client hide|show`
// switch it. The bot reads the file on every update, so a switch from the
// panel (another process) takes effect at once.

var errNoClientBot = errors.New("there is no client bot: reflux bot client")

// hasClientBot reports whether a client bot is set up (reflux bot client).
func (s Store) hasClientBot() bool {
	_, ok, err := s.loadClientBot()
	return err == nil && ok
}

// clientBotHidden reports whether the client bot keeps quiet to strangers.
func (s Store) clientBotHidden() bool {
	c, ok, err := s.loadClientBot()
	return err == nil && ok && c.Hidden
}

// setClientBotHidden hides the client bot from strangers or shows it to
// everyone again.
func (s Store) setClientBotHidden(hidden bool) error {
	c, ok, err := s.loadClientBot()
	if err != nil {
		return err
	}
	if !ok {
		return errNoClientBot
	}
	c.Hidden = hidden
	return writeJSON(s.clientBotPath(), c)
}

// inviteValid reports whether code is an invite that /start would take,
// without spending it.
func (s Store) inviteValid(code string, now time.Time) bool {
	if !inviteCodeRe.MatchString(code) {
		return false
	}
	var inv invite
	b, err := os.ReadFile(s.invitePath(code))
	return err == nil && json.Unmarshal(b, &inv) == nil && now.Before(inv.Expires)
}

// answers reports whether the client bot answers u, who sent text (empty
// for a button): everyone while it is shown; while hidden, the person of a
// client, of a request waiting or approved, or of a valid invite.
func (cb *clientBot) answers(u *telegram.User, text string) bool {
	if !cb.s.clientBotHidden() {
		return true
	}
	if _, ok := cb.s.clientOf(u.ID); ok {
		return true
	}
	if r, err := cb.s.getRequest(u.ID); err == nil && (r.State == reqPending || r.State == reqApproved) {
		return true
	}
	cmd, arg, _ := strings.Cut(text, " ")
	return strings.SplitN(cmd, "@", 2)[0] == "/start" && cb.s.inviteValid(strings.TrimSpace(arg), time.Now())
}

// toggleClientBot is the owner bot's settings button.
func (b *bot) toggleClientBot() screen {
	if err := b.s.setClientBotHidden(!b.s.clientBotHidden()); err != nil {
		return b.failed(err, b.btn("b.home", "home"))
	}
	return b.settingsScreen()
}

// clientBotLines adds the client bot's state and its switch to the owner
// bot's settings, when there is a client bot.
func (b *bot) clientBotLines(t *strings.Builder, kb keyboard) keyboard {
	if !b.s.hasClientBot() {
		return kb
	}
	state, button := "ui.cbot.shown", "b.cbot.hide"
	if b.s.clientBotHidden() {
		state, button = "ui.cbot.hidden", "b.cbot.show"
	}
	t.WriteString("\n" + b.tr(state) + "\n")
	return append(kb, []telegram.Button{b.btn(button, "cbhide")})
}

// clientBotAction is the panel's switch: POST /clientbot/hide or /show.
func (w *webServer) clientBotAction(r *http.Request) (string, error) {
	switch what := r.PathValue("what"); what {
	case "hide", "show":
		if err := w.s.setClientBotHidden(what == "hide"); err != nil {
			return "", err
		}
		return "/requests?ok=cbot_" + what, nil
	}
	return "", errors.New("clientbot: hide or show")
}
