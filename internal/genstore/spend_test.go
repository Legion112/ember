package genstore

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// ran records one finished request with its jobs' execution facts, the way the
// queue's recorder does: the request row, then each job.
//
// Through the Store interface and not by writing SQL, so the body runs against
// both implementations -- which is what stops the fake being kinder than the
// thing it stands in for. A fake that aggregated by delegating to the SQL
// could not disagree with it, and the SQL is the half most likely to be
// subtly wrong.
func ran(t *testing.T, s Store, r Request, jobs []JobRun) ReqID {
	t.Helper()
	id, err := s.Enqueue(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(t.Context(), id, func(g *Request) {
		g.Status = StatusDone
		g.Offer, g.RunID, g.Node = r.Offer, r.RunID, r.Node
		g.Started, g.Ended = r.Started, r.Ended
		g.Cost, g.Rate = r.Cost, r.Rate
		g.Elapsed, g.Billable = r.Elapsed, r.Billable
		g.LoadMeasured, g.LoadReported = r.LoadMeasured, r.LoadReported
		// Through Update and not through Enqueue, because that is the only
		// way it can be written: a request does not ask for a model, so the
		// INSERT does not name the column and the fake forgets it too.
		g.Model = r.Model
	}); err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if err := s.UpdateJob(t.Context(), id, j.Idx, func(g *JobRun) {
			g.Started, g.Ended = j.Started, j.Ended
			g.Cost, g.Images, g.Bytes = j.Cost, j.Images, j.Bytes
			g.Passes, g.Steps = j.Passes, j.Steps
		}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// day is a fixed instant so a ByDay bucket's key is predictable.
func day(n int) time.Time {
	return time.Date(2026, 11, n, 12, 0, 0, 0, time.UTC)
}

// rental builds a finished request on one card, with its own cost.
func rental(gpu string, created time.Time, cost node.MicroUSD, billable time.Duration, prompts ...string) Request {
	r := req(prompts...)
	r.GPU = gpu
	r.Created = created
	r.Cost, r.Billable = cost, billable
	r.Rate = node.Rate(2_490_000)
	// A finished rental has a start and an end. Left off, these rows read as
	// never having finished -- which is what "contributes no per-job figures"
	// is now conditioned on, and the fixture has to be the shape the queue
	// actually writes.
	r.Started, r.Ended = created, created.Add(billable)
	return r
}

// job is one job's execution facts.
func job(idx int, cost node.MicroUSD, images int, steps int64, took time.Duration) JobRun {
	start := day(1)
	return JobRun{
		Idx: idx, Started: start, Ended: start.Add(took),
		Cost: cost, Images: images, Bytes: int64(images) * 4096,
		Passes: 1, Steps: steps,
	}
}

func TestSpend_SumsOneRequestsJobsAndItsRental(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ran(t, s, rental("rtx4090", day(1), 1_200_000, 31*time.Minute, "a", "b"),
			[]JobRun{job(1, 400_000, 1, 20, 140*time.Second), job(2, 420_000, 2, 20, 141*time.Second)})

		got, err := s.Spend(t.Context(), SpendQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("Spend = %+v, want one total row", got)
		}
		b := got[0]
		if b.Key != "" {
			t.Errorf("an ungrouped total came back keyed %q", b.Key)
		}
		if b.Requests != 1 || b.Jobs != 2 || b.Images != 3 {
			t.Errorf("requests/jobs/images = %d/%d/%d, want 1/2/3", b.Requests, b.Jobs, b.Images)
		}
		// The rental's own cost, counted once -- not once per job. A plain
		// join between requests and jobs would have multiplied it by two
		// here, which is the mistake the job-side subquery exists to prevent
		// and the kind that looks plausible in a figure nobody has a second
		// copy of.
		if b.RunCost != 1_200_000 {
			t.Errorf("RunCost = %v, want the rental's 1200000 counted once", b.RunCost)
		}
		if b.JobCost != 820_000 {
			t.Errorf("JobCost = %v, want the two jobs' card time, 820000", b.JobCost)
		}
		if b.Steps != 40 {
			t.Errorf("Steps = %d, want 40", b.Steps)
		}
		if b.Sampling != 281*time.Second {
			t.Errorf("Sampling = %s, want 281s", b.Sampling)
		}
		if b.Billable != 31*time.Minute {
			t.Errorf("Billable = %s", b.Billable)
		}
		if b.Unrecorded != 0 {
			t.Errorf("Unrecorded = %d, want 0 for a request with job rows", b.Unrecorded)
		}
	})
}

// TestSpend_CountsARentalOnceAcrossTheRequestsItServed is the shape the whole
// served path exists for, and the one an aggregate is most likely to get
// wrong.
//
// One rental serves several requests, and only the request that OPENED it
// carries the cost -- which is how lifecycle writes it. So a period total is
// the sum over openers, and the requests that rode along contribute their own
// card time and no rental cost.
func TestSpend_CountsARentalOnceAcrossTheRequestsItServed(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		opener := rental("rtx4090", day(1), 1_200_000, 31*time.Minute, "a")
		opener.RunID = "run1"
		ran(t, s, opener, []JobRun{job(1, 400_000, 1, 20, 140*time.Second)})

		// Rode along on the same rental: same run id, no cost of its own.
		along := rental("rtx4090", day(1), 0, 0, "b")
		along.RunID = "run1"
		ran(t, s, along, []JobRun{job(1, 420_000, 1, 20, 141*time.Second)})

		got, err := s.Spend(t.Context(), SpendQuery{})
		if err != nil || len(got) != 1 {
			t.Fatalf("Spend = %+v, %v", got, err)
		}
		b := got[0]
		if b.Requests != 2 {
			t.Errorf("Requests = %d, want 2", b.Requests)
		}
		if b.RunCost != 1_200_000 {
			t.Errorf("RunCost = %v, want one rental's cost and not two", b.RunCost)
		}
		if b.JobCost != 820_000 {
			t.Errorf("JobCost = %v, want both requests' card time", b.JobCost)
		}
		// And the gap between them is the per-rental overhead this shape
		// pays once: provisioning, the image pull, the store read, the model
		// load.
		if b.Overhead() != 380_000 {
			t.Errorf("Overhead = %v, want 380000", b.Overhead())
		}
	})
}

