package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
)

// longAgo is a fixed distance into the past rather than a fixed date.
//
// spendDay's instants are a calendar date, which is right for a ledger test
// asserting on a printed day and WRONG for every assertion about a retention
// horizon: 2026-11-01 is a day old while this is being written and will be a
// year old later, so a test built on it passes today and starts answering a
// different question every morning. Measured against now, "older than 24h" is
// a property of the seed rather than of the calendar.
func longAgo() time.Time { return time.Now().Add(-90 * 24 * time.Hour) }

// outputsOf is the directory the images actually land in, so a test can look
// at the tree rather than only at the rows.
func outputsOf(t *testing.T, cfgPath string, d deps) string {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := app.ResolvePaths(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	return ps.Outputs
}

// generation seeds one finished request with events, job figures, image rows
// AND the files those rows name, under the key the queue would have used:
// "<request>/<stem>-NN.png", relative to the output directory.
//
// The files are written for real because half of what `ember history rm` and
// `du` do is on the filesystem, and a test that seeded only rows would assert
// that ember agreed with itself about bytes nothing had ever written -- the
// closed loop AGENTS.md names three times.
func generation(t *testing.T, cfgPath string, d deps, st genstore.Store,
	created, ended time.Time, images int, size int) genstore.ReqID {
	t.Helper()

	r := genstore.Request{Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: created}
	r.Jobs = append(r.Jobs, genstore.Job{Prompt: "a cat", Width: 1024, Height: 1024, Steps: 28})
	id, err := st.Enqueue(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(t.Context(), id, func(g *genstore.Request) {
		g.Status, g.RunID, g.Node = genstore.StatusDone, "run-"+string(id), "node-1"
		g.Started, g.Ended = created, ended
		g.Cost, g.Rate, g.Elapsed, g.Billable = 890_000, 890_000, time.Hour, time.Hour
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJob(t.Context(), id, 1, func(j *genstore.JobRun) {
		j.Started, j.Ended = created, ended
		j.Cost, j.Images, j.Bytes = 380_000, images, int64(images*size)
		j.Passes, j.Steps = 1, 28
	}); err != nil {
		t.Fatal(err)
	}
	// Two events per image, which is how this table gets to be the bulk.
	var es []report.Event
	for i := range images * 2 {
		es = append(es, report.Event{Seq: int64(i + 1), Kind: report.KindLog, Text: "step\n"})
	}
	if err := st.AppendEvents(t.Context(), id, es); err != nil {
		t.Fatal(err)
	}

	blobs, err := blob.OpenDir(outputsOf(t, cfgPath, d))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blobs.Close() }()
	for i := range images {
		key := string(id) + "/cat-0" + string(rune('0'+i)) + ".png"
		if err := blobs.Put(t.Context(), key, bytes.Repeat([]byte{'p'}, size)); err != nil {
			t.Fatal(err)
		}
		if err := st.AddImage(t.Context(), genstore.Image{
			Req: id, Job: 1, Index: i, Key: key, Bytes: int64(size), At: ended,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// TestHistoryDu_SplitsTheDatabaseFromTheTreeAndNamesWhatIsInNeither.
//
// The third figure is the one that would otherwise mislead: `ember up` writes
// into the same output directory and records nothing, so an operator reading
// one total, removing every request's images and freeing a fraction of it
// would conclude ember was broken.
func TestHistoryDu_SplitsTheDatabaseFromTheTreeAndNamesWhatIsInNeither(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 3, 400_000)
	})

	// An `ember up` run directory: real bytes in the tree that no row names.
	// transfer.RunDir's own shape, timestamp first and the run id after it.
	up := filepath.Join(outputsOf(t, cfg, d), "20261101-120000-run-xyz")
	if err := os.MkdirAll(up, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(up, "out.png"), bytes.Repeat([]byte{'u'}, 900_000), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runHistoryDu([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history du = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history du:\n%s", got)

	for _, want := range []string{
		"database", "outputs",
		"1 request(s), 1 job(s), 6 event(s), 3 image row(s)",
		"1.1 MiB", // the three 400 kB images the rows name
		"named by no image row",
		"878.9 KiB", // the `ember up` directory nothing recorded
		"4 file(s)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("du does not report %q:\n%s", want, got)
		}
	}
}

// TestHistoryDu_ReportsNothingAgainstAnEmptyHistory, which is the first thing
// anybody types it for. The output tree does not exist yet either.
func TestHistoryDu_ReportsNothingAgainstAnEmptyHistory(t *testing.T) {
	d := labDeps(t)
	// With the horizon set, so the prune footnote has every reason to fire and
	// must not: "0 event rows are older than history.keep_events" over an
	// empty database reads as a finding, and a line like that is how an
	// operator learns to ignore everything under the table.
	cfg := storeConfig(t, d, "history:\n  keep_events: 720h\n")

	var out bytes.Buffer
	if err := runHistoryDu([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history du = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history du on an empty history:\n%s", got)

	if !strings.Contains(got, "0 request(s), 0 job(s), 0 event(s), 0 image row(s)") {
		t.Errorf("du on an empty store said %q", got)
	}
	// And no footnotes at all, because there is nothing in either place and
	// nothing was reclaimed. A footnote over zero is a finding nobody can
	// act on.
	for _, unwanted := range []string{
		"named by no image row",
		"produced images and ha",
		"history.keep_events",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("du reports %q against an empty history:\n%s", unwanted, got)
		}
	}
}

// TestHistoryDu_ReportsWhatARemovalLeftBehind.
//
// "produced three images, has none on disk" and "produced none" must not read
// alike, and after `ember history rm` the first is exactly what a row looks
// like. The ledger keeps saying three, which is the point of keeping the rows,
// so something has to say the files are gone.
func TestHistoryDu_ReportsWhatARemovalLeftBehind(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 3, 1000)
	})

	var rm bytes.Buffer
	if err := runHistoryRm([]string{"-config", cfg, "-yes", string(id)}, &rm, d); err != nil {
		t.Fatalf("history rm = %v\n%s", err, rm.String())
	}

	var out bytes.Buffer
	if err := runHistoryDu([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Logf("ember history du after a removal:\n%s", got)
	if !strings.Contains(got, "1 finished request produced images and has none on disk") {
		t.Errorf("du does not say what the removal left:\n%s", got)
	}
}

// TestDuSize_CountsTheWriteAheadLogAndNotAMissingOne.
//
// A UNIT TEST ON THE THREE NAMES, because the integrated one cannot see this:
// the write-ahead log exists only while a connection is open, so by the time a
// test can stat the directory the checkpoint has folded it back into the main
// file and a figure that had dropped it looks right. The reinstatement pass
// found exactly that -- removing "-wal" and "-shm" from the list broke
// nothing.
//
// Both halves. The log is disk in use and can be larger than the file beside
// it, so a figure that counted only generations.db would report a fraction of
// what is there and would drop whenever a checkpoint happened, for no reason
// an operator could see; and a name that is not there is zero rather than an
// error, because there is no -shm until a reader attaches.
func TestDuSize_CountsTheWriteAheadLogAndNotAMissingOne(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "generations.db")
	if err := os.WriteFile(db, bytes.Repeat([]byte{'d'}, 1000), 0o600); err != nil {
		t.Fatal(err)
	}

	// Main file only: no log yet.
	n, err := dbSize(db)
	if err != nil || n != 1000 {
		t.Fatalf("dbSize = %d, %v; want 1000 with no log beside it", n, err)
	}

	if err := os.WriteFile(db+"-wal", bytes.Repeat([]byte{'w'}, 4000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-shm", bytes.Repeat([]byte{'s'}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err = dbSize(db); err != nil || n != 5032 {
		t.Errorf("dbSize = %d, %v; want 5032 -- the log is disk in use and here it is "+
			"four times the file beside it", n, err)
	}

	// A path that is not there at all is an error, which is different from a
	// log that has not been created: `ember history du` opens the store
	// first, so a missing database means something else is wrong.
	if _, err := dbSize(filepath.Join(dir, "nope.db")); err != nil {
		t.Errorf("a database that is not there = %v; a missing file is zero, as a missing log is", err)
	}
}

// TestHistoryRm_OlderThanIsADurationAndNotANumberOfDays.
//
// A horizon of 24h against a request that finished two days ago. The seeds in
// the test above are ninety days old, so every plausible misreading of the
// unit -- times 24, times 60 -- still selects them and the reinstatement pass
// found that: multiplying the duration by 24 broke nothing. Two days is
// inside 24 days and outside 24 hours, which is what makes the unit decisive.
func TestHistoryRm_OlderThanIsADurationAndNotANumberOfDays(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st,
			time.Now().Add(-3*24*time.Hour), time.Now().Add(-2*24*time.Hour), 1, 1000)
	})

	var out bytes.Buffer
	if err := runHistoryRm([]string{"-config", cfg, "-older-than", "24h", "-yes"}, &out, d); err != nil {
		t.Fatalf("rm = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), string(id)) {
		t.Errorf("a request that finished two days ago was not older than 24h:\n%s", out.String())
	}
}

// TestHistoryRm_OlderThanReachesEveryRequestAndNotOnlyAPage.
//
// `ember history` defaults to the newest 200 rows, because an unbounded
// listing is not an answer to "what have I run". A prune bounded that way
// skips everything older than the two hundredth newest request -- which is
// exactly the set it was asked to remove -- and says nothing, so the operator
// reads "200 requests" and believes it finished. A silently incomplete
// listing is the same defect `ScanStores` follows its pagination to the end
// to avoid, and the reinstatement pass found that nothing noticed: dropping
// the explicit limit broke no test, because every other case here seeds three
// requests.
//
// 201 of them, seeded as thinly as the assertion allows -- a row and an image
// row each, no events and no files -- because what is under test is the bound
// and not the bytes.
func TestHistoryRm_OlderThanReachesEveryRequestAndNotOnlyAPage(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	const n = 201
	var oldest genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		for i := range n {
			// Oldest first, so the request this must reach is the one a page
			// of the newest rows would leave out -- and every one of them a
			// clear two days old, so the count is 201 and not 200. The first
			// version put the newest one a day back, which ends inside a 24h
			// horizon and is correctly excluded: the expected answer was then
			// 200, the same number the bug produces, and the assertion could
			// not have failed.
			created := time.Now().Add(-time.Duration(n-i+1) * 24 * time.Hour)
			r := genstore.Request{Provider: "runpod", GPU: "rtx4090", TTL: time.Hour,
				Created: created, Jobs: []genstore.Job{{Prompt: "a cat"}}}
			id, err := st.Enqueue(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Update(t.Context(), id, func(g *genstore.Request) {
				g.Status, g.Started, g.Ended = genstore.StatusDone, created, created.Add(time.Minute)
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.AddImage(t.Context(), genstore.Image{
				Req: id, Job: 1, Index: 0, Key: string(id) + "/cat-00.png", Bytes: 1, At: created,
			}); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				oldest = id
			}
		}
	})

	var out bytes.Buffer
	if err := runHistoryRm([]string{"-config", cfg, "-older-than", "24h", "-dry-run"}, &out, d); err != nil {
		t.Fatalf("rm = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), string(oldest)) {
		t.Errorf("the oldest of %d requests was outside the page `rm` looked at", n)
	}
	if !strings.Contains(out.String(), "201 request(s)") {
		t.Errorf("rm considered a page rather than the history:\n%s",
			out.String()[max(0, len(out.String())-200):])
	}
}

// TestHistoryDu_JSONCarriesTheSameFigures, so a script does not have to parse
// the table.
func TestHistoryDu_JSONCarriesTheSameFigures(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "history:\n  keep_events: 720h\n")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 2, 1000)
	})

	var out bytes.Buffer
	if err := runHistoryDu([]string{"-config", cfg, "-json"}, &out, d); err != nil {
		t.Fatalf("history du --json = %v\n%s", err, out.String())
	}
	t.Logf("ember history du --json:\n%s", out.String())

	var got struct {
		Requests       int   `json:"requests"`
		Events         int   `json:"events"`
		Images         int   `json:"images"`
		ImageBytes     int64 `json:"image_bytes"`
		OutputBytes    int64 `json:"output_bytes"`
		PrunableEvents int   `json:"prunable_events"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("du --json is not JSON: %v\n%s", err, out.String())
	}
	if got.Requests != 1 || got.Images != 2 || got.ImageBytes != 2000 || got.OutputBytes != 2000 {
		t.Errorf("du --json = %+v", got)
	}
	// The seed finished ninety days ago, so every event is past a 720h
	// horizon and `du` says so without being asked for a bound of its own.
	if got.Events != 4 || got.PrunableEvents != 4 {
		t.Errorf("du --json events = %d, prunable = %d; want 4 and 4", got.Events, got.PrunableEvents)
	}
}

// TestHistoryPrune_RefusesUntilYesAndThenKeepsEverythingElse.
//
// The three properties in one body, because they are one decision: it says
// what would go, it refuses with a USAGE error so a script cannot read the
// exit status as success, and when it does run the request, its cost, its
// jobs and its images are all still there.
func TestHistoryPrune_RefusesUntilYesAndThenKeepsEverythingElse(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 2, 1000)
	})

	var refused bytes.Buffer
	err := runHistoryPrune([]string{"-config", cfg, "-older-than", "24h"}, &refused, d)
	if err == nil {
		t.Fatal("prune removed events without --yes")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("the refusal exits %d, want %d; a script that forgot --yes must not read success",
			ExitCode(err), ExitUsage)
	}
	t.Logf("ember history prune without --yes:\n%s%s\n", refused.String(), "ember: "+err.Error())
	if !strings.Contains(refused.String(), "4 event rows belong") {
		t.Errorf("the refusal does not say what would go:\n%s", refused.String())
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("err = %v, want it to name the flag", err)
	}

	// Nothing went.
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		es, err := st.Events(t.Context(), id, 0)
		if err != nil || len(es) != 4 {
			t.Errorf("Events = %d, %v; the refusal removed them anyway", len(es), err)
		}
	})

	var out bytes.Buffer
	if err := runHistoryPrune([]string{"-config", cfg, "-older-than", "24h", "-yes"}, &out, d); err != nil {
		t.Fatalf("prune = %v\n%s", err, out.String())
	}
	t.Logf("ember history prune --yes:\n%s", out.String())
	for _, want := range []string{"removed 4 event rows", "compacted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("prune does not report %q:\n%s", want, out.String())
		}
	}

	// And everything that is not an event stayed. This is #28's own "done
	// when": a prune keeps every request, job, cost and image it was asked to
	// keep.
	var ledger bytes.Buffer
	if err := runHistory([]string{"-config", cfg, "-json"}, &ledger, d); err != nil {
		t.Fatal(err)
	}
	var rows []genstore.Entry
	if err := json.Unmarshal(ledger.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the ledger has %d rows after a prune, want 1", len(rows))
	}
	if rows[0].Cost != node.MicroUSD(890_000) || rows[0].Images != 2 || rows[0].Bytes != 2000 {
		t.Errorf("the prune changed the ledger row: %+v", rows[0])
	}
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		if ims, err := st.Images(t.Context(), id); err != nil || len(ims) != 2 {
			t.Errorf("Images = %d, %v; the prune took the images", len(ims), err)
		}
	})
}

// TestHistory_OneOfSomethingReadsAsOne.
//
// The first version of these lines said "1 event row(s) belong to" and
// "1 finished request(s) produced images and have none on disk" -- a count
// followed by a plural verb, which is the pluralisation defect this
// repository has already shipped once and which only reading the output
// finds. The rule the lines follow now: a count in a list of counts or a
// table column keeps "(s)", because the column width must not move and there
// is no verb to agree with, and a count followed by a verb goes through
// plural.
func TestHistory_OneOfSomethingReadsAsOne(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "history:\n  keep_events: 24h\n")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		// One image, so one event pair becomes... two. Trimmed to one below.
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 1, 10)
	})
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		// One event row exactly, which is the only way to reach the singular
		// branch of the sentence about rows.
		if _, err := st.PruneEvents(t.Context(), time.Now(), false); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendEvents(t.Context(), id, []report.Event{
			{Seq: 1, Kind: report.KindLog, Text: "one\n"},
		}); err != nil {
			t.Fatal(err)
		}
	})

	var prune bytes.Buffer
	err := runHistoryPrune([]string{"-config", cfg, "-older-than", "24h"}, &prune, d)
	if err == nil {
		t.Fatal("prune removed without --yes")
	}
	t.Logf("ember history prune, one row:\n%s", prune.String())
	if !strings.Contains(prune.String(), "1 event row belongs to") {
		t.Errorf("the singular reads wrong:\n%s", prune.String())
	}

	var rm bytes.Buffer
	if err := runHistoryRm([]string{"-config", cfg, "-yes", string(id)}, &rm, d); err != nil {
		t.Fatal(err)
	}
	rm.Reset()
	if err := runHistoryRm([]string{"-config", cfg, string(id)}, &rm, d); err != nil {
		t.Fatal(err)
	}
	t.Logf("ember history rm, one request already stripped:\n%s", rm.String())
	if !strings.Contains(rm.String(), "1 request matched and it has no image on disk") {
		t.Errorf("the singular reads wrong:\n%s", rm.String())
	}

	var du bytes.Buffer
	if err := runHistoryDu([]string{"-config", cfg}, &du, d); err != nil {
		t.Fatal(err)
	}
	t.Logf("ember history du, one of each:\n%s", du.String())
	for _, want := range []string{
		"1 finished request produced images and has none on disk",
		"1 event row is older than history.keep_events (24h0m0s);\nsee `ember history prune`.",
	} {
		if !strings.Contains(du.String(), want) {
			t.Errorf("du does not read %q:\n%s", want, du.String())
		}
	}
}

// TestHistoryPrune_ADryRunRemovesNothing, including when --yes is also given:
// a flag that is sometimes a dry run is worse than none.
func TestHistoryPrune_ADryRunRemovesNothing(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 2, 1000)
	})

	for _, args := range [][]string{
		{"-config", cfg, "-older-than", "24h", "-dry-run"},
		{"-config", cfg, "-older-than", "24h", "-dry-run", "-yes"},
	} {
		var out bytes.Buffer
		if err := runHistoryPrune(args, &out, d); err != nil {
			t.Fatalf("prune %v = %v\n%s", args, err, out.String())
		}
		t.Logf("ember history prune %v:\n%s", args, out.String())
		if !strings.Contains(out.String(), "dry-run: nothing was removed") {
			t.Errorf("prune %v did not say it was a dry run:\n%s", args, out.String())
		}
		seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
			es, err := st.Events(t.Context(), id, 0)
			if err != nil || len(es) != 4 {
				t.Errorf("prune %v removed %d of 4 events in a dry run (err %v)", args, 4-len(es), err)
			}
		})
	}
}

// TestHistoryPrune_NothingToDoDoesNotCompact.
//
// VACUUM rewrites the whole file, so running it after a prune that removed
// nothing makes the no-op case the expensive one on exactly the database large
// enough to be worth pruning. The guard is inside genstore.Vacuum; this is the
// command-level half of the same assertion.
func TestHistoryPrune_NothingToDoDoesNotCompact(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		// Finished a minute ago, so a 24h horizon reaches nothing.
		generation(t, cfg, d, st, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Minute), 1, 10)
	})

	var out bytes.Buffer
	if err := runHistoryPrune([]string{"-config", cfg, "-older-than", "24h", "-yes"}, &out, d); err != nil {
		t.Fatalf("prune = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history prune with nothing to do:\n%s", got)
	if !strings.Contains(got, "no events from requests that finished before") {
		t.Errorf("prune did not say there was nothing to do:\n%s", got)
	}
	if strings.Contains(got, "compacted") {
		t.Errorf("prune compacted the database after removing nothing:\n%s", got)
	}
}

// TestHistoryPrune_TheHorizonComesFromTheConfigAndTheFlagOverridesIt.
//
// The precedence `ember up`'s applyFlags uses: the person typing the flag is
// the person who wrote the file. With neither it refuses and names the key,
// because "keep forever" is the default and a prune with no horizon would
// otherwise have to invent one.
func TestHistoryPrune_TheHorizonComesFromTheConfigAndTheFlagOverridesIt(t *testing.T) {
	// One deps per case, deliberately. app.ResolvePaths lets Deps.HomeDir win
	// over the config's own state_dir, so two configs built from one labDeps
	// share a database -- and these three cases each seed one request and
	// count the rows it owns.
	seed := func(t *testing.T, extra string) (deps, string) {
		t.Helper()
		d := labDeps(t)
		cfg := storeConfig(t, d, extra)
		seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
			generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 1, 10)
		})
		return d, cfg
	}

	d, bare := seed(t, "")
	var out bytes.Buffer
	err := runHistoryPrune([]string{"-config", bare}, &out, d)
	if err == nil {
		t.Fatal("prune invented a horizon")
	}
	if ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "history.keep_events") {
		t.Errorf("err = %v (exit %d), want a usage error naming the config key", err, ExitCode(err))
	}

	// With the key set, the bare verb has a horizon.
	d, set := seed(t, "history:\n  keep_events: 24h\n")
	out.Reset()
	if err := runHistoryPrune([]string{"-config", set, "-yes"}, &out, d); err != nil {
		t.Fatalf("prune = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "removed 2 event rows") {
		t.Errorf("the configured horizon was not used:\n%s", out.String())
	}

	// And the flag beats it, in the direction that removes less: a horizon of
	// a hundred years reaches nothing.
	d, again := seed(t, "history:\n  keep_events: 24h\n")
	out.Reset()
	if err := runHistoryPrune([]string{"-config", again, "-older-than", "876000h", "-yes"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no events from requests") {
		t.Errorf("the flag did not override the config:\n%s", out.String())
	}
}

// TestHistoryPrune_RefusesWhatCannotBeMeant: a negative horizon, a horizon the
// config made negative, and an argument where a flag was meant.
func TestHistoryPrune_RefusesWhatCannotBeMeant(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	bad := storeConfig(t, d, "history:\n  keep_events: -720h\n")

	for _, c := range []struct {
		name, want string
		args       []string
	}{
		{"negative flag", "negative", []string{"-config", cfg, "-older-than", "-24h"}},
		{"negative config", "keep_events", []string{"-config", bad}},
		{"an argument", "takes no arguments", []string{"-config", cfg, "24h"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runHistoryPrune(c.args, &out, d)
			if err == nil {
				t.Fatalf("accepted %v", c.args)
			}
			if ExitCode(err) != ExitUsage {
				t.Errorf("exit %d, want %d", ExitCode(err), ExitUsage)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestHistoryRm_RemovesThePicturesAndKeepsTheMoney.
//
// THIS IS THE DECISION #28 TURNED ON. The request row, its jobs and every cost
// column stay, because requests.cost is the only record that a rental
// happened and `ember spend` is a SUM over it -- a deleted row would make a
// period total quietly smaller than the invoice with no footnote possible. So
// the ledger reports the same figures after this ran as before, and the
// pictures are gone.
func TestHistoryRm_RemovesThePicturesAndKeepsTheMoney(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 3, 400_000)
	})

	var out bytes.Buffer
	if err := runHistoryRm([]string{"-config", cfg, "-yes", string(id)}, &out, d); err != nil {
		t.Fatalf("history rm = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history rm --yes:\n%s", got)
	if !strings.Contains(got, "removed 3 image(s), 1.1 MiB, and 3 row(s)") {
		t.Errorf("rm does not say what it removed:\n%s", got)
	}

	// The files are gone, and so is the directory they were the only contents
	// of -- an inode per pruned request, left behind forever, in the tree
	// `du` measures.
	if _, err := os.Stat(filepath.Join(outputsOf(t, cfg, d), string(id))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the request's directory survived its last image: %v", err)
	}

	var ledger bytes.Buffer
	if err := runHistory([]string{"-config", cfg, "-json"}, &ledger, d); err != nil {
		t.Fatal(err)
	}
	var rows []genstore.Entry
	if err := json.Unmarshal(ledger.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the ledger has %d rows after rm, want the request kept", len(rows))
	}
	if rows[0].Cost != node.MicroUSD(890_000) {
		t.Errorf("the rental cost went with the pictures: %s", rows[0].Cost)
	}
	if rows[0].Images != 3 || rows[0].Bytes != 1_200_000 || rows[0].JobCost != node.MicroUSD(380_000) {
		t.Errorf("the ledger forgot what the request produced: %+v", rows[0])
	}

	// And `ember spend` still totals the rental.
	var spend bytes.Buffer
	if err := runSpend([]string{"-config", cfg}, &spend, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spend.String(), "$0.89") {
		t.Errorf("spend lost the rental:\n%s", spend.String())
	}
}

// TestHistoryRm_RefusesUntilYesAndSaysWhatWouldGo, with the usage exit status
// so a script that forgot the flag cannot read it as a removal.
func TestHistoryRm_RefusesUntilYesAndSaysWhatWouldGo(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 2, 1000)
	})

	var out bytes.Buffer
	err := runHistoryRm([]string{"-config", cfg, string(id)}, &out, d)
	if err == nil {
		t.Fatal("rm removed images without --yes")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit %d, want %d", ExitCode(err), ExitUsage)
	}
	t.Logf("ember history rm without --yes:\n%sember: %s\n", out.String(), err)
	for _, want := range []string{string(id), "2 image(s)", "1 request(s)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the refusal does not report %q:\n%s", want, out.String())
		}
	}

	// The images are still there, rows and bytes both.
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		if ims, err := st.Images(t.Context(), id); err != nil || len(ims) != 2 {
			t.Errorf("Images = %d, %v; the refusal removed them", len(ims), err)
		}
	})
	if _, err := os.Stat(filepath.Join(outputsOf(t, cfg, d), string(id), "cat-00.png")); err != nil {
		t.Errorf("the refusal removed a file: %v", err)
	}
}

// TestHistoryRm_ADryRunRemovesNothing, --yes or not.
func TestHistoryRm_ADryRunRemovesNothing(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 2, 1000)
	})

	for _, extra := range [][]string{{"-dry-run"}, {"-dry-run", "-yes"}} {
		args := append([]string{"-config", cfg}, extra...)
		args = append(args, string(id))
		var out bytes.Buffer
		if err := runHistoryRm(args, &out, d); err != nil {
			t.Fatalf("rm %v = %v\n%s", extra, err, out.String())
		}
		t.Logf("ember history rm %v:\n%s", extra, out.String())
		if !strings.Contains(out.String(), "dry-run: nothing was removed") {
			t.Errorf("rm %v did not say it was a dry run:\n%s", extra, out.String())
		}
		if _, err := os.Stat(filepath.Join(outputsOf(t, cfg, d), string(id), "cat-01.png")); err != nil {
			t.Errorf("rm %v removed a file: %v", extra, err)
		}
	}
}

// TestHistoryRm_OlderThanTakesTheFinishedOnesOnly.
//
// Two requests, one old and one from a minute ago, and a running one. Only the
// old one goes: the horizon is measured from when a request FINISHED, and a
// request still in flight is refused outright because its images are still
// being written.
func TestHistoryRm_OlderThanTakesTheFinishedOnesOnly(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var old, recent, live genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		old = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 1, 1000)
		recent = generation(t, cfg, d, st, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Minute), 1, 1000)

		r := genstore.Request{Provider: "runpod", GPU: "rtx4090", TTL: time.Hour,
			Created: spendDay(1), Jobs: []genstore.Job{{Prompt: "x"}}}
		id, err := st.Enqueue(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.ClaimNext(t.Context(), "run-live", genstore.Compat{}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddImage(t.Context(), genstore.Image{Req: id, Job: 1, Key: string(id) + "/x-00.png", Bytes: 1}); err != nil {
			t.Fatal(err)
		}
		live = id
	})

	var out bytes.Buffer
	if err := runHistoryRm([]string{"-config", cfg, "-older-than", "24h", "-yes"}, &out, d); err != nil {
		t.Fatalf("rm = %v\n%s", err, out.String())
	}
	got := out.String()
	t.Logf("ember history rm --older-than 24h --yes:\n%s", got)
	if !strings.Contains(got, string(old)) {
		t.Errorf("the old request was not removed:\n%s", got)
	}
	for _, kept := range []genstore.ReqID{recent, live} {
		if strings.Contains(got, string(kept)) {
			t.Errorf("%s was removed and should not have been:\n%s", kept, got)
		}
	}

	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		for _, c := range []struct {
			id   genstore.ReqID
			want int
		}{{old, 0}, {recent, 1}, {live, 1}} {
			ims, err := st.Images(t.Context(), c.id)
			if err != nil || len(ims) != c.want {
				t.Errorf("Images(%s) = %d, %v; want %d", c.id, len(ims), err, c.want)
			}
		}
	})
}

// TestHistoryRm_RefusesARequestThatHasNotFinished.
//
// Named rather than skipped: the operator typed the id, so the likely cause is
// the wrong one -- and its images are still being written, so taking the first
// three of eight out from under the run producing the rest is the concrete
// harm.
func TestHistoryRm_RefusesARequestThatHasNotFinished(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		r := genstore.Request{Provider: "runpod", TTL: time.Hour, Jobs: []genstore.Job{{Prompt: "x"}}}
		var err error
		if id, err = st.Enqueue(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.ClaimNext(t.Context(), "run-live", genstore.Compat{}); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	err := runHistoryRm([]string{"-config", cfg, "-yes", string(id)}, &out, d)
	if err == nil {
		t.Fatal("rm accepted a running request")
	}
	if !strings.Contains(err.Error(), "running") {
		t.Errorf("err = %v, want it to name the status", err)
	}
}

// TestHistoryRm_RefusesWhatCannotBeMeant.
func TestHistoryRm_RefusesWhatCannotBeMeant(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 1, 10)
	})

	for _, c := range []struct {
		name, want string
		args       []string
	}{
		{"neither", "needs request ids or --older-than", []string{"-config", cfg}},
		{"both", "and got both", []string{"-config", cfg, "-older-than", "24h", string(id)}},
		{"negative", "negative", []string{"-config", cfg, "-older-than", "-24h"}},
		{"no such request", "no request", []string{"-config", cfg, "-yes", "deadbeefdeadbeef"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runHistoryRm(c.args, &out, d)
			if err == nil {
				t.Fatalf("accepted %v", c.args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestHistoryRm_NothingToRemoveIsNotARefusal.
//
// The operator asked for these to be gone and they are, which is the contract
// destroy has everywhere else here -- and a request whose images were already
// removed is the ordinary way to arrive at this line twice. Exit 3 there would
// make a cron entry that runs this nightly red on every night after the first.
func TestHistoryRm_NothingToRemoveIsNotARefusal(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 1, 10)
	})

	var out bytes.Buffer
	if err := runHistoryRm([]string{"-config", cfg, "-yes", string(id)}, &out, d); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	// Again, with no --yes, which must not refuse either: there is nothing to
	// confirm.
	if err := runHistoryRm([]string{"-config", cfg, string(id)}, &out, d); err != nil {
		t.Fatalf("a second rm = %v\n%s", err, out.String())
	}
	t.Logf("ember history rm on a request already stripped:\n%s", out.String())
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Errorf("rm said %q", out.String())
	}
}

// TestHistoryRm_JSONSaysWhichOfTheThreeOutcomesItWas.
//
// Removed is a field rather than implied by the exit status, because "it
// went", "it would have gone" and "it was refused for want of --yes" are
// otherwise two outcomes with identical bodies.
func TestHistoryRm_JSONSaysWhichOfTheThreeOutcomesItWas(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 2, 1000)
	})

	type rep struct {
		Removed  bool `json:"removed"`
		DryRun   bool `json:"dry_run"`
		Images   int  `json:"images"`
		Requests []struct {
			Request string `json:"request"`
			Images  int    `json:"images"`
			Bytes   int64  `json:"bytes"`
		} `json:"requests"`
	}
	decode := func(t *testing.T, b []byte) rep {
		t.Helper()
		var r rep
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("rm --json is not JSON: %v\n%s", err, b)
		}
		return r
	}

	// Refused.
	var out bytes.Buffer
	if err := runHistoryRm([]string{"-config", cfg, "-json", string(id)}, &out, d); err == nil {
		t.Fatal("rm --json removed images without --yes")
	}
	t.Logf("ember history rm --json without --yes:\n%s", out.String())
	r := decode(t, out.Bytes())
	if r.Removed || r.DryRun || r.Images != 2 || len(r.Requests) != 1 || r.Requests[0].Bytes != 2000 {
		t.Errorf("the refused report = %+v", r)
	}

	// Dry run.
	out.Reset()
	if err := runHistoryRm([]string{"-config", cfg, "-json", "-dry-run", string(id)}, &out, d); err != nil {
		t.Fatal(err)
	}
	if r := decode(t, out.Bytes()); r.Removed || !r.DryRun {
		t.Errorf("the dry-run report = %+v", r)
	}

	// Done.
	out.Reset()
	if err := runHistoryRm([]string{"-config", cfg, "-json", "-yes", string(id)}, &out, d); err != nil {
		t.Fatal(err)
	}
	t.Logf("ember history rm --json --yes:\n%s", out.String())
	if r := decode(t, out.Bytes()); !r.Removed || r.DryRun || r.Images != 2 {
		t.Errorf("the removal report = %+v", r)
	}

	// And nothing left, as an empty array rather than null.
	out.Reset()
	if err := runHistoryRm([]string{"-config", cfg, "-json", string(id)}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"requests": []`) {
		t.Errorf("an empty report sent null rather than []:\n%s", out.String())
	}
}

// TestHistoryRm_AHostileKeyIsRefusedRatherThanFollowed.
//
// A REQUEST ID IS THE KEY TO A PATH, and the key comes out of a database file
// on disk -- which an operator can edit, and which AddImage does not validate.
// Every byte removed here goes through blob.Store: the name is checked
// component by component and the removal goes through an os.Root, which is
// containment twice by two mechanisms, the rule internal/transfer follows for
// a tar member. Nothing in the command builds a path from an id.
func TestHistoryRm_AHostileKeyIsRefusedRatherThanFollowed(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var id genstore.ReqID
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		id = generation(t, cfg, d, st, longAgo(), longAgo().Add(time.Hour), 1, 10)
		// What a hand-edited database looks like.
		if err := st.AddImage(t.Context(), genstore.Image{
			Req: id, Job: 9, Index: 0, Key: "../../../etc/passwd", Bytes: 1,
		}); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	err := runHistoryRm([]string{"-config", cfg, "-yes", string(id)}, &out, d)
	if err == nil {
		t.Fatal("rm followed a key out of the output directory")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("err = %v, want the key named rather than cleaned", err)
	}
}

// TestHistory_TheBareVerbIsStillTheLedger.
//
// A leading word that is not a subcommand falls through to the flag parser, so
// `ember history --json` keeps working and `ember history nonsense` is still
// refused where it was. A second level of switch that required
// `ember history ls` would have broken both for the sake of symmetry.
func TestHistory_TheBareVerbIsStillTheLedger(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	seedSpend(t, cfg, d, func(t *testing.T, st genstore.Store) {
		ledgerRow(t, st, "run-a", "a100-80g", spendDay(1), 1_790_000, 1, 380_000)
	})

	var out bytes.Buffer
	if err := runHistory([]string{"-config", cfg}, &out, d); err != nil {
		t.Fatalf("history = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "when (utc)") {
		t.Errorf("the bare verb stopped printing the ledger:\n%s", out.String())
	}

	out.Reset()
	if err := runHistory([]string{"-config", cfg, "nonsense"}, &out, d); err == nil {
		t.Error("`ember history nonsense` was accepted")
	}

	// And the subcommands are reachable through the same entry point the
	// dispatch table uses. -h exits 3 as it does for every other flag set
	// here, which is `ember store ls -h`'s behaviour and not this change's.
	for _, sub := range []string{"du", "prune", "rm"} {
		out.Reset()
		if err := runHistory([]string{sub, "-h"}, &out, d); ExitCode(err) != ExitUsage {
			t.Errorf("history %s -h = %v, exit %d", sub, err, ExitCode(err))
		}
		if !strings.Contains(out.String(), "ember history "+sub) {
			t.Errorf("history %s -h does not describe itself:\n%s", sub, out.String())
		}
	}
	// The ledger's own help names them, so somebody reading `ember history -h`
	// learns they exist.
	out.Reset()
	_ = runHistory([]string{"-h"}, &out, d)
	for _, sub := range []string{"du", "prune", "rm"} {
		if !strings.Contains(out.String(), "  "+sub+" ") {
			t.Errorf("`ember history -h` does not list %q:\n%s", sub, out.String())
		}
	}
}
