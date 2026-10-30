package runpod

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/httpx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
)

// newTestProvider points a real Provider at an httptest.Server.
//
// BaseURL plus *http.Client is the seam, so every layer under test is the
// production one: the real httpx client, its real timeout, its real
// body-close behaviour. A RoundTripper would skip exactly those.
func newTestProvider(t *testing.T, h http.HandlerFunc) *Provider {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return &Provider{HTTP: httpx.Client{
		BaseURL: s.URL,
		Token:   node.NewSecret([]byte("rp-test-token")),
		HTTP:    s.Client(),
	}}
}

func serveFile(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}
}

func TestProvider_NameAndCaps(t *testing.T) {
	p := New(node.NewSecret([]byte("x")))
	if p.Name() != Name {
		t.Errorf("name = %q", p.Name())
	}
	if p.HTTP.BaseURL != BaseURL {
		t.Errorf("base URL = %q", p.HTTP.BaseURL)
	}

	c := p.Caps()
	// Reached over a public 22/tcp mapping, so AssertSSHOnly applies rather
	// than the stricter AssertNoPublishedPorts.
	if !c.PublicSSH {
		t.Error("RunPod must declare PublicSSH, or the strict exposure assertion rejects every pod")
	}
	if !c.HostKeyFromAPI {
		t.Error("ember injects a minted host key through env, so the first dial is verified")
	}
	if c.Free {
		t.Error("RunPod is not free, and a Free provider skips the budget check")
	}
	// Secure cloud is RunPod's own hardware and is not reclaimed underneath
	// the operator; community hosts are somebody else's machines.
	if c.Interruptible {
		t.Error("secure cloud should not be marked interruptible")
	}
	if (&Provider{Cloud: "COMMUNITY"}).Caps().Interruptible != true {
		t.Error("community cloud must be marked interruptible")
	}
}

func TestProvider_CloudDefaultsToSecure(t *testing.T) {
	// SECURE matches RunPod's own default. COMMUNITY is cheaper and runs on
	// hardware somebody else owns, which is a choice to make deliberately
	// rather than inherit from a zero value.
	if got := (&Provider{}).cloud(); got != cloudSecure {
		t.Errorf("got %q", got)
	}
	if got := (&Provider{Cloud: "community"}).cloud(); got != cloudCommunity {
		t.Errorf("got %q", got)
	}
}

func TestProvider_DiskDefault(t *testing.T) {
	gpu := node.Offer{GPU: "rtx4090", GPUCount: 1}
	if got := (&Provider{}).disk(gpu); got != DefaultDiskGiB {
		t.Errorf("got %d", got)
	}
	if got := (&Provider{DiskGiB: 200}).disk(gpu); got != 200 {
		t.Errorf("got %d", got)
	}
}

func TestProvider_DiskIsClampedOnACPUMachine(t *testing.T) {
	// Measured, not documented: the schema puts no maximum on the field and
	// the API refuses the pod with "Container Disk must be less than or
	// equal to 20". GPU pods have no such ceiling, so this is a property of
	// the product rather than a default that could be lowered for both.
	cpu := node.Offer{Token: "cpu3c", VCPU: 2}

	if got := (&Provider{}).disk(cpu); got != CPUMaxDiskGiB {
		t.Errorf("the default %d GiB was not clamped: got %d", DefaultDiskGiB, got)
	}
	if got := (&Provider{DiskGiB: 200}).disk(cpu); got != CPUMaxDiskGiB {
		t.Errorf("got %d, want the CPU ceiling", got)
	}
	// Clamped, not raised: a smaller request is honoured.
	if got := (&Provider{DiskGiB: 10}).disk(cpu); got != 10 {
		t.Errorf("got %d, want the smaller request honoured", got)
	}
}

