// Package queue drains generation requests onto up to max_nodes rentals.
//
// THE SHAPE, which is what was asked for: a request is a batch of jobs. While
// a node is up it keeps taking newly queued requests whose selection matches,
// and the moment there is none left it returns -- which destroys the node at
// once, with no idle timeout, because a node with nothing to do is a node
// billing for nothing.
//
// ONE RENTAL USED TO BE STRUCTURAL AND IS NOW BOUNDED. THIS IS THE TRADE AND
// NOT AN OVERSIGHT (#204). What stood here read: "there is one drain
// goroutine, and a rental lives inside one iteration of its loop, so two
// concurrent rentals cannot be expressed. The alternative -- a handler that
// starts a rental per request, with a counter saying no to the second -- is a
// counter that can be wrong, and being wrong costs two GPUs." That argument is
// still correct about counters, and the property it describes has been
// deliberately given up: a fleet is N rentals by definition, so at N>1 there is
// no structural version of it and a semaphore of size N is that counter with a
// larger constant. Nothing recovers "a double-clicked button CANNOT rent
// twice"; what is left is that it will not. See
// docs/threat-model/request-seal.md, which records the trade rather than being
// quietly falsified by it.
//
// SO WHAT IS LOAD-BEARING INSTEAD, in the order a reader should check it:
//
//   - `MaxNodes` is the bound, default 2 from the config. Without it the new
//     failure mode is runaway scale-out: a queue of fifty mutually
//     incompatible requests is fifty Create calls, each individually inside
//     every money ceiling, and reachable from the web UI.
//   - THE CLAIM IS STILL ON ONE GOROUTINE, which is what the counter argument
//     above is replaced by rather than abandoned. Run claims and registers
//     what the new rental will serve BEFORE it decides anything else, so a
//     second compatible request cannot provoke a third node while slot 2 is
//     still provisioning. That is a single-goroutine property doing the work,
//     not a count read after the fact.
//   - STRICT PACKING. The create-side claim asks for the oldest queued
//     request that NO live rental can serve (genstore.Compat.Live), so a
//     fitting rental always absorbs a request even if it is mid-batch. The
//     consequence is the counter-intuitive one: max_nodes>1 parallelises
//     INCOMPATIBLE work only -- a different provider, card or kind -- and a
//     homogeneous queue behaves exactly as it did at one node.
//   - THE PERIOD CEILINGS ARE NOW THE PRIMARY MONEY GUARD rather than the
//     backstop, and `Commitments` is what makes them hold with N creates in
//     flight: requests.cost is written from a deferred teardown, so a rental
//     in flight contributes zero to the ledger a concurrent create reads. See
//     commit.go.
//
// THERE ARE TWO KINDS OF WORK AND ONE PLACE THE DECISION IS MADE. `ember up`
// submitted over the unix socket is not a queued request -- see direct.go for
// the four reasons it cannot be one -- so it does not arrive through
// ClaimNext. It arrives through Submit, which is a RENDEZVOUS on an
// unbuffered channel that only this loop receives from: a send succeeds
// exactly when the supervisor is between decisions AND has a slot free, so
// Submit returning still means the loop owns the run and has somewhere to run
// it. A submission offered while every slot is busy waits, which is the honest
// expression of "this process is already renting as much as it may" -- and the
// client is told it is waiting rather than refused. The buffer is what would
// have made it a counter: "accepted, and it will start some time" needs
// somebody to know how many were already waiting.
//
// THE SHUTDOWN STORY IS NOW ABOUT A SET. Each rental lives on a slot
// goroutine, Run holds a WaitGroup and defers its Wait, so Run still does not
// return until every slot's deferred destroy has run -- the sentence changes
// from "waiting for this goroutine is waiting for the node to be gone" to
// "waiting for this goroutine is waiting for the SET of nodes to be gone". THE
// WAIT IS UNBOUNDED ON PURPOSE: a bound abandons a node, and a process that
// exits with a GPU still rented bills by the hour with nobody watching, which
// is the most expensive bug available here.
//
// PARALLELISM WITHIN ONE RENTAL IS STILL DELIBERATELY ABSENT, and that is a
// different question from the one above. sd-server's generation context lock
// serializes work on the device, so a second concurrent job on one node buys
// nothing; what N>1 buys is a second node for work the first one cannot serve
// at all.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/tts"
)

// idlePoll is how often the loop looks for work it was not told about.
//
// A fallback and not the mechanism: Kick is what the API calls after an
// enqueue, so the ordinary latency is zero. This exists because a kick
// delivered while the loop was busy is a kick the loop did not hear, and the
// alternative to a slow poll is a request that sits queued until the next
// one arrives.
const idlePoll = 15 * time.Second

// Planner turns a request into something that can be rented.
//
// An interface so the drain loop is testable with no Docker, no provider and
// no card -- which matters because what is being tested here is money
// behaviour, and the expensive bugs are in the loop rather than in the
// provisioning it calls.
type Planner interface {
	// Plan resolves what a request would rent and rents nothing.
	Plan(ctx context.Context, r genstore.Request) (Rental, error)
}

// Rental is a plan that has not been rented yet.
type Rental interface {
	// WithServed rents the node, makes the weights resident and calls fn. The
	// node is destroyed when fn returns, whatever it returns.
	//
	// sink receives the rental's own lifecycle: the node becoming ready, what
	// it cost, and a destroy that failed.
	WithServed(ctx context.Context, sink report.Sink, fn func(context.Context, app.Served) error) error

	// WithSpoken is the same for a speech rental, which has no server to
	// start and therefore nothing to wait for.
	//
	// TWO METHODS AND NOT ONE TAKING A KIND, because the two closures are
	// handed different things: a `Served` publishes what the server can do
	// and a `Spoken` publishes nothing. One method returning an `any` would
	// put a type assertion in the drain loop, which is the dispatch this
	// change exists to remove rather than relocate.
	WithSpoken(ctx context.Context, sink report.Sink, fn func(context.Context, app.Spoken) error) error

	// Card is the card this plan's offer carries, and it is the one fact the
	// scheduler advertises.
	//
	// ON THE RENTAL AND NOT ON Planner.Plan's SIGNATURE, because it is a
	// property of the thing that was planned: a plan knows which offer it
	// chose, and a second return value would mean every caller carrying a
	// card beside a rental it already has.
	//
	// READABLE BEFORE ANYTHING IS RENTED, which is what makes advertising it
	// cheap: Plan chooses the offer and rents nothing, so the card is known
	// one statement after the claim and the window in which a live rental has
	// no card is an offer lookup rather than a create call. See
	// genstore.Compat.Card for what the window costs and why ember holds this
	// fact rather than asking the node for it.
	Card() node.GPUModel

	// Image is the workload image this plan will run, for the fleet view and
	// for nothing else.
	//
	// A ROW IN A TABLE AND NOT AN INPUT TO ANY DECISION, which is why it is
	// safe for it to be here: nothing in the scheduler reads it, it is not
	// part of genstore.Compat, and two rentals differing only in image are
	// still compatible. What it answers is the question an operator asks of a
	// fleet they did not watch start -- "which of these is the FLUX node" --
	// and the config that named it may have been edited since, so reading it
	// off the plan is reading it from the only authority there is.
	Image() string
}

