//go:build darwin

package linkset

import "testing"

// Verbatim output from `networksetup -listnetworkserviceorder` on the machine
// this was developed against. Note service 5: macOS really did name it
// "iPhone USB USB" after re-enumerating the phone, and the hardware port name
// differs from the service name.
const realServiceOrder = `An asterisk (*) denotes that a network service is disabled.
(1) Action Camera
(Hardware Port: Action Camera, Device: en7)

(2) USB 10/100 LAN
(Hardware Port: USB 10/100 LAN, Device: en12)

(3) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)

(4) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(5) iPhone USB USB
(Hardware Port: iPhone USB, Device: en5)

(6) Turbo VPN
(Hardware Port: com.inconnecting.turbovpnformac, Device: )

(7) VPN 1
(Hardware Port: L2TP, Device: )
`

func TestParseFriendlyNames(t *testing.T) {
	got := parseFriendlyNames(realServiceOrder)

	want := map[string]string{
		"en7":     "Action Camera",
		"en12":    "USB 10/100 LAN",
		"bridge0": "Thunderbolt Bridge",
		"en0":     "Wi-Fi",
		"en5":     "iPhone USB USB",
	}
	for dev, name := range want {
		if got[dev] != name {
			t.Errorf("%s = %q, want %q", dev, got[dev], name)
		}
	}
}

func TestParseFriendlyNamesIgnoresServicesWithNoDevice(t *testing.T) {
	// The VPN entries have "Device: " with nothing after it. An empty key would
	// collide with every unnamed interface.
	got := parseFriendlyNames(realServiceOrder)

	if name, ok := got[""]; ok {
		t.Errorf("empty device key present with name %q", name)
	}
	if len(got) != 5 {
		t.Errorf("got %d entries, want 5: %v", len(got), got)
	}
}

func TestParseFriendlyNamesStripsDisabledMarker(t *testing.T) {
	in := `(2) * Old Adapter
(Hardware Port: Old Adapter, Device: en9)
`
	got := parseFriendlyNames(in)
	if got["en9"] != "Old Adapter" {
		t.Errorf("en9 = %q, want %q", got["en9"], "Old Adapter")
	}
}

func TestParseConstrained(t *testing.T) {
	// Verbatim ifconfig output for the tethered iPhone. The trailing
	// "constrained" is macOS telling us the link is metered.
	tethered := `en5: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500 constrained
	options=400<CHANNEL_IO>
	ether 02:00:00:00:00:05
	inet 172.20.10.2 netmask 0xfffffff0 broadcast 172.20.10.15
	status: active`

	wifi := `en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	options=6460<TSO4,TSO6,CHANNEL_IO,PARTIAL_CSUM,ZEROINVERT_CSUM>
	ether 02:00:00:00:00:01
	inet 192.168.1.42 netmask 0xffffff00 broadcast 192.168.1.505
	status: active`

	if !parseConstrained(tethered) {
		t.Error("tethered iPhone should parse as constrained")
	}
	if parseConstrained(wifi) {
		t.Error("Wi-Fi should not parse as constrained")
	}
}

func TestParseConstrainedIgnoresTheWordElsewhere(t *testing.T) {
	// "constrained" must be matched as a flag on the first line, not anywhere
	// it happens to appear.
	in := `en1: flags=8863<UP,BROADCAST> mtu 1500
	description: link is not constrained at all`

	if parseConstrained(in) {
		t.Error("matched 'constrained' outside the flags line")
	}
}

// TestSystemSeesThisMachine is an integration check against real hardware. It
// asserts only what must be true of any working Mac, so it is not brittle.
func TestSystemSeesThisMachine(t *testing.T) {
	raws, err := System()
	if err != nil {
		t.Fatalf("System() error: %v", err)
	}
	if len(raws) == 0 {
		t.Fatal("System() found no interfaces at all")
	}

	links := Discover(raws, FriendlyNames())
	if len(links) == 0 {
		t.Fatal("no usable uplink found; is this machine offline?")
	}
	for _, l := range links {
		if l.Index <= 0 {
			t.Errorf("%s has index %d; the pinning syscall needs a real ifindex", l.Iface, l.Index)
		}
		if !l.V4.IsValid() && !l.V6.IsValid() {
			t.Errorf("%s was accepted with no usable address", l.Iface)
		}
	}
	t.Logf("discovered %d usable link(s):", len(links))
	for _, l := range links {
		t.Logf("  %-16s %-6s idx=%-3d v4=%-16v v6=%v metered=%v",
			l.Label(), l.Iface, l.Index, l.V4, l.V6, l.Metered)
	}
}
