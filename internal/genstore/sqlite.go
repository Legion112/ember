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
	"time"

	_ "modernc.org/sqlite" // the pure-Go driver, registered as "sqlite"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// FileName is what the database is called inside the state directory.
//
// Beside internal/statestore's own directory rather than anywhere else, so one
// XDG_STATE_HOME move takes the journal, the pins and this together. They are
// all machine-local facts rather than settings.
const FileName = "generations.db"

// schemaVersion is the on-disk shape.
//
// A file written by a NEWER ember is refused rather than misparsed, for
// internal/statestore's reason: reading a newer layout as an older one drops
// columns, and the column it would drop is the one that says a request is
// still running.
const schemaVersion = 1

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
`

// migrate creates the schema and refuses a file from the future.
func (s *SQLite) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create the schema in %s: %w", s.path, err)
	}

	var have string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&have)
	switch {
	case errors.Is(err, sql.ErrNoRows):
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO requests
			(id, created_unix, status, provider, gpu, offer, ttl_ns, max_hourly, max_run,
			 preview, preview_every)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		string(r.ID), r.Created.UnixMilli(), string(r.Status), r.Provider, r.GPU, r.Offer,
		int64(r.TTL), int64(r.MaxHourly), int64(r.MaxRun), r.Preview, r.PreviewInterval)
	if err != nil {
		return "", fmt.Errorf("record request %s: %w", r.ID, err)
	}

	for i, j := range r.Jobs {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs
				(req, idx, prompt, negative, width, height, steps, batch_count, seed,
				 sampler, scheduler, cfg, guidance, output)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			string(r.ID), i+1, j.Prompt, j.NegativePrompt, j.Width, j.Height, j.Steps,
			j.BatchCount, j.Seed, j.Sampler, j.Scheduler, j.CFG, j.Guidance, j.Output)
		if err != nil {
			return "", fmt.Errorf("record job %d of request %s: %w", i+1, r.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit request %s: %w", r.ID, err)
	}
	return r.ID, nil
}

// requestColumns is the one spelling of the SELECT list.
//
// Named once because scanRequest reads it positionally: a column added to one
// and not the other silently shifts every field after it, and the field that
// shifts is whichever one happens to be next.
const requestColumns = `id, created_unix, status, provider, gpu, offer, ttl_ns,
	max_hourly, max_run, preview, preview_every, run_id, node,
	started_unix, ended_unix, cost, err`

type scanner interface {
	Scan(dest ...any) error
}

func scanRequest(sc scanner) (Request, error) {
	var (
		r                             Request
		id, status                    string
		created, started, ended       int64
		ttl, hourly, maxRun, cost     int64
		provider, gpu, offer          string
		preview, runID, nodeID, errTx string
		previewEvery                  int
	)
	if err := sc.Scan(&id, &created, &status, &provider, &gpu, &offer, &ttl,
		&hourly, &maxRun, &preview, &previewEvery, &runID, &nodeID,
		&started, &ended, &cost, &errTx); err != nil {
		return Request{}, err
	}
	r = Request{
		ID:              ReqID(id),
		Created:         time.UnixMilli(created).UTC(),
		Status:          Status(status),
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
		       sampler, scheduler, cfg, guidance, output
		FROM jobs WHERE req = ? ORDER BY idx`, string(id))
	if err != nil {
		return nil, fmt.Errorf("read the jobs of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.Prompt, &j.NegativePrompt, &j.Width, &j.Height, &j.Steps,
			&j.BatchCount, &j.Seed, &j.Sampler, &j.Scheduler, &j.CFG, &j.Guidance, &j.Output); err != nil {
			return nil, fmt.Errorf("read a job of %s: %w", id, err)
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

	q := `SELECT ` + requestColumns + ` FROM requests`
	args := []any{}
	if f.Status != "" {
		q += ` WHERE status = ?`
		args = append(args, string(f.Status))
	}
	q += ` ORDER BY created_unix DESC, id DESC LIMIT ?`
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
		UPDATE requests SET status = ?, run_id = ?, started_unix = ?
		WHERE id = (
			SELECT id FROM requests
			WHERE status = ?%s
			ORDER BY created_unix, id LIMIT 1
		)
		RETURNING ` + requestColumns

	where, args := "", []any{string(StatusRunning), runID, s.at().UnixMilli(), string(StatusQueued)}
	if !c.IsZero() {
		where = ` AND provider = ? AND gpu = ?`
		args = append(args, c.Provider, c.GPU)
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
			started_unix = ?, ended_unix = ?, cost = ?, err = ?
		WHERE id = ?`,
		string(r.Status), r.Offer, r.RunID, r.Node,
		unixOrZero(r.Started), unixOrZero(r.Ended), int64(r.Cost), r.Err, string(id))
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

// PutCaps caches what the generation server said it can do.
func (s *SQLite) PutCaps(ctx context.Context, c sdserver.Capabilities) error {
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode the capabilities: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('caps', ?)`, string(body))
	if err != nil {
		return fmt.Errorf("record the capabilities: %w", err)
	}
	return nil
}

// Caps reads the cached capabilities back. The second return is false when
// nothing has learned them yet, which is every process before its first
// rental.
func (s *SQLite) Caps(ctx context.Context) (sdserver.Capabilities, bool, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'caps'`).Scan(&body)
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

// Interrupt marks everything running as interrupted, and returns how many.
//
// Called once at startup. A request that was in flight when the process died
// cannot be resumed, because its rental is gone -- so it is not re-queued
// either: renting a second card for work the operator may no longer want is
// the kind of help nobody asked for. Whatever images it produced stay listed,
// because they were paid for.
func (s *SQLite) Interrupt(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE requests SET status = ?, ended_unix = ? WHERE status = ?`,
		string(StatusInterrupted), s.at().UnixMilli(), string(StatusRunning))
	if err != nil {
		return 0, fmt.Errorf("mark interrupted requests in %s: %w", s.path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count interrupted requests: %w", err)
	}
	return int(n), nil
}
