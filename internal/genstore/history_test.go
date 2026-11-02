package genstore

import (
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// ledger seeds one finished request on a named rental.
//
// Over spend_test.go's own ran, day, rental and job, because the ledger's
// questions are the rental-sharing ones those helpers have no reason to
// express: which request opened a rental, and which rode along on it.
func ledger(t *testing.T, s Store, provider, runID, gpu string, created time.Time,
	cost node.MicroUSD, jobs int, each node.MicroUSD) ReqID {
	t.Helper()
	r := Request{Provider: provider, GPU: gpu, TTL: time.Hour, Created: created}
	for range jobs {
		r.Jobs = append(r.Jobs, Job{Prompt: "a cat", Width: 512, Height: 512, Steps: 4})
	}
	r.RunID, r.Node = runID, runID+"-node"
	r.Started, r.Ended = created, created.Add(10*time.Minute)
	r.Cost = cost
	if cost > 0 {
		// Billable travels with the cost, because the queue gives both to the
		// request that opened the rental and neither to any other.
		r.Rate, r.Elapsed, r.Billable = node.Rate(1_790_000), 10*time.Minute, 10*time.Minute
	}
	var runs []JobRun
	for i := 1; i <= jobs; i++ {
		runs = append(runs, JobRun{
			Idx: i, Started: created, Ended: created.Add(time.Minute),
			Cost: each, Images: 2, Bytes: 3 << 20,
			Passes: 1, Steps: 28,
		})
	}
	return ran(t, s, r, runs)
}

func TestHistory_JoinsEachRequestToWhatItProduced(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ledger(t, s, "runpod", "run-a", "a100-80g", day(1), 1_790_000, 3, 380_000)

		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("History returned %d rows, want 1", len(rows))
		}
		e := rows[0]

		if e.Provider != "runpod" || e.GPU != "a100-80g" {
			t.Errorf("selection = %q/%q, want runpod/a100-80g", e.Provider, e.GPU)
		}
		if e.Status != StatusDone {
			t.Errorf("status = %q, want done", e.Status)
		}
		if e.RunID != "run-a" || e.Node != "run-a-node" {
			t.Errorf("rental = %q/%q, want run-a/run-a-node", e.RunID, e.Node)
		}
		if e.Jobs != 3 {
			t.Errorf("jobs = %d, want 3", e.Jobs)
		}
		// Two images per job, which is what batch_count does and is why the
		// image count is not the job count.
		if e.Images != 6 {
			t.Errorf("images = %d, want 6", e.Images)
		}
		if e.Bytes != 3*(3<<20) {
			t.Errorf("bytes = %d, want %d", e.Bytes, 3*(3<<20))
		}
		if e.Cost != 1_790_000 {
			t.Errorf("rental cost = %v, want $1.79", e.Cost)
		}
		if e.JobCost != 3*380_000 {
			t.Errorf("card time cost = %v, want $1.14", e.JobCost)
		}
		if e.Steps != 3*28 {
			t.Errorf("steps = %d, want 84", e.Steps)
		}
		if e.Sampling != 3*time.Minute {
			t.Errorf("sampling = %v, want 3m", e.Sampling)
		}
		if e.Wall() != 10*time.Minute {
			t.Errorf("wall = %v, want 10m", e.Wall())
		}
		if e.Rode {
			t.Error("the request that paid for the rental is marked as a rider")
		}
		if e.Unrecorded {
			t.Error("a request whose jobs all reported is marked unrecorded")
		}
	})
}

