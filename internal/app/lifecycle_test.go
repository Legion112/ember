package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
)

// collect keeps what a sink was given.
type collect struct{ es []report.Event }

func (c *collect) Emit(e report.Event) { c.es = append(c.es, e) }

func (c *collect) of(k report.Kind) []report.Event {
	var out []report.Event
	for _, e := range c.es {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func handle(id string) node.Handle { return node.Handle{Provider: "test", ID: id} }

var errTest = errors.New("provider unreachable")

// TestLifecycle_NodeReadyCarriesTheDeadlineAndThePrice.
//
// Those two are what an operator watching a rental needs: how long they have,
// and what it is costing while they have it. A UI missing either shows a node
// that appears to be free and to live forever.
func TestLifecycle_NodeReadyCarriesTheDeadlineAndThePrice(t *testing.T) {
	c := &collect{}
	plan := Plan{}
	o := plan.lifecycle(c)

	deadline := time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC)
	o.OnReady(handle("n1"), node.Endpoint{}, deadline, node.Rate(340000))

	got := c.of(report.KindNodeReady)
	if len(got) != 1 {
		t.Fatalf("got %d node_ready event(s), want 1", len(got))
	}
	if got[0].Node != "test/n1" {
		t.Errorf("node = %q, want test/n1", got[0].Node)
	}
	if !got[0].Deadline.Equal(deadline) {
		t.Errorf("deadline = %v, want %v", got[0].Deadline, deadline)
	}
	if got[0].Rate != node.Rate(340000) {
		t.Errorf("rate = %v, want $0.34/hr", got[0].Rate)
	}
}

// TestLifecycle_TheCostIsOneEventAndNotALogLine. The whole reason the hook was
// added to internal/provision: this number was reachable only through slog,
// from a deferred teardown, after the caller's closure had returned.
func TestLifecycle_TheCostIsOneEventAndNotALogLine(t *testing.T) {
	c := &collect{}
	plan := Plan{}
	o := plan.lifecycle(c)

	o.OnDestroyed(handle("n1"), node.Billing{
		Rate:     node.Rate(340000),
		Elapsed:  90 * time.Second,
		Billable: 2 * time.Minute,
		Cost:     node.MicroUSD(11333),
	}, nil)

	got := c.of(report.KindRunCost)
	if len(got) != 1 {
		t.Fatalf("got %d run_cost event(s), want 1", len(got))
	}
	e := got[0]
	if e.Cost != node.MicroUSD(11333) {
		t.Errorf("cost = %v, want $0.011333", e.Cost)
	}
	// Both durations, because they differ and the difference is the provider's
	// minimum billing unit. A reader given only the elapsed time would under-
	// report what was actually charged.
	if e.Elapsed != 90*time.Second || e.Billable != 2*time.Minute {
		t.Errorf("elapsed/billable = %v/%v, want 1m30s/2m0s", e.Elapsed, e.Billable)
	}
	if len(c.of(report.KindDestroyFailed)) != 0 {
		t.Error("a successful destroy also reported a failure")
	}
}

// TestLifecycle_AFailedDestroyIsNotACost.
//
// The two are different events on purpose. A failed destroy has no cost to
// report -- the node may still be running, so the bill is still growing -- and
// emitting a run_cost of zero would tell an operator the rental was free at
// the exact moment it has become unbounded.
func TestLifecycle_AFailedDestroyIsNotACost(t *testing.T) {
	c := &collect{}
	plan := Plan{}
	o := plan.lifecycle(c)

	o.OnDestroyed(handle("n1"), node.Billing{}, errTest)

	if n := len(c.of(report.KindRunCost)); n != 0 {
		t.Errorf("got %d run_cost event(s) for a destroy that failed, want 0", n)
	}
	got := c.of(report.KindDestroyFailed)
	if len(got) != 1 {
		t.Fatalf("got %d destroy_failed event(s), want 1", len(got))
	}
	if got[0].Node != "test/n1" {
		t.Errorf("the failure did not name the node that may still be running: %q", got[0].Node)
	}
	if got[0].Err == "" {
		t.Error("the failure did not say what went wrong")
	}
}

// TestLifecycle_DoesNotWriteBackToThePlansOptions is the property that makes
// this safe for a server.
//
// provision.Options is a value, and `ember serve` builds one at startup and
// shares it across runs -- BuildOptions says why the journal and the pin store
// must be per process. Assigning the hooks in place would send one run's
// progress to whichever run started last, which is a bug nothing else here
// could see: both runs would look like they were working.
func TestLifecycle_DoesNotWriteBackToThePlansOptions(t *testing.T) {
	// Through a pointer deliberately. With Plan{} the value receiver copies
	// before anything can be assigned, so the only way to introduce the bug
	// would be to change the receiver -- which breaks this file's other
	// callers and fails the *build* instead of this test. A reinstatement
	// that cannot tell a build failure from a pass is not a reinstatement, so
	// the one plan under test is addressable and the bug compiles.
	p := &Plan{}
	a, b := &collect{}, &collect{}

	oa := p.lifecycle(a)
	ob := p.lifecycle(b)

	if p.Options.OnReady != nil || p.Options.OnDestroyed != nil {
		t.Fatal("lifecycle wrote its hooks back onto the plan's shared Options")
	}

	oa.OnReady(handle("na"), node.Endpoint{}, time.Now(), 0)
	ob.OnReady(handle("nb"), node.Endpoint{}, time.Now(), 0)

	if len(a.of(report.KindNodeReady)) != 1 || len(b.of(report.KindNodeReady)) != 1 {
		t.Fatalf("one run's events reached the other: a=%d b=%d",
			len(a.of(report.KindNodeReady)), len(b.of(report.KindNodeReady)))
	}
	if a.es[0].Node != "test/na" || b.es[0].Node != "test/nb" {
		t.Errorf("events crossed: a saw %q, b saw %q", a.es[0].Node, b.es[0].Node)
	}
}

// TestLifecycle_OnlyTheEventThatMeansADeadlineCarriesOne.
//
// THE OTHER HALF OF report.Event's omitzero, asserted against the events this
// package really builds rather than against a struct literal. The tag test in
// internal/report proves a zero Deadline is absent from the wire; this one
// proves the field really is zero on the kinds that do not mean it -- which
// is the gap a check of either half alone sits in, and the gap
// `capabilities.model` and the unrecorded preview frame both sat in.
//
// node_memory is in the list because it is the kind furthest from the
// deadline's own hook: a different function, a different probe, and one of
// the five the issue measured carrying "deadline":"0001-01-01T00:00:00Z".
func TestLifecycle_OnlyTheEventThatMeansADeadlineCarriesOne(t *testing.T) {
	c := &collect{}
	o := Plan{}.lifecycle(c)

	deadline := time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC)
	o.OnReady(handle("n1"), node.Endpoint{}, deadline, node.Rate(340000))
	o.OnDestroyed(handle("n1"), node.Billing{
		Rate: node.Rate(340000), Elapsed: 90 * time.Second,
		Billable: 2 * time.Minute, Cost: node.MicroUSD(11333),
	}, nil)
	o.OnDestroyed(handle("n1"), node.Billing{}, errTest)

	spec := memSpec(t)
	reportMemory(t.Context(), c, answer(t, spec, strings.Join([]string{
		"cgroup2_max 34359738368",
		"cgroup2_current 33285996544",
		"mem_total_kb 741343232",
		"assets 60129542144",
	}, "\n")), spec, memOpts())

	if len(c.es) != 4 {
		t.Fatalf("got %d event(s), want node_ready, run_cost, destroy_failed and node_memory: %+v", len(c.es), c.es)
	}
	for _, e := range c.es {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(b, &keys); err != nil {
			t.Fatal(err)
		}
		raw, present := keys["deadline"]
		if e.Kind == report.KindNodeReady {
			if !present || string(raw) != `"2026-11-01T13:00:00Z"` {
				t.Errorf("node_ready lost the deadline an operator needs: %s", b)
			}
			continue
		}
		if present {
			t.Errorf("a %s claims a deadline nobody set: %s", e.Kind, raw)
		}
	}
}
