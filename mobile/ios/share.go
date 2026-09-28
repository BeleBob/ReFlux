//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"

	mobile "openflux-mobile"
	"openflux/share"
)

// Links are read by the core only: the app hands the string over and gets
// JSON back, the same reading Desktop, Android and the CLI make.

type shareResult struct {
	Error  string        `json:"error,omitempty"`
	Config *share.Config `json:"config,omitempty"`
	// Context is the encryption context a client derives for the link.
	Context string `json:"context,omitempty"`
	// Session is the profile ready for OpenFluxStartSession /
	// OpenFluxStartSessionPacketTunnel (with the link's secret): every
	// carrier, its name, priority and address, and the context. A
	// one-carrier link works there too, speaking classic to a classic node.
	Session string `json:"session,omitempty"`
	Link    string `json:"link,omitempty"`
}

// OpenFluxShareDecode reads an openflux:// link: {"config":...,
// "context":...,"session":...} or {"error":...}. Free with
// OpenFluxFreeString.
//
//export OpenFluxShareDecode
func OpenFluxShareDecode(link *C.char) *C.char {
	if link == nil {
		return jsonString(shareResult{Error: "empty link"})
	}
	specs, err := mobile.ShareSessionSpecs(C.GoString(link))
	if err != nil {
		return jsonString(shareResult{Error: err.Error()})
	}
	cfg, err := share.Decode(C.GoString(link))
	if err != nil {
		return jsonString(shareResult{Error: err.Error()})
	}
	var ctx struct {
		Context string `json:"context"`
	}
	_ = json.Unmarshal([]byte(specs), &ctx)
	return jsonString(shareResult{Config: &cfg, Context: ctx.Context, Session: specs})
}

// OpenFluxShareEncode builds a link from a share.Config JSON: {"link":...}
// or {"error":...}. The core validates it, so no invalid link leaves the
// app.
//
//export OpenFluxShareEncode
func OpenFluxShareEncode(cfgJSON *C.char) *C.char {
	if cfgJSON == nil {
		return jsonString(shareResult{Error: "empty config"})
	}
	var cfg share.Config
	if err := json.Unmarshal([]byte(C.GoString(cfgJSON)), &cfg); err != nil {
		return jsonString(shareResult{Error: "bad config json: " + err.Error()})
	}
	link, err := share.Encode(cfg)
	if err != nil {
		return jsonString(shareResult{Error: err.Error()})
	}
	return jsonString(shareResult{Link: link})
}
