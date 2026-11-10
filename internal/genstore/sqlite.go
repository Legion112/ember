package genstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the pure-Go driver, registered as "sqlite"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/tts"
)

// FileName is what the database is called inside the state directory.
//
// Beside internal/statestore's own directory rather than anywhere else, so one
// XDG_STATE_HOME move takes the journal and this together. They are both
// machine-local facts rather than settings. The host key pins used to be named
// here as a third and are not persisted any more (#120).
const FileName = "generations.db"

// schemaVersion is the on-disk shape.
//
// A file written by a NEWER ember is refused rather than misparsed, for
// internal/statestore's reason: reading a newer layout as an older one drops
// columns, and the column it would drop is the one that says a request is
// still running.
const schemaVersion = 8

// migrations takes an existing file forward, one version per entry:
// migrations[0] moves a version-1 file to version 2.
//
// IT EXISTS BECAUSE THE VERSION CHECK ONLY GUARDED ONE DIRECTION. migrate
// refused a file from the future and did nothing at all about a file from the
// past: the schema is `CREATE TABLE IF NOT EXISTS`, which is a no-op against a
// table that already exists, so the first column ever added would have been
// absent from every database written before it. Nothing would have reported
// that -- the open succeeds, the version check passes, and the failure
// arrives as `table jobs has no column named params` the next time an
// operator queues a request, with their existing history in a file ember can
// no longer write to. A version number that is only ever compared is a
// version number that records the problem instead of fixing it.
//
// Applied inside migrate's transaction, so a file is either entirely at the
// new version or entirely at the old one. SQLite's ALTER TABLE ADD COLUMN is
// not transactional on every engine, but it is here, and a half-migrated file
// is the one outcome that would need a human.
var migrations = [][]string{
	// 1 -> 2: the optional generation parameters.
	{
		// One column and not twenty-three, because nothing queries them: no
		// WHERE, no ORDER BY, no join. They are payload carried to the
		// generation call, which is what `events.body` already is, and a
		// column per field would mean a migration every time upstream adds a
		// knob -- for a table whose only reader selects every row of a
		// request by id.
		`ALTER TABLE jobs ADD COLUMN params TEXT NOT NULL DEFAULT '{}'`,

		// AND THE CACHED CAPABILITIES GO, because they are now incomplete in
		// a way that answers a question wrongly rather than not at all. The
		// cache is the JSON of sdserver.Capabilities, and a row written
		// before this version has no output_formats and no upscalers -- so
		// they decode as empty, and Validate reads an empty list as "this
		// build did not report that capability" and refuses a correct
		// output_format with a message blaming the server for a field ember
		// never asked it about. That is the capabilities.model mistake in
		// cache form: a shape believed rather than observed.
		//
		// Dropping it costs one rental's worth of a sampler typo going
		// uncaught, once, on an upgraded file -- which is what a fresh file
		// already does, and which the next rental repairs because PutCaps
		// runs on every one of them.
		`DELETE FROM meta WHERE key = 'caps'`,
	},

	// 2 -> 3: what a job and a rental actually did.
	//
	// Every figure here was already being emitted and none of it was
	// queryable, which is the whole of #32: `grep -rn 'SUM(' internal/genstore/`
	// returned nothing, the only aggregate in the program was a Go loop over
	// one request's whole event stream, and every measurement in README was
	// computed by hand from a run somebody watched.
	//
	// Added as columns rather than read out of events.body on demand, on the
	// write-through precedent requests.node and requests.cost already set.
	// The alternative is JSON-parsing every row of the table that grows per
	// step per job per rental, which is fine for one monthly total and is the
	// wrong foundation for grouping by card.
	//
	// NOTHING BACKFILLS, and that is not an oversight. The facts are in the
	// event bodies of requests that already ran, so a backfill is possible --
	// and it would have to re-derive passes and steps from progress frames
	// whose poll interval is unknown for those runs, which is the one number
	// this schema is careful not to infer. A row from before this version
	// reads as zero, `ember spend` says how many it had to leave out, and the
	// honest version of "what did last month cost" is "ember did not record
	// it then".
	{
		`ALTER TABLE jobs ADD COLUMN started_unix INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN ended_unix INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN cost INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN images INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN passes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN steps_done INTEGER NOT NULL DEFAULT 0`,

		`ALTER TABLE requests ADD COLUMN rate INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN elapsed_ns INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN billable_ns INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN load_measured_ns INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN load_reported_ns INTEGER NOT NULL DEFAULT 0`,
	},

	// 3 -> 4: what the node said about its memory, both views.
	//
	// 81 SECONDS AND $0.056 PER IMAGE NOBODY COULD EXPLAIN is what these are
	// for. Two runs of the same configuration against the same card in the
	// same datacentre differed by that much depending on whether the node
	// kept 56 GiB of weights in page cache, and every one of those nodes
	// reported 620-708 GiB of free RAM -- because `free` inside a container
	// reports the HOST's memory and not the limit the container is bounded
	// by. The leading guess was the pod's memory cgroup, and the file that
	// would settle it was two reads on a node that was already billing.
	//
	// Columns rather than events-only, on the precedent the execution columns
	// set one version ago: a figure a performance question is about has to be
	// queryable, and "was this node able to hold the model" asked over the
	// event table means JSON-parsing every row of the table that grows per
	// step per job per rental. The event stream stays the truth and these are
	// a derived index of it.
	//
	// mem_limit zero means NO cgroup limit rather than a limit of nothing --
	// cgroup v2 writes the literal "max" and v1 a sentinel near the top of
	// int64, and both are read as zero -- so mem_cgroup is what says whether
	// anything was read at all. Nothing backfills: the facts do not exist for
	// rentals that have already happened, and inventing them from the host's
	// view is precisely the mistake these columns were added to end.
	{
		`ALTER TABLE requests ADD COLUMN mem_cgroup INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN mem_limit INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN mem_current INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN mem_total INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN mem_cached INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN asset_bytes INTEGER NOT NULL DEFAULT 0`,
	},

	// 4 -> 5: which model served the request.
	//
	// THE ONE GROUPING `ember spend` COULD NOT OFFER, and the reason it could
	// not was this column's absence rather than a decision about reports:
	// `sd.serve` is config and not request, and the only stored copy of the
	// model was one cached `caps` row in `meta` that every rental overwrites.
	// So `--by gpu` grouped the dimension that is nearly useless alone --
	// "an A100 costs $X per image" is meaningless without the model, and the
	// two measurements this repository has made differ by a factor of about
	// three hundred in sampling time on hardware a factor of four apart.
	//
	// Written from the server's own /capabilities response at claim time, not
	// from the config: an operator may edit `sd.serve` between two rentals,
	// and a figure attributed to the model that is configured NOW is a figure
	// about a run that did not happen. It is capabilities.model.name -- which
	// upstream resolves as the basename of `--model`, or of
	// `--diffusion-model` when there is no `--model`, or empty when neither
	// is named (resolve_display_model_path in routes_sdcpp.cpp). A
	// three-file configuration therefore records its DIFFUSION model and says
	// nothing about the text encoder or the VAE beside it, which is written
	// down in README because it bounds what the grouping can distinguish.
	//
	// NOTHING BACKFILLS, and here that is not even a choice between evidence
	// and inference -- there is no evidence. The facts are not in the event
	// bodies of requests that already ran: no event carries the model, which
	// is why this is a column and not a read of `events`. The only stored
	// copy is that single `caps` row, and reading it would answer "whatever
	// is loaded now" for every row in the history -- the mistake schema
	// version 2 already had to DELETE that row over. A request from before
	// this version reads as empty and both readers print a dash for it.
	{
		`ALTER TABLE requests ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
	},
	// 5 -> 6: a second workload kind.
	//
	// TWO COLUMNS AND NOT A REWRITE. `requests.kind` is the discriminator the
	// claim reads -- it is in Compat, so it decides which rental a queued row
	// may ride, which makes it a WHERE and not payload. `jobs.tts` is the
	// utterance as the request named it, one JSON blob in one column, which
	// is what `jobs.params` already is for the other driver.
	//
	// AN EMPTY `kind` IS AN IMAGE GENERATION, which is why the default is ''
	// and not 'sd'. Every row written before this version is one, and a
	// migration that stamped a literal would be asserting about an operator's
	// history rather than recording the absence; KindOf is the one place that
	// reading lives, so the two spellings cannot drift. The alternative --
	// `UPDATE requests SET kind='sd'` -- also rewrites every row of the table
	// this schema is careful never to rewrite.
	//
	// `jobs.tts` is '' and not NULL for the reason every other column here
	// is: NOT NULL DEFAULT '' means a scan into a string needs no
	// sql.NullString at any call site, and "no utterance" and "the empty
	// JSON" are the same fact.
	{
		`ALTER TABLE requests ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN tts TEXT NOT NULL DEFAULT ''`,
	},

	// 6 -> 7: the claim becomes a lease, and the capability cache stops being
	// one row per process.
	//
	// THE LEASE COLUMN DEFAULTS TO ZERO AND ZERO IS EXPIRED, which is what
	// makes this migration behave for an existing file exactly as the blanket
	// startup sweep it replaces did: every row that was running when an older
	// ember stopped has no lease, so the first Reclaim takes it. The
	// dangerous default -- a row nothing will ever reclaim -- is therefore
	// unrepresentable rather than avoided by remembering to backfill.
	//
	// AND THE CACHED CAPABILITIES GO, for the reason version 2 already
	// deleted them once: the row is a shape believed rather than observed.
	// That one was incomplete; this one answers the wrong question. It is a
	// single `caps` key in `meta` written by whichever rental came up last,
	// and it was read to validate a request against the capabilities of a
	// card the request may not have asked for. There is no key to migrate it
	// to, because nothing recorded which shape produced it -- inventing one
	// would be asserting that last week's rental was this week's card, which
	// is the mistake requests.model exists because of. It costs one rental's
	// worth of a sampler typo going uncaught, once, per shape, which is what
	// a fresh file already does and which PutCaps repairs on the next rental.
	//
	// The `caps` TABLE is not created here. `schema` runs before the version
	// check on every open and its CREATE TABLE is IF NOT EXISTS, so a new
	// table arrives in an old file without a migration -- which is exactly
	// the property that makes a new COLUMN need one.
	{
		`ALTER TABLE requests ADD COLUMN lease_until_unix INTEGER NOT NULL DEFAULT 0`,
		`DELETE FROM meta WHERE key = 'caps'`,
	},

	// 7 -> 8: which declared workload the request named.
	//
	// AN EMPTY `workload` NAMES THE IMPLICIT ENTRY, which is why the default
	// is '' and nothing is backfilled. A configuration that declares no
	// `workloads:` describes one entry per driver out of its top-level keys,
	// and each of those entries is named after its own driver -- so
	// WorkloadOf resolves '' to KindOf(kind), which is exactly the id of the
	// entry such a row would have named. A migration that stamped a literal
	// would be guessing at an id the operator may since have chosen for
	// themselves, which is the `UPDATE requests SET kind='sd'` mistake one
	// column over.
	{
		`ALTER TABLE requests ADD COLUMN workload TEXT NOT NULL DEFAULT ''`,
	},
}

// defaultLimit bounds a listing that did not ask for one.
//
// An unbounded listing of every request ever made is not an answer to "what is
// in the queue", and it is the answer that gets slower every day the operator
// uses ember.
const defaultLimit = 200

// SQLite is the Store backed by a file.
type SQLite struct {
	db   *sql.DB
	path string
	now  func() time.Time
}

// Open opens or creates the database under dir.
func Open(dir string) (*SQLite, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the state directory %s: %w", dir, err)
	}
	return OpenFile(filepath.Join(dir, FileName))
}

// OpenFile opens or creates the database at path.
//
// The pragmas are not decoration. WAL lets the API read while the drain loop
// writes, which is the whole access pattern here; busy_timeout turns a
// momentary lock into a wait rather than an error an operator sees as a
// failure; foreign_keys makes the jobs-to-request link real, so a request
// cannot be deleted out from under its own jobs.
func OpenFile(path string) (*SQLite, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One writer. SQLite serializes writes anyway, and leaving the pool to
	// open several connections means several of them racing for the write
	// lock and reporting SQLITE_BUSY to whichever lost -- which looks to an
	// operator like ember failing to record their request.
	db.SetMaxOpenConns(1)

	s := &SQLite{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *SQLite) Close() error { return s.db.Close() }

// Path is the file this store is backed by, for an error message.
func (s *SQLite) Path() string { return s.path }

func (s *SQLite) at() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS requests (
	id            TEXT PRIMARY KEY,
	created_unix  INTEGER NOT NULL,
	status        TEXT    NOT NULL,
	-- Which DRIVER this request is for. '' is an image generation, which is
	-- every row written before schema version 6; see KindOf, which is the one
	-- place that reading lives.
	kind          TEXT    NOT NULL DEFAULT '',
	-- Which declared workload the operator's file named, which is a different
	-- question from the one above and is answered by a different authority.
	-- kind is what the STORE knows without reading ember.yaml -- it is what
	-- the ledger prints about a row whose entry has since been renamed or
	-- deleted. workload is the entry id, and it means nothing without the
	-- operator's file: it is what resolves to an image. '' is every row
	-- written before schema version 7 and every request against a file that
	-- declares no workloads; see WorkloadOf, which is the one place that
	-- reading lives.
	workload      TEXT    NOT NULL DEFAULT '',
	provider      TEXT    NOT NULL,
	gpu           TEXT    NOT NULL DEFAULT '',
	offer         TEXT    NOT NULL DEFAULT '',
	ttl_ns        INTEGER NOT NULL,
	max_hourly    INTEGER NOT NULL DEFAULT 0,
	max_run       INTEGER NOT NULL DEFAULT 0,
	preview       TEXT    NOT NULL DEFAULT '',
	preview_every INTEGER NOT NULL DEFAULT 0,
	run_id        TEXT    NOT NULL DEFAULT '',
	node          TEXT    NOT NULL DEFAULT '',
	started_unix  INTEGER NOT NULL DEFAULT 0,
	ended_unix    INTEGER NOT NULL DEFAULT 0,
	cost          INTEGER NOT NULL DEFAULT 0,

	-- The rental's own facts, promoted out of events.body for the reason the
	-- jobs columns above were.
	--
	-- billable is not elapsed, and both are kept: a node destroyed after four
	-- seconds on a provider with a one-minute minimum costs a minute, and an
	-- overhead computed against the elapsed time would credit ember with a
	-- saving the invoice does not show.
	rate          INTEGER NOT NULL DEFAULT 0,
	elapsed_ns    INTEGER NOT NULL DEFAULT 0,
	billable_ns   INTEGER NOT NULL DEFAULT 0,

	-- The model load, as ember timed it and as the server claimed.
	--
	-- Both, because they disagreed by two orders of magnitude on the one
	-- measurement anybody has made: sd-server reported 551ms for weights that
	-- are 56 GiB while the other arm of the same run spent 80-96s per image
	-- re-reading them. Storing one would mean choosing which figure to be
	-- wrong with, and the disagreement is itself the finding #30 turns on.
	load_measured_ns INTEGER NOT NULL DEFAULT 0,
	load_reported_ns INTEGER NOT NULL DEFAULT 0,

	-- What the node said about its memory, both views, and the weights it was
	-- about to read.
	--
	-- BOTH VIEWS BECAUSE THEY DISAGREE AND ONLY ONE BOUNDS THE PAGE CACHE.
	-- "free" inside a container reports the host's memory: every node in the
	-- FLUX.2 measurement said it had 620-708 GiB free, and two of them
	-- differed by 81 seconds and $0.056 per image depending on whether the
	-- weights stayed cached. mem_limit is the container's own cgroup limit,
	-- where zero means there is NO limit rather than a limit of nothing --
	-- cgroup v2 writes the literal "max" and v1 a sentinel near the top of
	-- int64 -- so mem_cgroup is what says whether anything was read at all.
	mem_cgroup    INTEGER NOT NULL DEFAULT 0,
	mem_limit     INTEGER NOT NULL DEFAULT 0,
	mem_current   INTEGER NOT NULL DEFAULT 0,
	mem_total     INTEGER NOT NULL DEFAULT 0,
	mem_cached    INTEGER NOT NULL DEFAULT 0,
	asset_bytes   INTEGER NOT NULL DEFAULT 0,

	-- Which model served this request, as the generation server reported it.
	--
	-- WRITTEN FROM A RESPONSE AND NEVER FROM THE CONFIG, which is the whole
	-- point of the column. sd.serve names files on the node and an operator
	-- can change it between two rentals, so a reading of the config answers
	-- "what would run now" where the question is "what ran then". The value
	-- is capabilities.model.name, out of the same /capabilities response
	-- PutCaps caches -- and that field is an OBJECT, {"name","path","stem"},
	-- which is the shape a real server corrected after a node was already up.
	--
	-- Empty means nobody read it: a request that never got a node, or one
	-- from before this column. Nothing backfills it, and both readers print
	-- it as a dash rather than as a blank cell, because the one answer
	-- available to a backfill is the cached caps row -- which is the CURRENT
	-- model and which the next rental overwrites.
	model         TEXT    NOT NULL DEFAULT '',

	-- When the claim on this row stops being held, in milliseconds.
	--
	-- THE run_id COLUMN IS A LEASE AND THIS IS ITS EXPIRY. ClaimNext writes
	-- both, Renew pushes this forward while the process that claimed the row
	-- is still responsible for it, and Reclaim marks interrupted whatever has
	-- run out. Before it existed a claimed row read "running" for ever if the
	-- process that claimed it died, and the only thing that cleared one was a
	-- sweep of EVERY running row at the next startup -- which orphaned the
	-- row of a node that was still alive and billing, because a node carries
	-- its run id in the provider's own label and that label is the source of
	-- truth about what is live.
	--
	-- ZERO IS EXPIRED, never "no expiry": every row written before schema
	-- version 7 holds zero, and a row nothing can reclaim is the one state
	-- this column exists to remove.
	--
	-- NOT ON genstore.Request, and that is deliberate. Update takes a
	-- callback over a Request and writes back every column it names, so a
	-- lease reachable from there is a lease any caller could extend to
	-- whenever it liked -- which is the same as not having one. The only
	-- writers are the three statements above.
	lease_until_unix INTEGER NOT NULL DEFAULT 0,

	err           TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS requests_queue ON requests(status, created_unix);

CREATE TABLE IF NOT EXISTS jobs (
	req         TEXT    NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
	idx         INTEGER NOT NULL,
	prompt      TEXT    NOT NULL DEFAULT '',
	negative    TEXT    NOT NULL DEFAULT '',
	width       INTEGER NOT NULL DEFAULT 0,
	height      INTEGER NOT NULL DEFAULT 0,
	steps       INTEGER NOT NULL DEFAULT 0,
	batch_count INTEGER NOT NULL DEFAULT 0,
	seed        INTEGER NOT NULL DEFAULT 0,
	sampler     TEXT    NOT NULL DEFAULT '',
	scheduler   TEXT    NOT NULL DEFAULT '',
	cfg         REAL    NOT NULL DEFAULT 0,
	guidance    REAL    NOT NULL DEFAULT 0,
	output      TEXT    NOT NULL DEFAULT '',
	-- The optional parameters, as the JSON of sdserver.Params. See
	-- migrations for why this is one column rather than one per field.
	params      TEXT    NOT NULL DEFAULT '{}',
	-- The utterance, when this request is a speech one; '' otherwise. One
	-- JSON blob in one column, which is what the params column above is for
	-- the other driver; see the 5 -> 6 migration for why not a second key in
	-- it. No backticks in this comment: the schema is a raw string literal,
	-- so one would close it.
	tts         TEXT    NOT NULL DEFAULT '',

	-- WHAT HAPPENED, as opposed to what was asked for above.
	--
	-- Every one of these was already emitted and none was queryable: they
	-- lived inside events.body, one opaque JSON blob per event, so "what did
	-- an image cost on this card" meant parsing every row of the largest
	-- table in the schema. They are columns for the reason requests.node and
	-- requests.cost are -- something needs to join on them -- and the event
	-- stream stays the append-only truth beside them. A disagreement between
	-- the two is resolved in the stream's favour and is a bug in the writer.
	--
	-- Derived figures are deliberately absent. it/s is steps_done over the
	-- span, computed in the query: a stored quotient is a number that can be
	-- wrong, and money is stored as microdollars and formatted late for the
	-- same reason.
	started_unix INTEGER NOT NULL DEFAULT 0,
	ended_unix   INTEGER NOT NULL DEFAULT 0,
	cost         INTEGER NOT NULL DEFAULT 0,
	images       INTEGER NOT NULL DEFAULT 0,
	bytes        INTEGER NOT NULL DEFAULT 0,

	-- passes is how many sampling passes were observed, and steps_done the
	-- steps across all of them.
	--
	-- Two numbers because one would lie. A pass restarts for each batch item,
	-- each high- and low-noise stage and each highres pass, so jobs.steps --
	-- what was asked for -- is the steps of ONE pass and the real iteration
	-- count is that multiplied by however many there were. An it/s computed
	-- from jobs.steps understates every batched or highres job.
	--
	-- steps_done is summed from each pass's own declared total rather than
	-- counted from the frames ember saw, and that distinction matters: ember
	-- deduplicates progress on the step, so at a poll slower than the card
	-- samples it OBSERVES fewer frames than there were steps. A count of
	-- frames would be a measurement of the poll interval. The pass count is
	-- observed, so a pass shorter than one poll could in principle be missed.
	passes       INTEGER NOT NULL DEFAULT 0,
	steps_done   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (req, idx)
);

CREATE TABLE IF NOT EXISTS events (
	req   TEXT    NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
	seq   INTEGER NOT NULL,
	kind  TEXT    NOT NULL,
	body  TEXT    NOT NULL,
	PRIMARY KEY (req, seq)
);

CREATE TABLE IF NOT EXISTS images (
	req      TEXT    NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
	job      INTEGER NOT NULL,
	idx      INTEGER NOT NULL,
	preview  INTEGER NOT NULL DEFAULT 0,
	key      TEXT    NOT NULL,
	bytes    INTEGER NOT NULL DEFAULT 0,
	at_unix  INTEGER NOT NULL,
	PRIMARY KEY (req, job, idx, preview)
);

-- What a generation server reported it can do, one row per rental shape.
--
-- A TABLE AND NOT A KEY IN meta, because the thing that was wrong with the
-- meta row was that it had no key: it was one row per PROCESS, written by
-- whichever rental came up last, and read to refuse a job for a request that
-- may have asked for a different card entirely. A composite key spells the
-- shape a request can name -- a kind, a provider and a card -- and a primary
-- key over the three is what makes "two shapes, two answers" a property of
-- the schema rather than of a string somebody builds.
--
-- THE CARD HERE IS THE ASK AND NOT THE SILICON. The only reader is enqueue
-- validation, which runs before any node exists, so the ask is all it can
-- look up by; a request that named no card reads the row left by the last
-- rental that also named none. Keying on the card ember actually rented would
-- make every row unreadable by the one caller that needs them.
--
-- kind is stored through KindOf, so '' and 'sd' cannot become two rows for
-- one shape -- which would be the empty-means-sd defect with a cache in front
-- of it.
--
-- at_unix is what the row was learned at. Nothing reads it to expire a row:
-- a capability document is about a server build and a card, and both are the
-- same next time that shape is rented. It is there because a cache nobody can
-- date is a cache nobody can debug.
CREATE TABLE IF NOT EXISTS caps (
	kind     TEXT    NOT NULL,
	provider TEXT    NOT NULL,
	gpu      TEXT    NOT NULL,
	body     TEXT    NOT NULL,
	at_unix  INTEGER NOT NULL,
	PRIMARY KEY (kind, provider, gpu)
);
`

// migrate creates the schema, brings an older file forward and refuses a file
// from the future.
func (s *SQLite) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create the schema in %s: %w", s.path, err)
	}

	var have string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&have)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A fresh file: the schema above already created the current shape,
		// so there is nothing to migrate and the version is simply recorded.
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES ('schema_version', ?)`, fmt.Sprint(schemaVersion))
		if err != nil {
			return fmt.Errorf("record the schema version in %s: %w", s.path, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read the schema version from %s: %w", s.path, err)
	}

	var v int
	if _, serr := fmt.Sscanf(have, "%d", &v); serr != nil {
		return fmt.Errorf("%s has an unreadable schema version %q", s.path, have)
	}
	if v > schemaVersion {
		return fmt.Errorf("%s was written by a newer ember (schema %d, this one understands %d); "+
			"it is refused rather than read, because reading a newer layout as an older one drops "+
			"columns and the one it would drop is the one that says a request is still running",
			s.path, v, schemaVersion)
	}
	if v < 1 {
		return fmt.Errorf("%s reports schema version %d, which no ember wrote", s.path, v)
	}
	if v == schemaVersion {
		return nil
	}

	// All of it or none of it. A file stopped halfway between two versions
	// reports the old one and has some of the new one, which is the state
	// nothing here could diagnose later.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the migration of %s: %w", s.path, err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := v; i < schemaVersion; i++ {
		for _, stmt := range migrations[i-1] {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migrate %s from schema %d to %d: %w", s.path, i, i+1, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = 'schema_version'`, fmt.Sprint(schemaVersion)); err != nil {
		return fmt.Errorf("record schema version %d in %s: %w", schemaVersion, s.path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the migration of %s: %w", s.path, err)
	}
	return nil
}

