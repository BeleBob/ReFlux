package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backups of the data directory: a tar.gz a day (a systemd user timer),
// the newest kept. They stay on this server: neither the bot nor the
// panel hands them out, they only report how old the last one is.
// A restore puts one back and recreates the containers.

const (
	backupKeep    = 14
	backupMaxAge  = 50 * time.Hour // a daily backup missed twice
	backupLayout  = "20060102-150405"
	backupPrefix  = "reflux-"
	backupSuffix  = ".tar.gz"
	backupTimerAt = "*-*-* 04:30:00"
)

// backupConfig is backup.json in the data directory.
type backupConfig struct {
	Dir  string `json:"dir"`
	Keep int    `json:"keep"`
}

func (s Store) backupConfigPath() string { return filepath.Join(s.Root, "backup.json") }

// defaultBackupDir is next to the data directory: ~/reflux-backups.
func (s Store) defaultBackupDir() string {
	return filepath.Join(filepath.Dir(s.Root), "reflux-backups")
}

// loadBackupConfig reads backup.json; installed is false without it.
func (s Store) loadBackupConfig() (c backupConfig, installed bool, err error) {
	c = backupConfig{Dir: s.defaultBackupDir(), Keep: backupKeep}
	b, err := os.ReadFile(s.backupConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, false, fmt.Errorf("%s: %w", s.backupConfigPath(), err)
	}
	if c.Keep < 1 {
		c.Keep = backupKeep
	}
	return c, true, nil
}

// backupSkip: what a backup leaves out, and a restore leaves in place —
// locks, sockets, logs, the panel's history and its sign-ins (an old
// backup must not bring back a session that was ended).
func backupSkip(rel string, mode fs.FileMode) bool {
	base := path.Base(rel)
	switch {
	case !mode.IsRegular() && !mode.IsDir():
		return true
	case strings.HasSuffix(base, ".lock"), strings.HasSuffix(base, ".tmp"), strings.HasPrefix(base, ".restore-"):
		return true
	}
	switch rel {
	case "heal.log", "metrics.json", "web-login.json", "web-sessions.json":
		return true
	}
	return false
}

// makeBackup writes the data directory to a new archive in dir.
func (s Store) makeBackup(dir string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".reflux-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // after the rename, a no-op
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(s.Root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.Root, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := e.Info()
		if err != nil {
			return err
		}
		if backupSkip(rel, info.Mode()) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if e.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	// A name of its own: two backups in a second (one before a restore)
	// must not overwrite each other.
	var dst string
	for i := 0; ; i++ {
		dst = filepath.Join(dir, backupPrefix+now.Add(time.Duration(i)*time.Second).Format(backupLayout)+backupSuffix)
		if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
			break
		}
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

// backupFile is a backup on disk.
type backupFile struct {
	Path string
	At   time.Time
	Size int64
}

// listBackups returns the backups in dir, the newest first.
func listBackups(dir string) ([]backupFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []backupFile
	for _, e := range entries {
		stamp, ok := strings.CutPrefix(e.Name(), backupPrefix)
		if stamp, ok = strings.CutSuffix(stamp, backupSuffix); !ok || !e.Type().IsRegular() {
			continue
		}
		at, err := time.ParseInLocation(backupLayout, stamp, time.Local)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, backupFile{Path: filepath.Join(dir, e.Name()), At: at, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// pruneBackups removes all but the keep newest backups.
func pruneBackups(dir string, keep int) error {
	list, err := listBackups(dir)
	if err != nil || len(list) <= keep {
		return err
	}
	for _, b := range list[keep:] {
		if err := os.Remove(b.Path); err != nil {
			return err
		}
	}
	return nil
}

// extractBackup unpacks an archive into dir: directories and regular
// files only, every path inside dir.
func extractBackup(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: not a ReFlux backup: %w", archive, err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		name := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("%s: unsafe path %q", archive, hdr.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		perm := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, perm|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: %q is not a file or a directory", archive, hdr.Name)
		}
	}
}

// restoreBackup replaces the data directory's contents with an archive's.
// What backups leave out (locks, logs, the panel's history and sign-ins)
// stays; the directory itself stays too, so the lock held by this command
// goes on holding.
func (s Store) restoreBackup(archive string) error {
	tmp, err := os.MkdirTemp(s.Root, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extractBackup(archive, tmp); err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Join(tmp, "clients")); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s: not a ReFlux backup (no clients directory)", archive)
	}
	old, err := os.ReadDir(s.Root)
	if err != nil {
		return err
	}
	for _, e := range old {
		info, err := e.Info()
		if err != nil {
			return err
		}
		if backupSkip(e.Name(), info.Mode()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Root, e.Name())); err != nil {
			return err
		}
	}
	restored, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range restored {
		if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(s.Root, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// ---- commands ----

func cmdBackup(s Store, args []string, stdout io.Writer) error {
	cfg, installed, err := s.loadBackupConfig()
	if err != nil {
		return err
	}
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "":
		p, err := s.makeBackup(cfg.Dir, time.Now())
		if err != nil {
			return err
		}
		if err := pruneBackups(cfg.Dir, cfg.Keep); err != nil {
			return err
		}
		list, _ := listBackups(cfg.Dir)
		var size int64
		if len(list) > 0 {
			size = list[0].Size
		}
		fmt.Fprintf(stdout, "Backup: %s (%s); %d kept in %s.\n", p, humanBytes(uint64(size)), len(list), cfg.Dir)
		if !installed {
			fmt.Fprintln(stdout, "A backup a day: reflux backup install")
		}
		return nil
	case "list":
		list, err := listBackups(cfg.Dir)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintf(stdout, "No backups in %s yet: reflux backup\n", cfg.Dir)
			return nil
		}
		for _, b := range list {
			fmt.Fprintf(stdout, "%s  %8s  %s\n", b.At.Format(time.DateTime), humanBytes(uint64(b.Size)), filepath.Base(b.Path))
		}
		fmt.Fprintf(stdout, "%d in %s; the newest %d are kept.\n", len(list), cfg.Dir, cfg.Keep)
		return nil
	case "install":
		fs := newFlagSet("backup install")
		dir := fs.String("dir", cfg.Dir, "where the backups go (on this server)")
		keep := fs.Int("keep", cfg.Keep, "how many backups to keep")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("backup install: unexpected %q", fs.Arg(0))
		}
		return backupInstall(s, *dir, *keep, stdout)
	}
	return fmt.Errorf("backup: unknown command %q (backup, backup list, backup install)", sub)
}

