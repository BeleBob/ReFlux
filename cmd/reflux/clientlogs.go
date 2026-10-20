package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A client's app can send its log to its node (control.SubtypeClientLog):
// the node keeps it in ClientLogDir, which is in its state directory, so it
// lands in state/<name>/client-logs on the host. The node decides what it
// takes (one log every 2 minutes, 20 a day, 1 MiB, the last 10 kept; see
// client_log.go in the core). The logs are for whoever helps the owner
// with the server: nothing sends them anywhere, backups leave them out.

// clientLogNodeDir is ClientLogDir inside a node: in its state directory.
const clientLogNodeDir = "/state/client-logs"

func (s Store) clientLogDir(name string) string {
	return filepath.Join(s.stateDir(name), "client-logs")
}

// clientLogFiles lists a client's logs, oldest first.
func (s Store) clientLogFiles(name string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(s.clientLogDir(name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	var out []os.DirEntry
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), "client-log-") && strings.HasSuffix(e.Name(), ".txt") {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, err
}

// cmdClientLogs: reflux client-logs [<name> [last | <file>]].
func cmdClientLogs(s Store, args []string, stdout io.Writer) error {
	switch len(args) {
	case 0:
		clients, err := s.List()
		if err != nil {
			return err
		}
		any := false
		for _, c := range clients {
			files, err := s.clientLogFiles(c.Name)
			if err != nil || len(files) == 0 {
				continue
			}
			any = true
			fmt.Fprintf(stdout, "%-16s %2d log(s), the newest %s\n", c.Name, len(files), logStampText(files[len(files)-1].Name()))
		}
		if !any {
			fmt.Fprintln(stdout, "No client has sent a log.")
		}
		return nil
	case 1, 2:
	default:
		return fmt.Errorf("usage: reflux client-logs [<name> [last | <file>]]")
	}
	name := args[0]
	if _, err := s.Get(name); err != nil {
		return err
	}
	files, err := s.clientLogFiles(name)
	if err != nil {
		return err
	}
	if len(args) == 1 {
		if len(files) == 0 {
			fmt.Fprintf(stdout, "%s has sent no log.\n", name)
			return nil
		}
		for _, f := range files {
			var size uint64
			if fi, err := f.Info(); err == nil {
				size = uint64(fi.Size())
			}
			fmt.Fprintf(stdout, "%s  %s  %s\n", f.Name(), logStampText(f.Name()), humanBytes(size))
		}
		return nil
	}
	if len(files) == 0 {
		return fmt.Errorf("%s has sent no log", name)
	}
	want := files[len(files)-1].Name()
	if args[1] != "last" {
		want = ""
		for _, f := range files {
			if f.Name() == args[1] {
				want = f.Name() // only a name from the list: no paths
			}
		}
		if want == "" {
			return fmt.Errorf("%s has no log %q: reflux client-logs %s lists them", name, args[1], name)
		}
	}
	f, err := os.Open(filepath.Join(s.clientLogDir(name), want))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(stdout, f)
	return err
}

// logStampText reads the time out of client-log-<stamp>.txt.
func logStampText(file string) string {
	stamp := strings.TrimSuffix(strings.TrimPrefix(file, "client-log-"), ".txt")
	t, err := time.Parse("20060102T150405Z", stamp)
	if err != nil {
		return "?"
	}
	return t.Local().Format(time.DateTime)
}
