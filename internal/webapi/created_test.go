package webapi

import (
	"context"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/reconcile"
	"github.com/Legion112/ember/internal/statestore"
)

// recorded is the instant every sweep below is built around, in a zone that is
// not UTC on purpose: statestore writes `time.Now()`, so a record made by an
// operator in Moscow carries +03:00 and the wire must still say Z. A fixture
// already in UTC would pass against a projection that forgot to convert.
var recorded = time.Date(2026, 11, 10, 14, 26, 44, 0, time.FixedZone("MSK", 3*60*60))

// aDatedSweep is one finding of each shape that can carry a record and one of
// each that cannot, so a single GET answers both halves of the rule.
func aDatedSweep(created time.Time) app.Scanned {
	return app.Scanned{
		Nodes: []reconcile.Finding{
			{
				Kind:     reconcile.KindOrphan,
				Provider: "local",
				Record:   &statestore.Record{RunID: "abc123", Created: created},
				Reason:   "recorded with no node id",
				Accrued:  &node.Billing{Rate: node.Rate(340000), Cost: node.MicroUSD(55_764_000)},
			},
			{
				// No record at all, which is what untracked MEANS: a node at
				// the provider that the journal has never heard of.
				//
				// IT STILL HAS A DEADLINE, read from the label it is wearing,
				// and that is the near-miss worth having in the fixture: a
				// deadline is the one instant ember holds for a node it has no
				// record of, so it is what an author reaches for when a column
				// looks empty. It dates the END of a rental nobody has priced,
				// not its beginning, and a row claiming it as a creation time
				// would read an hour of leak as the moment it started.
				Kind:     reconcile.KindUntracked,
				Provider: "local",
				Node: node.Node{
					Handle:   node.Handle{Provider: "local", ID: "n9"},
					Deadline: recorded.Add(30 * time.Minute),
				},
				Reason: "wearing ember's label with no record of it",
			},
			{
				Kind:     reconcile.KindUnreachable,
				Provider: "runpod",
				Reason:   "could not be asked what it is running",
			},
		},
		Leftovers: []reconcile.Finding{{
			Kind:     reconcile.KindLeftover,
			Provider: "local",
			Leftover: &node.Leftover{Name: "ember-abc123", RunID: "abc123"},
			Reason:   "network left behind",
		}},
		Stores: []reconcile.Finding{{
			Kind:     reconcile.KindStore,
			Provider: "runpod",
			Store:    &node.Store{Name: "flux2", SizeGiB: 100},
			Reason:   "a store, billing by the month",
			Monthly:  node.MicroUSD(7_000_000),
		}},
	}
}

// TestNodes_SaysWhenAFindingStartedCosting.
//
// THE DEFECT: a row carried a cost and not the instant it was measured from.
// reconcile.accrued is `node.Estimate(f.Record.Price, f.Record.Created, now, 0)`,
// so "$55.764" on an orphan was a figure an operator could not check -- a fast
// card for an hour and a slow one for a fortnight read identically, and the
// two call for very different afternoons.
//
// THE ZONE IS THE ASSERTION. The fixture is +03:00 because that is what
// statestore writes on the machine this is for, and a projection that dropped
// the `.UTC()` would ship `2026-11-10T14:26:44+03:00` -- a string the page
// parses as not-its-shape and prints raw, which is the better of the two
// failures but still three hours of disagreement with `ember history`.
func TestNodes_SaysWhenAFindingStartedCosting(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) { return aDatedSweep(recorded), nil }

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if len(got.Findings) != 3 {
		t.Fatalf("got %d finding(s), want 3", len(got.Findings))
	}
	if want := "2026-11-10T11:26:44Z"; got.Findings[0].Created != want {
		t.Errorf("created = %q, want %q: the record's own instant, in UTC",
			got.Findings[0].Created, want)
	}
	// The cost is still there. A creation time that arrived by displacing the
	// figure it explains would be a worse row than the one this replaced.
	if got.Findings[0].Accrued == nil {
		t.Error("the row lost the cost the creation time is measured from")
	}
}

// TestNodes_AFindingWithNoRecordSaysNothingAboutWhenItStarted.
//
// An untracked node is one ember has no record of, so there is no instant to
// report -- and the SAME absence is why its cost is blank. Both columns go
// quiet together, which is the honest pairing: a creation time beside an empty
// cost would invite the reading that ember knows when this started and has
// declined to price it.
//
// A store and a leftover are here for a different reason. node.Store carries
// no creation time at all and node.Leftover carries only the run's deadline,
// so neither could answer this even in principle; the test exists so that a
// later change which starts filling them is a change somebody makes on
// purpose rather than one that leaks out through a shared projection.
func TestNodes_AFindingWithNoRecordSaysNothingAboutWhenItStarted(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) { return aDatedSweep(recorded), nil }

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	for _, c := range []struct {
		what string
		in   findingView
	}{
		{"an untracked node", got.Findings[1]},
		{"an unreachable provider", got.Findings[2]},
		{"a leftover", got.Leftovers[0]},
		{"a store", got.Stores[0]},
	} {
		if c.in.Created != "" {
			t.Errorf("%s reports a creation time of %q; ember has no record of one",
				c.what, c.in.Created)
		}
	}
}

// TestNodes_ARecordWithNoCreationTimeIsNotTheYearOne.
//
// A record written by an older ember, or restored from a journal that lost the
// field, has a Record and a zero Created. `time.Time{}.Format(time.RFC3339)`
// is `0001-01-01T00:00:00Z`, which is not a missing value on screen: it is a
// confident answer, wrong by two millennia, sitting beside a cost -- and the
// reading it produces is that this leak has been running since the year one.
// Empty is the only spelling of "ember cannot say".
func TestNodes_ARecordWithNoCreationTimeIsNotTheYearOne(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) {
		return aDatedSweep(time.Time{}), nil
	}

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if c := got.Findings[0].Created; c != "" {
		t.Errorf("created = %q on a record that has no creation time, want empty", c)
	}
}
