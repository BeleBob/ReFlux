package mobile

import (
	"fmt"
	"net"
	"testing"
	"time"

	"openflux/transport"
)

const testSecret = "mobile session test secret"

// The phone must derive the same encryption context as an exit started with
// --url=<doc> and reach it over direct.
func TestBuildSessionConnectsOverDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	const doc = "https://docs.example/d"
	exit, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP,
		MaxPacketSize: transport.MaxNegotiatedPacket,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	dcfg := transport.DefaultDirectConfig()
	dcfg.ListenAddr, dcfg.IsExit = addr, true
	if err := exit.AddTransport("direct", transport.NewDirectTransport(transport.DefaultConfig(), dcfg), testSecret, doc, 100); err != nil {
		t.Fatal(err)
	}
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()

	// yandex never connects here (no such document); direct carries it.
	specs := fmt.Sprintf(`[
		{"name":"direct","type":"direct","priority":100,"params":{"dial":%q}},
		{"name":"yandex","type":"yandex","url":%q,"priority":50}
	]`, addr, doc)
	phone, err := buildSession(specs, testSecret, false)
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Stop()
	if err := phone.Start(); err != nil {
		t.Fatalf("handshake over direct: %v", err)
	}
	if got := CurrentTransport(); got != "direct" {
		t.Fatalf("CurrentTransport = %q, want direct", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !exit.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("exit did not see the phone")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSessionContextPrefersHighestPriorityURL(t *testing.T) {
	specs := []sessionSpec{
		{Name: "mailru", URL: "https://cloud.example/m", Priority: 10},
		{Name: "direct", Priority: 100},
		{Name: "yandex", URL: "https://docs.example/d", Priority: 50},
	}
	if got := sessionContext(specs); got != "https://docs.example/d" {
		t.Fatalf("context = %q", got)
	}
	if got := sessionContext([]sessionSpec{{Name: "direct"}}); got != "http://#" {
		t.Fatalf("context without URLs = %q", got)
	}
}
