package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// minRUPrefixes guards against applying a truncated or wrong list: the RU
// IPv4 space is several thousand prefixes; far fewer means a broken file.
const minRUPrefixes = 3000

// parseDelegated reads an RIR "delegated" statistics file (for Russia:
// https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest) and
// returns the allocated and assigned IPv4 space of country cc as the
// fewest prefixes that cover it exactly.
//
// A record is registry|cc|type|start|value|date|status[|...]; for ipv4,
// value is a count of addresses and need not be a power of two.
func parseDelegated(r io.Reader, cc string) ([]netip.Prefix, error) {
	var ranges []ipRange
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "|")
		if len(f) < 7 || f[1] != cc || f[2] != "ipv4" {
			continue
		}
		if f[6] != "allocated" && f[6] != "assigned" {
			continue
		}
		start, err := netip.ParseAddr(f[3])
		if err != nil || !start.Is4() {
			return nil, fmt.Errorf("bad start address %q", f[3])
		}
		count, err := strconv.ParseUint(f[4], 10, 32)
		if err != nil || count == 0 {
			return nil, fmt.Errorf("bad address count %q", f[4])
		}
		first := u32(start)
		last := uint64(first) + count - 1
		if last > 0xffffffff {
			return nil, fmt.Errorf("range %s+%d runs past 255.255.255.255", f[3], count)
		}
		ranges = append(ranges, ipRange{first, uint32(last)})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return rangesToPrefixes(mergeRanges(ranges)), nil
}

type ipRange struct{ first, last uint32 }

func u32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

func addr(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// mergeRanges sorts ranges and joins the overlapping and adjacent ones.
func mergeRanges(rs []ipRange) []ipRange {
	sort.Slice(rs, func(i, j int) bool { return rs[i].first < rs[j].first })
	var out []ipRange
	for _, r := range rs {
		if n := len(out); n > 0 && uint64(r.first) <= uint64(out[n-1].last)+1 {
			if r.last > out[n-1].last {
				out[n-1].last = r.last
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// rangesToPrefixes covers each range with the largest aligned blocks.
func rangesToPrefixes(rs []ipRange) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range rs {
		cur := uint64(r.first)
		end := uint64(r.last)
		for cur <= end {
			// The block may be no larger than cur's alignment allows and
			// must not run past the end of the range.
			size := 32
			if cur != 0 {
				size = bits.TrailingZeros32(uint32(cur))
			}
			for size > 0 && cur+(uint64(1)<<size)-1 > end {
				size--
			}
			out = append(out, netip.PrefixFrom(addr(uint32(cur)), 32-size))
			cur += uint64(1) << size
		}
	}
	return out
}

// readPrefixes reads one prefix per line, as writePrefixes writes them.
func readPrefixes(r io.Reader) ([]netip.Prefix, error) {
	var out []netip.Prefix
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := netip.ParsePrefix(line)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("bad prefix %q", line)
		}
		out = append(out, p.Masked())
	}
	return out, sc.Err()
}

func writePrefixes(w io.Writer, ps []netip.Prefix) error {
	bw := bufio.NewWriter(w)
	for _, p := range ps {
		fmt.Fprintln(bw, p)
	}
	return bw.Flush()
}

// diffPrefixes returns what is in next but not in prev, and the reverse.
func diffPrefixes(prev, next []netip.Prefix) (add, del []netip.Prefix) {
	old := make(map[netip.Prefix]bool, len(prev))
	for _, p := range prev {
		old[p] = true
	}
	now := make(map[netip.Prefix]bool, len(next))
	for _, p := range next {
		now[p] = true
		if !old[p] {
			add = append(add, p)
		}
	}
	for _, p := range prev {
		if !now[p] {
			del = append(del, p)
		}
	}
	return add, del
}