func TestSpend_GroupsByCardProviderAndDay(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ran(t, s, rental("rtx4090", day(1), 1_000_000, time.Hour, "a"),
			[]JobRun{job(1, 400_000, 1, 20, time.Minute)})
		ran(t, s, rental("a100-80g", day(2), 3_000_000, time.Hour, "b"),
			[]JobRun{job(1, 900_000, 2, 28, 2*time.Minute)})

		byGPU, err := s.Spend(t.Context(), SpendQuery{By: ByGPU})
		if err != nil {
			t.Fatal(err)
		}
		if len(byGPU) != 2 {
			t.Fatalf("by gpu = %+v, want two rows", byGPU)
		}
		// Sorted by key, so two runs of the same report print the same way.
		if byGPU[0].Key != "a100-80g" || byGPU[1].Key != "rtx4090" {
			t.Errorf("keys = %q, %q; want key order", byGPU[0].Key, byGPU[1].Key)
		}
		if byGPU[0].RunCost != 3_000_000 || byGPU[1].RunCost != 1_000_000 {
			t.Errorf("costs = %v, %v", byGPU[0].RunCost, byGPU[1].RunCost)
		}
		if byGPU[0].Images != 2 {
			t.Errorf("the a100 row has %d images", byGPU[0].Images)
		}

		byDay, err := s.Spend(t.Context(), SpendQuery{By: ByDay})
		if err != nil {
			t.Fatal(err)
		}
		if len(byDay) != 2 {
			t.Fatalf("by day = %+v, want two rows", byDay)
		}
		// UTC, and the key says which day it is: a boundary in local time
		// puts the same spend in different buckets depending on where the
		// operator was sitting.
		if byDay[0].Key != "2026-11-01" || byDay[1].Key != "2026-11-02" {
			t.Errorf("day keys = %q, %q", byDay[0].Key, byDay[1].Key)
		}

		byProvider, err := s.Spend(t.Context(), SpendQuery{By: ByProvider})
		if err != nil {
			t.Fatal(err)
		}
		if len(byProvider) != 1 || byProvider[0].Key != "local" {
			t.Fatalf("by provider = %+v", byProvider)
		}
		if byProvider[0].RunCost != 4_000_000 {
			t.Errorf("provider total = %v, want both rentals", byProvider[0].RunCost)
		}
	})
}

