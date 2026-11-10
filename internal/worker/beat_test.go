package worker

import (
	"context"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/control/controltest"
)

// beating runs the beat loop over a real control channel, at an interval a
// suite can wait out, and hands back ember's end.
//
// THE REAL CHANNEL AND THE REAL CODEC, like every other test in this package:
// `beat` sends through control.Conn, so what is being exercised is the frame
// the worker actually puts on the wire rather than a counter it increments.
//
// over is returned so a test can end the session the way Session's defer does.
func beating(t *testing.T, every time.Duration) (*control.Conn, chan struct{}, <-chan struct{}) {
	t.Helper()
	over := make(chan struct{})
	stopped := make(chan struct{})
	ember := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		beat(ctx, c, over, every, quiet())
		close(stopped)
		// Held open until the test is done with it: returning here would end
		// the session and close the channel, which is the thing being
		// distinguished from the beat loop stopping.
		<-ctx.Done()
		return nil
	})
	return ember, over, stopped
}

// TestBeat_KeepsSayingSoWithNothingToDo.
//
// THE STATE THE HEARTBEAT EXISTS FOR IS THE ONE WITH NO WORK IN IT. A beat
// that was emitted alongside progress would be a second progress stream: it
// would be current exactly when ember already knows the rental is fine, and
// silent exactly when it does not. So this drives a session with no job, no
// generation server and no frames in either direction, and asserts the frames
// arrive anyway.
//
// THREE AND NOT ONE, because one beat is consistent with a loop that fires
// once and stops -- which is what a `time.After` written in place of a ticker
// does, and is the shape somebody reaches for.
func TestBeat_KeepsSayingSoWithNothingToDo(t *testing.T) {
	ember, over, _ := beating(t, 5*time.Millisecond)
	defer close(over)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	for i := 1; i <= 3; i++ {
		f, err := ember.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for beat %d: %v", i, err)
		}
		if f.Kind != control.KindHeartbeat {
			t.Fatalf("beat %d arrived as a %q frame, want %q", i, f.Kind, control.KindHeartbeat)
		}
	}
}

// TestBeat_StopsWhenTheSessionDoesAndNotWhenTheNodeIsKilled.
//
// THE ORDERING THAT COSTS A GOODBYE. Session closes `over` on its way out, one
// statement before run() sends `done` -- and it is `over` rather than the
// context because the context is cancelled when the NODE is stopped, which is
// later. A beat racing that last frame is a worker that said goodbye and then
// spoke again, which ember's drainWorker loop reads as "the worker did not say
// goodbye" -- the line that is supposed to distinguish a tidy rental from one
// that was cut off, read afterwards on a machine that no longer exists.
//
// THE CHANNEL IS LEFT OPEN AFTERWARDS, deliberately: a test that asserted
// silence over a closed channel would pass against a beat loop that never
// stopped, because every send would fail. The silence has to be the loop's.
func TestBeat_StopsWhenTheSessionDoesAndNotWhenTheNodeIsKilled(t *testing.T) {
	ember, over, stopped := beating(t, 5*time.Millisecond)

	// One beat first, so the assertion below is about a loop that was running
	// rather than one that never started -- the control this would otherwise
	// be missing.
	if _, err := ember.Recv(t.Context()); err != nil {
		t.Fatalf("waiting for the first beat: %v", err)
	}

	close(over)
	select {
	case <-stopped:
	case <-afterLong():
		t.Fatal("the beat loop did not return when the session ended")
	}

	// Anything still in flight is drained with a short deadline, then the
	// channel must go quiet on a connection that is still open.
	quietCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	for {
		f, err := ember.Recv(quietCtx)
		if err != nil {
			// A deadline is the pass: nothing more arrived.
			break
		}
		if f.Kind == control.KindHeartbeat {
			// One frame already on the wire when `over` closed is possible;
			// a stream of them is the defect. The deadline above bounds how
			// many can be read, so reaching here twice means the loop kept
			// going.
			t.Fatalf("a beat arrived after the session ended: the loop is watching the node's "+
				"context instead of the session's, so a beat can race the %q frame that tells "+
				"ember this worker finished tidily", control.KindDone)
		}
	}
}

// TestSession_BeatsForTheWholeSessionAndNotOnlyWhileWorking.
//
// THE WIRING, which the two tests above cannot see: they drive `beat`
// directly, so a Session that never started it would leave both of them green.
// This is the production call with the production interval, which is why it
// asserts ONE beat and bounds it generously rather than counting -- the figure
// is control.HeartbeatEvery and a suite that waited for three of them would
// spend fifteen seconds proving a `go` statement exists.
func TestSession_BeatsForTheWholeSessionAndNotOnlyWhileWorking(t *testing.T) {
	ember, _ := started(t, Options{})

	ctx, cancel := context.WithTimeout(t.Context(), 4*control.HeartbeatEvery)
	defer cancel()
	for {
		f, err := ember.Recv(ctx)
		if err != nil {
			t.Fatalf("no heartbeat arrived within %v of a worker session starting, and this "+
				"session has no job, no server and nothing else to send: %v",
				4*control.HeartbeatEvery, err)
		}
		if f.Kind == control.KindHeartbeat {
			return
		}
	}
}
