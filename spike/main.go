//go:build darwin

// Phase 0 measurement spike for braid.
//
// Throwaway code. Its only job is to answer four questions before anything is
// built on top of them:
//
//  1. Does Darwin's IP_BOUND_IF / IPV6_BOUND_IF actually pin a socket to one
//     interface? Verified against the kernel's per-interface byte counters,
//     not by trusting that setsockopt returned nil.
//  2. What does each link actually deliver, alone?
//  3. Do two links together deliver meaningfully more than the faster one?
//  4. Is the cellular link IPv6-only?
//
// Subcommands: links | measure | ranges | reach
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Verified against /Applications/Xcode.app/.../MacOSX.sdk/usr/include on
// 2026-09-15: netinet/in.h:432 and netinet6/in6.h:506.
const (
	ipBoundIf   = 25
	ipv6BoundIf = 125
)

// The agent sandbox kills very large single transfers, so the spike issues
// repeated 20 MB requests instead of one huge one. 20 MB is proven to work.
const (
	payloadURL   = "https://speed.cloudflare.com/__down?bytes=20000000"
	perLinkDur   = 8 * time.Second
	aggregateDur = 12 * time.Second
	workers      = 4
)

type family int

const (
	fam4 family = 4
	fam6 family = 6
)

func (f family) String() string { return fmt.Sprintf("IPv%d", int(f)) }

func (f family) network() string {
	if f == fam6 {
		return "tcp6"
	}
	return "tcp4"
}

type link struct {
	name     string
	friendly string
	index    int
	v4, v6   netip.Addr
}

func (l link) addr(f family) (netip.Addr, bool) {
	a := l.v4
	if f == fam6 {
		a = l.v6
	}
	return a, a.IsValid()
}

func (l link) families() []family {
	var out []family
	if l.v4.IsValid() {
		out = append(out, fam4)
	}
	if l.v6.IsValid() {
		out = append(out, fam6)
	}
	return out
}

func (l link) label() string {
	if l.friendly == "" || l.friendly == l.name {
		return l.name
	}
	return fmt.Sprintf("%s (%s)", l.friendly, l.name)
}

// ---------------------------------------------------------------- discovery

// skipPrefixes are interfaces that are never a real uplink: tunnels, bridges,
// Apple Wireless Direct, low-latency WLAN, and the various tunnel shims.
var skipPrefixes = []string{"lo", "utun", "bridge", "awdl", "llw", "gif", "stf", "ap", "anpi", "vmenet"}

func discover() ([]link, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	friendly := friendlyNames()

	var out []link
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		skip := false
		for _, p := range skipPrefixes {
			if strings.HasPrefix(ifc.Name, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		l := link{name: ifc.Name, index: ifc.Index, friendly: friendly[ifc.Name]}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			pfx, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(pfx.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			// Only globally routable addresses can carry an uplink.
			if !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.Is4() && !l.v4.IsValid() {
				l.v4 = ip
			}
			if ip.Is6() && !l.v6.IsValid() {
				l.v6 = ip
			}
		}
		if l.v4.IsValid() || l.v6.IsValid() {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out, nil
}

var (
	reService  = regexp.MustCompile(`^\(\d+\)\s+(.+?)\s*$`)
	reHardware = regexp.MustCompile(`^\(Hardware Port:\s*(.*?),\s*Device:\s*(.*?)\)\s*$`)
)

// friendlyNames maps en0 -> "Wi-Fi" using the service order macOS reports.
// Best effort: a missing name is cosmetic, never fatal.
func friendlyNames() map[string]string {
	out := map[string]string{}
	b, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	pending := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := reHardware.FindStringSubmatch(line); m != nil {
			if dev := m[2]; dev != "" {
				name := pending
				if name == "" {
					name = m[1]
				}
				out[dev] = name
			}
			pending = ""
			continue
		}
		if m := reService.FindStringSubmatch(line); m != nil {
			pending = strings.TrimPrefix(m[1], "* ")
		}
	}
	return out
}

// ------------------------------------------------------------ pinned dialing

// transport builds an http.Transport whose every connection is forced onto one
// interface, in one address family. Two mechanisms are applied together:
// LocalAddr fixes the source address, and *_BOUND_IF forces the kernel to use
// that interface's scoped route regardless of the system default route. The
// second is the one that actually matters, and the one plexo omits.
func transport(l link, f family) (*http.Transport, error) {
	src, ok := l.addr(f)
	if !ok {
		return nil, fmt.Errorf("%s has no %s address", l.name, f)
	}
	idx := l.index
	net_ := f.network()

	d := &net.Dialer{
		Timeout:   10 * time.Second,
		LocalAddr: &net.TCPAddr{IP: src.AsSlice()},
		Control: func(network, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				switch network {
				case "tcp4":
					serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBoundIf, idx)
				case "tcp6":
					serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6BoundIf, idx)
				default:
					serr = fmt.Errorf("unexpected network %q", network)
				}
			}); err != nil {
				return err
			}
			return serr
		},
	}

	return &http.Transport{
		// Pin the family too: dialing "tcp4" makes Go resolve A records only,
		// so a v6-only link never tries to reach a v4-only origin.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return d.DialContext(ctx, net_, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   workers,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		DisableCompression:    true,
	}, nil
}