func TestCreate_SendsCPUOrGPUButNeverBoth(t *testing.T) {
	// The API refuses a pod that asks for both, so the two fields are
	// pointers and exactly one is filled. The offer decides, which is why
	// there is nothing to keep in step.
	gpu := node.Offer{Provider: Name, Token: "NVIDIA GeForce RTX 4090", GPU: "rtx4090", GPUCount: 1}
	if g, c := gpuFor(gpu), cpuFor(gpu); g == nil || c != nil {
		t.Errorf("a GPU offer gave gpu=%+v cpu=%+v", g, c)
	}
	cpu := node.Offer{Provider: Name, Token: "cpu3c", VCPU: 4}
	g, c := gpuFor(cpu), cpuFor(cpu)
	if g != nil || c == nil {
		t.Fatalf("a CPU offer gave gpu=%+v cpu=%+v", g, c)
	}
	if c.ID != "cpu3c" || c.VcpuCount != 4 {
		t.Errorf("cpu config = %+v", c)
	}
}

func TestOffers_MapsTheCatalogue(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		serveFile(t, "gpus.json")(w, r)
	})

	offers, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 2 {
		t.Fatalf("got %d offers: %+v", len(offers), offers)
	}
	if gotPath != "/v2/catalog/gpus" {
		t.Errorf("path = %q", gotPath)
	}
	// Availability is requested, so an offer ember returns is one RunPod says
	// it has rather than one it merely has a price for.
	if !strings.Contains(gotQuery, "include=AVAILABILITY") || !strings.Contains(gotQuery, "product=POD") {
		t.Errorf("query = %q", gotQuery)
	}
	if gotAuth != "Bearer rp-test-token" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestOffers_AppliesTheFilter(t *testing.T) {
	p := newTestProvider(t, serveFile(t, "gpus.json"))

	// "a100" accepts "a100-80g": the capacity suffix makes the catalogue's
	// name more specific than what an operator types.
	offers, err := p.Offers(t.Context(), node.Filter{GPU: node.NormalizeGPU("a100")})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 1 || !strings.Contains(string(offers[0].Token), "A100 80GB") {
		t.Fatalf("got %+v", offers)
	}
}

func TestOffers_ReportsAFailure(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	})
	_, err := p.Offers(t.Context(), node.Filter{})
	if err == nil {
		t.Fatal("a 401 was reported as success")
	}
	// The provider's own message is the only thing that says what is wrong.
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("the API's message is missing: %v", err)
	}
}

func request(t *testing.T) node.CreateRequest {
	t.Helper()
	spec, err := node.NewSpec(node.SpecOptions{
		Image: "ghcr.io/example/comfyui:v0.3.1",
		TTL:   time.Hour,
		Ports: []node.PortMap{{Name: "comfyui", Remote: 8188, Local: 8188}},
	})
	if err != nil {
		t.Fatal(err)
	}
	offer := node.Offer{
		Provider: Name, Token: "NVIDIA GeForce RTX 4090",
		GPU: node.NormalizeGPU("rtx4090"), GPUCount: 1, Price: node.Rate(690000),
	}
	req, err := node.NewCreateRequest(spec, offer, node.NewRunID(), time.Now(),
		"ssh-ed25519 AAAAoperator ember",
		node.NewSecret([]byte("-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----\n")),
		node.HostKey{Source: node.HostKeyMinted, Algo: "ssh-ed25519", Base64: "AAAAhostkey"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestCreate_SendsTheRightBody(t *testing.T) {
	var body createPodRequest
	var gotPath, gotMethod string

	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"newpod1","name":"` + body.Name + `","status":"PROVISIONING"}`))
	})

	req := request(t)
	h, err := p.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if h.Provider != Name || h.ID != "newpod1" {
		t.Errorf("handle = %+v", h)
	}
	if gotMethod != http.MethodPost || gotPath != "/v2/pods" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}

	// The label lives in the pod name, because RunPod has no tag API. This is
	// what makes an orphan findable without consulting local state.
	if _, _, err := provider.ParseLabel(body.Name); err != nil {
		t.Errorf("the pod name does not carry ember's label: %q (%v)", body.Name, err)
	}
	if body.Image != req.Spec.Image {
		t.Errorf("image = %q", body.Image)
	}
	if body.Gpu.ID != "NVIDIA GeForce RTX 4090" || body.Gpu.Count != 1 {
		t.Errorf("gpu = %+v", body.Gpu)
	}
	if body.Cloud != cloudSecure {
		t.Errorf("cloud = %q", body.Cloud)
	}
	if body.Disk != DefaultDiskGiB {
		t.Errorf("disk = %d", body.Disk)
	}
	// ember runs its own sshd with the key it minted; RunPod's own sshd would
	// present a key ember cannot predict, forcing TOFU against a new IP.
	if body.StartSsh {
		t.Error("startSsh was set, so RunPod's sshd would own the host key")
	}
	// A template carries ports and env from somebody else's defaults, and one
	// that gains an http port upstream would reintroduce public exposure with
	// no change here.
	if body.TemplateId != "" {
		t.Errorf("templateId = %q, want none", body.TemplateId)
	}
}

// TestCreate_DeclaresOnly22TCP. The single most dangerous field in this API:
// a port declared /http is served with no authentication at all.
func TestCreate_DeclaresOnly22TCP(t *testing.T) {
	var body createPodRequest
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"x","status":"PROVISIONING"}`))
	})

	if _, err := p.Create(t.Context(), request(t)); err != nil {
		t.Fatal(err)
	}
	if len(body.Ports) != 1 || body.Ports[0] != "22/tcp" {
		t.Fatalf("ports = %v, want exactly [22/tcp]", body.Ports)
	}
	for _, s := range body.Ports {
		if strings.Contains(s, "http") {
			t.Errorf("an http port was declared: %q", s)
		}
	}
}

