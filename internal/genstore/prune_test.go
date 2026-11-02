package genstore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/report"
)

// seedForPrune puts one finished request and one still running into a store,
// each with events, images and job figures.
//
// Both, always, because every assertion worth making here is about what was
// KEPT. A prune that removed everything would pass a test that only counted
// what went.
func seedForPrune(t *testing.T, s Store, ended time.Time) (done, live ReqID) {
	t.Helper()
	ctx := t.Context()

	for i, r := range []Request{req("old"), req("live")} {
		id, err := s.Enqueue(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ClaimNext(ctx, "run-1", Compat{}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendEvents(ctx, id, []report.Event{
			{Seq: 1, Kind: report.KindLog, Text: "a\n"},
			{Seq: 2, Kind: report.KindLog, Text: "b\n"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddImage(ctx, Image{Req: id, Job: 1, Index: 0, Key: string(id) + "/a-00.png", Bytes: 1000}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateJob(ctx, id, 1, func(j *JobRun) {
			j.Cost, j.Images, j.Bytes = 250_000, 1, 1000
			j.Started = ended.Add(-time.Minute)
			j.Ended = ended
		}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := s.Update(ctx, id, func(r *Request) {
				r.Status, r.Ended, r.Cost = StatusDone, ended, 900_000
			}); err != nil {
				t.Fatal(err)
			}
			done = id
			continue
		}
		live = id
	}
	return done, live
}

// TestPruneEvents_KeepsEverythingItWasAskedToKeep.
//
// THE ASSERTION IS ABOUT WHAT STAYED. #28's own "done when" is that a prune
// keeps every request, job, cost and image it was asked to keep, and the
// failure that matters is a DELETE whose WHERE clause reaches one table too
// far -- which a test counting removed rows cannot see.
func TestPruneEvents_KeepsEverythingItWasAskedToKeep(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ended := time.Now().Add(-48 * time.Hour)
		done, live := seedForPrune(t, s, ended)
		ctx := t.Context()

		n, err := s.PruneEvents(ctx, time.Now().Add(-24*time.Hour), false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("pruned %d event(s), want the finished request's 2", n)
		}

		// The events of the finished request are the only thing gone.
		if es, err := s.Events(ctx, done, 0); err != nil || len(es) != 0 {
			t.Errorf("Events(done) = %d, %v; want none left", len(es), err)
		}
		if es, err := s.Events(ctx, live, 0); err != nil || len(es) != 2 {
			t.Errorf("Events(live) = %d, %v; want the running request's 2 untouched", len(es), err)
		}

		// The request, its job, its cost and its image all stayed.
		r, err := s.Get(ctx, done)
		if err != nil {
			t.Fatalf("the pruned request is gone: %v", err)
		}
		if r.Cost != 900_000 || len(r.Jobs) != 1 {
			t.Errorf("the prune changed the request: cost=%s jobs=%d", r.Cost, len(r.Jobs))
		}
		ims, err := s.Images(ctx, done)
		if err != nil || len(ims) != 1 {
			t.Errorf("Images(done) = %d, %v; the images were paid for and are not events", len(ims), err)
		}
		runs, err := s.JobRuns(ctx, done)
		if err != nil || len(runs) != 1 || runs[0].Cost != 250_000 {
			t.Errorf("JobRuns(done) = %+v, %v; the per-job figures are not events either", runs, err)
		}

		// And the ledger reports the same figures it did before, which is the
		// whole argument for events being the prunable table.
		rows, err := s.History(ctx, Filter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range rows {
			if e.ID != done {
				continue
			}
			if e.Cost != 900_000 || e.JobCost != 250_000 || e.Images != 1 || e.Bytes != 1000 {
				t.Errorf("the ledger row changed: %+v", e)
			}
		}
	})
}

// TestPruneEvents_NeverTouchesARequestThatHasNotFinished.
//
// A horizon far in the future selects everything a date comparison can select,
// so what keeps the running request's events is the ended_unix test and
// nothing else. That is the live-stream case: a browser holding a cursor into
// a request in flight must not have the rows pulled out from under it.
func TestPruneEvents_NeverTouchesARequestThatHasNotFinished(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		_, live := seedForPrune(t, s, time.Now().Add(-48*time.Hour))
		ctx := t.Context()

		if _, err := s.PruneEvents(ctx, time.Now().Add(365*24*time.Hour), false); err != nil {
			t.Fatal(err)
		}
		es, err := s.Events(ctx, live, 0)
		if err != nil || len(es) != 2 {
			t.Errorf("Events(live) = %d, %v; a horizon in the future still must not reach a running request",
				len(es), err)
		}
	})
}

// TestPruneEvents_TheHorizonIsWhenTheRequestFINISHED.
//
// The other spellings of a period in this package bound created_unix, because
// an operator asks "what did last week cost". A retention horizon asks "how
// long after it finished", and for a request created in March and finished
// yesterday the two answers are forty weeks apart -- so a prune built on the
// created bound would delete the replay of a run that ended this week.
func TestPruneEvents_TheHorizonIsWhenTheRequestFINISHED(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		long := req("a long one")
		long.Created = time.Now().Add(-90 * 24 * time.Hour)
		id, err := s.Enqueue(ctx, long)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendEvents(ctx, id, []report.Event{{Seq: 1, Kind: report.KindLog, Text: "x\n"}}); err != nil {
			t.Fatal(err)
		}
		// Created three months ago, finished an hour ago.
		if err := s.Update(ctx, id, func(r *Request) {
			r.Status, r.Ended = StatusDone, time.Now().Add(-time.Hour)
		}); err != nil {
			t.Fatal(err)
		}

		n, err := s.PruneEvents(ctx, time.Now().Add(-24*time.Hour), false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("pruned %d event(s) of a request that finished an hour ago; the horizon is "+
				"measured from the end and not from the creation", n)
		}
	})
}

// TestPruneEvents_ADryRunRemovesNothingAndCountsTheSameRows.
//
// Both halves. A dry run that deletes is the defect; a dry run that counts a
// different set from the one the delete takes is the subtler one, and the
// store answers both through one const predicate so the only difference
// between the two statements is the verb.
func TestPruneEvents_ADryRunRemovesNothingAndCountsTheSameRows(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seedForPrune(t, s, time.Now().Add(-48*time.Hour))
		ctx := t.Context()
		before := time.Now().Add(-24 * time.Hour)

		dry, err := s.PruneEvents(ctx, before, true)
		if err != nil {
			t.Fatal(err)
		}
		if dry != 2 {
			t.Errorf("the dry run counted %d, want 2", dry)
		}
		// Twice, because a "dry run" that removed them would report two and
		// then zero and the first assertion alone would pass.
		again, err := s.PruneEvents(ctx, before, true)
		if err != nil {
			t.Fatal(err)
		}
		if again != dry {
			t.Errorf("the dry run removed something: %d then %d", dry, again)
		}

		wet, err := s.PruneEvents(ctx, before, false)
		if err != nil {
			t.Fatal(err)
		}
		if wet != dry {
			t.Errorf("the dry run predicted %d row(s) and the delete took %d", dry, wet)
		}
	})
}

