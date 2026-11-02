package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
)

// The three commands that remove something, and the rule they share.
//
// NOTHING IN EMBER DELETED AN OPERATOR'S DATA BEFORE THIS FILE. `ember store
// rm` destroys a volume somebody paid to fill, which is the nearest thing, and
// it takes a name that is never read from the config precisely so that there
// is no accident available. These are the first commands that act on what a
// generation produced, so they carry the inverse of the rule that keeps a
// rental from creating a standing monthly charge: nothing that runs a
// generation may delete an older one as a side effect, and nothing here runs
// on a timer, on startup, or as part of anything else.
//
// EVERY ONE OF THEM REFUSES WITHOUT --yes, AFTER PRINTING WHAT WOULD GO. The
// refusal is a usage error and so exits 3, not 0: a script that forgot the
// flag must not read the exit status as "it was removed". --dry-run is the
// spelling for asking on purpose, and it wins over --yes rather than losing to
// it, so `--yes --dry-run` removes nothing.
//
// A PROMPT WOULD HAVE BEEN WRONG HERE. ember's exit codes are a contract with
// cron and CI -- `ember ls` is meant to run from a timer -- and a command that
// blocks on stdin hangs there forever instead of answering. A flag the
// operator typed is the same evidence of intent with none of that.

// allRows is the limit a command passes when it has to consider the whole
// history rather than a page of it.
//
// `ember history` defaults to the newest 200 because an unbounded listing is
// not an answer to "what have I run". A prune bounded that way would silently
// skip everything older than the two hundredth newest request -- which is
// exactly the set it was asked to remove -- so the page size is wrong here in
// the one direction that matters, and it is spelled out rather than left to a
// zero that means "default".
const allRows = 1 << 30

// historyOpen is the config, the paths and the store, which all three of these
// need and `ember history` itself already assembles the same way.
func historyOpen(cfgPath string, d deps) (config.Config, app.Paths, *genstore.SQLite, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.Config{}, app.Paths{}, nil, err
	}
	ps, err := app.ResolvePaths(cfg, d)
	if err != nil {
		return config.Config{}, app.Paths{}, nil, err
	}
	store, err := genstore.Open(ps.State)
	if err != nil {
		return config.Config{}, app.Paths{}, nil, err
	}
	return cfg, ps, store, nil
}

