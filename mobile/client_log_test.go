package mobile

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// logExit gathers a log's parts like an exit and answers with ack, or not
// at all when silent.
type logExit struct {
	mu     sync.Mutex
	parts  map[uint16][]byte
	got    string
	silent bool
	ack    control.ClientLogAck
}

func (e *logExit) IsConnected() bool { return true }

func (e *logExit) SendControl(sub control.Subtype, b []byte) error {
	p, err := control.DecodeClientLogPart(b)
	if sub != control.SubtypeClientLog || err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.parts == nil {
		e.parts = map[uint16][]byte{}
	}
	e.parts[p.Part] = append([]byte(nil), p.Data...)
	if len(e.parts) < int(p.Parts) {
		return nil
	}
	var data []byte
	for i := uint16(0); i < p.Parts; i++ {
		data = append(data, e.parts[i]...)
	}
	dec, _ := zstd.NewReader(nil)
	text, err := dec.DecodeAll(data, nil)
	if err != nil {
		return err
	}
	e.got = string(text)
	if !e.silent {
		a := e.ack
		a.ID = p.ID
		b, _ := a.Encode()
		go receiveExitControl(control.SubtypeClientLogAck, b)
	}
	return nil
}

func useLogExit(t *testing.T, e *logExit) {
	t.Helper()
	old := logSession
	logSession = func() controlSender {
		if e == nil {
			return nil
		}
		return e
	}
	t.Cleanup(func() { logSession = old })
}

func TestSendLogGetsTheExitsAnswer(t *testing.T) {
	e := &logExit{ack: control.ClientLogAck{Status: control.ClientLogOK, Bytes: 11}}
	useLogExit(t, e)
	if got := SendLog("line 1\nline 2\n"); got != `{"id":0,"status":"ok","bytes":11}` {
		t.Errorf("answer %s", got)
	}
	if e.got != "line 1\nline 2\n" {
		t.Errorf("the exit got %q", e.got)
	}
	e.parts, e.ack = nil, control.ClientLogAck{Status: control.ClientLogTooSoon, RetryAfter: 90}
	if got := SendLog("again\n"); !strings.Contains(got, `"status":"too_soon","retry_after":90`) {
		t.Errorf("answer %s", got)
	}
}

// Only the newest megabyte goes, from a whole line.
func TestSendLogSendsTheNewestPart(t *testing.T) {
	e := &logExit{ack: control.ClientLogAck{Status: control.ClientLogOK}}
	useLogExit(t, e)
	var b bytes.Buffer
	for i := 0; b.Len() < 3*sendLogMaxText; i++ {
		b.WriteString(strings.Repeat("x", i%97) + " line\n")
	}
	b.WriteString("the last line\n")
	SendLog(b.String())
	if len(e.got) > sendLogMaxText || !strings.HasSuffix(e.got, "the last line\n") || !strings.HasSuffix(strings.SplitN(e.got, "\n", 2)[0], " line") {
		t.Errorf("sent %d bytes starting %q", len(e.got), e.got[:40])
	}
}

func TestSendLogWithoutAnAnswer(t *testing.T) {
	old := sendLogWait
	sendLogWait = 50 * time.Millisecond
	defer func() { sendLogWait = old }()
	useLogExit(t, &logExit{silent: true})
	if got := SendLog("x\n"); !strings.Contains(got, `"no_answer"`) {
		t.Errorf("answer %s", got)
	}
	useLogExit(t, nil)
	if got := SendLog("x\n"); !strings.Contains(got, `"not_connected"`) {
		t.Errorf("answer without a tunnel %s", got)
	}
}