// TestCreate_KeyMaterialTravelsInTheBodyNeverInArgv. /proc/<pid>/cmdline is
// world-readable, and on rented hardware the other tenants of that host are
// strangers.
func TestCreate_KeyMaterialTravelsInTheBodyNeverInArgv(t *testing.T) {
	var raw []byte
	var body createPodRequest
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ = readAll(r)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"id":"x","status":"PROVISIONING"}`))
	})

	if _, err := p.Create(t.Context(), request(t)); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(body.Env["EMBER_HOST_KEY"], "BEGIN OPENSSH PRIVATE KEY") {
		t.Error("the minted host key did not reach the node's environment")
	}
	if body.Env["EMBER_AUTHORIZED_KEY"] == "" {
		t.Error("the operator's public key did not reach the node")
	}
	if body.Env["EMBER_TTL_SECONDS"] == "" {
		t.Error("the node's self-destruct was not armed; a dead laptop would leave it billing")
	}
	// Nothing on the command line, which is the part that would be readable
	// by every other user on that machine.
	if strings.Contains(string(raw), "\"args\"") || strings.Contains(string(raw), "\"entrypoint\"") {
		t.Errorf("the create body carries a command line: %s", raw)
	}
}

// TestCreate_A2xxWithNoIDIsAnError, and a loud one. A pod may exist that ember
// cannot name, which is the worst case there is.
func TestCreate_A2xxWithNoIDIsAnError(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"PROVISIONING"}`))
	})

	_, err := p.Create(t.Context(), request(t))
	if err == nil {
		t.Fatal("a create with no pod id was accepted")
	}
	// The operator needs the label to go looking with, and the command.
	if !strings.Contains(err.Error(), "ember ls") {
		t.Errorf("the error does not say how to find the pod: %v", err)
	}
}

func TestCreate_ReportsAFailure(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"no capacity for NVIDIA GeForce RTX 4090"}`))
	})
	_, err := p.Create(t.Context(), request(t))
	if err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Fatalf("got %v", err)
	}
}

func TestInspect_MapsAReadyPod(t *testing.T) {
	var gotPath string
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		serveFile(t, "pod_running.json")(w, r)
	})

	n, err := p.Inspect(t.Context(), node.Handle{Provider: Name, ID: "abc123def456"})
	if err != nil {
		t.Fatal(err)
	}
	// A single fetch by id, never a filtered list: RunPod returns null for
	// ssh.direct and runtime on every list entry, so a list-based Inspect
	// would report every ready pod as provisioning forever.
	if gotPath != "/v2/pods/abc123def456" {
		t.Errorf("path = %q", gotPath)
	}
	if n.State != node.StateReady {
		t.Errorf("state = %v", n.State)
	}
}

func TestInspect_NotFoundIsErrNotFound(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"pod not found"}`))
	})

	// provision and reconcile both branch on this sentinel, so it has to be
	// matchable with errors.Is rather than being a status buried in a string.
	_, err := p.Inspect(t.Context(), node.Handle{Provider: Name, ID: "gone"})
	if !errors.Is(err, node.ErrNotFound) {
		t.Fatalf("got %v, want node.ErrNotFound", err)
	}
}