// --------------------------------------------------------- kernel counters

type counters struct{ in, out uint64 }

// ifBytes reads the kernel's own byte counters for an interface. This is the
// spike's ground truth: if we ask for en5 and the bytes land on en0, pinning
// is a lie no matter what setsockopt said.
//
// Fields are counted from the end of the line because the Address column is
// absent on some interfaces, which would shift any fixed index.
func ifBytes(iface string) (counters, error) {
	b, err := exec.Command("netstat", "-I", iface, "-b").Output()
	if err != nil {
		return counters{}, err
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 8 || fs[0] != iface {
			continue
		}
		if !strings.HasPrefix(fs[2], "<Link") {
			continue
		}
		n := len(fs)
		in, err1 := strconv.ParseUint(fs[n-5], 10, 64)
		out, err2 := strconv.ParseUint(fs[n-2], 10, 64)
		if err1 != nil || err2 != nil {
			return counters{}, fmt.Errorf("unparsable netstat row for %s: %v", iface, fs)
		}
		return counters{in: in, out: out}, nil
	}
	return counters{}, fmt.Errorf("no <Link> row for %s", iface)
}

func snapshot(links []link) map[string]counters {
	m := map[string]counters{}
	for _, l := range links {
		if c, err := ifBytes(l.name); err == nil {
			m[l.name] = c
		}
	}
	return m
}

func deltas(before, after map[string]counters) map[string]uint64 {
	d := map[string]uint64{}
	for name, a := range after {
		if b, ok := before[name]; ok && a.in >= b.in {
			d[name] = a.in - b.in
		}
	}
	return d
}

// ------------------------------------------------------------- measurement

// pull hammers payloadURL over tr for dur and returns bytes received.
func pull(ctx context.Context, tr *http.Transport, dur time.Duration) (int64, error) {
	client := &http.Client{Transport: tr}
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	var total int64
	var firstErr error
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 256<<10)
			for ctx.Err() == nil {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, payloadURL, nil)
				if err != nil {
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
					}
					return
				}
				for {
					n, rerr := resp.Body.Read(buf)
					if n > 0 {
						atomic.AddInt64(&total, int64(n))
					}
					if rerr != nil {
						break
					}
				}
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	n := atomic.LoadInt64(&total)
	if n == 0 && firstErr != nil {
		return 0, firstErr
	}
	return n, nil
}

func mbps(b int64, d time.Duration) float64 {
	return float64(b) * 8 / d.Seconds() / 1e6
}

