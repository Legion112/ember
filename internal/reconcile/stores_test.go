package reconcile

import (
	"errors"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
)

// rpRate is RunPod's measured rate below a terabyte: $0.07 per GiB per month.
const rpRate = node.StorePrice(70_000)

func store(prov node.ProviderID, id, name string, gib int, region string) node.Store {
	return node.Store{
		Handle:  node.StoreHandle{Provider: prov, ID: node.StoreID(id)},
		Name:    name,
		SizeGiB: gib,
		Region:  region,
		Price:   rpRate,
	}
}

func TestScanStores_ReportsWhatIsBeingCharged(t *testing.T) {
	fs := ScanStores(
		map[node.ProviderID][]node.Store{
			"runpod": {store("runpod", "v1", "models", 40, "EU-RO-1")},
		},
		nil,
		map[node.ProviderID]string{"runpod": "models"},
	)

	if len(fs) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(fs), fs)
	}
	f := fs[0]
	if f.Kind != KindStore {
		t.Errorf("kind = %s, want store", f.Kind)
	}
	if f.Store == nil || f.Store.Name != "models" {
		t.Errorf("finding does not carry its store: %+v", f)
	}
	// 40 GiB at $0.07 is $2.80 a month, every month, until somebody removes it.
	if want := node.MicroUSD(2_800_000); f.Monthly != want {
		t.Errorf("monthly = %d, want %d", f.Monthly, want)
	}
	if !strings.Contains(f.Reason, "named in this configuration") {
		t.Errorf("reason = %q, want it to say the config names this store", f.Reason)
	}
	if !strings.Contains(f.String(), "/month") {
		t.Errorf("String() = %q, want it to mark the figure as recurring", f.String())
	}
}

func TestScanStores_AStoreIsNotAFinding(t *testing.T) {
	// The decision this phase turns on. A store existing is the expected
	// state, and making `ember ls` exit 2 for one would teach the operator to
	// ignore exit 2 -- the same reasoning that leaves an untracked node
	// reported but unreaped.
	fs := ScanStores(
		map[node.ProviderID][]node.Store{"runpod": {store("runpod", "v1", "models", 40, "EU-RO-1")}},
		nil, nil,
	)
	if n := Attention(fs); n != 0 {
		t.Errorf("a store needed attention (%d findings)", n)
	}
	if KindStore.NeedsAttention() {
		t.Error("KindStore needs attention")
	}
}

func TestScanStores_SaysWhenNothingHereNamesIt(t *testing.T) {
	// Reported, not condemned. ember cannot tell a store that is finished
	// with from one kept between runs, so the line says what it knows and
	// names the command rather than calling it a leak.
	fs := ScanStores(
		map[node.ProviderID][]node.Store{"runpod": {store("runpod", "v1", "old-wan", 100, "US-KS-2")}},
		nil,
		map[node.ProviderID]string{"runpod": "models"},
	)
	if len(fs) != 1 {
		t.Fatalf("got %d findings", len(fs))
	}
	r := fs[0].Reason
	if !strings.Contains(r, "nothing in this configuration names it") {
		t.Errorf("reason = %q", r)
	}
	if !strings.Contains(r, "ember store rm old-wan") {
		t.Errorf("reason = %q, want it to name the command that removes it", r)
	}
	if Attention(fs) != 0 {
		t.Error("an unnamed store was treated as needing attention")
	}
}

func TestScanStores_AProviderThatCouldNotBeAskedIsAFinding(t *testing.T) {
	// "I could not ask RunPod what volumes it is billing me for" must not
	// render as "nothing is being billed".
	fs := ScanStores(
		nil,
		map[node.ProviderID]error{"runpod": errors.New("401 unauthorized")},
		nil,
	)
	if len(fs) != 1 || fs[0].Kind != KindUnreachable {
		t.Fatalf("got %+v, want one unreachable finding", fs)
	}
	if !strings.Contains(fs[0].Reason, "stores") {
		t.Errorf("reason = %q, want it to say this was the store listing; a provider "+
			"can fail one listing and answer the other", fs[0].Reason)
	}
	if Attention(fs) != 1 {
		t.Error("an unreachable store provider did not need attention")
	}
	// A nil entry in the error map is not a failure.
	if got := ScanStores(nil, map[node.ProviderID]error{"runpod": nil}, nil); len(got) != 0 {
		t.Errorf("a nil error produced %+v", got)
	}
}

