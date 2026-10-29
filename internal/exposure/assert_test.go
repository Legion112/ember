package exposure

import (
	"errors"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
)

func nodeWith(es ...node.Exposed) node.Node {
	n := node.Node{Handle: node.Handle{Provider: "runpod", ID: "abc123"}}
	return n.WithExposed(es)
}

func TestAssertSSHOnly_AcceptsExactlySSH(t *testing.T) {
	if err := AssertSSHOnly(nodeWith(node.Exposed{Port: 22, Proto: node.ProtoTCP})); err != nil {
		t.Fatalf("a correctly exposed node was refused: %v", err)
	}
}

// TestAssertSSHOnly_RefusesRunPodsHTTPProxy is the case this package exists
// for: one `8188/http` mapping is an unauthenticated ComfyUI on the public
// internet, and ComfyUI has arbitrary code execution through custom nodes.
func TestAssertSSHOnly_RefusesRunPodsHTTPProxy(t *testing.T) {
	n := nodeWith(
		node.Exposed{Port: 22, Proto: node.ProtoTCP},
		node.Exposed{Port: 8188, Proto: node.ProtoHTTP, URL: "https://abc123-8188.proxy.runpod.net"},
	)
	err := AssertSSHOnly(n)
	if err == nil {
		t.Fatal("a public HTTP proxy port was accepted")
	}

	var v *Violation
	if !errors.As(err, &v) {
		t.Fatalf("got %T, want *Violation", err)
	}
	// The operator needs the address anyone could paste into a browser, not
	// a count of how many ports were wrong.
	if !strings.Contains(err.Error(), "abc123-8188.proxy.runpod.net") {
		t.Errorf("the public URL is missing from the error: %v", err)
	}
	if !strings.Contains(err.Error(), "destroyed") {
		t.Errorf("the error does not say the node is being destroyed: %v", err)
	}
}

// TestAssertSSHOnly_RefusesAnUnknownProtocolOnTwentyTwo. A provider whose API
// grows a protocol ember does not parse must not have it read as plain TCP
// just because the port number is familiar.
func TestAssertSSHOnly_RefusesAnUnknownProtocolOnTwentyTwo(t *testing.T) {
	if err := AssertSSHOnly(nodeWith(node.Exposed{Port: 22, Proto: node.ProtoUnknown})); err == nil {
		t.Fatal("port 22 with an unknown protocol was accepted")
	}
}

func TestAssertSSHOnly_RefusesUDP(t *testing.T) {
	n := nodeWith(
		node.Exposed{Port: 22, Proto: node.ProtoTCP},
		node.Exposed{Port: 51820, Proto: node.ProtoUDP},
	)
	if err := AssertSSHOnly(n); err == nil {
		t.Fatal("a public UDP port was accepted")
	}
}

func TestAssertSSHOnly_RefusesANodeWithNoPublicSSH(t *testing.T) {
	err := AssertSSHOnly(nodeWith())
	if err == nil {
		t.Fatal("a node with nothing public was accepted by the SSH assertion")
	}
	if !strings.Contains(err.Error(), "no public SSH port") {
		t.Errorf("got %v", err)
	}
}

// TestAssertSSHOnly_RefusesTwoMappingsForTwentyTwo rather than picking one.
// Two public mappings mean the provider is doing something ember does not
// model, and guessing is how a connection lands somewhere unexpected.
func TestAssertSSHOnly_RefusesTwoMappingsForTwentyTwo(t *testing.T) {
	n := nodeWith(
		node.Exposed{Port: 22, Proto: node.ProtoTCP, URL: "1.2.3.4:40022"},
		node.Exposed{Port: 22, Proto: node.ProtoTCP, URL: "5.6.7.8:40022"},
	)
	err := AssertSSHOnly(n)
	if err == nil {
		t.Fatal("two public SSH mappings were accepted")
	}
	if !strings.Contains(err.Error(), "will not guess") {
		t.Errorf("got %v", err)
	}
}

// TestAssertSSHOnly_RefusesAnUnreportedNode: "the provider never told us" and
// "the provider published nothing" are the same empty slice, and treating the
// first as the second is how an unverified node gets waved through.
func TestAssertSSHOnly_RefusesAnUnreportedNode(t *testing.T) {
	err := AssertSSHOnly(node.Node{Handle: node.Handle{Provider: "runpod", ID: "abc123"}})
	if !errors.Is(err, ErrUnreported) {
		t.Fatalf("got %v, want ErrUnreported", err)
	}
	if !strings.Contains(err.Error(), "abc123") {
		t.Errorf("the error does not name the node: %v", err)
	}
}

