package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
)

// workerFor is the one row of a fleet listing about this rental, or a failure.
func workerFor(t *testing.T, q *Queue, opener genstore.ReqID) Worker {
	t.Helper()
	for _, w := range q.Fleet() {
		if w.Opener == opener {
			return w
		}
	}
	t.Fatalf("no fleet row for the rental %s opened; the fleet is %+v", opener, q.Fleet())
	return Worker{}
}

// TestFleet_ARiderGetsTheNodeAndNotTheCost.
//
// THE HOLE max_nodes>1 OPENED, AND THE EDGE IT HAS TO BE FIXED ALONG.
//
// `requests.node` is written through from the `node_ready` EVENT, and
// Queue.lifecycle routes that event to the request that OPENED the rental and
// to no other -- deliberately, because `run_cost` travels the same road and
// "attributing the whole rental to every request would multiply it". The
// consequence nobody had looked at is that at N>1 every RIDER's node column is
// empty: the second request on a rental generated on a real machine and its
// row cannot say which one, which is exactly where a fleet view looks.
//
// SO THE NODE IS WRITTEN AND THE COST IS NOT, and both halves are asserted
// here because either one alone is satisfied by the wrong fix. Routing
// node_ready to riders would make the first assertion pass and is the change
// that puts one rental's bill on N rows; writing nothing would leave the
// second passing and the hole open. The distinction is that a node is a LABEL
// -- every request on this rental ran on that machine, so writing it N times
// says one true thing N times -- and the cost is a QUANTITY.
//
// THE PACKING IS THE PRECONDITION, so this uses two requests for one card:
// strict packing puts the second on the first's rental, which is the only
// arrangement in which a rider exists at all.
func TestFleet_ARiderGetsTheNodeAndNotTheCost(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	opener := enqueue(t, st, "rtx2080ti", "first")
	rider := enqueue(t, st, "rtx2080ti", "second")

	stop := runLoop(t, q, p)
	waitFor(t, 20*time.Second, "both requests to generate on the one rental",
		func() bool { return p.batchesOn("rtx2080ti") >= 2 })
	// Stopped before the rows are read, so the deferred teardown has emitted
	// run_cost and Recorder has flushed it: a test that looked before that
	// would find no cost on EITHER row and would pass against any routing at
	// all.
	stop()

	rows := map[genstore.ReqID]genstore.Request{}
	for _, id := range []genstore.ReqID{opener, rider} {
		r, err := st.Get(t.Context(), id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		rows[id] = r
	}

	const want = "local/rtx2080ti"
	if rows[opener].Node != want {
		t.Errorf("the opener's node is %q, want %q", rows[opener].Node, want)
	}
	if rows[rider].Node != want {
		t.Errorf("the rider's node is %q, want %q.\n"+
			"node_ready reaches the opener's sink alone, so a rider's node column is empty "+
			"unless the drain loop writes it -- and an empty node on a request that really did "+
			"generate on a machine is a hole exactly where a fleet view looks.",
			rows[rider].Node, want)
	}
	// The SAME spelling, which is the point of taking it from the event
	// rather than rebuilding it from the offer: two spellings of one node id
	// is a fleet view and a queue table naming one machine two ways.
	if rows[opener].Node != rows[rider].Node {
		t.Errorf("the opener says %q and the rider says %q about one machine",
			rows[opener].Node, rows[rider].Node)
	}

	if rows[opener].Cost == 0 {
		t.Fatalf("the opener was charged nothing for the rental it paid for, so the cost "+
			"assertion below is vacuous: %+v", rows[opener])
	}
	if rows[rider].Cost != 0 {
		t.Errorf("the rider was charged %v for a rental it rode. The whole rental's cost is on "+
			"the opener's row; charging it again here is one bill counted once per request that "+
			"rode it, which is the arithmetic Queue.lifecycle exists to refuse.",
			rows[rider].Cost)
	}
}

// TestFleet_SaysWhichRequestIsOnWhichRental.
//
// THE SECOND OF THE THREE THINGS A FLEET VIEW IS FOR, and the one the queue
// table cannot answer: at N>1 it marks several rows `running` and nothing
// says which machine each is on.
//
// TWO INCOMPATIBLE REQUESTS, because that is what makes two rentals: strict
// packing means a homogeneous queue is one node. The kinds differ, which is
// the Compat axis that cannot be two values on one machine.
//
// THE ROWS ARE DISTINCT IN EVERY FIELD THAT IS SUPPOSED TO BE, which is what
// stops this passing against a listing that reports the first rental twice --
// the shape a map iterated into a slice with a captured loop variable
// produces, and the shape of the frozen-row defect one layer out.
func TestFleet_SaysWhichRequestIsOnWhichRental(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	sd := enqueueKind(t, st, genstore.KindSD, "rtx2080ti", "an image")
	speech := enqueueKind(t, st, genstore.KindTTS, "rtx4090")

	stop := runLoop(t, q, p)
	defer stop()
	waitFor(t, 20*time.Second, "two rentals to be open at once", func() bool { return p.gate.now() == 2 })
	waitFor(t, 20*time.Second, "both rentals to appear in the fleet", func() bool { return len(q.Fleet()) == 2 })

	byOpener := map[genstore.ReqID]Worker{}
	for _, w := range q.Fleet() {
		byOpener[w.Opener] = w
	}
	if len(byOpener) != 2 {
		t.Fatalf("the fleet lists %d distinct rentals for two nodes: %+v", len(byOpener), q.Fleet())
	}

	for _, c := range []struct {
		opener genstore.ReqID
		card   node.GPUModel
	}{{sd, "rtx2080ti"}, {speech, "rtx4090"}} {
		w := byOpener[c.opener]
		if w.Card != c.card {
			t.Errorf("the rental opened by %s says card %q, want %q", c.opener, w.Card, c.card)
		}
		if want := "local/" + string(c.card); w.Node != want {
			t.Errorf("the rental opened by %s says node %q, want %q", c.opener, w.Node, want)
		}
		// The image comes from the plan rather than from the request, and the
		// fake derives it from the ask -- so two rentals have two images and
		// a listing that reported one row twice fails here.
		if want := "ember:" + string(c.card); w.Image != want {
			t.Errorf("the rental opened by %s says image %q, want %q", c.opener, w.Image, want)
		}
		if w.Provider != "local" {
			t.Errorf("the rental opened by %s says provider %q", c.opener, w.Provider)
		}
	}
}

// creatingPlanner opens a rental that never reports a node.
//
// IT STANDS IN FOR A CREATE CALL IN FLIGHT, which the fleet planner above
// cannot produce: its rental emits node_ready as its first statement, so there
// is no window between the plan and the machine. On RunPod that window is
// seconds to minutes and the commitment has already been taken, so it is a
// real state and it is the one an operator is watching when they press the
// button.
type creatingPlanner struct {
	started chan struct{}
	release chan struct{}
}

func (p *creatingPlanner) Plan(context.Context, genstore.Request) (Rental, error) {
	return creatingRental{p: p}, nil
}

type creatingRental struct{ p *creatingPlanner }

func (r creatingRental) Card() node.GPUModel { return "rtx2080ti" }
func (r creatingRental) Image() string       { return "ember:lab" }

func (r creatingRental) WithServed(ctx context.Context, _ report.Sink, _ func(context.Context, app.Served) error) error {
	close(r.p.started)
	select {
	case <-r.p.release:
	case <-ctx.Done():
	case <-time.After(20 * time.Second):
	}
	// NO node_ready EVER, and no call of fn: this rental never got a machine.
	return nil
}

func (r creatingRental) WithSpoken(ctx context.Context, s report.Sink, _ func(context.Context, app.Spoken) error) error {
	return r.WithServed(ctx, s, nil)
}

// TestFleet_ARentalBeingCreatedIsAlreadyARow.
//
// MONEY IS COMMITTED FROM THE PLAN AND NOT FROM THE NODE. A create call is
// seconds to minutes, the commitment is taken before it, and a fleet view that
// only listed rentals whose node had come up would be silent for the whole of
// it -- which is exactly when an operator who pressed the button wants to know
// what ember is doing. So the row exists with no node, carrying the card the
// plan chose, and `State` says `starting`.
func TestFleet_ARentalBeingCreatedIsAlreadyARow(t *testing.T) {
	p := &creatingPlanner{started: make(chan struct{}), release: make(chan struct{})}
	st := genstore.NewRecordingStore()
	q := &Queue{
		Store: st, Planner: p, Workloads: speechWorkloads(t),
		Outputs: filepath.Join(t.TempDir(), "outputs"), MaxNodes: 1,
	}
	opener := enqueue(t, st, "rtx2080ti", "first")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()
	defer func() {
		cancel()
		close(p.release)
		select {
		case <-done: //nolint:errcheck // the loop's error is reported by its own log
		case <-time.After(30 * time.Second):
			t.Error("the supervisor did not return")
		}
	}()

	select {
	case <-p.started:
	case <-time.After(20 * time.Second):
		t.Fatal("the rental never started")
	}
	waitFor(t, 20*time.Second, "the fleet to carry the rental being created",
		func() bool { return len(q.Fleet()) == 1 })

	w := workerFor(t, q, opener)
	if w.Node != "" {
		t.Errorf("a rental whose create has not returned reports node %q", w.Node)
	}
	if got := w.State(time.Now()); got != WorkerStarting {
		t.Errorf("a rental being created is %q, want %q", got, WorkerStarting)
	}
	// The card and the image are known from the PLAN, which is why the row is
	// worth drawing at all: a row with nothing in it would be no better than
	// the silence it replaces.
	if w.Card != "rtx2080ti" || w.Image != "ember:lab" {
		t.Errorf("a rental being created says card=%q image=%q, want the plan's", w.Card, w.Image)
	}
}

// TestFleet_IsEmptyOnceEveryRentalHasGone.
//
// THE OTHER DIRECTION, AND IT IS THE ONE THAT READS AS REASSURING WHEN IT IS
// WRONG. A fleet listing a node ember has already destroyed is the max_nodes
// counter being wrong in the direction that makes an operator think they are
// paying for something they are not -- and, worse, it is the listing somebody
// reaches for to answer "did that stop?".
//
// Without it, `enlist` with no `delist` leaves every rental this process ever
// held on the page for the life of the server.
func TestFleet_IsEmptyOnceEveryRentalHasGone(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	enqueue(t, st, "rtx2080ti", "first")

	stop := runLoop(t, q, p)
	waitFor(t, 20*time.Second, "the rental to appear", func() bool { return len(q.Fleet()) == 1 })
	stop()

	if f := q.Fleet(); len(f) != 0 {
		t.Errorf("the fleet still lists %d rental(s) after every slot returned: %+v.\n"+
			"A row for a node ember has destroyed is the listing an operator reads to find out "+
			"whether it stopped.", len(f), f)
	}
}

// TestWorkerState_ClassifiesEveryShapeARentalCanBeIn.
//
// THE SWITCH ON ITS OWN, exhaustively, because the states it has to tell apart
// are reachable through the loop only by waiting out real intervals -- and the
// one that matters most, a worker that stops beating while its card bills, has
// no cheap end-to-end shape at all.
//
// THE ZERO-BEAT CASE IS ASSERTED ON ITS OUTCOME AND NOT ON ITS MECHANISM, and
// a reinstatement is why. `State` used to carry an arm of its own for it;
// disabling that arm was GREEN, because `now` minus the zero time is seventy
// years and the threshold comparison already answers stale. The arm was
// deleted and this row stayed: what has to hold is that a channel which has
// produced nothing does not read as healthy, however that is arrived at.
//
// AND `quiet` IS NOT `stale`. A speech rental runs piper over `ssh exec` and
// opens no control channel, so there is nobody to answer -- a different fact
// from "not answering", with a different remedy, and one that would otherwise
// paint a red row on every `ember serve` that says a word.
func TestWorkerState_ClassifiesEveryShapeARentalCanBeIn(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		w    Worker
		want WorkerState
	}{
		{"no node yet", Worker{}, WorkerStarting},
		{"no node yet, even with a channel", Worker{Channel: true, Beat: now}, WorkerStarting},
		{"no control channel at all", Worker{Node: "local/a"}, WorkerQuiet},
		{"a channel that has never spoken", Worker{Node: "local/a", Channel: true}, WorkerStale},
		{"beating now", Worker{Node: "local/a", Channel: true, Beat: now}, WorkerLive},
		{"one interval ago", Worker{Node: "local/a", Channel: true,
			Beat: now.Add(-control.HeartbeatEvery)}, WorkerLive},
		// ON THE THRESHOLD IS LIVE AND NOT STALE, which is the boundary a `>=`
		// written for a `>` moves: the comparison is "older than", so a beat
		// exactly at the limit is the last one that counts.
		{"exactly at the threshold", Worker{Node: "local/a", Channel: true,
			Beat: now.Add(-control.HeartbeatStale)}, WorkerLive},
		{"a moment past it", Worker{Node: "local/a", Channel: true,
			Beat: now.Add(-control.HeartbeatStale - time.Millisecond)}, WorkerStale},
		{"long past it", Worker{Node: "local/a", Channel: true,
			Beat: now.Add(-time.Hour)}, WorkerStale},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.w.State(now); got != c.want {
				t.Errorf("State = %q, want %q", got, c.want)
			}
		})
	}
}