// Queue is the drain loop.
type Queue struct {
	Store   genstore.Store
	Planner Planner

	// Workloads is what the operator's file declares, resolved once at
	// startup by the one function that reads it.

	// MaxNodes is how many rentals this process may hold at once.
	//
	// ANYTHING BELOW 1 MEANS 1, AND THAT IS WHY max_nodes: 1 IS NOT A SPECIAL
	// CASE. Every `queue.Queue{}` literal in the suite predates this field, so
	// a zero has to keep today's behaviour -- which means one node is the same
	// code path as two with a smaller bound, rather than a branch that only
	// one of the two ever exercises. The cost of that choice is that a
	// `cmd_serve` which FORGOT to set it would silently pin an operator who
	// wrote `max_nodes: 2` to one node, with nothing reporting it: the #105
	// class, where webapi.Options had no Scan field and GET /api/nodes
	// answered 501 in every shipped `ember serve`. That is closed by
	// TestServe_WiresTheNodeCeilingIntoTheDrainLoop, which parses this
	// package's production files.
	MaxNodes int

	// Commit is what rentals this process has agreed to spend before any of
	// them has been recorded, shared with DirectRunner.
	//
	// NIL IS TODAY'S ARITHMETIC. See commit.go: a nil *Commitments runs each
	// ceiling check against a committed total of zero, which is exactly what
	// every caller got before N>1 and what every existing test still gets.
	Commit *Commitments

	// Voices is the catalogue a speech request's voice id is resolved
	// against, from the operator's config.
	//
	// ON THE QUEUE AND READ NOWHERE ELSE ON THIS PATH, which is where the
	// seal lands: a stored row carries an entry **id** and no image and no
	// path, so this is the only thing that can turn one into a voice filename
	// -- and it comes from the config this process was started with, never
	// from a request. The IMAGE is resolved from the same file one layer down,
	// by `Narrow`, which is the planner's business and not the loop's.
	//
	// IT WAS A `tts.Catalogue` FOR THE WHOLE PROCESS, which stopped being
	// right when the image moved into the entry: two declared speech
	// workloads are two images with two voice sets on them, and one list
	// would offer each one's ids for the other's node.
	//
	// Empty means this process can be asked for nothing, which `webapi`
	// refuses at enqueue so that no row reaches the loop that could not be
	// resolved.
	Workloads []config.Workload

	// Outputs is the directory requests write into.
	//
	// A request's own files go in a subdirectory named after the request,
	// which is ALSO its blob key prefix -- one name for a path, so a row in
	// the database and a file on disk cannot disagree about where something
	// is. Two requests on one rental therefore cannot write over each
	// other's images, which a shared per-rental directory would allow the
	// moment two of them used the same output stem.
	Outputs string

	// Out is deliberately absent. The node's byte streams reach the operator
	// through AppPlanner.Out, which is the writer app.NewPlan is handed --
	// and a second copy on the Queue was set by `ember serve`, read by an
	// accessor nothing but two tests called, and therefore carried nothing
	// for the life of the field. It is the ninth instance of the defect
	// cmd/check-coverage's TestNoProductionDeadCode now reports, and the
	// first one a detector found rather than a person.

	Log *slog.Logger
	Now func() time.Time

	kick     chan struct{}
	kickOnce sync.Once

	// runs is the second way work reaches this loop, and the only other one.
	//
	// UNBUFFERED, which is the whole mechanism: a send succeeds exactly when
	// this loop is between decisions AND has a slot free, so Submit returning
	// means the loop owns the run and has somewhere to put it. A buffer would
	// turn that into "accepted, and it will start some time", which is the
	// shape a counter has -- the handler would have to be told how many were
	// already waiting in order to say anything true. See Submit and direct.go.
	runs     chan Direct
	runsOnce sync.Once

	mu sync.Mutex

	// slots is how many rentals are in flight, counting both kinds of work.
	//
	// READ AND WRITTEN UNDER mu AND DECIDED ON ONE GOROUTINE, which is the
	// distinction that matters: a slot goroutine decrements it as it exits, so
	// the lock is needed -- but every DECISION taken on it is taken by the
	// supervisor, which is the only thing that increments it. So this is not
	// the "counter that can be wrong" the one-rental invariant refused; it is
	// a count one goroutine keeps of goroutines it started, and the thing
	// stopping a request from provoking a node that is already coming is the
	// `live` registration below rather than this number.
	slots int

	// live is what each rental in flight can serve, keyed by the request that
	// opened it.
	//
	// IT IS THE CREATE SIDE OF fit-or-create AND IT IS REGISTERED BEFORE THE
	// SLOT STARTS. The supervisor claims, writes the compat here, and only
	// then spawns -- so a second compatible request arriving while slot 2 is
	// still provisioning is excluded by the next claim rather than given a
	// third node. Registering it from inside the slot would leave a window
	// whose width is a provider's create call, which on RunPod is seconds and
	// is long enough for a queue to scale out.
	//
	// A SUBMITTED RUN GETS NO ENTRY, which is correct rather than an omission:
	// `ember up` over the socket is not a queued request and never serves one,
	// so it occupies a slot and offers nothing to ride. See direct.go.
	live map[genstore.ReqID]genstore.Compat

	// current is every request generating right now.
	//
	// A SET, WHERE IT WAS ONE VALUE, and the comment it replaces argued from
	// the invariant #204 traded: "there is one drain goroutine and a rental
	// lives inside one iteration of its loop, so the request this process
	// holds is a single value". At max_nodes>1 several requests generate at
	// once, one per rental, so this is a set and Running is a sorted list --
	// sorted, because the wire field it becomes keys a CSS class on a table
	// and an order that churned would re-render rows for nothing.
	//
	// IT IS NOT MERGED INTO stops BELOW, although membership looks like the
	// same question. Cancel DELETES a stop channel and leaves the request in
	// here, because the batch really is still generating: the job in flight
	// cannot be interrupted, so it finishes and is paid for, and a row that
	// stopped being reported as running the instant somebody pressed cancel
	// would be a page claiming the card was free.
	current map[genstore.ReqID]struct{}

	// held is every request this loop has CLAIMED, which is not the same as
	// the ones it is generating.
	//
	// THE GAP BETWEEN THE TWO IS A 409'S REMEDIATION. `current` is set by
	// generate, so it is empty for the whole of provisioning -- the plan,
	// the create, the wait for sshd, the gpu probe, the asset verify and the
	// model load -- while the row has read `running` since ClaimNext. A
	// cancel arriving in that window reaches a queue with nothing to signal,
	// and webapi answered it by telling the operator to restart
	// `ember serve`: the one action that destroys the rental they are paying
	// for, in order to reconcile a request that was seconds from generating.
	// This field is what tells that window from the state the message is
	// true of -- a row left `running` by a process that is gone, which Run's
	// Interrupt sweep clears on the next start.
	//
	// A SET WITH AT MOST ONE ENTRY PER SLOT, which is the shape the `holder`
	// type below makes structural rather than remembered: one rental serves
	// its requests in turn, so a slot hands the entry over as it moves to the
	// next rider and gives it back when it exits. Counting it as a plain set
	// that each rider adds to would leave finished riders held for the life of
	// the rental -- harmless today, because Holding is only consulted for a
	// row that says `running`, and exactly the kind of "harmless" that stops
	// being so when somebody reads the field's name instead of this paragraph.
	//
	// AND IT IS WHAT THE LEASE RENEWAL READS, which is a second reader of one
	// set rather than a second set. Both ask the same question -- "which rows
	// read `running` because this process is alive" -- and a map kept
	// separately for the renewal would be a second answer to it, free to fall
	// behind in the direction where a live rental's row is reclaimed.
	held map[genstore.ReqID]struct{}

	// stops is the channel that cancels each generating request, by id.
	//
	// A MAP, WHERE IT WAS ONE CHANNEL, and the comment it replaces said why it
	// could not be: "a map keyed by request id would be a set of signals only
	// one of which can ever be listened to, and the other entries would be a
	// leak with a plausible-looking lookup over it". At max_nodes>1 every
	// entry has a listener -- one per rental -- so the lookup is the
	// mechanism rather than a plausible-looking shape over a single value.
	//
	// DELETED BY Cancel RATHER THAN CLOSED TWICE: a double close panics, in a
	// slot goroutine, with a node running -- and a double-clicked cancel
	// button is the ordinary case here, not the exotic one. Reinstating that
	// makes TestCancel_IsSafeToAskTwice report `close of closed channel`.
	stops map[genstore.ReqID]chan struct{}

	// sinks is one event stream per request, memoised.
	//
	// ONE PER REQUEST AND NOT ONE PER CALL, because the sequence number is
	// the whole contract with a watching client: it asks for everything
	// after what it has seen, and two numberings for one request means
	// events that overwrite each other. A rental's lifecycle events and its
	// batch's progress go to the same request, so they have to come from the
	// same counter.
	sinks map[genstore.ReqID]*sink

	// fleet is one entry per rental this process is holding, keyed by the
	// request that opened it. See fleet.go.
	//
	// KEYED BY THE OPENER AND NOT BY THE NODE, although the node is what an
	// operator reads. The opener exists from the moment the rental is planned
	// and the node does not: a create call is seconds to minutes of money
	// already committed, and a map keyed on the handle would have no row for
	// the whole of it.
	fleet map[genstore.ReqID]*berth
}

