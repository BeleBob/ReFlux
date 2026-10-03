package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/p1neappleXpress/OpenFlux/transport/script"
)

// runInspectScript is the desktop apps' way into transport/script.InspectTrust:
// unlike the mobile apps, which link the engine in via gomobile and call it
// in-process, the desktop app only ever has the compiled core binary, so it
// shells out to this subcommand to verify and read a downloaded transport
// before it is trusted. Always prints a JSON TrustReport to stdout and exits
// 0, even when the script itself fails to verify - "untrusted" is a report
// field (ok/signature), not a process failure.
func runInspectScript(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("--inspect-script", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dataPath := fs.String("data", "", "Path to the downloaded .flux package or bare .js source (required)")
	sigPath := fs.String("sig", "", "Path to the detached signature for a bare .js; ignored for .flux")
	pubkeyHex := fs.String("pubkey", "", "Candidate author public key (hex); omit to inspect without checking a signature")
	if err := fs.Parse(args); err != nil || *dataPath == "" {
		fmt.Fprintln(stdout, `{"ok":false,"signature":"unverified","error":"usage: --inspect-script --data=<path> [--sig=<path>] [--pubkey=<hex>]"}`)
		return 1
	}

	data, err := os.ReadFile(*dataPath)
	if err != nil {
		fmt.Fprintf(stdout, `{"ok":false,"signature":"unverified","error":%q}`+"\n", "read --data: "+err.Error())
		return 0
	}
	var sig []byte
	if *sigPath != "" {
		sig, err = os.ReadFile(*sigPath)
		if err != nil {
			fmt.Fprintf(stdout, `{"ok":false,"signature":"unverified","error":%q}`+"\n", "read --sig: "+err.Error())
			return 0
		}
	}

	report := script.InspectTrust(data, sig, *pubkeyHex, script.OfficialKeyHex)
	b, _ := json.Marshal(report)
	fmt.Fprintln(stdout, string(b))
	return 0
}
