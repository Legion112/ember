package local

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
)

// specImage is what request() asks for. The provider runs the spec's image
// now rather than its own default, so the fake has to know about it.
const specImage = "ghcr.io/example/comfyui:v1"

// newProvider returns a provider that is certain it has no GPU.
//
// Pinned rather than detected, because otherwise every assertion in this file
// would mean something different on a machine with a graphics card than on
// one without -- and the machine this repository is developed on has one
// while CI does not. A unit test whose result depends on the hardware under
// it is not a unit test.
func newProvider(t *testing.T) (*Provider, *dockerengine.RecordingEngine) {
	t.Helper()
	e := dockerengine.NewRecordingEngine()
	e.AddImage(DefaultImage)
	e.AddImage(specImage)
	p := New(e)
	p.Detect = func(context.Context) GPU { return GPU{} }
	return p, e
}

// testGPU is the card the GPU-flavoured tests pretend this machine has. The
// real one it was modelled on is an 11 GiB 2080 Ti.
var testGPU = GPU{Model: "rtx2080ti", VRAM: 11264, ComputeCap: "7.5"}

// newGPUProvider returns a provider with testGPU and a daemon that can pass
// it through.
//
// Both, because both are required and they fail independently -- the machine
// this was developed on had the card and not the runtime.
func newGPUProvider(t *testing.T) (*Provider, *dockerengine.RecordingEngine) {
	t.Helper()
	p, e := newProvider(t)
	p.Detect = func(context.Context) GPU { return testGPU }
	e.GPURuntime = true
	return p, e
}

