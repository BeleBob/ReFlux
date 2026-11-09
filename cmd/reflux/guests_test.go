package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The egress forwards for its guests (egress/guests): the namespace needs
// forwarding on; its own forward chain keeps everything else out.
func TestComposeEgressForwards(t *testing.T) {
	b, err := composeYAML("/home/u/reflux", nil, Options{NodeImage: "n", EgressImage: "e", UID: 1000, GID: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var f composeFile
	if err := json.Unmarshal(b[strings.IndexByte(string(b), '{'):], &f); err != nil {
		t.Fatal(err)
	}
	if got := f.Services["egress"].Sysctls["net.ipv4.ip_forward"]; got != "1" {
		t.Errorf("egress net.ipv4.ip_forward = %q", got)
	}
}
