package main

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/host"
)

// The host's figures that come from commands (df, docker) rather than
// /proc and /sys: runCmd and quiet are replaced in tests.

// disk is a mounted filesystem.
type disk struct {
	Mount       string
	Used, Total uint64
}

func (d disk) Percent() float64 {
	if d.Total == 0 {
		return 0
	}
	return 100 * float64(d.Used) / float64(d.Total)
}

// readDisks lists the real filesystems (df, without memory and container
// ones).
func readDisks() []disk {
	var b strings.Builder
	runCmd(&b, "df", "-P", "-k", "-x", "tmpfs", "-x", "devtmpfs", "-x", "overlay", "-x", "squashfs", "-x", "efivarfs", "-x", "nsfs")
	var out []disk
	seen := map[string]bool{}
	for _, l := range strings.Split(b.String(), "\n")[1:] {
		f := strings.Fields(l)
		if len(f) < 6 || seen[f[5]] {
			continue
		}
		total, err1 := strconv.ParseUint(f[1], 10, 64)
		used, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil || total == 0 || strings.HasPrefix(f[5], "/boot") {
			continue
		}
		seen[f[5]] = true
		out = append(out, disk{Mount: strings.Join(f[5:], " "), Used: used * 1024, Total: total * 1024})
	}
	return out
}

// containerUse is a reflux container's CPU time and memory.
type containerUse struct {
	Name     string
	CPUUsec  uint64
	MemBytes uint64
}

// containerIDs maps the running reflux containers to their full ids.
func containerIDs() map[string]string {
	var b strings.Builder
	quiet(&b, "ps", "--no-trunc", "--filter", "name=^reflux-", "--format", "{{.Names}} {{.ID}}")
	out := map[string]string{}
	for _, l := range strings.Split(b.String(), "\n") {
		if name, id, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
			out[name] = id
		}
	}
	return out
}

// readContainer reads a container's cgroup (v2, systemd driver).
func readContainer(name, id string) (containerUse, error) {
	dir := filepath.Join(host.SysRoot, "fs/cgroup/system.slice", "docker-"+id+".scope")
	c := containerUse{Name: name}
	lines, err := host.ReadLines(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return c, err
	}
	for _, l := range lines {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "usage_usec" {
			c.CPUUsec, _ = strconv.ParseUint(f[1], 10, 64)
		}
	}
	c.MemBytes, _ = host.ReadUint(filepath.Join(dir, "memory.current"))
	return c, nil
}