// runHistoryPrune drops the per-step events of finished requests.
//
// THE EVENTS ARE THE ONLY THING HERE THAT MAY GO BY AGE, and the argument is
// in internal/genstore/prune.go: the execution columns #32 added are a derived
// index of these rows, so a prune costs the replay and not a figure. Every
// number `ember spend` and `ember history` print is computed from columns on
// requests and jobs, which this cannot touch.
//
// What it does cost is worth saying out loud, because "a progress log for a
// client that is no longer connected" undersells it: the event stream is the
// store's own truth and the columns are the index. Anything a future ember
// wants to derive that today's columns do not hold has to come from these
// rows, and nothing backfills them.
func runHistoryPrune(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("history prune", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cfgPath   = fs.String("config", config.FileName, "path to ember.yaml")
		olderThan = fs.Duration("older-than", 0,
			"only events of requests that finished longer ago than this; defaults to history.keep_events")
		dryRun = fs.Bool("dry-run", false, "say how many would go and remove nothing")
		yes    = fs.Bool("yes", false, "actually remove them")
	)
	fs.Usage = func() {
		outf(out, "ember history prune drops the per-step events of finished requests.\n\n")
		outf(out, "Events are the bulk of the database and the one part of it that may go by\nage: a batch at 28 steps emits rows per step per job per pass, and passes\nrestart for each batch item and each highres pass. Every figure `ember\nspend` and `ember history` report comes from columns on the request and\nits jobs, so a prune changes no number either of them prints.\n\n")
		outf(out, "What it costs is the replay. The event stream is the record ember treats\nas the truth and those columns as an index of, so anything not already in\na column is gone with the rows, and nothing backfills them.\n\n")
		outf(out, "A request that has not finished is never touched, whatever the horizon:\nthe bound is measured from when a request ENDED, so a queued or running\none has nothing to compare and a browser streaming its progress cannot\nhave the rows pulled out from under it.\n\n")
		outf(out, "Nothing runs this for you. The only process that could is `ember serve`,\nwhich is the process that writes the events -- a prune on its startup\nwould delete the replay of the run you came back to look at, with no\nconfirmation and no output you would ever see.\n\n")
		outf(out, "usage: ember history prune [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember history prune takes no arguments and got %d (%v); "+
			"the horizon is --older-than", fs.NArg(), fs.Args())}
	}
	if *olderThan < 0 {
		// Refused rather than clamped, as `ember store abort` refuses a
		// negative --older-than: it reads as "no bound", and "ended before now
		// plus a month" selects every request that has ever finished.
		return usageError{fmt.Errorf("--older-than %s is negative; it is a minimum age, and a negative "+
			"one would select every request that has ever finished", *olderThan)}
	}

	cfg, _, store, err := historyOpen(*cfgPath, d)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	horizon := *olderThan
	if horizon == 0 {
		// The config is the default and the flag overrides it, which is the
		// precedence `ember up`'s applyFlags uses: the person typing the flag
		// is the person who wrote the file.
		if horizon, err = cfg.KeepEvents(); err != nil {
			return usageError{err}
		}
	}
	if horizon == 0 {
		return usageError{fmt.Errorf("no horizon: pass --older-than, or set history.keep_events in %s; "+
			"neither is set, which means keep every event forever", *cfgPath)}
	}

	ctx := context.Background()
	// Computed here and passed down as an instant, so the subtraction -- the
	// one place a sign or a unit can go wrong -- happens once and the store
	// takes an unambiguous bound rather than a duration it has to interpret.
	before := time.Now().Add(-horizon)

	n, err := store.PruneEvents(ctx, before, true)
	if err != nil {
		return err
	}
	if n == 0 {
		outf(out, "no events from requests that finished before %s.\n", stamp(before))
		return nil
	}

	// Said once, and only on the paths that do not go on to remove anything.
	// The first version printed the count and then "removed <the same
	// count>", which is two lines asserting one fact and reads as two
	// different numbers that happen to agree.
	if *dryRun || !*yes {
		outf(out, "%d event %s %s to requests that finished before %s.\n",
			n, plural(n, "row", "rows"), plural(n, "belongs", "belong"), stamp(before))
	}
	switch {
	case *dryRun:
		// Checked before --yes, so `--yes --dry-run` removes nothing: a flag
		// that is sometimes a dry run is worse than none.
		outf(out, "dry-run: nothing was removed.\n")
		return nil
	case !*yes:
		return usageError{errors.New("add --yes to remove them; the request rows, their costs and " +
			"their images are kept either way, and what goes is the per-step replay")}
	}

	removed, err := store.PruneEvents(ctx, before, false)
	if err != nil {
		return err
	}
	outf(out, "removed %d event %s from requests that finished before %s.\n",
		removed, plural(removed, "row", "rows"), stamp(before))
	return vacuum(ctx, out, store, removed)
}

// vacuum returns the freed pages to the filesystem, when there are any.
//
// The guard is inside genstore.Vacuum rather than here, so this cannot be the
// call site that forgot it; what this adds is saying so, because a command
// that rewrote a large file and said nothing looks like a hang.
func vacuum(ctx context.Context, out io.Writer, store *genstore.SQLite, removed int) error {
	ran, err := store.Vacuum(ctx, removed)
	if err != nil {
		return err
	}
	if ran {
		outf(out, "compacted %s.\n", store.Path())
	}
	return nil
}

// doomed is one request `ember history rm` would strip, and what that frees.
type doomed struct {
	Request genstore.ReqID `json:"request"`
	Images  int            `json:"images"`
	Bytes   int64          `json:"bytes"`
}

// rmReport is what --json prints.
//
// Removed is a field rather than implied by the exit status, because the three
// outcomes an operator scripts against -- it went, it would have gone, it was
// refused for want of --yes -- are otherwise two of them looking identical in
// the body.
type rmReport struct {
	Removed  bool     `json:"removed"`
	DryRun   bool     `json:"dry_run"`
	Requests []doomed `json:"requests"`
	Images   int      `json:"images"`
	Bytes    int64    `json:"bytes"`
}