func (q *Queue) log() *slog.Logger {
	if q.Log != nil {
		return q.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (q *Queue) at() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// dirOf is where a request's files are written: a real path, because
// sdserver.WriteImages opens an os.Root over it.
func (q *Queue) dirOf(id genstore.ReqID) string {
	return filepath.Join(q.Outputs, string(id))
}

func (q *Queue) kicks() chan struct{} {
	q.kickOnce.Do(func() {
		if q.kick == nil {
			// Buffered by one. A kick while the loop is busy must not block
			// the API handler that sent it, and a second kick before the loop
			// wakes is the same information as the first.
			q.kick = make(chan struct{}, 1)
		}
	})
	return q.kick
}

// Kick tells the loop there may be work. Never blocks.
func (q *Queue) Kick() {
	select {
	case q.kicks() <- struct{}{}:
	default:
	}
}

// Running reports which requests are generating right now, sorted.
//
// A LIST, WHERE IT WAS `(ReqID, bool)`, and it is the one singular in this
// change that is visible on the wire: it is projected as `"running"` by
// GET /api/requests and by the progress socket's envelope, and read by
// web/src/views/queue.rs to mark the row that is generating. At max_nodes>1
// several requests generate at once, so a single id would mark one of them and
// leave the others looking queued.
//
// SORTED, because that field feeds a keyed table in the browser: an order that
// churned between two polls would be a diff for no reason, and Go's map
// iteration order is deliberately random.
//
// EMPTY RATHER THAN NIL IS THE CALLER'S PROBLEM AND IS DEALT WITH THERE: this
// returns what it has, and webapi turns it into a `[]` on the wire, because Go
// marshals a nil slice as `null` and a client whose field is a list drops the
// whole envelope on a type error -- which cost this program a whole queue
// table once.
func (q *Queue) Running() []genstore.ReqID {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]genstore.ReqID, 0, len(q.current))
	for id := range q.current {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// nodes is how many rentals this process may hold at once, never below one.
//
// THE CLAMP IS WHY max_nodes: 1 IS THE SAME CODE PATH. A zero MaxNodes is
// every `queue.Queue{}` literal written before the field existed, and reading
// it as one keeps their behaviour exactly; config.Load refuses a file that
// writes 0, so an operator cannot reach this clamp by configuration.
func (q *Queue) nodes() int {
	if q.MaxNodes < 1 {
		return 1
	}
	return q.MaxNodes
}

// free is how many more rentals this process may open, and what the ones
// already open can serve.
//
// THE TWO ANSWERS COME FROM ONE LOCK HOLD, so that they are consistent with
// EACH OTHER: the supervisor's next decision is "may I create, and if so for
// what work none of these can take", and two separate reads could pair a slot
// count from one moment with a live set from another -- a free slot alongside
// a rental that has since gone, or the reverse.
//
// WHAT ONE HOLD DOES NOT BUY, said rather than left to be assumed: a slot can
// still exit between this read and the claim below it, so the claim may
// exclude work for a rental that no longer exists. That costs one wasted
// iteration and no money -- the claim finds nothing, the supervisor parks, and
// the exiting slot's own Kick wakes it to ask again with the shorter set.
// Holding the lock across ClaimNext would close it and would mean a SQLite
// statement under the mutex every slot exit takes, which is the wrong trade
// for a wasted loop.
func (q *Queue) free() (int, []genstore.Compat) {
	q.mu.Lock()
	defer q.mu.Unlock()
	live := make([]genstore.Compat, 0, len(q.live))
	for _, c := range q.live {
		live = append(live, c)
	}
	return q.nodes() - q.slots, live
}

// reserve takes a slot for a rental the supervisor is about to start, and
// records what it will serve.
//
// CALLED ON THE SUPERVISOR AND BEFORE THE SLOT GOROUTINE EXISTS, which is the
// whole of what stops runaway scale-out: see the `live` field. A zero compat
// records nothing to ride, which is a submitted run -- it occupies a slot and
// serves no queued request.
func (q *Queue) reserve(opener genstore.ReqID, compat genstore.Compat) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.slots++
	if compat.IsZero() {
		return
	}
	if q.live == nil {
		q.live = map[genstore.ReqID]genstore.Compat{}
	}
	q.live[opener] = compat
}

// freeSlot gives the slot back once its node is gone, and tells the supervisor.
//
// THE KICK IS WHY THERE IS NO SECOND CHANNEL. Two things changed when a slot
// exits -- there is capacity, and there is one fewer live compat -- and both
// are inputs to the supervisor's next decision, which is parked in the same
// select an enqueue wakes. A slot that exited silently would leave a queued
// request waiting out idlePoll for a node that is already free.
func (q *Queue) freeSlot(opener genstore.ReqID) {
	q.mu.Lock()
	q.slots--
	delete(q.live, opener)
	q.mu.Unlock()
	q.Kick()
}

// Holding reports whether this process has claimed that request -- anywhere
// between the claim and the end of its batch, generating or not.
//
// IT IS NOT Running. The two differ for exactly as long as a rental takes to
// come up, and that difference is the only thing that can tell a cancel
// arriving during provisioning -- where a rental IS in this process and is
// about to generate -- from a row left `running` by an ember that is gone.
// Telling those apart is the whole of webapi's two 409s, because the
// remediation for the second destroys the rental in the first.
func (q *Queue) Holding(id genstore.ReqID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if id == "" {
		return false
	}
	_, ok := q.held[id]
	return ok
}

// hold records that some slot is responsible for this request.
func (q *Queue) hold(id genstore.ReqID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.held == nil {
		q.held = map[genstore.ReqID]struct{}{}
	}
	q.held[id] = struct{}{}
}

// unhold forgets one.
func (q *Queue) unhold(id genstore.ReqID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.held, id)
}

// holder is one slot's answer to "which request am I responsible for".
//
// A TYPE RATHER THAN TWO CALLS AT EACH SITE, because what has to stay true is
// that the Queue's `held` set has AT MOST ONE ENTRY PER SLOT, and that is a
// hand-over: a rental serves its requests in turn, so taking the next rider
// has to give the previous one back in the same breath. Writing that out at
// each site is the "one name for a path, not one per call site" rule with a
// map in place of a directory -- and the site that forgot would leave a
// finished request reported as held, which webapi reads to choose between two
// 409s whose remediations are "wait" and "restart the server that is renting".
//
// It is not a lock and holds none: every method here takes the Queue's.
type holder struct {
	q  *Queue
	id genstore.ReqID
}

// take makes id the one request this slot is responsible for.
func (h *holder) take(id genstore.ReqID) {
	if h.id == id {
		return
	}
	// THE NEW ONE FIRST. Between the two calls Holding is asked by an HTTP
	// handler on another goroutine, and the honest overlap is "both" rather
	// than "neither": answering neither for an instant would send a cancel of
	// a request this process really does hold to the 409 that says to restart
	// the server, which destroys the rental.
	h.q.hold(id)
	if h.id != "" {
		h.q.unhold(h.id)
	}
	h.id = id
}

// done gives the entry back, and is a no-op twice.
func (h *holder) done() {
	if h.id == "" {
		return
	}
	h.q.unhold(h.id)
	h.id = ""
}

// Cancel stops the generation of one request and reports whether it reached
// it.
//
// THE RENTAL SURVIVES IT, which is the whole difference between this and what
// `ember serve` used to offer. Nothing here touches a context: cancelling the
// drain loop's would unwind through provision.WithNode's deferred destroy and
// take the node away, and with it every other request queued onto the same
// rental -- which is what the API's old 409 was protecting, correctly, against
// a mechanism that did not exist yet. Closing a channel ends one batch; the
// node, the server process and the resident weights are untouched, and the
// next claim is served by a card that is already warm.
//
// Kick's shape and for Kick's reason: the API has to reach the drain loop, and
// a handler must never block on it. False means this request is not the one
// generating -- it was already cancelled, or it has finished, or a different
// one is running -- and the caller must not read that as "stopped".
func (q *Queue) Cancel(id genstore.ReqID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if id == "" {
		return false
	}
	stop, ok := q.stops[id]
	if !ok {
		return false
	}
	close(stop)
	// DELETED RATHER THAN CLOSED AGAIN, and this line is the one that
	// matters: a double close panics, in a slot goroutine, with a node
	// running -- and a double-clicked cancel button is the ordinary case
	// here, not the exotic one. Reinstating the bug makes
	// TestCancel_IsSafeToAskTwice report `close of closed channel`.
	//
	// THE ID IS THE LOOKUP AND THAT IS THE GUARD, which is the half that used
	// to need saying twice: a cancel arriving a moment too late finds no entry
	// for a request that has finished, so it cannot reach the next request on
	// the same rental -- and at max_nodes>1 it cannot reach a request on
	// ANOTHER rental either, which a single channel plus an id check would
	// have made it possible to get wrong one refactor later.
	delete(q.stops, id)
	return true
}

