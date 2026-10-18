// Package routing decides, connection by connection, whether a client's
// traffic goes through the tunnel, straight out of the device, or nowhere.
// The lists it decides by (sites and addresses by category) come in one
// data file, built from public sources by cmd/routing-data.
package routing

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"
)

// The data file is gzip-compressed text:
//
//	reflux-routing 1
//	generated 2026-10-18T08:17:03Z
//	@site category-ru
//	s example.ru          the domain and its subdomains
//	f host.example.ru     the exact name
//	k keyword             any name containing it
//	r ^re(gexp)?$         any name matching it
//	@ip ru
//	1.2.3.0/24
//
// Names are lowercase; categories hold up to 64 site lists.
const dataMagic = "reflux-routing 1"

// Data is a loaded data file.
type Data struct {
	Generated string

	sites map[string]int // category -> bit
	// names are the s and f entries of every category, sorted, in one
	// blob: an entry is blob[offs[i]:offs[i+1]]. suffix and full are the
	// categories (bits) that hold it as a domain or as an exact name.
	blob         string
	offs         []uint32
	suffix, full []uint64
	keywords     []pattern
	regexps      []pattern

	ips map[string]*ipSet
}

type pattern struct {
	bit uint64
	kw  string
	re  *regexp.Regexp
}

// LoadFile loads a data file.
func LoadFile(path string) (*Data, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f)
}

