package genstore

import (
	"context"
	"fmt"
	"time"
)

// WHAT IS PRUNABLE, AND WHY THE CASCADES ARE NOT THE MECHANISM.
//
// Three ON DELETE CASCADE clauses sat in the schema with no statement anywhere
// that deletes a row, and #28 read them as the prune waiting to be wired up.
// They are not. A cascade makes "delete a request" and "delete its images" one
// operation, and those two are the retention decision pulling in opposite
// directions: the rows are small and are the only record that money was spent,
// the pictures are large and are the reason to prune at all.
//
// So the split is:
//
//   - EVENTS ARE PRUNABLE FREELY. They are a progress log for a client that is
//     no longer connected, and they are the bulk: a FLUX.2 batch at 28 steps
//     emits rows per step per job per pass, and passes restart for each batch
//     item and each highres pass. The execution columns #32 added are a derived
//     index of exactly these events, so dropping them loses the replay and
//     keeps every figure `ember spend` and `ember history` report.
//
//   - IMAGES ARE PRUNABLE WHEN THE OPERATOR NAMES THEM. They were paid for, so
//     nothing removes one by age on its own; `ember history rm` is the verb. The
//     ROWS go with the bytes, and that is what makes the figures survive: a
//     ledger row's image and byte counts come from jobs.images and jobs.bytes --
//     see jobSideByRequest, which both readers join against -- and never from
//     this table. The images table is the pointer to the files, not the
//     measurement of them.
//
//   - REQUESTS AND JOBS ARE NOT PRUNABLE AT ALL. Not by age, and not by id
//     either. requests.cost is the only record anywhere that a rental happened,
//     and `ember spend` is a SUM over it. Every other way of being short of
//     history in this schema announces itself -- Bucket.Unrecorded counts the
//     requests whose per-job figures are missing and `ember spend` prints how
//     many it had to leave out -- and a deleted row cannot be counted, because
//     there is nothing left to count. It would make the period total quietly
//     smaller than the invoice, which is the one direction AGENTS.md says a
//     money figure must never be wrong in, with no footnote possible. What it
//     would buy is a few hundred bytes a request; the bulk is the events and the
//     PNGs, and both of those have a verb above.
//
// The cascades stay, unreachable, and that is now a decision rather than an
// oversight: dropping a foreign key in SQLite means rebuilding the table, and
// if a request row ever were deleted, cascading is the only behaviour that
// would not leave orphans. What was wrong was reading them as the prune.
//
// NOTHING RUNS ANY OF THIS AUTOMATICALLY. The only process that could is
// `ember serve`, which is the process that writes the events -- a prune on its
// startup deletes the replay of the run the operator is coming back to look at,
// with no confirmation and no output they would ever see. Events cost disk and
// not dollars, so there is no deadline forcing the issue the way a billing node
// forces `reap`. `ember history du` is what says it is time.

// prunableEvents is the one spelling of what a prune reaches.
//
// ONE CONST BECAUSE THE DRY RUN AND THE DELETE SHARE IT. A count built from a
// second copy of this predicate is a dry run that can disagree with the thing
// it is predicting, which is worse than no dry run at all: the operator reads
// a number, types --yes, and a different set of rows goes. Here the only
// difference between the two statements is the verb in front of them.
//
// ended_unix rather than a status list, for the reason unrecorded uses it: a
// list in SQL would be a second spelling of Status.Terminal(), and every path
// that ends a request writes the end -- the queue on done and failed, the API
// on cancel, Interrupt on a restart. It is also what makes a live request
// unreachable from here by construction rather than by a filter: a queued or
// running request has no end, so a browser streaming its progress cannot have
// the rows pulled out from under it.
//
// The horizon is measured from when a request ENDED and not from when it was
// created, which is where the two spellings in this package legitimately part:
// Filter and SpendQuery bound created_unix because an operator asks "what did
// last week cost", and a retention horizon asks "how long after it finished",
// which for a request created in March and finished yesterday are forty weeks
// apart.
const prunableEvents = `FROM events WHERE req IN (
	SELECT id FROM requests WHERE ended_unix != 0 AND ended_unix < ?
)`