// TestDropImages_ForgetsTheFilesAndKeepsTheFigures.
//
// This is what makes `ember history rm` able to keep the measurement: a ledger
// row's image and byte counts come from jobs.images and jobs.bytes, which this
// does not touch, and never from the images table.
func TestDropImages_ForgetsTheFilesAndKeepsTheFigures(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ended := time.Now().Add(-48 * time.Hour)
		done, live := seedForPrune(t, s, ended)
		ctx := t.Context()

		n, err := s.DropImages(ctx, done)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("dropped %d image row(s), want 1", n)
		}
		if ims, err := s.Images(ctx, done); err != nil || len(ims) != 0 {
			t.Errorf("Images(done) = %d, %v; want none", len(ims), err)
		}
		// The other request's images are untouched, which is the WHERE clause
		// reaching exactly one request.
		if ims, err := s.Images(ctx, live); err != nil || len(ims) != 1 {
			t.Errorf("Images(live) = %d, %v; want the other request's image kept", len(ims), err)
		}

		rows, err := s.History(ctx, Filter{Status: StatusDone})
		if err != nil || len(rows) != 1 {
			t.Fatalf("History = %+v, %v", rows, err)
		}
		if rows[0].Images != 1 || rows[0].Bytes != 1000 || rows[0].JobCost != 250_000 {
			t.Errorf("the ledger forgot what the request produced: %+v", rows[0])
		}
		if rows[0].Cost != 900_000 {
			t.Errorf("the rental cost went with the pictures: %s", rows[0].Cost)
		}

		// Already gone is a success, the contract destroy has everywhere here.
		if again, err := s.DropImages(ctx, done); err != nil || again != 0 {
			t.Errorf("DropImages twice = %d, %v; want 0 and no error", again, err)
		}
	})
}

