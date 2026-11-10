package webapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/queue"
	"github.com/Legion112/ember/internal/reconcile"
)

// aFleet is two rentals this process is holding: one on the node `aSweep`
// prices, and one that is still being created.
//
// THE SECOND ONE IS WHY THE JOIN IS WORTH TESTING. A rental mid-create exists
// at no provider, so no listing mentions it and there is nothing to price --
// which is the row that a join written as "every finding gets a worker" would
// drop and a join written as "every worker gets a zero" would misprice.
func aFleet(now time.Time) []queue.Worker {
	return []queue.Worker{
		{
			Node: "local/n1", Provider: "local", Card: "rtx2080ti", Image: "ember:sd",
			Opener: "req-a", Request: "req-b",
			Since: now.Add(-9 * time.Minute), Beat: now.Add(-2 * time.Second), Channel: true,
		},
		{
			Provider: "runpod", Card: "h100", Image: "ember:flux",
			Opener: "req-c", Since: now.Add(-3 * time.Second),
		},
	}
}

// TestNodes_JoinsTheFleetToWhatTheSweepFound.
//
// TWO SOURCES, ONE RESPONSE, JOINED BY THE NODE'S HANDLE. What a rental IS
// comes from this process, which planned it; what it COSTS comes from
// reconcile, which is the one authority on money here. Pricing a rental from
// its own rate and a start time would be a second answer to "what am I paying
// for", and the second answer is the one that is wrong -- it is reconcile that
// knows about a node a provider has already stopped billing for.
func TestNodes_JoinsTheFleetToWhatTheSweepFound(t *testing.T) {
	now := time.Now()
	drain := &RecordingDrain{fleet: aFleet(now)}
	h := newHarness(t, func(o *Options) { o.Queue = drain })
	h.scan = func(context.Context) (app.Scanned, error) { return aSweep(), nil }

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if len(got.Workers) != 2 {
		t.Fatalf("got %d worker(s), want 2: %+v", len(got.Workers), got.Workers)
	}

	live := got.Workers[0]
	if live.Node != "local/n1" || live.Card != "rtx2080ti" || live.Image != "ember:sd" {
		t.Errorf("the live worker reads %+v, want what the queue planned", live)
	}
	// BOTH REQUESTS, because they are different questions and at max_nodes>1
	// they are usually different answers: the opener is who the bill is
	// attributed to and the request is what is generating.
	if live.Opener != "req-a" || live.Request != "req-b" {
		t.Errorf("opener = %q request = %q, want req-a riding req-b", live.Opener, live.Request)
	}
	if live.State != string(queue.WorkerLive) {
		t.Errorf("state = %q for a worker that beat two seconds ago", live.State)
	}
	// The cost is the FINDING's, which is the whole of the join.
	if live.Accrued == nil || live.Accrued.Micro != 11333 {
		t.Errorf("accrued = %+v, want the figure reconcile priced for local/n1", live.Accrued)
	}
	if live.Beat == "" || live.Up == "" {
		t.Errorf("beat = %q up = %q, want Go-formatted durations", live.Beat, live.Up)
	}

	starting := got.Workers[1]
	if starting.Node != "" || starting.State != string(queue.WorkerStarting) {
		t.Errorf("the second worker reads node=%q state=%q, want a rental being created",
			starting.Node, starting.State)
	}
	// ABSENT AND NOT ZERO. A "$0.00" against a card that is about to be
	// billed for is ember pricing a rental at nothing, which is the one
	// direction a money figure must never be wrong in.
	if starting.Accrued != nil {
		t.Errorf("a rental with no node at any provider was priced at %+v", starting.Accrued)
	}
	if starting.Request != "" {
		t.Errorf("a rental being created says %q is generating on it", starting.Request)
	}
}

// TestNodes_PricesEachRentalFromItsOwnNode.
//
// THE JOIN IS THE ASSERTION AND NOT THE PAIR OF VALUES. Two rentals, two node
// findings, two different costs: a join that ignored its key, took the first
// finding, or keyed on the wrong field gives both rows one figure, and a test
// with one rental could not see any of those.
//
// AND THE REINSTATEMENT THAT WAS NOT A FINDING IS RECORDED IN viewNodes, not
// here. The join is written over `sc.Nodes` because a store handle, a leftover
// and a record's run id all spell themselves `provider/name` -- and widening
// it to every finding came back GREEN, measured: no store or leftover finding
// carries `Accrued` or a `Node`, so there is nothing for this projection to
// read off one. The narrow join stays because that is the only reason it is
// harmless; a test asserting otherwise would have been a guard that cannot
// fail.
func TestNodes_PricesEachRentalFromItsOwnNode(t *testing.T) {
	now := time.Now()
	drain := &RecordingDrain{fleet: []queue.Worker{
		{Node: "local/n1", Provider: "local", Opener: "req-a", Since: now, Beat: now, Channel: true},
		{Node: "local/n2", Provider: "local", Opener: "req-b", Since: now, Beat: now, Channel: true},
	}}
	h := newHarness(t, func(o *Options) { o.Queue = drain })
	h.scan = func(context.Context) (app.Scanned, error) {
		return app.Scanned{Nodes: []reconcile.Finding{
			{
				Kind: reconcile.KindOK, Provider: "local",
				Node:    node.Node{Handle: node.Handle{Provider: "local", ID: "n1"}},
				Reason:  "running",
				Accrued: &node.Billing{Cost: node.MicroUSD(11333)},
			},
			{
				Kind: reconcile.KindOK, Provider: "local",
				Node:    node.Node{Handle: node.Handle{Provider: "local", ID: "n2"}},
				Reason:  "running",
				Accrued: &node.Billing{Cost: node.MicroUSD(777000)},
			},
		}}, nil
	}

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if len(got.Workers) != 2 {
		t.Fatalf("got %d worker(s), want 2", len(got.Workers))
	}
	want := map[string]int64{"local/n1": 11333, "local/n2": 777000}
	for _, w := range got.Workers {
		if w.Accrued == nil {
			t.Errorf("%s was priced at nothing", w.Node)
			continue
		}
		if w.Accrued.Micro != want[w.Node] {
			t.Errorf("%s is priced at %d, want %d -- the join took another node's finding",
				w.Node, w.Accrued.Micro, want[w.Node])
		}
	}
}

