package genstore

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// The model column, and what each test here is defending.
//
// requests.model exists because the only stored copy of a model used to be one
// cached `caps` row in `meta` that every rental overwrites -- so a historical
// request's model was not knowable, `ember spend --by model` was the one
// grouping that could not be offered, and `--by gpu` grouped the dimension
// that cannot be read without this one. FLUX.2 at 2048x2048 took 140.8s a job
// and sd15 at 256x256 took 0.43s marginal: three hundred times apart in
// sampling time on hardware four times apart.
//
// The value is a RESPONSE and not a reading of the config, which is the whole
// distinction. See the migration in sqlite.go for what upstream reports and
// what it does not.

// TestGroupBys_OffersEveryGroupingItDeclares reads this package's own source
// for GroupBy constants and compares them with GroupBys().
//
// IN BOTH DIRECTIONS, AND BY PARSING RATHER THAN BY A LIST, for the reason
// internal/cli's README test parses the dispatch table: a guard hard-coding
// today's four names is a hand-maintained list with extra steps, and it keeps
// passing after somebody adds a fifth. The two failures it is written against
// are the two halves of one mistake -- a constant Valid accepts that GroupBys
// does not offer is a grouping that works and is undocumented, so no flag help
// mentions it and nothing reads it; a name GroupBys offers that Valid refuses
// prints in the help of `ember spend --by` and fails when an operator types
// it. Both were reachable while this column was being added, because ByModel
// had to be written into three places and a switch is not a set.
func TestGroupBys_OffersEveryGroupingItDeclares(t *testing.T) {
	declared := declaredGroupBys(t)
	if len(declared) < 4 {
		// The parse found almost nothing, which is the failure mode of a test
		// that reads source: it would then pass by comparing two empty sets.
		t.Fatalf("only %v GroupBy constants were found in the package source; the parse is wrong", declared)
	}

	offered := map[GroupBy]bool{}
	for _, g := range GroupBys() {
		offered[g] = true
		if !g.Valid() {
			t.Errorf("GroupBys offers %q and Valid refuses it, so `ember spend --by %s` would fail when typed", g, g)
		}
	}

	for _, g := range declared {
		if g == ByTotal {
			// The zero value, which is "one total" and deliberately not a
			// grouping an operator names.
			continue
		}
		if !g.Valid() {
			t.Errorf("the constant for %q is declared and Valid refuses it", g)
		}
		if !offered[g] {
			t.Errorf("%q is a grouping this package implements and GroupBys does not offer it, "+
				"so nothing documents it and no command can reach it", g)
		}
	}
}

// declaredGroupBys reads the GroupBy constants out of the package's own files.
func declaredGroupBys(t *testing.T) []GroupBy {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []GroupBy
	for _, name := range names {
		if len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != "GroupBy" || len(vs.Values) != 1 {
					continue
				}
				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, GroupBy(v))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// A request records which model served it, and reads back as empty when
// nothing did.
func TestModel_RoundTripsAndIsEmptyWhenNobodyReadIt(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		served := ran(t, s, served(day(1), "flux2-dev-Q8_0.gguf"), []JobRun{job(1, 400_000, 1, 28, 140*time.Second)})
		nameless := ran(t, s, rental("rtx4090", day(2), 890_000, time.Hour, "a"),
			[]JobRun{job(1, 95_000, 1, 28, time.Minute)})

		got, err := s.Get(t.Context(), served)
		if err != nil {
			t.Fatal(err)
		}
		if got.Model != "flux2-dev-Q8_0.gguf" {
			t.Errorf("Model = %q, want what the server reported", got.Model)
		}
		blank, err := s.Get(t.Context(), nameless)
		if err != nil {
			t.Fatal(err)
		}
		// Empty and not a guess. The only stored copy of a past model is the
		// cached caps row the next rental overwrites, so there is nothing
		// here to fall back to that would not be a claim about a different
		// run.
		if blank.Model != "" {
			t.Errorf("a request nobody recorded a model for reads %q", blank.Model)
		}

		// And the ledger carries it, which is the reader `ember history`
		// prints from.
		rows, err := s.History(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("History = %+v, want two rows", rows)
		}
		for _, e := range rows {
			switch e.ID {
			case served:
				if e.Model != "flux2-dev-Q8_0.gguf" {
					t.Errorf("the ledger does not report the model: %+v", e)
				}
			case nameless:
				if e.Model != "" {
					t.Errorf("the ledger invented a model: %+v", e)
				}
			}
		}
	})
}

