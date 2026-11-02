package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
)

// ledgerRow records one request, with everything the ledger prints about it.
//
// Seeded through the store's own interface rather than by running the queue,
// as seedSpend's rows are and for the same reason: what is under test is the
// reader, and producing these rows for real would mean renting something.
func ledgerRow(t *testing.T, st genstore.Store, runID, gpu string, created time.Time,
	cost node.MicroUSD, jobs int, each node.MicroUSD) genstore.ReqID {
	t.Helper()
	r := genstore.Request{Provider: "runpod", GPU: gpu, TTL: time.Hour, Created: created}
	for range jobs {
		r.Jobs = append(r.Jobs, genstore.Job{Prompt: "a cat", Width: 1024, Height: 1024, Steps: 28})
	}
	id, err := st.Enqueue(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(t.Context(), id, func(g *genstore.Request) {
		g.Status = genstore.StatusDone
		g.RunID, g.Node = runID, runID+"-node"
		g.Started, g.Ended = created, created.Add(10*time.Minute)
		g.Cost = cost
		if cost > 0 {
			// Billable travels with the cost because only the request that
			// opened a rental is given either.
			g.Rate, g.Elapsed, g.Billable = 1_790_000, 10*time.Minute, 10*time.Minute
		}
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= jobs; i++ {
		if err := st.UpdateJob(t.Context(), id, i, func(j *genstore.JobRun) {
			j.Started, j.Ended = created, created.Add(2*time.Minute)
			j.Cost, j.Images, j.Bytes = each, 1, 1<<20
			j.Passes, j.Steps = 1, 28
		}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestHistory_ListsARequestPerRow(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		ledgerRow(t, st, "run-a", "a100-80g", spendDay(1), 1_790_000, 3, 380_000)
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history:\n%s", got)

	for _, want := range []string{
		"when (utc)", "request", "status", "provider", "card",
		"jobs", "images", "wall", "rental", "card time",
		"2026-11-01 12:00", "done", "runpod", "a100-80g",
		"$1.79", // the rental
		"$1.14", // three jobs at $0.38 of card time
		"10m0s", // the request's own wall time
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ledger does not report %q:\n%s", want, got)
		}
	}
}

// A rider's rental cost is zero by design, and a money column of zeros that
// means "charged to another row" is the misreading this marker exists to
// prevent.
func TestHistory_MarksARequestThatRodeAlongOnAnothersRental(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		ledgerRow(t, st, "run-a", "a100-80g", spendDay(1), 1_790_000, 2, 380_000)
		// Same rental, no cost of its own: this is what joining a rental that
		// is already up looks like in the database.
		ledgerRow(t, st, "run-a", "a100-80g", spendDay(1).Add(time.Minute), 0, 2, 380_000)
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history with a rider:\n%s", got)

	if !strings.Contains(got, "rode along") {
		t.Errorf("a request that rode along on another's rental is not named:\n%s", got)
	}
	// One rental, charged once. Summing the column must not double it, and
	// must not report the rider as free either.
	if !strings.Contains(got, "$1.79 of rental") {
		t.Errorf("the rental is not counted exactly once:\n%s", got)
	}

	var rows []genstore.Entry
	var js bytes.Buffer
	if err := runHistory([]string{"-config", cfg, "-json"}, &js, d); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(js.Bytes(), &rows); err != nil {
		t.Fatalf("history --json is not JSON: %v\n%s", err, js.String())
	}
	if len(rows) != 2 {
		t.Fatalf("history --json has %d rows, want 2:\n%s", len(rows), js.String())
	}
	// Newest first, so the rider is the first row.
	if !rows[0].Rode {
		t.Errorf("the rider is not marked as having rode along: %+v", rows[0])
	}
	if rows[1].Rode {
		t.Errorf("the request that paid for the rental is marked as a rider: %+v", rows[1])
	}
}

// A request that finished with no per-job figures is every request from before
// schema version 3. Zero images is a claim it produced nothing.
func TestHistory_DashesTheColumnsNothingRecorded(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		r := genstore.Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: spendDay(1),
			Jobs: []genstore.Job{{Prompt: "a cat"}, {Prompt: "a dog"}},
		}
		id, err := st.Enqueue(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		// Finished, with a rental cost, and not one job ever reported.
		if err := st.Update(t.Context(), id, func(g *genstore.Request) {
			g.Status = genstore.StatusDone
			g.RunID, g.Cost = "run-old", 890_000
			g.Started, g.Ended = spendDay(1), spendDay(1).Add(time.Hour)
		}); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history over an unrecorded run:\n%s", got)

	if !strings.Contains(got, "finished with no per-job figures") {
		t.Errorf("a request with no per-job figures is not named:\n%s", got)
	}
	if strings.Contains(got, " 0 image") {
		t.Errorf("an unrecorded request reports zero images, which claims it produced nothing:\n%s", got)
	}
}

// A queued request has nothing missing: it has not run.
func TestHistory_DoesNotCallAQueuedRequestUnrecorded(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		if _, err := st.Enqueue(t.Context(), genstore.Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: spendDay(1),
			Jobs: []genstore.Job{{Prompt: "a cat"}},
		}); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history = %v\n%s", err, out.String())
	}
	got := out.String()
	if strings.Contains(got, "finished with no per-job figures") {
		t.Errorf("a queued request is reported as missing figures it never had:\n%s", got)
	}
	if !strings.Contains(got, "queued") {
		t.Errorf("the queued request is not listed at all:\n%s", got)
	}

	// Its wall time is a dash and not a duration measured from the zero
	// time, which would be two thousand years in a column of minutes.
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "queued") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 10 {
			t.Fatalf("the queued row has %d columns, want 10: %q", len(f), line)
		}
		if f[8] != "-" {
			t.Errorf("the queued request's wall time is %q, want a dash: %q", f[8], line)
		}
	}

	// And the aggregate must not count it either, which is the same defect
	// one layer up.
	var spend bytes.Buffer
	if err := runSpend([]string{"-config", cfg}, &spend, d); err != nil {
		t.Fatalf("spend = %v\n%s", err, spend.String())
	}
	if strings.Contains(spend.String(), "no per-job figures") {
		t.Errorf("ember spend counts a queued request as history it is missing:\n%s", spend.String())
	}
}

func TestHistory_FiltersByProviderCardStatusAndPeriod(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		ledgerRow(t, st, "run-a", "a100-80g", spendDay(1), 1_790_000, 1, 380_000)
		ledgerRow(t, st, "run-b", "rtx4090", spendDay(5), 890_000, 1, 95_000)
	})

	cases := []struct {
		name  string
		args  []string
		want  string
		avoid string
	}{
		{"by card", []string{"-gpu", "rtx4090"}, "rtx4090", "a100-80g"},
		{"by provider", []string{"-provider", "runpod"}, "rtx4090", ""},
		{"by another provider", []string{"-provider", "local"}, "no generations recorded on local", "runpod"},
		{"by status", []string{"-status", "queued"}, "no generations recorded in status queued", "runpod"},
		// Half-open, as `ember spend` is: --until excludes its own day.
		{"by period", []string{"-since", "2026-11-01", "-until", "2026-11-05"}, "a100-80g", "rtx4090"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runHistory(append([]string{"-config", cfg}, c.args...), &out, d); err != nil {
				t.Fatalf("history = %v\n%s", err, out.String())
			}
			got := out.String()
			if !strings.Contains(got, c.want) {
				t.Errorf("history %v does not report %q:\n%s", c.args, c.want, got)
			}
			if c.avoid != "" && strings.Contains(got, c.avoid) {
				t.Errorf("history %v reports %q, which it filtered out:\n%s", c.args, c.avoid, got)
			}
		})
	}
}