// TestNodes_CountsADeadlineDownAndKeepsItNegative.
//
// "HOW LONG UNTIL ITS DEADLINE" IS THE THIRD COLUMN THE FLEET VIEW IS FOR, and
// the sign is the part worth an assertion. A node past its deadline and still
// running is the `expired` finding beside this row; a countdown clamped at
// zero would hide how long it has been over, which is how much of the bill was
// for nothing.
func TestNodes_CountsADeadlineDownAndKeepsItNegative(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name     string
		deadline time.Time
		negative bool
	}{
		{"still inside it", now.Add(42 * time.Minute), false},
		{"past it", now.Add(-7 * time.Minute), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			drain := &RecordingDrain{fleet: aFleet(now)[:1]}
			h := newHarness(t, func(o *Options) { o.Queue = drain })
			h.scan = func(context.Context) (app.Scanned, error) {
				return app.Scanned{Nodes: []reconcile.Finding{{
					Kind:     reconcile.KindOK,
					Provider: "local",
					Node: node.Node{
						Handle:   node.Handle{Provider: "local", ID: "n1"},
						Deadline: c.deadline,
					},
					Reason: "running",
				}}}, nil
			}

			got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
			if len(got.Workers) != 1 || got.Workers[0].Remaining == nil {
				t.Fatalf("the worker carries no deadline: %+v", got.Workers)
			}
			if neg := strings.HasPrefix(*got.Workers[0].Remaining, "-"); neg != c.negative {
				t.Errorf("remaining = %q, want negative=%v", *got.Workers[0].Remaining, c.negative)
			}
		})
	}
}

// TestNodes_SendsTheFleetAsAnEmptyListAndNeverNull.
//
// The fourth slice on this envelope, and the fourth chance at the defect that
// has already cost this repository a whole websocket update: Go marshals a nil
// slice as `null`, and serde's `default` covers a field that is MISSING rather
// than one that is present and null.
//
// ASSERTED ON THE BYTES, because `[]` and `null` both decode into a nil Go
// slice -- a test that unmarshalled this could not tell them apart, which is
// how the first one shipped.
func TestNodes_SendsTheFleetAsAnEmptyListAndNeverNull(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) { return app.Scanned{}, nil }

	body := h.do(t, "GET", "/api/nodes", "").Body.String()
	if !strings.Contains(body, `"workers":[]`) {
		t.Errorf("a server renting nothing does not send an empty workers list:\n%s", body)
	}
}

// TestNodes_AServerWithNoDrainLoopIsRentingNothing.
//
// NIL IS AN EMPTY FLEET AND NOT A MISSING ONE, which is the opposite of the
// `sweep` signal in the page. A server with no loop is renting nothing, so
// "no rows" is the TRUE answer rather than an absence -- and this is the shape
// every handler test is in, so the route must not 500 over it.
func TestNodes_AServerWithNoDrainLoopIsRentingNothing(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) { return aSweep(), nil }

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if len(got.Workers) != 0 {
		t.Errorf("a server with no drain loop reports %d rental(s)", len(got.Workers))
	}
	// The control: the sweep still arrived, so the empty fleet is an answer
	// about rentals rather than a route that failed.
	if len(got.Findings) == 0 {
		t.Error("the sweep's own findings went missing, so this test is not about the fleet")
	}
}

// TestNodes_ABeatThatHasNeverHappenedIsNotAMomentAgo.
//
// "NOTHING HAS ARRIVED YET" AND "A MOMENT AGO" ARE DIFFERENT ANSWERS, and the
// second is the reassuring one. A zero beat formatted as a duration would read
// as the age of the epoch or as "0s" depending on which way it was computed,
// and "0s" against a channel nobody has spoken on is the single most
// misleading cell this table could print.
func TestNodes_ABeatThatHasNeverHappenedIsNotAMomentAgo(t *testing.T) {
	drain := &RecordingDrain{fleet: []queue.Worker{{
		Node: "local/n1", Provider: "local", Opener: genstore.ReqID("req-a"), Channel: true,
	}}}
	h := newHarness(t, func(o *Options) { o.Queue = drain })
	h.scan = func(context.Context) (app.Scanned, error) { return app.Scanned{}, nil }

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if len(got.Workers) != 1 {
		t.Fatalf("got %d worker(s), want 1", len(got.Workers))
	}
	if got.Workers[0].Beat != "" {
		t.Errorf("a channel nobody has spoken on reports a beat of %q", got.Workers[0].Beat)
	}
	if got.Workers[0].State != string(queue.WorkerStale) {
		t.Errorf("state = %q for an open channel that has produced nothing, want %q: a worker "+
			"that was going to beat has had the chance by then, and reporting it as healthy "+
			"would be this listing's one reassuring lie",
			got.Workers[0].State, queue.WorkerStale)
	}
}
