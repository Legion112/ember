// Package loopback is the only place in ember that opens a listener.
//
// Everything ember forwards home — a ComfyUI web UI, a vLLM endpoint, a
// Jupyter port — is reachable on the operator's machine and nowhere else. That
// is not a configuration default; it is the point of the project. ComfyUI in
// particular has arbitrary file read and arbitrary code execution through
// custom nodes, so a ComfyUI published to a coffee-shop LAN is a shell on the
// operator's laptop handed to the room.
//
// The guarantee is structural: Bind's field is unexported and this package
// exports no struct-literal path, so the only Binds that exist are the ones
// Port, Port6 and ParseBind produced, and all three refuse anything net.Listen
// would publish off-host. TestNoListenOutsideLoopback keeps it that way by
// failing the build if net.Listen appears in any other package.
package loopback

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Bind is a local listen address proved to be loopback.
//
// The zero value is invalid and Listen refuses it, so a Bind nobody set cannot
// be mistaken for one somebody chose.
type Bind struct {
	ap netip.AddrPort
}

// Port returns 127.0.0.1:port.
//
// It cannot fail, which keeps internal call sites free of error handling they
// would be tempted to ignore.
func Port(port uint16) Bind {
	return Bind{ap: netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port)}
}

// Port6 returns [::1]:port.
func Port6(port uint16) Bind {
	return Bind{ap: netip.AddrPortFrom(netip.IPv6Loopback(), port)}
}

// ParseBind is the config path: it turns an operator-written string into a
// Bind, or explains why that string would have published a port.
func ParseBind(s string) (Bind, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return Bind{}, fmt.Errorf("empty bind address")
	}

	// netip decides first, and the failure branch only chooses which
	// explanation to give. An earlier version guessed "is this a hostname?"
	// before parsing, with a letters-and-dots heuristic; it classified
	// ":8188" and "[::1%eth0]:8188" as hostnames and gave both the wrong
	// advice. Guessing ahead of the real parser is how an error message sends
	// the reader somewhere the bug is not.
	ap, err := netip.ParseAddrPort(t)
	if err != nil {
		if host, _, splitErr := net.SplitHostPort(t); splitErr == nil && host != "" {
			if _, addrErr := netip.ParseAddr(host); addrErr != nil {
				// A *name*, resolved at listen time through /etc/hosts, NSS
				// and possibly DNS. A host whose hosts file maps localhost to
				// a LAN address, or a search domain that answers for it,
				// turns a "loopback" bind into a published one — and nothing
				// in the config would look wrong. A name is never proof of
				// loopback.
				return Bind{}, fmt.Errorf("bind address %q uses a hostname; write a loopback IP such as 127.0.0.1, because a name is resolved at listen time and can point anywhere", s)
			}
		}
		// A bare ":8188" or "8188" lands here. Both are rejected rather than
		// read as loopback: ":8188" means *wildcard* to net.Listen, and
		// accepting that spelling would leave a string in the config that
		// publishes the port the moment anything hands it to the standard
		// library directly.
		return Bind{}, fmt.Errorf("bind address %q must be written in full as ip:port, for example 127.0.0.1:8188: %w", s, err)
	}

	addr := ap.Addr()
	if addr.Zone() != "" {
		return Bind{}, fmt.Errorf("bind address %q carries a zone; a zoned address is a link-scope concept and has no business being a loopback bind", s)
	}
	if addr.Is4In6() {
		// ::ffff:127.0.0.1 *is* loopback and would be safe to listen on. It
		// is refused anyway because two spellings of one address is how a
		// later comparison against a recorded bind starts returning false.
		return Bind{}, fmt.Errorf("bind address %q is an IPv4-mapped IPv6 address; write it as %s", s, addr.Unmap().String())
	}
	if !addr.IsLoopback() {
		return Bind{}, fmt.Errorf("bind address %q is not loopback; ember never publishes a workload port off this machine", s)
	}
	return Bind{ap: ap}, nil
}

func (b Bind) AddrPort() netip.AddrPort { return b.ap }
func (b Bind) String() string           { return b.ap.String() }

// IsValid reports whether b was produced by a constructor.
func (b Bind) IsValid() bool { return b.ap.IsValid() && b.ap.Addr().IsLoopback() }

// Listen is the only net.Listen in ember.
//
// See TestNoListenOutsideLoopback: every guard above is worth nothing if a
// provider package opens its own listener because it was two lines shorter.
func Listen(ctx context.Context, b Bind) (net.Listener, error) {
	if !b.IsValid() {
		return nil, fmt.Errorf("refusing to listen on an unset bind address: a Bind nobody set is not a Bind somebody chose")
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", b.ap.String())
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", b, err)
	}
	return ln, nil
}
