package exposure

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Legion112/ember/internal/node"
)

// ErrUnreported is returned when a provider has not said what it published.
//
// It is a distinct error because the remedy is different from a failed
// assertion: a failed assertion means destroy this node, while an unreported
// one means the provider's Inspect does not populate exposure yet and the
// mapping code is incomplete. Both must fail, but they are not the same bug.
var ErrUnreported = errors.New("the provider did not report which ports it published, so ember cannot verify that only sshd is reachable")

// Violation is an exposure assertion that failed.
//
// It carries the offending mappings so the operator reads the public URL
// anyone could paste into a browser, rather than a count.
type Violation struct {
	Handle node.Handle
	Public []node.Exposed
	Why    string
}

func (v *Violation) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "node %s is exposed more widely than ember asked: %s", v.Handle, v.Why)
	for _, e := range v.Public {
		fmt.Fprintf(&b, "\n  reachable: %s", e)
	}
	b.WriteString("\n  this node is being destroyed: it is already wrong, and leaving it up while warning about it is the worst of both outcomes")
	return b.String()
}

// PlatformPort is a public mapping a provider adds to every node of its own
// accord, which no request shape can prevent.
//
// It exists because RunPod maps its web terminal port to every pod and offers
// no way to turn it off -- found on ember's first real provision, which this
// package refused and destroyed. Naming such a port is a deliberate, narrow
// concession: it is the provider's behaviour, it is identical on every node,
// and the alternative is that ember cannot use the provider at all.
//
// The concession is only safe while nothing in the node image listens on the
// port, so that is a separate, testable property of the image rather than
// something assumed here. Why says what the port is, because an operator
// reading an exposure report deserves to know which ports ember decided to
// tolerate and on whose authority.
type PlatformPort struct {
	Port  uint16
	Proto node.Proto
	Why   string
}

func (p PlatformPort) matches(e node.Exposed) bool {
	return e.Port == p.Port && e.Proto == p.Proto
}

// AssertSSHOnly requires that exactly one port is publicly reachable and that
// it is sshd over plain TCP.
//
// platform lists mappings the provider adds unconditionally. Everything not
// in that list, and not sshd, still destroys the node.
//
// It reads what the provider reports, never what ember requested. The point is
// to catch the provider publishing something ember did not ask for -- a
// default `8188/http` mapping inherited from a template, an EXPOSE line in the
// image that the provider turns into a public mapping -- and a record of
// ember's own request cannot catch any of that.
//
// Any failure means the node is destroyed rather than warned about. A warning
// leaves an unauthenticated ComfyUI on the internet for as long as the
// operator takes to read their terminal, and ember's whole claim is that this
// cannot happen.
func AssertSSHOnly(n node.Node, platform ...PlatformPort) error {
	es, reported := n.Exposure()
	if !reported {
		return fmt.Errorf("node %s: %w", n.Handle, ErrUnreported)
	}

	var bad []node.Exposed
	ssh := 0
	for _, e := range es {
		switch {
		case e.Port == 22 && e.Proto == node.ProtoTCP:
			ssh++
		case isPlatform(e, platform):
			// Tolerated, not approved. See PlatformPort.
		default:
			bad = append(bad, e)
		}
	}

	if len(bad) > 0 {
		return &Violation{Handle: n.Handle, Public: bad, Why: "these ports are reachable from the internet and only sshd may be"}
	}
	switch ssh {
	case 1:
		return nil
	case 0:
		// Not a security failure, but the node is unusable and the next error
		// would be an unexplained dial timeout.
		return &Violation{Handle: n.Handle, Why: "no public SSH port: the node cannot be reached, and ember forwards everything over SSH"}
	default:
		// Two public mappings for port 22 means the provider is doing
		// something ember does not model, and guessing which one is real is
		// how a connection ends up somewhere unexpected.
		return &Violation{Handle: n.Handle, Public: es, Why: fmt.Sprintf("%d public mappings for port 22; ember expects exactly one and will not guess which", ssh)}
	}
}

func isPlatform(e node.Exposed, platform []PlatformPort) bool {
	for _, p := range platform {
		if p.matches(e) {
			return true
		}
	}
	return false
}

// Tolerated reports the platform ports a node is actually carrying, so a
// caller can log what it decided to live with. An exposure that is waved
// through silently is one nobody re-examines.
func Tolerated(n node.Node, platform []PlatformPort) []node.Exposed {
	es, reported := n.Exposure()
	if !reported {
		return nil
	}
	var out []node.Exposed
	for _, e := range es {
		if isPlatform(e, platform) {
			out = append(out, e)
		}
	}
	return out
}

// AssertNoPublishedPorts requires that the provider published nothing at all.
//
// This is the stricter assertion, for providers reached without any public
// port: the local Docker one, where sshd is bound to loopback on the
// operator's own machine, and anything later reached over WireGuard. A
// provider that publishes nothing has no attack surface to argue about.
func AssertNoPublishedPorts(n node.Node) error {
	es, reported := n.Exposure()
	if !reported {
		return fmt.Errorf("node %s: %w", n.Handle, ErrUnreported)
	}
	if len(es) > 0 {
		return &Violation{Handle: n.Handle, Public: es, Why: "this provider is expected to publish nothing"}
	}
	return nil
}