const backupServiceTemplate = `# Generated by reflux.
[Unit]
Description=ReFlux backup of the data directory (stays on this server)

[Service]
Type=oneshot
ExecStart=%s backup
Environment=REFLUX_HOME=%s
`

const backupTimerTemplate = `# Generated by reflux.
[Unit]
Description=A ReFlux backup a day

[Timer]
OnCalendar=%s
Persistent=true

[Install]
WantedBy=timers.target
`

// backupInstall saves the settings and starts the daily timer.
func backupInstall(s Store, dir string, keep int, stdout io.Writer) error {
	if keep < 1 {
		return errors.New("backup install: --keep must be at least 1")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(s.Root, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("backup install: %s is inside the data directory; backups would back up backups", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSON(s.backupConfigPath(), backupConfig{Dir: dir, Keep: keep}); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	units := filepath.Join(cfg, "systemd", "user")
	if err := os.MkdirAll(units, 0o755); err != nil {
		return err
	}
	for name, body := range map[string]string{
		"reflux-backup.service": fmt.Sprintf(backupServiceTemplate, exe, s.Root),
		"reflux-backup.timer":   fmt.Sprintf(backupTimerTemplate, backupTimerAt),
	} {
		if err := os.WriteFile(filepath.Join(units, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	for _, a := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", "reflux-backup.timer"},
	} {
		if err := runCmd(stdout, "systemctl", a...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(a, " "), err)
		}
	}
	fmt.Fprintf(stdout, "A backup a day at 04:30 into %s, the newest %d kept. They stay on this server.\n", dir, keep)
	return nil
}

// cmdRestore puts a backup back and recreates the containers.
func cmdRestore(s Store, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := newFlagSet("restore")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	name, err := parseArgs(fs, args)
	if err != nil {
		return errors.New("restore: name one backup (see reflux backup list)")
	}
	cfg, _, err := s.loadBackupConfig()
	if err != nil {
		return err
	}
	archive := name
	if !strings.ContainsRune(name, os.PathSeparator) {
		archive = filepath.Join(cfg.Dir, name)
	}
	if _, err := os.Stat(archive); err != nil {
		return err
	}
	if !*yes {
		fmt.Fprintf(stdout, "Restore %s? Clients, keys and settings become what they were then; egress and every node restart. Type yes: ", archive)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(answer) != "yes" {
			return errors.New("not confirmed; nothing changed")
		}
	}
	// The current state first, so the restore can be undone.
	before, err := s.makeBackup(cfg.Dir, time.Now())
	if err != nil {
		return fmt.Errorf("backing up the current state failed, nothing restored: %w", err)
	}
	fmt.Fprintf(stdout, "The current state is in %s.\n", before)
	if err := s.restoreBackup(archive); err != nil {
		return fmt.Errorf("restore: %w; the state before it is in %s", err, before)
	}
	fmt.Fprintf(stdout, "Restored %s; recreating egress and the nodes.\n", filepath.Base(archive))
	if err := cmdRestart(s, stdout); err != nil {
		return fmt.Errorf("restored, but the containers did not restart: %w (then: reflux restart)", err)
	}
	fmt.Fprintln(stdout, "If the bot's or the panel's settings changed: reflux bot install; reflux web install")
	return nil
}

// backup checks that backups are made: the newest is at most two days old.
func (d *doctor) backup(s Store) {
	cfg, installed, err := s.loadBackupConfig()
	switch {
	case err != nil:
		d.warn("backup", "backup.bad", err)
		return
	case !installed:
		d.warn("backup", "backup.off")
		return
	}
	list, err := listBackups(cfg.Dir)
	switch {
	case err != nil:
		d.warn("backup", "backup.bad", err)
	case len(list) == 0:
		d.warn("backup", "backup.none", cfg.Dir)
	case time.Since(list[0].At) > backupMaxAge:
		d.warn("backup", "backup.old", durationPhrase(time.Since(list[0].At)))
	default:
		d.ok("backup", "backup.ok", durationPhrase(time.Since(list[0].At)), len(list), cfg.Dir)
	}
}
