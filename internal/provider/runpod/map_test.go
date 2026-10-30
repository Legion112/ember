package runpod

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/node"
)

// fixture decodes a committed response.
//
// Through the real wire structs, so a field renamed upstream shows up as a
// zero value in an assertion here rather than at runtime with a GPU attached.
func fixture[T any](t *testing.T, name string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return out
}

func TestMapStatus(t *testing.T) {
	for in, want := range map[string]node.State{
		"PROVISIONING": node.StateProvisioning,
		"STARTING":     node.StateProvisioning,
		"RUNNING":      node.StateReady,
		"EXITED":       node.StateFailed,
		"ERROR":        node.StateFailed,
		"TERMINATED":   node.StateDestroyed,
	} {
		got, err := mapStatus(in)
		if err != nil {
			t.Errorf("mapStatus(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("mapStatus(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestMapStatus_RefusesAnythingUnlisted. An unrecognised status means ember
// cannot tell whether money is burning, and the safe reading of that is
// "fail and say so", never "assume it is fine".
func TestMapStatus_RefusesAnythingUnlisted(t *testing.T) {
	for _, in := range []string{"", "HIBERNATING", "running", "Running", "PAUSED", "UNKNOWN"} {
		got, err := mapStatus(in)
		if err == nil {
			t.Errorf("mapStatus(%q) was accepted as %v", in, got)
		}
		if got != node.StateInvalid {
			t.Errorf("mapStatus(%q) returned %v alongside its error, want StateInvalid", in, got)
		}
	}
}

// TestMapStatus_AnUnknownStatusIsNeverQuietlyBillable. The error path must not
// hand back a state that reads as destroyed, or an unrecognised status would
// be silently excluded from every cost report.
func TestMapStatus_AnUnknownStatusIsNeverQuietlyBillable(t *testing.T) {
	got, _ := mapStatus("HIBERNATING")
	if !got.Billable() {
		t.Error("an unrecognised status reads as not billable, so ember would stop watching it")
	}
}

func TestMapPod_AReadyPod(t *testing.T) {
	n, err := mapPod(fixture[pod](t, "pod_running.json"))
	if err != nil {
		t.Fatal(err)
	}

	if n.State != node.StateReady {
		t.Errorf("state = %v", n.State)
	}
	if n.Handle.Provider != Name || n.Handle.ID != "abc123def456" {
		t.Errorf("handle = %+v", n.Handle)
	}

	ep, ok := n.Endpoint()
	if !ok {
		t.Fatal("a ready pod has no endpoint")
	}
	// The direct endpoint, not the proxy: RunPod documents the proxy as
	// carrying an interactive shell only, and port forwarding is the entire
	// mechanism ember is built on.
	if ep.Host != "213.173.98.7" || ep.Port != 40022 || ep.User != "root" {
		t.Errorf("endpoint = %+v; the proxy was used instead of ssh.direct?", ep)
	}

	// The label lives in the pod name, since RunPod has no tag API.
	if n.RunID != "0123456789abcdef" {
		t.Errorf("run id = %q", n.RunID)
	}
	if n.Deadline.Unix() != 1792900000 {
		t.Errorf("deadline = %v", n.Deadline)
	}

	// The rate RunPod says it is billing, as exact integer microdollars.
	if n.Offer.Price != node.Rate(690000) {
		t.Errorf("price = %v (%d micro), want $0.69/hr", n.Offer.Price, int64(n.Offer.Price))
	}
	if n.Offer.Region != "US-TX-3" {
		t.Errorf("region = %q", n.Offer.Region)
	}
	if n.Offer.GPU != node.NormalizeGPU("NVIDIA GeForce RTX 4090") {
		t.Errorf("gpu = %q", n.Offer.GPU)
	}
}

// TestMapPod_AReadyPodPassesTheExposureAssertion, which is the whole point of
// reading the runtime ports back.
func TestMapPod_AReadyPodPassesTheExposureAssertion(t *testing.T) {
	n, err := mapPod(fixture[pod](t, "pod_running.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := exposure.AssertSSHOnly(n); err != nil {
		t.Fatalf("a correctly exposed pod was refused: %v", err)
	}
}

// TestMapPod_AnHTTPPortFailsTheExposureAssertion is the case the whole
// exposure package exists for: on RunPod a port declared /http is served at
// {id}-{port}.proxy.runpod.net with no authentication, in front of a ComfyUI
// that has arbitrary code execution through custom nodes.
func TestMapPod_AnHTTPPortFailsTheExposureAssertion(t *testing.T) {
	n, err := mapPod(fixture[pod](t, "pod_exposed_http.json"))
	if err != nil {
		t.Fatal(err)
	}

	es, reported := n.Exposure()
	if !reported {
		t.Fatal("the pod's exposure was not reported")
	}
	if len(es) != 2 {
		t.Fatalf("got %v, want both public ports", es)
	}

	err = exposure.AssertSSHOnly(n)
	if err == nil {
		t.Fatal("a pod with a public 8188/http port was accepted")
	}
	if !strings.Contains(err.Error(), "8188/http") {
		t.Errorf("the error does not name the offending port: %v", err)
	}
}

// TestMapPod_ProvisioningHasNoEndpointAndNoExposureReport. RunPod leaves
// runtime null until the pod is RUNNING, and "RunPod did not say" must not
// read as "RunPod published nothing" -- that is the difference between an
// unverified node and a verified-safe one.
func TestMapPod_ProvisioningHasNoEndpointAndNoExposureReport(t *testing.T) {
	n, err := mapPod(fixture[pod](t, "pod_provisioning.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n.State != node.StateProvisioning {
		t.Errorf("state = %v", n.State)
	}
	if _, ok := n.Endpoint(); ok {
		t.Error("a provisioning pod reported an endpoint")
	}
	if _, reported := n.Exposure(); reported {
		t.Error("a pod with no runtime reported its exposure, so an unverified node would pass the assertion")
	}
}

// TestMapPod_RunningWithoutSSHIsStillProvisioning. RunPod sets the status to
// RUNNING before it assigns the public 22/tcp mapping that ssh.direct needs.
// Treating that gap as ready would hand WaitReady a node with no endpoint;
// treating it as failure would abort a pod that is about to work.
func TestMapPod_RunningWithoutSSHIsStillProvisioning(t *testing.T) {
	n, err := mapPod(fixture[pod](t, "pod_running_no_ssh.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n.State != node.StateProvisioning {
		t.Fatalf("state = %v, want provisioning", n.State)
	}
	if _, ok := n.Endpoint(); ok {
		t.Error("an endpoint was reported without ssh.direct")
	}
	// The operator reads this line when a provision is slow, so it has to
	// say which side is waiting.
	if !strings.Contains(n.Message, "22/tcp") {
		t.Errorf("message = %q", n.Message)
	}
}

func TestMapPod_AnExitedPodHasFailed(t *testing.T) {
	n, err := mapPod(fixture[pod](t, "pod_exited.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n.State != node.StateFailed {
		t.Errorf("state = %v", n.State)
	}
	// Failed is still billable: an exited pod on RunPod still holds its
	// volume, and ember must keep it in sight until it is destroyed.
	if !n.State.Billable() {
		t.Error("a failed pod reads as not billable")
	}
}

func TestMapPod_AnUnknownStatusIsAnError(t *testing.T) {
	_, err := mapPod(fixture[pod](t, "pod_unknown_status.json"))
	if err == nil {
		t.Fatal("a pod with an unrecognised status was mapped")
	}
	if !strings.Contains(err.Error(), "HIBERNATING") {
		t.Errorf("the error does not name the status: %v", err)
	}
}

// TestMapPod_APodThatIsNotEmbersGetsNoRunIDOrDeadline. The label is parsed
// strictly and never substring-matched: reap destroys what this identifies,
// and somebody else's pod in the same account is not an ember orphan.
func TestMapPod_APodThatIsNotEmbersGetsNoRunIDOrDeadline(t *testing.T) {
	p := fixture[pod](t, "pod_running.json")
	p.Name = "my-training-pod"

	n, err := mapPod(p)
	if err != nil {
		t.Fatal(err)
	}
	if n.RunID != "" {
		t.Errorf("run id = %q, want empty", n.RunID)
	}
	if !n.Deadline.IsZero() {
		t.Errorf("deadline = %v, want zero", n.Deadline)
	}
}

func TestMapExposure_SkipsPortsWithNoPublicMapping(t *testing.T) {
	eight := 8188
	ip := "213.173.98.7"
	p := pod{ID: "x", Runtime: &podRuntime{Ports: []runtimePort{
		{Private: 22, Public: &eight, Type: "tcp", IP: &ip},
		// No public mapping: reachable only inside the pod's own network,
		// which is what ember asks for for every workload port.
		{Private: 8188, Public: nil, Type: "http"},
	}}}

	es, reported, err := mapExposure(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reported {
		t.Fatal("a pod with a runtime did not report")
	}
	if len(es) != 1 || es[0].Port != 22 {
		t.Fatalf("got %v", es)
	}
}

func TestMapExposure_RefusesAnUnknownProtocol(t *testing.T) {
	forty := 40022
	p := pod{ID: "x", Runtime: &podRuntime{Ports: []runtimePort{
		{Private: 22, Public: &forty, Type: "sctp"},
	}}}
	if _, _, err := mapExposure(p); err == nil {
		t.Fatal("a protocol ember cannot parse was accepted on a public port")
	}
}

func TestMapExposure_APortWithNoIPStillReports(t *testing.T) {
	forty := 40022
	p := pod{ID: "x", Runtime: &podRuntime{Ports: []runtimePort{
		{Private: 22, Public: &forty, Type: "tcp", IP: nil},
	}}}
	es, _, err := mapExposure(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].URL != "" {
		t.Fatalf("got %v", es)
	}
}

func TestMapExposure_AnEmptyRuntimePortListIsAReportOfNone(t *testing.T) {
	es, reported, err := mapExposure(pod{ID: "x", Runtime: &podRuntime{}})
	if err != nil || !reported || len(es) != 0 {
		t.Fatalf("got %v, %v, %v", es, reported, err)
	}
}

// TestMapOffers_SecureCloud runs over a *captured* catalogue response, not an
// invented one. The numbers below are what RunPod actually returned on
// 2026-10-29; an earlier version of this test asserted prices I had made up
// from the schema's examples, and every one of them was wrong.
func TestMapOffers_SecureCloud(t *testing.T) {
	gs := fixture[listGpuTypesResponse](t, "gpus.json").Gpus
	if len(gs) != 4 {
		t.Fatalf("the fixture decoded %d gpus; the response field is probably not \"gpus\"", len(gs))
	}

	offers := mapOffers(gs, cloudSecure)

	// Of the four captured entries, two have capacity somewhere. The other
	// two report NONE overall and list no data centre at all.
	byToken := map[node.OfferToken]node.Offer{}
	for _, o := range offers {
		byToken[o.Token] = o
	}
	if len(offers) != 2 {
		t.Fatalf("got %d offers: %+v", len(offers), offers)
	}

	o := byToken["NVIDIA GeForce RTX 4090"]
	if o.Price != node.Rate(890_000) {
		t.Errorf("4090 secure price = %v (%d micro), want $0.89/hr", o.Price, int64(o.Price))
	}
	// RunPod quotes VRAM in GB and ember's Offer carries MiB. A straight
	// assignment would report a 24 GB card as 24 MiB, and every --min-vram
	// filter would then reject all real hardware with nothing erroring.
	if o.VRAM != node.MiB(24*1024) {
		t.Errorf("4090 VRAM = %v, want %v", o.VRAM, node.MiB(24*1024))
	}
	// The 4090 reads NONE overall and LOW in US-IL-1, so the region names
	// the site that actually has it.
	if o.Region != "US-IL-1" {
		t.Errorf("region = %q", o.Region)
	}
	if err := o.Validate(); err != nil {
		t.Errorf("the mapped offer does not validate: %v", err)
	}

	a := byToken["NVIDIA A100 80GB PCIe"]
	if a.Price != node.Rate(1_790_000) {
		t.Errorf("A100 secure price = %v, want $1.79/hr", a.Price)
	}
	if a.VRAM != node.MiB(80*1024) {
		t.Errorf("A100 VRAM = %v", a.VRAM)
	}
}

// TestMapOffers_TheTwoCloudsAreQuotedSeparately, and a GPU a tier does not
// offer is not an offer there: listing it would have ember up pick hardware
// that cannot be rented and fail at create.
func TestMapOffers_TheTwoCloudsAreQuotedSeparately(t *testing.T) {
	gs := fixture[listGpuTypesResponse](t, "gpus.json").Gpus
	community := map[node.OfferToken]node.Offer{}
	for _, o := range mapOffers(gs, cloudCommunity) {
		community[o.Token] = o
	}

	if got := community["NVIDIA GeForce RTX 4090"].Price; got != node.Rate(340_000) {
		t.Errorf("4090 community price = %v, want $0.34/hr", got)
	}
	if got := community["NVIDIA A100 80GB PCIe"].Price; got != node.Rate(1_190_000) {
		t.Errorf("A100 community price = %v, want $1.19/hr", got)
	}

	// Secure-only hardware must not appear in the community listing even
	// though the response carries a community price for it.
	if _, found := community["AMD Instinct MI300X OAM"]; found {
		t.Error("a secure-only GPU was offered in the community cloud")
	}
}

// TestMapOffers_NoCapacityAnywhereIsNotAnOffer. Both of these report NONE
// overall and list no data centre, which is the real shape of "none".
func TestMapOffers_NoCapacityAnywhereIsNotAnOffer(t *testing.T) {
	gs := fixture[listGpuTypesResponse](t, "gpus.json").Gpus
	for _, cloud := range []string{cloudSecure, cloudCommunity} {
		for _, o := range mapOffers(gs, cloud) {
			if o.Token == "AMD Instinct MI300X OAM" || o.Token == "NVIDIA A100-SXM4-40GB" {
				t.Errorf("%s: %q has no capacity anywhere and was offered", cloud, o.Token)
			}
		}
	}
}

func TestMapOffers_CloudNameIsCaseInsensitive(t *testing.T) {
	gs := fixture[listGpuTypesResponse](t, "gpus.json").Gpus
	if len(mapOffers(gs, "community")) != len(mapOffers(gs, "COMMUNITY")) {
		t.Error("the cloud tier is matched case-sensitively")
	}
}

// TestMapOffers_AZeroPriceIsNotFree. A zero is RunPod declining to quote, and
// a budget check against zero would wave through whatever it really costs.
func TestMapOffers_AZeroPriceIsNotFree(t *testing.T) {
	offers := mapOffers([]gpuType{{
		ID: "NVIDIA Mystery", Memory: 24, Secure: true,
		Price: gpuPrice{Secure: 0}, MaxCount: gpuCount{Secure: 1}, Availability: "HIGH",
	}}, cloudSecure)
	if len(offers) != 0 {
		t.Fatalf("a GPU with no quoted price became an offer: %+v", offers)
	}
}

func TestMapOffers_AnEmptyCatalogueIsNotAnError(t *testing.T) {
	if got := mapOffers(nil, cloudSecure); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestMapOffers_AMissingMaxCountStillYieldsOneGPU(t *testing.T) {
	offers := mapOffers([]gpuType{{
		ID: "NVIDIA Odd", Memory: 16, Secure: true,
		Price: gpuPrice{Secure: 1.5}, MaxCount: gpuCount{Secure: 0}, Availability: "LOW",
	}}, cloudSecure)
	if len(offers) != 1 {
		t.Fatalf("got %+v", offers)
	}
	if !strings.Contains(offers[0].Label, "up to 1 per pod") {
		t.Errorf("label = %q", offers[0].Label)
	}
}

// TestMapPorts_PublishesOnly22TCP is the second lock on the door that
// exposure.PortRequest already bolts shut, placed at the exact point where a
// wrong value becomes an unauthenticated public URL.
func TestMapPorts_PublishesOnly22TCP(t *testing.T) {
	reqs, err := exposure.Requests([]node.PortMap{
		{Name: "comfyui", Remote: 8188, Local: 8188},
		{Name: "metrics", Remote: 9090, Local: 19090},
	})
	if err != nil {
		t.Fatal(err)
	}

	ports, err := mapPorts(reqs)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 1 || ports[0] != "22/tcp" {
		t.Fatalf("got %v, want exactly [22/tcp]", ports)
	}
	// The workload ports are not declared at all. RunPod publishes only what
	// is listed, so saying nothing is how they stay private; they are reached
	// by forwarding over the SSH connection.
	for _, p := range ports {
		if strings.Contains(p, "8188") || strings.Contains(p, "9090") {
			t.Errorf("a workload port was declared to RunPod: %q", p)
		}
	}
}

func TestMapPorts_RefusesAnEmptyRequestSet(t *testing.T) {
	// Without 22/tcp RunPod leaves ssh.direct null, and ember would have no
	// way to reach the pod at all.
	if _, err := mapPorts(nil); err == nil {
		t.Fatal("a pod with no public SSH port was accepted")
	}
}

func TestMapPorts_RefusesAZeroRequest(t *testing.T) {
	if _, err := mapPorts([]exposure.PortRequest{{}}); err == nil {
		t.Fatal("a zero port request was accepted")
	}
}

func TestMapProto(t *testing.T) {
	// http is kept distinct from tcp rather than folded into it: collapsing
	// the two would leave the exposure assertion unable to tell a forwarded
	// SSH port from a public web shell.
	got, err := mapProto("http")
	if err != nil || got != node.ProtoHTTP {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := mapProto("nonsense"); err == nil {
		t.Fatal("an unknown protocol was accepted")
	}
}

// TestAvailability_ASummaryIsNotAVerdict is the case the live catalogue
// taught. RunPod reported an RTX 4090 as NONE overall while its US-IL-1 entry
// read LOW, so filtering on the top-level figure alone would have had ember
// answer "no capacity" for a card that was rentable.
func TestAvailability_ASummaryIsNotAVerdict(t *testing.T) {
	g := gpuType{Availability: "NONE", DataCenters: []dataCentre{{ID: "US-IL-1", Availability: "LOW"}}}
	if !hasCapacity(g) {
		t.Error("a GPU with capacity in one data centre was dropped")
	}
	if got := availabilityOf(g); got != "LOW" {
		t.Errorf("availability = %q, want the best any site reports", got)
	}
	if got := bestRegion(g); got != "US-IL-1" {
		t.Errorf("region = %q", got)
	}
}

func TestAvailability_NoneAnywhereIsNoCapacity(t *testing.T) {
	for _, g := range []gpuType{
		{Availability: "NONE"},
		{Availability: "NONE", DataCenters: []dataCentre{{ID: "X", Availability: "NONE"}}},
		{Availability: ""},
	} {
		if hasCapacity(g) {
			t.Errorf("%+v was treated as having capacity", g)
		}
	}
}

// TestAvailability_RegionNamesASiteThatHasSome, so `ember offers` does not
// print a data centre with none as though it were where the node would land.
func TestAvailability_RegionNamesASiteThatHasSome(t *testing.T) {
	g := gpuType{Availability: "LOW", DataCenters: []dataCentre{
		{ID: "EMPTY", Availability: "NONE"},
		{ID: "BUSY", Availability: "HIGH"},
	}}
	if got := bestRegion(g); got != "BUSY" {
		t.Errorf("region = %q, want the site with capacity", got)
	}
	if got := availabilityOf(g); got != "HIGH" {
		t.Errorf("availability = %q", got)
	}
	if got := bestRegion(gpuType{}); got != "" {
		t.Errorf("a GPU with no data centres reported region %q", got)
	}
}

// TestRank_AnUnknownLevelIsNeitherHiddenNorTrusted. ember should not drop a
// GPU because the API grew a word it does not know, nor claim it is plentiful.
func TestRank(t *testing.T) {
	if rank("HIGH") <= rank("MEDIUM") || rank("MEDIUM") <= rank("LOW") || rank("LOW") <= rank("NONE") {
		t.Error("the levels do not order")
	}
	if rank("NONE") != 0 || rank("") != 0 {
		t.Error("NONE and empty must be the floor")
	}
	if rank("PLENTIFUL") <= rank("NONE") || rank("PLENTIFUL") >= rank("MEDIUM") {
		t.Errorf("an unrecognised level ranks %d, want above NONE and below MEDIUM", rank("PLENTIFUL"))
	}
	if got := availabilityOf(gpuType{}); got != "unknown" {
		t.Errorf("got %q", got)
	}
}

// TestMapPod_APublicPortWithAnUnparseableProtocolIsAnError. The pod may be
// perfectly fine, but ember cannot tell whether that public port is a
// forwarded socket or a web shell, and the only safe answer to that is to
// refuse the node rather than hand it to a workload.
func TestMapPod_APublicPortWithAnUnparseableProtocolIsAnError(t *testing.T) {
	p := fixture[pod](t, "pod_running.json")
	p.Runtime.Ports[0].Type = "quic"

	_, err := mapPod(p)
	if err == nil {
		t.Fatal("a public port with an unknown protocol was mapped")
	}
	if !strings.Contains(err.Error(), "quic") {
		t.Errorf("the error does not name the protocol: %v", err)
	}
}

// TestMapPod_APodWithNoGPUBlockStillMaps. A CPU pod is not what ember rents,
// but a response missing the block must not panic the mapper -- the whole
// point of these being pure total functions is that no provider response can.
func TestMapPod_APodWithNoGPUBlockStillMaps(t *testing.T) {
	p := fixture[pod](t, "pod_running.json")
	p.Gpu = nil

	n, err := mapPod(p)
	if err != nil {
		t.Fatal(err)
	}
	if n.Offer.GPUCount != 1 {
		t.Errorf("gpu count = %d, want 1", n.Offer.GPUCount)
	}
}

// TestPort16_RefusesSomethingOutOfRange. A silent narrowing is worse than a
// refusal: uint16(70000) is 4464, so on the endpoint path ember would dial a
// port the node is not listening on and blame the node for the timeout, and
// on the exposure path a public port would be *reported as a different port* --
// making the assertion about what is reachable an assertion about the wrong
// number.
func TestPort16_RefusesSomethingOutOfRange(t *testing.T) {
	for _, n := range []int{0, -1, 65536, 70000, 1 << 20} {
		if got, err := port16(n); err == nil {
			t.Errorf("port16(%d) = %d, want an error", n, got)
		}
	}
	for _, n := range []int{1, 22, 8188, 65535} {
		got, err := port16(n)
		if err != nil {
			t.Errorf("port16(%d): %v", n, err)
		}
		if int(got) != n {
			t.Errorf("port16(%d) = %d", n, got)
		}
	}
}

func TestMapPod_RefusesAnOutOfRangeSSHPort(t *testing.T) {
	p := fixture[pod](t, "pod_running.json")
	p.Ssh.Direct.Port = 70000

	if _, err := mapPod(p); err == nil {
		t.Fatal("an out-of-range ssh.direct port was narrowed silently")
	}
}

func TestMapExposure_RefusesAnOutOfRangePublicPort(t *testing.T) {
	p := fixture[pod](t, "pod_running.json")
	p.Runtime.Ports[0].Private = 99999

	if _, _, err := mapExposure(p); err == nil {
		t.Fatal("an out-of-range public port was narrowed silently, so the exposure report would name a different port")
	}
}

func TestVolumePrice_ReadsTheVolumeRatherThanAssuming(t *testing.T) {
	// The rate comes off the volume's own reported type and size, so a volume
	// created elsewhere, or one whose tier changed, is priced as it is.
	for _, tc := range []struct {
		name string
		vol  networkVolume
		want node.StorePrice
	}{
		{"standard below a terabyte", networkVolume{Size: 40, Type: "STANDARD"}, standardRate},
		{"standard at the bulk boundary", networkVolume{Size: bulkFromGiB, Type: "STANDARD"}, standardBulkRate},
		{"standard above it", networkVolume{Size: 4000, Type: "STANDARD"}, standardBulkRate},
		{"high performance is dearer at any size", networkVolume{Size: 10, Type: "HIGH_PERFORMANCE"}, highPerformanceRate},
		{"case is not a tier", networkVolume{Size: 10, Type: "high_performance"}, highPerformanceRate},
		{
			// A tier RunPod has not told us about is priced as standard
			// rather than refused: the figure is informational and a listing
			// that errored would hide every other store beside it.
			name: "an unknown tier falls back", vol: networkVolume{Size: 40, Type: "PLATINUM"}, want: standardRate,
		},
		{"no tier reported", networkVolume{Size: 40}, standardRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumePrice(tc.vol); got != tc.want {
				t.Errorf("volumePrice = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMapVolume_RefusesWhatItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vol     networkVolume
		wantMsg string
	}{
		{"no id", networkVolume{Name: "ember-store-models", Size: 10}, "no id"},
		{"negative size", networkVolume{ID: "v1", Name: "ember-store-models", Size: -1}, "negative size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ours, err := mapVolume(tc.vol)
			if err == nil {
				t.Fatal("accepted")
			}
			if ours {
				t.Error("a volume that could not be mapped was still claimed as ember's")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}

	// Somebody else's volume is neither ours nor an error. `ember store rm`
	// acts on what this returns.
	s, ours, err := mapVolume(networkVolume{ID: "v1", Name: "my-training-data", Size: 10})
	if err != nil || ours {
		t.Errorf("mapVolume on a foreign volume = (%+v, %v, %v), want (zero, false, nil)", s, ours, err)
	}
}

func TestRegionOffers_ReadsTheSitesOwnAvailability(t *testing.T) {
	// The bug this exists to prevent: mapOffers reports bestRegion(g), the
	// single site with the most capacity. An RTX 4090 reads NONE overall and
	// LOW in US-IL-1, so filtering mapOffers' output by region would have
	// dropped a card that is rentable exactly where the store lives.
	gpus := fixture[listGpuTypesResponse](t, "gpus.json").Gpus

	got := regionOffers(gpus, cloudSecure, "US-IL-1")
	if len(got) != 1 {
		t.Fatalf("got %d offers in US-IL-1, want the 4090: %+v", len(got), got)
	}
	if got[0].Region != "US-IL-1" {
		t.Errorf("region = %q, want the store's site", got[0].Region)
	}
	if got[0].GPU != node.NormalizeGPU("rtx4090") {
		t.Errorf("gpu = %q", got[0].GPU)
	}

	// A site with capacity for a different card returns that card and not
	// the 4090.
	mtl := regionOffers(gpus, cloudSecure, "CA-MTL-3")
	if len(mtl) != 1 || !strings.Contains(string(mtl[0].GPU), "a100") {
		t.Errorf("CA-MTL-3 gave %+v, want the A100", mtl)
	}

	// A site nothing is available in is empty, which the caller turns into a
	// refusal naming the datacentre.
	if nowhere := regionOffers(gpus, cloudSecure, "XX-NOWHERE-9"); len(nowhere) != 0 {
		t.Errorf("an unknown site gave %+v", nowhere)
	}

	// No region asked for is the unfiltered catalogue.
	if all := regionOffers(gpus, cloudSecure, ""); len(all) != len(mapOffers(gpus, cloudSecure)) {
		t.Errorf("an empty region changed the catalogue: %d vs %d", len(all), len(mapOffers(gpus, cloudSecure)))
	}
}

func TestRegionOffers_SkipsASiteWithNoCapacity(t *testing.T) {
	// A datacentre that lists the card at NONE is not a placement, even
	// though the card appears in its list.
	gpus := []gpuType{{
		ID: "NVIDIA GeForce RTX 4090", Name: "RTX 4090", Memory: 24,
		Secure: true, Price: gpuPrice{Secure: 0.89}, MaxCount: gpuCount{Secure: 1},
		Availability: "HIGH",
		DataCenters:  []dataCentre{{ID: "EU-RO-1", Availability: "NONE"}},
	}}
	if got := regionOffers(gpus, cloudSecure, "EU-RO-1"); len(got) != 0 {
		t.Errorf("a site reporting NONE produced %+v", got)
	}
}