// served builds a finished rental that recorded its model.
func served(created time.Time, model string) Request {
	r := rental("rtx-pro-6000", created, 2_490_000, time.Hour, "a cat")
	r.Model = model
	return r
}

// A model given at enqueue is dropped by both stores.
//
// A REQUEST DOES NOT ASK FOR A MODEL. `sd.serve` chooses it and a request may
// not touch it -- the refusal that keeps Extra off Job, applied to the field
// that says what the operator will be billed against. So the INSERT does not
// name the column, and the fake forgets it too: a fake that remembered it
// would be KINDER than the file on disk, and a caller could build on a value
// that survives in tests and vanishes in production.
func TestModel_IsNotPartOfTheAsk(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		in := req("a cat")
		in.Model = "something-the-caller-chose.gguf"
		id, err := s.Enqueue(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Model != "" {
			t.Errorf("Enqueue kept a model the request asked for: %q", got.Model)
		}
	})
}

// Spend groups by model, and a request whose model nobody read is its own
// bucket rather than being folded into a named one.
func TestSpend_GroupsByModel(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ran(t, s, served(day(1), "flux2-dev-Q8_0.gguf"),
			[]JobRun{job(1, 400_000, 1, 28, 140*time.Second)})
		ran(t, s, served(day(2), "flux2-dev-Q8_0.gguf"),
			[]JobRun{job(1, 420_000, 1, 28, 141*time.Second)})
		ran(t, s, served(day(3), "sd15.gguf"),
			[]JobRun{job(1, 20_000, 2, 4, 10*time.Second)})
		ran(t, s, rental("rtx4090", day(4), 890_000, time.Hour, "a"),
			[]JobRun{job(1, 95_000, 1, 28, time.Minute)})

		got, err := s.Spend(t.Context(), SpendQuery{By: ByModel})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("Spend --by model = %+v, want a row per model and one for the unread", got)
		}
		// Keyed and ordered by the key, so the empty one comes first -- which
		// is what both stores do and what the conformance test compares.
		byKey := map[string]Bucket{}
		for _, b := range got {
			byKey[b.Key] = b
		}
		flux, ok := byKey["flux2-dev-Q8_0.gguf"]
		if !ok {
			t.Fatalf("no row for the model two requests ran against: %+v", got)
		}
		if flux.Requests != 2 || flux.JobCost != 820_000 {
			t.Errorf("the model's two requests did not group together: %+v", flux)
		}
		// The rentals, counted once each on the request that opened them --
		// the same rule every other grouping sums under.
		if flux.RunCost != 2*2_490_000 {
			t.Errorf("RunCost = %v, want both rentals", flux.RunCost)
		}
		if sd, ok := byKey["sd15.gguf"]; !ok || sd.Requests != 1 || sd.Images != 2 {
			t.Errorf("the other model's row is wrong: %+v", sd)
		}
		// THE UNREAD ONE IS NOT FOLDED IN. Attributing a request ember knows
		// nothing about to a model it does name would put money under a
		// heading that claims to describe it, which is the whole failure the
		// column exists to end -- in the report rather than in the database.
		blank, ok := byKey[""]
		if !ok {
			t.Fatalf("the request with no model is missing from the grouping: %+v", got)
		}
		if blank.Requests != 1 || blank.RunCost != 890_000 {
			t.Errorf("the unread row is wrong: %+v", blank)
		}
	})
}

