package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// The web panel's sampler: every sampleEvery it reads the host (CPU,
// memory, load, temperature, network, containers, the busiest processes)
// and, every other time, the nodes (traffic), keeping an hour of history
// for the charts. It also keeps the clients' traffic per day across node
// restarts, in state/<name>/traffic.json.

const (
	sampleEvery = 5 * time.Second
	historyLen  = 720 // an hour of samples
	trafficDays = 70  // days of traffic kept per client
	saveEvery   = time.Minute
)

// hostPoint is one sample of the host; rates are in bytes per second.
type hostPoint struct {
	At           time.Time
	CPU, Mem     float64 // percent
	Load1        float64
	TempC        float64
	LanRx, LanTx float64
	EgRx, EgTx   float64
}

// ratePoint is a client's traffic rate at a moment, bytes per second.
type ratePoint struct {
	At       time.Time
	Down, Up float64
}

type containerLive struct {
	Name     string
	CPU      float64 // percent of the host
	MemBytes uint64
}

// hostNow is the latest view of the host, for the pages.
type hostNow struct {
	Point      hostPoint
	Load       [3]float64
	Mem        memory
	Uptime     time.Duration
	CPUs       int
	Temps      []sensor
	Disks      []disk
	Top        []procUse
	Containers []containerLive
}

// dayTraffic is a client's traffic on one day: down to the client, up
// from it, in bytes.
type dayTraffic struct {
	Down uint64 `json:"down"`
	Up   uint64 `json:"up"`
}

// trafficFile is state/<name>/traffic.json.
type trafficFile struct {
	Days map[string]dayTraffic `json:"days"`
	// Last is the node's counters when last read: they count from the
	// node's start, so a smaller reading means it restarted.
	Last struct {
		Down, Up uint64
		Uptime   int64 // ms
	} `json:"last"`
}

type sampler struct {
	s Store
	// docker serializes the sampler's docker calls with the handlers'.
	docker *sync.Mutex

	mu      sync.Mutex
	host    []hostPoint
	now     hostNow
	rates   map[string][]ratePoint
	traffic map[string]*trafficFile
	dirty   map[string]bool

	// previous readings
	cpu      cpuTimes
	procs    map[int]proc
	lan, eg  [2]uint64
	at       time.Time
	ctrIDs   map[string]string
	ctrAt    time.Time
	ctrPrev  map[string]uint64
	nodesAt  time.Time
	nodePrev map[string]time.Time
	savedAt  time.Time
}

func newSampler(s Store, docker *sync.Mutex) *sampler {
	return &sampler{s: s, docker: docker, rates: map[string][]ratePoint{},
		traffic: map[string]*trafficFile{}, dirty: map[string]bool{},
		ctrPrev: map[string]uint64{}, nodePrev: map[string]time.Time{}}
}

func (m *sampler) run(stop <-chan struct{}) {
	for {
		m.tick(time.Now())
		select {
		case <-stop:
			m.save(true)
			return
		case <-time.After(sampleEvery):
		}
	}
}

// tick takes one sample.
func (m *sampler) tick(now time.Time) {
	cpu, cpuErr := readCPU()
	procs := readProcs()
	mem, _ := readMemory()
	load, _ := readLoad()
	up, _ := readUptime()
	temps := readTemps()
	lanRx, lanTx, _ := ifaceBytes(lanIface)
	egRx, egTx, _ := ifaceBytes(egressBridge)

	if now.Sub(m.ctrAt) > time.Minute || m.ctrIDs == nil {
		m.docker.Lock()
		m.ctrIDs = containerIDs()
		m.docker.Unlock()
		m.ctrAt = now
	}
	var ctrs []containerLive
	names := make([]string, 0, len(m.ctrIDs))
	for n := range m.ctrIDs {
		names = append(names, n)
	}
	sort.Strings(names)
	dt := now.Sub(m.at).Seconds()
	for _, n := range names {
		c, err := readContainer(n, m.ctrIDs[n])
		if err != nil {
			continue
		}
		live := containerLive{Name: n, MemBytes: c.MemBytes}
		if prev, ok := m.ctrPrev[n]; ok && dt > 0 && c.CPUUsec >= prev {
			live.CPU = 100 * float64(c.CPUUsec-prev) / 1e6 / dt / float64(cpuCount())
		}
		m.ctrPrev[n] = c.CPUUsec
		ctrs = append(ctrs, live)
	}

	p := hostPoint{At: now, Mem: mem.Percent(), Load1: load[0], LanRx: -1}
	if len(temps) > 0 {
		p.TempC = temps[0].C
	}
	var top []procUse
	if !m.at.IsZero() && dt > 0 {
		if cpuErr == nil {
			p.CPU = m.cpu.percent(cpu)
			top = topProcs(m.procs, procs, m.cpu, cpu, 6)
		}
		p.LanRx, p.LanTx = rate(m.lan[0], lanRx, dt), rate(m.lan[1], lanTx, dt)
		p.EgRx, p.EgTx = rate(m.eg[0], egRx, dt), rate(m.eg[1], egTx, dt)
	}
	m.cpu, m.procs, m.at = cpu, procs, now
	m.lan, m.eg = [2]uint64{lanRx, lanTx}, [2]uint64{egRx, egTx}

	m.mu.Lock()
	if p.LanRx >= 0 {
		m.host = appendCapped(m.host, p, historyLen)
	}
	m.now = hostNow{Point: p, Load: load, Mem: mem, Uptime: up, CPUs: cpuCount(), Temps: temps,
		Disks: m.now.Disks, Top: top, Containers: ctrs}
	m.mu.Unlock()

	// The disks and the nodes change slower.
	if now.Sub(m.nodesAt) >= 2*sampleEvery {
		disks := readDisks()
		m.mu.Lock()
		m.now.Disks = disks
		m.mu.Unlock()
		m.sampleNodes(now)
		m.nodesAt = now
	}
	m.save(false)
}

