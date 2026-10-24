package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// An exit given --client-log-dir (ClientLogDir in a .conf) keeps the logs
// its Session client sends (control.SubtypeClientLog) for whoever runs the
// exit (ReFlux). Who sent one is never in question: an exit serves the one
// client that holds its key, and nothing from the log names the file. What
// a client may send is bounded here, where the client cannot change it:
// one log every clientLogEvery, clientLogPerDay a day, clientLogMaxText of
// text, clientLogKept logs kept. The limits are counted from the files
// themselves, so a restart of the exit does not reset them. A log is kept
// as plain text, its control characters dropped. An exit without the
// option answers every log with "off".

var (
	// clientLogGap: a log with holes this long after its last part is
	// asked for the parts that are missing, up to clientLogAsks times. A
	// control message goes out once and nothing retransmits it, so one
	// lost part would otherwise leave the log unfinished, unanswered, and
	// the person looking at "the server did not answer".
	clientLogGap      = 3 * time.Second
	clientLogAsks     = 5
	clientLogEvery    = 2 * time.Minute
	clientLogPerDay   = 20
	clientLogKept     = 10
	clientLogMaxText  = 1 << 20
	clientLogAssembly = 2 * time.Minute
)

const clientLogStamp = "20060102T150405Z"

type clientLogSink struct {
	dir   string
	send  func(control.Subtype, []byte) error
	now   func() time.Time
	sleep func(time.Duration)

	mu      sync.Mutex
	cur     *clientLogParts
	settled uint32 // the last log answered: its late parts are ignored
	waiting bool   // an askLoop is running
}

type clientLogParts struct {
	id      uint32
	parts   [][]byte
	got     int
	started time.Time
	last    time.Time // when the newest part came
	asked   int       // resends asked for so far
}

func newClientLogSink(dir string, send func(control.Subtype, []byte) error) *clientLogSink {
	return &clientLogSink{dir: dir, send: send, now: time.Now, sleep: time.Sleep}
}

// handle is the exit's control callback: it takes client log parts and
// ignores everything else.
func (c *clientLogSink) handle(sub control.Subtype, payload []byte) {
	if sub != control.SubtypeClientLog {
		return
	}
	p, err := control.DecodeClientLogPart(payload)
	if err != nil {
		utils.Debugf("[CLIENT-LOG] %v", err)
		return
	}
	if ack, done := c.take(p); done {
		b, _ := ack.Encode()
		if err := c.send(control.SubtypeClientLogAck, b); err != nil {
			utils.Debugf("[CLIENT-LOG] answer: %v", err)
		}
	}
}

// take files one part; done says the log is answered with ack.
func (c *clientLogSink) take(p control.ClientLogPart) (ack control.ClientLogAck, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	ack.ID = p.ID
	if c.cur != nil && (c.cur.id != p.ID || now.Sub(c.cur.started) > clientLogAssembly) {
		c.cur = nil // a log that never completed: a newer one replaces it
	}
	if c.cur == nil {
		if p.ID == c.settled {
			return ack, false
		}
		c.settled = p.ID
		if c.dir == "" {
			ack.Status = control.ClientLogOff
			return ack, true
		}
		if wait := c.waitLocked(now); wait > 0 {
			ack.Status, ack.RetryAfter = control.ClientLogTooSoon, int((wait+time.Second-1)/time.Second)
			return ack, true
		}
		c.cur = &clientLogParts{id: p.ID, parts: make([][]byte, p.Parts), started: now}
	}
	if int(p.Parts) != len(c.cur.parts) {
		c.cur = nil
		ack.Status = control.ClientLogBad
		return ack, true
	}
	if c.cur.parts[p.Part] == nil {
		c.cur.parts[p.Part] = append([]byte(nil), p.Data...)
		c.cur.got++
	}
	c.cur.last = now
	if c.cur.got < len(c.cur.parts) {
		c.waitForRestLocked(p.ID)
		return ack, false
	}
	data := bytes.Join(c.cur.parts, nil)
	c.cur = nil
	text, err := unpackClientLog(data)
	switch {
	case errors.Is(err, errClientLogTooBig):
		ack.Status = control.ClientLogTooBig
	case err != nil:
		ack.Status = control.ClientLogBad
	default:
		n, err := c.saveLocked(text, now)
		if err != nil {
			utils.Infof("[CLIENT-LOG] keeping a log: %v", err)
			ack.Status = control.ClientLogError
			break
		}
		utils.Infof("[CLIENT-LOG] the client sent its log (%d bytes)", n)
		ack.Status, ack.Bytes = control.ClientLogOK, n
	}
	return ack, true
}

// waitForRestLocked arranges for the missing parts to be asked for once
// the log has been quiet for clientLogGap. Caller holds c.mu.
func (c *clientLogSink) waitForRestLocked(id uint32) {
	if c.waiting {
		return
	}
	c.waiting = true
	go c.askLoop(id)
}

