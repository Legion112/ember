package reconcile

import (
	"errors"
	"os"
	"os/exec"
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

// labelled is a live node that still carries the run id ember put in its
// provider-side label, which is how a record with no node id finds it again.
func labelled(id string, runID node.RunID, state node.State, deadline time.Time) node.Node {
	n := liveNode(id, state, deadline)
	n.RunID = runID
	return n
}

// TestScan_JoinsAnIntentRecordToItsNodeByRunID is the bug the SIGKILL lab
// found. A run killed between Create returning and the node id being written
// used to produce two findings -- an orphaned record that could not name its
// node, and an untracked node no record mentioned -- and reap could act on
// neither, although both carried the same run id in ember's own label the
// whole time.
func TestScan_JoinsAnIntentRecordToItsNodeByRunID(t *testing.T) {
	deadline := now.Add(time.Hour)
	fs := Scan(
		map[node.ProviderID][]node.Node{
			"test": {labelled("n1", "run1", node.StateReady, deadline)},
		},
		nil,
		[]statestore.Record{record("run1", "", statestore.PhaseIntent, deadline)},
		now,
	)

	if len(fs) != 1 {
		t.Fatalf("one node produced %d findings: %v", len(fs), fs)
	}
	f := fs[0]
	if f.Kind != KindOrphan {
		t.Errorf("kind = %v, want orphan: the node is inside its deadline, so reap must not touch it", f.Kind)
	}
	if f.Node.Handle.ID != "n1" {
		t.Errorf("the finding does not name the node: %+v", f.Node.Handle)
	}
	if f.Record == nil {
		t.Error("the finding carries no record, so reap cannot clear it afterwards")
	}
	if !strings.Contains(f.Reason, "found again by its label") {
		t.Errorf("the reason does not explain how the pairing was made: %q", f.Reason)
	}
}

// TestScan_AnIntentRecordWhoseNodeIsExpiredIsReapable. Same join, but past the
// deadline: now it is reap's business, and reap needs the handle the join
// supplied.
func TestScan_AnIntentRecordWhoseNodeIsExpiredIsReapable(t *testing.T) {
	past := now.Add(-time.Hour)
	fs := Scan(
		map[node.ProviderID][]node.Node{
			"test": {labelled("n1", "run1", node.StateReady, past)},
		},
		nil,
		[]statestore.Record{record("run1", "", statestore.PhaseIntent, past)},
		now,
	)

	if len(fs) != 1 || fs[0].Kind != KindExpired {
		t.Fatalf("got %v, want one expired finding", fs)
	}
	if fs[0].Node.Handle.ID != "n1" {
		t.Fatal("an expired finding with no handle is one reap cannot act on")
	}
}

// TestScan_AnIntentRecordWithNoMatchingNodeStaysUnanswerable. The create call
// may still have created something the provider has not listed yet, and only
// the provider can say -- so it is reported rather than assumed either way.
func TestScan_AnIntentRecordWithNoMatchingNodeStaysUnanswerable(t *testing.T) {
	deadline := now.Add(time.Hour)
	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {}},
		nil,
		[]statestore.Record{record("run1", "", statestore.PhaseIntent, deadline)},
		now,
	)

	if len(fs) != 1 || fs[0].Kind != KindOrphan {
		t.Fatalf("got %v, want one orphan", fs)
	}
	if fs[0].Node.Handle.ID != "" {
		t.Error("a finding with no matching node should carry no handle")
	}
	if !strings.Contains(fs[0].Reason, "no node wears this run's label") {
		t.Errorf("the reason does not say the label was looked for: %q", fs[0].Reason)
	}
}

// TestScan_ANodeWithNoRunIDIsStillUntracked. A provider that cannot report a
// run id -- or one whose label failed to parse -- must not have its node
// silently joined to an unrelated record.
func TestScan_ANodeWithNoRunIDIsStillUntracked(t *testing.T) {
	deadline := now.Add(time.Hour)
	fs := Scan(
		map[node.ProviderID][]node.Node{
			"test": {liveNode("n1", node.StateReady, deadline)}, // no RunID
		},
		nil,
		[]statestore.Record{record("run1", "", statestore.PhaseIntent, deadline)},
		now,
	)

	got := kinds(fs)
	if got[KindUntracked] != 1 || got[KindOrphan] != 1 {
		t.Fatalf("got %v, want one untracked node and one unanswerable record", got)
	}
}

// deadHolder is a holder on this machine whose process has certainly exited.
func deadHolder(t *testing.T) statestore.Holder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	h := statestore.ThisProcess()
	h.PID = pid
	if !h.Abandoned() {
		t.Fatalf("pid %d is still alive, so this test would prove nothing", pid)
	}
	return h
}

// TestScan_ANodeWhoseHolderIsGoneIsNotOK is the gap the SIGKILL lab found on
// CI. The node is running, recorded, and inside its deadline, so every signal
// reconcile had said "ok" -- and ls exited 0 over a GPU that was billing with
// nobody watching, which is the exact failure the whole cost-safety design
// exists to prevent.
func TestScan_ANodeWhoseHolderIsGoneIsNotOK(t *testing.T) {
	deadline := now.Add(time.Hour)
	rec := record("run1", "n1", statestore.PhaseReady, deadline)
	rec.Holder = deadHolder(t)

	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, deadline)}},
		nil,
		[]statestore.Record{rec},
		now,
	)

	if len(fs) != 1 {
		t.Fatalf("got %v", fs)
	}
	if fs[0].Kind != KindOrphan {
		t.Errorf("kind = %v, want orphan", fs[0].Kind)
	}
	// The operator needs the command that fixes it, not just the diagnosis:
	// reap deliberately will not touch a node inside its deadline.
	if !strings.Contains(fs[0].Reason, "ember down test/n1") {
		t.Errorf("the reason does not name the remedy: %q", fs[0].Reason)
	}
	if !strings.Contains(fs[0].Reason, "nobody watching") {
		t.Errorf("the reason does not say what is wrong: %q", fs[0].Reason)
	}
}

