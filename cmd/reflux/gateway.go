package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Files in the egress's config directory the owner's choices go to (see
// reflux-egress): worldSelect names the chosen world server, the egress
// switches to it within a probe round; ruDirectFile and ruFallbackFile set
// how Russia leaves, read when the egress starts.
const (
	worldSelect    = "world-select"
	ruDirectFile   = "ru-direct"
	ruFallbackFile = "ru-fallback-direct"
)

func (s Store) egressDir() string { return filepath.Join(s.Root, "egress") }

// worldConfigs lists the world servers in failover order.
func (s Store) worldConfigs() []string {
	files, _ := filepath.Glob(filepath.Join(s.egressDir(), "world-*.conf"))
	sort.Strings(files)
	return baseNames(files)
}

// chosenWorld is the world server the owner chose, "" when the egress
// picks.
func (s Store) chosenWorld() string {
	b, err := os.ReadFile(filepath.Join(s.egressDir(), worldSelect))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// chooseWorld makes the egress switch to world server name; "auto" drops
// the choice (the egress keeps the server it has).
func (s Store) chooseWorld(name string) error {
	path := filepath.Join(s.egressDir(), worldSelect)
	if name == "auto" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if !slices.Contains(s.worldConfigs(), name) {
		return fmt.Errorf("no world server %q in %s", name, s.egressDir())
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(name+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// russiaModes are how Russia can leave: its tunnel with a direct fallback,
// its tunnel only, or directly.
var russiaModes = []string{"fallback", "tunnel", "direct"}

func (s Store) russiaMode() string {
	if _, err := os.Stat(filepath.Join(s.egressDir(), ruDirectFile)); err == nil {
		return "direct"
	}
	if _, err := os.Stat(filepath.Join(s.egressDir(), ruFallbackFile)); err == nil {
		return "fallback"
	}
	return "tunnel"
}

// setRussiaMode writes the files for mode; the egress reads them when it
// starts.
func (s Store) setRussiaMode(mode string) error {
	if !slices.Contains(russiaModes, mode) {
		return fmt.Errorf("unknown mode %q: use %s", mode, strings.Join(russiaModes, ", "))
	}
	if mode != "direct" {
		if ru, _ := filepath.Glob(filepath.Join(s.egressDir(), "ru-*.conf")); len(ru) == 0 {
			return fmt.Errorf("mode %s needs a ru-*.conf in %s", mode, s.egressDir())
		}
	}
	set := func(name string, on bool) error {
		path := filepath.Join(s.egressDir(), name)
		if on {
			return os.WriteFile(path, nil, 0o600)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := set(ruDirectFile, mode == "direct"); err != nil {
		return err
	}
	return set(ruFallbackFile, mode == "fallback")
}

const gatewayUsage = `usage:
  reflux gateway                       world servers and how Russia leaves
  reflux gateway <world-N.conf|auto>   switch the world tunnel (within 10 s)
  reflux gateway russia <fallback|tunnel|direct>
                                       how Russia leaves; restarts egress and nodes`

func cmdGateway(s Store, args []string, stdout io.Writer) error {
	switch {
	case len(args) == 0:
		st, err := readEgressStatus()
		chosen := s.chosenWorld()
		fmt.Fprintln(stdout, "World servers, in failover order (* in use):")
		for _, w := range s.worldConfigs() {
			mark := " "
			if err == nil && w == st.World {
				mark = "*"
			}
			note := ""
			if w == chosen {
				note = "  (chosen)"
			}
			fmt.Fprintf(stdout, "  %s %s%s\n", mark, w, note)
		}
		if chosen == "" {
			fmt.Fprintln(stdout, "No server chosen: the egress keeps the one that works.")
		}
		fmt.Fprintf(stdout, "Russia: %s\n\n%s\n", s.russiaMode(), gatewayUsage)
		return nil
	case len(args) == 2 && args[0] == "russia":
		if err := s.setRussiaMode(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Russia: %s. Restarting egress and nodes...\n", args[1])
		return cmdRestart(s, stdout)
	case len(args) == 1:
		if err := s.chooseWorld(args[0]); err != nil {
			return err
		}
		if args[0] == "auto" {
			fmt.Fprintln(stdout, "No server chosen: the egress keeps the one that works.")
		} else {
			fmt.Fprintf(stdout, "The egress switches to %s within 10 seconds.\n", args[0])
		}
		return nil
	}
	return errors.New(gatewayUsage)
}
