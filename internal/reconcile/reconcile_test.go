package reconcile

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
	"github.com/Legion112/ember/internal/statestore"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func liveNode(id string, state node.State, deadline time.Time) node.Node {
	return node.Node{
		Handle:   node.Handle{Provider: "test", ID: id},
		State:    state,
		Deadline: deadline,
	}
}

func record(runID node.RunID, nodeID string, phase statestore.Phase, deadline time.Time) statestore.Record {
	return statestore.Record{
		RunID: runID, Provider: "test", NodeID: nodeID,
		Phase: phase, Label: "ember-" + string(runID) + "-1", Deadline: deadline,
	}
}

func kinds(fs []Finding) map[Kind]int {
	out := map[Kind]int{}
	for _, f := range fs {
		out[f.Kind]++
	}
	return out
}

func TestScan_HealthyNodeIsOK(t *testing.T) {
	id := node.NewRunID()
	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, now.Add(time.Hour))}},
		nil,
		[]statestore.Record{record(id, "n1", statestore.PhaseReady, now.Add(time.Hour))},
		now,
	)
	if len(fs) != 1 || fs[0].Kind != KindOK {
		t.Fatalf("got %v", fs)
	}
	if Attention(fs) != 0 {
		t.Error("a healthy node must not need attention")
	}
}

// TestScan_AnUnreachableProviderIsAFinding. "I could not check RunPod" must
// never render as a clean bill of health.
func TestScan_AnUnreachableProviderIsAFinding(t *testing.T) {
	fs := Scan(nil, map[node.ProviderID]error{"test": errors.New("connection refused")}, nil, now)
	if len(fs) != 1 || fs[0].Kind != KindUnreachable {
		t.Fatalf("got %v", fs)
	}
	if Attention(fs) == 0 {
		t.Fatal("an unreachable provider must make ls exit non-zero")
	}
	if !strings.Contains(fs[0].Reason, "connection refused") {
		t.Errorf("the reason should carry the provider's error: %q", fs[0].Reason)
	}
}

// TestScan_UntrackedNodeIsFound is what a lost state directory looks like, and
// the node is still billing.
func TestScan_UntrackedNodeIsFound(t *testing.T) {
	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, now.Add(time.Hour))}},
		nil, nil, now,
	)
	if len(fs) != 1 || fs[0].Kind != KindUntracked {
		t.Fatalf("got %v", fs)
	}
}

func TestScan_DestroyedNodesAreNotReported(t *testing.T) {
	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateDestroyed, now)}},
		nil, nil, now,
	)
	if len(fs) != 0 {
		t.Fatalf("a destroyed node is not a finding: %v", fs)
	}
}

func TestScan_ExpiredIsReportedWhetherTracked(t *testing.T) {
	past := now.Add(-time.Hour)
	id := node.NewRunID()

	tracked := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, past)}},
		nil,
		[]statestore.Record{record(id, "n1", statestore.PhaseReady, past)},
		now,
	)
	if k := kinds(tracked); k[KindExpired] != 1 {
		t.Errorf("tracked expired node: %v", tracked)
	}

	untracked := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n2", node.StateReady, past)}},
		nil, nil, now,
	)
	if k := kinds(untracked); k[KindExpired] != 1 {
		t.Errorf("untracked expired node: %v", untracked)
	}
}

// TestScan_AnIntentWithNoIDIsAnOrphan. The create call may still have created
// something, and only the provider can say.
func TestScan_AnIntentWithNoIDIsAnOrphan(t *testing.T) {
	id := node.NewRunID()
	rec := record(id, "", statestore.PhaseIntent, now.Add(time.Hour))

	fs := Scan(nil, nil, []statestore.Record{rec}, now)
	if len(fs) != 1 || fs[0].Kind != KindOrphan {
		t.Fatalf("got %v", fs)
	}
	// The label is how the operator goes and looks.
	if !strings.Contains(fs[0].Reason, rec.Label) {
		t.Errorf("the reason should carry the label to search for: %q", fs[0].Reason)
	}
}

