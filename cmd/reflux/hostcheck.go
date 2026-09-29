package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// readRules returns the host's IPv4 policy rules; tests replace it.
var readRules = func() (string, error) {
	out, err := exec.Command("ip", "-4", "rule", "show").Output()
	return string(out), err
}

// hostRuleProblem checks the host's `ip -4 rule show` output: when the host
// has policy rules of its own (a full-tunnel VPN such as awg0), the rule
// sending the egress subnet to the main table must exist and come before
// all of them, or the egress tunnels would go through the host's VPN. A
// host with only the kernel's default rules needs nothing. It returns ""
// when all is well.
func hostRuleProblem(rules string) string {
	defaults := map[int]bool{0: true, 32766: true, 32767: true}
	ours, first := -1, -1
	for _, line := range strings.Split(rules, "\n") {
		prefStr, rule, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		pref, err := strconv.Atoi(prefStr)
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(rule), "from "+EgressSubnet+" lookup main") {
			ours = pref
			continue
		}
		if !defaults[pref] && (first < 0 || pref < first) {
			first = pref
		}
	}
	switch {
	case first < 0:
		return ""
	case ours < 0:
		return fmt.Sprintf("the host has its own routing rules (a VPN?) but no rule for %s: "+
			"the egress tunnels would go through the host's VPN. Install "+
			"deploy/reflux/host/reflux-egress-route.service", EgressSubnet)
	case ours > first:
		return fmt.Sprintf("the rule for %s (priority %d) comes after another rule (priority %d): "+
			"restart it with `sudo systemctl restart reflux-egress-route`", EgressSubnet, ours, first)
	}
	return ""
}