// The ledger's per-request figures and the aggregate's sums come from one
// roll-up, and this is what says so. Two spellings of "what this run
// produced" would let `ember spend` and `ember history` disagree about one
// run with nothing to say which is right.
func TestHistory_AgreesWithTheAggregate(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ledger(t, s, "runpod", "run-a", "a100-80g", day(1), 1_790_000, 2, 380_000)
		ledger(t, s, "runpod", "run-b", "rtx4090", day(2), 890_000, 3, 95_000)

		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		buckets, err := s.Spend(t.Context(), SpendQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(buckets) != 1 {
			t.Fatalf("Spend returned %d buckets, want 1 total", len(buckets))
		}
		b := buckets[0]

		var sum Entry
		for _, e := range rows {
			sum.Jobs += e.Jobs
			sum.Images += e.Images
			sum.Bytes += e.Bytes
			sum.Cost += e.Cost
			sum.JobCost += e.JobCost
			sum.Steps += e.Steps
			sum.Sampling += e.Sampling
		}
		if len(rows) != b.Requests {
			t.Errorf("the ledger has %d rows and the aggregate counts %d requests", len(rows), b.Requests)
		}
		if sum.Jobs != b.Jobs || sum.Images != b.Images || sum.Bytes != b.Bytes {
			t.Errorf("ledger jobs/images/bytes = %d/%d/%d, aggregate = %d/%d/%d",
				sum.Jobs, sum.Images, sum.Bytes, b.Jobs, b.Images, b.Bytes)
		}
		if sum.Cost != b.RunCost || sum.JobCost != b.JobCost {
			t.Errorf("ledger rental/card = %v/%v, aggregate = %v/%v",
				sum.Cost, sum.JobCost, b.RunCost, b.JobCost)
		}
		if sum.Steps != b.Steps || sum.Sampling != b.Sampling {
			t.Errorf("ledger steps/sampling = %d/%v, aggregate = %d/%v",
				sum.Steps, sum.Sampling, b.Steps, b.Sampling)
		}
	})
}

// A rental serves every queued request it can, and its cost is charged to the
// one that opened it. Without this marker the other rows read as free.
func TestHistory_TellsARiderFromAnOpener(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		opener := ledger(t, s, "runpod", "run-a", "a100-80g", day(1), 1_790_000, 1, 380_000)
		rider := ledger(t, s, "runpod", "run-a", "a100-80g", day(1).Add(time.Minute), 0, 1, 380_000)

		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		got := map[ReqID]bool{}
		for _, e := range rows {
			got[e.ID] = e.Rode
		}
		if !got[rider] {
			t.Error("the request that joined a rental already up is not marked as a rider")
		}
		if got[opener] {
			t.Error("the request that paid for the rental is marked as a rider")
		}

		// And summing the column still counts the rental once, which is the
		// property the marker exists to keep readable.
		var total node.MicroUSD
		for _, e := range rows {
			total += e.Cost
		}
		if total != 1_790_000 {
			t.Errorf("the rentals sum to %v, want one rental at $1.79", total)
		}
	})
}

// A free rental is the local provider's, and its cost is legitimately zero --
// so "somebody else paid" cannot be decided on cost alone.
func TestHistory_DoesNotCallEveryRequestOnAFreeRentalARider(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), Request{
			Provider: "local", TTL: time.Hour, Created: day(1),
			Jobs: []Job{{Prompt: "a cat"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(t.Context(), id, func(g *Request) {
			g.Status = StatusDone
			g.RunID = "run-free"
			g.Started, g.Ended = day(1), day(1).Add(time.Minute)
			// No cost, because the local provider charges nothing -- but a
			// rental that ran was billable for its own elapsed time.
			g.Elapsed, g.Billable = time.Minute, time.Minute
		}); err != nil {
			t.Fatal(err)
		}
		rider := ledger(t, s, "local", "run-free", "", day(1).Add(2*time.Minute), 0, 1, 0)

		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range rows {
			switch e.ID {
			case id:
				if e.Rode {
					t.Error("the request that opened the free rental is marked as a rider")
				}
			case rider:
				if !e.Rode {
					t.Error("a request on a free rental somebody else opened is not marked as a rider")
				}
			}
		}
	})
}

