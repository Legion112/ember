package local

import (
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

func newProvider(t *testing.T) (*Provider, *dockerengine.RecordingEngine) {
	t.Helper()
	e := dockerengine.NewRecordingEngine()
	e.AddImage(DefaultImage)
	e.AddImage(specImage)
	return New(e), e
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
	p.Store = s.Handle
	p.Mount = mustMount(t, "/models")

	h, err := p.Create(t.Context(), request(t))
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
	p.Store, p.Mount = s.Handle, mustMount(t, "/models")

	h, err := p.Create(ctx, request(t))
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
