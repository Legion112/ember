package reconcile

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
	"github.com/Legion112/ember/internal/statestore"
)

var scanNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func leftover(name string, runID node.RunID, deadline time.Time) node.Leftover {
	return node.Leftover{
		Handle:   node.LeftoverHandle{Provider: "local", ID: node.LeftoverID(name)},
		Kind:     "network",
		Name:     "ember-" + name,
		RunID:    runID,
		Deadline: deadline,
	}
}

func TestScanLeftovers_ReportsWhatIsPastItsDeadline(t *testing.T) {
	got := ScanLeftovers(map[node.ProviderID][]node.Leftover{
		"local": {leftover("net1", "r1", scanNow.Add(-time.Hour))},
	}, nil, scanNow)

	if len(got) != 1 {
		t.Fatalf("ScanLeftovers = %+v, want one finding", got)
	}
	f := got[0]
	if f.Kind != KindLeftover {
		t.Errorf("kind = %s, want leftover", f.Kind)
	}
	if f.Leftover == nil || f.Leftover.Name != "ember-net1" {
		t.Fatalf("finding carries %+v, want the leftover itself so reap can remove it", f.Leftover)
	}
	// The name and not the handle's id: an operator matches this line against
	// `docker network ls`, which does not print daemon hashes.
	if s := f.String(); !strings.Contains(s, "local/ember-net1") {
		t.Errorf("String() = %q, want the network's own name", s)
	}
	if !strings.Contains(f.Reason, "ember reap") {
		t.Errorf("reason does not say what removes it: %q", f.Reason)
	}
	if !strings.Contains(f.Reason, "r1") {
		t.Errorf("reason does not name the run: %q", f.Reason)
	}
}

// TestScanLeftovers_SaysNothingAboutALiveRun is the create window.
//
// local.Create makes the network before the container, so for a moment a
// perfectly healthy run has a network with no node attached -- and reporting
// that would be a finding that is sometimes false, which teaches an operator
// to ignore exit 2. The deadline cannot be in the future for a run that has
// finished, so it is what tells the two apart.
func TestScanLeftovers_SaysNothingAboutALiveRun(t *testing.T) {
	got := ScanLeftovers(map[node.ProviderID][]node.Leftover{
		"local": {leftover("net1", "r1", scanNow.Add(time.Hour))},
	}, nil, scanNow)
	if len(got) != 0 {
		t.Fatalf("ScanLeftovers = %+v; a run inside its deadline was reported as wreckage", got)
	}
}

// TestScanLeftovers_ReportsANetworkWithNoReadableLabel: a zero deadline is
// treated as past, because the object wears ember's own label and nothing else
// will ever remove it.
func TestScanLeftovers_ReportsANetworkWithNoReadableLabel(t *testing.T) {
	got := ScanLeftovers(map[node.ProviderID][]node.Leftover{
		"local": {leftover("net1", "", time.Time{})},
	}, nil, scanNow)
	if len(got) != 1 {
		t.Fatalf("ScanLeftovers = %+v, want one finding", got)
	}
	if r := got[0].Reason; !strings.Contains(r, "could not read which run") {
		t.Errorf("reason guesses at a run it cannot know: %q", r)
	}
}

// TestScanLeftovers_AnUnreachableProviderIsAFinding: "I could not ask the
// daemon what ember left on it" must not render as "nothing was left", for the
// same reason `ember ls --all` exits non-zero on a provider it could not ask.
func TestScanLeftovers_AnUnreachableProviderIsAFinding(t *testing.T) {
	got := ScanLeftovers(nil, map[node.ProviderID]error{
		"local": errors.New("docker daemon is not reachable"),
		"other": nil,
	}, scanNow)

	if len(got) != 1 {
		t.Fatalf("ScanLeftovers = %+v, want only the provider that actually failed", got)
	}
	if got[0].Kind != KindUnreachable {
		t.Errorf("kind = %s, want unreachable", got[0].Kind)
	}
	// Named explicitly, because a provider can answer one listing and fail
	// another: three identically-worded lines would read as a duplicate.
	if r := got[0].Reason; !strings.Contains(r, "left behind") {
		t.Errorf("reason does not say which listing failed: %q", r)
	}
}

