package main

import (
	"fmt"
	"io"
	"strconv"
)

// A node logs nothing but its status lines unless its client is given a
// log level (`reflux debug <name> 1`). Level 1 adds what the carriers
// carry: the per-lane figures of a striped carrier (cups.online rooms),
// every 5 seconds, which is what tells a channel that works one way from
// one that works both. 2 adds every session, crypto and control step, 3
// hexdumps. The level lives in client.json, so apply, update and restart
// keep it; the node is recreated to pick it up.
//
// It is for looking into a client's trouble: 2 and 3 print a line per
// packet, which costs the node speed, so they are for minutes, not days.

// debugLevels is what each level says, for the CLI.
var debugLevels = map[int]string{
	0: "off: status lines only",
	1: "the carriers' per-lane figures every 5 s (cheap; leave it on while a problem is being looked into)",
	2: "every session, carrier and crypto step, a line per packet (slows the node down)",
	3: "the same with hexdumps (slower still)",
}

// cmdDebug: reflux debug [<name> [0-3]].
func cmdDebug(s Store, args []string, stdout io.Writer) error {
	if len(args) > 2 {
		return fmt.Errorf("usage: reflux debug [<name> [0-3]]")
	}
	if len(args) == 0 {
		clients, err := s.List()
		if err != nil {
			return err
		}
		on := false
		for _, c := range clients {
			if c.Debug > 0 {
				on = true
				fmt.Fprintf(stdout, "%-16s %d  %s\n", c.Name, c.Debug, debugLevels[c.Debug])
			}
		}
		if !on {
			fmt.Fprintln(stdout, "No node logs more than its status lines. reflux debug <name> 1 turns the carriers' figures on.")
		}
		return nil
	}
	c, err := s.Get(args[0])
	if err != nil {
		return err
	}
	if len(args) == 1 {
		fmt.Fprintf(stdout, "%s: %d  %s\n", c.Name, c.Debug, debugLevels[c.Debug])
		return nil
	}
	level, err := strconv.Atoi(args[1])
	if err != nil || level < 0 || level > 3 {
		return fmt.Errorf("the level is 0 to 3, not %q", args[1])
	}
	if c.Debug == level {
		fmt.Fprintf(stdout, "%s already logs at %d.\n", c.Name, level)
		return nil
	}
	c.Debug = level
	if err := s.Save(c); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s now logs at %d (%s).\nReading it: reflux logs %s\n", c.Name, level, debugLevels[level], c.Name)
	if level >= 2 {
		fmt.Fprintln(stdout, "A line per packet costs the node speed: turn it back to 1 or 0 once you are done.")
	}
	// The node reads node.conf at start, so it has to be recreated.
	return restartNode(s, c.Name, stdout)
}