// askLoop asks for the parts of log id that have not arrived, while it
// stays the log being assembled.
func (c *clientLogSink) askLoop(id uint32) {
	defer func() {
		c.mu.Lock()
		c.waiting = false
		c.mu.Unlock()
	}()
	for {
		c.sleep(clientLogGap)
		c.mu.Lock()
		cur := c.cur
		if cur == nil || cur.id != id {
			c.mu.Unlock()
			return // finished, replaced, or given up on
		}
		if c.now().Sub(cur.last) < clientLogGap {
			c.mu.Unlock()
			continue // parts are still coming
		}
		if cur.asked >= clientLogAsks {
			utils.Infof("[CLIENT-LOG] the client's log is %d part(s) short: giving up", len(cur.parts)-cur.got)
			c.cur = nil
			c.mu.Unlock()
			return
		}
		cur.asked++
		ack := control.ClientLogAck{ID: id, Status: control.ClientLogResend, Missing: missingParts(cur)}
		c.mu.Unlock()
		b, err := ack.Encode()
		if err != nil {
			return
		}
		utils.Debugf("[CLIENT-LOG] asking for %d missing part(s)", len(ack.Missing))
		if err := c.send(control.SubtypeClientLogAck, b); err != nil {
			utils.Debugf("[CLIENT-LOG] asking: %v", err)
		}
	}
}

// missingParts numbers the parts that have not arrived.
func missingParts(cur *clientLogParts) []uint16 {
	var out []uint16
	for i, p := range cur.parts {
		if p == nil {
			out = append(out, uint16(i))
		}
	}
	return out
}

var errClientLogTooBig = errors.New("client log: too big")

// unpackClientLog decompresses a log, at most clientLogMaxText of it, and
// keeps it plain text.
func unpackClientLog(data []byte) (string, error) {
	dec, err := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderMaxMemory(uint64(4*clientLogMaxText)))
	if err != nil {
		return "", err
	}
	defer dec.Close()
	raw, err := io.ReadAll(io.LimitReader(dec, int64(clientLogMaxText)+1))
	if err != nil {
		return "", err
	}
	if len(raw) > clientLogMaxText {
		return "", errClientLogTooBig
	}
	return cleanClientLog(raw), nil
}

// cleanClientLog keeps printable text, newlines and tabs: no control
// characters or terminal escapes reach whoever reads the file.
func cleanClientLog(raw []byte) string {
	s := strings.ToValidUTF8(string(raw), "�")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && !(r >= 0x80 && r < 0xa0)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// waitLocked is how long until the exit takes a log again (0: now).
func (c *clientLogSink) waitLocked(now time.Time) time.Duration {
	kept := c.keptLocked()
	var wait time.Duration
	if len(kept) > 0 {
		if w := kept[len(kept)-1].Add(clientLogEvery).Sub(now); w > wait {
			wait = w
		}
	}
	var today []time.Time
	for _, t := range kept {
		if now.Sub(t) < 24*time.Hour {
			today = append(today, t)
		}
	}
	if len(today) >= clientLogPerDay {
		if w := today[len(today)-clientLogPerDay].Add(24 * time.Hour).Sub(now); w > wait {
			wait = w
		}
	}
	return wait
}

// keptLocked lists when the kept logs came, oldest first. It counts the
// logs pruned since, too: their times stay in clientLogTimes.
func (c *clientLogSink) keptLocked() []time.Time {
	var out []time.Time
	for _, name := range c.namesLocked(clientLogTimes) {
		if t, ok := clientLogTime(name, clientLogTimes); ok {
			out = append(out, t)
		}
	}
	for _, name := range c.namesLocked(clientLogPrefix) {
		if t, ok := clientLogTime(name, clientLogPrefix); ok {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// Kept logs are client-log-<stamp>.txt; a pruned one leaves an empty
// sent-<stamp> behind for a day, so the daily limit still counts it.
const (
	clientLogPrefix = "client-log-"
	clientLogTimes  = "sent-"
)

func (c *clientLogSink) namesLocked(prefix string) []string {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func clientLogTime(name, prefix string) (time.Time, bool) {
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".txt")
	t, err := time.Parse(clientLogStamp, stamp)
	return t, err == nil
}

// saveLocked writes a log and prunes old ones; it returns the bytes kept.
func (c *clientLogSink) saveLocked(text string, now time.Time) (int, error) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return 0, err
	}
	stamp := now.UTC().Format(clientLogStamp)
	body := fmt.Sprintf("# ReFlux client log, received %s, %d bytes\n%s", now.UTC().Format(time.RFC3339), len(text), text)
	tmp, err := os.CreateTemp(c.dir, ".log-*.tmp")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // after the rename, a no-op
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(c.dir, clientLogPrefix+stamp+".txt")); err != nil {
		return 0, err
	}
	logs := c.namesLocked(clientLogPrefix)
	for len(logs) > clientLogKept {
		if t, ok := clientLogTime(logs[0], clientLogPrefix); ok && now.Sub(t) < 24*time.Hour {
			_ = os.WriteFile(filepath.Join(c.dir, clientLogTimes+t.UTC().Format(clientLogStamp)), nil, 0o600)
		}
		if err := os.Remove(filepath.Join(c.dir, logs[0])); err != nil {
			return 0, err
		}
		logs = logs[1:]
	}
	for _, name := range c.namesLocked(clientLogTimes) {
		if t, ok := clientLogTime(name, clientLogTimes); ok && now.Sub(t) >= 24*time.Hour {
			_ = os.Remove(filepath.Join(c.dir, name))
		}
	}
	return len(text), nil
}