func TestScanLeftovers_SortsAttentionFirstThenByProviderAndName(t *testing.T) {
	got := ScanLeftovers(map[node.ProviderID][]node.Leftover{
		"local": {
			leftover("b", "r2", scanNow.Add(-time.Hour)),
			leftover("a", "r1", scanNow.Add(-time.Hour)),
		},
	}, nil, scanNow)

	if len(got) != 2 {
		t.Fatalf("ScanLeftovers = %+v", got)
	}
	if got[0].Leftover.Name != "ember-a" || got[1].Leftover.Name != "ember-b" {
		t.Errorf("order = %s, %s; want name order so two runs print the same way twice",
			got[0].Leftover.Name, got[1].Leftover.Name)
	}
}

func TestLeftovers_NeedAttentionUnlikeAStore(t *testing.T) {
	// The difference is that ember can tell. A store existing is the expected
	// state and might hold weights somebody still wants; a network whose node
	// is gone is unambiguously garbage.
	if !KindLeftover.NeedsAttention() {
		t.Error("a leftover does not need attention, so ember ls would exit 0 over an accumulating leak")
	}
	if KindLeftover.String() != "leftover" {
		t.Errorf("Kind.String() = %q", KindLeftover.String())
	}
}

func TestCollectLeftovers_AsksOnlyProvidersThatHaveAny(t *testing.T) {
	reg := provider.NewRegistry()
	withAny := providertest.WithLeftovers("local", leftover("net1", "r1", scanNow.Add(-time.Hour)))
	plain := providertest.NewRecordingProvider("runpod")
	for _, p := range []provider.Provider{withAny, plain} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}

	got, errs := CollectLeftovers(t.Context(), reg)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	// Skipped and not reported as empty: "this provider has nothing beside a
	// node" and "this provider has no leftovers right now" are different
	// answers, and only one of them is worth a line.
	if _, asked := got["runpod"]; asked {
		t.Error("a provider with Caps.Leftovers false was asked anyway")
	}
	if len(got["local"]) != 1 {
		t.Errorf("got[local] = %+v, want the one leftover", got["local"])
	}
}

func TestCollectLeftovers_AFailedListingIsAnError(t *testing.T) {
	reg := provider.NewRegistry()
	p := providertest.WithLeftovers("local", leftover("net1", "r1", scanNow.Add(-time.Hour)))
	p.FailOn = "LeftoverList"
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}

	got, errs := CollectLeftovers(t.Context(), reg)
	if len(got) != 0 || errs["local"] == nil {
		t.Fatalf("CollectLeftovers = %+v, %v; a listing that failed must not read as a clean daemon", got, errs)
	}
}

func TestReapLeftovers_RemovesEveryFinding(t *testing.T) {
	reg := provider.NewRegistry()
	p := providertest.WithLeftovers("local",
		leftover("net1", "r1", scanNow.Add(-time.Hour)),
		leftover("net2", "r2", scanNow.Add(-time.Hour)),
	)
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}

	fs := ScanLeftovers(map[node.ProviderID][]node.Leftover{"local": p.Leftovers}, nil, scanNow)
	removed, err := ReapLeftovers(t.Context(), reg, fs)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
	if len(p.Leftovers) != 0 {
		t.Errorf("%+v is still on the daemon", p.Leftovers)
	}
}

// TestReapLeftovers_OneFailureDoesNotStopTheRest: the whole failure this
// exists to prevent is an accumulation, so a single network Docker refuses to
// remove must not leave the others behind.
func TestReapLeftovers_OneFailureDoesNotStopTheRest(t *testing.T) {
	reg := provider.NewRegistry()
	p := providertest.WithLeftovers("local",
		leftover("net1", "r1", scanNow.Add(-time.Hour)),
		leftover("net2", "r2", scanNow.Add(-time.Hour)),
	)
	p.Errors = map[string]error{"LeftoverDestroy net1": errors.New("still has active endpoints")}
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}

	fs := ScanLeftovers(map[node.ProviderID][]node.Leftover{"local": p.Leftovers}, nil, scanNow)
	removed, err := ReapLeftovers(t.Context(), reg, fs)
	if err == nil {
		t.Fatal("ReapLeftovers reported success over a removal the daemon refused")
	}
	if removed != 1 {
		t.Errorf("removed %d, want the one that could go", removed)
	}
	if !strings.Contains(err.Error(), "ember-net1") {
		t.Errorf("the error does not name what is still there: %v", err)
	}
}

