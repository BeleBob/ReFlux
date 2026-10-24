package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// testSink is a sink over a temporary directory with a clock the test
// moves and the answers it gave. The sink answers from its own goroutine
// (askLoop), so everything here is behind a lock.
type testSink struct {
	*clientLogSink
	mu    sync.Mutex
	clock time.Time
	acks  []control.ClientLogAck
}

func logSink(t *testing.T, dir string) *testSink {
	t.Helper()
	ts := &testSink{clock: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	ts.clientLogSink = newClientLogSink(dir, func(sub control.Subtype, b []byte) error {
		if sub != control.SubtypeClientLogAck {
			t.Errorf("answered with subtype 0x%02x", sub)
		}
		a, err := control.DecodeClientLogAck(b)
		if err != nil {
			t.Error(err)
			return err
		}
		ts.mu.Lock()
		ts.acks = append(ts.acks, a)
		ts.mu.Unlock()
		return nil
	})
	ts.now = ts.at
	return ts
}

func (ts *testSink) at() time.Time {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.clock
}

func (ts *testSink) advance(d time.Duration) {
	ts.mu.Lock()
	ts.clock = ts.clock.Add(d)
	ts.mu.Unlock()
}

// moveClockOnSleep makes the sink's own waits move the test's clock, so
// askLoop runs at the test's speed.
func (ts *testSink) moveClockOnSleep() {
	ts.sleep = func(d time.Duration) {
		ts.advance(d)
		time.Sleep(time.Millisecond)
	}
}

// idle: no log is being put together any more.
func (ts *testSink) idle() bool {
	ts.clientLogSink.mu.Lock()
	defer ts.clientLogSink.mu.Unlock()
	return ts.cur == nil
}

func (ts *testSink) answers() []control.ClientLogAck {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]control.ClientLogAck(nil), ts.acks...)
}

func (ts *testSink) last() control.ClientLogAck {
	a := ts.answers()
	if len(a) == 0 {
		return control.ClientLogAck{}
	}
	return a[len(a)-1]
}

func packLog(t *testing.T, text string) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeAll([]byte(text), nil)
}

// sendLog hands the sink a log's parts and returns its answer.
func sendLog(t *testing.T, ts *testSink, id uint32, data []byte) control.ClientLogAck {
	t.Helper()
	parts, err := control.SplitClientLog(id, data)
	if err != nil {
		t.Fatal(err)
	}
	before := len(ts.answers())
	for _, p := range parts {
		ts.handle(control.SubtypeClientLog, p.Encode())
	}
	got := ts.answers()
	if len(got) != before+1 {
		t.Fatalf("log %d: %d answers, want 1", id, len(got)-before)
	}
	return got[len(got)-1]
}

func keptLogs(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), clientLogPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestClientLogIsKeptAsPlainText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "client-logs")
	ts := logSink(t, dir)
	text := strings.Repeat("17:00:12  [STRIPE] per-lane 0: s52/r0 loss0%\n", 2000) + "\x1b[31mred\x07\r\nend\n"
	a := sendLog(t, ts, 7, packLog(t, text))
	if a.ID != 7 || a.Status != control.ClientLogOK || a.Bytes == 0 {
		t.Fatalf("answer %+v", a)
	}
	b, err := os.ReadFile(filepath.Join(dir, "client-log-20261020T120000Z.txt"))
	if err != nil {
		t.Fatalf("%v; kept %v", err, keptLogs(t, dir))
	}
	got := string(b)
	if !strings.HasPrefix(got, "# ReFlux client log, received 2026-10-20T12:00:00Z") || !strings.HasSuffix(got, "[31mred\nend\n") {
		t.Errorf("kept log starts %q, ends %q", got[:60], got[len(got)-20:])
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v, want 0700", fi.Mode().Perm())
	}
}

// One log every clientLogEvery, clientLogPerDay a day, the last
// clientLogKept kept; logs pruned from the directory still count for the
// day, and a new sink (an exit restart) counts them too.
func TestClientLogLimits(t *testing.T) {
	dir := t.TempDir()
	ts := logSink(t, dir)
	data := packLog(t, "a log\n")
	if a := sendLog(t, ts, 1, data); a.Status != control.ClientLogOK {
		t.Fatalf("first log: %+v", a)
	}
	ts.advance(30 * time.Second)
	if a := sendLog(t, ts, 2, data); a.Status != control.ClientLogTooSoon || a.RetryAfter != 90 {
		t.Fatalf("a log 30 s later: %+v, want too_soon in 90 s", a)
	}
	for id := uint32(3); id <= 21; id++ {
		ts.advance(clientLogEvery)
		if a := sendLog(t, ts, id, data); a.Status != control.ClientLogOK {
			t.Fatalf("log %d: %+v", id, a)
		}
	}
	if n := len(keptLogs(t, dir)); n != clientLogKept {
		t.Errorf("%d logs kept, want %d", n, clientLogKept)
	}
	ts.advance(clientLogEvery)
	restarted := logSink(t, dir) // an exit restart
	restarted.clock = ts.at()
	ts = restarted
	a := sendLog(t, ts, 22, data)
	if a.Status != control.ClientLogTooSoon {
		t.Fatalf("log 21 of the day: %+v, want too_soon", a)
	}
	// The first log came at 12:00:00, this one 40 min 30 s later.
	if want := 24*60*60 - (40*60 + 30); a.RetryAfter != want {
		t.Errorf("retry after %d s, want about %d (the first log of the day plus 24 h)", a.RetryAfter, want)
	}
	ts.advance(time.Duration(a.RetryAfter) * time.Second)
	if a := sendLog(t, ts, 23, data); a.Status != control.ClientLogOK {
		t.Fatalf("a day after the first log: %+v", a)
	}
}

