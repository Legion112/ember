//go:build integration

package integration

import (
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/test/integration/harness"
)

// TestHostKey_AMintedKeyAuthenticatesTheFirstConnection.
//
// This is the claim that makes trust-on-first-use unnecessary here. ember
// generates the host key on this machine, ships the private half to the node
// through the provider's API body, and the node's sshd adopts it. The very
// first dial is therefore verified against a fingerprint ember already knew,
// and nobody on the path to a brand-new IP has a window -- which is precisely
// the window TOFU cannot close.
func TestHostKey_AMintedKeyAuthenticatesTheFirstConnection(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "firstdial")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}

	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatalf("the first dial against an injected host key failed: %v", err)
	}
	_ = client.Close()
}

// TestHostKey_ARotatedKeyIsRefused is the test that proves the security
// control, and it is the reason this lab package starts its own node: the
// control only triggers on a second dial to the same node id, and ember's own
// nodes are destroyed by design before there can be a second dial.
//
// Rotating the host key inside the container is what an attacker substituting
// a machine looks like from the client's side. The dial must fail, there must
// be no prompt, and there is no --force anywhere in ember to get past it: a
// prompt is a bypass with extra steps, and for an ephemeral node the correct
// response is always to destroy it.
func TestHostKey_ARotatedKeyIsRefused(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "rotate")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}

	// First dial: succeeds against the key ember injected.
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatalf("the first dial failed, so the rotation proves nothing: %v", err)
	}
	_ = client.Close()

	l.RotateHostKey(t.Context(), n.ID)
	l.WaitSSH(n.Addr, 60*time.Second)

	// Second dial: the machine now presents a key ember never minted.
	_, err = dial(t, l, n, trust)
	if err == nil {
		t.Fatal("ember connected to a node presenting a host key it never minted")
	}

	var mismatch *hostkey.MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("got %T (%v), want *hostkey.MismatchError", err, err)
	}
}

// TestHostKey_AMismatchIsNotRetried. session.Dial retries, because sshd binds
// its listener before it will negotiate and the first dial legitimately fails.
// A mismatch is the one error it must not retry: retrying an authentication
// failure against a substituted machine is a slow-motion way of eventually
// accepting it, and it turns one clear error into a minute of silence.
func TestHostKey_AMismatchIsNotRetried(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "noretry")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	l.RotateHostKey(t.Context(), n.ID)
	l.WaitSSH(n.Addr, 60*time.Second)

	start := time.Now()
	_, err = dial(t, l, n, trust)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the dial succeeded against a rotated key")
	}
	var mismatch *hostkey.MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("got %T (%v), want *hostkey.MismatchError", err, err)
	}
	// The retry budget is tens of seconds. Returning inside a few means the
	// mismatch short-circuited it rather than being retried to exhaustion.
	if elapsed > 15*time.Second {
		t.Errorf("a mismatch took %v to report, so it was retried rather than refused outright", elapsed)
	}
}

// TestNodeImage_PublishesNothingButSSH is the evidence the platform-port
// concession rests on.
//
// RunPod maps its web terminal port to every pod, publicly and with no
// authentication, and offers no way to disable it. ember tolerates that only
// because nothing in its node image is listening there: probing the live
// proxy for that port returned 502 "Waiting for service to respond" -- a route
// to an empty room. That is a property of this image, so it is checked here
// rather than assumed, and it would stop being true the moment the image
// gained a service.
//
// IT WAS "LISTENS ON NOTHING BUT SSH" AND THE NODE IMAGE NOW HAS A WORKER,
// which is the one listener in it that is not sshd -- so the rule had to be
// stated properly rather than loosened. What a provider can publish is a
// port on the container's ROUTABLE address: a mapping targets the container's
// own interface, so a socket bound to 127.0.0.1 inside the pod's network
// namespace is not reachable through one however the provider maps it. The
// assertion is therefore about the ADDRESS and not only the port, which is
// strictly stronger than counting ports was: a worker that bound `0.0.0.0:7311`
// is caught by the rule itself now rather than by somebody noticing a number.
//
// AND THE WORKER'S OWN SOCKET IS ASSERTED, not merely tolerated. Permitting
// "any loopback listener" would pass against an image that had lost its
// worker entirely, which is a node ember cannot give work to -- so the port
// has to be there, and it has to be loopback.
func TestNodeImage_PublishesNothingButSSH(t *testing.T) {
	out := assertListensOnlyOnSSH(t, "")

	var worker *listener
	for _, l := range listeners(out) {
		if l.port == strconv.Itoa(control.Port) {
			found := l
			worker = &found
		}
	}
	if worker == nil {
		t.Fatalf("nothing is listening on %d, so this image has no worker and ember could give "+
			"a node built from it no work. Listening sockets:\n%s", control.Port, out)
	}
	if !isLoopback(worker.addr) {
		t.Errorf("the worker is bound to %s rather than to loopback, which is an address a "+
			"provider's port mapping can reach:\n%s", worker.addr, out)
	}
}