// TestUsage_CountsWhatItHoldsAndWhatARemovalLeft.
//
// Reclaimed is the figure that matters: "produced six images, has none on
// disk" and "produced none" must not read alike, and after `ember history rm`
// the first is exactly what a row looks like.
func TestUsage_CountsWhatItHoldsAndWhatARemovalLeft(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ended := time.Now().Add(-48 * time.Hour)
		done, _ := seedForPrune(t, s, ended)
		ctx := t.Context()

		u, err := s.Usage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if u.Requests != 2 || u.Jobs != 2 || u.Events != 4 || u.Images != 2 {
			t.Errorf("Usage = %+v, want 2 requests, 2 jobs, 4 events, 2 images", u)
		}
		if u.ImageBytes != 2000 {
			t.Errorf("ImageBytes = %d, want 2000", u.ImageBytes)
		}
		if u.Reclaimed != 0 {
			t.Errorf("Reclaimed = %d before anything was removed", u.Reclaimed)
		}

		if _, err := s.DropImages(ctx, done); err != nil {
			t.Fatal(err)
		}
		u, err = s.Usage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if u.Images != 1 || u.ImageBytes != 1000 {
			t.Errorf("Usage after a removal = %+v", u)
		}
		if u.Reclaimed != 1 {
			t.Errorf("Reclaimed = %d, want the one finished request whose images are gone", u.Reclaimed)
		}
	})
}

// TestUsage_AnEmptyStoreIsZeroAndNotAnError.
//
// SUM over no rows is NULL in SQLite, which scans into an int64 as a failure
// rather than as zero -- and an empty database is the first thing anybody runs
// `ember history du` against.
func TestUsage_AnEmptyStoreIsZeroAndNotAnError(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		u, err := s.Usage(t.Context())
		if err != nil {
			t.Fatalf("Usage on an empty store: %v", err)
		}
		if u != (Usage{}) {
			t.Errorf("Usage = %+v, want all zero", u)
		}
	})
}

// TestVacuum_OnlyRunsWhenAPruneRemovedSomething.
//
// The guard lives in Vacuum and not at the call site, so there is no unguarded
// entry point to forget -- and this is how "it did not run" is observable at
// all: against a CLOSED database a real VACUUM fails, so a zero count that
// comes back clean is proof that nothing was executed.
func TestVacuum_OnlyRunsWhenAPruneRemovedSomething(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}

	ran, err := s.Vacuum(t.Context(), 12)
	if err != nil || !ran {
		t.Errorf("Vacuum(12) = %v, %v; want it to run", ran, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []int{0, -1} {
		ran, err := s.Vacuum(t.Context(), removed)
		if err != nil {
			t.Errorf("Vacuum(%d) on a closed database = %v; it must not have touched it", removed, err)
		}
		if ran {
			t.Errorf("Vacuum(%d) reported it rewrote the file", removed)
		}
	}
}