// TestOpen_MigratesAVersionFourFile.
//
// Built by hand as a version-4 file rather than by checking out the old code,
// for the reason the version-1, version-2 and version-3 tests give: what has
// to be proved is that TODAY'S ember reads YESTERDAY'S file, and checking out
// yesterday's code shows the opposite. Yesterday is now version 4 -- the file
// an operator who ran `ember serve` last week has on disk.
func TestOpen_MigratesAVersionFourFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The version-4 schema: the memory columns, and no model. Only the tables
	// this test touches.
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta (key, value) VALUES ('schema_version', '4')`,
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
			mem_cgroup INTEGER NOT NULL DEFAULT 0, mem_limit INTEGER NOT NULL DEFAULT 0,
			mem_current INTEGER NOT NULL DEFAULT 0, mem_total INTEGER NOT NULL DEFAULT 0,
			mem_cached INTEGER NOT NULL DEFAULT 0, asset_bytes INTEGER NOT NULL DEFAULT 0,
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
		// THE CACHED CAPABILITIES ARE IN THE FILE, because they are in every
		// real one: PutCaps writes this row on every rental. It is the only
		// place a migration could read a model from, and it is the wrong
		// place -- it says what is loaded NOW, which for a history of
		// requests that already ran is a different claim about every one of
		// them. Seeded here so that a backfill written later has something to
		// find and this test goes red, which it did not while the file had no
		// caps row: the backfill wrote the empty string and the assertion
		// below passed against the bug.
		`INSERT INTO meta (key, value) VALUES ('caps',
			'{"Model":"whatever-is-loaded-today.gguf","Samplers":["euler"]}')`,
		`INSERT INTO requests (id, created_unix, status, provider, gpu, ttl_ns, cost,
			billable_ns, started_unix, ended_unix, mem_cgroup, mem_limit, asset_bytes)
		 VALUES ('v4', 1762000000000, 'done', 'runpod', 'rtx-pro-6000', 3600000000000,
			2490000, 3600000000000, 1762000000000, 1762003600000, 2, 34359738368, 60129542144)`,
		`INSERT INTO jobs (req, idx, prompt, steps, cost, images, steps_done, passes,
			started_unix, ended_unix)
		 VALUES ('v4', 1, 'a cat', 28, 400000, 1, 28, 1, 1762000000000, 1762000140000)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed a version-4 file: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a version-4 file was refused: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.Get(t.Context(), "v4")
	if err != nil {
		t.Fatalf("the migrated request could not be read: %v", err)
	}
	if got.Cost != 2_490_000 || got.GPU != "rtx-pro-6000" || got.Billable != time.Hour {
		t.Errorf("the migration changed what was there: %+v", got)
	}
	if got.MemLimit != 32<<30 || got.AssetBytes != 56<<30 {
		t.Errorf("the migration lost the memory columns: %+v", got)
	}
	// NOTHING BACKFILLS, and here there is not even a wrong answer available:
	// no event carries the model, and the one stored copy is the cached caps
	// row the next rental overwrites -- so a backfill would stamp every row
	// in the history with whatever is loaded today.
	if got.Model != "" {
		t.Errorf("a version-4 row invented a model: %q", got.Model)
	}
	// And the cache is still there and still the current model, which is the
	// thing that must not have been copied onto a historical row.
	caps, ok, err := s.Caps(t.Context())
	if err != nil || !ok {
		t.Fatalf("Caps after the migration = %+v, %v, %v", caps, ok, err)
	}
	if caps.Model == got.Model {
		t.Errorf("the migration copied the cached model %q onto a request that ran before it", caps.Model)
	}

	// Writing works, which is the thing the missing-column failure broke: Open
	// succeeds, the version check passes, and the first write fails with
	// "table requests has no column named model" over an operator's whole
	// history.
	if err := s.Update(t.Context(), "v4", func(r *Request) { r.Model = "flux2-dev-Q8_0.gguf" }); err != nil {
		t.Fatalf("could not write the model column on a migrated file: %v", err)
	}
	again, err := s.Get(t.Context(), "v4")
	if err != nil {
		t.Fatal(err)
	}
	if again.Model != "flux2-dev-Q8_0.gguf" {
		t.Errorf("the model column did not survive a write: %+v", again)
	}

	// And it groups, which is the reader this column exists for.
	rows, err := s.Spend(t.Context(), SpendQuery{By: ByModel})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Key != "flux2-dev-Q8_0.gguf" {
		t.Errorf("a migrated file does not group by model: %+v", rows)
	}
}
