//go:build darwin

package linkset

import (
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"
)

// System reports every interface the OS knows about, unfiltered. Feed the
// result to Discover to get the usable subset.
func System() ([]Raw, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	constrained := constrainedSet()

	out := make([]Raw, 0, len(ifaces))
	for _, ifc := range ifaces {
		r := Raw{
			Name:        ifc.Name,
			Index:       ifc.Index,
			Up:          ifc.Flags&net.FlagUp != 0,
			Loopback:    ifc.Flags&net.FlagLoopback != 0,
			Constrained: constrained[ifc.Name],
		}
		// An interface that refuses to list its addresses is reported with
		// none, and Discover will drop it. Not worth failing the whole scan.
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				n, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					r.Addrs = append(r.Addrs, ip.Unmap())
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// FriendlyNames maps a BSD device name to the name macOS shows a human, e.g.
// en0 -> "Wi-Fi". Best effort: a missing name is cosmetic, never fatal.
func FriendlyNames() map[string]string {
	b, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return map[string]string{}
	}
	return parseFriendlyNames(string(b))
}

var (
	reService  = regexp.MustCompile(`^\(\d+\)\s+(.*\S)\s*$`)
	reHardware = regexp.MustCompile(`^\(Hardware Port:\s*(.*?),\s*Device:\s*(.*?)\)\s*$`)
)

// parseFriendlyNames reads `networksetup -listnetworkserviceorder` output,
// which alternates a service line with its hardware-port line:
//
//	(4) Wi-Fi
//	(Hardware Port: Wi-Fi, Device: en0)
//
// The service name is preferred over the hardware-port name because that is
// what the user sees in System Settings, and the two can differ.
func parseFriendlyNames(out string) map[string]string {
	names := map[string]string{}
	pending := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)

		if m := reHardware.FindStringSubmatch(line); m != nil {
			// VPN services report an empty device; an empty key would collide
			// with every unnamed interface.
			if dev := m[2]; dev != "" {
				name := pending
				if name == "" {
					name = m[1]
				}
				names[dev] = name
			}
			pending = ""
			continue
		}
		if m := reService.FindStringSubmatch(line); m != nil {
			// A leading "*" marks a disabled service; keep the name, drop the marker.
			pending = strings.TrimSpace(strings.TrimPrefix(m[1], "*"))
		}
	}
	return names
}

// parseConstrained reports whether an interface's ifconfig block carries
// Darwin's "constrained" flag, which is the OS marking the link as metered.
// Only the flags line counts, so the word appearing elsewhere is ignored.
func parseConstrained(ifconfigBlock string) bool {
	first, _, _ := strings.Cut(ifconfigBlock, "\n")
	for _, field := range strings.Fields(first) {
		if field == "constrained" {
			return true
		}
	}
	return false
}

// constrainedSet parses one `ifconfig` invocation into per-interface metered
// flags, rather than exec'ing once per interface.
func constrainedSet() map[string]bool {
	flags := map[string]bool{}
	b, err := exec.Command("ifconfig").Output()
	if err != nil {
		return flags
	}

	name := ""
	var block strings.Builder
	flush := func() {
		if name != "" {
			flags[name] = parseConstrained(block.String())
		}
		block.Reset()
	}

	for _, line := range strings.Split(string(b), "\n") {
		// A block header starts at column zero and names the interface.
		if line != "" && line[0] != '\t' && line[0] != ' ' && strings.Contains(line, ":") {
			flush()
			name, _, _ = strings.Cut(line, ":")
		}
		block.WriteString(line)
		block.WriteString("\n")
	}
	flush()
	return flags
}
