//go:build integration

package integration

import (
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
	"github.com/Legion112/ember/test/integration/harness"
)

// labNode is a node this package started itself, rather than through ember.
type labNode struct {
	Pair hostkey.Pair
	ID   string
	Addr string
}

// mintAndStart brings up a node the same way the local provider does: a host
// key minted here, injected through the Engine API body, and the operator's
// own public key as the only way in.
func mintAndStart(t *testing.T, l *harness.Lab, name string) labNode {
	t.Helper()

	pair, err := hostkey.Mint()
	if err != nil {
		t.Fatal(err)
	}
	id, addr := l.StartNode(t.Context(), harness.NodeSpec{
		Name:          name,
		AuthorizedKey: identity(t, l).AuthorizedKey,
		HostKeyPEM:    string(pair.Private.Bytes()),
	})
	l.WaitSSH(addr, 90*time.Second)
	return labNode{Pair: pair, ID: id, Addr: addr}
}

// identity is the operator's key, generated inside the lab's own config dir so
// no lab ever reads or writes the real ~/.config/ember.
func identity(t *testing.T, l *harness.Lab) keys.Identity {
	t.Helper()
	id, err := keys.EnsureIdentity(filepath.Join(l.Dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func fingerprintOf(t *testing.T, p hostkey.Pair) hostkey.Fingerprint {
	t.Helper()
	fp, err := hostkey.FromNodeKey(p.NodeKey())
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// dial goes through session.Dial, the production path, so the retry policy and
// the mismatch short-circuit under test are the real ones rather than a
// reimplementation of them here.
func dial(t *testing.T, l *harness.Lab, n labNode, trust hostkey.Trust) (*sshdial.Client, error) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(n.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatal(err)
	}

	return session.Dial(t.Context(),
		sshdial.Target{
			NodeID:   n.ID,
			Endpoint: node.Endpoint{Host: host, Port: uint16(port), User: "root"},
		},
		trust,
		[]ssh.Signer{identity(t, l).Signer},
		// A short budget: these labs want the dial outcome, not a minute of
		// patience. The mismatch labs assert on how fast a refusal comes
		// back, which only means anything against a budget it could exceed.
		session.Options{SSHReady: 10 * time.Second, RetryEvery: 250 * time.Millisecond},
	)
}

// mintPEM is a fresh host key for a lab that does not care which one.
func mintPEM(t *testing.T) string {
	t.Helper()
	p, err := hostkey.Mint()
	if err != nil {
		t.Fatal(err)
	}
	return string(p.Private.Bytes())
}