func TestInspect_ReportsOtherFailures(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := p.Inspect(t.Context(), node.Handle{Provider: Name, ID: "x"})
	if err == nil {
		t.Fatal("a 500 was reported as success")
	}
	if errors.Is(err, node.ErrNotFound) {
		t.Error("a 500 was reported as not-found, which would have reap forget a live pod")
	}
}

// TestInspect_EscapesTheIDOnTheWire. A handle is provider data, so it is
// escaped rather than trusted to be path-safe.
//
// The assertion is on RequestURI and not on URL.Path, because net/http decodes
// the path before a handler sees it: %2F comes back as a literal slash, so
// URL.Path would show traversal for a request that was correctly escaped and
// the test would be measuring Go's decoder rather than ember's encoder. What
// ember controls, and all it controls, is what goes on the wire.
func TestInspect_EscapesTheIDOnTheWire(t *testing.T) {
	var gotURI string
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		_, _ = w.Write([]byte(`{"id":"x","status":"PROVISIONING"}`))
	})

	if _, err := p.Inspect(t.Context(), node.Handle{Provider: Name, ID: "a/../../b"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotURI, "/../") {
		t.Errorf("an unescaped traversal went on the wire: %q", gotURI)
	}
	if !strings.Contains(gotURI, "%2F") {
		t.Errorf("the slash was not escaped: %q", gotURI)
	}
}

// TestList_SkipsPodsThatAreNotEmbers. Somebody else's pod in the same account
// is not ember's to report, let alone to reap.
func TestList_SkipsPodsThatAreNotEmbers(t *testing.T) {
	p := newTestProvider(t, serveFile(t, "pods_list.json"))

	ns, err := p.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("got %d nodes, want only ember's: %+v", len(ns), ns)
	}
	if ns[0].Handle.ID != "abc123def456" {
		t.Errorf("got %+v", ns[0].Handle)
	}
	// List entries carry no endpoint, which is what RunPod returns and all
	// reconcile needs.
	if _, ok := ns[0].Endpoint(); ok {
		t.Error("a list entry reported an endpoint RunPod does not send")
	}
}