func request(t *testing.T) node.CreateRequest {
	t.Helper()
	spec, err := node.NewSpec(node.SpecOptions{
		Image: specImage,
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
	// Keyed on the spec's image, which is the one the provider now checks.
	e.Errors["ImageExists "+specImage] = errors.New("daemon broke")
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

// TestInspect_LoopbackPublishingIsNotExposure. The local provider binds
// published ports to 127.0.0.1, so nothing beyond this machine can reach the
// node and it has no public surface to report.
func TestInspect_LoopbackPublishingIsNotExposure(t *testing.T) {
	p, _ := newProvider(t)

	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Inspect(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}

	es, reported := n.Exposure()
	if !reported {
		t.Fatal("the local provider did not report its exposure, so every assertion over it fails")
	}
	if len(es) != 0 {
		t.Errorf("a loopback-published port was reported as public: %v", es)
	}
	if err := exposure.AssertNoPublishedPorts(n); err != nil {
		t.Errorf("a correctly bound local node failed the assertion: %v", err)
	}
}

// TestInspect_AWideBindIsReportedAsExposure is the bug this plumbing exists to
// catch. Docker's own default is to publish on 0.0.0.0, and a node whose sshd
// is reachable from the whole LAN must not pass an exposure assertion just
// because ember's create call asked for loopback. The report is derived from
// the IP the daemon gives back, so it tracks what actually happened.
func TestInspect_AWideBindIsReportedAsExposure(t *testing.T) {
	p, e := newProvider(t)
	e.BindIP = "0.0.0.0"

	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Inspect(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}

	es, reported := n.Exposure()
	if !reported {
		t.Fatal("exposure was not reported")
	}
	if len(es) != 1 || es[0].Port != 22 {
		t.Fatalf("a 0.0.0.0 binding was not reported as public: %v", es)
	}
	if err := exposure.AssertNoPublishedPorts(n); err == nil {
		t.Fatal("a LAN-reachable node passed the assertion")
	}
}

// TestPublicPorts_IsOrderedAndFiltered. The ordering is not cosmetic: the
// bindings come out of a map, so without the sort the violation message would
// list ports in a different order on each run, and an operator comparing two
// runs could not tell whether anything had changed.
func TestPublicPorts_IsOrderedAndFiltered(t *testing.T) {
	es := publicPorts(dockerengine.Container{Ports: map[uint16]dockerengine.Binding{
		8188: {IP: "0.0.0.0", Port: 48188},
		22:   {IP: "0.0.0.0", Port: 40022},
		9090: {IP: "127.0.0.1", Port: 49090}, // loopback: not exposure
		7000: {IP: "::1", Port: 47000},       // loopback in its other spelling
	}})

	if len(es) != 2 {
		t.Fatalf("got %v, want the two 0.0.0.0 bindings", es)
	}
	if es[0].Port != 22 || es[1].Port != 8188 {
		t.Errorf("not ordered by port: %v", es)
	}
	if es[0].URL != "0.0.0.0:40022" {
		t.Errorf("the host binding is missing from the report: %v", es[0])
	}
}

// TestPublicPorts_ReportsNoneRatherThanNil, so a container publishing nothing
// is distinguishable from one that was never asked.
func TestPublicPorts_ReportsNoneRatherThanNil(t *testing.T) {
	n := node.Node{}.WithExposed(publicPorts(dockerengine.Container{}))
	if _, reported := n.Exposure(); !reported {
		t.Fatal("a container with no ports reads as unreported")
	}
}

// TestCreate_RunsTheSpecsImage. The flag means the same thing on every
// provider now. It did not before: this provider ignored the spec and always
// ran its own image, so `--image` was silently dropped here while the
// identical flag on RunPod chose the node image. A flag that works on one
// provider and is quietly discarded on another is worse than one that fails.
func TestCreate_RunsTheSpecsImage(t *testing.T) {
	p, e := newProvider(t)

	if _, err := p.Create(t.Context(), request(t)); err != nil {
		t.Fatal(err)
	}
	cs := e.Containers()
	if len(cs) != 1 {
		t.Fatalf("got %d containers", len(cs))
	}
	if cs[0].Image != specImage {
		t.Errorf("ran %q, want the spec's image %q", cs[0].Image, specImage)
	}
}

// TestCreate_FallsBackToTheNodeImageWithoutASpecImage, which is the shape the
// provider's own construction uses when nothing named one.
func TestCreate_FallsBackToTheNodeImageWithoutASpecImage(t *testing.T) {
	p, e := newProvider(t)
	req := request(t)
	req.Spec.Image = ""

	if _, err := p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	cs := e.Containers()
	if len(cs) != 1 || cs[0].Image != DefaultImage {
		t.Fatalf("got %+v, want the default node image", cs)
	}
}

// TestCreate_NamesTheMissingImageAndHowToGetIt. A pull would fetch somebody
// else's image under the same name; a failure that says which image and what
// to run is the honest alternative.
func TestCreate_NamesTheMissingImageAndHowToGetIt(t *testing.T) {
	e := dockerengine.NewRecordingEngine() // no images at all
	p := New(e)

	_, err := p.Create(t.Context(), request(t))
	if err == nil {
		t.Fatal("Create ran with no image on the daemon")
	}
	if !strings.Contains(err.Error(), specImage) {
		t.Errorf("the error does not name the image: %v", err)
	}
	if !strings.Contains(err.Error(), "make docker-build") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func TestLocal_StoreCapsMatchImplementation(t *testing.T) {
	// Checked here rather than in internal/provider, which cannot import a
	// provider without a cycle. Every provider calls this, so a Caps.Stores
	// set without a StoreAPI -- or a StoreAPI nothing will ever ask for --
	// fails next to the provider that got it wrong.
	providertest.AssertStoreCapsMatch(t, New(dockerengine.NewRecordingEngine()))
}

func TestLocal_StoreLifecycle(t *testing.T) {
	p, _ := newProvider(t)
	ctx := t.Context()

	if got, err := p.StoreList(ctx); err != nil || len(got) != 0 {
		t.Fatalf("StoreList on a fresh engine = %+v, %v", got, err)
	}

	s, err := p.StoreCreate(ctx, "models", 40, "EU-RO-1")
	if err != nil {
		t.Fatalf("StoreCreate = %v", err)
	}
	if s.Name != "models" {
		t.Errorf("name = %q", s.Name)
	}
	if string(s.Handle.ID) != "ember-store-models" {
		t.Errorf("the volume is not named by the label ember parses: %q", s.Handle.ID)
	}
	// Free, and said so rather than left at a zero that happens to mean the
	// same thing -- the storage equivalent of Caps.Free.
	if s.Price != 0 || s.Monthly() != 0 {
		t.Errorf("a local store costs %v/%v", s.Price, s.Monthly())
	}

	// Idempotent, as the daemon is. `ember store create` run twice must not
	// be an error, or a config applied to a fresh machine and an existing one
	// behaves differently.
	if _, err := p.StoreCreate(ctx, "models", 40, "EU-RO-1"); err != nil {
		t.Errorf("a second StoreCreate = %v, want success", err)
	}

	got, err := p.StoreList(ctx)
	if err != nil || len(got) != 1 || got[0].Name != "models" {
		t.Fatalf("StoreList = %+v, %v", got, err)
	}

	if err := p.StoreDestroy(ctx, s.Handle); err != nil {
		t.Fatalf("StoreDestroy = %v", err)
	}
	if got, _ = p.StoreList(ctx); len(got) != 0 {
		t.Errorf("after destroy: %+v", got)
	}
	// Gone already is the state the caller asked for.
	if err := p.StoreDestroy(ctx, s.Handle); err != nil {
		t.Errorf("a second StoreDestroy = %v, want success", err)
	}
}

func TestLocal_StoreRefusals(t *testing.T) {
	p, _ := newProvider(t)
	if _, err := p.StoreCreate(t.Context(), "", 10, ""); err == nil {
		t.Error("an empty store name was accepted")
	}
	if err := p.StoreDestroy(t.Context(), node.StoreHandle{Provider: Name}); err == nil {
		t.Error("an empty handle was accepted")
	}
}

func TestLocal_StoreListIgnoresVolumesEmberDidNotName(t *testing.T) {
	// A volume carrying ember's kind label but not a name ember wrote is
	// skipped, the same way a foreign volume at RunPod is. `ember store rm`
	// acts on what this returns.
	p, eng := newProvider(t)
	if _, err := eng.VolumeCreate(t.Context(), "someone-elses-cache",
		map[string]string{LabelKind: "store"}); err != nil {
		t.Fatal(err)
	}
	got, err := p.StoreList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing ember can claim", got)
	}
}

func TestLocal_CreateMountsTheStore(t *testing.T) {
	// The assertion the recorder could not make before: Binds reached the
	// real client and nothing else, so a node created with a store looked
	// exactly like one created without.
	p, eng := newProvider(t)
	s, err := p.StoreCreate(t.Context(), "models", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	h, err := p.Create(t.Context(), storeRequest(t, s.Handle, "/models"))
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	c, err := eng.ContainerInspect(t.Context(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := "ember-store-models:/models"
	if len(c.Binds) != 1 || c.Binds[0] != want {
		t.Errorf("binds = %v, want [%s]", c.Binds, want)
	}
}

func TestLocal_CreateWithoutAStoreMountsNothing(t *testing.T) {
	p, eng := newProvider(t)
	h, err := p.Create(t.Context(), request(t))
	if err != nil {
		t.Fatal(err)
	}
	c, err := eng.ContainerInspect(t.Context(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Binds) != 0 {
		t.Errorf("binds = %v on a node with no store", c.Binds)
	}
}

func TestLocal_DestroyingANodeLeavesTheStore(t *testing.T) {
	// The property that makes a store a store. A node is torn down at the end
	// of every run; the weights on it are the thing that must not be.
	p, _ := newProvider(t)
	ctx := t.Context()
	s, err := p.StoreCreate(ctx, "models", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	h, err := p.Create(ctx, storeRequest(t, s.Handle, "/models"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(ctx, h); err != nil {
		t.Fatal(err)
	}
	got, err := p.StoreList(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("after destroying the node the store is %+v (%v); it must outlive the run", got, err)
	}
}

// storeRequest is a create request that mounts a store, built the way the CLI
// builds one: on the spec, so the provider carries no per-run state.
func storeRequest(t *testing.T, h node.StoreHandle, path string) node.CreateRequest {
	t.Helper()
	req := request(t)
	spec := req.Spec
	spec.Store, spec.Mount = h, mustMount(t, path)
	req.Spec = spec
	return req
}

func mustMount(t *testing.T, p string) node.Mount {
	t.Helper()
	m, err := node.MountAt(p)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLocal_StoreReportsEngineFailures(t *testing.T) {
	ctx := t.Context()

	create, _ := newProvider(t)
	create.Engine.(*dockerengine.RecordingEngine).FailOn = "VolumeCreate"
	if _, err := create.StoreCreate(ctx, "models", 10, ""); err == nil {
		t.Error("a failed VolumeCreate reported success")
	}

	list, _ := newProvider(t)
	list.Engine.(*dockerengine.RecordingEngine).FailOn = "VolumeList"
	if _, err := list.StoreList(ctx); err == nil {
		t.Error("a failed VolumeList reported success")
	}

	// A destroy that fails for a reason other than "already gone" is an
	// error: the volume is still there and still holding whatever is on it.
	del, _ := newProvider(t)
	if _, err := del.StoreCreate(ctx, "models", 10, ""); err != nil {
		t.Fatal(err)
	}
	del.Engine.(*dockerengine.RecordingEngine).FailOn = "VolumeRemove"
	err := del.StoreDestroy(ctx, node.StoreHandle{Provider: Name, ID: "ember-store-models"})
	if err == nil {
		t.Error("a failed VolumeRemove reported success")
	}
}

func TestLocal_StoreListSkipsOtherEmberResources(t *testing.T) {
	// Networks carry ember.kind too. Filtering on the key's presence is not
	// the same as filtering on its value, and a network listed as a store
	// would be a store `ember store rm` tried to delete.
	p, eng := newProvider(t)
	if _, err := eng.VolumeCreate(t.Context(), "ember-store-models",
		map[string]string{LabelKind: "network"}); err != nil {
		t.Fatal(err)
	}
	got, err := p.StoreList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing: that volume is not a store", got)
	}
}

// TestOffers_ReportsARealLocalGPU. The only free GPU offer ember has, and
// what makes "iterate locally, render once on a rented card" expressible.
func TestOffers_ReportsARealLocalGPU(t *testing.T) {
	p, _ := newGPUProvider(t)

	got, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d offers, want 1", len(got))
	}
	o := got[0]
	if o.GPU != testGPU.Model {
		t.Errorf("GPU = %q, want %q", o.GPU, testGPU.Model)
	}
	if o.VRAM != testGPU.VRAM {
		t.Errorf("VRAM = %d, want %d", o.VRAM, testGPU.VRAM)
	}
	// Still free. The budget check is skipped because Caps().Free says so,
	// not because a real price happened to be zero.
	if o.Price != 0 {
		t.Errorf("price = %v, want free", o.Price)
	}
}

// TestOffers_MatchesOnlyWhatTheCardCanActuallyDo.
//
// Handing back a 2080 Ti for `--gpu rtx4090` would be worse than handing back
// a CPU container, because the run would look like it worked.
func TestOffers_MatchesOnlyWhatTheCardCanActuallyDo(t *testing.T) {
	p, _ := newGPUProvider(t)

	for name, tc := range map[string]struct {
		f     node.Filter
		match bool
	}{
		"the card it has":        {node.Filter{GPU: "rtx2080ti"}, true},
		"a different card":       {node.Filter{GPU: "rtx4090"}, false},
		"vram it has":            {node.Filter{MinVRAM: 11264}, true},
		"less vram than it has":  {node.Filter{MinVRAM: 8192}, true},
		"more vram than it has":  {node.Filter{MinVRAM: 24576}, false},
		"no constraint at all":   {node.Filter{}, true},
		"the whole rented model": {node.Filter{MinVRAM: 81920}, false},
	} {
		got, err := p.Offers(t.Context(), tc.f)
		if err != nil {
			t.Fatal(err)
		}
		if (len(got) > 0) != tc.match {
			t.Errorf("%s: got %d offers, want match=%v", name, len(got), tc.match)
		}
	}
}

// TestCreate_HandsTheCardToTheContainer.
//
// The assertion the recorder exists for. A node created without a device
// request does not fail -- it comes up, runs the workload on the CPU, and
// looks exactly like a successful run.
func TestCreate_HandsTheCardToTheContainer(t *testing.T) {
	p, e := newGPUProvider(t)

	req := request(t)
	offers, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	req.Offer = offers[0]

	h, err := p.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.ContainerInspect(t.Context(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.GPUs != 1 {
		t.Errorf("the container was created with %d GPU(s), want 1; a node with no card "+
			"runs the workload on the CPU and looks like it worked", c.GPUs)
	}
}

// TestCreate_AsksForNoCardOnACPUOffer. The reverse, and it matters: the
// offer is what the operator was shown, so a CPU run must not quietly get a
// card either.
func TestCreate_AsksForNoCardOnACPUOffer(t *testing.T) {
	p, e := newProvider(t) // no GPU detected
	req := request(t)
	offers, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	req.Offer = offers[0]

	h, err := p.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.ContainerInspect(t.Context(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.GPUs != 0 {
		t.Errorf("a CPU offer produced a container with %d GPU(s)", c.GPUs)
	}
}

func TestGpusFor(t *testing.T) {
	for name, tc := range map[string]struct {
		o    node.Offer
		want int
	}{
		"a card":            {node.Offer{GPU: "rtx2080ti", VRAM: 11264, GPUCount: 1}, 1},
		"two cards":         {node.Offer{GPU: "rtx2080ti", VRAM: 11264, GPUCount: 2}, 2},
		"count unset":       {node.Offer{GPU: "rtx2080ti", VRAM: 11264}, 1},
		"the none sentinel": {node.Offer{GPU: "none", GPUCount: 1}, 0},
		"empty model":       {node.Offer{VRAM: 11264}, 0},
		// VRAM zero with a model name is not a card ember can use, and asking
		// the daemon for a device on that basis would be asking on no
		// evidence.
		"no vram": {node.Offer{GPU: "rtx2080ti", GPUCount: 1}, 0},
	} {
		if got := gpusFor(tc.o); got != tc.want {
			t.Errorf("%s: gpusFor = %d, want %d", name, got, tc.want)
		}
	}
}

// TestOffers_WillNotPromiseACardDockerCannotPassThrough.
//
// The state this machine was actually in: an 11 GiB 2080 Ti, and no
// nvidia-container-toolkit. nvidia-smi sees the card; the daemon refuses the
// device request with "could not select device driver". Advertising it would
// be a promise ember cannot keep, found at create, after a config was written
// around it.
func TestOffers_WillNotPromiseACardDockerCannotPassThrough(t *testing.T) {
	p, e := newGPUProvider(t)
	e.GPURuntime = false // the card is there; Docker cannot hand it over

	got, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d offers, want the CPU one", len(got))
	}
	if got[0].GPU != "none" || got[0].VRAM != 0 {
		t.Errorf("offered %s/%d MiB on a daemon with no nvidia runtime", got[0].GPU, got[0].VRAM)
	}

	// And a filter asking for the card gets a refusal rather than a CPU
	// container wearing its name. The refusal is explanatory -- see
	// TestOffers_ExplainsACardDockerCannotReach -- because "no capacity"
	// with a working GPU in the machine is not a usable thing to read.
	none, err := p.Offers(t.Context(), node.Filter{GPU: testGPU.Model})
	if err == nil {
		t.Errorf("a request for %s matched %v", testGPU.Model, none)
	}
	if len(none) != 0 {
		t.Errorf("a request for %s matched %v", testGPU.Model, none)
	}
}

// TestOffers_SurvivesADaemonThatCannotAnswer.
//
// The CPU offer is still good. Failing the whole listing over a GPU question
// would break every lab on every machine that never had a card.
func TestOffers_SurvivesADaemonThatCannotAnswer(t *testing.T) {
	p, e := newGPUProvider(t)
	e.Errors["HasGPURuntime"] = errors.New("daemon said no")

	got, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatalf("Offers failed because the GPU question did: %v", err)
	}
	if len(got) != 1 || got[0].GPU != "none" {
		t.Errorf("got %v, want a plain CPU offer", got)
	}
}

// TestOffers_ExplainsACardDockerCannotReach.
//
// "no capacity" is a baffling thing to read with a working GPU in the
// machine, and the fix is one package away. The runpod provider names the
// datacentre it could not place in; this names what to install.
func TestOffers_ExplainsACardDockerCannotReach(t *testing.T) {
	p, e := newGPUProvider(t)
	e.GPURuntime = false

	_, err := p.Offers(t.Context(), node.Filter{MinVRAM: 8192})
	if err == nil {
		t.Fatal("a GPU request on a daemon that cannot pass one through returned no error")
	}
	if !errors.Is(err, node.ErrNoCapacity) {
		t.Errorf("got %v, want it to wrap ErrNoCapacity", err)
	}
	for _, want := range []string{"rtx2080ti", "nvidia-container-toolkit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// A run that wants no GPU is unaffected: the CPU offer is still good,
	// and refusing it would break every lab on a machine with a card.
	got, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatalf("a CPU request was refused because of a GPU problem: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d offers for a CPU request, want 1", len(got))
	}
}