// TestSpend_PeriodIsHalfOpen so consecutive periods tile: a request on the
// boundary belongs to exactly one of them, and a month-by-month report does
// not double-count a day.
func TestSpend_PeriodIsHalfOpen(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		for _, d := range []int{1, 2, 3} {
			ran(t, s, rental("rtx4090", day(d), 1_000_000, time.Hour, "p"),
				[]JobRun{job(1, 100_000, 1, 4, time.Minute)})
		}

		got, err := s.Spend(t.Context(), SpendQuery{Since: day(2), Until: day(3)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("Spend = %+v", got)
		}
		if got[0].Requests != 1 {
			t.Errorf("Requests = %d, want only the one on the 2nd: Since includes and Until excludes",
				got[0].Requests)
		}

		// Unbounded at one end is what "everything since" asks for.
		since, err := s.Spend(t.Context(), SpendQuery{Since: day(2)})
		if err != nil || len(since) != 1 || since[0].Requests != 2 {
			t.Fatalf("Spend since the 2nd = %+v, %v", since, err)
		}
		until, err := s.Spend(t.Context(), SpendQuery{Until: day(2)})
		if err != nil || len(until) != 1 || until[0].Requests != 1 {
			t.Fatalf("Spend before the 2nd = %+v, %v", until, err)
		}
	})
}

// TestSpend_CountsWhatItHadToLeaveOut.
//
// A request that ran before these columns existed has no job rows to sum, and
// a total that silently omits it is the clean-bill-of-health failure
// `ember ls --all` exits non-zero to avoid, in a column of money. Nothing
// backfills, so saying how many were skipped is the honest alternative.
func TestSpend_CountsWhatItHadToLeaveOut(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		// Recorded.
		ran(t, s, rental("rtx4090", day(1), 1_000_000, time.Hour, "a"),
			[]JobRun{job(1, 400_000, 1, 20, time.Minute)})
		// Not: a request with jobs and no execution facts, which is exactly
		// what a version-2 row reads as.
		ran(t, s, rental("rtx4090", day(1), 1_000_000, time.Hour, "b"), nil)

		got, err := s.Spend(t.Context(), SpendQuery{})
		if err != nil || len(got) != 1 {
			t.Fatalf("Spend = %+v, %v", got, err)
		}
		b := got[0]
		if b.Requests != 2 {
			t.Errorf("Requests = %d, want both", b.Requests)
		}
		if b.Unrecorded != 1 {
			t.Errorf("Unrecorded = %d, want 1", b.Unrecorded)
		}
		// Its rental cost still counts: that figure was promoted to a column
		// before this change and is on the row.
		if b.RunCost != 2_000_000 {
			t.Errorf("RunCost = %v, want both rentals", b.RunCost)
		}
		// Its jobs count as jobs and contribute no card time, which is the
		// shape of the gap rather than a pretence that it is not there.
		if b.Jobs != 2 || b.JobCost != 400_000 {
			t.Errorf("jobs/jobCost = %d/%v", b.Jobs, b.JobCost)
		}
	})
}

func TestSpend_RefusesAGroupingItDoesNotImplement(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		// A grouping reaches a SQL GROUP BY, so the set is closed and checked
		// rather than passed through. This is the one place a caller's string
		// could have become part of a statement.
		_, err := s.Spend(t.Context(), SpendQuery{By: GroupBy("provider; DROP TABLE requests")})
		if err == nil {
			t.Fatal("an unknown grouping was accepted")
		}
		// A near miss as well as an injection, because the plausible typo is
		// what a closed set is for: "models" is not "model", and the answer
		// has to be a refusal rather than one row keyed on nothing.
		if _, err := s.Spend(t.Context(), SpendQuery{By: GroupBy("models")}); err == nil {
			t.Fatal("a grouping that is not in the set was accepted")
		}

		// And everything the set DOES name is accepted by both stores. A
		// grouping listed in GroupBys that Spend refuses would print in the
		// flag help and fail when typed, which is the disagreement GroupBys
		// exists to make impossible.
		for _, g := range GroupBys() {
			if _, err := s.Spend(t.Context(), SpendQuery{By: g}); err != nil {
				t.Errorf("Spend refused %q, which GroupBys offers: %v", g, err)
			}
		}
	})
}

func TestSpend_EmptyStoreIsNoRowsRatherThanAZeroRow(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		got, err := s.Spend(t.Context(), SpendQuery{})
		if err != nil {
			t.Fatal(err)
		}
		// No rows, not one row of zeroes: "ember has spent nothing" is for
		// the caller to say, and a zero row would print as a period that was
		// measured and came to nothing.
		if len(got) != 0 {
			t.Errorf("Spend on an empty store = %+v, want no rows", got)
		}
	})
}

