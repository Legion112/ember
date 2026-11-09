package worker

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/control"
)

// quiet is a logger that discards.
//
// The queue logs a cancel, and the goroutine that does so outlives the test in
// a session -- so nothing here writes to a *testing.T, which after its test
// has returned is a panic in whatever runs next.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func job(i, total int) control.Frame {
	return control.Frame{Kind: control.KindJob, Index: i, Total: total}
}

// nextWithin is queue.next with a deadline of its own.
//
// A WEDGE PRINTS NO `--- FAIL`, and this file's own reinstatement pass is what
// said so: with the hang-up's `drained` taken out, the test below blocked for
// the full four-minute package timeout and printed a goroutine dump instead of
// a failure naming anything. Every assertion here that expects next to say
// "there will be no more work" is an assertion about a call that BLOCKS when
// it is wrong, so each one carries its own bound -- which is the rule the
// browser labs already follow: race the thing you are waiting for against the
// thing that means it will never happen.
func nextWithin(t *testing.T, q *queue) (control.Frame, bool) {
	t.Helper()
	type answer struct {
		f  control.Frame
		ok bool
	}
	done := make(chan answer, 1)
	go func() {
		f, ok := q.next()
		done <- answer{f, ok}
	}()
	select {
	case a := <-done:
		return a.f, a.ok
	case <-afterLong():
		t.Fatal("WEDGED: next blocked instead of saying there would be no more work, " +
			"which is a hang rather than a failure and is why this carries its own deadline")
		return control.Frame{}, false
	}
}

// TestNextPoll_StartsFastAndCapsOut.
//
// The backoff exists because a flat two seconds measured SLOWER than no
// resident model at all: a 0.3 s job noticed up to 2 s late cost more than the
// 0.8 s of tensor loading being saved, at every batch size tried. The floor is
// what makes a short job noticed almost at once and the cap is what stops a
// long one pulling a preview frame every tenth of a second.
//
// It moved here with the loop it belongs to -- ember used to hold the poll and
// now the worker does, where the call is a syscall on the node rather than a
// round trip to the operator's machine.
func TestNextPoll_StartsFastAndCapsOut(t *testing.T) {
	if got := nextPoll(0); got != jobPollMin {
		t.Errorf("nextPoll(0) = %v, want the floor %v", got, jobPollMin)
	}
	if got := nextPoll(-time.Second); got != jobPollMin {
		t.Errorf("nextPoll(negative) = %v, want the floor %v", got, jobPollMin)
	}
	if got := nextPoll(jobPollMin); got != 2*jobPollMin {
		t.Errorf("nextPoll(%v) = %v, want it doubled", jobPollMin, got)
	}
	if got := nextPoll(jobPollMax); got != jobPollMax {
		t.Errorf("nextPoll(%v) = %v, want it capped", jobPollMax, got)
	}
	// It must actually reach the cap rather than overshoot it.
	d := jobPollMin
	for i := 0; i < 100; i++ {
		d = nextPoll(d)
	}
	if d != jobPollMax {
		t.Errorf("after 100 doublings the interval is %v, want the %v cap", d, jobPollMax)
	}
	// And the floor is a fraction of the cap rather than equal to it, which is
	// the flat cadence the measurement refused: a start that is already the
	// cap is a flat two seconds with extra arithmetic.
	if jobPollMin >= jobPollMax/4 {
		t.Errorf("the floor %v is not meaningfully below the cap %v, so the cadence is flat",
			jobPollMin, jobPollMax)
	}
}

// TestQueue_HandsBackWhatWasPushedInOrder.
//
// A batch arrives as a run of frames and is submitted one at a time, so the
// order is the whole content of the structure: out of order, a batch of eight
// writes eight correctly-named files holding the wrong pictures, which is the
// kind of wrong nobody notices.
func TestQueue_HandsBackWhatWasPushedInOrder(t *testing.T) {
	q := newQueue()
	for i := 1; i <= 3; i++ {
		q.push(job(i, 3), quiet())
	}
	q.push(control.Frame{Kind: control.KindDrained}, quiet())

	for i := 1; i <= 3; i++ {
		f, ok := nextWithin(t, q)
		if !ok {
			t.Fatalf("the queue ran dry at job %d", i)
		}
		if f.Index != i {
			t.Fatalf("got job %d, want %d", f.Index, i)
		}
	}
	if _, ok := nextWithin(t, q); ok {
		t.Error("the queue produced a fourth item from three jobs")
	}
}

