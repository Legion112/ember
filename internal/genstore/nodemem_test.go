package genstore

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOpen_MigratesAVersionThreeFile.
//
// Built by hand as a version-3 file rather than by checking out the old code,
// for the reason the version-1 and version-2 tests give: what has to be
// proved is that TODAY'S ember reads YESTERDAY'S file, and checking out the
// old code shows the opposite. Yesterday is now version 3 -- the file an
// operator who ran `ember serve` last week has on disk.
func TestOpen_MigratesAVersionThreeFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The version-3 schema: the execution columns, and none of the memory
	// ones. Only the tables this test touches.
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta (key, value) VALUES ('schema_version', '3')`,
		`CREATE TABLE requests (
			id TEXT PRIMARY KEY, created_unix INTEGER NOT NULL, status TEXT NOT NULL,
			provider TEXT NOT NULL, gpu TEXT NOT NULL DEFAULT '', offer TEXT NOT NULL DEFAULT '',
			ttl_ns INTEGER NOT NULL, max_hourly INTEGER NOT NULL DEFAULT 0,
			max_run INTEGER NOT NULL DEFAULT 0, preview TEXT NOT NULL DEFAULT '',
			preview_every INTEGER NOT NULL DEFAULT 0, run_id TEXT NOT NULL DEFAULT '',
			node TEXT NOT NULL DEFAULT '', started_unix INTEGER NOT NULL DEFAULT 0,
			ended_unix INTEGER NOT NULL DEFAULT 0, cost INTEGER NOT NULL DEFAULT 0,
			rate INTEGER NOT NULL DEFAULT 0, elapsed_ns INTEGER NOT NULL DEFAULT 0,
			billable_ns INTEGER NOT NULL DEFAULT 0,
			load_measured_ns INTEGER NOT NULL DEFAULT 0,
			load_reported_ns INTEGER NOT NULL DEFAULT 0,
			err TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE jobs (
			req TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
			idx INTEGER NOT NULL, prompt TEXT NOT NULL DEFAULT '',
			negative TEXT NOT NULL DEFAULT '', width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0, steps INTEGER NOT NULL DEFAULT 0,
			batch_count INTEGER NOT NULL DEFAULT 0, seed INTEGER NOT NULL DEFAULT 0,
			sampler TEXT NOT NULL DEFAULT '', scheduler TEXT NOT NULL DEFAULT '',
			cfg REAL NOT NULL DEFAULT 0, guidance REAL NOT NULL DEFAULT 0,
			output TEXT NOT NULL DEFAULT '', params TEXT NOT NULL DEFAULT '{}',
			started_unix INTEGER NOT NULL DEFAULT 0, ended_unix INTEGER NOT NULL DEFAULT 0,
			cost INTEGER NOT NULL DEFAULT 0, images INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0, passes INTEGER NOT NULL DEFAULT 0,
			steps_done INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (req, idx)
		)`,
		`CREATE TABLE events (
			req TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL,
			PRIMARY KEY (req, seq)
		)`,
		`CREATE TABLE images (
			req TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
			job INTEGER NOT NULL, idx INTEGER NOT NULL, preview INTEGER NOT NULL DEFAULT 0,
			key TEXT NOT NULL, bytes INTEGER NOT NULL DEFAULT 0, at_unix INTEGER NOT NULL,
			PRIMARY KEY (req, job, idx, preview)
		)`,
		`INSERT INTO requests (id, created_unix, status, provider, gpu, ttl_ns, cost,
			billable_ns, started_unix, ended_unix)
		 VALUES ('v3', 1762000000000, 'done', 'runpod', 'a100-80g', 3600000000000,
			1790000, 3600000000000, 1762000000000, 1762003600000)`,
		`INSERT INTO jobs (req, idx, prompt, steps, cost, images, steps_done, passes,
			started_unix, ended_unix)
		 VALUES ('v3', 1, 'a cat', 28, 380000, 1, 28, 1, 1762000000000, 1762000140000)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed a version-3 file: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a version-3 file was refused: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.Get(t.Context(), "v3")
	if err != nil {
		t.Fatalf("the migrated request could not be read: %v", err)
	}
	if got.Cost != 1_790_000 || got.GPU != "a100-80g" || got.Billable != time.Hour {
		t.Errorf("the migration changed what was there: %+v", got)
	}
	// NOTHING BACKFILLS, and this is what that means in practice. The host's
	// memory could be guessed at and the cgroup limit could not, and guessing
	// the one that is knowable is precisely the mistake these columns were
	// added to end.
	if got.MemCgroup != 0 || got.MemLimit != 0 || got.MemTotal != 0 || got.AssetBytes != 0 {
		t.Errorf("a version-3 row invented memory facts: %+v", got)
	}

	// Writing works, which is the thing the missing-column failure broke.
	if err := s.Update(t.Context(), "v3", func(r *Request) { r.MemLimit = 64 << 30 }); err != nil {
		t.Fatalf("could not write a memory column on a migrated file: %v", err)
	}
	again, err := s.Get(t.Context(), "v3")
	if err != nil {
		t.Fatal(err)
	}
	if again.MemLimit != 64<<30 {
		t.Errorf("the memory column did not survive a write: %+v", again)
	}

	// And the ledger reads it, which is the reader these columns exist for.
	rows, err := s.History(t.Context(), Filter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("History = %+v, %v", rows, err)
	}
	if rows[0].MemLimit != 64<<30 {
		t.Errorf("the ledger does not report the memory column: %+v", rows[0])
	}
}

