package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
)

func cmdHistory(args []string, out io.Writer) error { return runHistory(args, out, deps{}) }

// runHistory lists what ember has been asked for and what each request cost.
//
// THE LEDGER BESIDE THE AGGREGATE. `ember spend` answers "what did last week
// cost"; this answers "which run was it", which is the next question whenever
// the aggregate surprises somebody. Before these two, the only reader of the
// generation history was the web page -- so looking at a finished run meant
// starting a server that can rent a card, or opening SQLite by hand.
//
// IT CANNOT SPEND. There is no registry in this function and no provider is
// constructed, so there is nothing here that could contact one, let alone
// enqueue or claim; genstore.History runs one SELECT. "The ledger must not be
// able to rent" is a property of what this function has rather than a rule
// somebody has to remember, which is the DryProvider distinction applied to a
// command that does not need a provider at all.
func runHistory(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cfgPath  = fs.String("config", config.FileName, "path to ember.yaml")
		since    = fs.String("since", "", "only requests created at or after this date (YYYY-MM-DD, UTC)")
		until    = fs.String("until", "", "only requests created before this date (YYYY-MM-DD, UTC)")
		provider = fs.String("provider", "", "only requests that named this provider")
		gpu      = fs.String("gpu", "", "only requests that named this card")
		status   = fs.String("status", "", "only requests in this status")
		limit    = fs.Int("limit", 0, "how many rows at most, newest first")
		asJSON   = fs.Bool("json", false, "print the rows as JSON")
	)
	fs.Usage = func() {
		outf(out, "ember history lists the generations ember has run.\n\n")
		outf(out, "Out of the database `ember serve` writes, which is the only thing that\nrecords a generation. `ember up` prints its cost and keeps nothing, so a\nrun made that way is not in here -- see README.\n\n")
		outf(out, "Two cost columns. rental is what the provider charged for the node;\ncard time is what this request's own jobs took. A rental serves every\nqueued request it can, so the rental is charged to the request that\nopened it and a dash means this one rode along on somebody else's.\n\n")
		outf(out, "Dates are UTC and the period is half-open -- --since includes, --until\nexcludes -- the same rule `ember spend` uses, so the two tile alike.\n\n")
		outf(out, "It reads and nothing else: no provider is contacted and the queue cannot\nbe advanced through it.\n\n")
		outf(out, "usage: ember history [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember history takes no arguments and got %d (%v); "+
			"narrow it with --since, --provider, --gpu and --status", fs.NArg(), fs.Args())}
	}

	f := genstore.Filter{
		Provider: *provider,
		GPU:      *gpu,
		Status:   genstore.Status(*status),
		Limit:    *limit,
	}
	if f.Status != "" && !f.Status.Valid() {
		return usageError{fmt.Errorf("%q is not a status ember writes; they are %v",
			*status, genstore.Statuses())}
	}
	if *limit < 0 {
		// Refused rather than clamped, because -1 reads as "no limit" and
		// would silently become the default page instead.
		return usageError{fmt.Errorf("--limit %d is negative; leave it out for the default page", *limit)}
	}
	var err error
	if f.Since, err = parseDay(*since, "--since"); err != nil {
		return usageError{err}
	}
	if f.Until, err = parseDay(*until, "--until"); err != nil {
		return usageError{err}
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Since.Before(f.Until) {
		// The same refusal `ember spend` makes, for the same reason: an empty
		// listing over a period typed backwards reads as "ember ran nothing".
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

	rows, err := store.History(context.Background(), f)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if rows == nil {
			// An empty array and not null, for the reason the websocket's
			// first envelope sends `[]`: null is not an empty list, and a
			// consumer that iterates would fail on it.
			rows = []genstore.Entry{}
		}
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		outf(out, "no generations recorded%s.\n", historyPhrase(f))
		return nil
	}
	printHistory(out, rows)
	return nil
}

// historyRow is the column layout, named once so the header and the rows
// cannot drift apart -- which is how a table ends up with its headings one
// column off from its figures.
const historyRow = "%-16s %-16s %-11s %-10s %-12s %5s %6s %9s %11s %11s\n"