// Finished is the precondition. A queued request contributes no figures for
// the same arithmetic reason and is missing nothing.
func TestHistory_OnlyCallsAFinishedRequestUnrecorded(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		queued, err := s.Enqueue(t.Context(), Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: day(1),
			Jobs: []Job{{Prompt: "a cat"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		old, err := s.Enqueue(t.Context(), Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: day(2),
			Jobs: []Job{{Prompt: "a dog"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Finished, charged for, and not one job ever reported: a request
		// from before schema version 3.
		if err := s.Update(t.Context(), old, func(g *Request) {
			g.Status = StatusDone
			g.RunID, g.Cost = "run-old", 890_000
			g.Started, g.Ended = day(2), day(2).Add(time.Hour)
		}); err != nil {
			t.Fatal(err)
		}

		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range rows {
			switch e.ID {
			case queued:
				if e.Unrecorded {
					t.Error("a queued request is reported as missing figures it never had")
				}
			case old:
				if !e.Unrecorded {
					t.Error("a finished request whose jobs never reported is not marked unrecorded")
				}
			}
		}

		// The aggregate counts the same one and not the other.
		buckets, err := s.Spend(t.Context(), SpendQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(buckets) != 1 {
			t.Fatalf("Spend returned %d buckets, want 1", len(buckets))
		}
		if buckets[0].Unrecorded != 1 {
			t.Errorf("the aggregate counts %d unrecorded requests, want 1 (the finished one)",
				buckets[0].Unrecorded)
		}
	})
}

func TestHistory_FiltersAndOrders(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ledger(t, s, "runpod", "run-a", "a100-80g", day(1), 1_790_000, 1, 380_000)
		ledger(t, s, "runpod", "run-b", "rtx4090", day(5), 890_000, 1, 95_000)
		if _, err := s.Enqueue(t.Context(), Request{
			Provider: "local", TTL: time.Hour, Created: day(9),
			Jobs: []Job{{Prompt: "a cat"}},
		}); err != nil {
			t.Fatal(err)
		}

		cases := []struct {
			name string
			f    Filter
			want []string // the GPU of each row, newest first
		}{
			{"everything", Filter{}, []string{"", "rtx4090", "a100-80g"}},
			{"by card", Filter{GPU: "rtx4090"}, []string{"rtx4090"}},
			{"by provider", Filter{Provider: "runpod"}, []string{"rtx4090", "a100-80g"}},
			{"by status", Filter{Status: StatusQueued}, []string{""}},
			// Half-open: day 1 is in, day 5 is out.
			{"by period", Filter{Since: day(1), Until: day(5)}, []string{"a100-80g"}},
			{"since only", Filter{Since: day(5)}, []string{"", "rtx4090"}},
			{"until only", Filter{Until: day(5)}, []string{"a100-80g"}},
			{"bounded", Filter{Limit: 1}, []string{""}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				rows, err := s.History(t.Context(), c.f)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != len(c.want) {
					t.Fatalf("History(%+v) returned %d rows, want %d", c.f, len(rows), len(c.want))
				}
				for i, want := range c.want {
					if rows[i].GPU != want {
						t.Errorf("row %d is %q, want %q (newest first)", i, rows[i].GPU, want)
					}
				}
			})
		}
	})
}

func TestHistory_RefusesAStatusItDoesNotWrite(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if _, err := s.History(t.Context(), Filter{Status: "finished"}); err == nil {
			t.Fatal("History accepted a status ember does not write")
		}
	})
}

// A ledger row carries no prompts, and that is deliberate: reading them would
// be a query per row for text the table never prints. Asserted so that a
// later convenience cannot quietly reintroduce the N+1.
func TestHistory_DoesNotReadTheJobsOfEachRequest(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ledger(t, s, "runpod", "run-a", "a100-80g", day(1), 1_790_000, 2, 380_000)

		calls := 0
		if rec, ok := s.(*RecordingStore); ok {
			calls = len(rec.Calls)
		}
		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Jobs != 2 {
			t.Fatalf("History returned %d rows with %d jobs, want 1 row of 2", len(rows), rows[0].Jobs)
		}
		if rec, ok := s.(*RecordingStore); ok && len(rec.Calls)-calls != 1 {
			t.Errorf("History made %d store calls, want 1: %v",
				len(rec.Calls)-calls, rec.Calls[calls:])
		}
	})
}

// A cancelled request has an end and no start, and that is the reachable
// state Wall's guard is about.
//
// Cancelling is only allowed on a QUEUED request -- cancelling one that is
// generating would mean destroying the node and taking every other request on
// the rental with it -- so the API writes Ended over a row whose Started was
// never set. Subtracting the zero time there is two thousand years, in a
// column of minutes.
func TestHistory_HasNoWallTimeForARequestThatNeverStarted(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: day(1),
			Jobs: []Job{{Prompt: "a cat"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(t.Context(), id, func(g *Request) {
			g.Status = StatusCancelled
			g.Ended = day(1).Add(time.Minute)
		}); err != nil {
			t.Fatal(err)
		}

		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("History returned %d rows, want 1", len(rows))
		}
		if got := rows[0].Wall(); got != 0 {
			t.Errorf("wall = %v for a request that never started, want zero", got)
		}
		if !rows[0].Started.IsZero() {
			t.Errorf("Started = %v, want the zero time rather than the epoch", rows[0].Started)
		}
	})
}
