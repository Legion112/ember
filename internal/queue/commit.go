package queue

import (
	"sync"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
)

// THE LEDGER RACE, WHICH IS THE SHARPEST MONEY CONSEQUENCE OF max_nodes (#204).
//
// `requests.cost` is written from provision.WithNode's DEFERRED TEARDOWN, so a
// rental in flight contributes ZERO to the ledger a ceiling reads. With one
// rental that was bounded by one overshoot and #165 closed even that, by
// re-checking on the drain loop before the run started -- which works because
// the loop is SERIAL: at the moment of the re-check nothing else of this
// process's was renting, so "the completed cost of finished rentals" and
// "everything this process has agreed to spend" were the same set.
//
// At max_nodes>1 they are not. Two slots can be inside app.NewPlan at the same
// time, each reading a ledger in which the other's rental is worth nothing, and
// the overshoot is bounded by N rather than by 1. Worked, with N=2: a $20 day,
// $19 recorded, two concurrent creates each projected at $0.89 -- both see
// $19.89, both admit, the day ends at $20.78.
//
// WHAT THIS IS. The missing figure is not in the database and cannot be: it is
// what THIS PROCESS has agreed to spend and not yet recorded. So it is held
// here, beside the ledger read rather than inside it, and the check runs with
// the lock held so that two concurrent creates cannot both see a half-spent
// day.
//
// WHY A MUTEX ACROSS A SQLite SUM IS THE RIGHT TRADE. The expensive part of the
// check is one aggregate over `requests` on a single connection, which is
// microseconds; the thing it is protecting is a GPU. Serialising two creates'
// ceiling checks costs a wait nobody can measure and buys the only arithmetic
// that is correct.
//
// WHAT IT DOES NOT CLOSE, written down rather than papered over, because a
// money guard that is believed past its bounds is worse than one nobody
// trusts:
//
//   - THE STORE FILL IS UNCOMMITTED. AppPlanner.fillStore rents a machine with
//     no card, inside Plan and BEFORE the main Admit -- so while that sync runs,
//     a concurrent create reads a ledger that does not know about it. It is
//     projected against the same ceilings before it starts and recorded when it
//     finishes, so the unseen window is one $0.06/hour CPU machine for as long
//     as the fetch takes. Committing it would mean nesting an Admit inside an
//     Admit, or two entries under one request id, for a figure two orders of
//     magnitude below the thing being guarded.
//   - ANOTHER ember PROCESS'S IN-FLIGHT RENTAL IS INVISIBLE, and that is
//     unchanged by N: this is a map in one process's memory. Two `ember serve`s
//     over one generations.db see each other's FINISHED rentals through the
//     ledger and neither sees the other's commitments. The honest fix is a
//     table, which is a migration, a fake method and conformance rows for a
//     shape with one reader -- and it would still not cover the window between
//     a create call and its answer, which is what the journal's own "write the
//     record before the create" rule exists for.
//   - A STORE'S STANDING CHARGE IS STILL NOT COUNTED, which is ceiling.go's own
//     stated gap and is not about concurrency at all.

// Commitments is what rentals this process has agreed to spend, before any of
// them has been recorded in the ledger.
//
// KEYED BY THE OPENER'S REQUEST ID, which is the same key `requests.cost` is
// written under -- so the handover from "committed here" to "recorded there" is
// one id and not a correlation somebody has to get right. A rider adds nothing:
// it rides a rental already paid for, which is the same reason the period total
// is a SUM over openers.
//
// A NIL *Commitments IS TODAY'S ARITHMETIC AND EVERY METHOD TOLERATES ONE. That
// is not laziness about the wiring: every `queue.Queue{}` and every
// `AppPlanner{}` literal in the suite predates this type, and a nil that
// behaved differently from an empty one would mean the tests measured a
// mechanism production does not have. `Admit` on a nil calls the check with a
// committed total of zero, which is exactly what every caller got before N>1.
type Commitments struct {
	mu sync.Mutex

	// byReq is the projection each un-recorded rental was admitted for.
	//
	// A MAP AND NOT A RUNNING TOTAL, so that Release is idempotent and cannot
	// drive the total negative: a slot's defer always runs, and a subtraction
	// that ran twice -- or that ran for a rental whose Admit was refused --
	// would leave this process believing it had headroom it does not. A
	// delete of an absent key is a no-op; a subtraction of a figure nobody
	// added is a bug with a plausible-looking arithmetic over it.
	byReq map[genstore.ReqID]node.MicroUSD
}

// Admit runs check against what this process has already committed, and
// records projected only if it passes.
//
// THE CHECK RUNS WITH THE LOCK HELD, and that is the whole mechanism rather
// than an implementation detail: a read of the committed total followed by a
// check followed by a record is three statements with two windows in them, and
// at max_nodes>1 both windows are a second create. The shape is `ClaimNext`'s
// one-statement argument with a mutex in place of SQLite.
//
// RECORDED ONLY ON SUCCESS, because a refusal rents nothing: a commitment left
// behind by a refused create would subtract headroom from every later rental
// until the process restarted, and the operator would read it as a ceiling that
// had stopped working.
//
// THE CALLER RELEASES, AND THAT CONTRACT CROSSES TWO TYPES. AppPlanner.Plan
// admits -- it is where the projection is known, after the offer is chosen --
// and the slot that holds the rental releases: queue.once's defer for a queued
// request, and DirectRunner's own closure for a submitted run. Both release
// under the id they admitted under. The release is a DEFER at both sites, so
// there is no path out, error or not, that keeps a commitment for a rental that
// is over.
func (c *Commitments) Admit(
	id genstore.ReqID,
	projected node.MicroUSD,
	check func(committed node.MicroUSD) error,
) error {
	if c == nil {
		return check(0)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := check(c.totalLocked()); err != nil {
		return err
	}
	if c.byReq == nil {
		c.byReq = map[genstore.ReqID]node.MicroUSD{}
	}
	c.byReq[id] = projected
	return nil
}

// Committed is what this process has agreed to spend and not yet recorded.
//
// FOR A CHECK THAT READS WITHOUT RECORDING, which is the store fill: it is
// bounded by the same ceilings and is deliberately not a commitment of its own
// (see the note at the top of this file), so it asks what is outstanding and
// adds nothing. Reading without the lock held is honest about what it is -- a
// figure that may have changed by the time the check runs -- and that is the
// residue this file names rather than a race it hides.
func (c *Commitments) Committed() node.MicroUSD {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalLocked()
}

// Release forgets a rental's commitment once the ledger has the figure.
//
// CALLED AFTER THE TEARDOWN, which is what makes the handover lossless: the
// deferred destroy emits run_cost and Recorder flushes it urgently, so by the
// time the slot returns `requests.cost` holds what this entry was standing in
// for. Releasing earlier would open exactly the window this type closes;
// releasing later -- or not at all -- would shrink every later rental's
// headroom for the life of the process.
//
// A no-op on a nil receiver and on a key nothing admitted.
func (c *Commitments) Release(id genstore.ReqID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byReq, id)
}

// totalLocked sums the outstanding commitments. The caller holds mu.
func (c *Commitments) totalLocked() node.MicroUSD {
	var total node.MicroUSD
	for _, v := range c.byReq {
		total += v
	}
	return total
}
