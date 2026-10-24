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
