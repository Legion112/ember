package queue

import (
	"slices"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
)

// THE FLEET, WHICH IS WHAT max_nodes>1 MADE UNREADABLE.
//
// At one rental the queue table answered every question an operator had: the
// request that said `running` was THE request, on THE node, and `ember ls`
// named the machine it was on. At N rentals the same table says several things
// are running and nothing says which machine each is on, what that machine is,
// or whether it is still answering. Three facts went missing at once and they
// are the three this file is for:
//
//   - which rentals this process is holding, and what each one is;
//   - which request is generating on which one;
//   - whether each one's worker is still there.
//
// IT IS THIS PROCESS'S OWN BOOKKEEPING AND NOT A SECOND SCANNER. Everything
// here is a fact `ember serve` already had in hand and was throwing away:
// reconcile.Scan stays the one authority on what exists at a provider and what
// it costs, and `GET /api/nodes` JOINS the two by the node's handle rather than
// asking twice. Two scanners is two answers to "what am I paying for", and the
// one that disagrees is the one that misses a node -- which is the rule
// nodesView was built on and this does not get an exemption from.
//
// WHAT IS DELIBERATELY NOT HERE: a submitted `ember up`. Those occupy a rental
// slot through Queue.Submit and rent inside a closure the queue cannot see into
// -- Direct.Run is an opaque func, by design, because webapi must not be able
// to reach a plan -- so this process knows that a slot is taken and nothing
// else about it. Such a node is NOT invisible: it is at the provider wearing
// ember's label, so it is a finding in the same response, with its cost and
// its deadline. What it has no row for is the worker, because there is nothing
// here that saw one. Said in README rather than left for a reader to notice a
// row missing.

// WorkerState is what ember can say about one rental's worker.
//
// FOUR STATES AND NOT THE THREE IN THE ARGUMENT, because two of the three are
// about a channel that exists. "Gone" is not a state here at all: a rental
// whose channel closed is a rental whose slot has returned, so it leaves this
// list entirely and reconcile and the node's own deadline take over -- which
// is the honest spelling of it, since a row saying "gone" would be this
// process asserting something about a machine it is no longer talking to.
type WorkerState string

const (
	// WorkerStarting is a rental with no node yet: the offer is chosen, the
	// create may be in flight, and nothing has beaten because there is
	// nothing to beat. On RunPod this is seconds to minutes, and it is money
	// already committed, which is why it is a row rather than an absence.
	WorkerStarting WorkerState = "starting"

	// WorkerLive is a worker that beat within control.HeartbeatStale.
	WorkerLive WorkerState = "live"

	// WorkerStale is the expensive one, and the reason the heartbeat exists
	// rather than a reachability probe: the channel is open, the card is
	// billing, and the worker has stopped saying anything. A port check
	// cannot tell this from working.
	WorkerStale WorkerState = "stale"

	// WorkerQuiet is a rental with no control channel at all, which today is
	// every speech rental: piper is a binary with six flags run over `ssh
	// exec`, so there is no worker on the node to beat and nothing to be
	// stale about.
	//
	// SPELLED DIFFERENTLY FROM WorkerStale, which is the whole point of it
	// having its own name. "Not answering" and "there is nobody to answer"
	// are different facts with different remedies -- one is a node to look at
	// and one is how speech works -- and a speech rental reported as wedged
	// would be a red row on every `ember serve` that says a word.
	WorkerQuiet WorkerState = "quiet"
)

// Worker is one rental this process is holding, as a fleet view sees it.
//
// EVERY FIELD IS WHAT EMBER KNOWS AND NONE IS WHAT THE NODE SAID, with one
// exception that is marked. The node id arrives on the rental's own
// node_ready event -- see Queue.lifecycle -- so it is the same string the
// opener's row records, which is what stops a fleet view and the queue table
// naming one machine two ways.
type Worker struct {
	// Node is the provider's handle, "provider/id", empty while the rental is
	// still being created. It is the join key against reconcile's findings.
	Node string

	// Provider, Card and Image are what this rental IS, from the plan that
	// chose it rather than from anything the machine reported.
	Provider node.ProviderID
	Card     node.GPUModel
	Image    string

	// Opener is the request that paid for this rental to exist, which is the
	// one that `run_cost` and `node_ready` reach. Request is the one
	// generating on it right now, which is a rider as often as not and is
	// empty in the gap between two batches.
	Opener  genstore.ReqID
	Request genstore.ReqID

	// Since is when the rental was planned, on ember's clock.
	Since time.Time

	// Beat is when ember last heard this rental's worker, zero when it never
	// has. Channel says whether there is a worker at all; see WorkerQuiet.
	Beat    time.Time
	Channel bool
}

