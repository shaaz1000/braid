package linkset

import (
	"net/netip"
	"testing"
)

func addr(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	return a
}

// raw is a convenience for building a usable dual-stack interface in tests.
func raw(name string, index int, addrs ...string) Raw {
	r := Raw{Name: name, Index: index, Up: true}
	for _, s := range addrs {
		r.Addrs = append(r.Addrs, addr(s))
	}
	return r
}

func TestDiscoverKeepsGloballyRoutableInterface(t *testing.T) {
	got := Discover([]Raw{raw("en0", 11, "192.168.1.42", "2001:db8:1::1")}, nil)

	if len(got) != 1 {
		t.Fatalf("want 1 link, got %d: %+v", len(got), got)
	}
	l := got[0]
	if l.Iface != "en0" || l.Index != 11 {
		t.Errorf("want en0/11, got %s/%d", l.Iface, l.Index)
	}
	if l.V4 != addr("192.168.1.42") {
		t.Errorf("V4 = %v", l.V4)
	}
	if l.V6 != addr("2001:db8:1::1") {
		t.Errorf("V6 = %v", l.V6)
	}
}

func TestDiscoverAppliesFriendlyName(t *testing.T) {
	got := Discover([]Raw{raw("en0", 11, "192.168.1.42")}, map[string]string{"en0": "Wi-Fi"})

	if len(got) != 1 {
		t.Fatalf("want 1 link, got %d", len(got))
	}
	if got[0].Friendly != "Wi-Fi" {
		t.Errorf("Friendly = %q, want Wi-Fi", got[0].Friendly)
	}
}

func TestDiscoverSkipsUnusableInterfaces(t *testing.T) {
	// Each of these must be rejected for a different reason. A regression in any
	// one of them would put a dead link into the scheduler's rotation.
	cases := []struct {
		why string
		r   Raw
	}{
		{"down", Raw{Name: "en9", Index: 9, Up: false, Addrs: []netip.Addr{addr("10.0.0.5")}}},
		{"loopback", Raw{Name: "lo0", Index: 1, Up: true, Loopback: true, Addrs: []netip.Addr{addr("127.0.0.1")}}},
		{"tunnel", raw("utun3", 14, "10.8.0.2")},
		{"bridge", raw("bridge0", 15, "10.9.0.2")},
		{"awdl", raw("awdl0", 16, "10.10.0.2")},
		{"no addresses at all", Raw{Name: "en6", Index: 22, Up: true}},
		{"self-assigned only", raw("en14", 23, "169.254.206.108")},
		{"ipv6 link-local only", raw("en5", 21, "fe80::1")},
	}

	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			if got := Discover([]Raw{c.r}, nil); len(got) != 0 {
				t.Errorf("%s should be rejected, got %+v", c.why, got)
			}
		})
	}
}

func TestDiscoverSortsByInterfaceIndex(t *testing.T) {
	got := Discover([]Raw{
		raw("en5", 21, "172.20.10.2"),
		raw("en0", 11, "192.168.1.42"),
	}, nil)

	if len(got) != 2 {
		t.Fatalf("want 2 links, got %d", len(got))
	}
	if got[0].Iface != "en0" || got[1].Iface != "en5" {
		t.Errorf("want en0 then en5, got %s then %s", got[0].Iface, got[1].Iface)
	}
}

func TestMeteredFromConstrainedFlag(t *testing.T) {
	// macOS tags tethered interfaces "constrained"; that is the OS telling us
	// the link is metered, and it beats guessing from the name.
	r := raw("en5", 21, "172.20.10.2")
	r.Constrained = true

	got := Discover([]Raw{r}, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 link, got %d", len(got))
	}
	if !got[0].Metered {
		t.Error("constrained interface should be Metered")
	}
}

func TestMeteredFromFriendlyName(t *testing.T) {
	// Fallback for when the constrained flag is absent. The real service macOS
	// created on this machine was literally named "iPhone USB USB".
	cases := map[string]bool{
		"iPhone USB USB": true,
		"iPhone":         true,
		"Android tether": true,
		"Bluetooth PAN":  true,
		"Wi-Fi":          false,
		"USB 10/100 LAN": false,
		"Thunderbolt 1":  false,
	}

	for name, wantMetered := range cases {
		t.Run(name, func(t *testing.T) {
			got := Discover([]Raw{raw("enX", 30, "10.0.0.2")}, map[string]string{"enX": name})
			if len(got) != 1 {
				t.Fatalf("want 1 link, got %d", len(got))
			}
			if got[0].Metered != wantMetered {
				t.Errorf("%q: Metered = %v, want %v", name, got[0].Metered, wantMetered)
			}
		})
	}
}

func TestLinkFamiliesReportsOnlyWhatItHas(t *testing.T) {
	// A tether can offer IPv4 only for minutes before IPv6 appears, and a
	// different tethering method offers a different set. Nothing may assume
	// dual-stack.
	v4only := Link{V4: addr("172.20.10.2")}
	if got := v4only.Families(); len(got) != 1 || got[0] != Fam4 {
		t.Errorf("v4-only families = %v, want [Fam4]", got)
	}

	v6only := Link{V6: addr("2001:db8:1::1")}
	if got := v6only.Families(); len(got) != 1 || got[0] != Fam6 {
		t.Errorf("v6-only families = %v, want [Fam6]", got)
	}

	both := Link{V4: addr("192.168.1.42"), V6: addr("2001:db8:1::1")}
	if got := both.Families(); len(got) != 2 {
		t.Errorf("dual-stack families = %v, want 2", got)
	}

	none := Link{}
	if got := none.Families(); len(got) != 0 {
		t.Errorf("addressless families = %v, want empty", got)
	}
}

func TestLinkAddrReportsMissingFamily(t *testing.T) {
	l := Link{V4: addr("172.20.10.2")}

	if a, ok := l.Addr(Fam4); !ok || a != addr("172.20.10.2") {
		t.Errorf("Addr(Fam4) = %v, %v", a, ok)
	}
	if _, ok := l.Addr(Fam6); ok {
		t.Error("Addr(Fam6) should report not-ok on a v4-only link")
	}
}

func TestLinkLabelPrefersFriendlyName(t *testing.T) {
	withName := Link{Iface: "en0", Friendly: "Wi-Fi"}
	if got := withName.Label(); got != "Wi-Fi" {
		t.Errorf("Label() = %q, want Wi-Fi", got)
	}

	withoutName := Link{Iface: "en0"}
	if got := withoutName.Label(); got != "en0" {
		t.Errorf("Label() = %q, want en0", got)
	}
}

func TestLabelCollapsesRepeatedWords(t *testing.T) {
	// macOS created a service literally named "iPhone USB USB" after
	// re-enumerating the phone. That is the OS stuttering, not a name, and it
	// should not be what a person reads on a dashboard.
	cases := map[string]string{
		"iPhone USB USB":     "iPhone USB",
		"iPhone USB USB USB": "iPhone USB",
		"Wi-Fi":              "Wi-Fi",
		"USB 10/100 LAN":     "USB 10/100 LAN",
		"Thunderbolt Bridge": "Thunderbolt Bridge",
		"usb USB":            "usb",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := (Link{Iface: "en5", Friendly: in}).Label(); got != want {
				t.Errorf("Label(%q) = %q, want %q", in, got, want)
			}
		})
	}
}
