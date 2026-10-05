package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

// Image updates: the bot asks the registry every updatesEvery whether the
// node and egress images have a new digest, and offers the owner an update
// with a button; with night updates on, it updates by itself between
// nightFrom and nightTo. Only ghcr.io images are asked about, anonymously
// (ReFlux's are public).

const (
	updatesEvery = 6 * time.Hour
	nightFrom    = 4 // the hour night updates start, local time
	nightTo      = 5
)

// registryBase is ghcr.io's address; tests replace it.
var registryBase = "https://ghcr.io"

// imageState is an image's digest here and in the registry.
type imageState struct {
	Image, Local, Remote string
}

func (i imageState) newer() bool { return i.Remote != "" && i.Local != i.Remote }

// remoteDigest asks the registry for an image tag's digest: "" for an
// image not on ghcr.io.
func remoteDigest(image string) (string, error) {
	rest, ok := strings.CutPrefix(image, "ghcr.io/")
	if !ok {
		return "", nil
	}
	repo, tag, ok := strings.Cut(rest, ":")
	if !ok {
		tag = "latest"
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(registryBase + "/token?scope=repository:" + repo + ":pull")
	if err != nil {
		return "", err
	}
	var tok struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.Token == "" {
		return "", fmt.Errorf("registry token for %s: %v", repo, err)
	}
	req, _ := http.NewRequest(http.MethodHead, registryBase+"/v2/"+repo+"/manifests/"+tag, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"}, ", "))
	resp, err = client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry: %s for %s", resp.Status, image)
	}
	return resp.Header.Get("Docker-Content-Digest"), nil
}

// localDigest is the digest the image here was pulled at.
func localDigest(image string) string {
	var b strings.Builder
	if quiet(&b, "image", "inspect", "--format", "{{json .RepoDigests}}", image) != nil {
		return ""
	}
	var digests []string
	json.Unmarshal([]byte(strings.TrimSpace(b.String())), &digests)
	repo, _, _ := strings.Cut(image, ":")
	for _, d := range digests {
		if r, digest, ok := strings.Cut(d, "@"); ok && r == repo {
			return digest
		}
	}
	return ""
}

// checkImages compares the node and egress images with the registry.
func checkImages() ([]imageState, error) {
	o := options()
	var out []imageState
	var errs []error
	for _, img := range []string{o.NodeImage, o.EgressImage} {
		remote, err := remoteDigest(img)
		if err != nil {
			errs = append(errs, err)
		}
		out = append(out, imageState{Image: img, Local: localDigest(img), Remote: remote})
	}
	return out, errors.Join(errs...)
}

func anyNewer(states []imageState) bool {
	for _, i := range states {
		if i.newer() {
			return true
		}
	}
	return false
}

// remoteKey names a set of registry digests: an offer is made once each.
func remoteKey(states []imageState) string {
	var k []string
	for _, i := range states {
		k = append(k, i.Remote)
	}
	return strings.Join(k, ",")
}

// updateImages pulls the images, recreates what changed and removes the
// old ReFlux images: reflux update. The caller holds the lock.
func updateImages(s Store, stdout io.Writer) error {
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
}

// ---- the bot ----

// updatesState is what the bot remembers of updates across restarts.
type updatesState struct {
	Checked time.Time `json:"checked"`
	Offered string    `json:"offered,omitempty"` // the remote digests last offered
	Night   string    `json:"night,omitempty"`   // the remote digests last updated to at night
}

func (s Store) updatesPath() string { return filepath.Join(s.Root, "updates.json") }

func (s Store) readUpdates() updatesState {
	var u updatesState
	if b, err := os.ReadFile(s.updatesPath()); err == nil {
		json.Unmarshal(b, &u)
	}
	return u
}

// checkUpdates looks for new images every updatesEvery: it offers them
// once, or at night updates by itself when the owner turned that on. The
// caller holds b.mu.
func (b *bot) checkUpdates(now time.Time, auto bool) {
	st := b.s.readUpdates()
	if now.Sub(st.Checked) < updatesEvery && !(auto && inNight(now) && st.Offered != "" && st.Night != st.Offered) {
		return
	}
	states, err := checkImages()
	st.Checked = now
	defer func() { writeJSON(b.s.updatesPath(), st) }()
	if err != nil {
		log.Printf("bot: image updates: %v", err)
	}
	if !anyNewer(states) {
		return
	}
	key := remoteKey(states)
	if auto && inNight(now) && st.Night != key {
		st.Night = key
		err := b.update()
		if err != nil {
			b.t.Send(b.chat, b.tr("ui.upd.night.failed", html.EscapeString(err.Error())))
		} else {
			b.t.Send(b.chat, b.tr("ui.upd.night.done"))
		}
		st.Offered = key
		return
	}
	if st.Offered != key {
		if _, err := b.t.SendKeyboard(b.chat, b.tr("ui.upd.offer"), b.updateKeyboard()); err == nil {
			st.Offered = key
		}
	}
}

func inNight(t time.Time) bool { return t.Hour() >= nightFrom && t.Hour() < nightTo }

func (b *bot) updateKeyboard() keyboard {
	return keyboard{{b.btn("b.upd.now", "upd!:"+stamp())}, {b.btn("b.upd.screen", "upd")}}
}

// updatesScreen shows the images here and in the registry.
func (b *bot) updatesScreen(check bool) screen {
	var states []imageState
	var err error
	if check {
		states, err = checkImages()
		st := b.s.readUpdates()
		st.Checked = time.Now()
		writeJSON(b.s.updatesPath(), st)
	} else {
		o := options()
		for _, img := range []string{o.NodeImage, o.EgressImage} {
			states = append(states, imageState{Image: img, Local: localDigest(img)})
		}
	}
	var t strings.Builder
	t.WriteString(b.tr("ui.upd.title") + "\n")
	for _, i := range states {
		state := b.tr("ui.upd.unknown")
		switch {
		case i.Remote == "":
		case i.newer():
			state = b.tr("ui.upd.newer")
		default:
			state = b.tr("ui.upd.current")
		}
		fmt.Fprintf(&t, "\n%s: <code>%s</code> · %s", html.EscapeString(imageName(i.Image)), shortDigest(i.Local), state)
	}
	if err != nil {
		t.WriteString("\n\n⚠️ " + html.EscapeString(err.Error()))
	}
	auto := b.tr("ui.upd.auto.off")
	if c, err := b.s.loadBotConfig(); err == nil && c.AutoUpdate {
		auto = b.tr("ui.upd.auto.on")
	}
	t.WriteString("\n\n" + auto)
	kb := keyboard{{b.btn("b.upd.check", "upd?")}}
	if anyNewer(states) {
		kb = append(keyboard{{b.btn("b.upd.now", "upd!:"+stamp())}}, kb...)
	}
	return screen{t.String(), append(kb, []telegram.Button{b.btn("b.upd.auto", "updauto"), b.btn("b.server", "srv")})}
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	if d == "" {
		return "—"
	}
	return d
}

// updatePress handles upd (the screen), upd? (check now), upd!:<stamp>
// (update) and updauto (night updates on or off).
func (b *bot) updatePress(action, arg string) (screen, string) {
	switch action {
	case "upd":
		return b.updatesScreen(false), ""
	case "upd?":
		return b.updatesScreen(true), ""
	case "updauto":
		c, err := b.s.loadBotConfig()
		if err == nil {
			c.AutoUpdate = !c.AutoUpdate
			err = b.s.saveBotConfig(c)
		}
		if err != nil {
			return b.failed(err, b.btn("b.back", "upd")), ""
		}
		return b.updatesScreen(false), ""
	case "upd!":
		if !fresh(arg) {
			return b.updatesScreen(false), b.tr("ui.expired.button")
		}
		err := b.update()
		if err != nil {
			return b.failed(err, b.btn("b.back", "upd")), ""
		}
		sc := b.updatesScreen(false)
		sc.text = b.tr("ui.upd.done") + "\n\n" + sc.text
		return sc, ""
	}
	return b.home(), ""
}

// update runs reflux update for the bot, with the alerts of the restart
// it causes kept quiet. The caller holds b.mu.
func (b *bot) update() error {
	b.quietUntil = time.Now().Add(quietAfterUpdate)
	err := b.change(func() error { return updateImages(b.s, io.Discard) })
	b.quietUntil = time.Now().Add(quietAfterUpdate)
	return err
}

// quietAfterUpdate covers egress and the nodes coming back after an update.
const quietAfterUpdate = 5 * time.Minute