// State classifies this worker at now.
//
// A METHOD AND NOT A FIELD, because staleness is a question about the instant
// it is asked and a stored answer is an answer that was true once. It goes on
// the wire as a string all the same -- see webapi's workerView -- for
// nodesView.Attention's reason: a second copy of this switch in Rust is a copy
// whose author will not be looking at this one.
func (w Worker) State(now time.Time) WorkerState {
	switch {
	case w.Node == "":
		return WorkerStarting
	case !w.Channel:
		return WorkerQuiet
	case now.Sub(w.Beat) > control.HeartbeatStale:
		// A ZERO BEAT FALLS THROUGH HERE AND THERE IS NO ARM FOR IT, which is
		// a correction made by a reinstatement rather than a choice. There was
		// a `case w.Beat.IsZero(): return WorkerStale` above this line,
		// arguing that an open channel which has produced nothing is reported
		// conservatively -- and disabling it was GREEN, because `now` minus
		// the zero time is seventy years and this comparison already answers
		// stale. A second mechanism saying the same thing is a line that
		// cannot fail, which is the shape this repository keeps finding; the
		// argument survives here and the dead arm does not.
		//
		// What makes a zero beat unreachable on a live channel in the first
		// place is app.dialWorker: control.Open has READ the worker's hello
		// before it returns, so the handshake is the first beat. The
		// conservative answer is what is left for a `served` that somehow has
		// no pulse at all.
		return WorkerStale
	default:
		return WorkerLive
	}
}

// berth is one live rental's entry, written by the slot that holds it.
//
// Guarded by Queue.mu with everything else about slots, rather than by a lock
// of its own: `free` already takes that mutex to answer two questions in one
// observation, and a fleet listing has the same requirement -- a row pairing
// one rental's node with another's request would be worse than no row.
type berth struct {
	provider node.ProviderID
	card     node.GPUModel
	image    string
	since    time.Time

	node    string
	request genstore.ReqID

	// beat is this rental's worker's pulse, nil when the rental has no
	// control channel. A func rather than a time, because what is wanted is
	// the value at the moment the fleet is listed and not at the moment the
	// rental registered.
	beat func() time.Time
}

// enlist records a rental the supervisor has planned.
//
// CALLED AFTER Plan AND NOT AT reserve, which is a deliberate three-statement
// gap. `reserve` runs on the supervisor before the slot goroutine exists and
// knows the request's ASK -- its provider and the card it filtered for -- while
// this runs once an offer has been chosen, so the card here is the card that
// will be rented. A fleet view built from the ask would name a GPU model the
// operator never paid for.
func (q *Queue) enlist(opener genstore.ReqID, provider node.ProviderID, card node.GPUModel, image string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fleet == nil {
		q.fleet = map[genstore.ReqID]*berth{}
	}
	q.fleet[opener] = &berth{provider: provider, card: card, image: image, since: q.at()}
}

// delist forgets a rental whose slot has returned.
//
// THE DEFER THAT PAIRS WITH enlist IS IN `once`, beside the holder's and the
// commitment's, for their reason: there is no path out of that function --
// error, panic unwinding, or an ordinary return -- that may leave a row
// claiming ember is holding a node it has destroyed. A fleet view that listed
// a finished rental would be the `max_nodes` counter being wrong in the one
// direction that reads as reassuring.
func (q *Queue) delist(opener genstore.ReqID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.fleet, opener)
}

// berthOf is the entry for a rental, or nil. Called with the lock held.
func (q *Queue) berthOf(opener genstore.ReqID) *berth { return q.fleet[opener] }

// onNode records the machine a rental got, from its own node_ready event.
func (q *Queue) onNode(opener genstore.ReqID, id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if b := q.berthOf(opener); b != nil {
		b.node = id
	}
}

