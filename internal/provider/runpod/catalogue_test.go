package runpod

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
)

// observed is a fixed instant, so what lands in a sample is what the caller
// passed and never what a clock in here decided.
var observed = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func samplesBySite(in []catalogue.Sample) map[string]catalogue.Sample {
	out := map[string]catalogue.Sample{}
	for _, s := range in {
		out[s.ID+"@"+s.Datacentre] = s
	}
	return out
}

// TestGPUSamples_OneRowPerSiteOverTheCapturedCatalogue runs over the captured
// 2026-10-29 response, which is where the whole feature's justification came
// from: the RTX 4090 reads NONE overall and LOW in US-IL-1.
//
// mapOffers collapses that into one offer at bestRegion; this must not. The
// per-site rows are the measurement, and a collector that stored the summary
// would have recorded "unavailable" for a card that was rentable all along.
func TestGPUSamples_OneRowPerSiteOverTheCapturedCatalogue(t *testing.T) {
	gs := fixture[listGpuTypesResponse](t, "gpus.json").Gpus
	if len(gs) != 4 {
		t.Fatalf("the fixture decoded %d gpus", len(gs))
	}

	got := gpuSamples(gs, observed)

	// Four types: two with sites (one site and two sites), two with none.
	if len(got) != 5 {
		t.Fatalf("got %d row(s), want 5: 1 + 2 sites and 2 types with no site named\n%+v", len(got), got)
	}
	by := samplesBySite(got)

	il, ok := by["NVIDIA GeForce RTX 4090@US-IL-1"]
	if !ok {
		t.Fatalf("no row for the 4090 in US-IL-1: %+v", got)
	}
	if il.Availability != catalogue.LevelLow {
		t.Errorf("the site's level = %q, want LOW -- the site figure and not the summary", il.Availability)
	}
	if il.Overall != catalogue.LevelNone {
		t.Errorf("the summary = %q, want NONE kept beside the site's own level", il.Overall)
	}
	if !il.ObservedAt.Equal(observed) {
		t.Errorf("observed_at = %s, want the instant the caller passed", il.ObservedAt)
	}
	if il.Provider != Name || il.Kind != catalogue.KindGPU {
		t.Errorf("row = %+v", il)
	}
	if il.MemoryGiB != 24 {
		t.Errorf("memory = %d GiB", il.MemoryGiB)
	}
	// Both tiers, each with its own quote and ceiling, in exact
	// microdollars: int64(0.34*1e6) is 339999 on this hardware, which is why
	// no float reaches the file.
	if !il.Secure.Offered || il.Secure.Price != node.Rate(890_000) || il.Secure.MaxCount != 8 {
		t.Errorf("secure tier = %+v, want offered at exactly 890000 micro and 8 per pod", il.Secure)
	}
	if !il.Community.Offered || il.Community.Price != node.Rate(340_000) {
		t.Errorf("community tier = %+v, want offered at exactly 340000 micro", il.Community)
	}

	// The A100 PCIe is in two sites, and both are kept.
	for _, site := range []string{"CA-MTL-3", "US-MO-1"} {
		s, ok := by["NVIDIA A100 80GB PCIe@"+site]
		if !ok {
			t.Fatalf("no row for the A100 PCIe in %s", site)
		}
		if s.Availability != catalogue.LevelLow {
			t.Errorf("%s level = %q", site, s.Availability)
		}
	}
}

// TestGPUSamples_ATypeWithNoSiteIsKeptAndNotTurnedIntoOne. Three of the four
// captured entries that read NONE overall list no dataCenters array at all.
// Dropping them would make a card that is sold out everywhere -- which is one
// of the questions the series exists to answer -- indistinguishable from a
// card that was never offered.
//
// And its site level stays unreported rather than being filled in from the
// summary, because a summary written into the site column is exactly the row
// this function refuses to produce.
func TestGPUSamples_ATypeWithNoSiteIsKeptAndNotTurnedIntoOne(t *testing.T) {
	gs := fixture[listGpuTypesResponse](t, "gpus.json").Gpus
	by := samplesBySite(gpuSamples(gs, observed))

	mi, ok := by["AMD Instinct MI300X OAM@"]
	if !ok {
		t.Fatalf("the MI300X, which lists no datacentre, was dropped: %v", keys(by))
	}
	if mi.Datacentre != "" {
		t.Errorf("datacentre = %q, want empty", mi.Datacentre)
	}
	if mi.Availability != catalogue.LevelUnreported {
		t.Errorf("availability = %q, want unreported: no site said anything", mi.Availability)
	}
	if mi.Overall != catalogue.LevelNone {
		t.Errorf("overall = %q, want the summary RunPod did give", mi.Overall)
	}
	// catalogue.Sample.Level is what reads the summary back for such a row,
	// so the report has something to show.
	if mi.Level() != catalogue.LevelNone {
		t.Errorf("Level() = %q", mi.Level())
	}
}

