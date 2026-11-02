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
		// "models" and not "model", which is now a grouping ember offers: the
		// near miss is what a closed set is for, and a plural typo must be a
		// refusal rather than one row keyed on nothing.
		"an unknown grouping":   {"-by", "models"},
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

	// And the refusal names the groupings there are, from the one list, so a
	// typo is answered with the set rather than with a document to go and
	// read.
	var out bytes.Buffer
	err := runSpend([]string{"-config", cfg, "-by", "models"}, &out, d)
	if err == nil {
		t.Fatal("a grouping that is not in the set was accepted")
	}
	for _, g := range genstore.GroupBys() {
		if !strings.Contains(err.Error(), string(g)) {
			t.Errorf("the refusal does not name the %q grouping: %v", g, err)
		}
	}
}

// TestSpend_GroupsByModel.
//
// THE GROUPING THAT MAKES THE OTHERS READABLE, and the one `ember spend` used
// to refuse: every measurement this project has made is a statement about a
// model on a card, so "an A100 costs $X per image" means nothing on its own.
func TestSpend_GroupsByModel(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		modelRow(t, st, "flux2-dev-Q8_0.gguf", "rtx-pro-6000", spendDay(1),
			2_490_000, time.Hour, 2, 400_000, 28, 140*time.Second)
		modelRow(t, st, "sd15.gguf", "rtx2080ti", spendDay(2),
			340_000, time.Hour, 2, 20_000, 4, 10*time.Second)
		// A request from before the column: it has a rental cost and no
		// model, which is what the dash row is.
		rentalRow(t, st, "rtx4090", spendDay(3), 890_000, time.Hour, 1, 95_000, 28, time.Minute)
	})

	var out bytes.Buffer
	if err := runSpend([]string{"-config", cfg, "-by", "model"}, &out, d); err != nil {
		t.Fatalf("spend --by model = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember spend --by model:\n%s", got)

	if !strings.HasPrefix(got, "model ") {
		t.Errorf("the key column is not headed model:\n%s", got)
	}
	// IN FULL, NOT TRUNCATED. A model name cut to the old twelve-character
	// key column turns two quantisations of one model into the same row, the
	// substring mistake node.NormalizeGPU exists to prevent.
	for _, want := range []string{"flux2-dev-Q8_0.gguf", "sd15.gguf"} {
		if !strings.Contains(got, want) {
			t.Errorf("the model %q is not reported whole:\n%s", want, got)
		}
	}
	// THE COLUMNS STILL LINE UP, which a fixed-width key column did not do
	// once a nineteen-character filename went in it: every figure in that row
	// was pushed right, so the table read as if the money sat under the wrong
	// headings. Every line ends with a right-aligned column, so an equal
	// length is an exact statement about alignment.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	header := lines[0]
	for _, l := range lines[1:] {
		if l == "" {
			break // the table ends at the blank line before any prose
		}
		if len(l) != len(header) {
			t.Errorf("a row is %d characters and the header is %d, so the columns do not line up:\n%s",
				len(l), len(header), got)
		}
	}
	// And the request nobody recorded a model for is its own row, keyed with
	// a dash rather than with a blank cell or with a model it did not run.
	// Money under a heading that claims to describe it is the mistake the
	// column exists to end, and an empty key would print as whitespace an
	// operator reads as a figure that failed to print.
	var dashed bool
	for _, l := range lines[1:] {
		if l == "" {
			break
		}
		if strings.HasPrefix(l, "- ") && strings.Contains(l, "$0.89") {
			dashed = true
		}
	}
	if !dashed {
		t.Errorf("the request with no model is not keyed with a dash:\n%s", got)
	}
}

// A group with no images reports a dash per image rather than $0.
//
// FOUND BY READING THE OUTPUT of `--by model`, which is where it first became
// visible: grouped by card, a request from before ember recorded anything sits
// in a row with real images and its missing figures are absorbed; grouped by
// model it is its own row, and it printed "$0" per image over a $0.50 rental.
// That reads as free, which is the direction a money figure must never be
// wrong in -- and the footnote under the table calls per image an upper BOUND,
// so the row contradicted the only warning the report gives.
func TestSpend_DashesADivisionWithNoDenominator(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		r := genstore.Request{
			Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: spendDay(1),
			Jobs: []genstore.Job{{Prompt: "a cat"}},
		}
		id, err := st.Enqueue(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		// Finished, charged for, and not one job ever reported: a request
		// from before the execution columns existed.
		if err := st.Update(t.Context(), id, func(g *genstore.Request) {
			g.Status = genstore.StatusDone
			g.Cost, g.Billable = 500_000, time.Hour
			g.Started, g.Ended = spendDay(1), spendDay(1).Add(time.Hour)
		}); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	if err := runSpend([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("spend = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember spend over a request with no images:\n%s", got)

	if !strings.Contains(got, "$0.5") {
		t.Errorf("the rental that was paid for is not reported:\n%s", got)
	}
	// The rental is real and the quotients are not. The last two cells are
	// per image and steps/s; "$0" and "0.00" there are claims that a card
	// generated images for free at no rate. Card time of $0 beside them is a
	// SUM of nothing and stays a zero, which is a different statement.
	row := strings.Split(got, "\n")[1]
	cells := strings.Fields(row)
	if len(cells) < 2 {
		t.Fatalf("the row has no figures: %q", row)
	}
	for _, cell := range cells[len(cells)-2:] {
		if cell != "-" {
			t.Errorf("a division with no denominator printed as %q: %q", cell, row)
		}
	}
}

// modelRow records a finished rental that reported which model it served.
func modelRow(t *testing.T, st genstore.Store, model, gpu string, created time.Time,
	cost node.MicroUSD, billable time.Duration, jobs int, each node.MicroUSD,
	steps int64, took time.Duration) {
	t.Helper()
	rentalRow(t, st, gpu, created, cost, billable, jobs, each, steps, took)
	rows, err := st.List(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// The newest, which is the one just written. Through Update because a
	// request cannot ASK for a model -- Enqueue does not write the column, so
	// this is the only way in, and it is the way the queue uses.
	if err := st.Update(t.Context(), rows[0].ID, func(r *genstore.Request) {
		r.Model = model
	}); err != nil {
		t.Fatal(err)
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
