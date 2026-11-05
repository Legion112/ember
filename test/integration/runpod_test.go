//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/httpx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/runpod"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/sshx/sshexec"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/test/integration/harness"
	"github.com/Legion112/ember/test/integration/runpodfake"
)

// dockerNodes is the fake's data plane: real containers, started through the
// lab harness. The control plane lies about being RunPod; the machine it
// hands back does not.
type dockerNodes struct {
	t *testing.T
	l *harness.Lab
}

func (d dockerNodes) Start(m runpodfake.Machine) (string, string, uint16) {
	d.t.Helper()
	// The mounts are carried across, and that is what makes a store part of
	// the data plane rather than something the control plane only claims to
	// have attached: `mounts.network` becomes a real bind, so the node writes
	// its weights where the S3 endpoint will read them.
	binds := make([]harness.Bind, 0, len(m.Mounts))
	for _, mt := range m.Mounts {
		binds = append(binds, harness.Bind{Host: mt.Host, Path: mt.Path})
	}
	id, addr := d.l.StartNode(d.t.Context(), harness.NodeSpec{
		Name:          m.Name,
		AuthorizedKey: m.AuthorizedKey,
		HostKeyPEM:    m.HostKeyPEM,
		Binds:         binds,
	})
	host, port := runpodfake.SplitAddr(d.t, addr)
	return id, host, port
}

func (d dockerNodes) Stop(id string) { d.l.Remove(d.t.Context(), id) }

// runpodLab wires the real provider to the fake API and a real Docker daemon.
func runpodLab(t *testing.T, opts runpodfake.Options) (*runpod.Provider, *runpodfake.Fake, *harness.Lab) {
	t.Helper()
	l := harness.New(t)
	f := runpodfake.New(t, dockerNodes{t: t, l: l}, opts)

	p := runpod.New(node.NewSecret([]byte("fake-token")))
	p.HTTP = httpx.Client{BaseURL: f.URL, Token: node.NewSecret([]byte("fake-token"))}
	return p, f, l
}