// assertListensOnlyOnSSH is the rule, and it returns `ss`'s own output so a
// caller can say something more specific about the image it asked about.
func assertListensOnlyOnSSH(t *testing.T, image string) string {
	t.Helper()
	l := harness.New(t)

	if image != "" && !l.HaveImage(t.Context(), image) {
		t.Fatalf("%s is not built. The assertion this lab makes is about that image, "+
			"so it cannot be skipped:\n  make workload-sd-cuda", image)
	}

	n := mintAndStartImage(t, l, "listeners", image)

	// ss over the Docker API rather than over the tunnel: what is being
	// tested is the image, and the question is what it binds before anything
	// connects to it.
	//
	// Polled until sshd has bound, rather than read once. Reading once was a
	// race: embernode starts sshd as a child, and on a loaded daemon the
	// first ss can land before the bind -- which left `ports` empty and
	// failed with "the parse is broken, not the image", a misleading message
	// for a timing problem. Observed once while a 2.65 GB pull was in flight,
	// and not reproducible afterwards, which is the worst shape a failure in
	// a security lab can have: it teaches whoever sees it to run it again.
	//
	// The wait is for 22 specifically and not for "any listener", because
	// "any" is satisfied by exactly the thing this lab exists to catch.
	var out string
	var ports []string
	deadline := time.Now().Add(15 * time.Second)
	for {
		out = l.ExecOut(t.Context(), n.ID, "ss", "-H", "-ltn")
		ports = listeningPorts(out)
		if slices.Contains(ports, "22") || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(ports) == 0 {
		t.Fatalf("nothing is listening after %v, not even sshd; embernode did not come up:\n%s",
			15*time.Second, out)
	}
	if !slices.Contains(ports, "22") {
		t.Fatalf("sshd never bound 22 within %v; found %v:\n%s", 15*time.Second, ports, out)
	}

	// THE ADDRESS AND NOT ONLY THE PORT. A provider publishes a port on the
	// container's routable address, so a socket on loopback inside the pod's
	// network namespace is not reachable through a mapping however the
	// provider makes one -- while anything bound to a wildcard or to the
	// container's own interface is exactly what this lab exists to catch.
	//
	// Every tolerated loopback socket is NAMED on the way past, because a
	// concession that passes silently is one nobody re-examines -- which is
	// this repository's own argument for why `exposure.Tolerated` exists.
	for _, l := range listeners(out) {
		switch {
		case l.port == "22":
		case isLoopback(l.addr):
			t.Logf("tolerated: %s:%s is on loopback, which no port mapping can reach", l.addr, l.port)
		default:
			t.Errorf("the image listens on %s:%s, which is an address a provider's port mapping "+
				"can reach. Listening sockets:\n%s", l.addr, l.port, out)
		}
	}

	// And specifically not the port RunPod proxies to the world.
	if strings.Contains(out, "19123") {
		t.Errorf("the image listens on RunPod's public proxy port:\n%s", out)
	}
	return out
}

// listener is one socket `ss -H -ltn` reported.
type listener struct{ addr, port string }

// listeners reads the local-address column out of `ss -H -ltn`.
//
// Extracted so the poll above and the assertions below cannot disagree about
// what counts as a listener -- and parsed from the right rather than split on
// every colon, because an IPv6 address is full of them.
func listeners(out string) []listener {
	var found []listener
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		found = append(found, listener{addr: local[:i], port: local[i+1:]})
	}
	return found
}

// listeningPorts is the ports alone, for the poll that waits for sshd.
func listeningPorts(out string) []string {
	var ports []string
	for _, l := range listeners(out) {
		ports = append(ports, l.port)
	}
	return ports
}

// isLoopback decides whether an address `ss` printed is one no port mapping
// can reach.
//
// netip decides, after the brackets come off: `*` and `0.0.0.0` and `[::]` are
// the wildcards ss prints for a socket bound to everything, and none of them
// parses as an address -- which is the answer wanted, because a wildcard is
// the publishable case. Guessing ahead of the real parser is how
// loopback.ParseBind's first version sent readers somewhere the bug was not.
func isLoopback(addr string) bool {
	a, err := netip.ParseAddr(strings.Trim(addr, "[]"))
	if err != nil {
		return false
	}
	return a.IsLoopback()
}
