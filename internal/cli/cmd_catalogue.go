package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
)

func cmdCatalogue(args []string, out io.Writer) error { return runCatalogue(args, out, deps{}) }

// runCatalogue dispatches the catalogue subcommands.
//
// Two of them, and the second is why the first is worth having: a collector
// whose output nothing reads is the failure this repository has paid for three
// times -- a feature built end to end where each half agrees with itself and
// no test crosses the gap. `report` is the reader, and it is deliberately
// thin.
func runCatalogue(args []string, out io.Writer, d deps) error {
	if len(args) == 0 {
		catalogueUsage(out)
		return usageError{errors.New("ember catalogue needs a subcommand")}
	}
	switch args[0] {
	case "snapshot":
		return runCatalogueSnapshot(args[1:], out, d)
	case "report":
		return runCatalogueReport(args[1:], out, d)
	case "-h", "--help", "help":
		catalogueUsage(out)
		return nil
	default:
		catalogueUsage(out)
		return usageError{fmt.Errorf("unknown catalogue subcommand %q", args[0])}
	}
}

func catalogueUsage(out io.Writer) {
	outf(out, "ember catalogue records what a provider said it had, and when.\n\n")
	outf(out, "RunPod reports availability as a bare level -- HIGH, MEDIUM, LOW, NONE --\nper card and per datacentre, with no timestamp and no history of its own.\nEvery read is a point sample, so this is the only data in ember that\ncannot be collected later: an hour nobody sampled is gone.\n\n")
	outf(out, "It matters because a store pins placement. A network volume lives in one\nsite and every node that mounts it is created there, so the question stops\nbeing \"where can I rent this card today\" and becomes \"which site will\nreliably have it for as long as the weights are there\" -- which a single\nsample cannot answer.\n\n")
	outf(out, "  snapshot  read the catalogue once and append it to the series\n")
	outf(out, "  report    say what the series collected so far shows\n\n")
	outf(out, "snapshot costs nothing: it runs no machine and rents nothing. Schedule it\nhourly from cron or a systemd timer -- ember does not become a daemon for\nthis, because a process that must stay running to collect data is a\nprocess that will be found stopped.\n")
}

// runCatalogueSnapshot takes one sample and appends it.
func runCatalogueSnapshot(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("catalogue snapshot", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	fs.Usage = func() {
		outf(out, "ember catalogue snapshot reads the catalogue once and appends it.\n\n")
		outf(out, "Two free GETs per provider -- no pod, no GPU, nothing billed -- writing\none row per type per datacentre into a day file under the state\ndirectory. Per site and never the global summary: the live catalogue has\nreported a card NONE overall while one of its sites read LOW, so a\ncollector keeping the summary would record \"unavailable\" for cards that\nwere rentable all along. That is worse than no data.\n\n")
		outf(out, "The file is append-only, JSON lines, one per day. Nothing rewrites it:\nthe value is entirely in the series, and a format that can rewrite\nhistory is one that can lose it to a bug in the writer.\n\n")
		outf(out, "Hourly is plenty. Capacity is four levels rather than a number, so\nsampling faster than it can visibly change buys nothing.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember catalogue snapshot takes no arguments and got %d (%v); "+
			"it samples every configured provider that keeps a catalogue", fs.NArg(), fs.Args())}
	}

	snap, err := app.CollectCatalogue(context.Background(), *cfgPath, d)
	// Printed before the error is returned, because a partial snapshot is
	// still recorded: the rows that did arrive are an hour of the series that
	// cannot be taken again, and an operator reading only the failure would
	// not know they are there.
	printSnapshot(out, snap)
	if err != nil {
		return err
	}
	if len(snap.Failed) > 0 {
		// A provider that could not be asked is a finding, not a silence --
		// the rule `ember ls --all` exits non-zero for. Here the gap it
		// leaves is permanent, which is the stronger version of the same
		// argument.
		return &FindingsError{N: len(snap.Failed)}
	}
	return nil
}

