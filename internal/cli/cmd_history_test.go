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

// TestHistory_ShowsWhichModelServedARequest.
//
// The ledger's answer to "which run was it" is incomplete without it: a row
// with 56 GiB of weights and no model name says the size and not the thing,
// and a cost per image is a fact about a model on a card rather than about the
// card.
func TestHistory_ShowsWhichModelServedARequest(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id := ledgerRow(t, st, "run-a", "rtx-pro-6000", spendDay(1), 2_490_000, 2, 400_000)
		if err := st.Update(t.Context(), id, func(r *genstore.Request) {
			r.Model = "flux2-dev-Q8_0.gguf"
		}); err != nil {
			t.Fatal(err)
		}
		// And one from before the column, which is what a dash is for.
		ledgerRow(t, st, "run-b", "rtx4090", spendDay(2), 890_000, 1, 95_000)
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history with a model column:\n%s", got)

	if !strings.Contains(got, "model") {
		t.Errorf("the ledger has no model column:\n%s", got)
	}
	// Whole, not truncated: a name cut to a column width makes two
	// quantisations of one model read as the same model.
	if !strings.Contains(got, "flux2-dev-Q8_0.gguf") {
		t.Errorf("the model is not reported:\n%s", got)
	}
	// A DASH AND NOT A BLANK CELL for the request nobody read a model for,
	// the same rule the rental and card-time columns follow -- and the
	// footnote that says which two kinds of run those are, because a column
	// of dashes otherwise reads as ember having lost the names.
	line := rowFor(t, got, "rtx4090")
	if !strings.HasSuffix(strings.TrimRight(line, " "), "-") {
		t.Errorf("a request with no model does not end in a dash: %q", line)
	}
	if !strings.Contains(got, "have no model") {
		t.Errorf("the ledger does not say why a model is missing:\n%s", got)
	}

	// Through the wire type as well, where it is omitted rather than sent as
	// an empty string: `omitempty` and a dash are the same statement in two
	// spellings.
	var rows []genstore.Entry
	var js bytes.Buffer
	if err := runHistory([]string{"-config", cfg, "-json"}, &js, d); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(js.Bytes(), &rows); err != nil {
		t.Fatalf("history --json is not JSON: %v\n%s", err, js.String())
	}
	var seen int
	for _, e := range rows {
		switch e.GPU {
		case "rtx-pro-6000":
			seen++
			if e.Model != "flux2-dev-Q8_0.gguf" {
				t.Errorf("the JSON row does not carry the model: %+v", e)
			}
		case "rtx4090":
			seen++
			if e.Model != "" {
				t.Errorf("the JSON row invented a model: %+v", e)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("history --json has %d of the two rows:\n%s", seen, js.String())
	}
}

// rowFor is the one table line naming s, so an assertion about a cell is about
// that row rather than about the whole report.
func rowFor(t *testing.T, out, s string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	t.Fatalf("no row mentions %q:\n%s", s, out)
	return ""
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

// memRow seeds a finished request with what its node said about memory.
func memRow(t *testing.T, st genstore.Store, gpu string, created time.Time,
	cgroup int, limit, current, hostTotal, hostCached, assets int64) genstore.ReqID {
	t.Helper()
	id, err := st.Enqueue(t.Context(), genstore.Request{
		Provider: "runpod", GPU: gpu, TTL: time.Hour, Created: created,
		Jobs: []genstore.Job{{Prompt: "a cat"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(t.Context(), id, func(g *genstore.Request) {
		g.Status = genstore.StatusDone
		g.RunID, g.Cost = "run-"+gpu, 2_490_000
		g.Started, g.Ended = created, created.Add(42*time.Minute)
		g.MemCgroup = cgroup
		g.MemLimit, g.MemCurrent = limit, current
		g.MemTotal, g.MemCached = hostTotal, hostCached
		g.AssetBytes = assets
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestHistory_ReportsBothViewsOfTheNodesMemory.
//
// The two rows are the two arms of the FLUX.2 measurement: one node whose
// cgroup could hold the 56 GiB of weights and one whose could not, both
// reporting hundreds of gigabytes of host memory. That is the whole reason
// both views are recorded.
func TestHistory_ReportsBothViewsOfTheNodesMemory(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		memRow(t, st, "held", spendDay(1), 2, 64<<30, 58<<30, 700<<30, 56<<30, 56<<30)
		memRow(t, st, "spilled", spendDay(2), 2, 32<<30, 31<<30, 700<<30, 29<<30, 56<<30)
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg, "-memory"}, &out, d); err != nil {
		t.Fatalf("history --memory = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history --memory:\n%s", got)

	for _, want := range []string{
		"cgroup", "limit", "using", "host total", "weights",
		"v2", "64.0 GiB", "32.0 GiB", "700.0 GiB", "56.0 GiB",
		// The one derived figure, and the one the measurement turns on.
		"headroom",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("history --memory does not report %q:\n%s", want, got)
		}
	}
	// The node that could not hold the model has NEGATIVE headroom, which is
	// the interesting answer and must not be printed as a positive one.
	if !strings.Contains(got, "-24.0 GiB") {
		t.Errorf("a node that could not hold the weights does not report negative headroom:\n%s", got)
	}
	if !strings.Contains(got, "8.0 GiB") {
		t.Errorf("a node that could hold the weights does not report its headroom:\n%s", got)
	}

	// And the cost table is unchanged by the flag, because they are two
	// projections of the same rows rather than two listings.
	var costs bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &costs, d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(costs.String(), "host total") {
		t.Errorf("the default table grew a memory column:\n%s", costs.String())
	}
}

// A node that answered nothing is not a node with no limit, and a row of
// zeros that reads as "this node was fine" is the misreading to prevent.
func TestHistory_SaysWhenNoMemoryFactsWereRead(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		// A request from before these columns existed: no memory facts at all.
		memRow(t, st, "rtx4090", spendDay(1), 0, 0, 0, 0, 0, 0)
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg, "-memory"}, &out, d); err != nil {
		t.Fatalf("history --memory = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history --memory over an unread node:\n%s", got)

	if !strings.Contains(got, "before ember read the node's memory") {
		t.Errorf("a request with no memory facts is not named:\n%s", got)
	}
	if strings.Contains(got, "0 B") {
		t.Errorf("an unread figure is printed as zero bytes, which is a different claim:\n%s", got)
	}
	// Every cell a dash, headroom included: a figure computed over numbers
	// nobody read is the kind that gets believed.
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "rtx4090") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 11 {
			t.Fatalf("the row has %d columns, want 11: %q", len(f), line)
		}
		for _, cell := range f[5:] {
			if cell != "-" {
				t.Errorf("a figure nobody read printed as %q: %q", cell, line)
			}
		}
	}
}

// A node with no cgroup limit falls back to the host's total, which is the
// honest ceiling for a container that is not bounded.
func TestHistory_FallsBackToTheHostsTotalWithNoCgroupLimit(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		// cgroup v1 read, no limit set, so the host's total is the bound.
		memRow(t, st, "rtx4090", spendDay(1), 1, 0, 0, 64<<30, 12<<30, 8<<30)
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg, "-memory"}, &out, d); err != nil {
		t.Fatalf("history --memory = %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "v1") {
		t.Errorf("the hierarchy that was read is not named:\n%s", got)
	}
	// 64 GiB of host memory less 8 GiB of weights.
	// 64 GiB of host memory less 8 GiB of weights, with the limit column a
	// dash because there was none.
	if !strings.Contains(got, "56.0 GiB") {
		t.Errorf("the headroom is not computed against the host's total:\n%s", got)
	}
}