func TestHistory_RefusesABadFlag(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"an unknown status", []string{"-status", "finished"}, "not a status ember writes"},
		{"a backwards period", []string{"-since", "2026-11-05", "-until", "2026-11-01"}, "period is empty"},
		{"a negative limit", []string{"-limit", "-1"}, "is negative"},
		{"an argument", []string{"yesterday"}, "takes no arguments"},
		{"a date that is not one", []string{"-since", "last tuesday"}, "wants YYYY-MM-DD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runHistory(append([]string{"-config", cfg}, c.args...), &out, d)
			if err == nil {
				t.Fatalf("history %v was accepted:\n%s", c.args, out.String())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("history %v = %q, want it to mention %q", c.args, err, c.want)
			}
			if ExitCode(err) != ExitUsage {
				t.Errorf("history %v exits %d, want %d for a usage error",
					c.args, ExitCode(err), ExitUsage)
			}
		})
	}
}

// THE LEDGER MUST NOT BE ABLE TO SPEND. #27 asks for this as an assertion and
// not as a comment: `ember history` reads a database that a queue drains, and
// a reader that advanced the queue would rent a card for somebody who typed a
// listing command.
func TestHistory_CannotAdvanceTheQueue(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		if _, err := st.Enqueue(t.Context(), genstore.Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: spendDay(1),
			Jobs: []genstore.Job{{Prompt: "a cat"}},
		}); err != nil {
			t.Fatal(err)
		}
	})

	for range 3 {
		var out bytes.Buffer
		if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
			t.Fatalf("history = %v\n%s", err, out.String())
		}
	}

	// Still claimable afterwards, which is the fact that matters: a reader
	// that had claimed it would leave the queue drained and the request
	// running against a rental nothing holds.
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		rs, err := st.List(t.Context(), genstore.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) != 1 {
			t.Fatalf("the database holds %d requests after three listings, want 1", len(rs))
		}
		if rs[0].Status != genstore.StatusQueued {
			t.Errorf("the request is %q after three listings, want it still queued", rs[0].Status)
		}
		r, ok, err := st.ClaimNext(t.Context(), "run-x", genstore.Compat{})
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("nothing is left to claim after three listings; the ledger drained the queue")
		}
		if r.Status != genstore.StatusRunning {
			t.Errorf("the claimed request is %q, want running", r.Status)
		}
	})
}

