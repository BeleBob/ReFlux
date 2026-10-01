package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Tunnel availability: every minute it checks, the bot counts whether the
// world and the Russian tunnel worked, per day, in uptime.json (60 days).
// Russia on its direct fallback works; an egress that is down or a docker
// that does not answer counts as both tunnels down. The weekly report and
// the panel show the share.

const uptimeDays = 60

// uptimeCount is minutes up and minutes checked.
type uptimeCount [2]int

func (c uptimeCount) percent() float64 {
	if c[1] == 0 {
		return 0
	}
	return 100 * float64(c[0]) / float64(c[1])
}

type uptimeDay struct {
	World  uptimeCount `json:"world"`
	Russia uptimeCount `json:"russia"`
}

type uptimeFile struct {
	Days map[string]*uptimeDay `json:"days"`
}

func (s Store) uptimePath() string { return filepath.Join(s.Root, "uptime.json") }

func (s Store) readUptime() uptimeFile {
	f := uptimeFile{Days: map[string]*uptimeDay{}}
	if b, err := os.ReadFile(s.uptimePath()); err == nil {
		json.Unmarshal(b, &f)
	}
	if f.Days == nil {
		f.Days = map[string]*uptimeDay{}
	}
	return f
}

// tunnelsUp reads the checks: whether the world and Russia worked.
func tunnelsUp(fs []finding) (world, russia bool) {
	for _, f := range fs {
		switch f.Key {
		case "world":
			world = f.Level == levelOK
		case "russia":
			russia = f.Level != levelFail // the direct fallback carries Russia
		}
	}
	return world, russia
}

// recordUptime counts one check.
func (s Store) recordUptime(fs []finding, now time.Time) error {
	world, russia := tunnelsUp(fs)
	f := s.readUptime()
	key := now.Format(time.DateOnly)
	d := f.Days[key]
	if d == nil {
		d = &uptimeDay{}
		f.Days[key] = d
	}
	add := func(c *uptimeCount, up bool) {
		c[1]++
		if up {
			c[0]++
		}
	}
	add(&d.World, world)
	add(&d.Russia, russia)
	oldest := now.AddDate(0, 0, -uptimeDays).Format(time.DateOnly)
	for k := range f.Days {
		if k < oldest {
			delete(f.Days, k)
		}
	}
	return writeJSON(s.uptimePath(), f)
}

// uptimeView is a day's availability for the pages.
type uptimeView struct {
	Day           time.Time
	World, Russia float64 // percent
	Checked       bool
}

// uptimeSpan sums the days in [from, to) and lists them, the newest first.
func (s Store) uptimeSpan(from, to time.Time) (world, russia uptimeCount, days []uptimeView) {
	f := s.readUptime()
	keys := make([]string, 0, len(f.Days))
	for k := range f.Days {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, k := range keys {
		at, err := time.ParseInLocation(time.DateOnly, k, from.Location())
		if err != nil || at.Before(from) || !at.Before(to) {
			continue
		}
		d := f.Days[k]
		world[0], world[1] = world[0]+d.World[0], world[1]+d.World[1]
		russia[0], russia[1] = russia[0]+d.Russia[0], russia[1]+d.Russia[1]
		days = append(days, uptimeView{Day: at, World: d.World.percent(), Russia: d.Russia.percent(), Checked: d.World[1] > 0})
	}
	return world, russia, days
}