func printSnapshot(out io.Writer, snap app.CatalogueSnapshot) {
	if snap.At.IsZero() {
		// Nothing was attempted: the config, the credentials or the paths
		// failed first, and the error about to be returned says which.
		return
	}
	outf(out, "observed %s\n", snap.At.Format(time.RFC3339))
	for _, t := range snap.Taken {
		outf(out, "  %-10s %d gpu row(s), %d cpu row(s), %d site(s)",
			t.Provider, t.GPURows, t.CPURows, t.Sites)
		if t.NoSite > 0 {
			// Named, because otherwise the row count exceeds what the sites
			// account for and reads as an arithmetic mistake.
			outf(out, ", %d type(s) with no site named", t.NoSite)
		}
		outf(out, "\n")
	}
	for _, f := range snap.Failed {
		outf(out, "  %-10s could not be sampled: %v\n", f.Provider, f.Err)
	}
	for _, path := range snap.Files {
		outf(out, "appended %d row(s) to %s\n", snap.Samples, path)
	}
	if snap.First && snap.Samples > 0 {
		// Said once, on the run where it can still be acted on. A collector
		// that has to be scheduled and was not is a collector that will be
		// found stopped, and the hours it missed are not recoverable.
		outf(out, "\nThis is the first snapshot here, and its whole value is the series: one\n"+
			"sample answers nothing a live `ember offers` does not. Schedule it hourly --\n"+
			"cron or a systemd timer, see README -- because an hour nobody sampled cannot\n"+
			"be sampled later.\n")
	}
}

// runCatalogueReport reads the series back.
func runCatalogueReport(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("catalogue report", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cfgPath = fs.String("config", config.FileName, "path to ember.yaml")
		gpu     = fs.String("gpu", "", "only this card, matched as `ember up --gpu` matches")
		kind    = fs.String("kind", "", "only gpu or only cpu rows; empty is both")
		since   = fs.String("since", "", "only samples observed at or after this date (YYYY-MM-DD, UTC)")
		until   = fs.String("until", "", "only samples observed before this date (YYYY-MM-DD, UTC)")
		asJSON  = fs.Bool("json", false, "print the rows as JSON")
	)
	fs.Usage = func() {
		outf(out, "ember catalogue report says what the collected series shows.\n\n")
		outf(out, "One row per type per datacentre: how many samples, how often something\nwas rentable there, how the levels were distributed, and what the two\ncloud tiers quoted -- as a range, because whether prices drift is one of\nthe questions the series exists to answer.\n\n")
		outf(out, "The `masked` column is why the global summary is stored at all. It\ncounts samples where the card's overall figure said NONE while this site\nreported capacity -- the reading that would have had ember answer \"no\ncapacity\" for a card it could have rented.\n\n")
		outf(out, "Dates are UTC and the period is half-open -- --since includes, --until\nexcludes -- so consecutive periods tile without counting a sample twice.\n\n")
		outf(out, "usage: ember catalogue report [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember catalogue report takes no arguments and got %d (%v); "+
			"narrow it with --gpu, --kind, --since and --until", fs.NArg(), fs.Args())}
	}

	want := catalogue.Kind(strings.ToLower(*kind))
	if *kind != "" && !want.Valid() {
		return usageError{fmt.Errorf("cannot report on kind %q; ember records %q and %q, or both when --kind is empty",
			*kind, catalogue.KindGPU, catalogue.KindCPU)}
	}
	from, err := parseDay(*since, "--since")
	if err != nil {
		return usageError{err}
	}
	to, err := parseDay(*until, "--until")
	if err != nil {
		return usageError{err}
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		// Refused rather than answered with nothing, for the reason `ember
		// spend` refuses it: an empty report over a period typed backwards
		// reads as "capacity was never recorded", which is the one answer
		// this command must not give wrongly.
		return usageError{fmt.Errorf("--since %s is not before --until %s, so the period is empty", *since, *until)}
	}

	series, dir, err := app.ReadCatalogue(*cfgPath, d, from, to)
	if err != nil {
		return err
	}

	rows := filterRows(catalogue.Summarise(series.Samples), want, node.NormalizeGPU(*gpu))

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if rows == nil {
			// An empty array and not null, for the reason the websocket's
			// first envelope sends `[]`: null is not an empty list, and a
			// consumer that iterates fails on it.
			rows = []catalogue.Row{}
		}
		if err := enc.Encode(rows); err != nil {
			return err
		}
		// Nothing human goes on this stream. A note printed after the array
		// is a note inside the thing a caller is about to parse, so with
		// --json the finding travels as the error -- which main prints to
		// stderr and which still exits 2.
		return reportFindings(out, series, true)
	}

	if len(rows) == 0 {
		outf(out, "no samples recorded%s in %s.\n", periodPhrase(from, to), dir)
		outf(out, "Collect one with `ember catalogue snapshot`, then schedule it hourly.\n")
		return reportFindings(out, series, false)
	}

	printCatalogue(out, rows)
	printCatalogueFooter(out, series, rows)
	return reportFindings(out, series, false)
}