func TestReapLeftovers_IgnoresAnythingThatIsNotOne(t *testing.T) {
	reg := provider.NewRegistry()
	p := providertest.WithLeftovers("local")
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}

	// A node and a store finding, which must never reach LeftoverDestroy --
	// three resources, three lifetimes, and the guard is what stops one sweep
	// from being able to mean any of them.
	removed, err := ReapLeftovers(t.Context(), reg, []Finding{
		{Kind: KindExpired, Provider: "local", Node: node.Node{Handle: node.Handle{Provider: "local", ID: "c1"}}},
		{Kind: KindStore, Provider: "local", Store: &node.Store{Handle: node.StoreHandle{Provider: "local", ID: "vol1"}}},
		// A leftover kind with no leftover attached cannot happen through
		// ScanLeftovers; it is skipped rather than dereferenced.
		{Kind: KindLeftover, Provider: "local"},
	})
	if removed != 0 || err != nil {
		t.Fatalf("ReapLeftovers = %d, %v; it acted on something that is not a leftover", removed, err)
	}
	for _, c := range p.Calls {
		if strings.HasPrefix(c, "LeftoverDestroy") {
			t.Errorf("called %q for a finding that is not a leftover", c)
		}
	}
}

func TestReapLeftovers_UnknownProviderAndOneThatCannotSweep(t *testing.T) {
	reg := provider.NewRegistry()
	plain := providertest.NewRecordingProvider("runpod")
	if err := reg.Add(plain); err != nil {
		t.Fatal(err)
	}

	l := leftover("net1", "r1", scanNow.Add(-time.Hour))

	// A provider the registry does not have. Reported rather than skipped: a
	// finding nothing can act on must not be counted as a clean sweep.
	removed, err := ReapLeftovers(t.Context(), reg, []Finding{
		{Kind: KindLeftover, Provider: "gone", Leftover: &l},
	})
	if removed != 0 || err == nil {
		t.Errorf("ReapLeftovers = %d, %v, want an error naming the provider it could not get", removed, err)
	}

	// And one that is registered but cannot remove a leftover. Unreachable
	// through CollectLeftovers, and still an error: counting zero silently
	// would report a clean sweep over an object still sitting there.
	removed, err = ReapLeftovers(t.Context(), reg, []Finding{
		{Kind: KindLeftover, Provider: "runpod", Leftover: &l},
	})
	if removed != 0 || err == nil {
		t.Errorf("ReapLeftovers = %d, %v, want an error", removed, err)
	}
	if err != nil && !strings.Contains(err.Error(), "cannot remove one") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// TestReap_DestroyCarriesTheRunID.
//
// reap has the run id from the finding -- the node's own label first, the
// record second, which is the order everything here trusts them in. Without
// it, reaping an expired node whose container something else already removed
// leaves the network behind and reports a destroyed node, which is the bug
// this change is about reached through the unattended command rather than the
// interactive one.
func TestReap_DestroyCarriesTheRunID(t *testing.T) {
	reg := provider.NewRegistry()
	p := providertest.NewRecordingProvider("test")
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}

	n := node.Node{
		Handle: node.Handle{Provider: "test", ID: "c1"},
		RunID:  "0123456789abcdef",
		State:  node.StateReady,
	}
	destroyed, err := Reap(t.Context(), reg, nil, []Finding{
		{Kind: KindExpired, Provider: "test", Node: n},
	})
	if err != nil || destroyed != 1 {
		t.Fatalf("Reap = %d, %v", destroyed, err)
	}
	if got := p.DestroyedRuns(); len(got) != 1 || got[0] != n.RunID {
		t.Errorf("destroy was given %v, want the run id %q off the node's label", got, n.RunID)
	}
}

// TestFinding_RunIDFallsBackToTheRecord is the case the run-id index in Scan
// exists for: a node created but killed before ember recorded its id, found
// again by its label. The record is the only copy of the run id when the
// provider's listing did not carry one.
func TestFinding_RunIDPrefersTheLabelAndFallsBackToTheRecord(t *testing.T) {
	rec := &statestore.Record{RunID: "fedcba9876543210"}

	f := Finding{
		Node:   node.Node{Handle: node.Handle{Provider: "test", ID: "c1"}, RunID: "0123456789abcdef"},
		Record: rec,
	}
	if got := f.RunID(); got != "0123456789abcdef" {
		t.Errorf("RunID() = %q, want the node's label to win", got)
	}

	f.Node.RunID = ""
	if got := f.RunID(); got != rec.RunID {
		t.Errorf("RunID() = %q, want the record's %q", got, rec.RunID)
	}

	// Neither. An orphan record that never got an id has a run id; a
	// finding about an unreachable provider has neither, and asking must
	// not panic.
	if got := (Finding{}).RunID(); got != "" {
		t.Errorf("RunID() on an empty finding = %q", got)
	}
}