func TestAssertNoPublishedPorts(t *testing.T) {
	if err := AssertNoPublishedPorts(nodeWith()); err != nil {
		t.Fatalf("a node publishing nothing was refused: %v", err)
	}

	err := AssertNoPublishedPorts(nodeWith(node.Exposed{Port: 22, Proto: node.ProtoTCP}))
	if err == nil {
		t.Fatal("the strict assertion accepted a published port")
	}
	var v *Violation
	if !errors.As(err, &v) {
		t.Fatalf("got %T, want *Violation", err)
	}

	if err := AssertNoPublishedPorts(node.Node{}); !errors.Is(err, ErrUnreported) {
		t.Fatalf("got %v, want ErrUnreported", err)
	}
}

// TestAssertSSHOnly_ToleratesADeclaredPlatformPort.
//
// RunPod maps its web terminal port to every pod and offers no way to turn it
// off, which ember's first real provision discovered by refusing the node and
// destroying it. A provider that is usable at all has to be able to name such
// a port -- and only such a port.
func TestAssertSSHOnly_ToleratesADeclaredPlatformPort(t *testing.T) {
	platform := PlatformPort{Port: 19123, Proto: node.ProtoHTTP, Why: "RunPod's web terminal"}
	n := nodeWith(
		node.Exposed{Port: 22, Proto: node.ProtoTCP},
		node.Exposed{Port: 19123, Proto: node.ProtoHTTP, URL: "https://abc123-19123.proxy.runpod.net"},
	)

	if err := AssertSSHOnly(n, platform); err != nil {
		t.Fatalf("a declared platform port was refused: %v", err)
	}
	// Without the declaration it is still a violation, so the concession is
	// the provider's to make and not a general loosening.
	if err := AssertSSHOnly(n); err == nil {
		t.Fatal("the same port was accepted with nothing declaring it")
	}
}

// TestAssertSSHOnly_APlatformDeclarationIsPortAndProtocol. Declaring
// 19123/http must not wave through 19123/tcp: a different protocol on the
// same number is a different service.
func TestAssertSSHOnly_APlatformDeclarationIsPortAndProtocol(t *testing.T) {
	platform := PlatformPort{Port: 19123, Proto: node.ProtoHTTP}

	wrongProto := nodeWith(
		node.Exposed{Port: 22, Proto: node.ProtoTCP},
		node.Exposed{Port: 19123, Proto: node.ProtoTCP},
	)
	if err := AssertSSHOnly(wrongProto, platform); err == nil {
		t.Error("19123/tcp was accepted by a declaration of 19123/http")
	}

	wrongPort := nodeWith(
		node.Exposed{Port: 22, Proto: node.ProtoTCP},
		node.Exposed{Port: 8188, Proto: node.ProtoHTTP},
	)
	if err := AssertSSHOnly(wrongPort, platform); err == nil {
		t.Error("a declaration of 19123 waved through 8188")
	}
}

// TestTolerated_SaysWhatWasWavedThrough. An exposure accepted silently is one
// nobody re-examines.
func TestTolerated_SaysWhatWasWavedThrough(t *testing.T) {
	platform := PlatformPort{Port: 19123, Proto: node.ProtoHTTP, Why: "RunPod's web terminal"}
	n := nodeWith(
		node.Exposed{Port: 22, Proto: node.ProtoTCP},
		node.Exposed{Port: 19123, Proto: node.ProtoHTTP},
	)

	got := Tolerated(n, []PlatformPort{platform})
	if len(got) != 1 || got[0].Port != 19123 {
		t.Fatalf("got %v", got)
	}
	if len(Tolerated(nodeWith(node.Exposed{Port: 22, Proto: node.ProtoTCP}), []PlatformPort{platform})) != 0 {
		t.Error("a node carrying no platform port reported one")
	}
	// An unreported node tolerates nothing, because it has said nothing.
	if Tolerated(node.Node{}, []PlatformPort{platform}) != nil {
		t.Error("an unreported node reported tolerated ports")
	}
}