func TestScanStores_UnreachableSortsAboveStores(t *testing.T) {
	fs := ScanStores(
		map[node.ProviderID][]node.Store{
			"runpod": {store("runpod", "v2", "beta", 10, ""), store("runpod", "v1", "alpha", 10, "")},
			"aaa":    {store("aaa", "v3", "zeta", 10, "")},
		},
		map[node.ProviderID]error{"zzz": errors.New("down")},
		nil,
	)
	if len(fs) != 4 {
		t.Fatalf("got %d findings", len(fs))
	}
	if fs[0].Kind != KindUnreachable {
		t.Errorf("first finding is %s, want the unreachable provider first", fs[0].Kind)
	}
	// Then by provider, then by name, so two runs print the same order.
	var got []string
	for _, f := range fs[1:] {
		got = append(got, string(f.Provider)+"/"+f.Store.Name)
	}
	want := []string{"aaa/zeta", "runpod/alpha", "runpod/beta"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order = %v, want %v", got, want)
			break
		}
	}
}

func TestScanStores_NoStoresIsNoFindings(t *testing.T) {
	if fs := ScanStores(nil, nil, nil); len(fs) != 0 {
		t.Errorf("got %+v, want nothing", fs)
	}
}

func TestScanStores_RegionIsNamedWhereThereIsOne(t *testing.T) {
	withRegion := ScanStores(
		map[node.ProviderID][]node.Store{"runpod": {store("runpod", "v1", "m", 10, "EU-RO-1")}}, nil, nil)
	if !strings.Contains(withRegion[0].Reason, "GiB in EU-RO-1") {
		t.Errorf("reason = %q, want the datacentre named: it pins every node that "+
			"mounts the store", withRegion[0].Reason)
	}
	without := ScanStores(
		map[node.ProviderID][]node.Store{"local": {store("local", "v1", "m", 10, "")}}, nil, nil)
	// Checked as the clause it actually is, not a bare " in ": the rest of
	// the sentence says "nothing in this configuration", and a looser match
	// would pass for the wrong reason.
	if strings.Contains(without[0].Reason, "GiB in ") {
		t.Errorf("reason = %q, want no region clause for a provider that does not pin", without[0].Reason)
	}
}

func TestMonthlyTotal(t *testing.T) {
	fs := ScanStores(
		map[node.ProviderID][]node.Store{
			"runpod": {store("runpod", "v1", "a", 40, ""), store("runpod", "v2", "b", 10, "")},
		}, nil, nil)
	if got, want := MonthlyTotal(fs), node.MicroUSD(3_500_000); got != want {
		t.Errorf("MonthlyTotal = %d, want %d", got, want)
	}

	// Node findings carry no monthly charge and must not be counted.
	mixed := append(fs, Finding{Kind: KindExpired, Monthly: 999})
	if got, want := MonthlyTotal(mixed), node.MicroUSD(3_500_000); got != want {
		t.Errorf("MonthlyTotal over mixed findings = %d, want %d", got, want)
	}

	if got := MonthlyTotal(nil); got != 0 {
		t.Errorf("MonthlyTotal(nil) = %d", got)
	}

	// Saturates rather than wrapping. A negative total would print as a
	// credit, which is the one direction a cost must never round.
	huge := []Finding{
		{Kind: KindStore, Monthly: node.MaxMicroUSD},
		{Kind: KindStore, Monthly: node.MaxMicroUSD},
	}
	if got := MonthlyTotal(huge); got != node.MaxMicroUSD {
		t.Errorf("MonthlyTotal saturated to %d, want %d", got, node.MaxMicroUSD)
	}
}

func TestCollectStores_SkipsProvidersWithoutStores(t *testing.T) {
	// A provider without stores is not an error. Treating it as one would
	// make `ember ls` exit non-zero on every run against the local provider.
	plain := providertest.NewRecordingProvider("plain")
	withStores := providertest.WithStores("stored", store("stored", "v1", "models", 40, "EU-RO-1"))

	reg := provider.NewRegistry()
	for _, p := range []provider.Provider{plain, withStores} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}

	got, errs := CollectStores(t.Context(), reg)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if _, ok := got["plain"]; ok {
		t.Error("a provider without stores appeared in the listing")
	}
	if len(got["stored"]) != 1 {
		t.Errorf("got %+v, want one store from the provider that has them", got)
	}
}