// PruneEvents drops the events of finished requests that ended before the
// horizon, and reports how many rows went.
//
// dryRun counts them and touches nothing, through the same predicate.
func (s *SQLite) PruneEvents(ctx context.Context, before time.Time, dryRun bool) (int, error) {
	cut := before.UnixMilli()

	if dryRun {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) `+prunableEvents, cut).Scan(&n); err != nil {
			return 0, fmt.Errorf("count the prunable events in %s: %w", s.path, err)
		}
		return n, nil
	}

	res, err := s.db.ExecContext(ctx, `DELETE `+prunableEvents, cut)
	if err != nil {
		return 0, fmt.Errorf("prune the events in %s: %w", s.path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the pruned events in %s: %w", s.path, err)
	}
	return int(n), nil
}

// DropImages forgets the image rows of one request, and reports how many went.
//
// THE ROWS ONLY. The bytes are the caller's to remove, through blob.Store, and
// the order matters in one direction: the blobs go first. A row removed while
// its file is still there leaves bytes nothing in ember can name -- not
// `ember history du`, which prices the tree against these rows, and not a
// second `ember history rm`, which has nothing left to look up. That is the
// shape of an incomplete multipart upload, the one leak in this repository
// that billed with no detection path. A file removed while its row is still
// there is the harmless half: the row says what is missing and running the
// command again finishes the job.
//
// The request, its jobs and every cost column are untouched; see this file's
// header for why that is the whole point. A request whose images have been
// dropped still reports how many it produced and what they weighed, because
// those figures live on jobs and not here.
func (s *SQLite) DropImages(ctx context.Context, id ReqID) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE req = ?`, string(id))
	if err != nil {
		return 0, fmt.Errorf("forget the images of %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the forgotten images of %s: %w", id, err)
	}
	return int(n), nil
}

// Usage is what the history occupies, counted in rows.
//
// ROWS AND NOT BYTES, because the bytes of the database file are a question for
// the filesystem and the bytes of the images are a question for the blob store;
// a store that answered either would be guessing on behalf of a backend it does
// not have. What it can say is how many rows there are and what the image rows
// CLAIM their files weigh, which is the number `ember history du` subtracts the
// tree against -- and the gap is the part of the output directory ember never
// recorded.
type Usage struct {
	Requests int `json:"requests"`
	Jobs     int `json:"jobs"`
	Events   int `json:"events"`
	Images   int `json:"images"`

	// ImageBytes is what the image rows say their blobs weigh, which is a
	// claim about a file this store has never opened.
	ImageBytes int64 `json:"image_bytes"`

	// Reclaimed is how many finished requests reported producing images and
	// have no image rows left.
	//
	// WHICH IS WHAT `ember history rm` LEAVES BEHIND, and saying so is the
	// whole reason it is counted. The costs stayed and the pictures are gone,
	// so a request with jobs.images of 6 and no rows here is not a request
	// that produced nothing -- and without this line the operator has no way
	// to tell the two apart, which is the "a caption that is true of one mode"
	// defect in a column of counts.
	Reclaimed int `json:"reclaimed,omitempty"`
}

// Usage counts what the history holds.
func (s *SQLite) Usage(ctx context.Context) (Usage, error) {
	var u Usage

	// COALESCE on the sum because SUM over no rows is NULL, which scans into
	// an int64 as an error rather than as zero -- an empty database is the
	// first thing anybody runs this against.
	const stmt = `
		SELECT (SELECT COUNT(*) FROM requests),
		       (SELECT COUNT(*) FROM jobs),
		       (SELECT COUNT(*) FROM events),
		       (SELECT COUNT(*) FROM images),
		       (SELECT COALESCE(SUM(bytes), 0) FROM images),
		       (SELECT COUNT(*) FROM requests r
		          WHERE r.ended_unix != 0
		            AND EXISTS (SELECT 1 FROM jobs j WHERE j.req = r.id AND j.images > 0)
		            AND NOT EXISTS (SELECT 1 FROM images i WHERE i.req = r.id))`
	if err := s.db.QueryRowContext(ctx, stmt).Scan(
		&u.Requests, &u.Jobs, &u.Events, &u.Images, &u.ImageBytes, &u.Reclaimed); err != nil {
		return Usage{}, fmt.Errorf("measure the history in %s: %w", s.path, err)
	}
	return u, nil
}

// Vacuum returns freed pages to the filesystem, and does nothing when nothing
// was freed.
//
// THE GUARD IS INSIDE THE FUNCTION AND THAT IS THE POINT. SQLite does not
// return freed pages without a VACUUM, so a prune that is not followed by one
// frees disk nobody can see -- and a VACUUM rewrites the entire file, so
// running it after a prune that removed nothing makes the no-op case the
// expensive one on exactly the database large enough to be worth pruning. Both
// halves of that are a call site's rule in every other shape this could have
// taken; here removed is a parameter and there is no unguarded entry point, so
// a caller cannot forget it. The bool is what a test asserts against, because
// "it did not run" is otherwise only observable as a file that did not shrink.
//
// It is NOT on the Store interface. VACUUM is a property of a file, and a fake
// holding maps has nothing to compact -- a no-op on the interface would be a
// method every implementation has and only one means.
func (s *SQLite) Vacuum(ctx context.Context, removed int) (bool, error) {
	if removed <= 0 {
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return false, fmt.Errorf("compact %s after removing %d row(s): %w", s.path, removed, err)
	}
	return true, nil
}