func TestList_ReportsAnUnrecognisedStatusWithoutHidingTheRest(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"pods":[
			{"id":"ok1","name":"ember-0123456789abcdef-1792900000","status":"RUNNING"},
			{"id":"bad","name":"ember-fedcba9876543210-1792900000","status":"HIBERNATING"}
		]}`))
	})

	ns, err := p.List(t.Context())
	if err == nil {
		t.Fatal("an unrecognised status was accepted; ember cannot tell whether that pod is billing")
	}
	// Returned alongside what was understood: one bad pod must not hide the
	// rest of the account.
	if len(ns) != 1 || ns[0].Handle.ID != "ok1" {
		t.Errorf("the understood pods were discarded: %+v", ns)
	}
}

func TestList_ReportsAFailure(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, err := p.List(t.Context()); err == nil {
		t.Fatal("a 502 was reported as success")
	}
}

func TestDestroy_NotFoundIsSuccess(t *testing.T) {
	var gotMethod, gotPath string
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	})

	// The goal of destroy is that the pod is gone, and it is.
	if err := p.Destroy(t.Context(), node.Handle{Provider: Name, ID: "gone"}); err != nil {
		t.Fatalf("destroying an absent pod failed: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v2/pods/gone" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
}

func TestDestroy_ReportsARealFailure(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	})
	// A destroy that failed must be loud: provision writes PhaseDestroyFailed
	// on it, and that record is never cleared automatically.
	if err := p.Destroy(t.Context(), node.Handle{Provider: Name, ID: "x"}); err == nil {
		t.Fatal("a 500 from destroy was reported as success")
	}
}

// TestProvider_SatisfiesTheInterface at compile time.
var _ provider.Provider = (*Provider)(nil)

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

// TestCreate_RefusesAWorkloadPortOfZero, at the provider edge rather than
// trusting the spec to have been validated somewhere upstream.
func TestCreate_RefusesAWorkloadPortOfZero(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the create call reached RunPod with a bad port set")
		_, _ = w.Write([]byte(`{"id":"x","status":"PROVISIONING"}`))
	})

	req := request(t)
	req.Spec.Ports = []node.PortMap{{Name: "broken", Remote: 0}}
	if _, err := p.Create(t.Context(), req); err == nil {
		t.Fatal("a zero workload port was accepted")
	}
}

// TestCreate_MultiGPUAsksForTheCountTheOfferCarries, and never zero: RunPod's
// minimum is 1, and a body asking for 0 GPUs is rejected at the API with a
// message about a field the operator never set.
func TestCreate_MultiGPUAsksForTheCountTheOfferCarries(t *testing.T) {
	var body createPodRequest
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"x","status":"PROVISIONING"}`))
	})

	req := request(t)
	req.Offer.GPUCount = 4
	if _, err := p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if body.Gpu.Count != 4 {
		t.Errorf("gpu count = %d, want 4", body.Gpu.Count)
	}

	req.Offer.GPUCount = 0
	if _, err := p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if body.Gpu.Count != 1 {
		t.Errorf("gpu count = %d, want 1 for an offer that did not say", body.Gpu.Count)
	}
}

func TestCreate_PassesTheConfiguredCloudAndDataCentres(t *testing.T) {
	var body createPodRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"x","status":"PROVISIONING"}`))
	}))
	defer s.Close()

	p := &Provider{
		HTTP:        httpx.Client{BaseURL: s.URL, HTTP: s.Client()},
		Cloud:       "COMMUNITY",
		DataCenters: []string{"EU-SE-1"},
		DiskGiB:     120,
	}
	if _, err := p.Create(t.Context(), request(t)); err != nil {
		t.Fatal(err)
	}
	if body.Cloud != cloudCommunity {
		t.Errorf("cloud = %q", body.Cloud)
	}
	if len(body.DataCenterIds) != 1 || body.DataCenterIds[0] != "EU-SE-1" {
		t.Errorf("dataCenterIds = %v", body.DataCenterIds)
	}
	if body.Disk != 120 {
		t.Errorf("disk = %d", body.Disk)
	}
}

func TestInspect_ReportsAnUnrecognisedStatus(t *testing.T) {
	p := newTestProvider(t, serveFile(t, "pod_unknown_status.json"))
	if _, err := p.Inspect(t.Context(), node.Handle{Provider: Name, ID: "weird9"}); err == nil {
		t.Fatal("an unrecognised status was accepted")
	}
}

// TestEveryCallHitsThePathTheSpecDocuments.
//
// A regression test for the first live call this package ever made, which
// went to https://api.runpod.io/v2/v2/catalog/gpus. BaseURL ended in "/v2"
// because RunPod's migration guide describes the base that way, while the
// request paths were copied from the OpenAPI document, whose paths already
// begin with /v2. Each half was defensible and together they doubled it.
//
// Asserting the whole path rather than the suffix is the point: a test that
// checked only "contains /catalog/gpus" would have passed on the broken URL.
// The expectations below are the spec's own path strings.
func TestEveryCallHitsThePathTheSpecDocuments(t *testing.T) {
	if BaseURL != "https://api.runpod.io" {
		t.Fatalf("BaseURL = %q: the /v2 belongs to the paths, so adding it here doubles it", BaseURL)
	}

	for _, tc := range []struct {
		name string
		want string
		call func(*Provider) error
	}{
		{"offers", "/v2/catalog/gpus", func(p *Provider) error {
			_, err := p.Offers(t.Context(), node.Filter{})
			return err
		}},
		{"create", "/v2/pods", func(p *Provider) error {
			_, err := p.Create(t.Context(), request(t))
			return err
		}},
		{"inspect", "/v2/pods/abc123", func(p *Provider) error {
			_, err := p.Inspect(t.Context(), node.Handle{Provider: Name, ID: "abc123"})
			return err
		}},
		{"list", "/v2/pods", func(p *Provider) error {
			_, err := p.List(t.Context())
			return err
		}},
		{"destroy", "/v2/pods/abc123", func(p *Provider) error {
			return p.Destroy(t.Context(), node.Handle{Provider: Name, ID: "abc123"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"abc123","status":"PROVISIONING","gpus":[],"pods":[]}`))
			})
			if err := tc.call(p); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("%s hit %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestCaps_DeclaresRunPodsWebTerminalPort, which ember's first real provision
// found by refusing the node and destroying it.
func TestCaps_DeclaresRunPodsWebTerminalPort(t *testing.T) {
	ports := New(node.NewSecret([]byte("x"))).Caps().PlatformPorts
	if len(ports) != 1 {
		t.Fatalf("got %d platform ports, want exactly the web terminal: %+v", len(ports), ports)
	}
	if ports[0].Port != WebTerminalPort || ports[0].Proto != node.ProtoHTTP {
		t.Errorf("got %+v", ports[0])
	}
	// The reason travels with it, because an operator reading an exposure
	// report deserves to know what ember decided to tolerate and why.
	if !strings.Contains(ports[0].Why, "web terminal") {
		t.Errorf("Why = %q", ports[0].Why)
	}
}

// TestCreate_RefusesAWorkloadOnThePlatformPort. ember tolerates 19123 only
// because nothing listens there; a workload binding it is precisely the case
// that stops being true, and RunPod's proxy would serve it with no
// authentication at all.
func TestCreate_RefusesAWorkloadOnThePlatformPort(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the create call reached RunPod with a workload on the proxy port")
		_, _ = w.Write([]byte(`{"id":"x","status":"PROVISIONING"}`))
	})

	req := request(t)
	req.Spec.Ports = []node.PortMap{{Name: "comfyui", Remote: WebTerminalPort, Local: 8188}}

	_, err := p.Create(t.Context(), req)
	if err == nil {
		t.Fatal("a workload on the public proxy port was accepted")
	}
	if !strings.Contains(err.Error(), "proxy.runpod.net") {
		t.Errorf("the error does not say what makes this dangerous: %v", err)
	}
}

