// scripttest is a manual, live-network smoke test for one script
// transport: it loads a signed .js file exactly as the real factory does,
// starts it against a real URL, and prints every state/event transition
// plus received packets until the deadline. It exists because verifying a
// new script port (auth flow, wire framing, reconnect) against production
// infrastructure is a repeatable need - not just for mailru, for every
// transport this package ever gets.
//
//	scripttest -script transport/script/js/mailru.js -pubkey <hex> \
//	  -url "Vuri/d5nuZ5aQp" -duration 45s -send "hello"
//
// -cookies-file lets a captcha solved out-of-band (phone/WebView) unblock a
// yandex-family transport's happy path, exactly like the real app relaying
// a solved captcha's cookies back via ApplyCookies.
//
// -burst/-burst-size measure real submission throughput: pick a count well
// above the write queue's 1024-slot buffer (transport.DefaultConfig's
// MaxQueueSize), or the measurement is just "how fast can I fill a buffer",
// not the transport's actual sustained rate.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/script"
)

func main() {
	scriptPath := flag.String("script", "", "Path to the .js file (its <path>.sig must verify)")
	pubkeyHex := flag.String("pubkey", "", "Hex ed25519 public key the .sig must verify against")
	url := flag.String("url", "", "URL/weblink handed to the script's open() as cfg.url")
	paramsRaw := flag.String("param", "", "Extra cfg.params entries as key=value, comma-separated")
	duration := flag.Duration("duration", 30*time.Second, "How long to watch before stopping")
	sendText := flag.String("send", "", "If set, Send() this text once the transport reports connected")
	cookiesFile := flag.String("cookies-file", "",
		"JSON file shaped {\"<url>\": {\"cookieName\": \"value\", ...}} - applied via ApplyCookies "+
			"shortly after Start(), same as an app relaying a phone/WebView captcha solve back to the transport.")
	burstCount := flag.Int("burst", 0,
		"Once connected, send this many packets back-to-back as fast as Send() accepts them, then report "+
			"elapsed time and throughput. 0 (default) = no burst, just -send's single packet.")
	burstSize := flag.Int("burst-size", 512, "Payload size in bytes for each -burst packet")
	flag.Parse()

	if *scriptPath == "" || *pubkeyHex == "" {
		fmt.Fprintln(os.Stderr, "usage: scripttest -script <path> -pubkey <hex> -url <url> [-duration 30s] [-send text]")
		os.Exit(2)
	}

	pub, err := script.DecodePublicKeyHex(*pubkeyHex)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pubkey:", err)
		os.Exit(1)
	}

	params := map[string]interface{}{}
	if *paramsRaw != "" {
		for _, kv := range strings.Split(*paramsRaw, ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				params[k] = v
			}
		}
	}

	name := *scriptPath
	tr, err := script.New(name, *scriptPath, pub, *url, params, transport.DefaultConfig())
	if err != nil {
		fmt.Fprintln(os.Stderr, "New:", err)
		os.Exit(1)
	}

	tr.SetEventHandler(func(kind string, payload map[string]interface{}) {
		fmt.Printf("[%s] EVENT %s %+v\n", ts(), kind, payload)
	})
	recv := 0
	tr.Receive(func(b []byte) {
		recv++
		fmt.Printf("[%s] RECV #%d %d bytes %q\n", ts(), recv, len(b), truncate(string(b), 200))
	})

	fmt.Printf("[%s] starting %s against %q ...\n", ts(), *scriptPath, *url)
	if err := tr.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "Start:", err)
		os.Exit(1)
	}
	defer func() {
		fmt.Printf("[%s] stopping\n", ts())
		_ = tr.Stop()
	}()

	if *cookiesFile != "" {
		values, err := loadCookiesFile(*cookiesFile, *url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cookies-file:", err)
			os.Exit(1)
		}
		if err := tr.ApplyCookies(values); err != nil {
			fmt.Fprintln(os.Stderr, "ApplyCookies:", err)
			os.Exit(1)
		}
		fmt.Printf("[%s] applied %d cookies from %s\n", ts(), len(values), *cookiesFile)
	}

	deadline := time.After(*duration)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	sent := false
	burstDone := false
	for {
		select {
		case <-tick.C:
			state, errMsg := tr.LastState()
			stats := tr.Stats()
			fmt.Printf("[%s] connected=%-5v state=%-12s err=%q sent=%dB recv=%dB reconnects=%d\n",
				ts(), tr.IsConnected(), state, errMsg, stats.BytesSent, stats.BytesReceived, stats.Reconnects)
			if tr.IsConnected() && !sent && *sendText != "" {
				if err := tr.Send([]byte(*sendText)); err != nil {
					fmt.Printf("[%s] Send error: %v\n", ts(), err)
				} else {
					fmt.Printf("[%s] Send OK: %q\n", ts(), *sendText)
				}
				sent = true
			}
			if tr.IsConnected() && !burstDone && *burstCount > 0 {
				burstDone = true
				runBurst(tr, *burstCount, *burstSize)
			}
		case <-deadline:
			fmt.Printf("[%s] duration elapsed, done. total recv=%d packets\n", ts(), recv)
			return
		}
	}
}

// loadCookiesFile reads {"<url>": {"name": "value", ...}} and returns the
// map for urlHint if present, else the map from the file's only entry -
// same shape scripttest -cookies-file expects.
func loadCookiesFile(path, urlHint string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var all map[string]map[string]string
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if v, ok := all[urlHint]; ok {
		return v, nil
	}
	for _, v := range all {
		return v, nil
	}
	return nil, fmt.Errorf("no entries in %s", path)
}

// runBurst sends n packets of size bytes back-to-back as fast as Send()
// accepts them (no pacing) and reports elapsed time/throughput - the
// "how fast can this transport actually move data" measurement.
func runBurst(tr *script.ScriptTransport, n, size int) {
	payload := make([]byte, size)
	_, _ = rand.Read(payload)

	fmt.Printf("[%s] burst: sending %d x %dB packets as fast as Send() accepts them...\n", ts(), n, size)
	start := time.Now()
	sentOK, failed := 0, 0
	for i := 0; i < n; i++ {
		if err := tr.Send(payload); err != nil {
			failed++
			time.Sleep(2 * time.Millisecond) // back off on a full queue, don't spin
			continue
		}
		sentOK++
	}
	elapsed := time.Since(start)
	bps := float64(sentOK*size) / elapsed.Seconds()
	pps := float64(sentOK) / elapsed.Seconds()
	fmt.Printf("[%s] burst done: sent=%d failed=%d elapsed=%s throughput=%.0f pkt/s (%.1f KB/s submit-side)\n",
		ts(), sentOK, failed, elapsed.Round(time.Millisecond), pps, bps/1024)
}

func ts() string { return time.Now().Format("15:04:05.000") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