// Load reads a data file.
func Load(r io.Reader) (*Data, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("routing data: %w", err)
	}
	defer zr.Close()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != dataMagic {
		return nil, errors.New("routing data: not a routing data file")
	}
	d := &Data{sites: map[string]int{}, ips: map[string]*ipSet{}}
	names := map[string][2]uint64{}
	var bit uint64
	var ips *ipSet
	inSite := false
	for n := 2; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "generated "):
			d.Generated = strings.TrimPrefix(line, "generated ")
			continue
		case strings.HasPrefix(line, "@site "):
			name := strings.TrimSpace(strings.TrimPrefix(line, "@site "))
			i, ok := d.sites[name]
			if !ok {
				if len(d.sites) == 64 {
					return nil, fmt.Errorf("routing data:%d: more than 64 site lists", n)
				}
				i = len(d.sites)
				d.sites[name] = i
			}
			bit, inSite, ips = 1<<uint(i), true, nil
			continue
		case strings.HasPrefix(line, "@ip "):
			name := strings.TrimSpace(strings.TrimPrefix(line, "@ip "))
			if d.ips[name] == nil {
				d.ips[name] = &ipSet{}
			}
			ips, inSite = d.ips[name], false
			continue
		}
		switch {
		case inSite && len(line) > 2 && line[1] == ' ':
			v := strings.ToLower(strings.TrimSpace(line[2:]))
			switch line[0] {
			case 's':
				e := names[v]
				e[0] |= bit
				names[v] = e
			case 'f':
				e := names[v]
				e[1] |= bit
				names[v] = e
			case 'k':
				d.keywords = append(d.keywords, pattern{bit: bit, kw: v})
			case 'r':
				re, err := regexp.Compile(line[2:])
				if err != nil {
					continue // one bad pattern is not worth the whole file
				}
				d.regexps = append(d.regexps, pattern{bit: bit, re: re})
			}
		case ips != nil:
			p, err := netip.ParsePrefix(line)
			if err != nil {
				a, aerr := netip.ParseAddr(line)
				if aerr != nil {
					continue
				}
				p = netip.PrefixFrom(a, a.BitLen())
			}
			ips.add(p)
		default:
			return nil, fmt.Errorf("routing data:%d: %q outside a list", n, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("routing data: %w", err)
	}
	keys := make([]string, 0, len(names))
	size := 0
	for k := range names {
		keys = append(keys, k)
		size += len(k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.Grow(size)
	d.offs = make([]uint32, 0, len(keys)+1)
	d.suffix = make([]uint64, len(keys))
	d.full = make([]uint64, len(keys))
	for i, k := range keys {
		d.offs = append(d.offs, uint32(b.Len()))
		b.WriteString(k)
		d.suffix[i], d.full[i] = names[k][0], names[k][1]
	}
	d.offs = append(d.offs, uint32(b.Len()))
	d.blob = b.String()
	for _, s := range d.ips {
		s.finish()
	}
	return d, nil
}

func (d *Data) name(i int) string { return d.blob[d.offs[i]:d.offs[i+1]] }

// find returns the index of an exact name, or -1.
func (d *Data) find(s string) int {
	n := len(d.offs) - 1
	i := sort.Search(n, func(i int) bool { return d.name(i) >= s })
	if i < n && d.name(i) == s {
		return i
	}
	return -1
}

// siteMask is the set of site lists host is in.
func (d *Data) siteMask(host string) uint64 {
	if d == nil || host == "" {
		return 0
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	var m uint64
	if i := d.find(host); i >= 0 {
		m |= d.full[i] | d.suffix[i]
	}
	for s := host; ; {
		dot := strings.IndexByte(s, '.')
		if dot < 0 {
			break
		}
		s = s[dot+1:]
		if i := d.find(s); i >= 0 {
			m |= d.suffix[i]
		}
	}
	for _, p := range d.keywords {
		if m&p.bit == 0 && strings.Contains(host, p.kw) {
			m |= p.bit
		}
	}
	for _, p := range d.regexps {
		if m&p.bit == 0 && p.re.MatchString(host) {
			m |= p.bit
		}
	}
	return m
}

// mask is the bits of these site lists; lists the file lacks count for
// nothing.
func (d *Data) mask(lists ...string) uint64 {
	if d == nil {
		return 0
	}
	var m uint64
	for _, l := range lists {
		if i, ok := d.sites[l]; ok {
			m |= 1 << uint(i)
		}
	}
	return m
}

// InSite reports whether host is in the site list.
func (d *Data) InSite(list, host string) bool {
	m := d.mask(list)
	return m != 0 && d.siteMask(host)&m != 0
}

// InIP reports whether addr is in the address list.
func (d *Data) InIP(list string, addr netip.Addr) bool {
	if d == nil {
		return false
	}
	s := d.ips[list]
	return s != nil && s.contains(addr)
}

// Counts says how many entries the data has, for the log.
func (d *Data) Counts() (names, ranges int) {
	if d == nil {
		return 0, 0
	}
	for _, s := range d.ips {
		ranges += len(s.lo)
	}
	return len(d.offs) - 1, ranges
}

// ipSet is IPv4 ranges, merged and sorted once loaded.
type ipSet struct {
	lo, hi []uint32
}

func (s *ipSet) add(p netip.Prefix) {
	p = p.Masked()
	if !p.Addr().Is4() {
		return
	}
	a := p.Addr().As4()
	lo := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	hi := lo | uint32(uint64(1)<<(32-uint(p.Bits()))-1)
	s.lo, s.hi = append(s.lo, lo), append(s.hi, hi)
}

func (s *ipSet) finish() {
	idx := make([]int, len(s.lo))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return s.lo[idx[a]] < s.lo[idx[b]] })
	var lo, hi []uint32
	for _, i := range idx {
		if n := len(lo); n > 0 && (hi[n-1] == ^uint32(0) || s.lo[i] <= hi[n-1]+1) {
			if s.hi[i] > hi[n-1] {
				hi[n-1] = s.hi[i]
			}
			continue
		}
		lo, hi = append(lo, s.lo[i]), append(hi, s.hi[i])
	}
	s.lo, s.hi = lo, hi
}

func (s *ipSet) contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.Is4() {
		return false
	}
	a := addr.As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	i := sort.Search(len(s.lo), func(i int) bool { return s.lo[i] > v }) - 1
	return i >= 0 && v <= s.hi[i]
}
