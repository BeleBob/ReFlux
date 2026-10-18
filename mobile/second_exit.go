package mobile

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/p1neappleXpress/OpenFlux/routing"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// A second exit next to the running tunnel: another profile's node (say an
// official OpenFlux link) the tunnel mode shares its flows with. Each new
// site goes to the exit with fewer flows and stays there (packetRouter's
// pickExit), so a page loads over both while every site sees one address.
// The second exit's node may not tell the app its configuration, and its
// captchas, route and auth proxy are left to the main one.

var second struct {
	mu    sync.Mutex
	trans transport.Transport
	name  string
}

var errNoTunnel = errors.New("второй канал: основной туннель не запущен")

// StartSecondary connects a classic profile (with a key) as the second
// exit. The main tunnel (Start or StartSession) must be running. Returns ""
// or what went wrong.
func StartSecondary(transportType, documentURL, encryptionSecret, codec, maxToken, maxUid string) string {
	if encryptionSecret == "" {
		return "второй канал: нужен профиль с ключом шифрования"
	}
	if msg := validateClassic(transportType, documentURL, encryptionSecret, false); msg != "" {
		return msg
	}
	return startSecondaryWith(transportType, func() (transport.Transport, error) {
		if transportType == "" {
			transportType = "yandex"
		}
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

// StartSecondarySession connects a Session profile as the second exit;
// see StartSecondary.
func StartSecondarySession(specsJSON, encryptionSecret string) string {
	return startSecondaryWith("session", func() (transport.Transport, error) {
		t, _, err := buildSessionWith(specsJSON, encryptionSecret, false, sessionOptions{secondary: true})
		return t, err
	})
}

func startSecondaryWith(name string, build func() (transport.Transport, error)) string {
	client.mu.Lock()
	running := client.running && client.transport != nil
	client.mu.Unlock()
	if !running {
		return errNoTunnel.Error()
	}
	StopSecondary()
	appendLog(fmt.Sprintf("[ANDROID] Второй канал: запуск (%s)", name))
	trans, err := build()
	if err != nil {
		appendLog(fmt.Sprintf("[ERROR] Второй канал: %v", err))
		return err.Error()
	}
	trans.Receive(func(data []byte) {
		packet := append([]byte(nil), data...)
		if pr := active.Load(); pr != nil {
			packet = pr.inbound(packet)
		}
		queuePacket(packet)
	})
	if err := trans.Start(); err != nil {
		appendLog(fmt.Sprintf("[ERROR] Второй канал: %v", err))
		return err.Error()
	}
	pr := active.Load()
	if pr == nil {
		// No routing: a router that sends every flow through a tunnel,
		// only to share them between the two exits.
		pr = newPacketRouter(routing.NewRouter(routing.Config{Mode: routing.ModeTunnel}, nil), routingUpstream(), queuePacket)
		pr.primaryUp = IsConnected
		if !active.CompareAndSwap(nil, pr) {
			pr.close()
			pr = active.Load()
		}
	}
	second.mu.Lock()
	second.trans, second.name = trans, name
	second.mu.Unlock()
	pr.setSecond(trans)
	utils.Infof("[ANDROID] Второй канал: подключается, новые сайты делятся между двумя нодами")
	return ""
}

// StopSecondary disconnects the second exit; its flows break, new ones go
// through the main tunnel.
func StopSecondary() {
	second.mu.Lock()
	trans := second.trans
	second.trans, second.name = nil, ""
	second.mu.Unlock()
	if trans == nil {
		return
	}
	if pr := active.Load(); pr != nil {
		pr.setSecond(nil)
	}
	appendLog("[ANDROID] Второй канал: остановлен")
	_ = trans.Stop()
}

// SecondaryIsConnected reports whether the second exit is up.
func SecondaryIsConnected() bool {
	second.mu.Lock()
	t := second.trans
	second.mu.Unlock()
	return t != nil && t.IsConnected()
}

// SecondaryIsRunning reports whether a second exit was started (connected
// or still connecting).
func SecondaryIsRunning() bool {
	second.mu.Lock()
	defer second.mu.Unlock()
	return second.trans != nil
}
