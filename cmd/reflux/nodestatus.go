package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
)

// ipcSocket is where a node's core serves its IPC bridge (IPCSocket in
// node.conf): inside the node's state directory, which the host sees too.
const ipcSocket = "/state/ipc.sock"

// statusRetry is about how long to wait before asking a node again.
var statusRetry = 400 * time.Millisecond

// errStatusCut: connected, but no status came — another reader took the
// socket, or the node is busy.
var errStatusCut = errors.New("no status on the connection")

func (s Store) ipcPath(name string) string {
	return filepath.Join(s.stateDir(name), filepath.Base(ipcSocket))
}

// readNodeStatus asks a running node how its channel is doing. The core
// sends a status frame every second to whoever holds its IPC socket; the
// bridge serves one peer at a time, and nothing else uses it on an exit.
func readNodeStatus(path string, timeout time.Duration) (ipc.StatusPayload, error) {
	var st ipc.StatusPayload
	// A socket path is limited to 108 bytes; a long REFLUX_HOME goes
	// through a descriptor of the directory instead.
	if len(path) > 100 {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return st, err
		}
		defer dir.Close()
		path = fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(path))
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return st, err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		typ, payload, err := ipc.ReadFrame(conn)
		if err != nil {
			return st, fmt.Errorf("%w: %w", errStatusCut, err)
		}
		if typ != ipc.MsgStatus {
			continue // a captcha report or a log line
		}
		if err := ipc.DecodeJSON(payload, &st); err != nil {
			return st, err
		}
		return st, nil
	}
}

// nodeStatuses reads the status of every running node, in parallel.
// A node without an answer (stopped, starting, or running an image that
// predates the socket) is missing from the result.
func nodeStatuses(s Store, clients []Client) map[string]ipc.StatusPayload {
	type result struct {
		name string
		st   ipc.StatusPayload
		err  error
	}
	ch := make(chan result, len(clients))
	for _, c := range clients {
		go func(name string) {
			st, err := readNodeStatus(s.ipcPath(name), 3*time.Second)
			if errors.Is(err, errStatusCut) {
				// The core serves one IPC client at a time, and a new one
				// cuts the last: the panel and the bot reading at once (both
				// restart together on a deploy) lose a reading. Once more,
				// a moment later.
				time.Sleep(statusRetry + time.Duration(rand.Int64N(int64(statusRetry))))
				st, err = readNodeStatus(s.ipcPath(name), 3*time.Second)
			}
			ch <- result{name, st, err}
		}(c.Name)
	}
	out := map[string]ipc.StatusPayload{}
	var online []string
	for range clients {
		r := <-ch
		if r.err == nil {
			out[r.name] = r.st
			if r.st.Connected {
				online = append(online, r.name)
			}
		}
	}
	s.markSeen(online, time.Now())
	return out
}

// humanBytes formats a byte count for a table: 0 B, 812 KB, 1.4 GB.
func humanBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v < 10 {
		return fmt.Sprintf("%.1f %cB", v, "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.0f %cB", v, "KMGTPE"[exp])
}

// durationPhrase says an uptime coarsely: 45s, 12m, 5h, 3d.
func durationPhrase(d time.Duration) phrase {
	switch {
	case d < time.Minute:
		return ph("dur.s", int(d.Seconds()))
	case d < time.Hour:
		return ph("dur.m", int(d.Minutes()))
	case d < 48*time.Hour:
		return ph("dur.h", int(d.Hours()))
	}
	return ph("dur.d", int(d.Hours()/24))
}

// durationIn is durationPhrase in l.
func durationIn(l lang, d time.Duration) string {
	p := durationPhrase(d)
	return tr(l, p.ID, p.Args...)
}
