package main

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"
)

// Active reports whether c's node should run at now: not paused and not
// past its expiry.
func (c Client) Active(now time.Time) bool {
	return !c.Paused && (c.Expires.IsZero() || now.Before(c.Expires))
}

// accessPhrase says what c's access is at now: active, paused, expired or
// until a time.
func accessPhrase(c Client, now time.Time) phrase {
	switch {
	case c.Paused:
		return ph("access.paused")
	case c.Expires.IsZero():
		return ph("access.active")
	case !now.Before(c.Expires):
		return ph("access.expired")
	}
	return ph("access.until", c.Expires.Local().Format("2006-01-02 15:04"))
}

// accessText is accessPhrase in English: the ACCESS column of the list.
func accessText(c Client, now time.Time) string {
	p := accessPhrase(c, now)
	return tr(langEN, p.id, p.args...)
}

var relExpiryRe = regexp.MustCompile(`^([0-9]+)([dw])$`)

// parseExpiry reads when access ends: "never", a date (access lasts
// through that day, server time), a number of days or weeks ("30d",
// "2w") or a Go duration ("12h", "90m"), counted from now.
func parseExpiry(s string, now time.Time) (time.Time, error) {
	if s == "never" {
		return time.Time{}, nil
	}
	if d, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return d.AddDate(0, 0, 1), nil
	}
	if m := relExpiryRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 && n <= 3650 {
			if m[2] == "w" {
				n *= 7
			}
			return now.AddDate(0, 0, n).Truncate(time.Second), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(d).Truncate(time.Second), nil
	}
	return time.Time{}, fmt.Errorf("bad expiry %q: use never, a date (2026-12-31), 30d, 2w or 12h", s)
}

// setAccess changes a channel's pause or expiry and applies it: apply
// starts or removes the node to match.
func setAccess(s Store, name string, stdout io.Writer, change func(*Client) error) error {
	c, err := s.Get(name)
	if err != nil {
		return err
	}
	if err := change(&c); err != nil {
		return err
	}
	if err := s.Save(c); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %s\n", c.Name, accessText(c, time.Now()))
	return apply(s, stdout)
}

func cmdPause(s Store, args []string, stdout io.Writer) error {
	name, err := parseArgs(newFlagSet("pause"), args)
	if err != nil {
		return err
	}
	return setAccess(s, name, stdout, func(c *Client) error {
		c.Paused = true
		return nil
	})
}

func cmdResume(s Store, args []string, stdout io.Writer) error {
	name, err := parseArgs(newFlagSet("resume"), args)
	if err != nil {
		return err
	}
	return setAccess(s, name, stdout, func(c *Client) error {
		if !c.Expires.IsZero() && !time.Now().Before(c.Expires) {
			return fmt.Errorf("%s expired; extend it first: reflux expire %s 30d", c.Name, c.Name)
		}
		c.Paused = false
		return nil
	})
}

func cmdExpire(s Store, args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: reflux expire <name> <never|2026-12-31|30d|2w|12h>")
	}
	when, err := parseExpiry(args[1], time.Now())
	if err != nil {
		return err
	}
	return setAccess(s, args[0], stdout, func(c *Client) error {
		c.Expires = when
		return nil
	})
}

// activeClients filters the channels whose nodes should run.
func activeClients(clients []Client, now time.Time) []Client {
	var out []Client
	for _, c := range clients {
		if c.Active(now) {
			out = append(out, c)
		}
	}
	return out
}