func TestCollectStores_KeepsPartialResultsAndFailuresSideBySide(t *testing.T) {
	good := providertest.WithStores("good", store("good", "v1", "models", 40, ""))
	bad := providertest.WithStores("bad")
	bad.FailOn = "StoreList"

	reg := provider.NewRegistry()
	for _, p := range []provider.Provider{good, bad} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}

	got, errs := CollectStores(t.Context(), reg)
	if len(got["good"]) != 1 {
		t.Errorf("one provider failing hid what the other said: %+v", got)
	}
	if errs["bad"] == nil {
		t.Error("a failed store listing was not recorded as an error")
	}
	if _, ok := got["bad"]; ok {
		t.Error("the failed provider also appeared in the listing")
	}
}

func TestCollectStores_IgnoresAProviderThatDoesNotClaimTheCapability(t *testing.T) {
	// Caps is the contract. A provider that implements StoreAPI without
	// saying so is not asked, which is what AssertStoreCapsMatch exists to
	// catch at the provider rather than here.
	silent := providertest.WithStores("silent", store("silent", "v1", "m", 10, ""))
	silent.Capabilities.Stores = false

	reg := provider.NewRegistry()
	if err := reg.Add(silent); err != nil {
		t.Fatal(err)
	}
	got, errs := CollectStores(t.Context(), reg)
	if len(got) != 0 || len(errs) != 0 {
		t.Errorf("got %+v / %+v, want a provider that does not claim stores to be skipped", got, errs)
	}
}

func TestReap_NeverTouchesAStore(t *testing.T) {
	// Destroy takes a node.Handle and a store finding carries a zero one, so
	// without the guard the call would be made against an empty handle and
	// the error blamed on the provider. These are the two resources with
	// different lifetimes, and nothing that destroys a node may be handed a
	// store.
	p := providertest.WithStores("runpod", store("runpod", "v1", "models", 40, ""))
	reg := provider.NewRegistry()
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}

	s := store("runpod", "v1", "models", 40, "")
	destroyed, err := Reap(t.Context(), reg, nil, []Finding{
		// Deliberately mislabelled as expired: the guard must hold even if
		// something upstream classifies a store wrongly.
		{Kind: KindExpired, Provider: "runpod", Store: &s},
	})
	if err != nil {
		t.Fatalf("Reap = %v", err)
	}
	if destroyed != 0 {
		t.Errorf("Reap destroyed %d things, want 0", destroyed)
	}
	for _, c := range p.Recorded() {
		if strings.HasPrefix(c, "Destroy") || strings.HasPrefix(c, "StoreDestroy") {
			t.Errorf("Reap called %q", c)
		}
	}
	if left, _ := p.StoreList(t.Context()); len(left) != 1 {
		t.Errorf("the store is gone: %+v", left)
	}
}

func TestFinding_StringNamesTheStore(t *testing.T) {
	s := store("runpod", "vol_abc", "models", 40, "EU-RO-1")
	f := Finding{Kind: KindStore, Provider: "runpod", Store: &s, Monthly: s.Monthly(), Reason: "because"}
	got := f.String()
	if !strings.Contains(got, "runpod/vol_abc") {
		t.Errorf("String() = %q, want the store handle", got)
	}
	if !strings.Contains(got, "/month") {
		t.Errorf("String() = %q, want the charge marked recurring", got)
	}

	// A free store prints no figure rather than "$0".
	free := store("local", "v1", "lab", 10, "")
	free.Price = 0
	ff := Finding{Kind: KindStore, Provider: "local", Store: &free, Monthly: free.Monthly()}
	if strings.Contains(ff.String(), "/month") {
		t.Errorf("String() = %q, want no charge for a free store", ff.String())
	}
}

func TestKindStore_HasAName(t *testing.T) {
	if got := KindStore.String(); got != "store" {
		t.Errorf("KindStore.String() = %q", got)
	}
}
