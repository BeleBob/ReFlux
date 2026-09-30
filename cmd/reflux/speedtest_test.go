package main

import (
	"io"
	"strings"
	"testing"
)

// A test service that refuses (429) is replaced by the next source.
func TestSpeedTestFallsBackToTheNextSource(t *testing.T) {
	fakeDocker(t, "")
	var tried []string
	runDocker = func(stdout io.Writer, args ...string) error {
		cmd := strings.Join(args, " ")
		switch {
		case strings.Contains(cmd, "speed.cloudflare.com"):
			tried = append(tried, "cloudflare")
			io.WriteString(stdout, "0\n") // 429: nothing arrives
		case strings.Contains(cmd, "proof.ovh.net"):
			tried = append(tried, "ovh")
			io.WriteString(stdout, "50000000\n")
		case strings.Contains(cmd, "speedtest.selectel.ru"):
			tried = append(tried, "selectel")
			io.WriteString(stdout, "50000000\n")
		}
		return nil
	}
	rs := speedTest()
	if strings.Join(tried, ",") != "selectel,cloudflare,ovh" {
		t.Errorf("tried %v", tried)
	}
	if len(rs) != 2 || rs[0].Err != nil || rs[1].Err != nil || rs[1].Source != "proof.ovh.net" || rs[1].Bytes != 50_000_000 {
		t.Errorf("results %+v", rs)
	}
	if l := speedLines(langEN, rs); !strings.Contains(l[1], "from proof.ovh.net") {
		t.Errorf("line %q", l[1])
	}
}
