package mobile

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// The app may send its log to the exit of the running tunnel, which keeps
// it for whoever runs the exit (control.SubtypeClientLog; ReFlux exits keep
// them in the client's state directory). It goes through the Session like
// any control message, so only to the exit this app holds the key of. What
// the exit takes, and how often, the exit decides; the app shows its answer.

// sendLogWait is how long SendLog waits for the exit's answer.
var sendLogWait = 60 * time.Second

// sendLogMaxText is the most of the log that is sent: its newest part.
const sendLogMaxText = 1 << 20

// controlSender is what SendLog needs of the tunnel's Session.
type controlSender interface {
	SendControl(control.Subtype, []byte) error
	IsConnected() bool
}

// logSession is the running tunnel's Session (route.go), nil when none
// runs or the phone is an exit itself; tests replace it.
var logSession = func() controlSender {
	route.mu.Lock()
	s := route.session
	route.mu.Unlock()
	if s == nil || s.IsExit() {
		return nil
	}
	return s
}

var logAcks struct {
	sync.Mutex
	waiting map[uint32]chan control.ClientLogAck
}

// SendLog sends text, the app's log, to the exit and waits for its answer,
// at most a minute. Call it off the main thread. It returns the answer as
// JSON: {"status":"ok","bytes":N}; {"status":"too_soon","retry_after":S}
// (seconds); "too_big", "off" (the exit keeps no logs), "bad", "error"; or,
// from this side, "not_connected" (no Session tunnel up) and "no_answer"
// (the exit did not answer: one that predates client logs ignores them).
func SendLog(text string) string {
	s := logSession()
	if s == nil || !s.IsConnected() {
		return logAnswer(control.ClientLogAck{Status: "not_connected"})
	}
	data, err := packLog(text)
	if err != nil {
		return logAnswer(control.ClientLogAck{Status: control.ClientLogTooBig})
	}
	id := rand.Uint32() | 1 // never 0: an exit takes 0 for "none yet"
	parts, err := control.SplitClientLog(id, data)
	if err != nil {
		return logAnswer(control.ClientLogAck{Status: control.ClientLogTooBig})
	}
	ch := make(chan control.ClientLogAck, 1)
	logAcks.Lock()
	if logAcks.waiting == nil {
		logAcks.waiting = map[uint32]chan control.ClientLogAck{}
	}
	logAcks.waiting[id] = ch
	logAcks.Unlock()
	defer func() {
		logAcks.Lock()
		delete(logAcks.waiting, id)
		logAcks.Unlock()
	}()
	for _, p := range parts {
		if err := s.SendControl(control.SubtypeClientLog, p.Encode()); err != nil {
			appendLog("[LOG] sending the log: " + err.Error())
			return logAnswer(control.ClientLogAck{Status: "not_connected"})
		}
	}
	select {
	case a := <-ch:
		return logAnswer(a)
	case <-time.After(sendLogWait):
		return logAnswer(control.ClientLogAck{Status: "no_answer"})
	}
}

// packLog compresses the newest sendLogMaxText of text, cutting more off
// its start while it is over what one log may take on the wire.
func packLog(text string) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	limit := sendLogMaxText
	for {
		if len(text) > limit {
			text = text[len(text)-limit:]
			if i := strings.IndexByte(text, '\n'); i >= 0 && i < len(text)-1 {
				text = text[i+1:] // from a whole line
			}
		}
		data := enc.EncodeAll([]byte(text), nil)
		if len(data) <= control.ClientLogMaxParts*control.ClientLogPartBytes {
			return data, nil
		}
		limit = len(text) / 2
	}
}

func logAnswer(a control.ClientLogAck) string {
	a.ID = 0
	b, _ := json.Marshal(a)
	return string(b)
}

// receiveClientLogAck hands the exit's answer to the SendLog waiting on it.
func receiveClientLogAck(payload []byte) {
	a, err := control.DecodeClientLogAck(payload)
	if err != nil {
		return
	}
	logAcks.Lock()
	ch := logAcks.waiting[a.ID]
	logAcks.Unlock()
	if ch != nil {
		select {
		case ch <- a:
		default:
		}
	}
}

// receiveExitControl is the manager's control callback on the client side:
// what the exit tells the app beyond the transports.
func receiveExitControl(sub control.Subtype, payload []byte) {
	switch sub {
	case control.SubtypeConfigOffer:
		receiveConfigOffer(sub, payload)
	case control.SubtypeClientLogAck:
		receiveClientLogAck(payload)
	}
}