// filterRows narrows the summary to what was asked for.
//
// After Summarise rather than before it, so a row's sample count is the count
// of what was observed and not of what the filter kept -- a share computed
// over a filtered denominator would move when the operator changed --gpu.
func filterRows(in []catalogue.Row, kind catalogue.Kind, gpu node.GPUModel) []catalogue.Row {
	var out []catalogue.Row
	for _, r := range in {
		if kind != "" && r.Kind != kind {
			continue
		}
		// The card filter applies to GPU rows only. A CPU family's id is
		// "cpu5c", and normalising it through a GPU model matcher would
		// silently drop every CPU row whenever --gpu is set -- which is
		// right, and saying so here is what keeps it from looking like a
		// match that failed.
		if gpu != "" && (r.Kind != catalogue.KindGPU || !r.Match(gpu)) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// printCatalogue writes the table, grouped by type.
//
// Grouped rather than one flat table with the card repeated on every line,
// because the comparison the series is for is between SITES of one card: that
// is the decision a store makes irreversible.
func printCatalogue(out io.Writer, rows []catalogue.Row) {
	// The header once, at the top, and not per group. A real catalogue is
	// forty types over a handful of sites each, so a header per type is
	// forty header lines in an eighty-line table -- which is how a report
	// stops being read. Printed before the first group rather than after,
	// so it is still on screen when the first numbers are.
	outf(out, "  %-14s %7s %9s %5s %5s %5s %5s %7s %7s  %-14s %s\n",
		"site", "samples", "rentable", "high", "med", "low", "none", "unseen", "masked",
		"secure", "community")

	lastKey := ""
	summaries := 0
	for _, r := range rows {
		key := string(r.Kind) + "\x00" + r.ID
		if key != lastKey {
			lastKey = key
			outf(out, "\n%s  %s%s\n", r.Kind, r.ID, describe(r))
		}
		site := r.Datacentre
		if r.FromSummary {
			// Marked rather than printed as a site, because its level counts
			// are the global summary's. A row that silently read as a
			// datacentre would be the exact confusion the per-site rule
			// exists to prevent.
			site = "(no site)*"
			summaries++
		}
		// The last column is not padded: a line that ends in fourteen spaces
		// is a line that looks different in a diff, a mail and a terminal
		// than it does in a file.
		outf(out, "  %-14s %7d %9s %5d %5d %5d %5d %7d %7d  %-14s %s\n",
			site, r.Samples, share(r), r.High, r.Medium, r.Low, r.None, r.Unreported, r.Masked,
			prices(r.Secure), prices(r.Community))
	}
	if summaries > 0 {
		outf(out, "\n* the provider named no datacentre for this type in those samples, so the\n"+
			"  levels shown are its overall summary -- the only figure it gave.\n")
	}
}

// describe is the parenthetical after a type's id.
func describe(r catalogue.Row) string {
	var bits []string
	if r.Name != "" && r.Name != r.ID {
		bits = append(bits, r.Name)
	}
	if r.MemoryGiB > 0 {
		bits = append(bits, fmt.Sprintf("%d GiB", r.MemoryGiB))
	}
	if len(bits) == 0 {
		return ""
	}
	return "  (" + strings.Join(bits, ", ") + ")"
}

// share formats the rentable fraction, or says there is nothing to divide.
//
// A dash and not 0%. Zero per cent means the provider said NONE every time;
// no reported level means it did not say, and sending an operator to move a
// store over a field that was absent is the mistake this column would
// otherwise make.
func share(r catalogue.Row) string {
	f, ok := r.Share()
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", f*100)
}

// prices formats one tier's quote over the period.
func prices(p catalogue.Prices) string {
	if !p.Offered {
		return "-"
	}
	if !p.Moved() {
		return p.Min.String()
	}
	// Both ends, because a tier whose price moved is the answer to "do prices
	// drift" and a single figure cannot carry it.
	return fmt.Sprintf("%s-%s", node.MicroUSD(p.Min), p.Max)
}

func printCatalogueFooter(out io.Writer, series catalogue.Series, rows []catalogue.Row) {
	// Counted over the rows that were printed and not over the whole read.
	// With --gpu set the two disagree, and "12 sample(s) in 2 row(s)" over a
	// table showing six is a figure that describes something the reader
	// cannot see. Found by reading the output.
	shown := 0
	var first, last time.Time
	for _, r := range rows {
		shown += r.Samples
		if first.IsZero() || r.First.Before(first) {
			first = r.First
		}
		if r.Last.After(last) {
			last = r.Last
		}
	}
	outf(out, "\n%d sample(s) in %d row(s) over %d %s, %s to %s.\n",
		shown, len(rows), len(series.Days), plural(len(series.Days), "day file", "day files"),
		first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
	if dropped := len(series.Samples) - shown; dropped > 0 {
		outf(out, "%d sample(s) were read and left out by the filter.\n", dropped)
	}

	// The design note's own objection, printed rather than left in the note:
	// a collector with three days of data answers nothing a live query does
	// not. Saying so is what keeps a short series from being read as a
	// history.
	if span := last.Sub(first); span < 7*24*time.Hour {
		outf(out, "The series covers %s. Availability moves on the scale of a day, so this is\n"+
			"closer to a point sample than to a history -- it becomes an answer after weeks.\n",
			roughly(span))
	}
	for _, r := range rows {
		if len(r.Unknown) > 0 {
			// Named out loud. An unrecognised level is the one thing in the
			// file nothing in ember could have predicted, and it ranks as
			// LOW rather than as NONE -- so it is counted, and the operator
			// is told the word rather than left to wonder which column it
			// went into.
			outf(out, "%s at %s reported %s, which ember does not recognise; counted as capacity\n"+
				"rather than as none, and shown in the `other` column of --json.\n",
				r.ID, keyOr(r.Datacentre), joinLevels(r.Unknown))
		}
	}
}

func joinLevels(ls []catalogue.Level) string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, Quote(string(l)))
	}
	return strings.Join(out, ", ")
}

