//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/test/integration/harness"
	"github.com/Legion112/ember/test/integration/runpodfake"
)

// cpuSpec is a run that wants a machine with no card: the shape
// `ember store sync` rents to pull weights onto a store, where the job is
// bound by the network and paying for a GPU would cost fifteen times what it
// meant to.
func cpuSpec(t *testing.T, port int) node.Spec {
	t.Helper()
	s, err := node.NewSpec(node.SpecOptions{
		Image:  harness.LabImage,
		TTL:    10 * time.Minute,
		Ports:  []node.PortMap{{Name: "sshd", Remote: 22, Local: uint16(port)}},
		Filter: node.Filter{CPUOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// pickedOffer is PickOffer rather than the withNode helper, because these labs
// are about which offer gets chosen and withNode drops it.
func pickedOffer(t *testing.T, p provider.Provider, spec node.Spec, o provision.Options) node.Offer {
	t.Helper()
	offer, err := provision.PickOffer(t.Context(), p, spec.Filter, spec.TTL, o)
	if err != nil {
		t.Fatalf("pick an offer for %+v: %v", spec.Filter, err)
	}
	return offer
}

// createBody is the one create call ember made, or a failure.
//
// Read off the recorded request rather than from the fake's pod state, so what
// is asserted is the JSON that went to RunPod -- which is the thing the API
// judges and the thing a mapping bug would get wrong.
func createBody(t *testing.T, f *runpodfake.Fake) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, r := range f.Requests() {
		if r.Method == http.MethodPost && r.Path == "/v2/pods" {
			found = append(found, r.Body)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d create calls, want exactly 1", len(found))
	}
	return found[0]
}

// fakeCall sends one request straight at the fake, for the labs that assert on
// what the fake itself answers rather than on what ember does with it. The
// bearer token is any non-empty one: the fake checks the header is there
// because a fake that accepted an unauthenticated call cannot catch ember
// forgetting to send it.
func fakeCall(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	var r *bytes.Reader
	if body != "" {
		r = bytes.NewReader([]byte(body))
	}
	var req *http.Request
	var err error
	if r == nil {
		req, err = http.NewRequestWithContext(t.Context(), method, url, nil)
	} else {
		req, err = http.NewRequestWithContext(t.Context(), method, url, r)
	}
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer fake-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(out)
}

// TestRunPod_CPUOnlyIsExclusiveInBothDirections, read through the two live
// catalogue endpoints rather than through a hand-built offer list.
//
// The money invariant: "a run that wanted a card and silently got a CPU box
// fails at the workload; one that wanted a cheap downloader and got a 4090
// costs fifteen times what it meant to." Both halves are checked here because
// only one of them is reachable through this provider's own branch -- Offers
// asks the CPU endpoint only when CPUOnly is set, so a GPU run never even sees
// a CPU offer to be matched against. That structure is the real defence, and
// it is also why a break in the other direction would go unnoticed at this
// layer: the filter is therefore asked about each catalogue's offers directly,
// which is what makes a one-sided exclusivity check fail this lab.
//
// One asymmetry found by reinstating each half and watching this go red. The
// CPU side is single-locked -- make Match's first line one-sided that way and
// a CPU-only filter matches a 4090, which fails here. The GPU side is
// double-locked: a CPU offer carries GPUCount 0, so `o.GPUCount < want`
// refuses it on its own, and breaking only the exclusivity line in that
// direction fails nothing. That is a property of Match worth knowing rather
// than a hole in this lab -- the assertion is live, as removing both locks
// together does fail it -- but it means the exclusivity line is the only
// thing standing between a cheap downloader and a card, and nothing but
// GPUCount stands the other way.
func TestRunPod_CPUOnlyIsExclusiveInBothDirections(t *testing.T) {
	p, f, _ := runpodLab(t, runpodfake.Options{})

	gpus, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatalf("list gpu offers: %v", err)
	}
	cpus, err := p.Offers(t.Context(), node.Filter{CPUOnly: true})
	if err != nil {
		t.Fatalf("list cpu offers: %v", err)
	}
	if len(gpus) == 0 || len(cpus) == 0 {
		t.Fatalf("the captured catalogues produced %d gpu and %d cpu offers; both must be non-empty or this lab proves nothing", len(gpus), len(cpus))
	}

	cpuWanted := node.Filter{CPUOnly: true}
	gpuWanted := node.Filter{}
	for _, o := range gpus {
		if o.IsCPU() {
			t.Errorf("the gpu catalogue produced a CPU offer: %s", o)
		}
		if cpuWanted.Match(o) {
			t.Errorf("a run asking for a cheap downloader matched %s, which is a card", o)
		}
	}
	for _, o := range cpus {
		if !o.IsCPU() {
			t.Errorf("the cpu catalogue produced a GPU offer: %s", o)
		}
		if gpuWanted.Match(o) {
			t.Errorf("a run asking for a card matched %s, which has none; it would fail at the workload", o)
		}
		if o.VCPU <= 0 {
			t.Errorf("offer %s reports no cores, so IsCPU is true only by accident", o)
		}
		if err := o.Validate(); err != nil {
			t.Errorf("cpu offer %+v does not validate: %v", o, err)
		}
	}

	// A CPU run must not have cost a GPU catalogue read or the reverse: the
	// endpoints are separate calls, and asking both would double the latency
	// of the common case to serve the rare one.
	gpuCalls, cpuCalls := 0, 0
	for _, r := range f.Requests() {
		switch r.Path {
		case "/v2/catalog/gpus":
			gpuCalls++
		case "/v2/catalog/cpus":
			cpuCalls++
		}
		if strings.HasPrefix(r.Path, "/v2/catalog/") {
			// The parameters are the difference between a catalogue with
			// capacity in it and one where every entry is dropped as having
			// none, and the endpoint refuses the pair without product.
			if !strings.Contains(r.Query, "include=AVAILABILITY") || !strings.Contains(r.Query, "product=POD") {
				t.Errorf("%s was asked with %q, want include=AVAILABILITY and product=POD", r.Path, r.Query)
			}
		}
	}
	if gpuCalls != 1 || cpuCalls != 1 {
		t.Errorf("got %d gpu and %d cpu catalogue reads, want one each", gpuCalls, cpuCalls)
	}
}

// TestRunPod_ACPUOnlyRunRentsAMachineWithNoCard, end to end through a create.
//
// The direction that costs money. Until the fake served /v2/catalog/cpus this
// could not be written at all: the request 404'd, so cpuOffers had never run
// against the fake and nothing had ever watched ember turn a CPU offer into a
// create body. The body is where the mistake would land -- `gpu` and `cpu` are
// mutually exclusive on this API and exactly one must be set, so a CPU-only
// run that sent `gpu` would either be refused or, worse, be given a card.
func TestRunPod_ACPUOnlyRunRentsAMachineWithNoCard(t *testing.T) {
	port := harness.FreePort(t)
	p, f, l := runpodLab(t, runpodfake.Options{ReadyAfter: time.Second, SSHAfter: time.Second})
	opts := labOptions(t, l)

	spec := cpuSpec(t, port)
	offer := pickedOffer(t, p, spec, opts)
	if !offer.IsCPU() {
		t.Fatalf("a CPU-only filter picked %s", offer)
	}
	// The cheapest family in the captured catalogue, at its two-core minimum:
	// $0.03 per vCPU per hour against $0.89 for an RTX 4090, which is the
	// whole argument for renting one of these to fill a store.
	if offer.Token != "cpu3c" {
		t.Errorf("picked %q, want the cheapest family cpu3c", offer.Token)
	}
	if want := node.RateFromUSD(0.06); offer.Price != want {
		t.Errorf("offer priced at %v, want %v (two cores at $0.03 each)", offer.Price, want)
	}

	ran := false
	err := provision.WithNode(t.Context(), p, spec, offer, opts,
		func(ctx context.Context, s provision.Session) error {
			// Dialled, because `ember store sync` runs its download over this
			// connection: a CPU rental ember cannot reach is a CPU rental that
			// bills and does nothing.
			client := dialSession(t, l, s)
			ran = true
			return client.Close()
		})
	if err != nil {
		t.Fatalf("a CPU-only provision failed: %v", err)
	}
	if !ran {
		t.Error("the workload never ran on the CPU node")
	}

	body := createBody(t, f)
	if _, found := body["gpu"]; found {
		t.Errorf("a CPU-only run asked RunPod for a card: gpu=%v", body["gpu"])
	}
	cpu, ok := body["cpu"].(map[string]any)
	if !ok {
		t.Fatalf("the create body names no cpu configuration: %v", body)
	}
	if cpu["id"] != "cpu3c" {
		t.Errorf("created cpu %v, want cpu3c", cpu["id"])
	}
	// vcpuCount is required and bounded by the family's own min and max, which
	// is why the offer carries it rather than the create picking a number.
	if n, _ := cpu["vcpuCount"].(float64); int(n) != offer.VCPU {
		t.Errorf("created %v cores, want the offer's %d", cpu["vcpuCount"], offer.VCPU)
	}

	if live := f.Live(); len(live) != 0 {
		t.Errorf("%d CPU pod(s) survived: %v", len(live), live)
	}
}

// TestRunPod_ClampsACPUPodsContainerDisk.
//
// A CPU pod refuses a container disk above 20 GiB and the schema publishes no
// maximum for the field: the limit is enforced at the handler and was found by
// a 400. ember clamps rather than refuses, because the disk is incidental to
// what a CPU machine is rented for -- the download lands on the mounted store,
// and the container's own disk only has to hold the image.
//
// Until the fake answered that 400 this was exercised against canned bodies
// only, so removing the clamp broke nothing anybody could see. The fake now
// refuses the pod, which is what makes the create below evidence of the clamp
// rather than of nothing.
func TestRunPod_ClampsACPUPodsContainerDisk(t *testing.T) {
	port := harness.FreePort(t)
	p, f, l := runpodLab(t, runpodfake.Options{ReadyAfter: time.Second, SSHAfter: time.Second})
	// More than a CPU pod will take, which is not an odd thing to configure:
	// the default is 50, because a GPU run that fills its disk mid-pull fails
	// after it has already started billing.
	p.DiskGiB = 100
	opts := labOptions(t, l)

	spec := cpuSpec(t, port)
	offer := pickedOffer(t, p, spec, opts)
	err := provision.WithNode(t.Context(), p, spec, offer, opts,
		func(context.Context, provision.Session) error { return nil })
	if err != nil {
		t.Fatalf("a CPU pod asking for 100 GiB of disk was refused rather than clamped: %v", err)
	}

	if disk, _ := createBody(t, f)["disk"].(float64); int(disk) != 20 {
		t.Errorf("ember asked for %v GiB of container disk, want it clamped to 20", disk)
	}
}

// TestRunPod_AGPUPodsContainerDiskIsNotClamped is the other half, and the
// reason the clamp is a property of the product rather than a ceiling that
// could simply be lowered for both. A GPU pod has no such limit, and model
// weights are why 50 is the default: one video checkpoint is tens of
// gigabytes.
func TestRunPod_AGPUPodsContainerDiskIsNotClamped(t *testing.T) {
	port := harness.FreePort(t)
	p, f, l := runpodLab(t, runpodfake.Options{ReadyAfter: time.Second, SSHAfter: time.Second})
	p.DiskGiB = 100
	opts := labOptions(t, l)

	spec := runpodSpec(t, port)
	offer := pickedOffer(t, p, spec, opts)
	if offer.IsCPU() {
		t.Fatalf("a GPU filter picked %s", offer)
	}
	if err := provision.WithNode(t.Context(), p, spec, offer, opts,
		func(context.Context, provision.Session) error { return nil }); err != nil {
		t.Fatalf("provision: %v", err)
	}

	if disk, _ := createBody(t, f)["disk"].(float64); int(disk) != 100 {
		t.Errorf("a GPU pod asked for %v GiB, want the configured 100 untouched", disk)
	}
}

// TestRunPodFake_RefusesAnOversizedCPUDisk is the check on the fake itself.
//
// The clamp lab above proves something only while the service it runs against
// would have said no. A fake that quietly accepted 100 GiB would turn that lab
// into an assertion about a number ember chose for reasons nothing enforces --
// the same defect as the DELETE of an already-deleted pod answering 200 while
// the live API 404s, which let a destroy-is-idempotent check pass with the
// not-found path never taken.
//
// It runs against a fake with no data plane, because a regression that made
// this create succeed must not start a container from a lab that is already
// failing.
func TestRunPodFake_RefusesAnOversizedCPUDisk(t *testing.T) {
	f := runpodfake.New(t, nil, runpodfake.Options{})

	post := func(t *testing.T, body string) (int, string) {
		t.Helper()
		return fakeCall(t, http.MethodPost, f.URL+"/v2/pods", body)
	}

	code, body := post(t, `{"name":"x","image":"y","cpu":{"id":"cpu3c","vcpuCount":2},"disk":100}`)
	if code != http.StatusBadRequest {
		t.Errorf("a CPU pod with 100 GiB of disk got status %d, want 400", code)
	}
	if !strings.Contains(body, "Container Disk must be less than or equal to 20") {
		t.Errorf("the refusal does not carry the API's own message: %s", body)
	}

	// At the limit it is accepted, or the clamp would be tested against a
	// service that refuses the value ember clamps to.
	if code, body := post(t, `{"name":"x","image":"y","cpu":{"id":"cpu3c","vcpuCount":2},"disk":20}`); code != http.StatusOK {
		t.Errorf("a CPU pod with exactly 20 GiB got status %d (%s), want 200", code, body)
	}
	// And a GPU pod is not subject to it at all.
	if code, body := post(t, `{"name":"x","image":"y","gpu":{"id":"NVIDIA GeForce RTX 4090","count":1},"disk":100}`); code != http.StatusOK {
		t.Errorf("a GPU pod with 100 GiB got status %d (%s), want 200", code, body)
	}
}

// TestRunPodFake_CatalogueRefusesAvailabilityWithoutProduct.
//
// The endpoint will not answer what is available without being told which
// product it is about -- stock differs between pods and serverless -- and the
// refusal is a 400 rather than a quietly reduced answer. Reproduced because
// the query lives in one constant per endpoint feeding three call sites, and a
// parameter dropped there yields a priced catalogue with no capacity in it,
// which reads to an operator as "that card is sold out everywhere".
func TestRunPodFake_CatalogueRefusesAvailabilityWithoutProduct(t *testing.T) {
	f := runpodfake.New(t, nil, runpodfake.Options{})

	get := func(t *testing.T, path string) (int, string) {
		t.Helper()
		return fakeCall(t, http.MethodGet, f.URL+path, "")
	}

	for _, path := range []string{"/v2/catalog/cpus", "/v2/catalog/gpus"} {
		if code, _ := get(t, path+"?include=AVAILABILITY"); code != http.StatusBadRequest {
			t.Errorf("GET %s?include=AVAILABILITY got status %d, want 400", path, code)
		}
		code, body := get(t, path+"?include=AVAILABILITY&product=POD")
		if code != http.StatusOK {
			t.Errorf("GET %s with both parameters got status %d: %s", path, code, body)
		}
		if !strings.Contains(body, `"availability"`) {
			t.Errorf("GET %s asked for availability and got none: %s", path, body)
		}
		// Not asked for, not reported: the levels exist because include asks
		// for them, and ember drops an entry nothing says has capacity.
		if _, body := get(t, path+"?product=POD"); strings.Contains(body, `"availability"`) {
			t.Errorf("GET %s reported availability nobody asked for: %s", path, body)
		}
	}
}