func TestScan_ADestroyedIntentIsNotAnOrphan(t *testing.T) {
	rec := record(node.NewRunID(), "", statestore.PhaseDestroyed, now)
	if fs := Scan(nil, nil, []statestore.Record{rec}, now); len(fs) != 0 {
		t.Fatalf("got %v", fs)
	}
}

func TestScan_RecordWithNoNodeAtTheProviderIsAnOrphan(t *testing.T) {
	id := node.NewRunID()
	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {}},
		nil,
		[]statestore.Record{record(id, "n1", statestore.PhaseReady, now.Add(time.Hour))},
		now,
	)
	if len(fs) != 1 || fs[0].Kind != KindOrphan {
		t.Fatalf("got %v", fs)
	}
}

// TestScan_DoesNotCallSomethingAnOrphanWhenItCouldNotLook. A provider that
// failed to answer tells us nothing about its nodes.
func TestScan_DoesNotCallSomethingAnOrphanWhenItCouldNotLook(t *testing.T) {
	id := node.NewRunID()
	fs := Scan(
		nil,
		map[node.ProviderID]error{"test": errors.New("timeout")},
		[]statestore.Record{record(id, "n1", statestore.PhaseReady, now.Add(time.Hour))},
		now,
	)
	k := kinds(fs)
	if k[KindOrphan] != 0 {
		t.Errorf("a record was called an orphan although the provider was never asked: %v", fs)
	}
	if k[KindUnreachable] != 1 {
		t.Errorf("the unreachable provider was not reported: %v", fs)
	}
}

func TestScan_DestroyFailedIsReportedEvenWhenTheNodeIsGone(t *testing.T) {
	id := node.NewRunID()
	rec := record(id, "n1", statestore.PhaseDestroyFailed, now.Add(time.Hour))
	rec.LastErr = "provider unreachable"

	fs := Scan(map[node.ProviderID][]node.Node{"test": {}}, nil, []statestore.Record{rec}, now)
	if len(fs) != 1 || fs[0].Kind != KindDestroyFailed {
		t.Fatalf("got %v", fs)
	}
	if !strings.Contains(fs[0].Reason, "provider unreachable") {
		t.Errorf("the original failure should be carried: %q", fs[0].Reason)
	}
}

// TestScan_SortsTheWorstFirst, so an operator reading a long list sees what
// costs money before what does not.
func TestScan_SortsTheWorstFirst(t *testing.T) {
	past := now.Add(-time.Hour)
	okID, expID := node.NewRunID(), node.NewRunID()

	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {
			liveNode("ok", node.StateReady, now.Add(time.Hour)),
			liveNode("exp", node.StateReady, past),
		}},
		map[node.ProviderID]error{"other": errors.New("down")},
		[]statestore.Record{
			record(okID, "ok", statestore.PhaseReady, now.Add(time.Hour)),
			record(expID, "exp", statestore.PhaseReady, past),
		},
		now,
	)
	if len(fs) < 3 {
		t.Fatalf("got %d findings", len(fs))
	}
	if fs[len(fs)-1].Kind != KindOK {
		t.Errorf("the healthy node should sort last, got %v", fs)
	}
	if Attention(fs) != 2 {
		t.Errorf("Attention = %d, want 2", Attention(fs))
	}
}

func TestKind_String(t *testing.T) {
	if KindExpired.String() != "expired" {
		t.Errorf("got %q", KindExpired.String())
	}
	if got := Kind(99).String(); got != "kind(99)" {
		t.Errorf("got %q", got)
	}
	if !KindUntracked.NeedsAttention() || KindOK.NeedsAttention() {
		t.Error("NeedsAttention is inverted")
	}
}