func TestClientLogRefusals(t *testing.T) {
	ts := logSink(t, t.TempDir())
	if a := sendLog(t, ts, 1, packLog(t, strings.Repeat("x", clientLogMaxText+1))); a.Status != control.ClientLogTooBig {
		t.Errorf("over 1 MiB of text: %+v", a)
	}
	if a := sendLog(t, ts, 2, []byte("not zstd at all")); a.Status != control.ClientLogBad {
		t.Errorf("garbage: %+v", a)
	}
	off := logSink(t, "")
	if a := sendLog(t, off, 3, packLog(t, "x")); a.Status != control.ClientLogOff {
		t.Errorf("an exit without ClientLogDir: %+v", a)
	}
}

// Parts that do not belong are ignored or end the log; other subtypes and
// parts that do not parse get no answer at all.
func TestClientLogStrayParts(t *testing.T) {
	dir := t.TempDir()
	ts := logSink(t, dir)
	ts.handle(control.SubtypeConfigOffer, []byte(`{}`))
	ts.handle(control.SubtypeClientLog, []byte{1, 2, 3})
	ts.handle(control.SubtypeClientLog, control.ClientLogPart{ID: 9, Part: 3, Parts: 2, Data: []byte{1}}.Encode())
	if len(ts.answers()) != 0 {
		t.Fatalf("answered %+v", ts.answers())
	}
	// A log that never completes is replaced by the next one.
	big, _ := control.SplitClientLog(5, make([]byte, 3*control.ClientLogPartBytes))
	ts.handle(control.SubtypeClientLog, big[0].Encode())
	if a := sendLog(t, ts, 6, packLog(t, "the next log\n")); a.ID != 6 || a.Status != control.ClientLogOK {
		t.Fatalf("the next log: %+v", a)
	}
	// Late parts of an answered log are ignored.
	late, _ := control.SplitClientLog(6, packLog(t, "the next log\n"))
	ts.handle(control.SubtypeClientLog, late[0].Encode())
	if len(ts.answers()) != 1 {
		t.Errorf("answered a late part: %+v", ts.answers())
	}
	// Parts that disagree on the count end the log.
	two, _ := control.SplitClientLog(8, make([]byte, 2*control.ClientLogPartBytes))
	ts.advance(time.Hour)
	ts.handle(control.SubtypeClientLog, two[0].Encode())
	ts.handle(control.SubtypeClientLog, control.ClientLogPart{ID: 8, Part: 0, Parts: 3, Data: []byte{1}}.Encode())
	if a := ts.last(); a.ID != 8 || a.Status != control.ClientLogBad {
		t.Errorf("a part with another count: %+v", a)
	}
	if n := len(keptLogs(t, dir)); n != 1 {
		t.Errorf("%d logs kept, want 1", n)
	}
}

// A part that never arrives: the exit asks for it instead of leaving the
// log unfinished and the client without an answer. This is what made a
// log sent from the phone fail now and then.
func TestClientLogAsksForMissingParts(t *testing.T) {
	dir := t.TempDir()
	ts := logSink(t, dir)
	// The clock moves by itself: askLoop's wait is the test's step.
	ts.moveClockOnSleep()
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "17:0%d:%02d [STRIPE] per-lane %d: s%d/r%d loss%d%%\n", i%10, i%60, i%4, i*7, i*13, i%37)
	}
	data := packLog(t, b.String())
	parts, err := control.SplitClientLog(11, data)
	if err != nil || len(parts) < 3 {
		t.Fatalf("%d parts, %v", len(parts), err)
	}
	// Everything but the middle part.
	lost := len(parts) / 2
	for i, p := range parts {
		if i != lost {
			ts.handle(control.SubtypeClientLog, p.Encode())
		}
	}
	waitFor(t, "the exit to ask for the lost part", func() bool { return len(ts.answers()) > 0 })
	a := ts.answers()[0]
	if a.ID != 11 || a.Status != control.ClientLogResend || len(a.Missing) != 1 || a.Missing[0] != uint16(lost) {
		t.Fatalf("answer %+v, want a resend of part %d", a, lost)
	}
	ts.handle(control.SubtypeClientLog, parts[lost].Encode())
	// The clock here is the test's, so the exit may ask again in between;
	// what matters is that the resent part completes the log.
	waitFor(t, "the log to be kept", func() bool { return ts.last().Status == control.ClientLogOK })
	if n := len(keptLogs(t, dir)); n != 1 {
		t.Errorf("%d logs kept, want 1", n)
	}
}

// A client that goes away mid-log: the exit asks a few times, gives up,
// and takes the next log.
func TestClientLogGivesUpOnAHalfLog(t *testing.T) {
	dir := t.TempDir()
	ts := logSink(t, dir)
	ts.moveClockOnSleep()
	parts, _ := control.SplitClientLog(12, make([]byte, 3*control.ClientLogPartBytes))
	ts.handle(control.SubtypeClientLog, parts[0].Encode())
	waitFor(t, "the exit to give up", ts.idle)
	if got := ts.answers(); len(got) != clientLogAsks {
		t.Fatalf("%d asks, want %d", len(got), clientLogAsks)
	}
	for _, a := range ts.answers() {
		if a.Status != control.ClientLogResend || len(a.Missing) != 2 {
			t.Fatalf("ask %+v", a)
		}
	}
	if a := sendLog(t, ts, 13, packLog(t, "the next log\n")); a.Status != control.ClientLogOK {
		t.Fatalf("the next log: %+v", a)
	}
}
