// Command reflux-egress runs inside the ReFlux egress container. It gives
// the exit nodes, which share the container's network namespace, exactly
// two ways out: an AmneziaWG tunnel to Russia for Russian addresses and one
// to the rest of the world for everything else, with a kill switch so no
// packet ever leaves from the host's own address. The world tunnel fails
// over through the configs in order; DNS goes to a local validating
// resolver that forwards over TLS through the world tunnel.
package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	configDir  = "/etc/reflux/egress"
	stateDir   = "/var/lib/reflux-egress"
	runDir     = "/run/reflux-egress"
	bakedList  = "/usr/share/reflux-egress/ru.cidr"
	ripeURL    = "https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest"
	refreshAge = 24 * time.Hour
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "run":
		run()
	case "health":
		st, err := readStatus(filepath.Join(runDir, "status.json"))
		if err == nil {
			err = healthy(st, time.Now())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
	case "status":
		st, err := readStatus(filepath.Join(runDir, "status.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "no status yet:", err)
			os.Exit(1)
		}
		fmt.Print(statusText(st, time.Now()))
	case "ru-cidr":
		// Build time: an RIR delegated file on stdin, RU prefixes on stdout.
		ps, err := parseDelegated(os.Stdin, "RU")
		if err == nil && len(ps) < minRUPrefixes {
			err = fmt.Errorf("only %d prefixes", len(ps))
		}
		if err == nil {
			err = writePrefixes(os.Stdout, ps)
		}
		if err != nil {
			log.Fatal(err)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: reflux-egress [run|health|status|ru-cidr]")
		os.Exit(2)
	}
}

// run never exits on its own: the exit nodes live in this container's
// network namespace, and a restarted container would leave them in a dead
// one. Anything that fails is retried; until it works, the kill switch
// keeps everything in.
func run() {
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		log.Fatal(err)
	}
	c := &controller{run: execRunner, runDir: runDir, confDir: configDir, stateDir: stateDir, lookup: resolveLocal}
	stop := make(chan struct{})

	for {
		ru, direct, fallback, world, err := loadConfigs(configDir)
		if err == nil {
			c.ru, c.ruDirect, c.ruFallback, c.world = ru, direct, fallback, world
			russia := ru.Name
			if direct {
				russia = "direct (" + directFile + ")"
			}
			if fallback {
				russia += ", out of the uplink while it does not answer (" + fallbackFile + ")"
			}
			c.carrierHosts = loadCarrierHosts(configDir)
			if len(c.carrierHosts) > 0 {
				log.Printf("configs: carriers out of the uplink (%s): %s", carrierFile, strings.Join(c.carrierHosts, ", "))
			}
			// Start with the world server the owner chose, else the last
			// one that held.
			c.picked = c.selection()
			c.cur = c.firstWorld()
			log.Printf("configs: russia %s, world %d in order (%s first)", russia, len(world), world[c.cur].Name)
			break
		}
		fail(c, "configs", err)
	}
	for {
		err := c.setup()
		if err == nil {
			break
		}
		fail(c, "setup", err)
	}
	russia := c.ru.Name
	if c.ruDirect {
		russia = "the uplink (direct)"
	}
	log.Printf("up: russia via %s, world via %s", russia, c.world[c.cur].Name)

	loadPrefixes(c)
	// Only now: a resolver started before the tunnels finds its upstreams
	// unreachable and would not try them again for a while.
	go superviseResolver(stop)
	go refreshPrefixes(c, stop)
	go c.watch(stop)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	close(stop)
	log.Print("stopping")
}

// fail records err in the status and waits before the next attempt.
func fail(c *controller, what string, err error) {
	log.Printf("%s: %v; retrying in 30s", what, err)
	c.mu.Lock()
	c.st.Error = what + ": " + err.Error()
	c.st.Updated = time.Now().UTC()
	st := c.st
	c.mu.Unlock()
	writeStatus(filepath.Join(runDir, "status.json"), st)
	time.Sleep(30 * time.Second)
}

// loadPrefixes applies the cached RU list, else the one built into the
// image. Russian traffic is only routed to Russia once a list is applied.
func loadPrefixes(c *controller) {
	for _, path := range []string{filepath.Join(stateDir, "ru.cidr"), bakedList} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		ps, err := readPrefixes(f)
		st, _ := f.Stat()
		f.Close()
		if err == nil {
			err = c.setPrefixes(ps, st.ModTime().UTC())
		}
		if err == nil {
			return
		}
		log.Printf("ru: %s: %v", path, err)
	}
	log.Print("ru: no usable list yet; Russian addresses go through the world tunnel until one is fetched")
}

