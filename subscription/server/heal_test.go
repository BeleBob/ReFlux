package server

import (
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
)

func TestAutoNodeConfigRules(t *testing.T) {
	good := func() *Config {
		return &Config{
			PublicHost: "h", AdminToken: strings.Repeat("x", 24),
			Pools: []Pool{{ID: "a", Name: "A", Channel: "c", AutoNode: true,
				Transports: []provision.ChannelTransport{{Type: "cupsonline", URL: ""}}}},
			Plans: []Plan{{ID: "p", Name: "P", Days: 1, Devices: 1}},
		}
	}
	if err := good().Check(); err != nil {
		t.Fatal(err)
	}
	auto := good()
	if err := auto.Check(); err != nil {
		t.Fatal(err)
	}
	if got := auto.NodeScript; got != "/opt/openflux-node/node-install.sh" {
		t.Fatalf("NodeScript default %q", got)
	}
	var emptyCups = []provision.ChannelTransport{{Type: "cupsonline", URL: ""}}
	for name, mutate := range map[string]func(*Config){
		"underscore channel":  func(c *Config) { c.Pools[0].Channel = "a_b" },
		"bad transport":       func(c *Config) { c.Pools[0].Transports = []provision.ChannelTransport{{Type: "vyandex"}} },
		"duplicate transport": func(c *Config) {
			c.Pools[0].Transports = append(append(emptyCups, emptyCups...), emptyCups...)
		},
		"yandex empty url":    func(c *Config) { c.Pools[0].Transports = []provision.ChannelTransport{{Type: "yandex"}} },
		"cups without autoNode": func(c *Config) { c.Pools[0].AutoNode = false },
		"bad mode":            func(c *Config) { c.Pools[0].Mode = "l9" },
	} {
		c := good()
		mutate(c)
		if err := c.Check(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// No autoNode: the old pools stay valid (an empty cups transport nowhere).
	plain := &Config{
		PublicHost: "h", AdminToken: strings.Repeat("x", 24),
		Pools: []Pool{{ID: "a", Name: "A", Channel: "c"}},
		Plans: []Plan{{ID: "p", Name: "P", Days: 1, Devices: 1}},
	}
	if err := plain.Check(); err != nil {
		t.Fatal(err)
	}
}