// NewReqID is a random, sortable-enough identifier.
//
// Random and not sequential, because an id that leaks a count is an id that
// tells anyone who sees one URL how many others exist. Ordering comes from
// created_unix, which is what ORDER BY uses.
func NewReqID() ReqID {
	var b [8]byte
	// crypto/rand.Read does not fail on any platform ember runs on, and the
	// signature kept the error for history; math/rand would be wrong here
	// because these end up in URLs.
	_, _ = rand.Read(b[:])
	return ReqID(hex.EncodeToString(b[:]))
}

// Enqueue records a request as queued.
func (s *SQLite) Enqueue(ctx context.Context, r Request) (ReqID, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	if r.ID == "" {
		r.ID = NewReqID()
	}
	if r.Created.IsZero() {
		r.Created = s.at()
	}
	// Queued whatever the caller said. Enqueue is how a request *enters* the
	// queue, and letting a caller enqueue something already "done" would put
	// a row in the history that never happened.
	r.Status = StatusQueued

	if err := s.insert(ctx, r, time.Time{}, time.Time{}); err != nil {
		return "", err
	}
	return r.ID, nil
}

// Adopt records a run that is already happening.
//
// StatusRunning and not StatusQueued, which is the whole reason this is not
// Enqueue: see the Store interface for what a claimable `ember up` row would
// cost. Started is set here because the run has started -- this is called
// before the create call -- so Entry.Wall is its own span from the beginning
// rather than from whenever something first wrote to the row.
func (s *SQLite) Adopt(ctx context.Context, r Request) (ReqID, error) {
	if err := r.ValidateRun(); err != nil {
		return "", err
	}
	if r.ID == "" {
		r.ID = NewReqID()
	}
	if r.Created.IsZero() {
		r.Created = s.at()
	}
	r.Status = StatusRunning
	started := r.Started
	if started.IsZero() {
		started = r.Created
	}

	// THE LEASE IS THE RUN'S OWN DEADLINE, because this process has no
	// renewal loop: see Store.Adopt for why `adopted + TTL` is the honest
	// bound and what the width of the create call costs it.
	if err := s.insert(ctx, r, started, r.Created.Add(r.TTL)); err != nil {
		return "", err
	}
	return r.ID, nil
}