// runHistoryRm removes a request's generated images from disk.
//
// AND KEEPS THE REQUEST. #28 asked for the row to go too, cascading, and that
// is the one part of it this does not do -- the argument is in
// internal/genstore/prune.go: requests.cost is the only record anywhere that a
// rental happened, `ember spend` is a SUM over it, and a deleted row makes the
// period total quietly smaller than the invoice with no footnote available.
// Every other way of being short of history here announces itself.
//
// So what this removes is the thing that is actually large and actually the
// reason to prune: the PNGs, and the rows that point at them. The ledger's
// image and byte counts come from jobs.images and jobs.bytes, not from the
// images table, so `ember history` reports the same figures after this ran as
// before -- and `ember history du` counts the requests in that state, because
// "produced six images, has none" and "produced none" must not read alike.
//
// A REQUEST ID IS THE KEY TO A PATH, which is why nothing here builds one.
// Every byte removed is removed through blob.Store under the key the database
// holds, which is rooted at the output directory and validated component by
// component -- containment twice, by two mechanisms, as internal/transfer
// does it for a tar member. There is no filepath.Join on a request id in this
// function.
func runHistoryRm(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("history rm", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cfgPath   = fs.String("config", config.FileName, "path to ember.yaml")
		olderThan = fs.Duration("older-than", 0, "every request that finished longer ago than this")
		dryRun    = fs.Bool("dry-run", false, "say what would go and remove nothing")
		yes       = fs.Bool("yes", false, "actually remove them")
		asJSON    = fs.Bool("json", false, "print the report as JSON")
	)
	fs.Usage = func() {
		outf(out, "ember history rm removes a request's generated images from disk.\n\n")
		outf(out, "IT KEEPS THE REQUEST. The row, its jobs, what the rental cost and what\neach job cost all stay, because they are the only record that the money\nwas spent -- `ember spend` is a SUM over them, and a deleted row would\nmake a period total quietly smaller than the invoice. What goes is the\npictures and the rows that point at them.\n\n")
		outf(out, "So `ember history` keeps reporting how many images the request produced\nand what they weighed: those counts come from the job rows, not from the\nfiles. `ember history du` says how many requests are in that state.\n\n")
		outf(out, "A generated image is not recoverable, which is why this takes ids you\ntyped or an explicit --older-than, prints what it would take, and then\nrefuses until --yes. A request that has not finished is never touched.\n\n")
		outf(out, "usage: ember history rm <id>... [flags]\n       ember history rm --older-than 720h [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if *olderThan < 0 {
		return usageError{fmt.Errorf("--older-than %s is negative; it is a minimum age, and a negative "+
			"one would select every request that has ever finished", *olderThan)}
	}
	// Exclusive in both directions, not merged. An id names one request and
	// --older-than names a set; accepting both would have to mean the union,
	// which is "remove these, and also everything older than that" -- and the
	// operator who typed both almost certainly meant "remove this one, if it
	// is older than that". Guessing between those costs images.
	switch {
	case fs.NArg() == 0 && *olderThan == 0:
		return usageError{errors.New("ember history rm needs request ids or --older-than; " +
			"with neither there is nothing to remove, and it does not default to everything")}
	case fs.NArg() > 0 && *olderThan != 0:
		return usageError{fmt.Errorf("ember history rm takes ids or --older-than and got both "+
			"(%v and %s); they would have to mean the union, which is not what either reads like",
			fs.Args(), *olderThan)}
	}

	_, ps, store, err := historyOpen(*cfgPath, d)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	targets, err := rmTargets(ctx, store, fs.Args(), *olderThan)
	if err != nil {
		return err
	}

	// Read before anything is touched, so the figure printed is the figure
	// removed rather than a guess made from the job columns -- which count
	// what a run PRODUCED and keep counting it after a previous rm.
	var (
		plan  []doomed
		keys  = map[genstore.ReqID][]string{}
		total doomed
	)
	for _, id := range targets {
		ims, err := store.Images(ctx, id)
		if err != nil {
			return err
		}
		if len(ims) == 0 {
			continue
		}
		entry := doomed{Request: id, Images: len(ims)}
		for _, im := range ims {
			entry.Bytes += im.Bytes
			keys[id] = append(keys[id], im.Key)
		}
		plan = append(plan, entry)
		total.Images += entry.Images
		total.Bytes += entry.Bytes
	}

	report := rmReport{DryRun: *dryRun, Requests: plan, Images: total.Images, Bytes: total.Bytes}
	if len(plan) == 0 {
		// Not a refusal and not exit 3. The operator asked for these to be
		// gone and they are, which is the contract destroy has everywhere
		// else here -- and a request whose images were already removed is the
		// ordinary way to arrive at this line twice.
		if *asJSON {
			return writeRmJSON(out, report)
		}
		outf(out, "nothing to remove: %d %s matched and %s no image on disk.\n",
			len(targets), plural(len(targets), "request", "requests"),
			plural(len(targets), "it has", "none of them has"))
		return nil
	}

	if !*asJSON {
		for _, p := range plan {
			outf(out, "  %-16s %3d image(s)  %10s\n", p.Request, p.Images, human(p.Bytes))
		}
		outf(out, "%d request(s), %d image(s), %s\n", len(plan), total.Images, human(total.Bytes))
	}

	switch {
	case *dryRun:
		// Before --yes is even looked at, so `--yes --dry-run` removes
		// nothing: a flag that is sometimes a dry run is worse than none.
		if *asJSON {
			return writeRmJSON(out, report)
		}
		outf(out, "dry-run: nothing was removed.\n")
		return nil
	case !*yes:
		if *asJSON {
			if err := writeRmJSON(out, report); err != nil {
				return err
			}
		}
		return usageError{errors.New("add --yes to remove them; a generated image is not recoverable, " +
			"and the costs and counts stay either way")}
	}

	blobs, err := blob.OpenDir(ps.Outputs)
	if err != nil {
		return err
	}
	defer func() { _ = blobs.Close() }()

	rows := 0
	for _, p := range plan {
		// THE BYTES GO BEFORE THE ROW. A row removed while its file is still
		// there leaves bytes nothing in ember can name -- not `du`, which
		// prices the tree against these rows, and not a second rm, which has
		// nothing left to look up. That is the shape of the abandoned
		// multipart upload, the one leak here that billed with no detection
		// path. The other order is the harmless half: the row says what is
		// missing and running this again finishes it.
		for _, key := range keys[p.Request] {
			if err := blobs.Delete(ctx, key); err != nil {
				return err
			}
		}
		n, err := store.DropImages(ctx, p.Request)
		if err != nil {
			return err
		}
		rows += n
	}

	report.Removed = true
	if *asJSON {
		if err := writeRmJSON(out, report); err != nil {
			return err
		}
		// No compaction line in JSON, which would not be part of the
		// document; the pages are still returned.
		_, err := store.Vacuum(ctx, rows)
		return err
	}
	outf(out, "removed %d image(s), %s, and %d row(s).\n", total.Images, human(total.Bytes), rows)
	return vacuum(ctx, out, store, rows)
}

func writeRmJSON(out io.Writer, r rmReport) error {
	if r.Requests == nil {
		// An empty array and not null, for the reason the websocket's first
		// envelope sends one: null is not an empty list, and a consumer that
		// iterates fails on it.
		r.Requests = []doomed{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// rmTargets resolves what was asked for into request ids.
//
// An id that names nothing is an error rather than a skip: the operator typed
// it, so the likely cause is a typo or the wrong half of an id, and silently
// removing the images of the other three while saying nothing about the fourth
// is how somebody concludes it worked.
//
// A request that has not finished is refused on the same reasoning and a
// stronger one -- its images are still being written, and a job that is three
// of eight images in would have the first three taken out from under the run
// producing the rest.
func rmTargets(ctx context.Context, store genstore.Store, ids []string, olderThan time.Duration) ([]genstore.ReqID, error) {
	if len(ids) > 0 {
		out := make([]genstore.ReqID, 0, len(ids))
		for _, raw := range ids {
			id := genstore.ReqID(raw)
			r, err := store.Get(ctx, id)
			if errors.Is(err, genstore.ErrNotFound) {
				return nil, fmt.Errorf("no request %s in the history; `ember history` lists them", id)
			}
			if err != nil {
				return nil, err
			}
			if !r.Status.Terminal() {
				return nil, fmt.Errorf("request %s is %s, so its images may still be being written; "+
					"wait for it to finish or cancel it", id, r.Status)
			}
			out = append(out, id)
		}
		return out, nil
	}

	before := time.Now().Add(-olderThan)
	// Until is a sound narrowing and not the test itself: a request cannot end
	// before it was created, so everything that FINISHED before the cut was
	// also created before it -- and a request created in March and finished
	// yesterday must not match a thirty-day horizon, which is why the end is
	// then checked in full below.
	rows, err := store.History(ctx, genstore.Filter{Until: before, Limit: allRows})
	if err != nil {
		return nil, err
	}
	var out []genstore.ReqID
	for _, e := range rows {
		if e.Ended.IsZero() || !e.Ended.Before(before) {
			continue
		}
		out = append(out, e.ID)
	}
	return out, nil
}

// runHistoryDu says what the history occupies.
//
// AN OPERATOR CANNOT DECIDE TO PRUNE SOMETHING THEY CANNOT MEASURE, which is
// #28's own line and the whole reason this exists beside the two verbs. The
// split is the one that matters: the database holds the rows, the output tree
// holds the pictures, and they are pruned by different commands with different
// arguments about whether they may go at all.
//
// THE THIRD FIGURE IS THE ONE THAT WOULD OTHERWISE MISLEAD. Part of the output
// tree is named by no image row, because `ember up` writes into the same
// directory and records nothing in the database -- "`ember up` prints its cost
// and keeps nothing", as `ember history`'s own usage says. Without that line
// an operator reads one total, removes every request's images, frees a
// fraction of it and concludes ember is broken.
func runHistoryDu(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("history du", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cfgPath = fs.String("config", config.FileName, "path to ember.yaml")
		asJSON  = fs.Bool("json", false, "print the figures as JSON")
	)
	fs.Usage = func() {
		outf(out, "ember history du says what the generation history occupies.\n\n")
		outf(out, "Split into the database, which holds the rows, and the output tree, which\nholds the images -- they are removed by different commands, and only one\nof the two is removable by age at all.\n\n")
		outf(out, "The database figure includes SQLite's write-ahead log, which is disk in\nuse and can be larger than the file beside it: a figure that counted only\ngenerations.db would report a fraction of what is there and would drop\nwhenever a checkpoint happened, for no reason you could see.\n\n")
		outf(out, "Part of the output tree is named by no row in the database: `ember up`\nwrites into the same directory and records nothing, so `ember history rm`\ncan never reach it. That figure is printed rather than folded in.\n\n")
		outf(out, "usage: ember history du [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember history du takes no arguments and got %d (%v)",
			fs.NArg(), fs.Args())}
	}

	cfg, ps, store, err := historyOpen(*cfgPath, d)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	u, err := store.Usage(ctx)
	if err != nil {
		return err
	}
	dbBytes, err := dbSize(store.Path())
	if err != nil {
		return err
	}
	treeBytes, files, err := treeSize(ps.Outputs)
	if err != nil {
		return err
	}

	// Asked for even though prune is what applies it, because "what would a
	// prune free" is the question du is being read to answer. Zero horizon
	// means the key is unset, and then there is nothing to report rather than
	// a count against a horizon nobody chose.
	prunable := 0
	horizon, err := cfg.KeepEvents()
	if err != nil {
		return err
	}
	if horizon > 0 {
		if prunable, err = store.PruneEvents(ctx, time.Now().Add(-horizon), true); err != nil {
			return err
		}
	}

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			genstore.Usage
			Database        string        `json:"database"`
			DatabaseBytes   int64         `json:"database_bytes"`
			Outputs         string        `json:"outputs"`
			OutputBytes     int64         `json:"output_bytes"`
			OutputFiles     int           `json:"output_files"`
			UnrecordedBytes int64         `json:"unrecorded_bytes"`
			KeepEvents      time.Duration `json:"keep_events_ns,omitempty"`
			PrunableEvents  int           `json:"prunable_events,omitempty"`
		}{
			Usage:         u,
			Database:      store.Path(),
			DatabaseBytes: dbBytes,
			Outputs:       ps.Outputs, OutputBytes: treeBytes, OutputFiles: files,
			UnrecordedBytes: unrecordedBytes(treeBytes, u.ImageBytes),
			KeepEvents:      horizon, PrunableEvents: prunable,
		})
	}

	outf(out, "database  %-10s  %s\n", human(dbBytes), store.Path())
	outf(out, "          %d request(s), %d job(s), %d event(s), %d image row(s) naming %s\n",
		u.Requests, u.Jobs, u.Events, u.Images, human(u.ImageBytes))
	outf(out, "outputs   %-10s  %s\n", human(treeBytes), ps.Outputs)
	outf(out, "          %d file(s)\n", files)

	if extra := unrecordedBytes(treeBytes, u.ImageBytes); extra > 0 {
		outf(out, "\n%s under outputs is named by no image row. `ember up` writes into this\n"+
			"tree and records nothing, so `ember history rm` can never reach it.\n", human(extra))
	}
	// The plural rule these lines follow, which the first version did not: a
	// count in a LIST of counts or a table column keeps "(s)", because the
	// column width must not move and there is no verb to agree with. A count
	// followed by a verb goes through plural, or the line reads "1 request(s)
	// ... have none" -- which is the pluralisation defect this repository has
	// already shipped once, found by reading the output rather than by a test.
	if u.Reclaimed > 0 {
		outf(out, "\n%d finished %s produced images and %s none on disk, which is what\n"+
			"`ember history rm` leaves: the costs and the counts stayed and the pictures\nare gone.\n",
			u.Reclaimed, plural(u.Reclaimed, "request", "requests"),
			plural(u.Reclaimed, "has", "have"))
	}
	// Only when there is something to say. The first version printed it
	// whenever the key was set, so a history with nothing prunable reported
	// "0 event rows are older than history.keep_events" and pointed at a
	// command -- which reads as a finding and is the way an operator learns to
	// ignore the lines under the table. Found by reading the output.
	//
	// No pronoun at the end, deliberately: "removes them" against "removes it"
	// is a third agreement to keep in step, for a clause that adds nothing the
	// command name does not.
	if prunable > 0 {
		outf(out, "\n%d event %s %s older than history.keep_events (%s);\nsee `ember history prune`.\n",
			prunable, plural(prunable, "row", "rows"), plural(prunable, "is", "are"), horizon)
	}
	return nil
}

