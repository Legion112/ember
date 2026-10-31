//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/statestore"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
	"github.com/Legion112/ember/test/integration/harness"
	"github.com/Legion112/ember/test/integration/runpodfake"
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
	return mintAndStartImage(t, l, name, "")
}

// mintAndStartImage starts a node from a named image, for the labs that
// assert about one other than ember:lab. An empty name is the lab's own.
func mintAndStartImage(t *testing.T, l *harness.Lab, name, image string) labNode {
	t.Helper()

	pair, err := hostkey.Mint()
	if err != nil {
		t.Fatal(err)
	}
	id, addr := l.StartNode(t.Context(), harness.NodeSpec{
		Name:          name,
		Image:         image,
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

// labOptions are provision.Options pointed entirely at the lab.
func labOptions(t *testing.T, l *harness.Lab) provision.Options {
	t.Helper()
	store, err := statestore.Open(filepath.Join(t.TempDir(), statestore.DirName))
	if err != nil {
		t.Fatal(err)
	}
	pins, err := hostkey.OpenStore(filepath.Join(t.TempDir(), "known_hosts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return provision.Options{
		Store:    store,
		Pins:     pins,
		Identity: identity(t, l),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// testLogger writes to w, for labs asserting on what was logged.
func testLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// dialSession connects to a provisioned node through the production path.
func dialSession(t *testing.T, l *harness.Lab, s provision.Session) *sshdial.Client {
	t.Helper()
	client, err := session.Dial(t.Context(),
		sshdial.Target{NodeID: s.Node.Handle().ID, Endpoint: s.Node.Endpoint()},
		s.Trust,
		[]ssh.Signer{identity(t, l).Signer},
		session.Options{SSHReady: 90 * time.Second, RetryEvery: 500 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("dial %s: %v", s.Node.Endpoint(), err)
	}
	return client
}

// allPodIDs is every pod the fake ever created.
func allPodIDs(f *runpodfake.Fake) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range f.Requests() {
		if strings.HasPrefix(r.Path, "/v2/pods/") {
			id := strings.TrimPrefix(r.Path, "/v2/pods/")
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// withNode picks an offer and rents it, the way runUp does.
//
// Two steps rather than one because provision.WithNode no longer lists offers
// itself: the workload image is chosen from the architecture of the card in
// the offer, so a second listing inside WithNode could rent a different card
// than the image was chosen for. A lab about which offer gets picked calls
// provision.PickOffer directly.
func withNode(
	ctx context.Context,
	p provider.Provider,
	spec node.Spec,
	o provision.Options,
	fn func(context.Context, provision.Session) error,
) error {
	offer, err := provision.PickOffer(ctx, p, spec.Filter, spec.TTL, o)
	if err != nil {
		return err
	}
	return provision.WithNode(ctx, p, spec, offer, o, fn)
}
