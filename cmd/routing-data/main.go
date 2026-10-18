// routing-data builds the routing package's data file from public lists:
// the v2ray geosite.dat and geoip.dat (runetfreedom/russia-v2ray-rules-dat:
// the community categories plus ru-blocked and ru-available-only-inside)
// and the mobile internet whitelists (ancored/ru-mobile-whitelist: names
// and addresses). Only the lists the routing modes use go in.
//
//	routing-data -geosite geosite.dat -geoip geoip.dat \
//	    -whitelist-sni sni.txt -whitelist-ip ip.txt -o routing.dat.gz
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/routing"
)

// The geosite and geoip lists that go in, as named in the .dat files.
var (
	siteLists = []string{
		routing.SiteRussia, routing.SiteGovRussia, routing.SiteOnlyInside,
		routing.SiteBlocked, routing.SiteAds, routing.SiteWinSpy, routing.SitePrivate,
	}
	ipLists = []string{routing.IPRussia, routing.IPPrivate}
)

func main() {
	geosite := flag.String("geosite", "", "geosite.dat")
	geoip := flag.String("geoip", "", "geoip.dat")
	sni := flag.String("whitelist-sni", "", "whitelisted names, one per line")
	ips := flag.String("whitelist-ip", "", "whitelisted addresses and CIDRs, one per line")
	out := flag.String("o", "routing.dat.gz", "output file")
	flag.Parse()
	if *geosite == "" || *geoip == "" || *sni == "" || *ips == "" {
		flag.Usage()
		os.Exit(2)
	}
	var b bytes.Buffer
	if err := build(&b, *geosite, *geoip, *sni, *ips, time.Now().UTC()); err != nil {
		log.Fatal(err)
	}
	var z bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&z, gzip.BestCompression)
	zw.Write(b.Bytes())
	zw.Close()
	// Check that it loads before it replaces anything.
	d, err := routing.Load(bytes.NewReader(z.Bytes()))
	if err != nil {
		log.Fatalf("the built file does not load: %v", err)
	}
	if err := os.WriteFile(*out, z.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	names, ranges := d.Counts()
	log.Printf("%s: %d names, %d address ranges, %d bytes", *out, names, ranges, z.Len())
}

