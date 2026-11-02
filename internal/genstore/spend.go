package genstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// The query half of the store, and why it took a second pass to exist.
//
// Everything needed to say what a job cost was already being EMITTED.
// report.Event carries integer-microdollar Cost and Spent, Measured against
// Reported, Elapsed against Billable, the pass and the step within it, the
// files and the bytes. All of it was persisted, into one opaque JSON blob per
// event -- and the jobs table, the row a job IS, had no execution columns at
// all: no started, no ended, no cost, no steps sampled.
//
// So per-job cost and timing existed nowhere ember could query, and the only
// aggregate in the program was a Go loop over one request's entire event
// stream. `grep -rn 'SUM(\|GROUP BY' internal/genstore/` returned nothing.
// Every measurement in README -- the nine rented-card rows, the cmds-against-
// sd.jobs crossover the whole `sd:` block rests on, ember's own 7% per-rental
// overhead -- was computed by hand from a run somebody watched, which means
// nothing trends it and nobody would notice ember's own overhead regressing.
//
// The columns are the fix and this is what reads them. Two rules hold it
// together:
//
// THE EVENT STREAM STAYS THE TRUTH. The columns are a derived index, written
// through from the same events that go into `events`, on the precedent
// requests.node and requests.cost already set. A disagreement between the two
// is resolved in the stream's favour and is a bug in the writer, not a fact
// about the run.
//
// NOTHING DERIVED IS STORED. Cost per image, it/s and the sampling share of
// the bill are methods on Bucket, computed from the columns on the way out. A
// stored quotient is a number that can be wrong while the terms it came from
// are right -- the same reason money is kept as integer microdollars and
// formatted late.

