package mobile

// Script-transport (JS/goja) engine surface for the apps. This file is what
// links the goja engine into the gomobile AAR: importing transport/script
// here pulls the whole engine into the app's core library, next to the
// native transports and the phpbox node, without changing either.
//
// For this intermediate bundle the only exported entry point is an in-process
// self-test: it loads a signed echo script, runs a full open->connected->
// send->emit round trip inside the app's own process and reports the result,
// so the app can prove on the device that the JS engine is present and runs.
// Picking a real script transport in a profile is a later step.

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/script"
)

// InspectTransport reads a downloaded transport before it is trusted or run
// and returns a JSON report for the trust dialog, WITHOUT running open() or
// touching the network. data is a .flux package (zip) or a bare .js source;
// sig is the detached signature for a bare .js (ignored for .flux, which
// carries its own); pubkeyHex is the candidate author key (from the share
// link / GitHub release / manual entry), or "" to inspect without a key.
//
// Fields: ok, name, version, params[{key,label,type,required}],
// signature ("valid"|"invalid"|"unverified"), fingerprint (SHA-256 of the
// key), official (bool), author, error. See script.InspectTrust, which this
// wraps for gomobile (whose bindings only cross basic scalar types).
func InspectTransport(data []byte, sig []byte, pubkeyHex string) string {
	b, _ := json.Marshal(script.InspectTrust(data, sig, pubkeyHex, script.OfficialKeyHex))
	return string(b)
}

// ScriptFingerprint returns the SHA-256 (hex) of an author public key, the
// stable id the UI shows and the user compares out of band. "" if the key
// can't be decoded.
func ScriptFingerprint(pubkeyHex string) string {
	return script.Fingerprint(pubkeyHex)
}

// OfficialScriptKey is the first-party signing key (hex), so the app can pin
// bundled/official transports without hardcoding it in Kotlin too.
func OfficialScriptKey() string { return script.OfficialKeyHex }

//go:embed scriptassets/echo.js scriptassets/echo.js.sig
var scriptAssets embed.FS

// embedPubKeyHex verifies the embedded echo script. Its matching private key
// is a throwaway kept out of the repo; it is only ever used to sign this one
// self-test asset, never a real transport.
const embedPubKeyHex = "80a27854f0ed7709d35dc382f8f23b5a9171cbe27844bc7bc189ca84dde84c7f"

// ScriptEngineAvailable reports whether the JS (goja) script-transport engine
// is linked into this build. Always true once this file is compiled in; the
// app can call it to decide whether to show script-transport UI.
func ScriptEngineAvailable() bool { return true }

// ScriptEngineSelfTest loads the embedded signed echo script, runs a full
// open->connected->Send->emit round trip in this process, and returns a JSON
// string: {"ok":bool,"engine":"goja","state":...,"sent":N,"recv":N,"error":...}.
//
// dir must be a writable directory (the app passes its cacheDir): the engine's
// loader verifies <script>.sig next to the script on disk, so the embedded
// bytes are materialized there first. Everything is cleaned up before return.
func ScriptEngineSelfTest(dir string) string {
	res := map[string]interface{}{"ok": false, "engine": "goja"}
	fail := func(stage string, err error) string {
		res["error"] = stage + ": " + err.Error()
		b, _ := json.Marshal(res)
		return string(b)
	}

	if dir == "" {
		dir = os.TempDir()
	}
	work, err := os.MkdirTemp(dir, "jsselftest-")
	if err != nil {
		return fail("mkdir", err)
	}
	defer os.RemoveAll(work)

	for _, name := range []string{"echo.js", "echo.js.sig"} {
		b, err := scriptAssets.ReadFile("scriptassets/" + name)
		if err != nil {
			return fail("read asset "+name, err)
		}
		if err := os.WriteFile(filepath.Join(work, name), b, 0o600); err != nil {
			return fail("write "+name, err)
		}
	}

	pub, err := script.DecodePublicKeyHex(embedPubKeyHex)
	if err != nil {
		return fail("pubkey", err)
	}

	tr, err := script.New("echo", filepath.Join(work, "echo.js"), pub, "local://selftest", nil, transport.DefaultConfig())
	if err != nil {
		return fail("new", err) // signature failure lands here too
	}

	var mu sync.Mutex
	recv := 0
	tr.Receive(func(b []byte) {
		mu.Lock()
		recv += len(b)
		mu.Unlock()
	})

	if err := tr.Start(); err != nil {
		return fail("start", err)
	}
	defer tr.Stop()

	// Wait for the script to report connected, then send one probe and wait
	// for it to loop back up through emit().
	deadline := time.Now().Add(4 * time.Second)
	for !tr.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !tr.IsConnected() {
		state, errMsg := tr.LastState()
		res["state"] = state
		return fail("connect", fmt.Errorf("not connected (state=%s err=%q)", state, errMsg))
	}

	probe := []byte("js-engine-selftest")
	if err := tr.Send(probe); err != nil {
		return fail("send", err)
	}
	echoDeadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := recv
		mu.Unlock()
		if got >= len(probe) || time.Now().After(echoDeadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	state, _ := tr.LastState()
	mu.Lock()
	got := recv
	mu.Unlock()
	res["state"] = state
	res["sent"] = len(probe)
	res["recv"] = got
	res["ok"] = got >= len(probe)
	b, _ := json.Marshal(res)
	return string(b)
}