// TestQueue_AnEmptyQueueIsNotTheEndOfTheWORK.
//
// THIS IS THE MONEY ASSERTION IN THIS FILE. "The worker exits when its queue
// empties" is true only together with ember having said no more jobs are
// coming, and those are two different facts: a drain loop serving several
// requests on one rental empties this queue between every one of them. A
// worker that stopped at the first empty queue would end the rental there and
// throw away the resident weights, which cost 80-96 s per image to put back --
// the exact figure the whole resident-server shape exists to pay once.
//
// It is asserted as "next blocks", which is what a queue that is empty and not
// drained has to do, and the bound is the test's own: a next() that returned
// instead of blocking would make this fail on the channel read rather than
// hang with no --- FAIL to read.
func TestQueue_AnEmptyQueueIsNotTheEndOfTheWork(t *testing.T) {
	q := newQueue()

	returned := make(chan control.Frame, 1)
	go func() {
		f, ok := q.next()
		if ok {
			returned <- f
		}
	}()

	select {
	case f := <-returned:
		t.Fatalf("next returned %+v from an empty queue nobody has drained", f)
	case <-afterShort():
	}

	// And the next request on the same rental is served by the same worker.
	q.push(job(1, 1), quiet())
	select {
	case f := <-returned:
		if f.Index != 1 {
			t.Errorf("got %+v", f)
		}
	case <-afterLong():
		t.Fatal("a job pushed onto an empty queue was never handed out")
	}
}

// TestQueue_ACancelDropsEveryQueuedJobAtOnce.
//
// WHERE THE MONEY IN A BATCH IS. Stopping only the job in flight would leave
// the next one starting a second later on the same card: the button would
// appear to work and the bill would not change, which is the "reads as
// working" defect this repository keeps finding.
func TestQueue_ACancelDropsEveryQueuedJobAtOnce(t *testing.T) {
	q := newQueue()
	for i := 1; i <= 5; i++ {
		q.push(job(i, 5), quiet())
	}
	q.push(control.Frame{Kind: control.KindCancel}, quiet())
	q.push(control.Frame{Kind: control.KindDrained}, quiet())

	// What comes back is the cancellation to report and nothing else: the five
	// jobs are gone.
	f, ok := nextWithin(t, q)
	if !ok {
		t.Fatal("a cancel nobody has reported is work, and the queue said there was none")
	}
	if f.Kind != control.KindCancelled || f.InFlight {
		t.Fatalf("got %+v, want a cancelled frame with no job in flight", f)
	}
	if next, ok := nextWithin(t, q); ok {
		t.Errorf("a job survived the cancel: %+v", next)
	}
}

// TestQueue_ACancelIsConsumedExactlyOnce.
//
// Both halves of the fork read this flag -- the poll loop of a job in flight,
// and next() when the cancel landed in the gap -- so a flag either could read
// twice would be two cancellation events for one button press, and one neither
// consumed would be ember waiting for ever for frames about jobs that have
// already been dropped.
func TestQueue_ACancelIsConsumedExactlyOnce(t *testing.T) {
	t.Run("by the job in flight", func(t *testing.T) {
		q := newQueue()
		q.push(control.Frame{Kind: control.KindCancel}, quiet())
		if !q.takeCancel() {
			t.Fatal("the job in flight did not see the cancel")
		}
		if q.takeCancel() {
			t.Error("the cancel was taken twice")
		}
		q.push(control.Frame{Kind: control.KindDrained}, quiet())
		if f, ok := nextWithin(t, q); ok {
			t.Errorf("next also reported the cancel the job in flight already handled: %+v", f)
		}
	})

	t.Run("by next, in the gap between jobs", func(t *testing.T) {
		q := newQueue()
		q.push(control.Frame{Kind: control.KindCancel}, quiet())
		f, ok := nextWithin(t, q)
		if !ok || f.Kind != control.KindCancelled {
			t.Fatalf("next = %+v, %v", f, ok)
		}
		if q.takeCancel() {
			t.Error("a job starting afterwards saw a cancel that had already been reported")
		}
	})
}