func runpodSpec(t *testing.T) node.Spec {
	t.Helper()
	s, err := node.NewSpec(node.SpecOptions{
		Image:   harness.LabImage,
		TTL:     10 * time.Minute,
		Outputs: []node.OutputPath{{Remote: "/etc/hostname", Local: "host"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRunPod_FullLifecycleAgainstTheFake is the lab the live smoke test cost
// money to perform.
//
// The control plane is pretend and everything under it is real: a real
// container with a real sshd, a real SSH handshake verified against the key
// ember minted, a real forward and a real tar stream home. It reproduces the
// behaviours that cost money to discover -- a pod that is RUNNING before
// ssh.direct appears, and the web terminal port RunPod publishes unbidden --
// so none of them can come back silently.
func TestRunPod_FullLifecycleAgainstTheFake(t *testing.T) {
	p, f, l := runpodLab(t, runpodfake.Options{
		ReadyAfter: 2 * time.Second,
		SSHAfter:   2 * time.Second,
	})

	outDir := filepath.Join(t.TempDir(), "outputs")
	var fetched transfer.Result

	err := withNode(t.Context(), p, runpodSpec(t), labOptions(t, l),
		func(ctx context.Context, s provision.Session) error {
			client := dialSession(t, l, s)
			defer func() { _ = client.Close() }()

			res, ferr := transfer.Fetch(ctx, sshexec.New(client.SSH()),
				[]node.OutputPath{{Remote: "/etc/hostname", Local: "host"}}, outDir)
			fetched = res
			return ferr
		})
	if err != nil {
		t.Fatalf("the full lifecycle failed: %v", err)
	}

	if fetched.Files != 1 {
		t.Errorf("fetched %d files, want 1", fetched.Files)
	}
	body, rerr := os.ReadFile(filepath.Join(outDir, "host", "hostname"))
	if rerr != nil || len(strings.TrimSpace(string(body))) == 0 {
		t.Errorf("the output did not come back: %v %q", rerr, body)
	}

	// The pod is deleted, and so is the container behind it.
	if live := f.Live(); len(live) != 0 {
		t.Errorf("%d pod(s) survived: %v", len(live), live)
	}
}

// TestRunPod_DeclaresOnly22TCPToTheAPI. The single most dangerous field in
// RunPod's API: a port declared /http is served at
// {id}-{port}.proxy.runpod.net with no authentication of any kind.
//
// THERE IS NOTHING A SPEC CAN DO TO THIS, which is what the assertion is
// against: the create body's port list comes from exposure.Requests, whose
// only member is sshd and which takes no argument, so the figure below is a
// property of the program rather than of this spec.
func TestRunPod_DeclaresOnly22TCPToTheAPI(t *testing.T) {
	p, f, l := runpodLab(t, runpodfake.Options{ReadyAfter: time.Second, SSHAfter: time.Second})

	err := withNode(t.Context(), p, runpodSpec(t), labOptions(t, l),
		func(context.Context, provision.Session) error { return nil })
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	var created string
	for _, r := range f.Requests() {
		if r.Method == "POST" && r.Path == "/v2/pods" {
			created = r.Path
			ports, _ := r.Body["ports"].([]any)
			if len(ports) != 1 || ports[0] != "22/tcp" {
				t.Fatalf("ember declared %v to RunPod, want exactly [22/tcp]", ports)
			}
		}
	}
	if created == "" {
		t.Fatal("no create call was made")
	}
}

// TestRunPod_KeyMaterialReachesTheNodeThroughTheAPIBody and never argv:
// /proc/<pid>/cmdline is world-readable, and on rented hardware the other
// tenants of that host are strangers.
func TestRunPod_KeyMaterialReachesTheNodeThroughTheAPIBody(t *testing.T) {
	p, f, l := runpodLab(t, runpodfake.Options{ReadyAfter: time.Second, SSHAfter: time.Second})

	err := withNode(t.Context(), p, runpodSpec(t), labOptions(t, l),
		func(context.Context, provision.Session) error { return nil })
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	for _, id := range allPodIDs(f) {
		_, env, _, ok := f.Pod(id)
		if !ok {
			continue
		}
		if !strings.Contains(env["EMBER_HOST_KEY"], "BEGIN OPENSSH PRIVATE KEY") {
			t.Error("the minted host key did not reach the node's environment")
		}
		if env["EMBER_AUTHORIZED_KEY"] == "" {
			t.Error("the operator's public key did not reach the node")
		}
		if env["EMBER_TTL_SECONDS"] == "" {
			t.Error("the node's self-destruct was not armed")
		}
	}
	for _, r := range f.Requests() {
		if r.Method != "POST" {
			continue
		}
		for _, k := range []string{"args", "argsStr", "entrypoint"} {
			if _, found := r.Body[k]; found {
				t.Errorf("the create body carries a command line in %q", k)
			}
		}
	}
}

// TestRunPod_RefusesAndDestroysANodeExposedBeyondThePlatformPort.
//
// ember tolerates RunPod's web terminal mapping because nothing in its node
// image listens there. Anything else published is still a violation, and the
// node is destroyed rather than warned about -- it is already wrong, and
// leaving it up while complaining is the worst of both outcomes.
func TestRunPod_RefusesAndDestroysANodeExposedBeyondThePlatformPort(t *testing.T) {
	p, f, l := runpodLab(t, runpodfake.Options{
		ReadyAfter:      time.Second,
		SSHAfter:        time.Second,
		ExtraPublicPort: 8188,
	})

	err := withNode(t.Context(), p, runpodSpec(t), labOptions(t, l),
		func(context.Context, provision.Session) error {
			t.Error("the workload ran on a publicly exposed node")
			return nil
		})
	if err == nil {
		t.Fatal("a node with an extra public http port was accepted")
	}

	var v *exposure.Violation
	if !errors.As(err, &v) {
		t.Fatalf("got %T (%v), want *exposure.Violation", err, err)
	}
	if !strings.Contains(err.Error(), "8188") {
		t.Errorf("the error does not name the offending port: %v", err)
	}
	if live := f.Live(); len(live) != 0 {
		t.Errorf("%d exposed pod(s) survived: %v", len(live), live)
	}
}

// TestRunPod_ToleratesThePlatformPortAlone is the other half: with only the
// mapping RunPod forces on every pod, the provision goes through. Without
// this the lab above would pass for a provider ember simply cannot use.
func TestRunPod_ToleratesThePlatformPortAlone(t *testing.T) {
	p, _, l := runpodLab(t, runpodfake.Options{ReadyAfter: time.Second, SSHAfter: time.Second})

	ran := false
	err := withNode(t.Context(), p, runpodSpec(t), labOptions(t, l),
		func(context.Context, provision.Session) error {
			ran = true
			return nil
		})
	if err != nil {
		t.Fatalf("a node carrying only RunPod's own mapping was refused: %v", err)
	}
	if !ran {
		t.Error("the workload never ran")
	}
}

// TestRunPod_PinsTheMintedKeyRatherThanTrustingOnFirstUse.
//
// The regression test for the quietest bug ember has had. RunPod reports no
// host key, so reading the provider's answer dropped ember into TOFU against
// a brand-new IP while a warning scrolled past -- and every unit test passed,
// because the fake provider echoed a key back and the local provider really
// does. This fake reports none, exactly like the real service.
//
// **It asserted the absence of a log line nothing emits, for its whole life.**
// The warning is gone -- the fallback is a refusal now, which is right -- and
// what went with it was the only thing this lab was looking at: `"first use"`
// appears in no log call in any package at any level, so the assertion could
// not fire, and reinstating the original defect as a one-line TOFU fallback in
// provision.trustFor left this lab and the other ninety-nine green (#111).
// The dial below could not see it either, and that is worth being precise
// about: the node really does present the key ember injected, so a TOFU trust
// records it and the handshake succeeds. A dial proves the key matches; it
// cannot prove *which mechanism* decided it matched.
//
// So the assertion is the mechanism, and it is made before anything connects:
// a pin refuses a key it does not hold from the first connection, and TOFU
// accepts the first key it is ever shown. The refusal and the dial together
// are the pair -- without the dial the refusal would be consistent with a
// trust that refuses everything, which is what a zero hostkey.Pin is, and
// without the refusal the dial is consistent with both mechanisms.
func TestRunPod_PinsTheMintedKeyRatherThanTrustingOnFirstUse(t *testing.T) {
	p, _, l := runpodLab(t, runpodfake.Options{ReadyAfter: time.Second, SSHAfter: time.Second})

	err := withNode(t.Context(), p, runpodSpec(t), labOptions(t, l),
		func(ctx context.Context, s provision.Session) error {
			// Offered first, while the trust has seen nothing. Under TOFU
			// this is accepted *and pinned*, so the dial below then fails
			// too: the reinstated defect goes red twice rather than once.
			foreign, err := hostkey.Mint()
			if err != nil {
				t.Fatal(err)
			}
			id := s.Node.Handle().ID
			cbErr := hostkey.Callback(s.Trust, id)("h", nil, foreign.Public)
			if cbErr == nil {
				t.Error("the first host key offered to this session was accepted although ember " +
					"did not mint it; that is trust-on-first-use against an address RunPod reports " +
					"no key for, which is the one case TOFU cannot defend")
			} else {
				var mm *hostkey.MismatchError
				if !errors.As(cbErr, &mm) {
					t.Errorf("want a *hostkey.MismatchError for a key ember did not mint, got %T: %v", cbErr, cbErr)
				}
			}

			// And the real node, presenting the real injected key, through a
			// real handshake: the acceptance half, over sshd rather than over
			// a callback ember called itself.
			client := dialSession(t, l, s)
			return client.Close()
		})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
}

// TestRunPod_AStatusItCannotReadIsAnError rather than an assumption. Not
// knowing a pod's state means not knowing whether it is costing money.
func TestRunPod_AStatusItCannotReadIsAnError(t *testing.T) {
	p, _, _ := runpodLab(t, runpodfake.Options{})
	_, err := p.Inspect(t.Context(), node.Handle{Provider: runpod.Name, ID: "nosuchpod"})
	if !errors.Is(err, node.ErrNotFound) {
		t.Fatalf("got %v, want node.ErrNotFound", err)
	}
}

// TestRunPod_OffersComeFromTheCapturedCatalogue, so the fake and the unit
// tests cannot drift apart: both read the same captured response.
func TestRunPod_OffersComeFromTheCapturedCatalogue(t *testing.T) {
	p, _, _ := runpodLab(t, runpodfake.Options{})

	offers, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) == 0 {
		t.Fatal("the captured catalogue produced no offers")
	}
	for _, o := range offers {
		if err := o.Validate(); err != nil {
			t.Errorf("offer %+v does not validate: %v", o, err)
		}
		if o.Price <= 0 {
			t.Errorf("offer %q has no price", o.Token)
		}
	}
}

var _ provider.Provider = (*runpod.Provider)(nil)