// Run supervises up to MaxNodes rentals until ctx ends.
//
// IT RETURNS ONLY ONCE EVERY RENTAL IN FLIGHT HAS BEEN DESTROYED, which is
// what makes "wait for this goroutine" a complete shutdown -- and at
// max_nodes>1 that is a set rather than one node. The WaitGroup below is
// deferred FIRST, so there is no path out of this function, error or not, that
// returns while a slot still holds a node.
//
// THE WAIT HAS NO BOUND, and that is the one decision here somebody will want
// to revisit. A bound would mean this returns while a destroy is still in
// flight, `ember serve` exits, and a GPU bills by the hour with nobody
// watching -- the most expensive bug available in this program. The bounded
// waits elsewhere in the tree are bounded because what they are waiting for is
// a WRITE (see writeCtx) or a byte stream; this one is waiting for a node to
// stop existing.
//
// WHAT THIS GOROUTINE DOES AND DOES NOT DO. It claims, it registers what the
// new rental will serve, and it spawns -- and nothing else in the program
// does any of those three. It does not rent: a slot does, and
// cmd/check-coverage's TestNothingRentsOutsideInternalApp bounds the other
// half by asking which packages may name provision.WithNode. Keeping the
// claim here is what replaces the invariant #204 traded: a count read after
// the fact is the counter that argument refuses, and a single goroutine
// deciding is not.
func (q *Queue) Run(ctx context.Context) error {
	// A row whose lease has run out belonged to a process that is not
	// renewing it. Its rental cannot be resumed, so it is marked rather than
	// re-queued -- see genstore.StatusInterrupted.
	//
	// IT NO LONGER SWEEPS EVERY RUNNING ROW, and the line it logs no longer
	// claims their rentals are gone. That claim was the sweep's whole
	// justification and it is not ember's to make: a node carries its run id
	// and its deadline in the provider's own label, the label is the source of
	// truth, and internal/reconcile is what reads it. A second `ember serve`
	// starting beside a live one used to mark every one of the first's
	// in-flight requests interrupted on that premise.
	q.reclaim(ctx)

	// EVERY SLOT IS WAITED FOR, AND THE DEFER IS THE FIRST STATEMENT AFTER THE
	// SWEEP. See the doc comment: unbounded, on purpose.
	var wg sync.WaitGroup
	defer wg.Wait()

	// pending is a submitted run this loop has taken and not yet started.
	//
	// IT IS A VALUE AND NOT A QUEUE, and the reason survives N>1 unchanged:
	// the channel is unbuffered, so the supervisor holds at most one run at a
	// time and every other submitter is still parked on its own send. A slice
	// here would be a backlog this loop owned, which is the thing Submit's
	// rendezvous exists not to create -- a submission the client could not be
	// told the position of. It is only ever taken when a slot is free, so a
	// run held here is one that is about to start rather than one queued
	// behind N rentals.
	var pending *Direct

	for {
		if ctx.Err() != nil {
			// A SUBMISSION THIS LOOP TOOK AND NEVER STARTED LEAVES THROUGH
			// HERE, and it used to leave silently. Called unconditionally so
			// the ordinary shutdown -- `pending` nil -- goes through the same
			// line rather than through a condition that is almost always
			// false.
			q.dropPending(pending)
			return nil
		}

		// THE LEASE HEARTBEAT, AT THE TOP OF THE LOOP AND NOT ON A TICKER OF
		// ITS OWN. What has to stay true is that a row reads `running` for
		// only as long as some process is alive and responsible for it, and
		// the supervisor is the one goroutine that is always alive while this
		// process is: its longest park is idlePoll, which is one eighth of
		// genstore.LeaseTerm. A ticker would be a second goroutine whose
		// liveness is a separate question from the loop's, and a lease
		// renewed by a goroutine that outlives a wedged supervisor is a lease
		// that says nothing.
		q.renew(ctx)

		// AND THE SWEEP RUNS HERE TOO, not only at startup. "The process that
		// held this row is gone" becomes true while ember is running -- a
		// second ember on the same store, killed with SIGKILL -- and a sweep
		// that only ever ran before the first claim would leave that row
		// reading `running` until the next restart.
		q.reclaim(ctx)

		slots, live := q.free()

		// A SUBMITTED RUN IS TAKEN BEFORE A CLAIM, and the non-blocking shape
		// is what makes that true without starving the queue: a submission
		// already parked on the channel goes first, so it is not overtaken by
		// a request queued while it was waiting for a rental to end, and a
		// loop with nothing parked falls straight through to ClaimNext.
		//
		// ONLY WITH A SLOT FREE, which is what keeps Submit's rendezvous
		// meaning what it says: taking a run this process has no capacity for
		// would turn "the loop owns it" into "the loop owes you one".
		if pending == nil && slots > 0 {
			select {
			case d := <-q.handoff():
				pending = &d
			default:
			}
		}

		if slots > 0 {
			if pending != nil {
				d := *pending
				pending = nil
				// NO COMPAT: a submitted run serves no queued request, so it
				// takes a slot and offers nothing to ride. See the `live`
				// field.
				q.spawn(ctx, &wg, d.Req, genstore.Compat{}, func(ctx context.Context) error {
					return q.direct(ctx, d)
				})
				continue
			}
			started, err := q.claim(ctx, &wg, live)
			if err != nil {
				// Reported and not returned. A claim that failed is this
				// store's problem for a moment; ending the loop would mean one
				// bad read stops every later request from running, and the
				// slots already open would be waited for with nothing left to
				// start.
				q.log().Error("could not look for work", "err", err)
			}
			if started {
				// Straight round again: there may be more incompatible work
				// and another slot to put it on, and it should not wait for a
				// poll.
				continue
			}
		}

		// THE HANDOFF IS OFFERED ONLY WHEN THERE IS SOMEWHERE TO PUT IT, and a
		// nil channel blocks for ever, which is the idiom. With every slot
		// busy the submitter stays parked on its own send and is told it is
		// waiting; a slot's exit calls Kick, which is what wakes this select
		// to reconsider.
		var offer chan Direct
		if pending == nil && slots > 0 {
			offer = q.handoff()
		}

		select {
		case <-ctx.Done():
			// FALLS THROUGH TO THE TOP RATHER THAN RETURNING HERE, so that
			// `Run` has ONE return out of its loop and therefore one
			// dropPending call site.
			//
			// WHAT THAT IS AND IS NOT, because the first wording of this
			// comment claimed a defect and the claim does not survive being
			// checked. It said a shutdown landing here with a `pending` run in
			// hand returned silently -- the #151 defect, reintroduced. That
			// state is UNREACHABLE: both receives of the handoff are guarded
			// by `pending == nil && slots > 0`, and `slots` is incremented
			// only by `spawn`, which only this goroutine calls -- so a run
			// taken at either select is spawned in the same iteration, and
			// there is no interleaving in which this goroutine parks holding
			// one with no slot for it.
			//
			// SO THIS IS A SECOND RETURN PATH REMOVED AND NOT A SILENT DROP
			// FIXED. It is worth removing anyway, and the reason is the rule
			// rather than a bug: the branch is unreachable because of a guard
			// three statements away, and a reader who widens that guard -- say
			// to take a submission while a slot is provisioning -- makes it
			// reachable with nothing reporting it. One return means that
			// reader cannot reintroduce the drop by moving a condition. There
			// is no test, because there is no state to produce: see
			// TestDropPending_..., which is over the LINE for the same reason.
		case d := <-offer:
			// RECEIVED HERE AND NOT ONLY AT THE TOP, because an idle loop is
			// parked in this select and a submission must not wait out
			// idlePoll to be noticed. Kicking instead -- Submit calling Kick
			// and then parking on the send -- races: the loop can reach the
			// top, find nothing parked yet, claim nothing, and come back here
			// having already consumed the kick, so the run would start up to
			// fifteen seconds later for no reason a reader could see.
			//
			// AND IT IS A SIBLING OF ctx.Done() ABOVE, which is the case
			// dropPending exists for: both are ready at a shutdown that lands
			// while a submission is parked, Go picks uniformly, and taking the
			// handoff means the run is held across one more top-of-loop
			// `ctx.Err()` that returns without starting it. Submit has already
			// answered nil by then. See Submit's own comment for the
			// measurement and for why that outcome is right and only its
			// account was wrong.
			pending = &d
		case <-q.kicks():
		case <-time.After(idlePoll):
		}
	}
}

// renew pushes the lease forward on every row this process is responsible for.
//
// REPORTED AND NEVER RETURNED, for the reason the claim's own error is: a
// store that could not be written for a moment is not a reason to stop
// draining a queue. What a missed renewal costs is bounded and is not money --
// the row is reclaimed early and the request's own ending writes over it, the
// same transient genstore.Store.Adopt already documents -- and what returning
// here would cost is every later request in the queue.
func (q *Queue) renew(ctx context.Context) {
	ids := q.heldIDs()
	if len(ids) == 0 {
		return
	}
	if _, err := q.Store.Renew(ctx, ids); err != nil {
		q.log().Warn("could not renew the lease on the requests this process is running; "+
			"they may be reported as interrupted while they are still generating", "err", err)
	}
}

// reclaim marks interrupted whatever lease has run out.
func (q *Queue) reclaim(ctx context.Context) {
	n, err := q.Store.Reclaim(ctx)
	if err != nil {
		q.log().Error("could not reclaim expired requests; the history is wrong but the queue still works", "err", err)
		return
	}
	if n > 0 {
		// WHAT THIS SAYS AND WHAT IT CAREFULLY DOES NOT. It says nobody is
		// renewing the lease, which is what was measured. It does NOT say the
		// rentals are gone, which the sweep this replaced did say and could
		// not know: `ember ls` reads the provider's labels and is the only
		// thing that can answer that.
		q.log().Warn("requests were left running by a process that is no longer renewing their lease; "+
			"check `ember ls` for nodes they may have left behind", "count", n)
	}
}

