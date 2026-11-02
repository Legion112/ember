package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
)

// spendDay is a fixed instant so a day-grouped key is predictable.
func spendDay(n int) time.Time { return time.Date(2026, 11, n, 12, 0, 0, 0, time.UTC) }

// seedSpend writes a finished request straight into the database the command
// reads, through the store's own interface.
//
// Out of band, as a lab seeds a volume through Docker: what is under test is
// the reader, and going through `ember serve` to produce the rows would mean
// renting something.
func seedSpend(t *testing.T, cfgPath string, d deps, fn func(t *testing.T, st genstore.Store)) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := app.ResolvePaths(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	st, err := genstore.Open(ps.State)
	if err != nil {
		t.Fatal(err)
	}
	fn(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// rentalRow records one finished rental with jobs that each produced an image.
func rentalRow(t *testing.T, st genstore.Store, gpu string, created time.Time,
	cost node.MicroUSD, billable time.Duration, jobs int, each node.MicroUSD,
	steps int64, took time.Duration) {
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
		g.Cost, g.Billable = cost, billable
		// A finished rental has a start and an end, as the queue writes it.
		g.Started, g.Ended = created, created.Add(billable)
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= jobs; i++ {
		if err := st.UpdateJob(t.Context(), id, i, func(j *genstore.JobRun) {
			j.Started, j.Ended = created, created.Add(took)
			j.Cost, j.Images, j.Bytes = each, 1, 1<<20
			j.Passes, j.Steps = 1, steps
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpend_ReportsRentalAndCardTimeSeparately(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		rentalRow(t, st, "rtx4090", spendDay(1), 890_000, time.Hour, 8, 95_000, 28, 7*time.Minute)
	})

	var out bytes.Buffer
	if err := runSpend([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("spend = %v\n%s", err, out.String())
	}
	got := out.String()

	// The two cost figures and the gap between them, which is the whole
	// reason the served shape exists: the rental also paid for provisioning,
	// the image pull, the store read and the model load once for everything
	// it served.
	for _, want := range []string{"rental", "card time", "overhead", "per image", "steps/s"} {
		if !strings.Contains(got, want) {
			t.Errorf("the table has no %q column:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "$0.89") {
		t.Errorf("the rental cost is not reported:\n%s", got)
	}
	if !strings.Contains(got, "$0.76") {
		t.Errorf("the card time (8 jobs at $0.095) is not reported:\n%s", got)
	}
	// 0.89 - 0.76, which is what a batch pays once rather than per job.
	if !strings.Contains(got, "$0.13") {
		t.Errorf("the overhead is not reported:\n%s", got)
	}
	if !strings.Contains(got, "generating") {
		t.Errorf("the sampling share of the bill is not reported:\n%s", got)
	}
}

func TestSpend_GroupsByCardAndByDay(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		rentalRow(t, st, "rtx4090", spendDay(1), 890_000, time.Hour, 2, 95_000, 28, time.Minute)
		rentalRow(t, st, "a100-80g", spendDay(2), 1_790_000, time.Hour, 2, 380_000, 28, 2*time.Minute)
	})

	var byCard bytes.Buffer
	if err := runSpend([]string{"-config", cfg, "-by", "gpu"}, &byCard, d); err != nil {
		t.Fatalf("spend --by gpu = %v\n%s", err, byCard.String())
	}
	got := byCard.String()
	if !strings.Contains(got, "card") || !strings.Contains(got, "rtx4090") || !strings.Contains(got, "a100-80g") {
		t.Errorf("by card:\n%s", got)
	}
	// The total line appears only when there is more than one row, so a
	// single-row report does not print the same numbers twice.
	if !strings.Contains(got, "total: 2 request(s)") {
		t.Errorf("no total across the cards:\n%s", got)
	}

	var byDay bytes.Buffer
	if err := runSpend([]string{"-config", cfg, "-by", "day"}, &byDay, d); err != nil {
		t.Fatal(err)
	}
	// UTC days, which is what the store groups by: a boundary in local time
	// would put the same spend in different buckets depending on where the
	// operator was sitting.
	for _, want := range []string{"2026-11-01", "2026-11-02"} {
		if !strings.Contains(byDay.String(), want) {
			t.Errorf("by day has no %s:\n%s", want, byDay.String())
		}
	}
}

func TestSpend_NarrowsToAHalfOpenPeriod(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		for _, n := range []int{1, 2, 3} {
			rentalRow(t, st, "rtx4090", spendDay(n), 1_000_000, time.Hour, 1, 100_000, 4, time.Minute)
		}
	})

	var out bytes.Buffer
	if err := runSpend([]string{"-config", cfg, "-since", "2026-11-02", "-until", "2026-11-03"}, &out, d); err != nil {
		t.Fatal(err)
	}
	// One request: --since includes and --until excludes, so consecutive
	// periods tile without counting a day twice.
	if !strings.Contains(out.String(), "    1 ") {
		t.Errorf("the period did not narrow to one request:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "$1") {
		t.Errorf("one rental's cost is not reported:\n%s", out.String())
	}
}

// TestSpend_SaysWhichPeriodFoundNothing. "no generations recorded" on its own
// reads as "ember has never generated anything" when it means "none last
// Tuesday".
func TestSpend_SaysWhichPeriodFoundNothing(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")

	var out bytes.Buffer
	if err := runSpend([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no generations recorded.") {
		t.Errorf("got %q", out.String())
	}

	out.Reset()
	if err := runSpend([]string{"-config", cfg, "-since", "2026-11-02"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "since 2026-11-02") {
		t.Errorf("the empty report does not name its period: %q", out.String())
	}
}

// TestSpend_NamesPerImageAsAnUpperBoundWhenHistoryIsMissing.
//
// A request from before the execution columns existed contributes its rental
// cost and none of its images, so the division is over a smaller denominator
// than the money paid for. Found by reading the real output rather than by a
// test, which is why there is now a test.
func TestSpend_NamesPerImageAsAnUpperBoundWhenHistoryIsMissing(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		rentalRow(t, st, "rtx4090", spendDay(1), 890_000, time.Hour, 2, 95_000, 28, time.Minute)
		// A version-2 row: jobs, a rental cost, and no execution facts.
		id, err := st.Enqueue(t.Context(), genstore.Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: spendDay(1),
			Jobs: []genstore.Job{{Prompt: "old"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Update(t.Context(), id, func(g *genstore.Request) {
			g.Status = genstore.StatusDone
			g.Cost = 500_000
			g.Started, g.Ended = spendDay(1), spendDay(1).Add(time.Hour)
		}); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	if err := runSpend([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "1 request(s) contribute their rental cost and no per-job figures") {
		t.Errorf("the gap in the history is not reported:\n%s", got)
	}
	if !strings.Contains(got, "upper bound") {
		t.Errorf("per image is not named as an upper bound:\n%s", got)
	}
	if !strings.Contains(got, "Nothing backfills them") {
		t.Errorf("the report implies the gap could be filled:\n%s", got)
	}
}

func TestSpend_JSONIsTheSameNumbersAndAnEmptyArray(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")

	// Empty first: an empty array and not null, for the reason the
	// websocket's first envelope sends `[]` -- null is not an empty list and
	// a consumer that iterates fails on it.
	var empty bytes.Buffer
	if err := runSpend([]string{"-config", cfg, "-json"}, &empty, d); err != nil {
		t.Fatal(err)
	}
	var none []genstore.Bucket
	if err := json.Unmarshal(empty.Bytes(), &none); err != nil {
		t.Fatalf("the empty JSON does not decode: %v (%q)", err, empty.String())
	}
	if strings.Contains(empty.String(), "null") {
		t.Errorf("an empty report came back as null: %q", empty.String())
	}

	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		rentalRow(t, st, "rtx4090", spendDay(1), 890_000, time.Hour, 8, 95_000, 28, 7*time.Minute)
	})

	var out bytes.Buffer
	if err := runSpend([]string{"-config", cfg, "-by", "gpu", "-json"}, &out, d); err != nil {
		t.Fatal(err)
	}
	var rows []genstore.Bucket
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("the JSON does not decode: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	b := rows[0]
	if b.Key != "rtx4090" || b.Requests != 1 || b.Jobs != 8 || b.Images != 8 {
		t.Errorf("row = %+v", b)
	}
	if b.RunCost != 890_000 || b.JobCost != 760_000 {
		t.Errorf("costs = %v/%v", b.RunCost, b.JobCost)
	}
	// Integer microdollars on the wire, never a formatted string and never a
	// float: $0.1+$0.2 is not $0.3 in float64, and a consumer that did its
	// own arithmetic on dollars would drift from ember's.
	if !strings.Contains(out.String(), `"run_cost_micro_usd": 890000`) {
		t.Errorf("money is not integer microdollars on the wire:\n%s", out.String())
	}
	// The derived figures are absent, because they are divisions of fields
	// that are present: a consumer should divide rather than trust a quotient
	// ember rounded.
	if strings.Contains(out.String(), "per_image") || strings.Contains(out.String(), "steps_per_second") {
		t.Errorf("a derived figure was stored in the JSON:\n%s", out.String())
	}
}

func TestSpend_Refusals(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")

	for name, args := range map[string][]string{
		"an unknown grouping":   {"-by", "model"},
		"a grouping with SQL":   {"-by", "provider; DROP TABLE requests"},
		"a date that is not":    {"-since", "last tuesday"},
		"a backwards period":    {"-since", "2026-11-03", "-until", "2026-11-01"},
		"an equal-ended period": {"-since", "2026-11-01", "-until", "2026-11-01"},
		"a positional argument": {"extra"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := runSpend(append([]string{"-config", cfg}, args...), &out, d)
			if ExitCode(err) != ExitUsage {
				t.Errorf("runSpend(%v) = %v, want a usage error", args, err)
			}
		})
	}

	// And the refusal for a model grouping says why, because it is the one an
	// operator will reach for first and the reason is not obvious.
	var out bytes.Buffer
	err := runSpend([]string{"-config", cfg, "-by", "model"}, &out, d)
	if err == nil || !strings.Contains(err.Error(), "nothing records which model") {
		t.Errorf("the refusal does not say why grouping by model is not offered: %v", err)
	}
}

func TestSpend_HelpSaysWhatIsNotInThere(t *testing.T) {
	var out bytes.Buffer
	_ = runSpend([]string{"-h"}, &out, labDeps(t))
	got := out.String()
	// `ember up` prints its cost and keeps nothing, so half the measurements
	// in README came from runs this command cannot see. Saying so is what
	// stops the report being read as everything ember has ever spent.
	if !strings.Contains(got, "ember up") {
		t.Errorf("the help does not say that `ember up` records nothing:\n%s", got)
	}
	if !strings.Contains(got, "half-open") {
		t.Errorf("the help does not explain the period bounds:\n%s", got)
	}
}