// TestScan_AnAbandonedHolderIsAlsoSeenOnTheLabelMatchedPath, which is the
// crash-before-the-id-was-recorded case. Both paths lead to a live node and a
// record, so both have to make the same judgement.
func TestScan_AnAbandonedHolderIsAlsoSeenOnTheLabelMatchedPath(t *testing.T) {
	deadline := now.Add(time.Hour)
	rec := record("run1", "", statestore.PhaseIntent, deadline)
	rec.Holder = deadHolder(t)

	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {labelled("n1", "run1", node.StateReady, deadline)}},
		nil,
		[]statestore.Record{rec},
		now,
	)

	if len(fs) != 1 || fs[0].Kind != KindOrphan {
		t.Fatalf("got %v", fs)
	}
	if !strings.Contains(fs[0].Reason, "nobody watching") {
		t.Errorf("the label-matched path does not check the holder: %q", fs[0].Reason)
	}
}

// TestScan_ALiveHolderIsStillOK. The baseline that keeps the feature worth
// having: a healthy long-running session must not become a finding, or the
// operator learns to ignore ls.
func TestScan_ALiveHolderIsStillOK(t *testing.T) {
	deadline := now.Add(time.Hour)
	rec := record("run1", "n1", statestore.PhaseReady, deadline)
	rec.Holder = statestore.ThisProcess()

	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, deadline)}},
		nil,
		[]statestore.Record{rec},
		now,
	)
	if len(fs) != 1 || fs[0].Kind != KindOK {
		t.Fatalf("got %v, want one ok finding", fs)
	}
}

// TestScan_AnExpiredNodeIsExpiredRegardlessOfItsHolder. Past the deadline is
// reap's business whether or not anybody is holding it; reporting it as merely
// abandoned would leave it to a human.
func TestScan_AnExpiredNodeIsExpiredRegardlessOfItsHolder(t *testing.T) {
	past := now.Add(-time.Hour)
	rec := record("run1", "n1", statestore.PhaseReady, past)
	rec.Holder = deadHolder(t)

	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, past)}},
		nil,
		[]statestore.Record{rec},
		now,
	)
	if len(fs) != 1 || fs[0].Kind != KindExpired {
		t.Fatalf("got %v, want expired", fs)
	}
}

// TestScan_ReportsWhatANodeHasCostSoFar. "An orphaned H100 burns 175 roubles
// an hour in silence" is the reason this command exists, and a number is what
// turns that from a sentence in a README into something an operator acts on
// this afternoon.
func TestScan_ReportsWhatANodeHasCostSoFar(t *testing.T) {
	deadline := now.Add(time.Hour)
	rec := record("run1", "n1", statestore.PhaseReady, deadline)
	rec.Price = node.Rate(2_690_000) // an H100, $2.69/hr
	rec.Created = now.Add(-90 * time.Minute)

	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, deadline)}},
		nil,
		[]statestore.Record{rec},
		now,
	)
	if len(fs) != 1 {
		t.Fatalf("got %v", fs)
	}
	if fs[0].Accrued == nil {
		t.Fatal("a recorded node reported no accrued cost")
	}
	// 90 minutes at $2.69/hr.
	if want := node.MicroUSD(4_035_000); fs[0].Accrued.Cost != want {
		t.Errorf("cost = %v (%d micro), want %v", fs[0].Accrued.Cost, int64(fs[0].Accrued.Cost), want)
	}
	if !strings.Contains(fs[0].String(), "so far") {
		t.Errorf("the line does not carry the cost: %q", fs[0].String())
	}
}

// TestScan_NoCostWhereEmberCannotKnowOne. An untracked node has no record, so
// ember has no quoted rate and no start time; inventing one would be worse
// than saying nothing, because a wrong number is acted on with the same
// confidence as a right one.
func TestScan_NoCostWhereEmberCannotKnowOne(t *testing.T) {
	deadline := now.Add(time.Hour)
	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, deadline)}},
		nil, nil, now,
	)
	if len(fs) != 1 || fs[0].Kind != KindUntracked {
		t.Fatalf("got %v", fs)
	}
	if fs[0].Accrued != nil {
		t.Errorf("a cost was reported for a node ember knows nothing about: %v", fs[0].Accrued)
	}
	if strings.Contains(fs[0].String(), "so far") {
		t.Errorf("the line claims a cost: %q", fs[0].String())
	}
}

// TestScan_AFreeProviderPrintsNoCostLine. A local run reporting "~$0 so far"
// invites the reader to wonder whether the accounting is broken.
func TestScan_AFreeProviderPrintsNoCostLine(t *testing.T) {
	deadline := now.Add(time.Hour)
	rec := record("run1", "n1", statestore.PhaseReady, deadline)
	rec.Price = 0

	fs := Scan(
		map[node.ProviderID][]node.Node{"test": {liveNode("n1", node.StateReady, deadline)}},
		nil, []statestore.Record{rec}, now,
	)
	if len(fs) != 1 {
		t.Fatalf("got %v", fs)
	}
	if strings.Contains(fs[0].String(), "so far") {
		t.Errorf("a free run reported a cost: %q", fs[0].String())
	}
}
