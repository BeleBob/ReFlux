package mobile

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/p1neappleXpress/OpenFlux/routing"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Extra exits next to the running tunnel: other profiles' nodes (say an
// official OpenFlux link) the tunnel mode shares its flows with. Each new
// site goes to the exit with the fewest flows and stays there
// (packetRouter's pickExit), so a page loads over all of them while every
// site sees one address. An extra exit's node may not tell the app its
// configuration, and its captchas, route and auth proxy are left to the
// main one. The app names each by an id of its own (its profile's).

const maxExtraExits = 4

// ExtraNoTunnel is what StartExtra returns while the main tunnel is not
// running (it is starting or restarting): not an error, try again soon.
const ExtraNoTunnel = "no_tunnel"

type extraExit struct {
	slot  int
	trans transport.Transport
}

var extras = struct {
	sync.Mutex
	byID map[string]*extraExit
}{byID: map[string]*extraExit{}}

// StartExtra connects a classic profile (with a key) as an extra exit
// named id, replacing one of that id. The main tunnel (Start or
// StartSession) must be running. Returns "", ExtraNoTunnel or what went
// wrong.
func StartExtra(id, transportType, documentURL, encryptionSecret, codec, maxToken, maxUid string) string {
	if encryptionSecret == "" {
		return "нужен профиль с ключом шифрования"
	}
	if msg := validateClassic(transportType, documentURL, encryptionSecret, false); msg != "" {
		return msg
	}
	if transportType == "" {
		transportType = "yandex"
	}
	return startExtraWith(id, func() (transport.Transport, error) {
		specs, err := json.Marshal([]sessionSpec{{
			Name: transportType, Type: transportType, URL: documentURL, Priority: 100,
			Params: classicParams(transportType, documentURL, maxToken, maxUid),
		}})
		if err != nil {
			return nil, err
		}
		t, _, err := buildSessionWith(string(specs), encryptionSecret, false,
			sessionOptions{classic: true, codec: codec, secondary: true})
		return t, err
	})
}

// StartExtraSession connects a Session profile as an extra exit; see
// StartExtra.
func StartExtraSession(id, specsJSON, encryptionSecret string) string {
	return startExtraWith(id, func() (transport.Transport, error) {
		t, _, err := buildSessionWith(specsJSON, encryptionSecret, false, sessionOptions{secondary: true})
		return t, err
	})
}

func startExtraWith(id string, build func() (transport.Transport, error)) string {
	client.mu.Lock()
	running := client.running && client.transport != nil
	client.mu.Unlock()
	if !running {
		return ExtraNoTunnel
	}
	StopExtra(id)
	extras.Lock()
	used := map[int]bool{}
	for _, e := range extras.byID {
		used[e.slot] = true
	}
	slot := 0
	for s := 1; s <= maxExtraExits; s++ {
		if !used[s] {
			slot = s
			break
		}
	}
	extras.Unlock()
	if slot == 0 {
		return fmt.Sprintf("не больше %d дополнительных каналов", maxExtraExits)
	}
	appendLog(fmt.Sprintf("[ANDROID] Дополнительный канал %s: запуск", id))
	trans, err := build()
	if err != nil {
		appendLog(fmt.Sprintf("[ERROR] Дополнительный канал %s: %v", id, err))
		return err.Error()
	}
	trans.Receive(func(data []byte) {
		packet := append([]byte(nil), data...)
		if pr := active.Load(); pr != nil {
			// Counts the exit as heard and keeps its probe answers.
			if packet = pr.fromExtra(trans, packet); packet == nil {
				return
			}
		}
		queuePacket(packet)
	})
	if err := trans.Start(); err != nil {
		appendLog(fmt.Sprintf("[ERROR] Дополнительный канал %s: %v", id, err))
		return err.Error()
	}
	pr := active.Load()
	if pr == nil {
		// No routing: a router that sends every flow through a tunnel,
		// only to share them between the exits.
		pr = newPacketRouter(routing.NewRouter(routing.Config{Mode: routing.ModeTunnel}, nil), routingUpstream(), queuePacket)
		pr.primaryUp = IsConnected
		if !active.CompareAndSwap(nil, pr) {
			pr.close()
			pr = active.Load()
		}
	}
	extras.Lock()
	if old := extras.byID[id]; old != nil {
		// Started twice at once: keep this one.
		go old.trans.Stop()
	}
	extras.byID[id] = &extraExit{slot: slot, trans: trans}
	extras.Unlock()
	pr.setExtra(slot, trans)
	utils.Infof("[ANDROID] Дополнительный канал %s подключается: новые сайты делятся между нодами", id)
	return ""
}

// StopExtra disconnects the extra exit id; its flows break, new ones go
// through the other exits.
func StopExtra(id string) {
	extras.Lock()
	e := extras.byID[id]
	delete(extras.byID, id)
	extras.Unlock()
	if e == nil {
		return
	}
	if pr := active.Load(); pr != nil {
		pr.setExtra(e.slot, nil)
	}
	appendLog(fmt.Sprintf("[ANDROID] Дополнительный канал %s: остановлен", id))
	_ = e.trans.Stop()
}

// StopExtras disconnects every extra exit.
func StopExtras() {
	for _, id := range extraIDs() {
		StopExtra(id)
	}
}

func extraIDs() []string {
	extras.Lock()
	defer extras.Unlock()
	ids := make([]string, 0, len(extras.byID))
	for id := range extras.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ExtraIDs lists the extra exits started (connected or connecting),
// comma-separated.
func ExtraIDs() string { return strings.Join(extraIDs(), ",") }

// ExtraIsConnected reports whether the extra exit id is up: its node
// answers (its probes), not only its carriers.
func ExtraIsConnected(id string) bool {
	extras.Lock()
	e := extras.byID[id]
	extras.Unlock()
	if e == nil {
		return false
	}
	pr := active.Load()
	if pr == nil {
		return e.trans.IsConnected()
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.extraUp(e.slot, pr.now())
}