func rate(prev, next uint64, dt float64) float64 {
	if next < prev || dt <= 0 {
		return 0
	}
	return float64(next-prev) / dt
}

func appendCapped[T any](s []T, v T, n int) []T {
	s = append(s, v)
	if len(s) > n {
		s = append(s[:0:0], s[len(s)-n:]...)
	}
	return s
}

// sampleNodes reads every running node's counters and adds what they
// counted since the last reading to today's traffic.
func (m *sampler) sampleNodes(now time.Time) {
	clients, err := m.s.List()
	if err != nil {
		return
	}
	live := nodeStatuses(m.s, activeClients(clients, now))
	day := now.Format(time.DateOnly)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range clients {
		st, ok := live[c.Name]
		if !ok {
			continue
		}
		tf := m.trafficOf(c.Name)
		down, up := st.BytesOut, st.BytesIn
		var dDown, dUp uint64
		if st.UptimeMs < tf.Last.Uptime || down < tf.Last.Down || up < tf.Last.Up {
			dDown, dUp = down, up // the node restarted: it counts from zero
		} else {
			dDown, dUp = down-tf.Last.Down, up-tf.Last.Up
		}
		tf.Last.Down, tf.Last.Up, tf.Last.Uptime = down, up, st.UptimeMs
		if dDown+dUp > 0 {
			d := tf.Days[day]
			d.Down += dDown
			d.Up += dUp
			tf.Days[day] = d
		}
		m.dirty[c.Name] = true
		if prev, ok := m.nodePrev[c.Name]; ok {
			if secs := now.Sub(prev).Seconds(); secs > 0 {
				m.rates[c.Name] = appendCapped(m.rates[c.Name],
					ratePoint{At: now, Down: float64(dDown) / secs, Up: float64(dUp) / secs}, historyLen/2)
			}
		}
		m.nodePrev[c.Name] = now
	}
}

func (s Store) trafficPath(name string) string {
	return filepath.Join(s.stateDir(name), "traffic.json")
}

// trafficOf loads a client's traffic file once. The caller holds m.mu.
func (m *sampler) trafficOf(name string) *trafficFile {
	if tf, ok := m.traffic[name]; ok {
		return tf
	}
	tf := &trafficFile{}
	if b, err := os.ReadFile(m.s.trafficPath(name)); err == nil {
		json.Unmarshal(b, tf)
	}
	if tf.Days == nil {
		tf.Days = map[string]dayTraffic{}
	}
	m.traffic[name] = tf
	return tf
}

// save writes the changed traffic files, at most every saveEvery unless
// forced; days beyond trafficDays are dropped.
func (m *sampler) save(force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !force && time.Since(m.savedAt) < saveEvery {
		return
	}
	m.savedAt = time.Now()
	cut := time.Now().AddDate(0, 0, -trafficDays).Format(time.DateOnly)
	for name := range m.dirty {
		tf := m.traffic[name]
		for d := range tf.Days {
			if d < cut {
				delete(tf.Days, d)
			}
		}
		if _, err := os.Stat(m.s.stateDir(name)); err != nil {
			delete(m.traffic, name) // revoked or renamed meanwhile
			continue
		}
		if err := writeJSON(m.s.trafficPath(name), tf); err != nil {
			log.Printf("web: traffic of %s: %v", name, err)
		}
	}
	m.dirty = map[string]bool{}
}

// clientTraffic is a client's traffic today, this month and over the
// days kept.
type clientTraffic struct {
	Today, Month, All dayTraffic
	Rates             []ratePoint
}

func (m *sampler) clientTraffic(name string, now time.Time) clientTraffic {
	m.mu.Lock()
	defer m.mu.Unlock()
	tf := m.trafficOf(name)
	var ct clientTraffic
	today, month := now.Format(time.DateOnly), now.Format("2006-01")
	for d, t := range tf.Days {
		ct.All.Down += t.Down
		ct.All.Up += t.Up
		if d[:7] == month {
			ct.Month.Down += t.Down
			ct.Month.Up += t.Up
		}
		if d == today {
			ct.Today = t
		}
	}
	ct.Rates = append([]ratePoint(nil), m.rates[name]...)
	return ct
}

// snapshot is the host now and its history.
func (m *sampler) snapshot() (hostNow, []hostPoint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now, append([]hostPoint(nil), m.host...)
}
