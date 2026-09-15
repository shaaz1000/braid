//go:build darwin

package dial

import (
	"braid/internal/linkset"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func loopback(t *testing.T) linkset.Link {
	t.Helper()
	ifc, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatalf("lo0 not found: %v", err)
	}
	return linkset.Link{
		Iface: "lo0",
		Index: ifc.Index,
		V4:    netip.MustParseAddr("127.0.0.1"),
		V6:    netip.MustParseAddr("::1"),
	}
}

func get(t *testing.T, tr *http.Transport, url string) (string, error) {
	t.Helper()
	c := &http.Client{Transport: tr}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func TestTransportErrorsOnMissingFamily(t *testing.T) {
	// The USB tether was IPv4-only for a while. Asking it for IPv6 must be a
	// clear error, never a silent fallback onto another interface.
	v4only := linkset.Link{Iface: "en5", Index: 21, V4: netip.MustParseAddr("172.20.10.2")}

	if _, err := Transport(v4only, linkset.Fam4); err != nil {
		t.Errorf("Fam4 on a v4 link should work, got %v", err)
	}
	_, err := Transport(v4only, linkset.Fam6)
	if err == nil {
		t.Fatal("Fam6 on a v4-only link must error")
	}
	if !strings.Contains(err.Error(), "en5") || !strings.Contains(err.Error(), "IPv6") {
		t.Errorf("error should name the link and family, got %q", err)
	}
}

func TestPinnedTransportReachesServerOverIPv4(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pinned-v4")
	}))
	defer srv.Close()

	tr, err := Transport(loopback(t), linkset.Fam4)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	defer tr.CloseIdleConnections()

	body, err := get(t, tr, srv.URL)
	if err != nil {
		t.Fatalf("request over pinned transport failed: %v", err)
	}
	if body != "pinned-v4" {
		t.Errorf("body = %q", body)
	}
}

func TestPinnedTransportReachesServerOverIPv6(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback available: %v", err)
	}
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "pinned-v6")
		})},
	}
	srv.Start()
	defer srv.Close()

	tr, err := Transport(loopback(t), linkset.Fam6)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	defer tr.CloseIdleConnections()

	body, err := get(t, tr, srv.URL)
	if err != nil {
		t.Fatalf("request over pinned v6 transport failed: %v", err)
	}
	if body != "pinned-v6" {
		t.Errorf("body = %q", body)
	}
}

func TestTransportActuallyBindsTheSourceAddress(t *testing.T) {
	// If LocalAddr were being dropped, this would connect happily. Binding an
	// address that exists on no interface must fail.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	bogus := linkset.Link{Iface: "lo0", Index: 1, V4: netip.MustParseAddr("10.99.99.99")}
	tr, err := Transport(bogus, linkset.Fam4)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	defer tr.CloseIdleConnections()

	if _, err := get(t, tr, srv.URL); err == nil {
		t.Fatal("binding an address this machine does not have must fail the dial")
	}
}

func TestTransportActuallyAppliesBoundIf(t *testing.T) {
	// The load-bearing assertion of the whole project. IP_BOUND_IF is what
	// forces traffic onto a non-default interface; if it were being silently
	// ignored, a nonexistent interface index would make no difference and this
	// request would succeed over loopback anyway.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	badIndex := linkset.Link{Iface: "lo0", Index: 9999, V4: netip.MustParseAddr("127.0.0.1")}
	tr, err := Transport(badIndex, linkset.Fam4)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	defer tr.CloseIdleConnections()

	if _, err := get(t, tr, srv.URL); err == nil {
		t.Fatal("a nonexistent interface index must fail the dial; IP_BOUND_IF is not taking effect")
	}
}

func TestSeparateTransportsDoNotShareConnections(t *testing.T) {
	// A pooled connection belongs to the interface it was dialed on, so each
	// link needs its own Transport or chunks would leak onto the wrong link.
	a, err := Transport(loopback(t), linkset.Fam4)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Transport(loopback(t), linkset.Fam4)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("Transport must return a distinct transport per call")
	}
}