// unrecordedBytes is the output tree less what the image rows name.
//
// Never negative. The rows are a claim about files this process has not
// opened, so a file shortened or removed behind ember's back makes the claim
// exceed the tree -- and a negative "not recorded anywhere" figure would read
// as ember having found bytes that do not exist.
func unrecordedBytes(tree, named int64) int64 {
	if tree <= named {
		return 0
	}
	return tree - named
}

// dbSize is the database and the files SQLite keeps beside it.
//
// THE WAL IS PART OF THE ANSWER. The store opens with journal_mode(WAL), so a
// freshly written database is a small main file and a write-ahead log that can
// be larger than it -- a figure that counted only generations.db would report
// a fraction of the disk in use and would drop whenever a checkpoint happened,
// for no reason an operator could see. A missing one is zero rather than an
// error: there is no -shm until a reader attaches.
func dbSize(path string) (int64, error) {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("measure %s: %w", p, err)
		}
		total += st.Size()
	}
	return total, nil
}

// treeSize is the bytes and the files under dir.
//
// A tree that is not there is zero and not an error: the output directory is
// created by the first run that fetches something, and `ember history du` on a
// machine that has only queued requests is a reasonable thing to type.
//
// Directories themselves are not counted. Their own size is a filesystem
// detail -- 4 KiB an inode on ext4, nothing on others -- and the question
// being asked is how much of this is the images.
func treeSize(dir string) (int64, int, error) {
	var (
		total int64
		files int
	)
	err := filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		files++
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("measure %s: %w", dir, err)
	}
	return total, files, nil
}
