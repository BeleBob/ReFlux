package server

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/subscription"
)

// channelInfo is a node channel as node-install.sh left it on disk.
type channelInfo struct {
	key        string
	port       int
	context    string
	transports []provision.ChannelTransport
}

var knownTransports = map[string]bool{"vyandex": true, "yandex": true, "mailru": true, "boards": true, "cupsonline": true}

// readChannel reads <dir>/<channel>/{node.conf,encryption-key}.
func readChannel(dir, channel string) (*channelInfo, error) {
	base := filepath.Join(dir, channel)
	keyBytes, err := os.ReadFile(filepath.Join(base, "encryption-key"))
	if err != nil {
		return nil, fmt.Errorf("channel %s: %w", channel, err)
	}
	f, err := os.Open(filepath.Join(base, "node.conf"))
	if err != nil {
		return nil, fmt.Errorf("channel %s: %w", channel, err)
	}
	defer f.Close()

	info := &channelInfo{key: strings.TrimSpace(string(keyBytes))}
	var section, typ, url, listen string
	flush := func() {
		switch {
		case section == "":
		case typ == "direct":
			if _, p, err := net.SplitHostPort(listen); err == nil {
				info.port, _ = strconv.Atoi(p)
			}
		case knownTransports[typ] && url != "":
			info.transports = append(info.transports, provision.ChannelTransport{Type: typ, URL: url})
		}
		typ, url, listen = "", "", ""
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "["):
			flush()
			section = strings.Trim(line, "[]")
		case strings.HasPrefix(line, "#") || line == "":
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch {
			case section == "" && k == "URL":
				info.context = v
			case k == "Type":
				typ = v
			case k == "URL":
				url = v
			case k == "Listen":
				listen = v
			}
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(info.key) != 64 || info.port == 0 || len(info.transports) == 0 {
		return nil, fmt.Errorf("channel %s: no key, direct port or documents in its config", channel)
	}
	return info, nil
}

// linkCache builds the openflux:// link of each pool, and rebuilds it when the
// channel's files change, so a transport changed in the app, or a rotated key,
// reaches subscribers on their next refresh.
type linkCache struct {
	mu      sync.Mutex
	dir     string
	entries map[string]cached
}

type cached struct {
	conf, key time.Time
	link      string
}

func newLinkCache(dir string) *linkCache { return &linkCache{dir: dir, entries: map[string]cached{}} }

func (c *linkCache) link(p Pool, channel string) (string, error) {
	base := filepath.Join(c.dir, channel)
	ci, err := os.Stat(filepath.Join(base, "node.conf"))
	if err != nil {
		return "", err
	}
	ki, err := os.Stat(filepath.Join(base, "encryption-key"))
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[channel]; ok && e.conf.Equal(ci.ModTime()) && e.key.Equal(ki.ModTime()) {
		return e.link, nil
	}
	info, err := readChannel(c.dir, channel)
	if err != nil {
		return "", err
	}
	link, err := provision.ShareLinkContext(p.Name, info.key, p.Host, info.port, info.transports, info.context)
	if err != nil {
		return "", err
	}
	c.entries[channel] = cached{conf: ci.ModTime(), key: ki.ModTime(), link: link}
	return link, nil
}

// profiles are the locations a plan or key may use. A pool whose files cannot
// be read is left out: the others still work.
func (s *Server) profiles(poolIDs []string) []subscription.Profile {
	want := map[string]bool{}
	for _, id := range poolIDs {
		want[id] = true
	}
	// out starts empty, never nil: the JSON of a document with no usable pool
	// must be "profiles":[], which the apps read, and not "profiles":null.
	out := []subscription.Profile{}
	for _, p := range s.cfg.Pools {
		if len(want) > 0 && !want[p.ID] {
			continue
		}
		link, err := s.links.link(p, p.Channel)
		if err != nil {
			s.logf("pool %s: %v", p.ID, err)
			if p.AutoNode {
				s.start(nodeRequest{Pool: p, Channel: p.Channel})
			}
			continue
		}
		out = append(out, subscription.Profile{ID: p.ID, Name: p.Name, Link: link})
	}
	return out
}
