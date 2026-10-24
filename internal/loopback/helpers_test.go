package loopback

import (
	"net"
	"strconv"
	"testing"
	"time"
)

const dialTimeout = 250 * time.Millisecond

func itoa(i int) string { return strconv.Itoa(i) }

// nonLoopbackAddrs returns this machine's non-loopback IPv4 addresses.
//
// If the machine has none — a network-isolated CI container is the usual
// case — the test that uses it has nothing to prove and passes trivially. That
// is honest: the assertion is "not reachable on an address other than
// loopback", and a machine with no other address cannot violate it.
func nonLoopbackAddrs(t *testing.T) []string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("enumerate interfaces: %v", err)
	}
	var out []string
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.To4() == nil {
			continue
		}
		out = append(out, n.IP.String())
	}
	return out
}
