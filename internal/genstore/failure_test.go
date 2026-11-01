package genstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// TestSQLite_EveryMethodReportsAClosedDatabase.
//
// NOT A COVERAGE EXERCISE. `ember serve` shuts down by cancelling every run
// and then closing what it owns, and a run finishing at that moment calls
// Update and AppendEvents against a database that is on its way out. Every
// one of those paths has to come back as an error naming what it was doing --
// a panic there happens inside a deferred teardown, which is where it takes
// the destroy with it and leaks the node.
func TestSQLite_EveryMethodReportsAClosedDatabase(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Enqueue(t.Context(), req("a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"Enqueue", func() error { _, err := s.Enqueue(ctx, req("b")); return err }},
		{"Get", func() error { _, err := s.Get(ctx, id); return err }},
		{"List", func() error { _, err := s.List(ctx, Filter{}); return err }},
		{"List filtered", func() error { _, err := s.List(ctx, Filter{Status: StatusQueued}); return err }},
		{"ClaimNext", func() error { _, _, err := s.ClaimNext(ctx, "run-1", Compat{}); return err }},
		{"ClaimNext compat", func() error {
			_, _, err := s.ClaimNext(ctx, "run-1", Compat{Provider: "local", GPU: "x"})
			return err
		}},
		{"Update", func() error { return s.Update(ctx, id, func(*Request) {}) }},
		{"AppendEvents", func() error {
			return s.AppendEvents(ctx, id, []report.Event{{Seq: 1, Kind: report.KindLog, Text: "x\n"}})
		}},
		{"Events", func() error { _, err := s.Events(ctx, id, 0); return err }},
		{"AddImage", func() error { return s.AddImage(ctx, Image{Req: id, Job: 1, Key: "a.png"}) }},
		{"Images", func() error { _, err := s.Images(ctx, id); return err }},
		{"PutCaps", func() error { return s.PutCaps(ctx, sdserver.Capabilities{Model: "m"}) }},
		{"Caps", func() error { _, _, err := s.Caps(ctx); return err }},
		{"Interrupt", func() error { _, err := s.Interrupt(ctx); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatal("reported success against a closed database")
			}
			// The message has to say what ember was doing, not only that SQL
			// failed: "sql: database is closed" alone tells an operator
			// nothing about which request lost its progress.
			if strings.Contains(err.Error(), "database is closed") && len(err.Error()) < 30 {
				t.Errorf("err = %q, want it to name the operation too", err)
			}
		})
	}
}

