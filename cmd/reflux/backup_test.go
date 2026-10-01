package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// archiveNames lists the entries of a backup.
func archiveNames(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

func fakeSystemctl(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	old := runCmd
	runCmd = func(stdout io.Writer, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { runCmd = old })
	return &calls
}

func readString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBackupAndRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	docker := fakeDocker(t, "")
	systemctl := fakeSystemctl(t)
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: home}
	writeFile(t, filepath.Join(home, "egress", "world-1.conf"), "world\n")
	writeFile(t, filepath.Join(home, "telegram.json"), `{"token":"x"}`)
	writeFile(t, filepath.Join(home, "metrics.json"), "[]")
	writeFile(t, filepath.Join(home, "web-sessions.json"), "{}")
	writeFile(t, filepath.Join(home, "heal.log"), "")
	writeFile(t, filepath.Join(home, ".lock"), "")
	sock, err := net.Listen("unix", s.ipcPath("phone"))
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	key := readString(t, filepath.Join(home, "clients", "phone", "key"))

	dir := filepath.Join(t.TempDir(), "backups")
	if err := run([]string{"backup", "install", "--dir", filepath.Join(home, "b")}, nil, io.Discard); err == nil {
		t.Error("backups inside the data directory accepted")
	}
	var out strings.Builder
	if err := run([]string{"backup", "install", "--dir", dir, "--keep", "5"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*systemctl, "\n"), "systemctl --user enable --now reflux-backup.timer") {
		t.Errorf("timer not started: %q", *systemctl)
	}
	units := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user")
	if svc := readString(t, filepath.Join(units, "reflux-backup.service")); !strings.Contains(svc, " backup\n") || !strings.Contains(svc, "REFLUX_HOME="+home) {
		t.Errorf("service unit:\n%s", svc)
	}
	if timer := readString(t, filepath.Join(units, "reflux-backup.timer")); !strings.Contains(timer, "OnCalendar=*-*-* 04:30:00") || !strings.Contains(timer, "Persistent=true") {
		t.Errorf("timer unit:\n%s", timer)
	}

	out.Reset()
	if err := run([]string{"backup"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	list, _ := listBackups(dir)
	if len(list) != 1 {
		t.Fatalf("%d backups:\n%s", len(list), out.String())
	}
	if fi, _ := os.Stat(list[0].Path); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v", fi.Mode())
	}
	names := archiveNames(t, list[0].Path)
	for _, want := range []string{"clients/", "clients/phone/key", "clients/phone/client.json", "egress/world-1.conf", "telegram.json", "backup.json"} {
		if !slices.Contains(names, want) {
			t.Errorf("the backup lacks %s: %q", want, names)
		}
	}
	for _, left := range []string{".lock", "metrics.json", "web-sessions.json", "heal.log", "state/phone/ipc.sock"} {
		if slices.Contains(names, left) {
			t.Errorf("the backup has %s", left)
		}
	}

	// Things change...
	writeFile(t, filepath.Join(home, "clients", "phone", "key"), "another key")
	if err := run([]string{"add", "tablet", "--url", testURL + "2", "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(home, "egress", "world-1.conf"))
	writeFile(t, filepath.Join(home, "metrics.json"), "[1]")

	// ...a restore asks first...
	name := filepath.Base(list[0].Path)
	if err := run([]string{"restore", name}, strings.NewReader("no\n"), io.Discard); err == nil {
		t.Error("restored without a yes")
	}
	if readString(t, filepath.Join(home, "clients", "phone", "key")) != "another key" {
		t.Fatal("a refused restore changed the data")
	}
	// ...and puts it back.
	*docker = nil
	out.Reset()
	if err := run([]string{"restore", name}, strings.NewReader("yes\n"), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := readString(t, filepath.Join(home, "clients", "phone", "key")); got != key {
		t.Errorf("key %q, want the backed up one", got)
	}
	if _, err := os.Stat(filepath.Join(home, "clients", "tablet")); err == nil {
		t.Error("the client added after the backup survived the restore")
	}
	if readString(t, filepath.Join(home, "egress", "world-1.conf")) != "world\n" {
		t.Error("egress config not restored")
	}
	if readString(t, filepath.Join(home, "metrics.json")) != "[1]" {
		t.Error("the panel's history was replaced")
	}
	if _, err := os.Stat(filepath.Join(home, ".lock")); err != nil {
		t.Error("the lock file went")
	}
	if ents, _ := filepath.Glob(filepath.Join(home, ".restore-*")); len(ents) != 0 {
		t.Errorf("left behind: %q", ents)
	}
	if !strings.Contains(strings.Join(*docker, "\n"), "up --detach --remove-orphans --force-recreate") {
		t.Errorf("containers not recreated: %q", *docker)
	}
	// The state before the restore is a backup of its own.
	list, _ = listBackups(dir)
	if len(list) != 2 || !slices.Contains(archiveNames(t, list[0].Path), "clients/tablet/key") {
		t.Fatalf("no backup of the state before the restore: %+v", list)
	}
	if !strings.Contains(out.String(), "The current state is in "+list[0].Path) {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestBackupsRotate(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 1, 4, 30, 0, 0, time.Local)
	for i := 0; i < 20; i++ {
		writeFile(t, filepath.Join(dir, backupPrefix+base.AddDate(0, 0, i).Format(backupLayout)+backupSuffix), "x")
	}
	writeFile(t, filepath.Join(dir, "notes.txt"), "mine")
	if err := pruneBackups(dir, 14); err != nil {
		t.Fatal(err)
	}
	list, _ := listBackups(dir)
	if len(list) != 14 || !list[0].At.Equal(base.AddDate(0, 0, 19)) || !list[13].At.Equal(base.AddDate(0, 0, 6)) {
		t.Errorf("kept %d: %+v", len(list), list)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Error("pruning removed a file that is not a backup")
	}
}

// writeArchive makes a tar.gz of the given entries.
func writeArchive(t *testing.T, p string, entries []tar.Header) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = 1
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
}

func TestRestoreRefusesForeignArchives(t *testing.T) {
	home := t.TempDir()
	s := Store{Root: home}
	writeFile(t, filepath.Join(home, "clients", "phone", "key"), "k")
	dir := t.TempDir()
	for name, entries := range map[string][]tar.Header{
		"escape":   {{Name: "clients/", Typeflag: tar.TypeDir, Mode: 0o700}, {Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o600}},
		"absolute": {{Name: "/tmp/evil", Typeflag: tar.TypeReg, Mode: 0o600}},
		"link":     {{Name: "clients/", Typeflag: tar.TypeDir, Mode: 0o700}, {Name: "clients/x", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		"other":    {{Name: "photos/", Typeflag: tar.TypeDir, Mode: 0o700}, {Name: "photos/a.jpg", Typeflag: tar.TypeReg, Mode: 0o600}},
	} {
		p := filepath.Join(dir, name+".tar.gz")
		writeArchive(t, p, entries)
		if err := s.restoreBackup(p); err == nil {
			t.Errorf("%s: restored", name)
		}
		if readString(t, filepath.Join(home, "clients", "phone", "key")) != "k" {
			t.Fatalf("%s: the data changed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(home), "evil")); err == nil {
		t.Error("a path escaped the data directory")
	}
	writeFile(t, filepath.Join(dir, "plain.tar.gz"), "not gzip")
	if err := s.restoreBackup(filepath.Join(dir, "plain.tar.gz")); err == nil {
		t.Error("not an archive: restored")
	}
}

func TestDoctorBackupCheck(t *testing.T) {
	s := Store{Root: t.TempDir()}
	dir := t.TempDir()
	check := func() finding {
		d := &doctor{}
		d.backup(s)
		return d.findings[0]
	}
	if f := check(); f.Level != levelWarn || f.Msg != "backup.off" {
		t.Errorf("not installed: %+v", f)
	}
	writeJSON(s.backupConfigPath(), backupConfig{Dir: dir, Keep: 14})
	if f := check(); f.Level != levelWarn || f.Msg != "backup.none" {
		t.Errorf("no backups: %+v", f)
	}
	old := backupPrefix + time.Now().Add(-3*24*time.Hour).Format(backupLayout) + backupSuffix
	writeFile(t, filepath.Join(dir, old), "x")
	if f := check(); f.Level != levelWarn || f.Msg != "backup.old" || !strings.Contains(f.text(langEN), "3d") {
		t.Errorf("old backup: %+v %q", f, f.text(langEN))
	}
	writeFile(t, filepath.Join(dir, backupPrefix+time.Now().Add(-time.Hour).Format(backupLayout)+backupSuffix), "x")
	f := check()
	if f.Level != levelOK || f.Sig != check().Sig {
		t.Errorf("fresh backup: %+v", f)
	}
	if got := shortText(s, langRU, f); got != "Резервная копия: 1 ч назад, хранится 2" {
		t.Errorf("short text %q", got)
	}
}
