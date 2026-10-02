package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	old, oldTag := releaseVersion, imageTag
	t.Cleanup(func() { releaseVersion, imageTag = old, oldTag })
	releaseVersion, imageTag = "", "main"
	if v := version(); v != commit() {
		t.Errorf("a main build is its commit: %q", v)
	}
	if o := options(); !strings.HasSuffix(o.NodeImage, ":main") || !strings.HasSuffix(o.EgressImage, ":main") {
		t.Errorf("main build images %+v", o)
	}
	releaseVersion, imageTag = "1.2.3", "latest"
	if v := version(); v != "v1.2.3" {
		t.Errorf("release version %q", v)
	}
	if o := options(); o.NodeImage != "ghcr.io/belebob/reflux-node:latest" || o.EgressImage != "ghcr.io/belebob/reflux-egress:latest" {
		t.Errorf("release images %+v", o)
	}
	t.Setenv("REFLUX_NODE_IMAGE", "example/node:x")
	var out strings.Builder
	if err := run([]string{"version"}, nil, &out); err != nil || !strings.HasPrefix(out.String(), "reflux v1.2.3 (commit ") ||
		!strings.Contains(out.String(), "node image   example/node:x") {
		t.Errorf("reflux version: %v\n%s", err, out.String())
	}
}

// The changelog the release notes come from: "Не выпущено" first, then
// versions newest first, each with a date and some text.
func TestChangelog(t *testing.T) {
	b, err := os.ReadFile("../../docs/reflux/CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	heading := regexp.MustCompile(`^## \[([^\]]+)\](?: - (\d{4}-\d{2}-\d{2}))?$`)
	semver := regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
	var versions []string
	body := map[string]int{}
	cur := ""
	for _, line := range strings.Split(string(b), "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			cur = m[1]
			if cur != "Не выпущено" {
				if !semver.MatchString(cur) || m[2] == "" {
					t.Errorf("bad release heading %q", line)
				}
				versions = append(versions, cur)
			} else if len(versions) > 0 {
				t.Error("«Не выпущено» is not the first section")
			}
			continue
		}
		if cur != "" && strings.TrimSpace(line) != "" {
			body[cur]++
		}
	}
	if len(versions) == 0 {
		t.Fatal("no releases in the changelog")
	}
	num := func(v string) [3]int {
		m := semver.FindStringSubmatch(v)
		var n [3]int
		for i := range n {
			for _, c := range m[i+1] {
				n[i] = n[i]*10 + int(c-'0')
			}
		}
		return n
	}
	for i, v := range versions {
		if body[v] == 0 {
			t.Errorf("release %s has no notes", v)
		}
		if i > 0 {
			a, p := num(v), num(versions[i-1])
			if !(a[0] < p[0] || a[0] == p[0] && (a[1] < p[1] || a[1] == p[1] && a[2] < p[2])) {
				t.Errorf("%s is listed after %s: newest first", v, versions[i-1])
			}
		}
	}
}
