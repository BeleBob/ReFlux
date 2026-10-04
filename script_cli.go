package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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

// scriptUpdateFlags are the flags shared by the update subcommands.
func scriptUpdateFlags(name string, args []string) (*flag.FlagSet, script.Installed, *string, *string, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var inst script.Installed
	fs.StringVar(&inst.ID, "id", "", "Installed transport id (required)")
	fs.StringVar(&inst.Version, "version", "", "Installed version")
	fs.IntVar(&inst.Wire, "wire", 0, "Installed wire generation (0 = 1)")
	fs.StringVar(&inst.PubkeyHex, "pubkey", "", "Pinned author key (hex, required)")
	updates := fs.String("update", "", "Comma-separated update.json URLs from the package manifest (required)")
	channel := fs.String("channel", "stable", "stable | nightly")
	dir := fs.String("dir", "", "Scripts directory")
	ok := fs.Parse(args) == nil && inst.ID != "" && inst.PubkeyHex != ""
	for _, u := range strings.Split(*updates, ",") {
		if u = strings.TrimSpace(u); u != "" {
			inst.Update = append(inst.Update, u)
		}
	}
	return fs, inst, channel, dir, ok
}

func printUpdateReport(stdout io.Writer, rep script.UpdateReport) int {
	b, _ := json.Marshal(rep)
	fmt.Fprintln(stdout, string(b))
	return 0
}

// runCheckScriptUpdate: --check-script-update --id= --version= --pubkey= --update=<url,..> [--wire=] [--channel=]
// Prints a JSON UpdateReport; "no update" and "failed" are report fields, not exit codes.
func runCheckScriptUpdate(args []string, stdout io.Writer) int {
	_, inst, channel, _, ok := scriptUpdateFlags("--check-script-update", args)
	if !ok {
		fmt.Fprintln(stdout, `{"status":"error","code":"usage"}`)
		return 1
	}
	return printUpdateReport(stdout, script.CheckUpdate(context.Background(), inst, *channel, script.HTTPFetcher))
}

// runApplyScriptUpdate: as the check, plus --dir= and optionally --allow-wire-break.
func runApplyScriptUpdate(args []string, stdout io.Writer) int {
	wireBreak := false
	rest := args[:0:0]
	for _, a := range args {
		if a == "--allow-wire-break" {
			wireBreak = true
			continue
		}
		rest = append(rest, a)
	}
	_, inst, channel, dir, ok := scriptUpdateFlags("--apply-script-update", rest)
	if !ok || *dir == "" {
		fmt.Fprintln(stdout, `{"status":"error","code":"usage"}`)
		return 1
	}
	return printUpdateReport(stdout, script.ApplyUpdate(context.Background(), inst, *channel, *dir, wireBreak, script.HTTPFetcher))
}

// runRollbackScript: --rollback-script --id= --pubkey= --dir=
func runRollbackScript(args []string, stdout io.Writer) int {
	_, inst, _, dir, ok := scriptUpdateFlags("--rollback-script", args)
	if !ok || *dir == "" {
		fmt.Fprintln(stdout, `{"status":"error","code":"usage"}`)
		return 1
	}
	return printUpdateReport(stdout, script.RollbackPackage(*dir, inst.ID, inst.PubkeyHex))
}
