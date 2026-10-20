package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// logSink is a sink over a temporary directory with a clock the test moves;
// acks collects what it answered.
func logSink(t *testing.T, dir string) (c *clientLogSink, acks *[]control.ClientLogAck, now *time.Time) {
	t.Helper()
	var got []control.ClientLogAck
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c = newClientLogSink(dir, func(sub control.Subtype, b []byte) error {
		if sub != control.SubtypeClientLogAck {
			t.Errorf("answered with subtype 0x%02x", sub)
		}
		a, err := control.DecodeClientLogAck(b)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
		return nil
	})
	c.now = func() time.Time { return clock }
	return c, &got, &clock
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
func sendLog(t *testing.T, c *clientLogSink, acks *[]control.ClientLogAck, id uint32, data []byte) control.ClientLogAck {
	t.Helper()
	parts, err := control.SplitClientLog(id, data)
	if err != nil {
		t.Fatal(err)
	}
	before := len(*acks)
	for _, p := range parts {
		c.handle(control.SubtypeClientLog, p.Encode())
	}
	if len(*acks) != before+1 {
		t.Fatalf("log %d: %d answers, want 1", id, len(*acks)-before)
	}
	return (*acks)[len(*acks)-1]
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
	c, acks, _ := logSink(t, dir)
	text := strings.Repeat("17:00:12  [STRIPE] per-lane 0: s52/r0 loss0%\n", 2000) + "\x1b[31mred\x07\r\nend\n"
	a := sendLog(t, c, acks, 7, packLog(t, text))
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
	c, acks, now := logSink(t, dir)
	data := packLog(t, "a log\n")
	if a := sendLog(t, c, acks, 1, data); a.Status != control.ClientLogOK {
		t.Fatalf("first log: %+v", a)
	}
	*now = now.Add(30 * time.Second)
	if a := sendLog(t, c, acks, 2, data); a.Status != control.ClientLogTooSoon || a.RetryAfter != 90 {
		t.Fatalf("a log 30 s later: %+v, want too_soon in 90 s", a)
	}
	for id := uint32(3); id <= 21; id++ {
		*now = now.Add(clientLogEvery)
		if a := sendLog(t, c, acks, id, data); a.Status != control.ClientLogOK {
			t.Fatalf("log %d: %+v", id, a)
		}
	}
	if n := len(keptLogs(t, dir)); n != clientLogKept {
		t.Errorf("%d logs kept, want %d", n, clientLogKept)
	}
	*now = now.Add(clientLogEvery)
	c, acks, _ = logSink(t, dir) // an exit restart
	c.now = func() time.Time { return *now }
	a := sendLog(t, c, acks, 22, data)
	if a.Status != control.ClientLogTooSoon {
		t.Fatalf("log 21 of the day: %+v, want too_soon", a)
	}
	// The first log came at 12:00:00, this one 40 min 30 s later.
	if want := 24*60*60 - (40*60 + 30); a.RetryAfter != want {
		t.Errorf("retry after %d s, want about %d (the first log of the day plus 24 h)", a.RetryAfter, want)
	}
	*now = now.Add(time.Duration(a.RetryAfter) * time.Second)
	if a := sendLog(t, c, acks, 23, data); a.Status != control.ClientLogOK {
		t.Fatalf("a day after the first log: %+v", a)
	}
}

func TestClientLogRefusals(t *testing.T) {
	c, acks, _ := logSink(t, t.TempDir())
	if a := sendLog(t, c, acks, 1, packLog(t, strings.Repeat("x", clientLogMaxText+1))); a.Status != control.ClientLogTooBig {
		t.Errorf("over 1 MiB of text: %+v", a)
	}
	if a := sendLog(t, c, acks, 2, []byte("not zstd at all")); a.Status != control.ClientLogBad {
		t.Errorf("garbage: %+v", a)
	}
	off, offAcks, _ := logSink(t, "")
	if a := sendLog(t, off, offAcks, 3, packLog(t, "x")); a.Status != control.ClientLogOff {
		t.Errorf("an exit without ClientLogDir: %+v", a)
	}
}

// Parts that do not belong are ignored or end the log; other subtypes and
// parts that do not parse get no answer at all.
func TestClientLogStrayParts(t *testing.T) {
	dir := t.TempDir()
	c, acks, _ := logSink(t, dir)
	c.handle(control.SubtypeConfigOffer, []byte(`{}`))
	c.handle(control.SubtypeClientLog, []byte{1, 2, 3})
	c.handle(control.SubtypeClientLog, control.ClientLogPart{ID: 9, Part: 3, Parts: 2, Data: []byte{1}}.Encode())
	if len(*acks) != 0 {
		t.Fatalf("answered %+v", *acks)
	}
	// A log that never completes is replaced by the next one.
	big, _ := control.SplitClientLog(5, make([]byte, 3*control.ClientLogPartBytes))
	c.handle(control.SubtypeClientLog, big[0].Encode())
	if a := sendLog(t, c, acks, 6, packLog(t, "the next log\n")); a.ID != 6 || a.Status != control.ClientLogOK {
		t.Fatalf("the next log: %+v", a)
	}
	// Late parts of an answered log are ignored.
	late, _ := control.SplitClientLog(6, packLog(t, "the next log\n"))
	c.handle(control.SubtypeClientLog, late[0].Encode())
	if len(*acks) != 1 {
		t.Errorf("answered a late part: %+v", *acks)
	}
	// Parts that disagree on the count end the log.
	two, _ := control.SplitClientLog(8, make([]byte, 2*control.ClientLogPartBytes))
	c.now = func() time.Time { return time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC) }
	c.handle(control.SubtypeClientLog, two[0].Encode())
	c.handle(control.SubtypeClientLog, control.ClientLogPart{ID: 8, Part: 0, Parts: 3, Data: []byte{1}}.Encode())
	if a := (*acks)[len(*acks)-1]; a.ID != 8 || a.Status != control.ClientLogBad {
		t.Errorf("a part with another count: %+v", a)
	}
	if n := len(keptLogs(t, dir)); n != 1 {
		t.Errorf("%d logs kept, want 1", n)
	}
}
