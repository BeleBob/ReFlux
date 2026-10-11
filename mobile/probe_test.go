package mobile

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
)

// Without a Session tunnel there is nothing to probe, and the answer says so.
func TestProbeTunnelWithoutATunnel(t *testing.T) {
	setAuthProxy(nil)
	var r struct {
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		Unavailable bool   `json:"unavailable"`
	}
	if err := json.Unmarshal([]byte(ProbeTunnel("http://example.com/", 100)), &r); err != nil || r.OK || r.Error == "" || !r.Unavailable {
		t.Errorf("probe without a tunnel: %+v %v", r, err)
	}
}

// The probe reaches a web server through the phone's Session, the exit and
// the exit's own network, and tells the time it took.
func TestProbeTunnelThroughTheExit(t *testing.T) {
	web := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	ln, err := net.Listen("tcp4", net.JoinHostPort(lanIPv4(t).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	web.Listener = ln
	web.Start()
	defer web.Close()

	free, _ := net.Listen("tcp", "127.0.0.1:0")
	directAddr := free.Addr().String()
	free.Close()

	const doc = "https://docs.example/d"
	exitSess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP,
		MaxPacketSize: transport.MaxNegotiatedPacket,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	exit := manager.New(exitSess, nil, testSecret, doc)
	dcfg := transport.DefaultDirectConfig()
	dcfg.ListenAddr, dcfg.IsExit = directAddr, true
	direct := transport.NewDirectTransport(transport.DefaultConfig(), dcfg)
	if err := exitSess.AddTransport("direct", direct, testSecret, doc, 100); err != nil {
		t.Fatal(err)
	}
	if err := exit.Add("direct", "direct", direct, 100, nil); err != nil {
		t.Fatal(err)
	}
	// Two carriers, as in TestExitCaptchaSolvedThroughTunnel: a single one
	// makes the phone start in classic fallback.
	stuck := &stubCarrier{}
	if err := exit.Add("yandex", "yandex", stuck, 50, stuck); err != nil {
		t.Fatal(err)
	}
	exitSess.SetControlHandler(exit.DispatchControl)
	exitTunnel := tunnel.NewTCPTunnelMode(exit, true, tunnel.ExitModeL4)
	defer exitTunnel.Close()
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()

	specs := fmt.Sprintf(`[{"name":"direct","type":"direct","priority":100,"params":{"dial":%q}},
		{"name":"yandex","type":"yandex","url":%q,"priority":50}]`, directAddr, doc)
	phone, err := buildSession(specs, testSecret, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = phone.Stop(); setAuthProxy(nil) }()
	if err := phone.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the exit to see the phone", exit.IsConnected)

	var r struct {
		OK     bool   `json:"ok"`
		Status int    `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(ProbeTunnel(web.URL+"/generate_204", 10000)), &r); err != nil || !r.OK || r.Status != http.StatusNoContent {
		t.Fatalf("probe through the tunnel: %+v %v", r, err)
	}
}