// TestFleet_ReadsTheBeatAtTheMomentItIsAsked.
//
// THE FROZEN-ROW DEFECT, ON THE GO SIDE OF THE WIRE. A listing that copied
// each rental's beat in when the rental registered would answer the same
// instant for ever: every row would read as live for the first twenty seconds
// of the server's life and stale afterwards, whatever the workers did. It is
// the same defect the browser keeps finding in a keyed `<For>` -- a value
// captured outside the thing that re-reads it -- and it is cheaper to catch
// here.
//
// So the berth holds a FUNC and this asserts that calling Fleet twice, with a
// different answer underneath, gives two different answers.
func TestFleet_ReadsTheBeatAtTheMomentItIsAsked(t *testing.T) {
	q := &Queue{}
	const opener = genstore.ReqID("req-1")
	q.enlist(opener, "local", "rtx2080ti", "ember:lab")
	q.onNode(opener, "local/abc")

	var beat time.Time
	q.onChannel(opener, func() time.Time { return beat })

	first := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	beat = first
	if got := workerFor(t, q, opener).Beat; !got.Equal(first) {
		t.Fatalf("the fleet reports a beat of %v, want %v", got, first)
	}

	second := first.Add(time.Minute)
	beat = second
	got := workerFor(t, q, opener).Beat
	if got.Equal(first) {
		t.Errorf("the fleet still reports %v after the worker beat at %v.\n"+
			"The beat was read once and stored, so every row reports the instant its rental "+
			"registered -- which is a table that is live for twenty seconds and wedged for ever "+
			"afterwards, whatever the workers do.", got, second)
	}
	if !got.Equal(second) {
		t.Errorf("the fleet reports %v, want %v", got, second)
	}
}