// reportFindings names what the reader could not read, and makes it an exit
// status.
//
// Exit 2 and never a silent skip. A line ember could not place is a sample
// that is in the file and not in the total, and a report quietly missing part
// of its history is the clean-bill-of-health failure `ember ls --all` exits
// non-zero to avoid -- here over a file that cannot be rebuilt.
//
// A file in the directory that is not a day file counts too. A day somebody
// gzipped or renamed is a shorter history than this report thinks it has, and
// the report would not otherwise say the data existed.
func reportFindings(out io.Writer, series catalogue.Series, quiet bool) error {
	n := series.Malformed + len(series.Ignored)
	if n == 0 {
		return nil
	}
	if quiet {
		// One line, as the error, so --json stays parseable.
		return fmt.Errorf("the series is not wholly readable: %d unplaceable line(s) (%s) and "+
			"%d file(s) that are not day files (%s); the rows printed are only what could be read: %w",
			series.Malformed, keyOr(series.Reason), len(series.Ignored),
			keyOr(strings.Join(series.Ignored, ", ")), &FindingsError{N: n})
	}
	if series.Malformed > 0 {
		outf(out, "\n%d line(s) could not be placed in the series and were left out of every\n"+
			"figure above. The first: %s\n", series.Malformed, series.Reason)
		outf(out, "A truncated last line is what a crash during an append leaves behind. The\n"+
			"file is append-only, so nothing in ember will repair or remove it.\n")
	}
	if len(series.Ignored) > 0 {
		outf(out, "\n%d file(s) in the directory are not day files and were not read: %s\n",
			len(series.Ignored), strings.Join(series.Ignored, ", "))
	}
	return &FindingsError{N: n}
}

// roughly says how long a span is in the unit an operator thinks in.
//
// Hours below a day and whole days above, because "27h30m0s" is how long the
// series is in a format nobody reads as "a day and a bit" -- the same defect
// as the unreadable byte count `human` was fixed for, found the same way.
func roughly(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minute(s)", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0f hour(s)", d.Hours())
	default:
		return fmt.Sprintf("%.0f day(s)", d.Hours()/24)
	}
}
