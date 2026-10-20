package control

import (
	"bytes"
	"testing"
)

func TestClientLogPartsRoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte("log "), 700) // 2800 bytes: three parts
	parts, err := SplitClientLog(42, data)
	if err != nil || len(parts) != 3 {
		t.Fatalf("%d parts, %v", len(parts), err)
	}
	var joined []byte
	for i, p := range parts {
		got, err := DecodeClientLogPart(p.Encode())
		if err != nil || got.ID != 42 || got.Part != uint16(i) || got.Parts != 3 {
			t.Fatalf("part %d: %+v, %v", i, got, err)
		}
		joined = append(joined, got.Data...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("the parts do not make the log")
	}
	for _, bad := range [][]byte{
		nil,
		ClientLogPart{ID: 1, Part: 0, Parts: 1}.Encode(),                                      // no data
		ClientLogPart{ID: 1, Part: 1, Parts: 1, Data: []byte{1}}.Encode(),                     // past the end
		ClientLogPart{ID: 1, Part: 0, Parts: ClientLogMaxParts + 1, Data: []byte{1}}.Encode(), // too many
		ClientLogPart{ID: 1, Part: 0, Parts: 1, Data: make([]byte, ClientLogPartBytes+1)}.Encode(),
	} {
		if _, err := DecodeClientLogPart(bad); err == nil {
			t.Errorf("accepted %x", bad[:min(len(bad), 12)])
		}
	}
	if _, err := SplitClientLog(1, make([]byte, ClientLogMaxParts*ClientLogPartBytes+1)); err == nil {
		t.Error("split a log over 256 KiB")
	}
}