// A row that says "failed" and nothing else is the trip back to reading
// SQLite by hand that this command exists to end.
func TestHistory_NamesWhyARequestFailed(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id, err := st.Enqueue(t.Context(), genstore.Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: spendDay(1),
			Jobs: []genstore.Job{{Prompt: "a cat"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Update(t.Context(), id, func(g *genstore.Request) {
			g.Status = genstore.StatusFailed
			g.Started, g.Ended = spendDay(1), spendDay(1).Add(time.Minute)
			// Multi-line, as a node's own complaint arrives: the table must
			// not reflow around it.
			g.Err = "get sd version from file failed\nsd-server exited with status 1"
		}); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history over a failure:\n%s", got)

	if !strings.Contains(got, "get sd version from file failed") {
		t.Errorf("the reason the request failed is not reported:\n%s", got)
	}
	// Flattened onto one line, so the reason is one row of prose and not a
	// paragraph in the middle of a table.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Contains(line, "sd-server exited") &&
			!strings.Contains(line, "get sd version") {
			t.Errorf("a stored error was printed across lines:\n%s", got)
		}
	}
}

// The same state through the table, because the column is where an operator
// would see two thousand years.
func TestHistory_DashesTheWallTimeOfACancelledRequest(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id, err := st.Enqueue(t.Context(), genstore.Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: spendDay(1),
			Jobs: []genstore.Job{{Prompt: "a cat"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// What cancelling a queued request writes: an end, and no start.
		if err := st.Update(t.Context(), id, func(g *genstore.Request) {
			g.Status = genstore.StatusCancelled
			g.Ended = spendDay(1).Add(time.Minute)
		}); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history over a cancelled request:\n%s", got)

	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "cancelled") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 10 {
			t.Fatalf("the cancelled row has %d columns, want 10: %q", len(f), line)
		}
		if f[8] != "-" {
			t.Errorf("the cancelled request's wall time is %q, want a dash: %q", f[8], line)
		}
	}
}