func TestFinding_StringNamesSomethingUseful(t *testing.T) {
	id := node.NewRunID()
	rec := record(id, "", statestore.PhaseIntent, now)

	withNode := Finding{Kind: KindExpired, Node: liveNode("n1", node.StateReady, now)}
	if !strings.Contains(withNode.String(), "test/n1") {
		t.Errorf("got %q", withNode.String())
	}
	withRecord := Finding{Kind: KindOrphan, Provider: "test", Record: &rec}
	if !strings.Contains(withRecord.String(), string(id)) {
		t.Errorf("got %q", withRecord.String())
	}
	bare := Finding{Kind: KindUnreachable}
	if !strings.Contains(bare.String(), "-") {
		t.Errorf("got %q", bare.String())
	}
}

func TestCollect_KeepsPartialResultsAndFailuresSideBySide(t *testing.T) {
	good := providertest.NewRecordingProvider("good")
	good.Existing = []node.Node{liveNode("n1", node.StateReady, now)}
	bad := providertest.NewRecordingProvider("bad")
	bad.FailOn = "List"

	reg := provider.NewRegistry()
	for _, p := range []*providertest.RecordingProvider{good, bad} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}

	listings, errs := Collect(t.Context(), reg)
	if len(listings["good"]) != 1 {
		t.Error("one provider failing hid what the other said")
	}
	if errs["bad"] == nil {
		t.Error("the failure was swallowed")
	}
}

func TestReap_DestroysOnlyWhatIsExpired(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	reg := provider.NewRegistry()
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}
	store, err := statestore.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	id := node.NewRunID()
	if err := store.Create(record(id, "exp", statestore.PhaseReady, now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	fs := []Finding{
		{Kind: KindExpired, Provider: "test", Node: liveNode("exp", node.StateReady, now.Add(-time.Hour)), Record: &rec},
		// Untracked is reported and left alone: it might be another machine's
		// node, and destroying somebody's running work because this laptop
		// had not heard of it is not a trade to make automatically.
		{Kind: KindUntracked, Provider: "test", Node: liveNode("mystery", node.StateReady, now.Add(time.Hour))},
		{Kind: KindOK, Provider: "test", Node: liveNode("fine", node.StateReady, now.Add(time.Hour))},
	}

	n, err := Reap(t.Context(), reg, store, fs)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("destroyed %d, want only the expired one", n)
	}
	calls := strings.Join(p.Recorded(), "|")
	if !strings.Contains(calls, "Destroy exp") {
		t.Error("the expired node was not destroyed")
	}
	if strings.Contains(calls, "mystery") || strings.Contains(calls, "fine") {
		t.Errorf("reap touched something it should not have: %s", calls)
	}
	if _, err := store.Get(id); err == nil {
		t.Error("the record was not cleared after a confirmed destroy")
	}
}

func TestReap_ReportsFailuresAndKeepsGoing(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Errors["Destroy bad"] = errors.New("provider said no")
	reg := provider.NewRegistry()
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}

	fs := []Finding{
		{Kind: KindExpired, Provider: "test", Node: liveNode("bad", node.StateReady, now.Add(-time.Hour))},
		{Kind: KindExpired, Provider: "test", Node: liveNode("good", node.StateReady, now.Add(-time.Hour))},
		{Kind: KindExpired, Provider: "nowhere", Node: liveNode("x", node.StateReady, now.Add(-time.Hour))},
	}
	n, err := Reap(t.Context(), reg, nil, fs)
	if err == nil {
		t.Fatal("a failed destroy was not reported")
	}
	if n != 1 {
		t.Errorf("destroyed %d; one failure must not stop the rest", n)
	}
}

// TestReap_TreatsAMissingNodeAsDestroyed, because the goal is that it is gone.
func TestReap_TreatsAMissingNodeAsDestroyed(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Errors["Destroy ghost"] = node.ErrNotFound
	reg := provider.NewRegistry()
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}

	n, err := Reap(t.Context(), reg, nil,
		[]Finding{{Kind: KindExpired, Provider: "test", Node: liveNode("ghost", node.StateReady, now.Add(-time.Hour))}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("destroyed %d, want 1", n)
	}
}