// TestGPUSamples_NothingIsFiltered. mapOffers drops a tier that does not sell
// the card, a zero price and a type with no capacity anywhere, because an
// offer ember cannot rent is noise. Here each of those is the observation.
func TestGPUSamples_NothingIsFiltered(t *testing.T) {
	gs := fixture[listGpuTypesResponse](t, "gpus.json").Gpus
	by := samplesBySite(gpuSamples(gs, observed))

	// Secure: false, secure price 0, community: true at $1.00. mapOffers
	// drops it from a secure listing entirely; the series keeps it.
	sxm, ok := by["NVIDIA A100-SXM4-40GB@"]
	if !ok {
		t.Fatalf("the card no cloud tier sells securely was dropped: %v", keys(by))
	}
	if sxm.Secure.Offered {
		t.Error("secure reports as offered for a card with secure:false")
	}
	if sxm.Secure.Price != 0 {
		t.Errorf("secure price = %v, want nothing: RunPod quoted 0, which is declining to quote", sxm.Secure.Price)
	}
	if !sxm.Community.Offered || sxm.Community.Price != node.Rate(1_000_000) {
		t.Errorf("community tier = %+v, want offered at $1.00", sxm.Community)
	}
}

// TestGPUSamples_APriceIsRoundedAndNotTruncated.
//
// $2.01 rather than one of the fixture's own prices, and the number was
// searched for rather than guessed: of the 2000 two-decimal prices up to $20,
// 38 come out a microdollar low through int64(float * 1e6) on this toolchain
// and $2.01 is the first of them -- 2009999. Every price in the captured
// catalogue happens to survive truncation, so a test over the fixture alone
// cannot tell node.RateFromUSD from a multiplication. That is how a money bug
// ships with a green suite.
//
// AGENTS.md names int64(0.44*1e6) as 439999 and ParseFloat("0.29")*1e6 as
// 289999. Neither reproduces on go1.27/amd64 here -- both come out exact --
// so the class of defect is real and the examples have drifted, which is
// exactly why this asserts against a value checked today.
func TestGPUSamples_APriceIsRoundedAndNotTruncated(t *testing.T) {
	got := gpuSamples([]gpuType{{
		ID: "SYNTHETIC", Secure: true, Availability: "HIGH",
		Price: gpuPrice{Secure: 2.01}, MaxCount: gpuCount{Secure: 1},
	}}, observed)
	if len(got) != 1 {
		t.Fatalf("got %d row(s)", len(got))
	}
	if got[0].Secure.Price != node.Rate(2_010_000) {
		t.Errorf("price = %d micro, want exactly 2010000: $2.01 truncates to 2009999",
			int64(got[0].Secure.Price))
	}
}

func TestGPUSamples_AnEmptyCatalogueIsNoRows(t *testing.T) {
	if got := gpuSamples(nil, observed); len(got) != 0 {
		t.Errorf("got %d row(s) from no types", len(got))
	}
}

// TestCPUSamples_PerSiteAndPricedPerVCPU. The price here is per vCPU per hour,
// which is what the endpoint quotes; mapCPUOffers multiplies it by a core
// count because that is what a budget compares against, and a core count is a
// property of a run rather than of the catalogue.
func TestCPUSamples_PerSiteAndPricedPerVCPU(t *testing.T) {
	cs := fixture[listCpuTypesResponse](t, "cpus.json").Cpus
	if len(cs) != 2 {
		t.Fatalf("the fixture decoded %d cpu families", len(cs))
	}

	got := cpuSamples(cs, observed)
	if len(got) != 3 {
		t.Fatalf("got %d row(s), want 3: two sites plus one\n%+v", len(got), got)
	}
	by := samplesBySite(got)

	ks, ok := by["cpu3c@US-KS-2"]
	if !ok {
		t.Fatalf("no row for cpu3c in US-KS-2: %v", keys(by))
	}
	if ks.Kind != catalogue.KindCPU {
		t.Errorf("kind = %q", ks.Kind)
	}
	// HIGH overall while this site reads LOW: the same disagreement the GPU
	// catalogue has, which is why the CPU rows are collected the same way.
	if ks.Availability != catalogue.LevelLow || ks.Overall != catalogue.LevelHigh {
		t.Errorf("cpu3c in US-KS-2 = %q with summary %q, want LOW with HIGH",
			ks.Availability, ks.Overall)
	}
	if ks.Secure.Price != node.Rate(30_000) {
		t.Errorf("price = %v, want exactly 30000 micro -- $0.03 per vCPU per hour", ks.Secure.Price)
	}
	// Exact, and that is all this can prove on this path: no per-vCPU figure
	// RunPod plausibly quotes -- three decimals, under $0.20 -- comes out
	// differently under truncation than under rounding, so the decisive case
	// for the conversion lives on the GPU side where the numbers are large
	// enough. See TestGPUSamples_APriceIsRoundedAndNotTruncated.
	if mo := samplesBySite(got)["cpu3m@EU-RO-1"]; mo.Secure.Price != node.Rate(55_000) {
		t.Errorf("cpu3m price = %v, want exactly 55000 micro", mo.Secure.Price)
	}
	// There is no community price for a CPU family: the endpoint quotes
	// securePerVcpu and serverlessPerVcpu, and serverless is not a pod. So
	// the tier is not offered rather than offered at zero.
	if ks.Community.Offered || ks.Community.Price != 0 {
		t.Errorf("community tier = %+v, want nothing", ks.Community)
	}
	if ks.MemoryGiB != 0 {
		t.Errorf("memory = %d; RAM on a CPU family is a multiple of the core count a run picks", ks.MemoryGiB)
	}
}