// onChannel records how to ask this rental's worker when it last beat.
//
// A NIL beat IS THE MEANINGFUL CALL and not a skipped one: it is what a speech
// rental registers, and it is how WorkerQuiet is told from WorkerStale. The
// call site is the same statement in both arms of `once`'s switch, so a kind
// added later has to decide in writing which it is.
func (q *Queue) onChannel(opener genstore.ReqID, beat func() time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if b := q.berthOf(opener); b != nil {
		b.beat = beat
	}
}

// onRequest records which request is generating on a rental.
//
// Cleared with an empty id rather than deleted, because the rental is still
// there between two batches: a row that vanished while its drain loop claimed
// the next request would be a node that blinked out of a fleet view and back
// while it was billing the whole time.
func (q *Queue) onRequest(opener, req genstore.ReqID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if b := q.berthOf(opener); b != nil {
		b.request = req
	}
}

// nodeOf is the machine a rental got, empty until node_ready arrived.
func (q *Queue) nodeOf(opener genstore.ReqID) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if b := q.berthOf(opener); b != nil {
		return b.node
	}
	return ""
}

// Fleet is every rental this process is holding, sorted.
//
// SORTED BY OPENER, which is a genstore.ReqID and therefore time-ordered, so
// the oldest rental is first and a listing taken twice puts the same rental in
// the same place. Not by node: a rental that has not been created yet has no
// node, and sorting on a key half the rows share would churn the table between
// two presses for no reason -- which is exactly why Running() is sorted.
//
// THE BEAT IS READ HERE, INSIDE THE LOCK, which is the one line of this
// function that matters. It is a func on the berth precisely so that the
// instant it answers is the instant the fleet is listed; copying a timestamp
// in at registration would make every row report the moment its rental
// started, and the whole table would read as wedged.
func (q *Queue) Fleet() []Worker {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Worker, 0, len(q.fleet))
	for opener, b := range q.fleet {
		w := Worker{
			Node: b.node, Provider: b.provider, Card: b.card, Image: b.image,
			Opener: opener, Request: b.request, Since: b.since,
			Channel: b.beat != nil,
		}
		if b.beat != nil {
			w.Beat = b.beat()
		}
		out = append(out, w)
	}
	slices.SortFunc(out, func(a, b Worker) int {
		return strings.Compare(string(a.Opener), string(b.Opener))
	})
	return out
}

// watchNode is the opener's own sink with the node's handle tapped out of it.
//
// WHY A TAP AND NOT A NEW ROUTE FOR THE EVENT. node_ready carries the handle
// and reaches the opener's sink alone -- that is Queue.lifecycle's written
// rule, and it is the same rule that keeps run_cost off every rider's row:
// "attributing the whole rental to every request would multiply it". So the
// fleet does not get its own event and the riders do not get their own sink.
// One sink, forwarded unchanged, with ember noting in passing which machine
// this rental is on.
//
// THE MONEY EVENTS ARE NOT TAPPED AND MUST NOT BE. The only field taken here is
// the node id, which is a LABEL: every request on this rental ran on that
// machine, so writing it to each of them says one true thing N times, where
// writing the cost to each would say one bill N times. That is the same
// distinction drain already makes for `requests.model`, and the reason this
// tap reads exactly one field of exactly one kind.
func (q *Queue) watchNode(opener genstore.ReqID) report.Sink {
	// WRAPPED AROUND Queue.lifecycle AND NOT AROUND sinkFor, so that "which
	// request gets the rental's cost" stays answered in exactly one place.
	// Reaching past it to the sink would be a second route to the opener's
	// stream, free to stop agreeing with the first about which request that
	// is.
	return nodeTap{to: q.lifecycle(opener), q: q, opener: opener}
}

type nodeTap struct {
	to     report.Sink
	q      *Queue
	opener genstore.ReqID
}

func (t nodeTap) Emit(e report.Event) {
	if e.Kind == report.KindNodeReady && e.Node != "" {
		t.q.onNode(t.opener, e.Node)
	}
	// FORWARDED WHATEVER HAPPENED ABOVE, and unconditionally. This sink is
	// the opener's whole history: a tap that could swallow an event would be
	// a request whose record is missing the moment its node came up, in
	// exchange for a table.
	t.to.Emit(e)
}
