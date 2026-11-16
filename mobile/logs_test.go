package mobile

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The pending log keeps the newest keptLogs lines, and a line costs no
// copy of the whole list: packet logs come hundreds a second, under the
// lock the device's packets take.
func TestAppendLogKeepsTheNewest(t *testing.T) {
	ReadLogs()
	for i := 0; i < 2500; i++ {
		appendLog(fmt.Sprintf("line %d", i))
	}
	lines := strings.Split(ReadLogs(), "\n")
	if len(lines) != keptLogs || lines[0] != fmt.Sprintf("line %d", 2500-keptLogs) || lines[len(lines)-1] != "line 2499" {
		t.Fatalf("%d lines, %q ... %q", len(lines), lines[0], lines[len(lines)-1])
	}
	if ReadLogs() != "" {
		t.Error("lines left after reading")
	}
	// Past keptLogs: the old list copied itself on every line here.
	for i := 0; i < keptLogs+10; i++ {
		appendLog("x")
	}
	allocs := testing.AllocsPerRun(400, func() { appendLog("x") })
	if allocs > 0.5 {
		t.Errorf("%.2f allocations a line: the list is copied on every line", allocs)
	}
	ReadLogs()
}

// ReadTimedLogs gives each line the time it was written, not the time the
// app read it: the app read the lines of a stop hours later, at the next
// start, and showed the stop then.
func TestReadTimedLogsKeepsTheWriteTime(t *testing.T) {
	ReadLogs()
	appendLog("[ANDROID] first")
	appendLog("second\n  0000 45 00")
	written := time.Now()
	time.Sleep(300 * time.Millisecond)
	records := strings.Split(ReadTimedLogs(), LogRecordSeparator)
	if len(records) != 2 {
		t.Fatalf("%d records: %q", len(records), records)
	}
	for i, want := range []string{"[ANDROID] first", "second\n  0000 45 00"} {
		ms, text, ok := strings.Cut(records[i], "\t")
		if !ok || text != want {
			t.Fatalf("record %d = %q, want text %q", i, records[i], want)
		}
		at, err := strconv.ParseInt(ms, 10, 64)
		if err != nil {
			t.Fatalf("record %d time %q: %v", i, ms, err)
		}
		if d := written.UnixMilli() - at; d < 0 || d > 150 {
			t.Errorf("record %d stamped %d ms before it was written: want the write time, not the read time", i, d)
		}
	}
	if ReadTimedLogs() != "" {
		t.Error("lines left after reading")
	}
}
