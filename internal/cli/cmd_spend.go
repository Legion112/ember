package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
)

func cmdSpend(args []string, out io.Writer) error { return runSpend(args, out, deps{}) }

// runSpend reports what ember has spent, out of the columns the generation
// history now carries.
//
// IT EXISTS SO THE QUERY LAYER HAS A READER. Execution columns, a migration
// and an aggregate with nothing calling them would be the exact failure this
// repository has already paid for twice -- a feature built end to end and
// never connected, where every half agrees with itself and no test crosses
// the gap. `ember serve`'s API is the only other thing that reads this
// database, and it answers questions about one request.
//
// It deliberately does not grow into #27's ledger. That issue wants
// `ember history` beside this, with per-request listings and an operator
// workflow; what is here is the thin reader that proves the SUM is right and
// gives #27 something to build on rather than a second cost-summing path to
// reconcile with.
func runSpend(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("spend", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cfgPath = fs.String("config", config.FileName, "path to ember.yaml")
		by      = fs.String("by", "", "group by provider, gpu or day; empty is one total")
		since   = fs.String("since", "", "only requests created at or after this date (YYYY-MM-DD, UTC)")
		until   = fs.String("until", "", "only requests created before this date (YYYY-MM-DD, UTC)")
		asJSON  = fs.Bool("json", false, "print the rows as JSON")
	)
	fs.Usage = func() {
		outf(out, "ember spend reports what the generation history says was spent.\n\n")
		outf(out, "Out of the database `ember serve` writes, which is the only thing that\nrecords a generation. `ember up` prints its cost and keeps nothing, so a\nrun made that way is not in here -- see README.\n\n")
		outf(out, "Two cost figures, and the difference between them is the point. The\nrental is what the provider charges; card time is what the jobs\nthemselves took. A batch pays provisioning, the image pull, the store\nread and the model load once for everything it serves, and that gap is\nthe overhead column.\n\n")
		outf(out, "Dates are UTC and the period is half-open -- --since includes, --until\nexcludes -- so consecutive periods tile without counting a day twice.\n\n")
		outf(out, "usage: ember spend [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember spend takes no arguments and got %d (%v); "+
			"narrow it with --since, --until and --by", fs.NArg(), fs.Args())}
	}

	group := genstore.GroupBy(*by)
	if !group.Valid() {
		return usageError{fmt.Errorf("cannot group by %q; ember groups by %v, or by nothing for one total. "+
			"Grouping by model is not offered because nothing records which model served a request -- see README",
			*by, genstore.GroupBys())}
	}
	q := genstore.SpendQuery{By: group}
	var err error
	if q.Since, err = parseDay(*since, "--since"); err != nil {
		return usageError{err}
	}
	if q.Until, err = parseDay(*until, "--until"); err != nil {
		return usageError{err}
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Since.Before(q.Until) {
		// Refused rather than quietly returning nothing. An empty report over
		// a period the operator typed backwards reads as "ember spent
		// nothing", which is the one answer a spend report must not give
		// wrongly.
		return usageError{fmt.Errorf("--since %s is not before --until %s, so the period is empty",
			*since, *until)}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ps, err := app.ResolvePaths(cfg, d)
	if err != nil {
		return err
	}
	store, err := genstore.Open(ps.State)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	rows, err := store.Spend(context.Background(), q)
	if err != nil {
		return err
	}

	if *asJSON {
		// Through the Bucket type's own tags, so the JSON and the table
		// cannot describe different numbers. The derived figures are not in
		// it: they are divisions of fields that are, and a consumer that
		// wants cost per image should divide rather than trust a quotient
		// ember rounded.
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if rows == nil {
			// An empty array and not null, for the reason the websocket's
			// first envelope sends `[]`: null is not an empty list, and a
			// consumer that iterates would fail on it.
			rows = []genstore.Bucket{}
		}
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		outf(out, "no generations recorded%s.\n", periodPhrase(q))
		return nil
	}
	printSpend(out, rows, group)
	return nil
}

// printSpend writes the table.
//
// Measured facts first and derived figures after, in that order deliberately:
// the columns an operator can check against an invoice come before the ones
// ember computed for them.
func printSpend(out io.Writer, rows []genstore.Bucket, group genstore.GroupBy) {
	label := map[genstore.GroupBy]string{
		genstore.ByProvider: "provider",
		genstore.ByGPU:      "card",
		genstore.ByDay:      "day",
	}[group]

	if label != "" {
		outf(out, "%-12s %5s %5s %6s %11s %11s %11s %10s %7s\n",
			label, "reqs", "jobs", "images", "rental", "card time", "overhead", "per image", "steps/s")
	} else {
		outf(out, "%5s %5s %6s %11s %11s %11s %10s %7s\n",
			"reqs", "jobs", "images", "rental", "card time", "overhead", "per image", "steps/s")
	}

	var (
		total   genstore.Bucket
		skipped int
	)
	for _, b := range rows {
		if label != "" {
			outf(out, "%-12s ", keyOr(b.Key))
		}
		outf(out, "%5d %5d %6d %11s %11s %11s %10s %7.2f\n",
			b.Requests, b.Jobs, b.Images, b.RunCost, b.JobCost, b.Overhead(),
			b.PerImage(), b.StepsPerSecond())

		total.Requests += b.Requests
		total.Jobs += b.Jobs
		total.Images += b.Images
		total.RunCost += b.RunCost
		total.JobCost += b.JobCost
		total.Steps += b.Steps
		total.Sampling += b.Sampling
		total.Billable += b.Billable
		skipped += b.Unrecorded
	}

	if len(rows) > 1 {
		outf(out, "\ntotal: %d request(s), %d image(s), %s of rental, %s of card time\n",
			total.Requests, total.Images, total.RunCost, total.JobCost)
	}
	if share := total.SamplingShare(); share > 0 {
		// The share of the bill that was generating. The rest is the overhead
		// a batch exists to pay once, and it is the number ember's own 7%
		// figure in README was computed by hand from.
		outf(out, "%.0f%% of billable rental time was spent generating.\n", share*100)
	}
	if skipped > 0 {
		// Said out loud, every time. A total missing part of its history and
		// not saying so is the clean-bill-of-health failure `ember ls --all`
		// exits non-zero to avoid, in a column of money.
		//
		// And the per-image figure is named as an upper bound, which was
		// found by reading the real output rather than by a test: those
		// requests contribute their rental cost and none of their images, so
		// the division is over a smaller denominator than the money paid
		// for. A figure that is wrong in the expensive direction and does
		// not say so is worse than one that is missing.
		outf(out, "%d request(s) contribute their rental cost and no per-job figures -- they ran\n"+
			"before ember recorded them, or finished without a job completing. So per image\n"+
			"is an upper bound here, and steps/s is computed only over what was recorded.\n"+
			"Nothing backfills them.\n", skipped)
	}
}

// keyOr prints a dash for a group with no key.
//
// A CPU-only request has no card, and the empty string in that column would
// read as a missing value rather than as the answer.
func keyOr(k string) string {
	if k == "" {
		return "-"
	}
	return k
}

// parseDay reads a YYYY-MM-DD bound as UTC.
//
// A date and not a timestamp, because a spend report is asked for in days;
// UTC and not local, because a day boundary in local time puts the same spend
// in different buckets depending on where the operator was sitting, and the
// grouped rows are keyed in UTC too.
func parseDay(s, flagName string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q is not a date; ember wants YYYY-MM-DD, in UTC", flagName, s)
	}
	return t, nil
}

// periodPhrase names the period in the nothing-found line, so "no generations
// recorded" cannot be read as "ember has never generated anything" when it
// means "none last Tuesday".
func periodPhrase(q genstore.SpendQuery) string {
	const day = "2006-01-02"
	switch {
	case !q.Since.IsZero() && !q.Until.IsZero():
		return fmt.Sprintf(" between %s and %s", q.Since.Format(day), q.Until.Format(day))
	case !q.Since.IsZero():
		return " since " + q.Since.Format(day)
	case !q.Until.IsZero():
		return " before " + q.Until.Format(day)
	default:
		return ""
	}
}