// insert writes the ask, and nothing a run produces.
//
// ONE SPELLING FOR TWO ENTRY POINTS. Enqueue and Adopt differ by the status
// and by whether the run has already started, and in nothing else -- so the
// column list, the job rows and the parameter encoding live here. Two
// statements would be two places to remember what this one deliberately does
// NOT name: cost, rate, node, run_id, the memory columns and the model are
// written by whatever watched the run, so a caller that set them on the way in
// would have them dropped. That is the same structural refusal that keeps
// Extra off Job, applied to the columns an operator is billed against.
// leaseUntil IS A PARAMETER AND NOT A COLUMN OF r, for the reason the lease is
// absent from genstore.Request: a caller that could name its own expiry could
// name one in the far future, which is the same as having no lease at all.
// Enqueue passes the zero time, because a QUEUED row is held by nobody -- the
// lease begins at the claim.
func (s *SQLite) insert(ctx context.Context, r Request, started, leaseUntil time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO requests
			(id, created_unix, status, kind, workload, provider, gpu, offer, ttl_ns, max_hourly,
			 max_run, preview, preview_every, started_unix, lease_until_unix)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(r.ID), r.Created.UnixMilli(), string(r.Status), r.Kind, r.Workload,
		r.Provider, r.GPU, r.Offer,
		int64(r.TTL), int64(r.MaxHourly), int64(r.MaxRun), r.Preview, r.PreviewInterval,
		unixOrZero(started), unixOrZero(leaseUntil))
	if err != nil {
		return fmt.Errorf("record request %s: %w", r.ID, err)
	}

	for i, j := range r.Jobs {
		// Marshalled here rather than in a helper on Job, because the error
		// belongs to this write: a job whose parameters cannot be encoded
		// must not be half-recorded, and the caller needs to be told which
		// job it was.
		params, err := json.Marshal(j.Params)
		if err != nil {
			return fmt.Errorf("encode the parameters of job %d of request %s: %w", i+1, r.ID, err)
		}
		// Marshalled here for params' reason, and '' rather than 'null' for a
		// job that has none: the column is NOT NULL, and "no utterance" and
		// "the empty string" being one fact is what keeps every scan below a
		// plain string.
		ask := ""
		if j.TTS != nil {
			b, err := json.Marshal(j.TTS)
			if err != nil {
				return fmt.Errorf("encode the utterance of job %d of request %s: %w", i+1, r.ID, err)
			}
			ask = string(b)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO jobs
				(req, idx, prompt, negative, width, height, steps, batch_count, seed,
				 sampler, scheduler, cfg, guidance, output, params, tts)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			string(r.ID), i+1, j.Prompt, j.NegativePrompt, j.Width, j.Height, j.Steps,
			j.BatchCount, j.Seed, j.Sampler, j.Scheduler, j.CFG, j.Guidance, j.Output,
			params, ask)
		if err != nil {
			return fmt.Errorf("record job %d of request %s: %w", i+1, r.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit request %s: %w", r.ID, err)
	}
	return nil
}

// requestColumns is the one spelling of the SELECT list.
//
// Named once because scanRequest reads it positionally: a column added to one
// and not the other silently shifts every field after it, and the field that
// shifts is whichever one happens to be next.
const requestColumns = `id, created_unix, status, kind, provider, gpu, offer, ttl_ns,
	max_hourly, max_run, preview, preview_every, run_id, node,
	started_unix, ended_unix, cost, rate, elapsed_ns, billable_ns,
	load_measured_ns, load_reported_ns,
	mem_cgroup, mem_limit, mem_current, mem_total, mem_cached, asset_bytes,
	model, err, workload`

type scanner interface {
	Scan(dest ...any) error
}

func scanRequest(sc scanner) (Request, error) {
	var (
		r                             Request
		id, status                    string
		created, started, ended       int64
		ttl, hourly, maxRun, cost     int64
		rate, elapsed, billable       int64
		loadMeasured, loadReported    int64
		memLimit, memCurrent          int64
		memTotal, memCached, assets   int64
		memCgroup                     int
		provider, gpu, offer          string
		preview, runID, nodeID, errTx string
		model, kind, workload         string
		previewEvery                  int
	)
	if err := sc.Scan(&id, &created, &status, &kind, &provider, &gpu, &offer, &ttl,
		&hourly, &maxRun, &preview, &previewEvery, &runID, &nodeID,
		&started, &ended, &cost, &rate, &elapsed, &billable,
		&loadMeasured, &loadReported,
		&memCgroup, &memLimit, &memCurrent, &memTotal, &memCached, &assets,
		&model, &errTx, &workload); err != nil {
		return Request{}, err
	}
	r = Request{
		ID:              ReqID(id),
		Created:         time.UnixMilli(created).UTC(),
		Status:          Status(status),
		Kind:            kind,
		Workload:        workload,
		Provider:        provider,
		GPU:             gpu,
		Offer:           offer,
		TTL:             time.Duration(ttl),
		MaxHourly:       node.Rate(hourly),
		MaxRun:          node.MicroUSD(maxRun),
		Preview:         preview,
		PreviewInterval: previewEvery,
		RunID:           runID,
		Node:            nodeID,
		Cost:            node.MicroUSD(cost),
		Rate:            node.Rate(rate),
		Elapsed:         time.Duration(elapsed),
		Billable:        time.Duration(billable),
		LoadMeasured:    time.Duration(loadMeasured),
		LoadReported:    time.Duration(loadReported),
		MemCgroup:       memCgroup,
		MemLimit:        memLimit,
		MemCurrent:      memCurrent,
		MemTotal:        memTotal,
		MemCached:       memCached,
		AssetBytes:      assets,
		Model:           model,
		Err:             errTx,
	}
	// Zero stays zero rather than becoming the epoch: "this never started"
	// and "this started in 1970" are different claims, and a UI showing the
	// second is a UI nobody believes.
	if started != 0 {
		r.Started = time.UnixMilli(started).UTC()
	}
	if ended != 0 {
		r.Ended = time.UnixMilli(ended).UTC()
	}
	return r, nil
}

func (s *SQLite) jobsOf(ctx context.Context, q scannerQuerier, id ReqID) ([]Job, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT prompt, negative, width, height, steps, batch_count, seed,
		       sampler, scheduler, cfg, guidance, output, params, tts
		FROM jobs WHERE req = ? ORDER BY idx`, string(id))
	if err != nil {
		return nil, fmt.Errorf("read the jobs of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Job
	for rows.Next() {
		var j Job
		var params, ask string
		if err := rows.Scan(&j.Prompt, &j.NegativePrompt, &j.Width, &j.Height, &j.Steps,
			&j.BatchCount, &j.Seed, &j.Sampler, &j.Scheduler, &j.CFG, &j.Guidance, &j.Output,
			&params, &ask); err != nil {
			return nil, fmt.Errorf("read a job of %s: %w", id, err)
		}
		// Reported rather than swallowed. The column is written by this
		// package and holds `{}` for a job with nothing set, so unreadable
		// JSON here means the file was edited or corrupted -- and a request
		// silently losing the parameters it was queued with would generate
		// at the wrong settings and look like it worked.
		if err := json.Unmarshal([]byte(params), &j.Params); err != nil {
			return nil, fmt.Errorf("read the parameters of a job of %s: %w", id, err)
		}
		// Reported rather than swallowed, for params' reason one column over,
		// and the consequence here is sharper: a speech job whose utterance
		// did not decode would be a job with no text, which the queue would
		// refuse at the node rather than here -- after a card was rented.
		if ask != "" {
			var a tts.Ask
			if err := json.Unmarshal([]byte(ask), &a); err != nil {
				return nil, fmt.Errorf("read the utterance of a job of %s: %w", id, err)
			}
			j.TTS = &a
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

type scannerQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Get returns one request with its jobs.
func (s *SQLite) Get(ctx context.Context, id ReqID) (Request, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+requestColumns+` FROM requests WHERE id = ?`, string(id))
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Request{}, fmt.Errorf("read request %s: %w", id, err)
	}
	r.Jobs, err = s.jobsOf(ctx, s.db, id)
	if err != nil {
		return Request{}, err
	}
	return r, nil
}

// List returns requests newest first.
func (s *SQLite) List(ctx context.Context, f Filter) ([]Request, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	// Through filterWhere, the same clause builder History uses, so a listing
	// and the ledger cannot come to disagree about what --status or a period
	// bound selects.
	//
	where, args := filterWhere(f)
	//nolint:gosec // G202: where is fixed clause text from filterWhere; every value is a ? parameter
	q := `SELECT ` + requestColumns + ` FROM requests r ` + where +
		` ORDER BY created_unix DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("read a request: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The jobs are read per request rather than in one join, because a join
	// would return one row per job and the listing would have to de-duplicate
	// requests by hand -- which is where an off-by-one loses the last job of
	// the last request.
	for i := range out {
		js, err := s.jobsOf(ctx, s.db, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Jobs = js
	}
	return out, nil
}

// servesClause is the claim relation as SQL predicates over the requests
// table, and the args that go with it: could the rental c describes run this
// row.
//
// ONE BUILDER, TWO CALLERS, WHICH IS THE POINT. ClaimNext reads it for the
// ride side and reads its negation once per live rental for the create side,
// so the relation a rental claims by and the relation the supervisor excludes
// by cannot drift. They used to be two copies of one condition in one
// function, and the two ways that drifts are both expensive: an exclusion
// WIDER than the ride relation starves a request for the length of a rental,
// and one NARROWER hands it a second card. It is also what makes the
// uncertainty about a live rental's card harmless -- an unadvertised card
// narrows both sides together. See genstore.Compat.Card.
//
// THREE ADMISSIONS FOR THE CARD, and only the third needs the advertised fact:
//
//   - the request named no card, so any card this provider has satisfies it;
//   - the request named exactly what this rental asked for, which is the whole
//     of the rule before Compat.Card existed;
//   - the request named something this rental's actual card Accepts, which is
//     the capacity-suffix rule node.GPUModel owns -- "a100" is satisfied by
//     "a100-80g" -- and is also how a rental that asked for nothing serves a
//     request that asked for the card it happens to have got.
//
// THE FIRST ADMISSION DOES NOT CHECK THE REQUEST'S OWN PRICE CEILING, said
// rather than left to be found. A request naming max_hourly_usd below the rate
// of the rental it rides is served anyway, because riding spends nothing of
// its own: the rental exists, its cost is the opener's, and the period
// ceilings were projected against the opener's whole TTL before the node was
// created. Refusing the ride would mean renting a second card, which is the
// one outcome that does cost money.
//
// THE ASK IS COMPARED AS STORED, which bounds the card admission. webapi
// normalises a card name on the way in, so new rows hold what
// node.NormalizeGPU produces and cardAdmits speaks the same spelling -- but a
// row written before that does not, and for one of those only the first two
// admissions can fire. That is exactly the packing it had before this field,
// and the alternative is rewriting rows, which is the thing this schema never
// does.
//
// THE KIND IS COMPARED THROUGH A CASE AND NOT AS A COLUMN, which is the one
// place the empty-means-sd rule has to reach into SQL. A plain `kind = ?`
// would refuse every row written before schema version 6 to an sd rental --
// they hold ” and the compat holds 'sd' -- so a restart after an upgrade
// would leave an operator's queue permanently unclaimable. Getting it wrong
// on the exclusion side is the expensive direction: a migrated row would read
// as incompatible with the node already serving its siblings and would be
// given a SECOND card. Rewriting the rows was the alternative and is the
// thing this schema never does.
//
// THE CARD IS AN IN-LIST AND NOT A LIKE, because node.GPUModel.Accepts is not
// a string operation this dialect has: it admits the exact name and the name
// with its capacity suffix cut off at a '-', which is a SET of at most a
// handful of spellings and is computed in Go by cardAdmits. A `LIKE gpu ||
// '-%'` would push the rule into SQL, where `_` is a wildcard and a card name
// containing one would quietly match more than it should -- parse, don't
// grep, and never build a pattern out of stored text.
func servesClause(c Compat) (string, []any) {
	args := []any{KindSD, WorkloadOf(c.Workload, c.Kind), c.Provider, c.GPU}
	cards := cardAdmits(c)
	in := ""
	for _, name := range cards {
		in += " OR gpu = ?"
		args = append(args, name)
	}
	// THE WORKLOAD IS COMPARED THROUGH WorkloadOf AND NOT AS A COLUMN, which
	// is the one place the two empty-means rules reach into SQL. A plain
	// `AND workload = ?` would refuse every row written before schema version
	// 8 to any rental at all -- they hold '' and the compat holds an entry id
	// -- so a restart after an upgrade would leave an operator's queue
	// permanently unclaimable. Rewriting the rows was the alternative and is
	// the thing this schema never does.
	//
	// The nested CASE is WorkloadOf spelled in SQL: an empty workload falls
	// back to the kind, and an empty kind to 'sd'. Both stores spell the same
	// rule, which is what the conformance table exists to hold.
	return `((CASE WHEN workload = '' THEN ` +
		`(CASE WHEN kind = '' THEN ? ELSE kind END) ELSE workload END) = ? AND provider = ?` +
		` AND (gpu = '' OR gpu = ?` + in + `))`, args
}

// servesPendingClause is servesClause widened by what is not known yet, which
// is the create side's question: MIGHT this rental absorb that row.
//
// A SECOND CLAUSE AND NOT A FLAG, because the two are read in opposite
// directions and a reader has to see which one they have. For a rental whose
// offer has not come back the answer is yes for everything of its kind and
// provider, and the money-safe reading of "might" on the side that spends
// money is "does". See genstore.Compat.Card for the measurement that made this
// mandatory rather than a refinement, and for what the stall costs.
//
// IT CANNOT STARVE A ROW. The widening ends when the card arrives or when the
// slot exits, and both of those kick the supervisor -- Queue.advertise and
// Queue.freeSlot -- so a row this excluded is reconsidered within one offer
// lookup rather than at the next poll.
func servesPendingClause(c Compat) (string, []any) {
	if !c.CardKnown {
		return `((CASE WHEN workload = '' THEN ` +
				`(CASE WHEN kind = '' THEN ? ELSE kind END) ELSE workload END) = ? ` +
				`AND provider = ?)`,
			[]any{KindSD, WorkloadOf(c.Workload, c.Kind), c.Provider}
	}
	return servesClause(c)
}

// cardAdmits is every ask this rental's actual card satisfies beyond the
// rental's own, which is node.GPUModel.Accepts enumerated.
//
// Accepts admits `want` when it equals the card or when it is the card with a
// capacity suffix cut off at a '-': "a100" is satisfied by "a100-80g". So the
// set is the card itself plus each of its '-'-separated prefixes, which is at
// most two spellings for every card ember has seen and is bounded by the name.
// Enumerating rather than pattern-matching is what keeps the one definition of
// Accepts in internal/node.
func cardAdmits(c Compat) []string {
	if c.Card == "" {
		return nil
	}
	name := string(c.Card)
	out := []string{name}
	for i := len(name) - 1; i > 0; i-- {
		if name[i] != '-' {
			continue
		}
		// Through Accepts itself rather than trusted, so a change to that
		// rule cannot leave this enumerating spellings it no longer admits.
		if c.Card.Accepts(node.GPUModel(name[:i])) {
			out = append(out, name[:i])
		}
	}
	return out
}

// ClaimNext takes the oldest queued request c can serve.
//
// ONE STATEMENT, which is the entire reason this is a method and not two reads
// in the caller. The drain loop's question is "serve another or destroy the
// node", and a read followed by a write leaves a window where a request
// pushed between them is neither claimed nor counted: the node is destroyed
// with work outstanding, and the operator's request sits in a queue nothing
// is draining.
func (s *SQLite) ClaimNext(ctx context.Context, runID string, c Compat) (Request, bool, error) {
	// The subselect picks the row; the UPDATE claims it. SQLite evaluates
	// both inside one implicit transaction, so no other connection can take
	// the same row -- and there is only one connection anyway, by
	// SetMaxOpenConns above.
	const pick = `
		UPDATE requests SET status = ?, run_id = ?, started_unix = ?, lease_until_unix = ?
		WHERE id = (
			SELECT id FROM requests
			WHERE status = ?%s
			ORDER BY created_unix, id LIMIT 1
		)
		RETURNING ` + requestColumns

	now := s.at()
	where, args := "", []any{
		string(StatusRunning), runID, now.UnixMilli(),
		// THE LEASE, WRITTEN IN THE SAME STATEMENT AS THE CLAIM. A row
		// claimed in one statement and stamped with its expiry in a second
		// would have a window in which it reads `running` with a zero lease,
		// and a zero lease is an expired one -- so a Reclaim landing there
		// would take the row back from a rental that had just got it.
		now.Add(LeaseTerm).UnixMilli(),
		string(StatusQueued),
	}
	if !c.IsZero() {
		clause, cargs := servesClause(c)
		where = " AND " + clause
		args = append(args, cargs...)
	}
	// AND ONE EXCLUSION PER LIVE RENTAL, which is the create side of
	// fit-or-create and is still this one statement. See Compat.Live: the
	// supervisor asks for the oldest queued request that none of the rentals
	// already up can serve, so a request a busy rental could take waits for it
	// instead of being given a card of its own.
	//
	// THE NEGATION OF THE SAME BUILDER, which is what keeps the two sides one
	// relation. They used to be two copies of one condition, and the
	// expensive direction is this one: a ride predicate that is narrower than
	// the exclusion starves a request for the length of a rental, and an
	// exclusion narrower than the ride hands a request a SECOND card.
	for _, l := range c.Live {
		clause, cargs := servesPendingClause(l)
		where += " AND NOT " + clause
		args = append(args, cargs...)
	}

	row := s.db.QueryRowContext(ctx, fmt.Sprintf(pick, where), args...)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, fmt.Errorf("claim the next request: %w", err)
	}
	r.Jobs, err = s.jobsOf(ctx, s.db, r.ID)
	if err != nil {
		return Request{}, false, err
	}
	return r, true, nil
}

// Update applies fn to a request and writes it back.
func (s *SQLite) Update(ctx context.Context, id ReqID, fn func(*Request)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`SELECT `+requestColumns+` FROM requests WHERE id = ?`, string(id))
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("read request %s: %w", id, err)
	}

	fn(&r)
	if !r.Status.Valid() {
		return fmt.Errorf("request %s: status %q is not one ember writes", id, r.Status)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE requests SET status = ?, offer = ?, run_id = ?, node = ?,
			started_unix = ?, ended_unix = ?, cost = ?,
			rate = ?, elapsed_ns = ?, billable_ns = ?,
			load_measured_ns = ?, load_reported_ns = ?,
			mem_cgroup = ?, mem_limit = ?, mem_current = ?,
			mem_total = ?, mem_cached = ?, asset_bytes = ?,
			model = ?, err = ?
		WHERE id = ?`,
		string(r.Status), r.Offer, r.RunID, r.Node,
		unixOrZero(r.Started), unixOrZero(r.Ended), int64(r.Cost),
		int64(r.Rate), int64(r.Elapsed), int64(r.Billable),
		int64(r.LoadMeasured), int64(r.LoadReported),
		r.MemCgroup, r.MemLimit, r.MemCurrent,
		r.MemTotal, r.MemCached, r.AssetBytes,
		r.Model, r.Err, string(id))
	if err != nil {
		return fmt.Errorf("update request %s: %w", id, err)
	}
	return tx.Commit()
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// AppendEvents records progress.
//
// INSERT OR REPLACE on (req, seq) rather than a plain INSERT, because a
// re-delivered event is not an error: a writer that crashed between emitting
// and committing will emit the same sequence number again, and a unique
// violation there would turn a resumed run into a failed one.
func (s *SQLite) AppendEvents(ctx context.Context, id ReqID, es []report.Event) error {
	if len(es) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO events (req, seq, kind, body) VALUES (?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare the event insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range es {
		body, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode event %d of %s: %w", e.Seq, id, err)
		}
		if _, err := stmt.ExecContext(ctx, string(id), e.Seq, string(e.Kind), string(body)); err != nil {
			return fmt.Errorf("record event %d of %s: %w", e.Seq, id, err)
		}
	}
	return tx.Commit()
}

// Events returns this request's events after a sequence number.
//
// The cursor is what makes a reconnect lossless: a browser says "I have seen
// up to 41" and gets everything since, whether it was away for a second or
// for an hour. A ring buffer in memory could not answer that question once it
// had wrapped.
func (s *SQLite) Events(ctx context.Context, id ReqID, after int64) ([]report.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT body FROM events WHERE req = ? AND seq > ? ORDER BY seq`, string(id), after)
	if err != nil {
		return nil, fmt.Errorf("read the events of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []report.Event
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("read an event of %s: %w", id, err)
		}
		var e report.Event
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("decode an event of %s: %w", id, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddImage records a file a request produced.
//
// INSERT OR REPLACE, because a preview is overwritten as sampling proceeds:
// the same (request, job, index, preview) key is written once per distinct
// frame, and the row that matters is the last one.
func (s *SQLite) AddImage(ctx context.Context, im Image) error {
	if im.Key == "" {
		return fmt.Errorf("an image of %s has no blob key, so nothing could find its bytes", im.Req)
	}
	at := im.At
	if at.IsZero() {
		at = s.at()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO images (req, job, idx, preview, key, bytes, at_unix)
		VALUES (?,?,?,?,?,?,?)`,
		string(im.Req), im.Job, im.Index, boolInt(im.Preview), im.Key, im.Bytes, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("record image %s of %s: %w", im.Key, im.Req, err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Images returns what a request produced, results before previews.
func (s *SQLite) Images(ctx context.Context, id ReqID) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT job, idx, preview, key, bytes, at_unix
		FROM images WHERE req = ? ORDER BY preview, job, idx`, string(id))
	if err != nil {
		return nil, fmt.Errorf("read the images of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Image
	for rows.Next() {
		im := Image{Req: id}
		var preview int
		var at int64
		if err := rows.Scan(&im.Job, &im.Index, &preview, &im.Key, &im.Bytes, &at); err != nil {
			return nil, fmt.Errorf("read an image of %s: %w", id, err)
		}
		im.Preview = preview != 0
		im.At = time.UnixMilli(at).UTC()
		out = append(out, im)
	}
	return out, rows.Err()
}

// PutCaps caches what the generation server said it can do, for one shape.
func (s *SQLite) PutCaps(ctx context.Context, sh Shape, c sdserver.Capabilities) error {
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode the capabilities: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO caps (kind, provider, gpu, body, at_unix) VALUES (?, ?, ?, ?, ?)`,
		KindOf(sh.Kind), sh.Provider, sh.GPU, string(body), s.at().UnixMilli())
	if err != nil {
		return fmt.Errorf("record the capabilities: %w", err)
	}
	return nil
}

// Caps reads the cached capabilities for one shape back. The second return is
// false when nothing has learned them yet, which is every shape before its
// first rental.
func (s *SQLite) Caps(ctx context.Context, sh Shape) (sdserver.Capabilities, bool, error) {
	var body string
	err := s.db.QueryRowContext(ctx,
		`SELECT body FROM caps WHERE kind = ? AND provider = ? AND gpu = ?`,
		KindOf(sh.Kind), sh.Provider, sh.GPU).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return sdserver.Capabilities{}, false, nil
	}
	if err != nil {
		return sdserver.Capabilities{}, false, fmt.Errorf("read the capabilities: %w", err)
	}
	var c sdserver.Capabilities
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return sdserver.Capabilities{}, false, fmt.Errorf("decode the capabilities: %w", err)
	}
	return c, true, nil
}

// Renew extends the lease on the rows this caller is still responsible for.
func (s *SQLite) Renew(ctx context.Context, ids []ReqID) (int, error) {
	if len(ids) == 0 {
		// No statement at all, rather than one with an empty IN list. An
		// `IN ()` is a syntax error in this dialect, and the caller with
		// nothing held is the ordinary case: an idle `ember serve` runs this
		// every fifteen seconds.
		return 0, nil
	}
	// IT CAN SHORTEN A LEASE AS WELL AS EXTEND ONE, which is right even
	// though the name says otherwise: an Adopt row starts with the run's own
	// deadline, up to a whole TTL away, and once this process is renewing it
	// the honest expiry is "this process is alive" rather than "the node has
	// not timed out". A `ember up` over the socket killed with SIGKILL is
	// therefore reclaimed in LeaseTerm instead of in an hour.
	//
	// AND status = 'running' IS NOT DECORATION. A renewal races a request's
	// own ending -- the supervisor renews at the top of its loop while a slot
	// is writing `done` from its own goroutine -- and pushing a lease forward
	// on a row that has finished would leave a terminal row carrying a live
	// lease. Nothing reads it there, which is exactly why it would never be
	// noticed.
	//nolint:gosec // G202: the only interpolation is a fixed-text placeholder list sized by len(ids)
	q := `UPDATE requests SET lease_until_unix = ? WHERE status = ? AND id IN (?` +
		strings.Repeat(`, ?`, len(ids)-1) + `)`
	args := []any{s.at().Add(LeaseTerm).UnixMilli(), string(StatusRunning)}
	for _, id := range ids {
		args = append(args, string(id))
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("renew the lease on %d request(s) in %s: %w", len(ids), s.path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count renewed requests: %w", err)
	}
	return int(n), nil
}

// Reclaim marks interrupted every running request whose lease has expired, and
// returns how many.
//
// See Store.Reclaim for why this is keyed on the lease rather than sweeping
// every running row, and why a reclaimed request is not re-queued.
func (s *SQLite) Reclaim(ctx context.Context) (int, error) {
	now := s.at().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE requests SET status = ?, ended_unix = ?
			WHERE status = ? AND lease_until_unix <= ?`,
		string(StatusInterrupted), now, string(StatusRunning), now)
	if err != nil {
		return 0, fmt.Errorf("reclaim expired requests in %s: %w", s.path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count reclaimed requests: %w", err)
	}
	return int(n), nil
}