// UpdateJob applies fn to one job's execution facts and writes them back.
func (s *SQLite) UpdateJob(ctx context.Context, id ReqID, idx int, fn func(*JobRun)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT `+jobRunColumns+` FROM jobs WHERE req = ? AND idx = ?`,
		string(id), idx)
	j, err := scanJobRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Named with both halves, because the index is the part that goes
		// wrong: the events report a 1-based job number and the table is
		// keyed on it, so an off-by-one writes nothing and silently leaves
		// every figure at zero.
		return fmt.Errorf("job %d of %s: %w", idx, id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("read job %d of %s: %w", idx, id, err)
	}
	j.Req, j.Idx = id, idx

	fn(&j)

	_, err = tx.ExecContext(ctx, `
		UPDATE jobs SET started_unix = ?, ended_unix = ?, cost = ?,
			images = ?, bytes = ?, passes = ?, steps_done = ?
		WHERE req = ? AND idx = ?`,
		unixOrZero(j.Started), unixOrZero(j.Ended), int64(j.Cost),
		j.Images, j.Bytes, j.Passes, j.Steps, string(id), idx)
	if err != nil {
		return fmt.Errorf("update job %d of %s: %w", idx, id, err)
	}
	return tx.Commit()
}

// JobRuns returns what each of a request's jobs did.
func (s *SQLite) JobRuns(ctx context.Context, id ReqID) ([]JobRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT idx, `+jobRunColumns+` FROM jobs WHERE req = ? ORDER BY idx`, string(id))
	if err != nil {
		return nil, fmt.Errorf("read what the jobs of %s did: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []JobRun
	for rows.Next() {
		var (
			j              JobRun
			idx            int
			started, ended int64
			cost           int64
		)
		if err := rows.Scan(&idx, &started, &ended, &cost, &j.Images, &j.Bytes,
			&j.Passes, &j.Steps); err != nil {
			return nil, fmt.Errorf("read a job of %s: %w", id, err)
		}
		j.Req, j.Idx, j.Cost = id, idx, node.MicroUSD(cost)
		if started != 0 {
			j.Started = time.UnixMilli(started).UTC()
		}
		if ended != 0 {
			j.Ended = time.UnixMilli(ended).UTC()
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// jobRunColumns is the one spelling of the execution SELECT list, named once
// for the reason requestColumns is: scanJobRun reads it positionally.
const jobRunColumns = `started_unix, ended_unix, cost, images, bytes, passes, steps_done`

func scanJobRun(sc scanner) (JobRun, error) {
	var (
		j              JobRun
		started, ended int64
		cost           int64
	)
	if err := sc.Scan(&started, &ended, &cost, &j.Images, &j.Bytes,
		&j.Passes, &j.Steps); err != nil {
		return JobRun{}, err
	}
	j.Cost = node.MicroUSD(cost)
	// Zero stays zero rather than becoming the epoch, as scanRequest does it:
	// "this never started" and "this started in 1970" are different claims,
	// and the second poisons every duration computed from it.
	if started != 0 {
		j.Started = time.UnixMilli(started).UTC()
	}
	if ended != 0 {
		j.Ended = time.UnixMilli(ended).UTC()
	}
	return j, nil
}

// Spend aggregates what was spent.
//
// One statement with a subquery per side, and not a single join, because the
// two sides have different cardinalities: a request has many jobs, so joining
// them and summing requests.cost would multiply each rental's cost by its job
// count. That is the kind of wrong that looks plausible -- a bill several
// times too large, in a figure nobody has another copy of to check against.
func (s *SQLite) Spend(ctx context.Context, q SpendQuery) ([]Bucket, error) {
	if !q.By.Valid() {
		return nil, fmt.Errorf("cannot group spend by %q; ember groups by %v or not at all",
			q.By, GroupBys())
	}

	key, group := groupExpr(q.By)
	where, args := spendWhere(q)

	// The job-side sums come from a subquery keyed on the request, so each
	// rental's own cost is counted once per request row rather than once per
	// job.
	stmt := `
		SELECT ` + key + ` AS k,
		       COUNT(*),
		       COALESCE(SUM(r.cost), 0),
		       COALESCE(SUM(r.billable_ns), 0),
		       COALESCE(SUM(j.n), 0),
		       COALESCE(SUM(j.images), 0),
		       COALESCE(SUM(j.bytes), 0),
		       COALESCE(SUM(j.cost), 0),
		       COALESCE(SUM(j.steps), 0),
		       COALESCE(SUM(j.span), 0),
		       SUM(CASE WHEN COALESCE(j.recorded, 0) = 0 THEN 1 ELSE 0 END)
		FROM requests r
		LEFT JOIN (
			SELECT req,
			       COUNT(*) AS n,
			       SUM(images) AS images,
			       SUM(bytes) AS bytes,
			       SUM(cost) AS cost,
			       SUM(steps_done) AS steps,
			       SUM(CASE WHEN started_unix > 0 AND ended_unix >= started_unix
			                THEN (ended_unix - started_unix) * 1000000 ELSE 0 END) AS span,
			       SUM(CASE WHEN ended_unix > 0 THEN 1 ELSE 0 END) AS recorded
			FROM jobs GROUP BY req
		) j ON j.req = r.id
		` + where + `
		GROUP BY k
		ORDER BY k`

	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("sum what was spent: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Bucket
	for rows.Next() {
		var (
			b                        Bucket
			runCost, billable        int64
			jobCost, bytes, steps    int64
			span                     int64
			jobs, images, unrecorded sql.NullInt64
		)
		if err := rows.Scan(&b.Key, &b.Requests, &runCost, &billable,
			&jobs, &images, &bytes, &jobCost, &steps, &span, &unrecorded); err != nil {
			return nil, fmt.Errorf("read a spend row: %w", err)
		}
		b.Jobs = int(jobs.Int64)
		b.Images = int(images.Int64)
		b.Bytes = bytes
		b.RunCost = node.MicroUSD(runCost)
		b.JobCost = node.MicroUSD(jobCost)
		b.Steps = steps
		b.Sampling = time.Duration(span)
		b.Billable = time.Duration(billable)
		b.Unrecorded = int(unrecorded.Int64)
		if !group {
			// ByTotal selects a literal so the GROUP BY has something to
			// group on. Handing that back would put an empty column in front
			// of every figure, so only a grouped query keeps its key.
			b.Key = ""
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// groupExpr is the SELECT expression for a grouping, and whether it names
// anything.
//
// A switch over the closed set rather than the string itself, so no caller's
// value reaches the statement. GroupBy.Valid has already refused anything
// else; this is the second half of the same seal, and it is what makes the
// concatenation above safe to read.
func groupExpr(g GroupBy) (expr string, named bool) {
	switch g {
	case ByProvider:
		return "r.provider", true
	case ByGPU:
		// The empty string rather than NULL for a request that named no card:
		// it groups with the other CPU-only runs instead of becoming its own
		// row with a blank key, and "" is what the column already holds.
		return "r.gpu", true
	case ByDay:
		// UTC, and said so in the format, because a day boundary in local
		// time makes the same spend land in different buckets depending on
		// where the operator was sitting.
		return "strftime('%Y-%m-%d', r.created_unix / 1000, 'unixepoch')", true
	default:
		return "''", false
	}
}

// spendWhere bounds the period, with the bounds as parameters.
//
// Half-open: Since is inclusive and Until exclusive, so consecutive periods
// tile without a request landing in both. A zero bound is unbounded at that
// end, which is what "what has ember ever cost" asks.
func spendWhere(q SpendQuery) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	if !q.Since.IsZero() {
		clauses = append(clauses, "r.created_unix >= ?")
		args = append(args, q.Since.UnixMilli())
	}
	if !q.Until.IsZero() {
		clauses = append(clauses, "r.created_unix < ?")
		args = append(args, q.Until.UnixMilli())
	}
	if len(clauses) == 0 {
		return "", nil
	}
	w := "WHERE " + clauses[0]
	for _, c := range clauses[1:] {
		w += " AND " + c
	}
	return w, args
}