func TestCPUSamples_AFamilyWithNoSiteIsKept(t *testing.T) {
	got := cpuSamples([]cpuType{{ID: "cpu5c", Availability: "HIGH"}}, observed)
	if len(got) != 1 {
		t.Fatalf("got %d row(s)", len(got))
	}
	if got[0].Datacentre != "" || got[0].Overall != catalogue.LevelHigh {
		t.Errorf("row = %+v", got[0])
	}
}

func keys(m map[string]catalogue.Sample) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- the provider method ---------------------------------------------------

func TestCatalogueSnapshot_ReadsBothCatalogues(t *testing.T) {
	var paths []string
	p := newTestProvider(t, func(w http.ResponseWriter, req *http.Request) {
		paths = append(paths, req.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.URL.Path, "cpus") {
			serveFile(t, "cpus.json")(w, req)
			return
		}
		serveFile(t, "gpus.json")(w, req)
	})

	got, err := p.CatalogueSnapshot(context.Background(), observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("got %d row(s), want 5 gpu and 3 cpu", len(got))
	}
	// include=AVAILABILITY is what adds the levels at all and product=POD is
	// not optional with it -- the endpoint refuses the pair without it. A
	// snapshot that quietly dropped either would record a catalogue with no
	// availability in it, which is a day of rows that answer nothing.
	for _, want := range []string{
		"/v2/catalog/gpus?include=AVAILABILITY&product=POD",
		"/v2/catalog/cpus?include=AVAILABILITY&product=POD",
	} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no request to %q; got %v", want, paths)
		}
	}
}

// TestCatalogueSnapshot_ACPUFailureStillReturnsTheGPURows. An hour of GPU
// observations is the thing that cannot be recovered, so it comes back beside
// the error rather than being discarded with it -- the shape List already
// uses for a pod status it did not recognise.
func TestCatalogueSnapshot_ACPUFailureStillReturnsTheGPURows(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "cpus") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		serveFile(t, "gpus.json")(w, req)
	})

	got, err := p.CatalogueSnapshot(context.Background(), observed)
	if err == nil {
		t.Fatal("a failing cpu catalogue was not reported")
	}
	if len(got) != 5 {
		t.Errorf("got %d row(s) beside the error, want the 5 gpu rows that did arrive", len(got))
	}
	if !strings.Contains(err.Error(), "cpu types") {
		t.Errorf("the error should name what failed, got %v", err)
	}
}

// TestCatalogueSnapshot_AGPUFailureIsFatal: that is the series the store
// decision is made from, and half a snapshot of it is worse than none.
func TestCatalogueSnapshot_AGPUFailureIsFatal(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	got, err := p.CatalogueSnapshot(context.Background(), observed)
	if err == nil {
		t.Fatal("a failing gpu catalogue was not reported")
	}
	if got != nil {
		t.Errorf("got %d row(s), want none", len(got))
	}
}

// TestCatalogueSnapshot_RefusesAnObservationWithNoTime. observed_at is the
// only field RunPod does not supply; a zero time silently replaced by this
// function's own clock would be a row dated by whichever layer looked at a
// clock last.
func TestCatalogueSnapshot_RefusesAnObservationWithNoTime(t *testing.T) {
	asked := false
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		asked = true
		w.WriteHeader(http.StatusOK)
	})
	if _, err := p.CatalogueSnapshot(context.Background(), time.Time{}); err == nil {
		t.Fatal("a snapshot with no observation time was accepted")
	}
	if asked {
		t.Error("it called the API before noticing it could not date the result")
	}
}

func TestCaps_CatalogueMatchesTheInterface(t *testing.T) {
	providertest.AssertCatalogueCapsMatch(t, New(node.NewSecret([]byte("k"))))
}

var _ provider.CatalogueAPI = (*Provider)(nil)
