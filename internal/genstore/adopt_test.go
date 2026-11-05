package genstore

import (
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// The tests for the entry point `ember up` uses.
//
// Every one of them runs against both stores, for the reason the table exists:
// a fake that is kinder than the thing it stands in for tests a store that
// does not exist. Two of them are specifically about that -- Adopt forgetting
// what a run produces, and a jobless run's figures -- because both were
// places the fake could have been more generous than the INSERT and the SQL.

// TestAdopt_RecordsARunNobodyQueued round-trips the ask.
func TestAdopt_RecordsARunNobodyQueued(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		in := req("a cat")
		in.Offer = "offer-1"
		id, err := s.Adopt(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provider != "local" || got.GPU != "rtx2080ti" || got.Offer != "offer-1" {
			t.Errorf("selection = %q/%q/%q", got.Provider, got.GPU, got.Offer)
		}
		if got.TTL != time.Hour || got.MaxHourly != node.Rate(340000) {
			t.Errorf("ceilings = %v/%v", got.TTL, got.MaxHourly)
		}
		if len(got.Jobs) != 1 || got.Jobs[0].Prompt != "a cat" {
			t.Errorf("jobs = %+v", got.Jobs)
		}
		// Running and already started. The row is written before the create
		// call, so the run HAS begun -- a zero start would make Entry.Wall
		// zero for every `ember up` and read as a generation that took no
		// time.
		if got.Status != StatusRunning {
			t.Errorf("status = %q, want running", got.Status)
		}
		if got.Started.IsZero() {
			t.Error("Adopt recorded no start, so the run's own wall time is unknowable")
		}
	})
}

// TestAdopt_IsNotClaimable is the money test.
//
// The reason Adopt is not Enqueue: Enqueue writes StatusQueued whatever the
// caller said, and a queued row is what ClaimNext takes. An `ember up` row
// written that way would be claimed by an `ember serve` in another terminal --
// the ordinary setup for somebody who has both -- which would rent a SECOND
// card and generate the batch again from the row describing the run already in
// flight. One mistaken status, two GPUs.
func TestAdopt_IsNotClaimable(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if _, err := s.Adopt(t.Context(), req("a cat")); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.ClaimNext(t.Context(), "run-1", Compat{})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("a queue claimed %s, which `ember up` is already running: "+
				"that rents a second card for work in flight", got.ID)
		}
	})
}

// TestAdopt_TakesARunWithNoJobs, which Enqueue refuses.
//
// A configuration that describes no jobs has none, and that rental is the
// cheapest thing ember does and the easiest to leak. Refusing to record it
// would refuse exactly the row worth having.
func TestAdopt_TakesARunWithNoJobs(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		jobless := req()
		if len(jobless.Jobs) != 0 {
			t.Fatal("the fixture has jobs, so this test proves nothing")
		}
		id, err := s.Adopt(t.Context(), jobless)
		if err != nil {
			t.Fatalf("Adopt refused a forwarded run: %v", err)
		}
		if _, err := s.Enqueue(t.Context(), jobless); err == nil {
			t.Error("Enqueue accepted a request with no jobs, which nothing would ever run")
		}
		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Jobs) != 0 {
			t.Errorf("a jobless run came back with %d job(s)", len(got.Jobs))
		}
	})
}

// TestAdopt_ForgetsWhatTheRunWillProduce.
//
// Adopt's INSERT names the ask and no more, so a caller that set the cost, the
// node, the memory figures or the model on the way in has them dropped -- the
// same structural refusal that keeps Extra off Job, applied to the columns an
// operator is billed against. The fake used to remember all but the model,
// which is exactly the divergence the conformance table exists to catch: a
// caller would see the value come back, build on it, and the behaviour would
// change the day a file on disk was underneath.
func TestAdopt_ForgetsWhatTheRunWillProduce(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		in := req("a cat")
		in.Cost = node.MicroUSD(999)
		in.Rate = node.Rate(888)
		in.Node = "local/pretend"
		in.RunID = "0123456789abcdef"
		in.Model = "somebody-elses.gguf"
		in.MemCgroup, in.MemLimit = 2, 1<<30
		in.Elapsed, in.Billable = time.Minute, time.Hour
		in.Err = "a failure that has not happened"

		id, err := s.Adopt(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			field string
			zero  bool
		}{
			{"cost", got.Cost == 0},
			{"rate", got.Rate == 0},
			{"node", got.Node == ""},
			{"run_id", got.RunID == ""},
			{"model", got.Model == ""},
			{"mem_cgroup", got.MemCgroup == 0},
			{"mem_limit", got.MemLimit == 0},
			{"elapsed", got.Elapsed == 0},
			{"billable", got.Billable == 0},
			{"err", got.Err == ""},
		} {
			if !c.zero {
				t.Errorf("%s came back from Adopt; a run's outcome must not be settable on the way in", c.field)
			}
		}
	})
}

// TestHistory_AJoblessRunReportsNoPerJobFigures.
//
// The flag used to require jobs > 0, which no reachable state could make
// decisive: Validate refuses a queued request with none. Adopt makes it
// reachable, and the answer has to be the one that stops a column of zeros in
// a money ledger reading as free -- a plain `ember up` pays for a rental and
// divides it into no images, which is precisely what Unrecorded means.
func TestHistory_AJoblessRunReportsNoPerJobFigures(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Adopt(t.Context(), req())
		if err != nil {
			t.Fatal(err)
		}
		// Finished, and with a rental cost: that is the state the flag is
		// about. An unfinished one is missing nothing.
		if err := s.Update(t.Context(), id, func(r *Request) {
			r.Status = StatusDone
			r.Ended = time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC)
			r.Cost = node.MicroUSD(500000)
			r.Billable = time.Minute
		}); err != nil {
			t.Fatal(err)
		}

		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d ledger row(s), want 1", len(rows))
		}
		if rows[0].Jobs != 0 {
			t.Errorf("jobs = %d, want 0", rows[0].Jobs)
		}
		if rows[0].Cost != node.MicroUSD(500000) {
			t.Errorf("rental = %v, want $0.50 -- the whole point of recording it", rows[0].Cost)
		}
		if !rows[0].Unrecorded {
			t.Error("a finished run with no jobs is not flagged, so `ember history` prints " +
				"0 images and $0 of card time, which reads as a rental that produced " +
				"nothing for free")
		}

		buckets, err := s.Spend(t.Context(), SpendQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(buckets) != 1 {
			t.Fatalf("got %d bucket(s), want 1", len(buckets))
		}
		if buckets[0].Unrecorded != 1 {
			t.Errorf("Bucket.Unrecorded = %d, want 1: `ember spend` must say it had to "+
				"leave this rental's per-job figures out", buckets[0].Unrecorded)
		}
		if buckets[0].RunCost != node.MicroUSD(500000) {
			t.Errorf("RunCost = %v, want $0.50", buckets[0].RunCost)
		}
	})
}

// TestAdopt_RefusesARunWithNoTTL, because a node nothing destroys on a
// deadline is the leak this program exists to prevent, and a record of one
// that claims no deadline is a record nothing can reconcile.
func TestAdopt_RefusesARunWithNoTTL(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		r := req("a cat")
		r.TTL = 0
		if _, err := s.Adopt(t.Context(), r); err == nil {
			t.Error("Adopt accepted a run with no TTL")
		}
		r = req("a cat")
		r.Provider = ""
		if _, err := s.Adopt(t.Context(), r); err == nil {
			t.Error("Adopt accepted a run that names no provider")
		}
	})
}
