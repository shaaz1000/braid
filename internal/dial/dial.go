//go:build darwin

// Package dial builds HTTP transports whose every connection is forced onto
// one network interface, in one address family.
//
// This is the mechanism the rest of braid rests on. macOS routes all traffic
// via a single default gateway, so a socket must be explicitly pinned or a
// second uplink sits idle no matter how many connections are opened.
package dial

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"braid/internal/linkset"
)

// Darwin socket options for pinning a socket to an interface. Values verified
// against MacOSX.sdk/usr/include: netinet/in.h:432 and netinet6/in6.h:506.
const (
	ipBoundIf   = 25
	ipv6BoundIf = 125
)

// Tuning for chunk transfers: enough idle connections to keep a link busy,
// and timeouts short enough that a wedged link is dropped rather than stalling
// the whole job.
const (
	dialTimeout           = 10 * time.Second
	idleTimeout           = 30 * time.Second
	responseHeaderTimeout = 20 * time.Second
	maxIdlePerHost        = 8
)

// Transport returns an *http.Transport pinned to one link and family.
//
// Two mechanisms are applied together. LocalAddr fixes the source address, and
// *_BOUND_IF forces the kernel to use that interface's scoped route regardless
// of the system default route. The second is the one that actually moves
// traffic onto a non-default link; source binding alone is not sufficient.
//
// Each call returns a distinct Transport. Connection pools must not be shared
// between links, because a pooled connection belongs to the interface it was
// dialed on.
func Transport(l linkset.Link, f linkset.Family) (*http.Transport, error) {
	src, ok := l.Addr(f)
	if !ok {
		return nil, fmt.Errorf("link %s has no %s address", l.Iface, f)
	}

	index := l.Index
	network := f.Network()

	d := &net.Dialer{
		Timeout:   dialTimeout,
		LocalAddr: &net.TCPAddr{IP: src.AsSlice()},
		Control: func(network, _ string, c syscall.RawConn) error {
			var setErr error
			if err := c.Control(func(fd uintptr) {
				switch network {
				case "tcp4":
					setErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBoundIf, index)
				case "tcp6":
					setErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6BoundIf, index)
				default:
					setErr = fmt.Errorf("cannot pin unexpected network %q", network)
				}
			}); err != nil {
				return err
			}
			return setErr
		},
	}

	return &http.Transport{
		// The family is pinned as well as the interface: dialing "tcp4" makes
		// the resolver return A records only, so a v4-only link never attempts
		// an IPv6 origin it cannot reach.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       idleTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		// Chunk bodies are already-compressed bytes and must arrive with the
		// exact length the Range request asked for.
		DisableCompression: true,
	}, nil
}
