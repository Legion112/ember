package local

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

func newProvider(t *testing.T) (*Provider, *dockerengine.RecordingEngine) {
	t.Helper()
	e := dockerengine.NewRecordingEngine()
	e.AddImage(DefaultImage)
	return New(e), e
}

func request(t *testing.T) node.CreateRequest {
	t.Helper()
	spec, err := node.NewSpec(node.SpecOptions{
		Image: "ghcr.io/example/comfyui:v1",
		TTL:   time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	offer := node.Offer{Provider: Name, Token: "local", GPU: "none", GPUCount: 1}
	req, err := node.NewCreateRequest(spec, offer, node.NewRunID(), time.Now(),
		"ssh-ed25519 AAAAoperator ember",
		node.NewSecret([]byte("-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n")),
		node.HostKey{Source: node.HostKeyMinted, Algo: "ssh-ed25519", Base64: "AAAAhostkey"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestOffers_IsFreeAndHasNoGPU(t *testing.T) {
	p, _ := newProvider(t)

	got, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d offers, want 1", len(got))
	}
	if got[0].Price != 0 {
		t.Errorf("the local provider must be free, got %v", got[0].Price)
	}
	if !p.Caps().Free {
		t.Error("Caps must say so, or provision cannot skip the budget check honestly")
	}
}

// TestOffers_DoesNotPretendToHaveAGPU: handing back a CPU container for
// --gpu rtx4090 would make a benchmark meaningless, and the operator would
// have no way to tell from the output.
func TestOffers_DoesNotPretendToHaveAGPU(t *testing.T) {
	p, _ := newProvider(t)
	for name, f := range map[string]node.Filter{
		"a named gpu": {GPU: "rtx4090"},
		"any vram":    {MinVRAM: 1},
	} {
		got, err := p.Offers(t.Context(), f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("%s: got %v, want nothing", name, got)
		}
	}
}

func TestOffers_ReportsAnUnreachableDaemon(t *testing.T) {
	p, e := newProvider(t)
	e.Errors["Ping"] = errors.New("cannot connect to the docker daemon")

	_, err := p.Offers(t.Context(), node.Filter{})
	if err == nil {
		t.Fatal("Offers succeeded with no daemon")
	}
	if !strings.Contains(err.Error(), "docker daemon") {
		t.Errorf("got %v", err)
	}
}

func TestCreate_LabelsEverythingAndInjectsKeysThroughEnv(t *testing.T) {
	p, e := newProvider(t)
	req := request(t)

	h, err := p.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if h.Provider != Name || h.ID == "" {
		t.Fatalf("handle = %v", h)
	}

	cs := e.Containers()
	if len(cs) != 1 {
		t.Fatalf("got %d containers", len(cs))
	}
	c := cs[0]
	if c.Labels[dockerengine.LabelRunID] != string(req.RunID) {
		t.Error("the run id label is what makes an orphan findable; it is missing")
	}
	if c.Labels[LabelKind] != "node" {
		t.Errorf("kind label = %q", c.Labels[LabelKind])
	}
	if got := c.Labels[LabelHostKey]; got != "ssh-ed25519 AAAAhostkey" {
		t.Errorf("host key label = %q; without it nothing can be pinned", got)
	}
	if _, _, err := provider.ParseLabel(c.Labels[LabelLabel]); err != nil {
		t.Errorf("the ownership label does not parse back: %v", err)
	}

	nets := e.Networks()
	if len(nets) != 1 || nets[0].Labels[dockerengine.LabelRunID] != string(req.RunID) {
		t.Errorf("the network is unlabelled, so reap could not find it: %v", nets)
	}
}

// TestCreate_RefusesAnOfferFromAnotherProvider: bidding a Vast ask id at the
// local daemon is a programming error, not something to improvise around.
func TestCreate_RefusesAnOfferFromAnotherProvider(t *testing.T) {
	p, _ := newProvider(t)
	req := request(t)
	req.Offer.Provider = "runpod"
	if _, err := p.Create(t.Context(), req); err == nil {
		t.Fatal("an offer from another provider was accepted")
	}
}

// TestCreate_RefusesAMissingImageWithTheFix rather than pulling one: the image
// is built from this repository and a silent pull would fetch somebody else's.
func TestCreate_RefusesAMissingImageWithTheFix(t *testing.T) {
	e := dockerengine.NewRecordingEngine() // no image added
	p := New(e)
	_, err := p.Create(t.Context(), request(t))
	if err == nil {
		t.Fatal("Create succeeded with no image")
	}
	if !strings.Contains(err.Error(), "make docker-build") {
		t.Errorf("the error should name the command that fixes it, got %v", err)
	}
}

// TestCreate_LeavesNothingBehindWhenItFails is the money property in
// miniature: a half-created node is one nothing will clean up.
func TestCreate_LeavesNothingBehindWhenItFails(t *testing.T) {
	for name, failOn := range map[string]string{
		"network create fails":   "NetworkCreate",
		"container create fails": "ContainerCreate",
		"container start fails":  "ContainerStart",
	} {
		p, e := newProvider(t)
		e.FailOn = failOn

		if _, err := p.Create(t.Context(), request(t)); err == nil {
			t.Errorf("%s: Create reported success", name)
			continue
		}
		if got := e.Containers(); len(got) != 0 {
			t.Errorf("%s: left %d containers behind", name, len(got))
		}
		if got := e.Networks(); len(got) != 0 {
			t.Errorf("%s: left %d networks behind", name, len(got))
		}
	}
}

func TestCreate_ReportsAFailedImageCheck(t *testing.T) {
	p, e := newProvider(t)
	e.Errors["ImageExists "+DefaultImage] = errors.New("daemon broke")
	if _, err := p.Create(t.Context(), request(t)); err == nil {
		t.Fatal("Create continued after it could not check for the image")
	}
}

// TestInspect_RunningWithoutAPublishedPortIsNotReady is the invariant the
// whole ReadyNode type exists for. Docker reports a container as running the
// instant it starts, which is before the port map is readable.
func TestInspect_RunningWithoutAPublishedPortIsNotReady(t *testing.T) {
	p, e := newProvider(t)
	e.PortMap[sshPort] = 0 // published, but the daemon has no host port yet

	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Inspect(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if n.State != node.StateProvisioning {
		t.Fatalf("state = %v, want provisioning", n.State)
	}
	if _, ok := n.Endpoint(); ok {
		t.Error("a node with no published port must not report an endpoint")
	}
	if _, err := node.Ready(n); err == nil {
		t.Error("Ready accepted a node with no endpoint")
	}
}

func TestInspect_ReadyCarriesAUsableEndpoint(t *testing.T) {
	p, e := newProvider(t)
	e.PortMap[sshPort] = 49213

	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Inspect(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	r, err := node.Ready(n)
	if err != nil {
		t.Fatalf("a started container should be ready: %v", err)
	}
	ep := r.Endpoint()
	if ep.Host != "127.0.0.1" || ep.Port != 49213 || ep.User != "root" {
		t.Errorf("endpoint = %v", ep)
	}
	// And the host key must come back so it can be pinned.
	if n.HostKey.IsZero() || n.HostKey.Source != node.HostKeyMinted {
		t.Errorf("host key = %+v", n.HostKey)
	}
	if n.Deadline.IsZero() {
		t.Error("the deadline did not survive the label; reap would treat this node as expired")
	}
}

func TestInspect_MissingNodeIsErrNotFound(t *testing.T) {
	p, _ := newProvider(t)
	_, err := p.Inspect(t.Context(), node.Handle{Provider: Name, ID: "ghost"})
	if !errors.Is(err, node.ErrNotFound) {
		t.Fatalf("got %v, want node.ErrNotFound so destroy can treat it as success", err)
	}
}

func TestInspect_ReportsAnExitedWorkload(t *testing.T) {
	p, e := newProvider(t)
	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	e.SetState(h.ID, "exited", 7)

	n, err := p.Inspect(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if n.State != node.StateFailed {
		t.Errorf("state = %v, want failed", n.State)
	}
	if !strings.Contains(n.Message, "7") {
		t.Errorf("the exit code belongs in the message, got %q", n.Message)
	}
}

// TestInspect_RefusesAnUnrecognisedState: not knowing whether a node is
// running is, on a paid provider, the difference between a bill and no bill.
func TestInspect_RefusesAnUnrecognisedState(t *testing.T) {
	p, e := newProvider(t)
	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	e.SetState(h.ID, "quantum", 0)

	if _, err := p.Inspect(t.Context(), h); err == nil {
		t.Fatal("an unrecognised container state was mapped to something")
	}
}

func TestInspect_MapsEveryKnownState(t *testing.T) {
	for state, want := range map[string]node.State{
		"created":    node.StateProvisioning,
		"restarting": node.StateProvisioning,
		"running":    node.StateReady,
		"paused":     node.StateStopped,
		"exited":     node.StateFailed,
		"dead":       node.StateFailed,
		"removing":   node.StateDestroyed,
	} {
		p, e := newProvider(t)
		h, err := p.Create(t.Context(), request(t))
		if err != nil {
			t.Fatal(err)
		}
		e.SetState(h.ID, state, 0)
		n, err := p.Inspect(t.Context(), h)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if n.State != want {
			t.Errorf("%q mapped to %v, want %v", state, n.State, want)
		}
	}
}

func TestDestroy_RemovesTheContainerThenTheNetwork(t *testing.T) {
	p, e := newProvider(t)
	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if len(e.Containers()) != 0 || len(e.Networks()) != 0 {
		t.Fatalf("destroy left %d containers and %d networks", len(e.Containers()), len(e.Networks()))
	}

	// The order matters: removing the network first fails while the container
	// is attached, and the error would be about the network rather than about
	// the node the operator asked to destroy.
	calls := strings.Join(e.Recorded(), "\n")
	cIdx := strings.Index(calls, "ContainerRemove")
	nIdx := strings.Index(calls, "NetworkRemove")
	if cIdx < 0 || nIdx < 0 || cIdx > nIdx {
		t.Errorf("wrong teardown order:\n%s", calls)
	}
}

// TestDestroy_IsIdempotent: the goal of destroy is that the node is gone, and
// if it is already gone then it is.
func TestDestroy_IsIdempotent(t *testing.T) {
	p, _ := newProvider(t)
	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(t.Context(), h); err != nil {
		t.Fatalf("destroying twice: %v", err)
	}
	if err := p.Destroy(t.Context(), node.Handle{Provider: Name, ID: "never-existed"}); err != nil {
		t.Fatalf("destroying something that never existed: %v", err)
	}
}

func TestDestroy_SurfacesRealFailures(t *testing.T) {
	for name, failOn := range map[string]string{
		"inspect fails":          "ContainerInspect",
		"container remove fails": "ContainerRemove",
		"network list fails":     "NetworkList",
		"network remove fails":   "NetworkRemove",
	} {
		p, e := newProvider(t)
		h, err := p.Create(t.Context(), request(t))
		if err != nil {
			t.Fatal(err)
		}
		e.FailOn = failOn
		if err := p.Destroy(t.Context(), h); err == nil {
			t.Errorf("%s: Destroy reported success; the record would be cleared while the node still runs", name)
		}
	}
}

func TestList_FindsNodesByLabel(t *testing.T) {
	p, e := newProvider(t)
	if _, err := p.Create(t.Context(), request(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(t.Context(), request(t)); err != nil {
		t.Fatal(err)
	}

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d nodes, want 2", len(got))
	}

	e.FailOn = "ContainerList"
	if _, err := p.List(t.Context()); err == nil {
		t.Error("List hid a daemon failure; \"I could not check\" must not read as \"nothing is running\"")
	}
}

func TestList_SurfacesAContainerItCannotMap(t *testing.T) {
	p, e := newProvider(t)
	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	e.SetState(h.ID, "quantum", 0)

	// A container wearing ember's label that does not map is still running
	// and still ember's, so it is a finding rather than something to drop.
	if _, err := p.List(t.Context()); err == nil {
		t.Fatal("List silently dropped a node it could not understand")
	}
}

func TestProvider_Defaults(t *testing.T) {
	p := New(dockerengine.NewRecordingEngine())
	p.Image, p.User = "", ""
	if p.image() != DefaultImage {
		t.Errorf("image() = %q", p.image())
	}
	if p.user() != "root" {
		t.Errorf("user() = %q", p.user())
	}
	if p.Name() != Name {
		t.Errorf("Name() = %q", p.Name())
	}
}

// TestProviderSatisfiesTheInterface keeps the seam honest at compile time.
var _ provider.Provider = (*Provider)(nil)
