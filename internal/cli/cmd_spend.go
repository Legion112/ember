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
		by      = fs.String("by", "", "group by provider, gpu, day or model; empty is one total")
		since   = fs.String("since", "", "only requests created at or after this date (YYYY-MM-DD, UTC)")
		until   = fs.String("until", "", "only requests created before this date (YYYY-MM-DD, UTC)")
		asJSON  = fs.Bool("json", false, "print the rows as JSON")
	)
	fs.Usage = func() {
		outf(out, "ember spend reports what the generation history says was spent.\n\n")
		outf(out, "Out of the database both `ember serve` and `ember up` write to, so a\nrental shows up here however it was started. An `ember up` that forwarded\nports or ran `cmds` contributes its rental cost and no per-job figures,\nbecause it had no jobs to report -- see README.\n\n")
		outf(out, "Two cost figures, and the difference between them is the point. The\nrental is what the provider charges; card time is what the jobs\nthemselves took. A batch pays provisioning, the image pull, the store\nread and the model load once for everything it serves, and that gap is\nthe overhead column.\n\n")
		outf(out, "Dates are UTC and the period is half-open -- --since includes, --until\nexcludes -- so consecutive periods tile without counting a day twice.\n\n")
		outf(out, "--by model groups by the name the generation server reported when the\nweights went resident, not by what `sd.serve` names today: a config edited\nbetween two rentals would otherwise attribute last week's spend to this\nweek's weights. It is the checkpoint's filename, or the diffusion model's\nwhere a configuration names its text encoder and VAE separately -- so two\nsetups differing only in those two read as one model. A request from\nbefore ember recorded it, one that never got a node, and a run that\nstarted no generation server at all -- any `ember up` with ports or\n`cmds` -- group under a dash; nothing backfills.\n\n")
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
		return usageError{fmt.Errorf("cannot group by %q; ember groups by %v, or by nothing for one total",
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
		outf(out, "no generations recorded%s.\n", periodPhrase(q.Since, q.Until))
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
		genstore.ByModel:    "model",
	}[group]

	// THE KEY COLUMN IS AS WIDE AS THE WIDEST KEY, which it was not until a
	// model name went in it. A model is a filename -- "flux2-dev-Q8_0.gguf"
	// is nineteen characters where "rtx4090" is seven -- and a fixed twelve
	// pushed every figure in that row out of its column, so the table read as
	// if the numbers belonged to different headings. Truncating instead would
	// have been worse: a name cut to twelve turns two quantisations of one
	// model into one row's worth of identical text, which is the substring
	// mistake node.NormalizeGPU exists to prevent, in a report.
	//
	// Never narrower than the old twelve, so every other grouping prints
	// exactly as it did.
	keyWidth := len(label)
	for _, b := range rows {
		if n := len(keyOr(b.Key)); n > keyWidth {
			keyWidth = n
		}
	}
	if keyWidth < 12 {
		keyWidth = 12
	}
	keyCell := fmt.Sprintf("%%-%ds ", keyWidth)

	if label != "" {
		outf(out, keyCell, label)
		outf(out, "%5s %5s %6s %11s %11s %11s %10s %7s\n",
			"reqs", "jobs", "images", "rental", "card time", "overhead", "per image", "steps/s")
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
			outf(out, keyCell, keyOr(b.Key))
		}
		// A DASH WHERE THE DIVISION HAS NO DENOMINATOR, which is the rule
		// `ember history` already applies to its own columns and which this
		// table did not until a model grouping gave the unrecorded requests a
		// row of their own. Aggregated by card they hid inside a row with
		// real images; grouped by model, a request from before ember recorded
		// anything is its own row -- and it printed "$0" per image over a
		// $0.50 rental, which reads as free. That is wrong in the cheap
		// direction, and the footnote three screens down calls per image an
		// upper BOUND, so the table was contradicting the only warning it
		// gives. Found by reading the output, not by a test.
		perImage, rate := b.PerImage().String(), fmt.Sprintf("%.2f", b.StepsPerSecond())
		if b.Images <= 0 {
			perImage = "-"
		}
		if b.Steps <= 0 || b.Sampling <= 0 {
			rate = "-"
		}
		outf(out, "%5d %5d %6d %11s %11s %11s %10s %7s\n",
			b.Requests, b.Jobs, b.Images, b.RunCost, b.JobCost, b.Overhead(),
			perImage, rate)

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
		outf(out, "%d request(s) contribute their rental cost and no per-job figures -- they had\n"+
			"no jobs at all, or they ran before ember recorded them, or they finished\n"+
			"without a job completing. So per image is an upper bound here, and steps/s is\n"+
			"computed only over what was recorded. Nothing backfills them.\n", skipped)
	}
}

// keyOr prints a dash for a group with no key.
//
// A CPU-only request has no card, and the empty string in that column would
// read as a missing value rather than as the answer. Grouped by model it is
// the other way round and the dash is just as necessary: the key is empty
// because nobody read the model -- a request that never got a node, or one
// from before schema version 5 -- and a blank cell in a column of names reads
// as a value that failed to print.
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

// periodPhrase names the period in a nothing-found line, so "no generations
// recorded" cannot be read as "ember has never generated anything" when it
// means "none last Tuesday".
//
// Shared with `ember catalogue report`, which has the same half-open period
// and the same hazard. Two copies of this sentence would drift, and the
// drifted one would describe a period the command did not query.
func periodPhrase(since, until time.Time) string {
	const day = "2006-01-02"
	switch {
	case !since.IsZero() && !until.IsZero():
		return fmt.Sprintf(" between %s and %s", since.Format(day), until.Format(day))
	case !since.IsZero():
		return " since " + since.Format(day)
	case !until.IsZero():
		return " before " + until.Format(day)
	default:
		return ""
	}
}