func TestUpdateJob_RefusesAnIndexThatIsNotAJob(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("only"))
		if err != nil {
			t.Fatal(err)
		}
		for _, idx := range []int{0, 2, -1} {
			err := s.UpdateJob(t.Context(), id, idx, func(*JobRun) {})
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateJob(idx %d) = %v, want ErrNotFound", idx, err)
			}
		}
		if err := s.UpdateJob(t.Context(), "nosuchrequest", 1, func(*JobRun) {}); !errors.Is(err, ErrNotFound) {
			t.Errorf("UpdateJob on an absent request = %v, want ErrNotFound", err)
		}
		// JobRuns is a "list the children of" read, like Images and Events:
		// an absent request is no rows and no error, in both stores. The
		// first version of the fake returned ErrNotFound here and the
		// conformance test caught it -- a fake stricter than the real store
		// is the usual danger inverted, and it would have grown a caller a
		// branch for an error nothing returns.
		runs, err := s.JobRuns(t.Context(), "nosuchrequest")
		if err != nil || len(runs) != 0 {
			t.Errorf("JobRuns on an absent request = %+v, %v; want nothing and no error", runs, err)
		}
	})
}

func TestUpdateJob_MergesWhatArrivesSeparately(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("one"))
		if err != nil {
			t.Fatal(err)
		}
		start := day(1)

		// The facts arrive on four different events, which is why this is
		// read-modify-write and not a setter per fact: a setter would be four
		// statements that each have to remember not to clobber the others.
		if err := s.UpdateJob(t.Context(), id, 1, func(j *JobRun) { j.Started = start }); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateJob(t.Context(), id, 1, func(j *JobRun) { j.Images, j.Bytes = 2, 8192 }); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateJob(t.Context(), id, 1, func(j *JobRun) { j.Passes, j.Steps = 2, 28 }); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateJob(t.Context(), id, 1, func(j *JobRun) {
			j.Ended, j.Cost = start.Add(90*time.Second), 31_000
		}); err != nil {
			t.Fatal(err)
		}

		runs, err := s.JobRuns(t.Context(), id)
		if err != nil || len(runs) != 1 {
			t.Fatalf("JobRuns = %+v, %v", runs, err)
		}
		got := runs[0]
		if got.Req != id || got.Idx != 1 {
			t.Errorf("identity = %s/%d", got.Req, got.Idx)
		}
		if !got.Started.Equal(start) || got.Images != 2 || got.Bytes != 8192 ||
			got.Passes != 2 || got.Steps != 28 || got.Cost != 31_000 {
			t.Errorf("a later write clobbered an earlier one: %+v", got)
		}
		if got.Sampling() != 90*time.Second {
			t.Errorf("Sampling = %s, want 90s", got.Sampling())
		}
	})
}

func TestBucket_DerivedFiguresAreComputedNotStored(t *testing.T) {
	b := Bucket{
		Requests: 1, Jobs: 8, Images: 8,
		RunCost: 2_000_000, JobCost: 1_600_000,
		Steps: 224, Sampling: 1120 * time.Second, Billable: 1400 * time.Second,
	}
	if got := b.PerImage(); got != 250_000 {
		t.Errorf("PerImage = %v, want 250000", got)
	}
	if got := b.Overhead(); got != 400_000 {
		t.Errorf("Overhead = %v, want 400000", got)
	}
	if got := b.StepsPerSecond(); got != 0.2 {
		t.Errorf("StepsPerSecond = %v, want 0.2", got)
	}
	if got := b.SamplingShare(); got != 0.8 {
		t.Errorf("SamplingShare = %v, want 0.8", got)
	}

	// Zero in, zero out, never a division by nothing.
	var z Bucket
	if z.PerImage() != 0 || z.Overhead() != 0 || z.StepsPerSecond() != 0 || z.SamplingShare() != 0 {
		t.Errorf("the zero bucket derived something: %+v", z)
	}

	// A rental whose cost event never arrived has jobs that ran and no run
	// cost. Reporting that as a negative overhead would read as a refund.
	odd := Bucket{RunCost: 0, JobCost: 500_000}
	if got := odd.Overhead(); got != 0 {
		t.Errorf("Overhead with no rental cost = %v, want 0 rather than a negative", got)
	}
}