// heldIDs is every request this process is responsible for.
func (q *Queue) heldIDs() []genstore.ReqID {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]genstore.ReqID, 0, len(q.held))
	for id := range q.held {
		out = append(out, id)
	}
	// Sorted so the statement's parameter list is stable, which is what makes
	// a recorded call comparable between runs.
	slices.Sort(out)
	return out
}

// advertise records the card a rental actually got, so the supervisor's next
// exclusion is exact rather than conservative.
//
// ON THE SLOT AND NOT ON THE SUPERVISOR, which is the one registration that
// could not happen before the goroutine: the card is in the offer, the offer
// is chosen by Plan, and Plan is what the slot does first. Everything the
// supervisor has to know before it spawns -- the kind, the provider and the
// ask -- `reserve` already wrote.
//
// IT KICKS, and that is what makes the conservative window cheap rather than
// merely short. Until this call the supervisor excludes every request of this
// rental's kind and provider from getting a node of its own -- see
// genstore.Compat.Card -- so a request this rental turns out NOT to be able to
// serve is waiting on exactly this call. Without the kick it would wait out
// idlePoll on top of the plan.
func (q *Queue) advertise(opener genstore.ReqID, card node.GPUModel) {
	q.mu.Lock()
	c, ok := q.live[opener]
	if ok {
		c.Card, c.CardKnown = card, true
		q.live[opener] = c
	}
	q.mu.Unlock()
	if ok {
		q.Kick()
	}
}

// claim takes the oldest queued request no live rental can serve and starts a
// rental for it. It reports whether it started one.
//
// THE CLAIM AND THE REGISTRATION ARE BOTH HERE, ON THE SUPERVISOR, which is
// the whole of what stops runaway scale-out: `reserve` writes what this rental
// will serve before the slot goroutine exists, so the supervisor's very next
// claim already excludes it. A compat registered from inside the slot would
// leave a window the width of a provider's create call -- seconds on RunPod --
// in which a second compatible request gets a card of its own.
//
// `live` IS PASSED IN RATHER THAN READ HERE, because the capacity and the live
// set have to be one observation: see `free`.
func (q *Queue) claim(ctx context.Context, wg *sync.WaitGroup, live []genstore.Compat) (bool, error) {
	lease := string(genstore.NewReqID())

	// THE CREATE-SIDE CLAIM: work none of the rentals already up can take.
	// The three ride fields are deliberately empty -- this is the claim that
	// decides whether to CREATE, so it takes the oldest queued request
	// whatever it asked for, minus everything a live rental will absorb. See
	// genstore.Compat.Live.
	first, ok, err := q.Store.ClaimNext(ctx, lease, genstore.Compat{Live: live})
	if err != nil {
		return false, fmt.Errorf("claim a request: %w", err)
	}
	if !ok {
		return false, nil
	}

	// THE WORKLOAD IS IN Compat, so a speech request cannot ride an sd rental
	// and a request for one declared image workload cannot ride another's.
	// Without it the provider and the GPU would match, the claim would
	// succeed, and the drain loop would hand an utterance to a resident
	// sd-server that has no route for it -- or a FLUX batch to a server
	// holding SD1.5 -- after a card had been paid for. See genstore.Compat
	// for why the entry id and not the driver.
	compat := genstore.Compat{
		Workload: genstore.WorkloadOf(first.Workload, first.Kind),
		Kind:     genstore.KindOf(first.Kind),
		Provider: first.Provider,
		GPU:      first.GPU,
	}
	q.spawn(ctx, wg, first.ID, compat, func(ctx context.Context) error {
		return q.once(ctx, lease, first, compat)
	})
	return true, nil
}

// spawn takes a slot and runs one rental on a goroutine of its own.
//
// THE RESERVE IS BEFORE THE `go`, which is not an ordering detail: the
// supervisor is the only thing that increments `slots` and writes `live`, so
// doing both here means its next decision sees this rental whether or not the
// goroutine has been scheduled. The release is the slot's own defer, so there
// is no path out -- error, panic unwinding, or an ordinary return -- that
// leaves a slot taken for a node that is gone.
func (q *Queue) spawn(
	ctx context.Context,
	wg *sync.WaitGroup,
	opener genstore.ReqID,
	compat genstore.Compat,
	work func(context.Context) error,
) {
	q.reserve(opener, compat)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer q.freeSlot(opener)
		q.slot(ctx, work)
	}()
}

// slot is where a rental runs, and it is a named method for a reason.
//
// IT IS THE FRAME A TEST LOOKS FOR. `TestSubmit_TheRunExecutesOnTheSlotsOwnGoroutine`
// asserts this function is in the submitted run's own stack, because the three
// behavioural assertions beside it are satisfied by any mechanism that
// serialises correctly -- including a goroutine per submission with a mutex,
// which is a counter with better manners and is the shape somebody reaches for.
// A run that executed anywhere but on a slot this supervisor spawned is a
// rental this process did not count, and that is the whole exposure at N>1.
func (q *Queue) slot(ctx context.Context, work func(context.Context) error) {
	if err := work(ctx); err != nil {
		// Reported and not returned. A request that could not be planned is
		// that request's problem; taking the supervisor down would mean one
		// bad config stops every later request from running.
		q.log().Error("a rental ended badly", "err", err)
	}
}