func human(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

// ------------------------------------------------------------ subcommands

func cmdLinks() error {
	links, err := discover()
	if err != nil {
		return err
	}
	if len(links) == 0 {
		return fmt.Errorf("no candidate uplinks found")
	}
	fmt.Printf("%-22s %-6s %-5s %-16s %s\n", "LINK", "IFACE", "IDX", "IPv4", "IPv6")
	for _, l := range links {
		v4, v6 := "-", "-"
		if l.v4.IsValid() {
			v4 = l.v4.String()
		}
		if l.v6.IsValid() {
			v6 = l.v6.String()
		}
		name := l.friendly
		if name == "" {
			name = "?"
		}
		fmt.Printf("%-22s %-6s %-5d %-16s %s\n", name, l.name, l.index, v4, v6)
	}
	return nil
}

type result struct {
	l        link
	f        family
	bytes    int64
	dur      time.Duration
	deltas   map[string]uint64
	err      error
	pinnedOK bool
	pinNote  string
}

func measureOne(ctx context.Context, links []link, l link, f family) result {
	r := result{l: l, f: f}
	tr, err := transport(l, f)
	if err != nil {
		r.err = err
		return r
	}

	before := snapshot(links)
	start := time.Now()
	r.bytes, r.err = pull(ctx, tr, perLinkDur)
	r.dur = time.Since(start)
	r.deltas = deltas(before, snapshot(links))

	// Ground truth: most of the traffic must have arrived on the interface we
	// pinned to. Other interfaces always carry some background noise, so the
	// test is "the pinned link carried the clear majority", not equality.
	var onPinned, onOthers uint64
	for name, d := range r.deltas {
		if name == l.name {
			onPinned = d
		} else {
			onOthers += d
		}
	}
	switch {
	case r.bytes == 0:
		r.pinNote = "no data transferred"
	case onPinned >= uint64(float64(r.bytes)*0.8):
		r.pinnedOK = true
		r.pinNote = fmt.Sprintf("%s on %s", human(onPinned), l.name)
	default:
		r.pinNote = fmt.Sprintf("PINNING FAILED: %s downloaded but only %s on %s (%s elsewhere)",
			human(uint64(r.bytes)), human(onPinned), l.name, human(onOthers))
	}
	return r
}

func cmdMeasure() error {
	links, err := discover()
	if err != nil {
		return err
	}
	if err := cmdLinks(); err != nil {
		return err
	}
	fmt.Println()

	if len(links) < 2 {
		fmt.Printf("!! Only %d usable uplink found. The aggregate test needs two.\n", len(links))
		fmt.Println("!! Plug the iPhone in over USB with Personal Hotspot on, then re-run.")
		fmt.Println("!! Continuing with the single-link tests so pinning is still verified.")
	}

	// ---- per-link, per-family
	fmt.Println("== Per-link throughput, pinned with *_BOUND_IF ==")
	best := map[string]float64{}
	var results []result
	for _, l := range links {
		for _, f := range l.families() {
			r := measureOne(context.Background(), links, l, f)
			results = append(results, r)
			if r.err != nil {
				fmt.Printf("  %-28s %-5s  ERROR: %v\n", l.label(), f, r.err)
				continue
			}
			rate := mbps(r.bytes, r.dur)
			if rate > best[l.name] {
				best[l.name] = rate
			}
			flag := "ok  "
			if !r.pinnedOK {
				flag = "FAIL"
			}
			fmt.Printf("  %-28s %-5s %7.1f Mbps  %-9s [%s] %s\n",
				l.label(), f, rate, human(uint64(r.bytes)), flag, r.pinNote)
		}
	}

	// ---- aggregate
	if len(links) >= 2 {
		fmt.Println("\n== All links concurrently ==")
		before := snapshot(links)
		var wg sync.WaitGroup
		type agg struct {
			l     link
			f     family
			bytes int64
			err   error
		}
		aggs := make([]agg, 0, len(links))
		var mu sync.Mutex
		start := time.Now()
		for _, l := range links {
			fams := l.families()
			if len(fams) == 0 {
				continue
			}
			// Use whichever family measured faster for this link.
			f := fams[0]
			var bestRate float64
			for _, r := range results {
				if r.l.name == l.name && r.err == nil {
					if rate := mbps(r.bytes, r.dur); rate > bestRate {
						bestRate, f = rate, r.f
					}
				}
			}
			wg.Add(1)
			go func(l link, f family) {
				defer wg.Done()
				tr, err := transport(l, f)
				if err != nil {
					mu.Lock()
					aggs = append(aggs, agg{l: l, f: f, err: err})
					mu.Unlock()
					return
				}
				n, err := pull(context.Background(), tr, aggregateDur)
				mu.Lock()
				aggs = append(aggs, agg{l: l, f: f, bytes: n, err: err})
				mu.Unlock()
			}(l, f)
		}
		wg.Wait()
		dur := time.Since(start)
		d := deltas(before, snapshot(links))

		var total int64
		sort.Slice(aggs, func(i, j int) bool { return aggs[i].l.index < aggs[j].l.index })
		for _, a := range aggs {
			if a.err != nil {
				fmt.Printf("  %-28s %-5s  ERROR: %v\n", a.l.label(), a.f, a.err)
				continue
			}
			total += a.bytes
			fmt.Printf("  %-28s %-5s %7.1f Mbps  (kernel saw %s on %s)\n",
				a.l.label(), a.f, mbps(a.bytes, dur), human(d[a.l.name]), a.l.name)
		}

		var bestAlone float64
		for _, r := range best {
			if r > bestAlone {
				bestAlone = r
			}
		}
		aggRate := mbps(total, dur)
		fmt.Printf("\n  TOTAL %.1f Mbps   best single link alone %.1f Mbps   gain %+.0f%%\n",
			aggRate, bestAlone, (aggRate/bestAlone-1)*100)
		if aggRate < bestAlone*1.15 {
			fmt.Println("  VERDICT: bonding is NOT paying off here. Investigate before building on it.")
		} else {
			fmt.Println("  VERDICT: bonding works. Proceed.")
		}
	}

	allPinned := true
	for _, r := range results {
		if r.err == nil && !r.pinnedOK {
			allPinned = false
		}
	}
	fmt.Printf("\n== Pinning verdict: ")
	if allPinned {
		fmt.Println("IP_BOUND_IF holds on every link tested. ==")
	} else {
		fmt.Println("FAILED on at least one link. braid's whole premise is at risk. ==")
	}
	return nil
}

// cmdRanges checks that real origins honour byte ranges, which is the other
// half of the premise: without 206 responses there is nothing to split.
func cmdRanges() error {
	urls := []string{
		"https://speed.cloudflare.com/__down?bytes=20000000",
		"https://cdn.jsdelivr.net/npm/react@18.3.1/umd/react.production.min.js",
		"https://download.documentfoundation.org/libreoffice/stable/25.2.5/mac/aarch64/LibreOffice_25.2.5_MacOS_aarch64.dmg",
		"https://dl.google.com/go/go1.27.1.darwin-arm64.tar.gz",
	}
	fmt.Printf("%-6s %-10s %-14s %s\n", "STATUS", "RANGES", "SIZE", "URL")
	for _, u := range urls {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Range", "bytes=0-0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Printf("%-6s %-10s %-14s %s  (%v)\n", "ERR", "-", "-", u, err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		ranges, size := "no", "unknown"
		if resp.StatusCode == http.StatusPartialContent {
			ranges = "YES (206)"
			if cr := resp.Header.Get("Content-Range"); cr != "" {
				if i := strings.LastIndex(cr, "/"); i >= 0 {
					if n, err := strconv.ParseUint(cr[i+1:], 10, 64); err == nil {
						size = human(n)
					}
				}
			}
		}
		fmt.Printf("%-6d %-10s %-14s %s\n", resp.StatusCode, ranges, size, u)
	}
	return nil
}

// cmdReach starts a listener so we can find out whether the tethered iPhone
// can reach a service on the Mac. If it cannot, the phone still benefits by
// reaching the daemon over house Wi-Fi instead.
func cmdReach() error {
	links, err := discover()
	if err != nil {
		return err
	}
	const port = 8099
	fmt.Println("Listening on 0.0.0.0:8099 for 90s. Open any of these on the phone:")
	for _, l := range links {
		if l.v4.IsValid() {
			fmt.Printf("    http://%s:%d/   (via %s)\n", l.v4, port, l.label())
		}
	}
	fmt.Println("\nWaiting for a request...")

	hits := make(chan string, 8)
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port)}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits <- fmt.Sprintf("%s  UA=%s", r.RemoteAddr, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "braid spike: the Mac is reachable from this device.")
	})
	go srv.ListenAndServe()
	defer srv.Close()

	deadline := time.After(90 * time.Second)
	for {
		select {
		case h := <-hits:
			fmt.Printf("REACHABLE: request from %s\n", h)
			return nil
		case <-deadline:
			fmt.Println("TIMEOUT: nothing connected in 90s.")
			return nil
		}
	}
}

func main() {
	cmd := "measure"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "links":
		err = cmdLinks()
	case "measure":
		err = cmdMeasure()
	case "ranges":
		err = cmdRanges()
	case "reach":
		err = cmdReach()
	default:
		err = fmt.Errorf("usage: spike [links|measure|ranges|reach]")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