// TestFleet_ASpeechRentalIsQuietAndNotStale, through the loop.
//
// The table above states the rule and this is the WIRING: `once` registers a
// nil pulse in the speech arm, and a test of the switch alone would be green
// against an arm that forgot to register anything -- which is the same state
// and arrives by a different route.
func TestFleet_ASpeechRentalIsQuietAndNotStale(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 1)
	opener := enqueueKind(t, st, genstore.KindTTS, "rtx2080ti")

	stop := runLoop(t, q, p)
	defer stop()
	waitFor(t, 20*time.Second, "the speech rental to appear with its node",
		func() bool { return len(q.Fleet()) == 1 && q.Fleet()[0].Node != "" })

	w := workerFor(t, q, opener)
	if w.Channel {
		t.Errorf("a speech rental reports a control channel; piper is run over `ssh exec` and " +
			"there is no worker on the node to beat")
	}
	if got := w.State(time.Now()); got != WorkerQuiet {
		t.Errorf("a speech rental is %q, want %q. There being nobody to answer is a different "+
			"fact from not answering, and a red row on every rental that says a word is a red "+
			"row an operator learns to ignore.", got, WorkerQuiet)
	}
}

// TestFleet_KeepsTheRentalWhenNothingIsGeneratingOnIt.
//
// THE ROW AN OPERATOR MOST NEEDS AND THE ONE A `running` LIST CANNOT SHOW: a
// node that is up, billing, and has nothing on it. Between two batches the
// drain loop is claiming, and a fleet that dropped the row there would be a
// node blinking out of the table and back while it billed throughout -- and,
// worse, would make `Fleet()` a second spelling of `Running()`.
func TestFleet_KeepsTheRentalWhenNothingIsGeneratingOnIt(t *testing.T) {
	q := &Queue{}
	const opener = genstore.ReqID("req-1")
	q.enlist(opener, "local", "rtx2080ti", "ember:lab")
	q.onNode(opener, "local/abc")
	q.onRequest(opener, "req-7")

	if got := workerFor(t, q, opener).Request; got != "req-7" {
		t.Fatalf("the fleet says %q is generating, want req-7", got)
	}

	q.onRequest(opener, "")
	w := workerFor(t, q, opener)
	if w.Request != "" {
		t.Errorf("the fleet still says %q is generating after the batch ended", w.Request)
	}
	if w.Node != "local/abc" {
		t.Errorf("the rental lost its node when its batch ended: %+v", w)
	}
}