func build(w io.Writer, geositePath, geoipPath, sniPath, ipPath string, now time.Time) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "reflux-routing 1\ngenerated %s\n", now.Format(time.RFC3339))

	raw, err := os.ReadFile(geositePath)
	if err != nil {
		return err
	}
	sites, err := readGeoSite(raw, siteLists)
	if err != nil {
		return fmt.Errorf("%s: %w", geositePath, err)
	}
	for _, name := range siteLists {
		entries, ok := sites[name]
		if !ok || len(entries) == 0 {
			return fmt.Errorf("%s: no list %q", geositePath, name)
		}
		fmt.Fprintf(bw, "@site %s\n", name)
		for _, e := range entries {
			fmt.Fprintf(bw, "%c %s\n", e.kind, e.value)
		}
	}
	names, err := readLines(sniPath)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("%s: empty", sniPath)
	}
	fmt.Fprintf(bw, "@site %s\n", routing.SiteWhitelist)
	for _, n := range names {
		n = strings.Trim(strings.ToLower(n), ".")
		if strings.HasPrefix(n, "*.") {
			n = n[2:]
		}
		if n != "" && !strings.ContainsAny(n, " \t/") {
			fmt.Fprintf(bw, "s %s\n", n)
		}
	}

	raw, err = os.ReadFile(geoipPath)
	if err != nil {
		return err
	}
	geo, err := readGeoIP(raw, ipLists)
	if err != nil {
		return fmt.Errorf("%s: %w", geoipPath, err)
	}
	for _, name := range ipLists {
		if len(geo[name]) == 0 {
			return fmt.Errorf("%s: no list %q", geoipPath, name)
		}
		fmt.Fprintf(bw, "@ip %s\n", name)
		for _, p := range geo[name] {
			fmt.Fprintln(bw, p)
		}
	}
	lines, err := readLines(ipPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(bw, "@ip %s\n", routing.IPWhitelist)
	n := 0
	for _, l := range lines {
		if p, err := netip.ParsePrefix(l); err == nil && p.Addr().Is4() {
			fmt.Fprintln(bw, p.Masked())
			n++
		} else if a, err := netip.ParseAddr(l); err == nil && a.Is4() {
			fmt.Fprintln(bw, a)
			n++
		}
	}
	if n == 0 {
		return fmt.Errorf("%s: no IPv4 addresses", ipPath)
	}
	return bw.Flush()
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// ---- the v2ray .dat files (protobuf) ----
//
//	GeoSiteList { repeated GeoSite entry = 1; }
//	GeoSite     { string country_code = 1; repeated Domain domain = 2; }
//	Domain      { Type type = 1; string value = 2; ... }  Plain=0 Regex=1 RootDomain=2 Full=3
//	GeoIPList   { repeated GeoIP entry = 1; }
//	GeoIP       { string country_code = 1; repeated CIDR cidr = 2; }
//	CIDR        { bytes ip = 1; uint32 prefix = 2; }

type siteEntry struct {
	kind  byte // s, f, k, r
	value string
}

var errProto = errors.New("malformed protobuf")

// fields calls f for each field of a message: its number, wire type, the
// varint value or the bytes.
func fields(b []byte, f func(num int, typ int, v uint64, data []byte) error) error {
	for len(b) > 0 {
		key, n := uvarint(b)
		if n <= 0 {
			return errProto
		}
		b = b[n:]
		num, typ := int(key>>3), int(key&7)
		switch typ {
		case 0:
			v, n := uvarint(b)
			if n <= 0 {
				return errProto
			}
			b = b[n:]
			if err := f(num, typ, v, nil); err != nil {
				return err
			}
		case 2:
			l, n := uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return errProto
			}
			data := b[n : n+int(l)]
			b = b[n+int(l):]
			if err := f(num, typ, 0, data); err != nil {
				return err
			}
		case 1:
			if len(b) < 8 {
				return errProto
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return errProto
			}
			b = b[4:]
		default:
			return errProto
		}
	}
	return nil
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

func readGeoSite(raw []byte, want []string) (map[string][]siteEntry, error) {
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[strings.ToUpper(w)] = true
	}
	out := map[string][]siteEntry{}
	err := fields(raw, func(num, typ int, _ uint64, entry []byte) error {
		if num != 1 || typ != 2 {
			return nil
		}
		var code string
		var domains [][]byte
		if err := fields(entry, func(num, typ int, _ uint64, data []byte) error {
			switch {
			case num == 1 && typ == 2:
				code = string(data)
			case num == 2 && typ == 2:
				domains = append(domains, data)
			}
			return nil
		}); err != nil {
			return err
		}
		if !wanted[strings.ToUpper(code)] {
			return nil
		}
		list := make([]siteEntry, 0, len(domains))
		for _, d := range domains {
			var kind uint64
			var value string
			if err := fields(d, func(num, typ int, v uint64, data []byte) error {
				switch {
				case num == 1 && typ == 0:
					kind = v
				case num == 2 && typ == 2:
					value = string(data)
				}
				return nil
			}); err != nil {
				return err
			}
			if value == "" || strings.ContainsAny(value, "\n\r") {
				continue
			}
			k := map[uint64]byte{0: 'k', 1: 'r', 2: 's', 3: 'f'}[kind]
			if k == 0 {
				continue
			}
			if k != 'r' {
				value = strings.ToLower(value)
			}
			list = append(list, siteEntry{k, value})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].value < list[j].value })
		out[strings.ToLower(code)] = list
		return nil
	})
	return out, err
}

func readGeoIP(raw []byte, want []string) (map[string][]netip.Prefix, error) {
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[strings.ToUpper(w)] = true
	}
	out := map[string][]netip.Prefix{}
	err := fields(raw, func(num, typ int, _ uint64, entry []byte) error {
		if num != 1 || typ != 2 {
			return nil
		}
		var code string
		var cidrs [][]byte
		if err := fields(entry, func(num, typ int, _ uint64, data []byte) error {
			switch {
			case num == 1 && typ == 2:
				code = string(data)
			case num == 2 && typ == 2:
				cidrs = append(cidrs, data)
			}
			return nil
		}); err != nil {
			return err
		}
		if !wanted[strings.ToUpper(code)] {
			return nil
		}
		for _, c := range cidrs {
			var ip []byte
			var bits uint64
			if err := fields(c, func(num, typ int, v uint64, data []byte) error {
				switch {
				case num == 1 && typ == 2:
					ip = data
				case num == 2 && typ == 0:
					bits = v
				}
				return nil
			}); err != nil {
				return err
			}
			if len(ip) != 4 || bits > 32 {
				continue // IPv6: the tunnel is IPv4 only
			}
			a := netip.AddrFrom4([4]byte(ip))
			out[strings.ToLower(code)] = append(out[strings.ToLower(code)], netip.PrefixFrom(a, int(bits)).Masked())
		}
		return nil
	})
	return out, err
}