// TestOpen_MigratesAVersionTwoFile.
//
// Built by hand as a version-2 file rather than by checking out the old code,
// for the reason the version-1 test gives: what has to be proved is that
// TODAY'S ember reads YESTERDAY'S file. The first migration existed because
// `CREATE TABLE IF NOT EXISTS` is a no-op against a table that already
// exists, so a column added to the schema is absent from every file written
// before it -- and the failure arrives as `table jobs has no column named
// cost` the next time an operator queues a request, with their history in a
// file ember can no longer write to.
func TestOpen_MigratesAVersionTwoFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The version-2 schema, as it was: requests and jobs without any of the
	// execution columns. Only the tables this test touches.
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta (key, value) VALUES ('schema_version', '2')`,
		`CREATE TABLE requests (
			id TEXT PRIMARY KEY, created_unix INTEGER NOT NULL, status TEXT NOT NULL,
			provider TEXT NOT NULL, gpu TEXT NOT NULL DEFAULT '', offer TEXT NOT NULL DEFAULT '',
			ttl_ns INTEGER NOT NULL, max_hourly INTEGER NOT NULL DEFAULT 0,
			max_run INTEGER NOT NULL DEFAULT 0, preview TEXT NOT NULL DEFAULT '',
			preview_every INTEGER NOT NULL DEFAULT 0, run_id TEXT NOT NULL DEFAULT '',
			node TEXT NOT NULL DEFAULT '', started_unix INTEGER NOT NULL DEFAULT 0,
			ended_unix INTEGER NOT NULL DEFAULT 0, cost INTEGER NOT NULL DEFAULT 0,
			err TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE jobs (
			req TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
			idx INTEGER NOT NULL, prompt TEXT NOT NULL DEFAULT '',
			negative TEXT NOT NULL DEFAULT '', width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0, steps INTEGER NOT NULL DEFAULT 0,
			batch_count INTEGER NOT NULL DEFAULT 0, seed INTEGER NOT NULL DEFAULT 0,
			sampler TEXT NOT NULL DEFAULT '', scheduler TEXT NOT NULL DEFAULT '',
			cfg REAL NOT NULL DEFAULT 0, guidance REAL NOT NULL DEFAULT 0,
			output TEXT NOT NULL DEFAULT '', params TEXT NOT NULL DEFAULT '{}',
			PRIMARY KEY (req, idx)
		)`,
		`CREATE TABLE events (
			req TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL,
			PRIMARY KEY (req, seq)
		)`,
		`CREATE TABLE images (
			req TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
			job INTEGER NOT NULL, idx INTEGER NOT NULL, preview INTEGER NOT NULL DEFAULT 0,
			key TEXT NOT NULL, bytes INTEGER NOT NULL DEFAULT 0, at_unix INTEGER NOT NULL,
			PRIMARY KEY (req, job, idx, preview)
		)`,
		`INSERT INTO requests (id, created_unix, status, provider, gpu, ttl_ns, cost)
		 VALUES ('old1', 1762000000000, 'done', 'runpod', 'rtx4090', 3600000000000, 1500000)`,
		`INSERT INTO jobs (req, idx, prompt, steps) VALUES ('old1', 1, 'a cat', 20)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed a version-2 file: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a version-2 file was refused: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The old request is still readable, and its pre-existing facts survived.
	got, err := s.Get(t.Context(), "old1")
	if err != nil {
		t.Fatalf("the migrated request could not be read: %v", err)
	}
	if got.Cost != 1_500_000 || got.GPU != "rtx4090" || len(got.Jobs) != 1 {
		t.Errorf("the migration changed what was there: %+v", got)
	}
	// And the new columns read as zero rather than failing, which is what
	// "nothing backfills" means in practice.
	if got.Billable != 0 || got.LoadMeasured != 0 {
		t.Errorf("a version-2 row invented execution facts: %+v", got)
	}
	// Two migrations now run over this file rather than one, so it also
	// proves the chain rather than a single step: the memory columns came
	// later still and must read as zero for the same reason.
	if got.MemCgroup != 0 || got.MemLimit != 0 || got.AssetBytes != 0 {
		t.Errorf("a version-2 row invented memory facts: %+v", got)
	}

	// Writing works, which is the thing the missing-column failure broke.
	if err := s.UpdateJob(t.Context(), "old1", 1, func(j *JobRun) { j.Cost = 7 }); err != nil {
		t.Fatalf("could not write a job row on a migrated file: %v", err)
	}

	// And the aggregate counts it as a request it could not measure.
	sp, err := s.Spend(t.Context(), SpendQuery{})
	if err != nil || len(sp) != 1 {
		t.Fatalf("Spend = %+v, %v", sp, err)
	}
	if sp[0].RunCost != 1_500_000 {
		t.Errorf("the migrated rental's cost = %v", sp[0].RunCost)
	}
}