// TestQueue_ACancelAppliesToTheBatchItArrivedDuringAndNotTheNextOne.
//
// A cancelled request must not end the rental -- that is the whole point of
// cancelling a job rather than a node -- so the request ember claims next is
// served by the card that is already warm. A cancel flag that survived into
// it would drop a batch nobody stopped.
func TestQueue_ACancelAppliesToTheBatchItArrivedDuringAndNotTheNextOne(t *testing.T) {
	q := newQueue()
	q.push(control.Frame{Kind: control.KindCancel}, quiet())
	if f, ok := nextWithin(t, q); !ok || f.Kind != control.KindCancelled {
		t.Fatalf("next = %+v, %v", f, ok)
	}

	// The next request on this rental.
	q.push(job(1, 2), quiet())
	q.push(job(2, 2), quiet())
	q.push(control.Frame{Kind: control.KindDrained}, quiet())
	for i := 1; i <= 2; i++ {
		f, ok := nextWithin(t, q)
		if !ok || f.Kind != control.KindJob || f.Index != i {
			t.Fatalf("the request after a cancelled one lost job %d: %+v, %v", i, f, ok)
		}
	}
}

// TestQueue_AHangUpEndsTheWorkAndCarriesItsReason.
//
// "A node ember cannot reach is a node ember cannot destroy, so there is no
// value in a workload outliving the connection": a job finished after the
// laptop died is a card paid for a picture nobody will fetch. A tidy goodbye
// and a dropped connection both end the work and are told apart, because the
// node's log is the only record of which happened.
func TestQueue_AHangUpEndsTheWorkAndCarriesItsReason(t *testing.T) {
	t.Run("a tidy goodbye", func(t *testing.T) {
		q := newQueue()
		q.push(job(1, 1), quiet())
		q.hangUp(control.ErrClosed)
		if _, ok := nextWithin(t, q); ok {
			t.Error("work survived the channel closing")
		}
		hungUp, err := q.endedBy()
		if !hungUp {
			t.Error("a hang-up does not read as one, so the node's log would say ember drained it")
		}
		if err != nil {
			t.Errorf("a tidy goodbye is a reason: %v", err)
		}
	})

	t.Run("a connection that dropped", func(t *testing.T) {
		q := newQueue()
		q.hangUp(io.ErrUnexpectedEOF)
		hungUp, err := q.endedBy()
		if !hungUp || err == nil {
			t.Errorf("hungUp=%v err=%v; a drop has to carry its reason into the node's log", hungUp, err)
		}
	})

	t.Run("a pending cancel does not survive it", func(t *testing.T) {
		q := newQueue()
		q.push(control.Frame{Kind: control.KindCancel}, quiet())
		q.hangUp(control.ErrClosed)
		if f, ok := nextWithin(t, q); ok {
			// Reporting it would be a send on a channel that is already gone,
			// which turns the ordinary end of a rental into a failure with a
			// transport error in it.
			t.Errorf("next = %+v after a hang-up", f)
		}
	})
}

// TestQueue_NextIsWokenByACancelWhileItWaits.
//
// Without this, a cancel arriving while the queue is empty waits for the next
// push -- which in the gap after the last job of a batch is for ever, so the
// request never ends and ember never learns the cancel landed.
func TestQueue_NextIsWokenByACancelWhileItWaits(t *testing.T) {
	q := newQueue()
	got := make(chan control.Frame, 1)
	go func() {
		if f, ok := q.next(); ok {
			got <- f
		}
	}()
	q.push(control.Frame{Kind: control.KindCancel}, quiet())
	select {
	case f := <-got:
		if f.Kind != control.KindCancelled {
			t.Errorf("got %+v", f)
		}
	case <-afterLong():
		t.Fatal("a cancel did not wake a waiting next")
	}
}

// TestQueue_WokenIsClosedByACancelAndByAHangUp.
//
// The poll loop of a job in flight selects on this, and a cancel it only
// noticed at the next tick is a cancel that takes effect two seconds late --
// which is how a button that works comes to look like one that does not.
func TestQueue_WokenIsClosedByACancelAndByAHangUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		push func(*queue)
	}{
		{"a cancel", func(q *queue) { q.push(control.Frame{Kind: control.KindCancel}, quiet()) }},
		{"a hang-up", func(q *queue) { q.hangUp(control.ErrClosed) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newQueue()
			w := q.woken()
			select {
			case <-w:
				t.Fatal("the poll loop was woken before anything happened")
			default:
			}
			tc.push(q)
			select {
			case <-w:
			case <-afterLong():
				t.Fatal("the poll loop was not woken")
			}
		})
	}
}