// once rents for a request the supervisor has already claimed, and drains
// everything compatible onto that one rental.
//
// IT NO LONGER CLAIMS, AND THAT IS WHERE THE CLAIM WENT RATHER THAN A
// SIMPLIFICATION. This function used to open with `ClaimNext` and ran inline
// in Run's loop; at max_nodes>1 the claim has to happen on the supervisor and
// the compat has to be registered before the next decision, or two compatible
// requests provoke two nodes while the first is still provisioning. So Run
// claims, `reserve` records, and this is what a slot then does with it. The
// package's own tests drive the two in that order through `claimAndRun` --
// see the note there about what that helper cannot see.
func (q *Queue) once(ctx context.Context, lease string, first genstore.Request, compat genstore.Compat) error {
	// HELD FROM THE CLAIM AND NOT FROM THE GENERATION. Everything between the
	// supervisor's claim and drain's first iteration is provisioning, and the
	// row has read `running` since that claim -- so this is the window a
	// cancel used to be answered in with "restart ember serve". Given back
	// when this returns, which is after the deferred destroy: a request whose
	// rental is still being torn down is still this process's.
	held := &holder{q: q}
	held.take(first.ID)
	defer held.done()

	// AND THE COMMITMENT IS GIVEN BACK HERE, which is the other half of
	// commit.go and is a defer before the plan for the same reason the holder
	// is: by the time this returns the deferred teardown has emitted run_cost
	// and Recorder has flushed it urgently, so the ledger a concurrent create
	// reads now has the figure and this process no longer needs to remember
	// it. Releasing a key nothing admitted is a no-op, which is what makes
	// this correct on the path where Plan refused before admitting anything.
	defer q.Commit.Release(first.ID)

	rental, err := q.Planner.Plan(ctx, first)
	if err != nil {
		// Nothing was rented, so nothing is billing. The request carries the
		// reason, which is the whole point of resolving before renting.
		q.fail(first.ID, err)
		return fmt.Errorf("request %s: %w", first.ID, err)
	}

	// THE ADVERTISEMENT, AND IT IS THE FIRST STATEMENT AFTER THE PLAN. The
	// offer is chosen and nothing is rented yet, so this is the earliest
	// moment the card exists and the latest one at which the supervisor is
	// still excluding conservatively. Both copies are written here: the local
	// compat, which this rental rides by for the rest of its life, and the
	// live entry the supervisor excludes by.
	//
	// IT IS WHY THE RIDE SIDE IS EXACT FROM THE FIRST CLAIM. drain's own
	// ClaimNext reads `compat`, and a rental that learned its card only when
	// the node came up would spend its whole provisioning refusing requests it
	// could serve -- which is the create-side defect in the other direction.
	compat.Card, compat.CardKnown = rental.Card(), true
	q.advertise(first.ID, compat.Card)

	// AND THE FLEET ROW, BESIDE THE ADVERTISEMENT AND FOR ITS REASON: this is
	// the earliest moment the card is known, and the money is committed from
	// here whether or not the create succeeds. A table that only listed
	// rentals whose node had come up would be silent for the whole of a
	// create -- which on RunPod is minutes, and is exactly when an operator
	// who pressed the button wants to know what ember is doing.
	//
	// Given back by the defer below rather than at each return, because there
	// is no path out of this function that may leave ember claiming to hold a
	// node it has destroyed.
	q.enlist(first.ID, node.ProviderID(first.Provider), compat.Card, rental.Image())
	defer q.delist(first.ID)

	// Released after the rental returns and not inside it: the rental's cost
	// and a destroy that failed arrive from the deferred teardown, which runs
	// after the closure below has finished.
	var served []genstore.ReqID
	defer func() { q.release(served...) }()

	// ONE RENTAL EITHER WAY, AND STILL ONE SLOT. This is a branch over which
	// rental to open, not over where the work happens: both arms call q.drain,
	// which is called from nowhere else, and the rental lives inside this one
	// slot -- which the supervisor counted before this goroutine existed.
	// Nothing here is a second path that could rent --
	// cmd/check-coverage's TestNothingRentsOutsideInternalApp is what bounds
	// that, and neither arm names provision.WithNode.
	var rentErr error
	switch genstore.KindOf(first.Kind) {
	case genstore.KindTTS:
		// THE ENTRY'S OWN CATALOGUE, resolved from the file this process was
		// started with. Narrow has already refused a row whose entry no
		// longer has this driver, so the lookup below cannot land on an sd
		// entry -- and a miss here is a row whose entry was deleted between
		// the claim and now, which is a refusal rather than a rental.
		// ONE RETURN VALUE, because master moved this arm out of `claim` and
		// into the slot goroutine, which answers an error alone: whether the
		// loop made progress is the supervisor's question and is already
		// settled by the time a rental is being driven.
		cat, cerr := q.voicesFor(first)
		if cerr != nil {
			q.endIfUnfinished(first.ID, cerr)
			return cerr
		}
		rentErr = rental.WithSpoken(ctx, q.watchNode(first.ID), func(ctx context.Context, sp app.Spoken) error {
			// NIL, AND THE NIL IS THE STATEMENT. A speech rental runs piper
			// over `ssh exec` and opens no control channel, so there is no
			// worker on the node to beat -- which is WorkerQuiet rather than
			// WorkerStale. Written here in the same place the sd arm
			// registers its pulse, so a kind added later cannot acquire
			// "there is nobody to answer" by forgetting to say anything.
			q.onChannel(first.ID, nil)
			// NO PutCaps, and that is the absence rather than an omission:
			// piper publishes no capabilities, so there is nothing to cache
			// and nothing a request could be validated against that
			// internal/tts does not already check before anything is
			// rented. A `caps` row written here would be an sd server's
			// shape holding a speech rental's facts.
			return q.drain(ctx, ttsRunner{sp: sp, cat: cat}, lease, compat, first, &served, held)
		})
	default:
		rentErr = rental.WithServed(ctx, q.watchNode(first.ID), func(ctx context.Context, s app.Served) error {
			// The channel is open by the time this closure runs -- withServer
			// opens it, completes the handshake and waits for the weights
			// before calling fn -- so this is the first moment there is a
			// pulse to read, and every moment after it is one the fleet view
			// can ask about.
			q.onChannel(first.ID, s.Beat)
			// Learned once per RENTAL SHAPE and cached, so a mistyped sampler
			// is refused at the moment it is typed rather than a minute into
			// a rented card.
			//
			// KEYED, WHERE IT USED TO BE ONE ROW PER PROCESS. The defence for
			// the single row was "`sd.serve` is fixed by the config, so every
			// rental in this process reports the same capabilities", and the
			// request seal it rests on is what refutes it: a request names a
			// provider AND a card, so two live rentals of one process differ
			// in both -- and a generation server's capability list legitimately
			// differs with the card, which is the entire reason `images:`
			// maps an architecture to a build. With one row the last rental up
			// overwrote the cache and the next enqueue was validated against
			// another node's answer. See genstore.Store.PutCaps.
			//
			// THE SHAPE COMES FROM THE COMPAT AND NOT FROM THE OFFER, which is
			// deliberate even though the card is in hand by now: the only
			// reader is enqueue validation, which has a request and no node,
			// so the key has to be something a request can produce.
			if err := q.Store.PutCaps(ctx, compat.Shape(), s.Caps()); err != nil {
				q.log().Warn("could not cache the server's capabilities; the next request will be "+
					"checked against a card instead of against a cache", "err", err)
			}
			return q.drain(ctx, sdRunner{s: s}, lease, compat, first, &served, held)
		})
	}
	if rentErr != nil {
		// The rental broke. If drain never reached this request, nothing has
		// written how it ended and the row still says `running`; if it did,
		// this leaves what drain wrote alone.
		q.endIfUnfinished(first.ID, rentErr)
	}
	return rentErr
}

// drain generates each request in turn on one rental.
//
// It returns when nothing compatible is left, which destroys the node: no idle
// timeout, because a node with nothing to do is a node billing for nothing.
func (q *Queue) drain(ctx context.Context, s runner, lease string, compat genstore.Compat, first genstore.Request, served *[]genstore.ReqID, held *holder) error {
	req := first
	for {
		*served = append(*served, req.ID)
		// A rider is held too, from the moment it is claimed: the claim at
		// the bottom of this loop writes `running` to its row, and there is
		// a real gap before generate registers its stop channel -- the model
		// is resident by now, so it is short, and a short window answered
		// with a destructive remediation is still answered wrongly.
		//
		// THROUGH THE SLOT'S OWN holder, so taking the rider hands the
		// previous request's entry back in the same breath: `held` is a set
		// across slots now, and a rider that simply ADDED to it would leave
		// every finished request on this rental reported as held for the life
		// of the node.
		held.take(req.ID)
		// WHICH MODEL SERVED THIS REQUEST, recorded before it generates and
		// to EVERY request on the rental rather than only to the opener.
		//
		// That is the opposite of how the rental's cost, its memory figures
		// and its load spans are written, and the difference is that those
		// are money: attributing one rental's cost to each of its requests
		// multiplies the bill, so they go to whichever request paid for the
		// node to exist. A model is a label, not a quantity -- every request
		// this rental serves ran against the same weights -- and writing it
		// to the opener alone would split one model's spend between its own
		// name and the dash, which is the figure `ember spend --by model`
		// exists to produce.
		//
		// From s.Caps() and not from the config: `sd.serve` is editable
		// between rentals, and the capabilities are a response. There is no
		// event carrying the model, so unlike the cost columns this one is
		// not a derived index of the event stream -- the response is the
		// authority, read once per rental, cached by PutCaps a few lines up.
		// AN EMPTY NAME MEANS NOBODY READ A MODEL, and never "a server
		// answered and named nothing". This write happens only after
		// WaitModel reported ready, and a server with no weights reports
		// `empty`, which that loop retries and ends as a NotReadyError -- so
		// the rental fails before a claim gets this far, and s.Caps() is only
		// ever read from a server that loaded something. What leaves the
		// column empty is an `ember up` with `cmds` and no server at all, or
		// a request from before the column existed. That is what groupExpr
		// and keyOr already act on, and what `ember history`'s footnote says
		// in words.
		//
		// The earlier version of this comment defended the same write with
		// the unreachable case -- and the shape does exist, because
		// capabilities_no_model.json really reports
		// `{"name":"","path":"","stem":""}`; it is still the right fixture
		// for the feature-gating path, which is a different question and does
		// not pass through here. A reader who believed it had two ways to
		// act, both wrong: make the column nullable to tell the two states
		// apart, which is a branch nothing can reach, or read a stored `''`
		// as evidence that a server answered with no name, when it is
		// evidence that no server was asked. A comment that is wrong about
		// why a seal holds is how the seal gets removed by somebody being
		// helpful -- the AssertNoStray comment that claimed the cleanup ran
		// "before AssertNoStray records the failure, never instead of it"
		// while the code did the opposite. Nothing is invented either way:
		// inventing a value here is the mistake the column exists to end.
		//
		// The model_resident event carries the same name, so the OPENER has
		// it written twice with the same value. That is not two sources of
		// truth -- both read one /capabilities response -- but two carriers
		// of it, and each is the only one available on its own path: the
		// queue holds the app.Served and `ember up` holds nothing but a
		// sink. A rider is why this write cannot be dropped in favour of the
		// event: the rental's own events reach the opener's sink alone.
		//
		// AND THE NODE, FOR THE SAME REASON AND NOT FOR THE COST'S. This is
		// the hole max_nodes>1 opened in the queue table: `requests.node` is
		// written through from the `node_ready` EVENT, and that event reaches
		// the opener's sink and no other -- so every rider's node column was
		// empty, which is precisely where a fleet view is meant to look.
		//
		// IT IS FIXED HERE AND NOT BY ROUTING THE EVENT, and the difference
		// is the whole of Queue.lifecycle's rule. node_ready and run_cost
		// travel together to the opener because the opener is the request
		// that paid for the rental to exist; splitting them so that riders
		// saw node_ready would mean a second sink's worth of lifecycle
		// routing, one field away from a rider being credited with the
		// rental's bill -- "attributing the whole rental to every request
		// would multiply it". So the EVENT routing is untouched and this is
		// an ordinary row write off the event path, exactly like the model
		// above it, and for the identical reason: the node is a LABEL. Every
		// request this rental served ran on that machine, so writing it to
		// each says one true thing N times. Writing the cost to each would
		// say one bill N times, which is the thing being refused.
		//
		// THE VALUE IS THE ONE THE EVENT CARRIED, read back from the fleet
		// entry the lifecycle tap filled in -- see watchNode. Not a second
		// spelling of the handle built here from an offer or a record: two
		// spellings of one node id is a fleet view and a queue table naming
		// one machine two ways, which is the defect class viewFinding's own
		// comment records.
		//
		// EMPTY IS LEFT ALONE rather than written, because an empty column
		// and a column written with nothing are the same dash to every
		// reader -- and the one way to be here with no node is the rental
		// having reached a drain loop before its own node_ready landed, which
		// is a window, not a fact about the request.
		node := q.nodeOf(first.ID)
		q.onRequest(first.ID, req.ID)
		q.set(req.ID, func(r *genstore.Request) {
			r.Model = s.Model()
			if node != "" {
				r.Node = node
			}
		})
		err := q.generate(ctx, s, req)
		q.finish(req.ID, err)
		// THE RENTAL STAYS AND THE REQUEST GOES, which is the gap between two
		// batches: the node is up and billing while this loop claims the next
		// one, so the fleet row must stay and say it is generating nothing.
		q.onRequest(first.ID, "")
		switch {
		case errors.Is(err, app.ErrCancelled):
			// A CANCELLED REQUEST DOES NOT STOP THE RENTAL, and that is the
			// entire point of being able to cancel one. The operator asked
			// for this batch to stop, not for the node to go away -- and the
			// requests behind it on this rental are other people's work, or
			// their own, which they said nothing about. Treating this like a
			// failure would reinstate exactly the behaviour webapi's old 409
			// refused the cancel in order to avoid, this time without the
			// warning.
			q.log().Info("a request was cancelled while generating; the rental carries on",
				"request", req.ID)
		case err != nil:
			// A failed batch stops the rental. A rental that carried on after
			// one request failed would bill for work whose premise an
			// operator is now going to re-examine, and the remaining
			// requests are still queued -- they get a fresh node.
			return fmt.Errorf("request %s: %w", req.ID, err)
		}
		if ctx.Err() != nil {
			return nil
		}

		// THE RACE THAT MATTERS, and it is why ClaimNext is one statement: a
		// request pushed a moment ago must be served by the node that is
		// still up, and one pushed after this returns must start a new
		// rental. There is no third answer, and no window in which a request
		// is neither.
		next, ok, err := q.Store.ClaimNext(ctx, lease, compat)
		if err != nil {
			return fmt.Errorf("claim another request for this rental: %w", err)
		}
		if !ok {
			return nil
		}
		req = next
	}
}

