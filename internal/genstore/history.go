package genstore

import (
	"context"
	"fmt"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// The ledger, and why it is a second reader rather than a wider List.
//
// internal/genstore was WRITE-ONLY from the command line. The queue filled it,
// the web API served one request's rows to a browser, and
// `grep -rln genstore internal/cli/ cmd/` named one file -- cmd_serve.go,
// which only starts the queue. So the only way to see what ember had run was
// to start a server that can rent a card, open a browser, and click; or to
// open SQLite by hand, which is how the sequence-number bug was found and is a
// poor way to answer a recurring question.
//
// `ember spend` (#32) made the money queryable in aggregate. This is the other
// half: the row-by-row ledger, which is what an operator reads when the
// aggregate surprises them.
//
// ONE STATEMENT, NOT ONE PER ROW. List reads the jobs of each request in a
// second query, deliberately -- it answers "what is in the queue" and the
// prompts are the answer. A ledger over a month would issue a query per
// request for prompts it never prints. The job-side figures come from
// jobSideByRequest, the same roll-up Spend sums, so the two cannot come to
// disagree about what one run produced.
//
// IT CANNOT WRITE. There is no statement here but a SELECT, and nothing on the
// path from `ember history` to this method takes a provider or a writer. "The
// ledger must not be able to enqueue, claim or rent" is therefore a property
// of the shape rather than a rule somebody has to keep.

// History returns the ledger, newest first.
func (s *SQLite) History(ctx context.Context, f Filter) ([]Entry, error) {
	if f.Status != "" && !f.Status.Valid() {
		return nil, fmt.Errorf("status %q is not one ember writes", f.Status)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	where, args := filterWhere(f)

	// THE CONCATENATED PIECES ARE CONSTANTS. where comes from filterWhere,
	// which emits fixed clause text and puts every value in args as a `?`;
	// jobSideByRequest is a literal. Nothing an operator types is
	// interpolated, which is the same seal Spend's statement carries.
	//
	// The rode-along test asks whether some OTHER request on this rental
	// carries the rental's cost. Billable as well as cost, because a free
	// rental -- the local provider's price is zero -- would otherwise make
	// every request on it look like an opener, and then a column of zeros
	// means two different things in one table.
	//
	// It is only ever about the other rows, and `r.cost = 0 AND
	// r.billable_ns = 0` was in here as well until reinstating its removal
	// changed no answer in any state the queue can produce: the rental's
	// facts are written to the opener and to nobody else, so an opener can
	// never find another row carrying them. A condition no reachable state
	// makes decisive is the dead half of a seal, and this file is not the
	// place to keep one.
	//
	//nolint:gosec // G202: where is fixed text from filterWhere and jobSideByRequest is a literal; values are parameters
	stmt := `
		SELECT r.id, r.created_unix, r.status, r.provider, r.gpu,
		       r.run_id, r.node, r.started_unix, r.ended_unix,
		       r.cost, r.rate, r.elapsed_ns, r.billable_ns, r.err,
		       COALESCE(j.n, 0),
		       COALESCE(j.images, 0),
		       COALESCE(j.bytes, 0),
		       COALESCE(j.cost, 0),
		       COALESCE(j.steps, 0),
		       COALESCE(j.span, 0),
		       COALESCE(j.recorded, 0),
		       (r.run_id <> '' AND EXISTS (
		           SELECT 1 FROM requests o
		           WHERE o.run_id = r.run_id AND o.id <> r.id
		             AND (o.cost > 0 OR o.billable_ns > 0)))
		FROM requests r
		LEFT JOIN (` + jobSideByRequest + `) j ON j.req = r.id
		` + where + `
		ORDER BY r.created_unix DESC, r.id DESC
		LIMIT ?`

	rows, err := s.db.QueryContext(ctx, stmt, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("read the generation ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Entry
	for rows.Next() {
		var (
			e                             Entry
			id, status                    string
			created, started, ended       int64
			cost, rate, elapsed, billable int64
			jobCost, span, recorded       int64
			rode                          bool
		)
		if err := rows.Scan(&id, &created, &status, &e.Provider, &e.GPU,
			&e.RunID, &e.Node, &started, &ended,
			&cost, &rate, &elapsed, &billable, &e.Err,
			&e.Jobs, &e.Images, &e.Bytes, &jobCost, &e.Steps, &span,
			&recorded, &rode); err != nil {
			return nil, fmt.Errorf("read a ledger row: %w", err)
		}
		e.ID, e.Status = ReqID(id), Status(status)
		e.Created = time.UnixMilli(created).UTC()
		// Zero stays zero rather than becoming the epoch, as scanRequest and
		// scanJobRun do it: "this never started" and "this started in 1970"
		// are different claims, and the second poisons every duration
		// computed from it.
		if started != 0 {
			e.Started = time.UnixMilli(started).UTC()
		}
		if ended != 0 {
			e.Ended = time.UnixMilli(ended).UTC()
		}
		e.Cost, e.Rate = node.MicroUSD(cost), node.Rate(rate)
		e.Elapsed, e.Billable = time.Duration(elapsed), time.Duration(billable)
		e.JobCost = node.MicroUSD(jobCost)
		e.Sampling = time.Duration(span)
		e.Rode = rode
		e.Unrecorded = unrecorded(ended, e.Jobs, recorded)
		out = append(out, e)
	}
	return out, rows.Err()
}

// unrecorded reports whether a request finished with jobs that never
// reported, and so contributes rental cost and no per-job figures.
//
// FINISHED IS THE PRECONDITION, and leaving it out was a real defect in the
// first version of Bucket.Unrecorded: a queued request contributes no cost and
// no images for the same arithmetic reason, so `ember spend` counted it and
// told the operator a request that had not run was missing figures. Nothing is
// missing from a request that has not happened yet.
//
// IT IS THE FACT AND NOT THE CAUSE, which the wording of both commands used to
// get wrong. Two different things land here -- a request from before schema
// version 3, and one that failed or was cancelled before a job completed --
// and the arithmetic warning is warranted for both, because both pay for a
// rental and divide into no images. Naming only the first blamed the schema
// for a run that simply broke, which was found by reading the output of a
// failed request rather than by a test.
//
// The test is ended_unix rather than a status list, because a status list in
// SQL would be a second spelling of Status.Terminal() -- and every path that
// ends a request writes the end: the queue on done and failed, the API on
// cancel, Interrupt on a restart.
func unrecorded(ended int64, jobs int, recorded int64) bool {
	return ended != 0 && jobs > 0 && recorded == 0
}

// filterWhere bounds a listing, with every value as a parameter.
func filterWhere(f Filter) (string, []any) {
	var c clauses
	c.period(f.Since, f.Until)
	c.eq("r.status", string(f.Status))
	c.eq("r.provider", f.Provider)
	c.eq("r.gpu", f.GPU)
	return c.sql(), c.args
}
