package mobile

import (
	"encoding/json"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// A ReFlux client's documents: cups.online rooms first, mail.ru behind. The
// node derives its context from all of them (the mail.ru document, since
// rooms do not count); an app that leaves mail.ru out passes that context
// explicitly and keeps the node's keys.
func TestSessionContextKeepsTheNodesKeysWithoutMailru(t *testing.T) {
	full := `[{"name":"cupsonline","type":"cupsonline","url":"rooms","priority":100},
		{"name":"mailru","type":"mailru","url":"https://cloud.mail.ru/public/a","priority":50},
		{"name":"mailru-2","type":"mailru","url":"https://cloud.mail.ru/public/b","priority":50}]`
	ctx, err := SessionContext(full)
	if err != nil {
		t.Fatal(err)
	}
	if ctx != "https://cloud.mail.ru/public/a" {
		t.Fatalf("context of the full list = %q", ctx)
	}
	short := `[{"name":"cupsonline","type":"cupsonline","url":"rooms","priority":100}]`
	if got, _ := SessionContext(short); got == ctx {
		t.Fatalf("the rooms alone give the node's context %q: the test proves nothing", got)
	}
	b, _ := json.Marshal(map[string]interface{}{"context": ctx, "transports": json.RawMessage(short)})
	if got, err := SessionContext(string(b)); err != nil || got != ctx {
		t.Fatalf("context of the short list with the explicit one = %q, %v; want %q", got, err, ctx)
	}
	if _, err := SessionContext("not json"); err == nil {
		t.Error("broken specs give no error")
	}
}

func TestCarrierStatus(t *testing.T) {
	clearRoute()
	if got := CarrierStatus(); got != "[]" {
		t.Fatalf("without a Session: %s", got)
	}
	sess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP,
		MaxPacketSize: transport.MaxNegotiatedPacket,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddTransport("cupsonline", &stubCarrier{}, testSecret, "ctx", 100); err != nil {
		t.Fatal(err)
	}
	if err := sess.AddTransport("mailru", &stubCarrier{}, testSecret, "ctx", 50); err != nil {
		t.Fatal(err)
	}
	setSessionRoute(sess, map[string]string{"cupsonline": "cupsonline", "mailru": "mailru"})
	defer clearRoute()
	var got []carrierStatus
	if err := json.Unmarshal([]byte(CarrierStatus()), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "cupsonline" || got[0].Type != "cupsonline" || got[1].Type != "mailru" {
		t.Fatalf("carriers = %+v", got)
	}
	if got[0].Attached || got[0].Live {
		t.Errorf("a carrier that never connected is attached or live: %+v", got[0])
	}
}