// TestFleet_AWorkerThatStopsBeatingGoesStaleWithoutLeavingTheList.
//
// THE MIDDLE ROW OF THE TABLE THE HEARTBEAT EXISTS FOR, driven through the
// real supervisor: the channel is open, the card is billing, and the worker
// has stopped saying anything. A reachability probe cannot tell this from
// working, which is the whole argument for a beat over a /healthz on the node
// -- and a /healthz would have cost a second public-port minter beside
// exposure.SSHPort.
//
// THE ROW HAS TO STAY, which is the half that is easy to get wrong in the
// obvious way: dropping a rental whose worker went quiet would make the one
// state worth seeing the one state that is invisible, and ember would still be
// paying for the card.
//
// THE CONTROL IS THE SAME ROW BEFORE THE BEATS STOP. Without it this is
// satisfied by a listing that reports every rental as stale always -- which is
// exactly what a pulse nobody updates produces, and is the defect on the other
// side of this wire.
func TestFleet_AWorkerThatStopsBeatingGoesStaleWithoutLeavingTheList(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 1)
	opener := enqueue(t, st, "rtx2080ti", "first")

	stop := runLoop(t, q, p)
	defer stop()
	// The rental is held open after its batch -- where the deferred destroy
	// is -- so this observes a live node with nothing generating on it.
	waitFor(t, 20*time.Second, "the rental to be up with its channel registered", func() bool {
		for _, w := range q.Fleet() {
			if w.Opener == opener && w.Node != "" && w.Channel {
				return true
			}
		}
		return false
	})

	before := workerFor(t, q, opener)
	if got := before.State(time.Now()); got != WorkerLive {
		t.Fatalf("a rental whose worker is beating reads %q, want %q -- so the assertion below "+
			"would pass against a listing that says %q about everything", got, WorkerLive, got)
	}

	// The worker stops. Nothing else about the rental changes: the node is
	// there, the channel is open, and the card is billing.
	p.stoppedBeatingAt(time.Now().Add(-control.HeartbeatStale - time.Second))

	after := workerFor(t, q, opener)
	if got := after.State(time.Now()); got != WorkerStale {
		t.Errorf("a worker that has said nothing for longer than %v reads %q, want %q",
			control.HeartbeatStale, got, WorkerStale)
	}
	if after.Node != before.Node || after.Card != before.Card {
		t.Errorf("the row changed identity when it went stale: %+v then %+v", before, after)
	}
}
