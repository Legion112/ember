package node

import "fmt"

// Proto is the transport of a publicly reachable port.
//
// The zero value is ProtoUnknown rather than TCP. A provider whose API grows a
// protocol this table does not know must not have it silently read as "plain
// TCP, that is just SSH, carry on": not knowing what is listening on a public
// port is exactly the case that has to fail.
type Proto uint8

const (
	// ProtoUnknown is the zero value and never passes an exposure assertion.
	ProtoUnknown Proto = iota
	ProtoTCP
	ProtoUDP

	// ProtoHTTP is RunPod's dangerous one. A port declared `8188/http` is
	// served at https://{id}-8188.proxy.runpod.net with no authentication of
	// any kind, so it is a distinct protocol here and not a flavour of TCP.
	ProtoHTTP
)

var protoNames = map[Proto]string{
	ProtoUnknown: "unknown",
	ProtoTCP:     "tcp",
	ProtoUDP:     "udp",
	ProtoHTTP:    "http",
}

func (p Proto) String() string {
	if n, ok := protoNames[p]; ok {
		return n
	}
	return fmt.Sprintf("proto(%d)", uint8(p))
}

// ParseProto maps a provider's protocol string, refusing anything unlisted.
//
// Parse, do not grep: an unrecognised protocol is an error and not a default,
// because the caller is about to decide whether a port is safe to leave open.
func ParseProto(s string) (Proto, error) {
	switch s {
	case "tcp", "TCP":
		return ProtoTCP, nil
	case "udp", "UDP":
		return ProtoUDP, nil
	case "http", "HTTP", "https", "HTTPS":
		return ProtoHTTP, nil
	default:
		return ProtoUnknown, fmt.Errorf("unknown port protocol %q: ember cannot tell whether such a port is publicly reachable, so it refuses to assume", s)
	}
}

// Exposed is one port a provider has made reachable from outside the node.
//
// It is what the provider says it did, read back from its API after create --
// never what ember asked for. The whole point of the check above it is to
// catch a provider that published something ember did not request, and a
// record of the request cannot catch that.
type Exposed struct {
	// Port is the port on the node itself.
	Port uint16

	// Proto is how it is reachable.
	Proto Proto

	// URL is the public address, where the provider gives one. Kept for the
	// error message: "8188/http is public" is far less use to an operator
	// than the address anyone on the internet can paste into a browser.
	URL string
}

func (e Exposed) String() string {
	s := fmt.Sprintf("%d/%s", e.Port, e.Proto)
	if e.URL != "" {
		s += " at " + e.URL
	}
	return s
}

// WithExposed returns a copy of n carrying what the provider published.
//
// Like WithEndpoint, this exists so the field can stay unexported while
// providers still populate it from outside the package.
//
// An empty es is stored as an empty slice and never as nil, which is the whole
// reason this is not one append. `append([]Exposed(nil), es...)` returns nil
// when es is empty, so a provider correctly reporting "I published nothing"
// would have been indistinguishable from one that was never asked -- turning
// the safest possible answer into a failed assertion.
func (n Node) WithExposed(es []Exposed) Node {
	out := make([]Exposed, len(es))
	copy(out, es)
	n.exposed = out
	return n
}

// Exposed reports the ports the provider says are publicly reachable, and
// whether the provider answered the question at all.
//
// The second return exists because "this provider published nothing" and "this
// provider was never asked" are the same empty slice, and treating the second
// as the first is how an unchecked node gets waved through. Only a provider
// that has actually reported its mappings -- including reporting none --
// satisfies an exposure assertion.
func (n Node) Exposure() ([]Exposed, bool) {
	if n.exposed == nil {
		return nil, false
	}
	return n.exposed, true
}