// The memory facts round-trip through both stores, and the derived answers
// are computed on the way out rather than stored.
func TestHistory_CarriesWhatTheNodeSaidAboutItsMemory(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), Request{
			Provider: "runpod", GPU: "a100-80g", TTL: time.Hour, Created: day(1),
			Jobs: []Job{{Prompt: "a cat"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(t.Context(), id, func(r *Request) {
			r.Status = StatusDone
			r.Started, r.Ended = day(1), day(1).Add(time.Hour)
			r.MemCgroup = 2
			r.MemLimit, r.MemCurrent = 32<<30, 31<<30
			r.MemTotal, r.MemCached = 700<<30, 29<<30
			r.AssetBytes = 56 << 30
		}); err != nil {
			t.Fatal(err)
		}

		rows, err := s.History(t.Context(), Filter{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("History = %+v, %v", rows, err)
		}
		e := rows[0]
		if e.MemCgroup != 2 || e.MemLimit != 32<<30 || e.MemCurrent != 31<<30 {
			t.Errorf("the cgroup's view did not round-trip: %+v", e)
		}
		if e.MemTotal != 700<<30 || e.MemCached != 29<<30 {
			t.Errorf("the host's view did not round-trip: %+v", e)
		}
		if e.AssetBytes != 56<<30 {
			t.Errorf("the asset figure did not round-trip: %+v", e)
		}

		// The ceiling is the cgroup's and not the host's, which is the whole
		// hypothesis: this node reported 700 GiB free and could not hold a
		// 56 GiB model.
		if e.memory().Ceiling() != 32<<30 {
			t.Errorf("memory().Ceiling() = %d, want the cgroup's limit", e.memory().Ceiling())
		}
		h, ok := e.MemHeadroom()
		if !ok {
			t.Fatal("headroom is unknowable with both figures present")
		}
		if h != (32<<30)-(56<<30) {
			t.Errorf("headroom = %d, want -24 GiB", h)
		}
	})
}

// A ceiling with no weights beside it answers nothing, through both stores.
//
// THIS CASE WAS COVERED BY A TEST OF CODE THAT NEVER RAN. The same four lines
// existed on nodemem.Report, where nothing called them, and its test was the
// only thing anywhere asserting this half of the knowability rule -- so
// deleting `|| Assets <= 0` from the live method left `go test ./...` green.
// Measured, before the two copies became one.
//
// It is not a hypothetical row: a baked image and a local container have no
// asset set at all, so every local run records a ceiling and no weights. A
// headroom printed there would be the whole limit, as though the model took
// no room.
func TestHistory_AnsweringNothingAboutTheWeightsIsNotAHeadroom(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), Request{
			Provider: "local", TTL: time.Hour, Created: day(1), Jobs: []Job{{Prompt: "a"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(t.Context(), id, func(r *Request) {
			r.Status = StatusDone
			// Both views read, no assets: the local provider's shape.
			r.MemCgroup, r.MemLimit, r.MemTotal = 2, 8<<30, 64<<30
		}); err != nil {
			t.Fatal(err)
		}

		rows, err := s.History(t.Context(), Filter{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("History = %+v, %v", rows, err)
		}
		e := rows[0]
		// The ceiling is knowable -- the cgroup answered -- and the headroom
		// is not, which are different questions and must not collapse into
		// one.
		if e.memory().Ceiling() != 8<<30 {
			t.Errorf("memory().Ceiling() = %d, want the cgroup's limit", e.memory().Ceiling())
		}
		h, ok := e.MemHeadroom()
		if ok {
			t.Errorf("headroom = %d is knowable with no asset figure", h)
		}
		if h != 0 {
			t.Errorf("headroom = %d over a figure nobody read, want zero", h)
		}
	})
}

// A node with no cgroup limit falls back to the host's total, and a node that
// answered nothing answers nothing.
func TestHistory_TellsNoLimitFromNoAnswer(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		noLimit, err := s.Enqueue(t.Context(), Request{
			Provider: "local", TTL: time.Hour, Created: day(1), Jobs: []Job{{Prompt: "a"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(t.Context(), noLimit, func(r *Request) {
			r.Status = StatusDone
			r.MemCgroup, r.MemTotal, r.AssetBytes = 1, 64<<30, 8<<30
		}); err != nil {
			t.Fatal(err)
		}
		silent, err := s.Enqueue(t.Context(), Request{
			Provider: "local", TTL: time.Hour, Created: day(2), Jobs: []Job{{Prompt: "b"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(t.Context(), silent, func(r *Request) { r.Status = StatusDone }); err != nil {
			t.Fatal(err)
		}

		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range rows {
			switch e.ID {
			case noLimit:
				if e.memory().Ceiling() != 64<<30 {
					t.Errorf("a node with no cgroup limit does not fall back to the host's total: %+v", e)
				}
				if h, ok := e.MemHeadroom(); !ok || h != (64<<30)-(8<<30) {
					t.Errorf("headroom = %d, %v; want 56 GiB", h, ok)
				}
			case silent:
				if e.memory().Ceiling() != 0 {
					t.Errorf("a node that answered nothing reports a ceiling: %+v", e)
				}
				if _, ok := e.MemHeadroom(); ok {
					t.Error("headroom is knowable over a node that answered nothing")
				}
			}
		}
	})
}
