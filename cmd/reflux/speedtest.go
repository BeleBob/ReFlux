package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// speedTargets are downloaded through the egress, as the clients' traffic
// goes: a Russian file (in the RU prefixes) and a world one. Each has
// sources to try in order: the test services answer 429 Too Many Requests
// to an address that downloaded a lot, as Cloudflare did after a day of
// tests through one world server.
var speedTargets = []struct {
	key  string
	urls []string
}{
	{"russia", []string{"https://speedtest.selectel.ru/100MB", "https://mirror.yandex.ru/debian/ls-lR.gz"}},
	{"world", []string{
		"https://speed.cloudflare.com/__down?bytes=50000000", // it refuses 100 MB
		"https://proof.ovh.net/files/100Mb.dat",
	}},
}

// speedBytes is how much of each file is read.
const speedBytes = 50_000_000

type speedResult struct {
	Key    string
	Source string // the host the file came from
	Bytes  int64
	Took   time.Duration
	Err    error
}

// Mbps is the download rate in megabits per second.
func (r speedResult) Mbps() float64 {
	if r.Took <= 0 {
		return 0
	}
	return float64(r.Bytes) * 8 / r.Took.Seconds() / 1e6
}

// speedTest downloads each target in the egress container, one after the
// other. The time includes starting docker exec, a few tens of
// milliseconds against seconds of download.
func speedTest() []speedResult {
	var out []speedResult
	for _, t := range speedTargets {
		var r speedResult
		for _, u := range t.urls {
			if r = download(t.key, u); r.Err == nil {
				break
			}
		}
		out = append(out, r)
	}
	return out
}

// download reads up to speedBytes of u in the egress container.
func download(key, u string) speedResult {
	r := speedResult{Key: key, Source: host(u)}
	var b strings.Builder
	start := time.Now()
	err := quiet(&b, "exec", "reflux-egress", "sh", "-c",
		fmt.Sprintf("wget -q -T 15 -O - '%s' 2>/dev/null | head -c %d | wc -c", u, speedBytes))
	r.Took, r.Err = time.Since(start), err
	if err == nil {
		r.Bytes, r.Err = strconv.ParseInt(strings.TrimSpace(b.String()), 10, 64)
	}
	if r.Err == nil && r.Bytes < 1_000_000 {
		r.Err = fmt.Errorf("only %d bytes arrived from %s", r.Bytes, r.Source)
	}
	return r
}

func host(u string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	return h
}

// speedLines renders the results in l, with the way each target took.
func speedLines(l lang, rs []speedResult) []string {
	st, stErr := readEgressStatus()
	var lines []string
	for _, r := range rs {
		way := ""
		if stErr == nil {
			switch r.Key {
			case "world":
				way = st.World
			case "russia":
				way = tr(l, ruMode(st).ID)
			}
		}
		if r.Err != nil {
			lines = append(lines, tr(l, "speed.fail", ph("speed."+r.Key), way, r.Err))
			continue
		}
		lines = append(lines, tr(l, "speed.ok", ph("speed."+r.Key), way, r.Mbps(), float64(r.Bytes)/1e6, r.Took.Seconds(), r.Source))
	}
	return lines
}

func cmdSpeedtest(stdout io.Writer) error {
	fmt.Fprintln(stdout, "Downloading through the egress, as the clients' traffic goes...")
	for _, l := range speedLines(langEN, speedTest()) {
		fmt.Fprintln(stdout, l)
	}
	return nil
}