// TestSQLite_UsesTheInjectedClock. A recorded timestamp is compared -- by a
// listing that orders by it, and by a UI showing how long a request waited --
// so the clock is injectable. Nothing here waits on one.
func TestSQLite_UsesTheInjectedClock(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }

	id, err := s.Enqueue(t.Context(), req("a"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created.Equal(at) {
		t.Errorf("created = %v, want the injected %v", got.Created, at)
	}

	claimed, ok, err := s.ClaimNext(t.Context(), "run-1", Compat{})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !claimed.Started.Equal(at) {
		t.Errorf("started = %v, want the injected %v", claimed.Started, at)
	}
}

// TestRecording_InjectsFailuresTheSameWayEveryOtherFakeHere Does.
//
// Errors keyed by the call, FailOn as a substring -- the field names
// dockerengine.RecordingEngine and go-tun's linux.RecordingRunner use, so
// anyone who has read one fake here reads this one without learning anything.
func TestRecording_InjectsFailuresTheSameWayEveryOtherFakeHereDoes(t *testing.T) {
	s := NewRecordingStore()
	boom := errors.New("boom")

	// Keyed by the exact call.
	s.Errors["Enqueue req-1"] = boom
	if _, err := s.Enqueue(t.Context(), req("a")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the injected failure", err)
	}
	if got := s.Err("Enqueue req-1"); !errors.Is(got, boom) {
		t.Errorf("Err() = %v, want the injected failure", got)
	}
	if s.Err("nothing injected here") == nil {
		t.Error("Err() for a call with no injected failure returned nil")
	}

	// By substring, which is what FailOn is for: a test that does not want to
	// know the generated id.
	s2 := NewRecordingStore()
	s2.FailOn = "Enqueue"
	if _, err := s2.Enqueue(t.Context(), req("a")); err == nil {
		t.Error("FailOn did not fail a matching call")
	}

	// And the calls are recorded, which is what makes "one job at a time"
	// assertable from above.
	if len(s2.Calls) == 0 {
		t.Error("nothing was recorded")
	}
	if err := s2.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

// TestRecording_FailsEveryCallItWasToldTo walks the rest of the surface, so a
// caller can inject a failure anywhere the real store can produce one.
func TestRecording_FailsEveryCallItWasToldTo(t *testing.T) {
	s := NewRecordingStore()
	id, err := s.Enqueue(t.Context(), req("a"))
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	for _, c := range []struct {
		on   string
		call func() error
	}{
		{"Get", func() error { _, err := s.Get(ctx, id); return err }},
		{"List", func() error { _, err := s.List(ctx, Filter{}); return err }},
		{"ClaimNext", func() error { _, _, err := s.ClaimNext(ctx, "r", Compat{}); return err }},
		{"Update", func() error { return s.Update(ctx, id, func(*Request) {}) }},
		{"AppendEvents", func() error {
			return s.AppendEvents(ctx, id, []report.Event{{Seq: 1, Kind: report.KindLog}})
		}},
		{"Events", func() error { _, err := s.Events(ctx, id, 0); return err }},
		{"AddImage", func() error { return s.AddImage(ctx, Image{Req: id, Job: 1, Key: "a.png"}) }},
		{"Images", func() error { _, err := s.Images(ctx, id); return err }},
		{"PutCaps", func() error { return s.PutCaps(ctx, sdserver.Capabilities{}) }},
		{"Caps", func() error { _, _, err := s.Caps(ctx); return err }},
		{"Interrupt", func() error { _, err := s.Interrupt(ctx); return err }},
	} {
		t.Run(c.on, func(t *testing.T) {
			s.FailOn = c.on
			defer func() { s.FailOn = "" }()
			if err := c.call(); err == nil {
				t.Errorf("FailOn %q did not fail the call", c.on)
			}
		})
	}
}

// TestRecording_RefusesTheSameRequestTwice, because the real store's primary
// key does: a duplicate id would make Get ambiguous and the queue serve one
// request twice.
func TestRecording_RefusesTheSameRequestTwice(t *testing.T) {
	s := NewRecordingStore()
	in := req("a")
	in.ID = "fixed"
	if _, err := s.Enqueue(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(t.Context(), in); err == nil {
		t.Error("the same id was enqueued twice")
	}
}

// TestRecording_HandsOutCopies.
//
// Without it, a caller appending to the Jobs it was given writes into the
// store, and the store then holds jobs nobody enqueued -- which would show up
// as a request that grew between two reads.
func TestRecording_HandsOutCopies(t *testing.T) {
	s := NewRecordingStore()
	id, err := s.Enqueue(t.Context(), req("a"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	got.Jobs[0].Prompt = "rewritten"
	got.Jobs = append(got.Jobs, Job{Prompt: "appended"})

	again, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Jobs) != 1 || again.Jobs[0].Prompt != "a" {
		t.Errorf("a caller mutated the store's own copy: %+v", again.Jobs)
	}
}

// TestRecording_UsesTheInjectedClock, so the fake is comparable with the real
// one in a test that pins a timestamp.
func TestRecording_UsesTheInjectedClock(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := NewRecordingStore()
	s.Now = func() time.Time { return at }

	id, err := s.Enqueue(t.Context(), req("a"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created.Equal(at) {
		t.Errorf("created = %v, want the injected %v", got.Created, at)
	}
}

// TestSQLite_RefusesRowsItCannotDecode.
//
// Events and the capability cache are stored as JSON, so a row that is not
// JSON is possible: a partial write, or somebody with sqlite3 and a theory.
// It has to come back as an error naming the request, because the alternative
// is a replay that silently stops at the bad row -- a client would then
// believe it had caught up when it had not.
func TestSQLite_RefusesRowsItCannotDecode(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	id, err := s.Enqueue(t.Context(), req("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (req, seq, kind, body) VALUES (?,1,'log','not json')`, string(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Events(t.Context(), id, 0); err == nil {
		t.Error("a replay accepted a row that is not an event")
	}

	if _, err := s.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('caps','not json')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Caps(t.Context()); err == nil {
		t.Error("the capability cache accepted a value that is not JSON")
	}
}

// TestOpen_RefusesAnUnreadableSchemaVersion. A version nothing can parse is
// not a version ember may assume is its own: assuming would read an unknown
// layout, which is the mistake the version exists to prevent.
func TestOpen_RefusesAnUnreadableSchemaVersion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE meta SET value = 'not a number' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(dir)
	if err == nil {
		t.Fatal("a database with an unreadable schema version was opened anyway")
	}
	if !strings.Contains(err.Error(), "unreadable schema version") {
		t.Errorf("err = %v, want it to say the version could not be read", err)
	}
}
