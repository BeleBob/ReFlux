package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Docker drops a container's log with the container, and reflux recreates
// containers often (apply, update, restart, heal): the log of a node that
// misbehaved was gone by the time anyone looked. So before reflux recreates
// or removes containers it copies their logs into logs/, one file per run
// of a container (named by when it started, so a second apply refreshes the
// file instead of pushing an older run out), keeping the last few runs.
// Backups leave them out.

// keptContainerLogs is how many runs of each container keep their log.
const keptContainerLogs = 3

// containerLogTail bounds one saved log: a node at -ddd logs every packet.
const containerLogTail = "20000"

// logStamp names a run in its log file: when the container started.
const logStamp = "20060102T150405Z"

// dockerLogs copies a container's log, which the core writes to stderr;
// tests replace it.
var dockerLogs = func(name string, w io.Writer) error {
	cmd := exec.Command("docker", "logs", "--tail", containerLogTail, name)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

func (s Store) logsDir() string { return filepath.Join(s.Root, "logs") }

// saveAllLogs keeps the logs of egress and every client's node.
func saveAllLogs(s Store, stdout io.Writer) {
	clients, err := s.List()
	if err != nil {
		return
	}
	names := []string{"reflux-egress"}
	for _, c := range clients {
		names = append(names, "reflux-node-"+c.Name)
	}
	saveLogs(s, names, stdout)
}

// saveLogs keeps the logs of the running containers among names. It only
// warns: a log that could not be kept must not stop a restart.
func saveLogs(s Store, names []string, stdout io.Writer) {
	started := containerStarts(names)
	for _, name := range names {
		t, ok := started[name]
		if !ok {
			continue
		}
		if err := s.saveLog(name, t); err != nil {
			fmt.Fprintf(stdout, "warning: keeping the log of %s: %v\n", name, err)
		}
	}
}

// saveLog writes the log of a container's run that started at started.
func (s Store) saveLog(name string, started time.Time) error {
	dir := s.logsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".log-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // after the rename, a no-op
	if err := dockerLogs(name, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	dst := filepath.Join(dir, name+"-"+started.UTC().Format(logStamp)+".log")
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	return s.pruneLogs(name)
}

// pruneLogs removes all but the newest keptContainerLogs runs' logs of a
// container.
func (s Store) pruneLogs(name string) error {
	runs, err := s.savedLogs(name)
	if err != nil {
		return err
	}
	for len(runs) > keptContainerLogs {
		if err := os.Remove(runs[0]); err != nil {
			return err
		}
		runs = runs[1:]
	}
	return nil
}

// savedLogs lists a container's saved logs, oldest first. A name is matched
// whole: the logs of node "a" are not those of node "a-b".
func (s Store) savedLogs(name string) ([]string, error) {
	entries, err := os.ReadDir(s.logsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		stamp, ok := strings.CutPrefix(e.Name(), name+"-")
		if !ok {
			continue
		}
		stamp, ok = strings.CutSuffix(stamp, ".log")
		if _, err := time.Parse(logStamp, stamp); !ok || err != nil {
			continue
		}
		out = append(out, filepath.Join(s.logsDir(), e.Name()))
	}
	sort.Strings(out)
	return out, nil
}