// TestCaps_BillsPerSecondWithNoMinimum.
//
// Measured against a real invoice rather than assumed. Four pods of 37s,
// 165s, 49s and 67s at $0.89/hour: RunPod charged $0.08, the per-second total
// is $0.0787, and a one-minute-minimum total would be $0.0870. The first is
// within the dashboard's rounding and the second is 0.7 cents out -- exactly
// what a minute's floor adds to the two sub-minute pods.
//
// The field said a minute before, carried over from general knowledge about
// GPU providers and never checked against a bill.
func TestCaps_BillsPerSecondWithNoMinimum(t *testing.T) {
	if got := New(node.NewSecret([]byte("x"))).Caps().MinBilling; got != 0 {
		t.Errorf("MinBilling = %v, want 0: RunPod bills per second", got)
	}

	// A short pod therefore costs what it ran for, not a minute.
	start := time.Now()
	b := node.Estimate(node.Rate(890_000), start, start.Add(37220*time.Millisecond), 0)
	if b.Billable != b.Elapsed {
		t.Errorf("billable %v != elapsed %v; a floor was applied", b.Billable, b.Elapsed)
	}
	// 37.22s at $0.89/hr.
	if want := node.MicroUSD(9203); b.Cost < want-5 || b.Cost > want+5 {
		t.Errorf("cost = %v (%d micro), want about %d", b.Cost, int64(b.Cost), int64(want))
	}
	if strings.Contains(b.String(), "minimum") {
		t.Errorf("a minimum was reported where none applies: %q", b.String())
	}
}

func TestRunPod_StoreCapsMatchImplementation(t *testing.T) {
	providertest.AssertStoreCapsMatch(t, New(node.NewSecret([]byte("k"))))
}