// printHistory writes the table.
func printHistory(out io.Writer, rows []genstore.Entry) {
	outf(out, historyRow, "when (utc)", "request", "status", "provider", "card",
		"jobs", "images", "wall", "rental", "card time")

	var (
		rental, card     node.MicroUSD
		requests, images int
		rode, unrecorded int
		failed           []struct{ id, err string }
	)
	for _, e := range rows {
		if e.Err != "" {
			failed = append(failed, struct{ id, err string }{string(e.ID), oneLine(e.Err)})
		}
		imgs, cardTime := fmt.Sprintf("%d", e.Images), e.JobCost.String()
		if e.Unrecorded {
			// A dash rather than a zero. This request finished and reported
			// no per-job figures at all, which is what every request from
			// before schema version 3 looks like -- and "0 images" is a claim
			// that it produced nothing, which is a different fact.
			imgs, cardTime = "-", "-"
			unrecorded++
		}
		rentalCell := e.Cost.String()
		if e.Rode {
			rentalCell = "-"
			rode++
		}

		outf(out, historyRow,
			e.Created.UTC().Format("2006-01-02 15:04"), string(e.ID),
			string(e.Status), e.Provider, keyOr(e.GPU),
			fmt.Sprintf("%d", e.Jobs), imgs, wall(e.Wall()),
			rentalCell, cardTime)

		requests++
		images += e.Images
		// Summing the rental column is sound exactly because a rider's cost
		// is zero: each rental is counted once, on the request that opened
		// it, which is the rule `genstore.Bucket.RunCost` sums under.
		rental += e.Cost
		card += e.JobCost
	}

	// One blank line between the table and any prose, whichever prose there
	// turns out to be. Without it a footnote with no total above it runs
	// straight into the last row and reads as another one.
	if len(rows) > 1 || rode > 0 || unrecorded > 0 || len(failed) > 0 {
		outf(out, "\n")
	}
	if len(rows) > 1 {
		outf(out, "total: %d request(s), %d image(s), %s of rental, %s of card time\n",
			requests, images, rental, card)
	}
	for _, f := range failed {
		// The reason, not just the status. A ledger that says "failed" and
		// nothing else sends the operator back to reading SQLite by hand,
		// which is the whole complaint this command answers.
		outf(out, "%s: %s\n", f.id, f.err)
	}
	if rode > 0 {
		outf(out, "%d request(s) rode along on a rental another request opened, so their rental\n"+
			"cost is a dash rather than zero: the node was charged once, to whichever\n"+
			"request paid for it to exist.\n", rode)
	}
	if unrecorded > 0 {
		outf(out, "%d request(s) finished with no per-job figures -- from before ember recorded\n"+
			"them, or without a job completing -- so their images and card time are a dash\n"+
			"rather than zero. Nothing backfills them.\n", unrecorded)
	}
}

// oneLine is a stored error as one line of a listing.
//
// Bounded and flattened, because the string came from whatever failed: a
// workload's complaint can be a page of a node's stderr, and a table that
// reflows around one row is a table nothing can be read out of. The full text
// is in --json and in the event stream.
func oneLine(s string) string {
	const max = 100
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// wall formats a request's own span, or says there is none.
//
// A dash for zero rather than "0s", because zero here means an end or a start
// was never recorded -- a queued request, or one the process died holding --
// and "0s" reads as a generation that took no time.
func wall(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return d.Round(time.Second).String()
}

// historyPhrase names the period in the nothing-found line, so "no
// generations recorded" cannot be read as "ember has never generated
// anything" when it means "none from runpod last Tuesday".
func historyPhrase(f genstore.Filter) string {
	const day = "2006-01-02"
	var s string
	switch {
	case !f.Since.IsZero() && !f.Until.IsZero():
		s = fmt.Sprintf(" between %s and %s", f.Since.Format(day), f.Until.Format(day))
	case !f.Since.IsZero():
		s = " since " + f.Since.Format(day)
	case !f.Until.IsZero():
		s = " before " + f.Until.Format(day)
	}
	if f.Provider != "" {
		s += " on " + f.Provider
	}
	if f.GPU != "" {
		s += " for " + f.GPU
	}
	if f.Status != "" {
		s += " in status " + string(f.Status)
	}
	return s
}