// generate runs one request's jobs and records what came back.
func (q *Queue) generate(ctx context.Context, s runner, req genstore.Request) error {
	stop := make(chan struct{})
	q.mu.Lock()
	if q.current == nil {
		q.current = map[genstore.ReqID]struct{}{}
	}
	if q.stops == nil {
		q.stops = map[genstore.ReqID]chan struct{}{}
	}
	q.current[req.ID] = struct{}{}
	q.stops[req.ID] = stop
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		// Not closed here: Cancel may already have, and the only reader is
		// the Generate call this defer runs after.
		//
		// BOTH DELETES ARE NOW REQUIRED RATHER THAN HYGIENE, which is what
		// changed when these became maps. The old pair of scalars was
		// overwritten by the next batch, so clearing them was tidiness and the
		// guard was the id check in Cancel; a map entry nobody deletes is a
		// leak that grows per request for the life of the process, and a stale
		// `current` entry is a request reported as generating after it
		// finished. Cancel's lookup is still the guard -- it is keyed by id, so
		// a cancel arriving a moment too late reaches neither the next request
		// on this rental nor any request on another one.
		delete(q.current, req.ID)
		delete(q.stops, req.ID)
		q.mu.Unlock()
	}()

	// THE CONVERSION FROM A STORED ROW TO A DRIVER'S BATCH MOVED TO THE
	// RUNNER, which is what made a second kind possible without a switch
	// here: this function is the stop channel's registration and nothing
	// else, and it reads the same for both kinds.
	dir := q.dirOf(req.ID)
	snk := q.sinkFor(req.ID)
	err := s.Generate(ctx, snk, req, dir, stop)
	snk.flush()
	return err
}

// fail records a request that never got a node.
//
// THE THIRD WRITER OF A TERMINAL STATUS, found by sweeping the class #159
// names two of. Its own call site says "nothing was rented, so nothing is
// billing -- the request carries the reason, which is the whole point of
// resolving before renting", and that was untrue in one window: Planner.Plan
// takes the drain loop's context, so a SIGINT landing during the offer
// listing, the store pre-flight or the period check returned a context error
// AND left this write unable to land. Same writeCtx for the same two reasons.
//
// Through classify rather than a literal StatusFailed, because detaching the
// context without that would have traded a row with no reason for a row with a
// confident wrong one: a plan a money ceiling refused is a failure, and a plan
// the operator interrupted is not. There is one account of what Ctrl-C means
// in this package and this is now the third caller of it.
func (q *Queue) fail(id genstore.ReqID, cause error) {
	status, msg := classify(cause)

	ctx, cancel := writeCtx()
	defer cancel()

	if uerr := q.Store.Update(ctx, id, func(r *genstore.Request) {
		r.Status = status
		r.Err = msg
		r.Ended = q.at()
	}); uerr != nil {
		q.log().Error("could not record a failed request", "request", id, "err", uerr)
	}
}

// classify maps the error a request ended with onto the row it leaves behind.
//
// ONE FUNCTION, AND FOR A WHILE THERE WERE TWO. The writers are fail, for a
// request that never got a node; drain, for one it generated; once, for one
// whose rental broke before drain could reach it; and Recorder.Finish, which
// is how `ember up` and a run submitted over the socket record the same fact.
// Two copies of this switch are two accounts of what Ctrl-C means -- and
// record.go held the second one, `statusOf`, byte-for-byte the same answers
// under a comment claiming to be the single spelling, while this one's comment
// claimed the same thing. See the note where it was: it predated this function
// and the commit that extracted this one in the name of having only one is the
// commit that gave the package two.
//
// The interesting answer is the one that is not obvious: a cancelled context
// is NOT a failure.
func classify(cause error) (genstore.Status, string) {
	switch {
	case cause == nil:
		return genstore.StatusDone, ""
	case errors.Is(cause, app.ErrCancelled):
		// The operator asked for this one. Recorded with its reason, which
		// names how far it got and whether the job in flight could be
		// stopped -- the event stream has the detail, and the row is what
		// `ember ls` and the queue table read.
		return genstore.StatusCancelled, cause.Error()
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		// Not a failure. The operator pressed Ctrl-C, or the node reached its
		// deadline; calling that a failed generation would send them looking
		// for a bug that is not there.
		//
		// THERE IS A THIRD CAUSE AND IT IS CONFLATED WITH THE OTHER TWO,
		// which is #171.
		// `http.Client.Timeout` expiring satisfies
		// errors.Is(err, context.DeadlineExceeded), and internal/httpx sets a
		// 60s DefaultTimeout on every client -- so a provider that did not
		// answer inside app.NewPlan lands here, and the row then says the
		// operator stopped a run the provider never started. That is the
		// mirror of the confidently-wrong row the literal StatusFailed in
		// fail used to write, and it is recorded rather than fixed here for
		// two reasons: finish and endIfUnfinished have answered this way
		// since 001a0d3, so routing fail through this function makes the
		// third writer consistent rather than inventing the conflation, and
		// requests.err carries the provider's own sentence either way.
		//
		// The discriminator that would separate them is whether OUR context
		// ended, which is a different question from what the error wraps --
		// and it is exactly the input writeCtx's no-context-parameter design
		// removes from these writers on purpose. So telling the two apart is
		// a change to what a writer is told rather than to this switch, and
		// belongs to whoever takes #171 -- cited by number because a pointer
		// to an issue nobody can find is the shape this repository keeps
		// correcting.
		return genstore.StatusCancelled, cause.Error()
	default:
		return genstore.StatusFailed, cause.Error()
	}
}

