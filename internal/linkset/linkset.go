// Package linkset decides which network interfaces can carry an uplink.
//
// Discovery is split in two: Discover is pure and takes already-gathered Raw
// records, so every filtering rule is testable without touching hardware. The
// OS-facing half lives in system_darwin.go.
package linkset

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Family is an IP address family. A link may have one, both, or neither —
// USB tethering on this Mac offered IPv4 only, while the same phone's Wi-Fi
// hotspot offered both, so nothing may assume dual-stack.
type Family int

const (
	Fam4 Family = 4
	Fam6 Family = 6
)

func (f Family) String() string { return fmt.Sprintf("IPv%d", int(f)) }

// Network is the Go dial network for this family. Dialing "tcp4" makes the
// resolver return A records only, which keeps a v4-only link from ever trying
// to reach an origin over IPv6.
func (f Family) Network() string {
	if f == Fam6 {
		return "tcp6"
	}
	return "tcp4"
}

// Raw is one interface as the operating system describes it, before any
// judgement about whether it is usable.
type Raw struct {
	Name     string
	Index    int
	Up       bool
	Loopback bool
	Addrs    []netip.Addr
	// Constrained mirrors Darwin's "constrained" interface flag, which is the
	// OS marking a link as metered. More reliable than guessing from the name.
	Constrained bool
}

// Link is an interface we are willing to send traffic on.
type Link struct {
	Iface    string
	Friendly string
	Index    int
	V4, V6   netip.Addr
	Metered  bool
}

// Addr returns the link's address in one family, and whether it has one.
func (l Link) Addr(f Family) (netip.Addr, bool) {
	a := l.V4
	if f == Fam6 {
		a = l.V6
	}
	return a, a.IsValid()
}

// Families lists the families this link can actually use, IPv4 first.
func (l Link) Families() []Family {
	var out []Family
	if l.V4.IsValid() {
		out = append(out, Fam4)
	}
	if l.V6.IsValid() {
		out = append(out, Fam6)
	}
	return out
}

// Label is the name to show a human.
func (l Link) Label() string {
	if l.Friendly != "" {
		return l.Friendly
	}
	return l.Iface
}

// skipPrefixes never carry an uplink: loopback, tunnels, Thunderbolt bridges,
// Apple Wireless Direct, low-latency WLAN, and assorted tunnel shims.
var skipPrefixes = []string{"lo", "utun", "bridge", "awdl", "llw", "gif", "stf", "ap", "anpi", "vmenet"}

// meteredHints match friendly names of links that cost money per byte. Note
// the absence of a bare "usb": a "USB 10/100 LAN" adapter is not metered.
var meteredHints = []string{"iphone", "ipad", "android", "tether", "hotspot", "bluetooth"}

// Discover selects the usable links from raw OS records, newest-sorted by
// interface index so output is stable between calls.
func Discover(raws []Raw, friendly map[string]string) []Link {
	var out []Link
	for _, r := range raws {
		if !r.Up || r.Loopback || hasSkipPrefix(r.Name) {
			continue
		}

		l := Link{Iface: r.Name, Index: r.Index, Friendly: friendly[r.Name]}
		for _, a := range r.Addrs {
			a = a.Unmap()
			// A link-local or self-assigned address means the link came up but
			// nothing configured it — there is no route out of it.
			if !a.IsGlobalUnicast() || a.IsLinkLocalUnicast() {
				continue
			}
			if a.Is4() && !l.V4.IsValid() {
				l.V4 = a
			}
			if a.Is6() && !l.V6.IsValid() {
				l.V6 = a
			}
		}
		if !l.V4.IsValid() && !l.V6.IsValid() {
			continue
		}

		l.Metered = r.Constrained || nameSuggestsMetered(l.Friendly)
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func hasSkipPrefix(name string) bool {
	for _, p := range skipPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func nameSuggestsMetered(friendly string) bool {
	lower := strings.ToLower(friendly)
	for _, h := range meteredHints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}