// refreshPrefixes fetches the RU list from RIPE through the tunnels once
// the cached one is a day old, and keeps it current.
func refreshPrefixes(c *controller, stop <-chan struct{}) {
	wait := time.Minute
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		c.mu.Lock()
		age := time.Since(c.st.RUListAt)
		c.mu.Unlock()
		if age < refreshAge {
			wait = refreshAge - age
			continue
		}
		if err := fetchPrefixes(c); err != nil {
			log.Printf("ru: refresh: %v", err)
			wait = time.Hour
			continue
		}
		wait = refreshAge
	}
}

func fetchPrefixes(c *controller) error {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(ripeURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", ripeURL, resp.Status)
	}
	ps, err := parseDelegated(io.LimitReader(resp.Body, 64<<20), "RU")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := c.setPrefixes(ps, now); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := writePrefixes(&buf, ps); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(stateDir, "ru.cidr.tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, "ru.cidr"))
}

// superviseResolver keeps unbound running on 127.0.0.1:53.
func superviseResolver(stop <-chan struct{}) {
	conf := filepath.Join(runDir, "unbound.conf")
	if err := os.WriteFile(conf, []byte(unboundConf), 0o644); err != nil {
		log.Printf("dns: %v", err)
		return
	}
	for {
		cmd := exec.Command("unbound", "-d", "-p", "-c", conf)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err := cmd.Start()
		if err == nil {
			log.Print("dns: unbound started")
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-stop:
				cmd.Process.Signal(syscall.SIGTERM)
				<-done
				return
			case err = <-done:
			}
		}
		log.Printf("dns: unbound exited: %v; restarting in 5s", err)
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// unboundConf: a validating resolver for the namespace, forwarding over
// TLS to Quad9 (blocks known malicious domains) and Cloudflare. Their
// addresses are outside the RU prefixes, so queries leave through the world
// tunnel, encrypted. serve-expired answers from the cache while a tunnel
// fails over, so an open carrier document keeps resolving; the infra
// settings bring the upstreams back quickly once it has.
const unboundConf = `server:
	interface: 127.0.0.1
	port: 53
	# Queries reach it through the DNS redirect with their original source:
	# the tunnel address, not 127.0.0.1. A phone's VPN sends its queries to
	# 1.1.1.1:53 and got REFUSED with 127.0.0.0/8 only. It listens on
	# 127.0.0.1, so only processes in this namespace can reach it anyway.
	access-control: 0.0.0.0/0 allow
	do-ip6: no
	username: ""
	chroot: ""
	directory: "/run/reflux-egress"
	pidfile: ""
	use-syslog: no
	logfile: ""
	verbosity: 0
	tls-cert-bundle: /etc/ssl/certs/ca-certificates.crt
	trust-anchor-file: /usr/share/dnssec-root/trusted-key.key
	qname-minimisation: yes
	harden-glue: yes
	harden-dnssec-stripped: yes
	prefetch: yes
	serve-expired: yes
	serve-expired-ttl: 86400
	cache-min-ttl: 60
	# An upstream that timed out (a tunnel failing over) is tried again
	# within a minute instead of being left out for 15.
	infra-host-ttl: 60
	infra-keep-probing: yes
	# The carriers' zones are unsigned; proving that needs the parent's DS
	# records through the world tunnel. Skipping it keeps them resolving
	# while that tunnel is down.
	domain-insecure: "mail.ru"
	domain-insecure: "datacloudmail.ru"
	domain-insecure: "yandex.ru"
	domain-insecure: "yandex.net"
	hide-identity: yes
	hide-version: yes

# The carriers' own domains resolve by the Russian route (plain DNS to
# Yandex, inside the Russian tunnel unless Russia is direct; its
# DNS-over-TLS drops the handshake), so a carrier
# document keeps working while the world tunnel is down. Nothing is
# disclosed: the carrier connection itself goes there anyway.
forward-zone:
	name: "mail.ru"
	forward-addr: 77.88.8.8
	forward-addr: 77.88.8.1
forward-zone:
	name: "datacloudmail.ru"
	forward-addr: 77.88.8.8
	forward-addr: 77.88.8.1
forward-zone:
	name: "yandex.ru"
	forward-addr: 77.88.8.8
	forward-addr: 77.88.8.1
forward-zone:
	name: "yandex.net"
	forward-addr: 77.88.8.8
	forward-addr: 77.88.8.1

forward-zone:
	name: "."
	forward-tls-upstream: yes
	forward-addr: 9.9.9.9@853#dns.quad9.net
	forward-addr: 149.112.112.112@853#dns.quad9.net
	forward-addr: 1.1.1.1@853#cloudflare-dns.com
	forward-addr: 1.0.0.1@853#cloudflare-dns.com
`