// finish records how a request ended.
//
// IT TAKES NO CONTEXT, and that is the fix for #159 rather than an economy.
// This used to be handed the drain loop's own -- the context `ember serve`
// cancels on SIGINT, which is what produced most of the errors that reach
// here -- so the write answered `begin: context canceled` and the row kept the
// `running` ClaimNext wrote: a generation an operator stopped with Ctrl-C came
// back from the next start swept to `interrupted` with no reason anywhere. A
// parameter the caller cannot supply is a parameter nobody can get wrong,
// which is this repository's "prefer making a bug unrepresentable over testing
// for it" rule; writeCtx has both halves and the argument for each.
func (q *Queue) finish(id genstore.ReqID, cause error) {
	status, msg := classify(cause)

	ctx, cancel := writeCtx()
	defer cancel()

	if err := q.Store.Update(ctx, id, func(r *genstore.Request) {
		r.Status = status
		r.Err = msg
		r.Ended = q.at()
	}); err != nil {
		q.log().Error("could not record how a request ended", "request", id, "status", status, "err", err)
	}
}

// endIfUnfinished records how a rental ended for a request drain never
// reached, and leaves alone one it did.
//
// WHAT IT IS FOR. ClaimNext writes `running` and the terminal status is
// written by drain, inside WithServed's closure -- so every failure between
// those two points returned with the row still reading `running`: the create
// call, waiting for sshd, the GPU probe, the asset verify, the model load.
// Every one of them happens after money has been committed, and nothing
// corrected the row afterwards, because Interrupt only sweeps at startup. The
// operator was left with a request that ran for the life of the process
// against a rental that no longer existed, no reason recorded in
// requests.err, and a cancel button answering with the provisioning-window
// 409 -- "the rental is coming up" -- which by then is false.
//
// WHY IT IS CONDITIONAL AND NOT A SECOND WRITE. drain calls finish for every
// request it generates, so by the time an error reaches here the row may
// already say how it really ended -- done, or cancelled by the operator, or
// failed with the server's own sentence. An unconditional write would replace
// that with whatever the rental returned, which is this repository's
// two-writers rule exactly: the loser overwrites the one that knew. A
// teardown that failed after a successful batch is the ordinary case, and it
// must not turn a done request into a failed one.
//
// The check is inside the mutator rather than a read before it, because
// Store.Update runs SELECT, the mutator and UPDATE in one transaction: asking
// first and writing after would be the same decision with a window in it.
//
// IT TAKES NO CONTEXT, for finish's reason and more sharply: the error being
// recorded here is the one the RENTAL returned, and the ordinary way for a
// rental to return an error is a cancelled context. So this writer's most
// common input was precisely the state in which its write could not land --
// the half-working fix #159 is about, in the one case its own argument is
// written around.
func (q *Queue) endIfUnfinished(id genstore.ReqID, cause error) {
	status, msg := classify(cause)

	ctx, cancel := writeCtx()
	defer cancel()

	if err := q.Store.Update(ctx, id, func(r *genstore.Request) {
		if r.Status.Terminal() {
			return
		}
		r.Status = status
		r.Err = msg
		r.Ended = q.at()
	}); err != nil {
		q.log().Error("could not record a request whose rental failed", "request", id, "err", err)
	}
}

// release drops a request's event stream once nothing more will be emitted
// for it.
//
// Not in finish: a rental's cost and a destroy that failed arrive from a
// deferred teardown, AFTER the last batch has been recorded as done, and they
// belong to the request that opened the rental. Forgetting its stream at
// finish would renumber them from 1 and overwrite the progress already
// stored.
func (q *Queue) release(ids ...genstore.ReqID) {
	for _, id := range ids {
		q.forget(id)
	}
}

// lifecycle routes a rental's own events to the request that opened it.
//
// WHICH REQUEST GETS THE RENTAL'S COST is a real question with no tidy answer,
// so the answer is written down: node_ready and run_cost go to the request
// that opened the rental, because it is the one that paid for it to exist.
// Every request that rode along gets its own per-job card time, which its own
// sink records -- and those two figures differ by exactly the per-rental
// overhead this whole shape exists to pay once. Summing the per-request
// figures and calling it the bill would understate it; attributing the whole
// rental to every request would multiply it.
//
// It is the opener's ordinary sink and nothing more, which is the point: the
// write-through of those events to the row lives in Recorder, where `ember up`
// reaches the same code rather than a second copy of it. What makes "only the
// opener is charged" true is that these events are emitted HERE, to this one
// request's sink, and a rider's sink never sees them.
func (q *Queue) lifecycle(opener genstore.ReqID) report.Sink { return q.sinkFor(opener) }

// set applies fn to a request row, off the event path.
//
// Only drain's model write uses it now: everything else a rental learns is
// written through by Recorder, from the events it already emits. WithoutCancel
// for Recorder.set's reason -- a teardown's context is already cancelled on
// Ctrl-C, and the row is the only record of what the rental cost.
func (q *Queue) set(id genstore.ReqID, fn func(*genstore.Request)) {
	ctx, cancel := writeCtx()
	defer cancel()
	if err := q.Store.Update(ctx, id, fn); err != nil {
		q.log().Error("could not record a rental's lifecycle", "request", id, "err", err)
	}
}

// sinkFor is this request's event stream, created once.
func (q *Queue) sinkFor(id genstore.ReqID) *sink {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.sinks == nil {
		q.sinks = map[genstore.ReqID]*sink{}
	}
	if s, ok := q.sinks[id]; ok {
		return s
	}
	rec := &Recorder{
		Store: q.Store, Req: id, Now: q.at, Log: q.log(),
		// The blob key, because this request's images are served by the web
		// UI out of a store rooted at Outputs -- see Recorder.Key, and
		// dirOf, which names the same path on disk.
		Key: func(name string) string { return BlobKey(id, name) },
	}
	s := &sink{Stream: report.NewStream(q.at, rec), buf: rec}
	q.sinks[id] = s
	return s
}

// forget releases a finished request's stream, so a long-running server does
// not hold one per request it has ever served.
func (q *Queue) forget(id genstore.ReqID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.sinks, id)
}

// sink is one request's event stream: numbered, then buffered, then stored.
//
// THE NUMBERING IS WHY THIS EXISTS, and its absence was a real bug that only
// running the thing found. Emitters do not set Seq -- report.Stream does, and
// nothing was wrapping them -- so every event arrived as Seq 0, and
// genstore's INSERT OR REPLACE on (request, seq) meant each one overwrote the
// last. A generation that produced a node_ready, a job_started, four progress
// frames, an images event and a cost left exactly one row.
//
// The unit test did not catch it because its fake set Seq itself, which is
// kinder than anything real: a fake that is tidier than the thing it stands
// in for tests a program that does not exist.
type sink struct {
	*report.Stream
	buf *Recorder
}

// flush writes whatever is buffered.
//
// ON ITS OWN CONTEXT, for the reason finish takes none: generate's only caller
// hands it the drain loop's, and this flush runs AFTER s.Generate has returned
// -- so on Ctrl-C it was the tail of the event stream being written through
// the context that had just ended the generation it describes. The stream is
// the truth and the columns a derived index of it, which makes a lost tail the
// expensive half rather than the cheap one.
func (s *sink) flush() {
	ctx, cancel := writeCtx()
	defer cancel()
	s.buf.Flush(ctx)
}

// Cost is gone, and what replaced it is the point of the change that removed
// it.
//
// It summed a request's card time by scanning that request's entire event
// stream in Go, and it was the only aggregate anywhere in ember -- against a
// table whose facts live inside one opaque JSON blob per event, growing per
// step per job per rental. It was also called by nothing but its own test for
// the whole of its life, so the coverage floor was the only thing keeping it.
//
// genstore.Spend answers the same question with a SUM over columns, and the
// questions it can additionally answer -- per card, per day, cost per image,
// the sampling share of the bill -- are the ones an event loop could never
// have reached. Two cost-summing paths would have been the "two sources of
// truth for one number" mistake Offer.VCPU exists to avoid, so there is one.

// voicesFor is the catalogue the request's own declared workload publishes.
//
// RESOLVED FROM THE ENTRY AND NOT FROM A PROCESS-WIDE FIELD, which is what the
// image moving into the entry forces: two declared speech workloads are two
// images with two voice sets on them. The lookup is `WorkloadOf`'s, so a row
// written before the column and a row naming the implicit entry resolve the
// same way the claim did.
//
// A REFUSAL AND NOT A FALLBACK for an id that is not there. It can only happen
// when the operator deleted or renamed the entry between the claim and here,
// and the honest answer is that this row names a workload this process does not
// have -- a fallback to any other entry would rent a machine for a voice set
// nobody asked for.
func (q *Queue) voicesFor(r genstore.Request) (tts.Catalogue, error) {
	id := genstore.WorkloadOf(r.Workload, r.Kind)
	for _, w := range q.Workloads {
		if w.ID != id {
			continue
		}
		cat, err := w.TTSCatalogue()
		if err != nil {
			return nil, fmt.Errorf("the voices workload %q publishes: %w", id, err)
		}
		return cat, nil
	}
	return nil, fmt.Errorf("this request names workload %q and this ember serve declares no "+
		"such workload; nothing has been rented", id)
}
