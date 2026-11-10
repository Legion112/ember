package app

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/control/controltest"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// TestPrepare_ABeatDuringTheModelLoadIsNotAProtocolError.
//
// THE WAIT THE BEAT IS MOST USEFUL DURING IS THE ONE THAT WOULD HAVE REFUSED
// IT. The weights take 80-96 s for an ordinary image and four minutes for
// FLUX.2; nothing else travels on the channel while they load, and the prepare
// loop's `default` arm refuses anything it does not recognise -- so without an
// arm of its own a beating worker fails the rental at "the worker answered a
// prepare with a %q frame", after the card has been allocated.
//
// AND THE PULSE HAS TO MOVE, which is the other half and the reason this is
// one test rather than two: a loop that merely ignored the frame would be
// green on the refusal and would leave a fleet view reporting every rental as
// not answering for the whole of its longest window.
func TestPrepare_ABeatDuringTheModelLoadIsNotAProtocolError(t *testing.T) {
	const caps = `{"model":{"name":"sd15"}}`
	ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		if _, err := c.Recv(ctx); err != nil {
			return err
		}
		// A beat, then the line a long load prints, then another beat, then
		// the answer. Two of them on either side of an ordinary frame,
		// because an arm that handled the first and fell through afterwards
		// is the shape of a `case` written inside the wrong block.
		for _, f := range []control.Frame{
			{Kind: control.KindHeartbeat},
			{Kind: control.KindLog, Text: "still loading\n"},
			{Kind: control.KindHeartbeat},
			{Kind: control.KindPrepared, Elapsed: 90 * time.Second, Caps: json.RawMessage(caps)},
		} {
			if err := c.Send(ctx, f); err != nil {
				return err
			}
		}
		return idleLikeAWorker(ctx, c)
	})

	beat := &pulse{}
	before := time.Now()
	load, raw, err := prepareWorker(t.Context(), io.Discard, ch, beat, time.Minute)
	if err != nil {
		t.Fatalf("a worker that beat while its weights loaded failed the prepare: %v", err)
	}
	if load != 90*time.Second || string(raw) != caps {
		t.Errorf("prepare returned load=%v caps=%s", load, raw)
	}
	if got := beat.last(); got.Before(before) {
		t.Errorf("the pulse reads %v and the prepare started at %v: a beat during the model "+
			"load was dropped, so a fleet view consulted during the longest window of a rental "+
			"would report the node as not answering", got, before)
	}
}

// TestGenerate_ABeatInABatchIsNotAJobEventAndNotAnError.
//
// TWO FAILURES IN ONE FRAME, which is why both are asserted here. The batch
// loop refuses an unknown kind -- "the worker sent a %q frame in the middle of
// a batch" -- so a beat with no arm ends the request; and the loop counts
// frames towards `ended`, so a beat counted as one would end a batch of two
// after one job, with the second job's image written on a node ember has
// stopped reading.
//
// AND NOTHING IS EMITTED FOR IT. A beat is not something that happened to a
// request: it is the instant ember last heard the machine. Emitting it would
// write a row per five seconds per rental into the event stream, which is the
// truth about what a run DID -- and would appear in the operator's own
// history of a generation as a line about a frame they did not ask for.
func TestGenerate_ABeatInABatchIsNotAJobEventAndNotAnError(t *testing.T) {
	ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		if _, err := c.Recv(ctx); err != nil {
			return err
		}
		for _, f := range []control.Frame{
			{Kind: control.KindHeartbeat},
			{Kind: control.KindStarted, Index: 1, Total: 1},
			{Kind: control.KindHeartbeat},
			{Kind: control.KindEnded, Index: 1, Total: 1, Status: sdserver.StatusCompleted.String()},
		} {
			if err := c.Send(ctx, f); err != nil {
				return err
			}
		}
		return idleLikeAWorker(ctx, c)
	})

	var got []report.Event
	sink := report.SinkFunc(func(e report.Event) { got = append(got, e) })
	s := served{ch: ch, runner: &nodeShell{t: t}, beat: &pulse{}}

	before := time.Now()
	if err := s.Generate(t.Context(), sink, testRun(sdserver.Job{Prompt: "a paper boat"}),
		filepath.Join(t.TempDir(), "r"), nil); err != nil {
		t.Fatalf("a worker that beat mid-batch failed the request: %v", err)
	}
	if b := s.Beat(); b.Before(before) {
		t.Errorf("the pulse reads %v and the batch started at %v: the beats were dropped", b, before)
	}
	for _, e := range got {
		if strings.Contains(strings.ToLower(string(e.Kind)), "beat") ||
			strings.Contains(strings.ToLower(string(e.Kind)), "heart") {
			t.Errorf("a heartbeat reached the request's event stream as %q. The stream is the "+
				"truth about what the run did; a beat is a fact about a channel, and one row per "+
				"five seconds per rental in it is noise in the one place an operator reads to "+
				"find out what happened.", e.Kind)
		}
	}
	// The control: the batch really did run through this loop, so the
	// assertions above are about frames that were read rather than about a
	// loop that returned early.
	var ended bool
	for _, e := range got {
		if e.Kind == report.KindUnitCost && e.Unit == report.UnitJob {
			ended = true
		}
	}
	if !ended {
		t.Fatalf("the batch emitted no per-job cost, so this test did not drive the loop it is "+
			"about: %v", got)
	}
}

// TestServed_BeatIsZeroUntilAWorkerHasSaidSomething.
//
// "NOTHING HAS ARRIVED YET" AND "A MOMENT AGO" ARE DIFFERENT ANSWERS and the
// second one is the reassuring one, so the first must not be spelled as a
// fresh timestamp. A pulse initialised to `time.Now()` would make every
// rental read as live from the instant its channel opened -- including one
// whose worker never sent a frame, which is precisely the node this exists to
// find.
func TestServed_BeatIsZeroUntilAWorkerHasSaidSomething(t *testing.T) {
	var s Served = served{beat: &pulse{}}
	if b := s.Beat(); !b.IsZero() {
		t.Errorf("a channel nobody has spoken on reports a beat of %v, want the zero time", b)
	}
}

// TestDialWorker_TheHandshakeIsTheFirstBeat.
//
// THE DEFECT THE BROWSER FOUND, stated where `go test` can hold it. Without
// the seed, a rental read as NOT ANSWERING for the first five seconds of its
// channel -- the gap before the worker's first tick -- and the fleet lab
// caught a row saying "not answering, and still billing" about a node that was
// eight seconds old with its weights still loading. Every unit test around it
// was green, because every one of them was about a beat that had already
// arrived.
//
// IT IS SOUND BECAUSE control.Open READS BEFORE IT RETURNS. The handshake is
// the worker's hello frame and its version, so holding a *Conn at all means
// the worker spoke -- which makes "ember has never heard this worker" a state
// an open channel cannot be in, rather than one classified conservatively.
func TestDialWorker_TheHandshakeIsTheFirstBeat(t *testing.T) {
	dial := controltest.Node(t, func(ctx context.Context, c *control.Conn) error {
		// A worker that says hello -- which control.Serve does -- and then
		// nothing at all. That is the first five seconds of every rental,
		// and it must not read as a worker that has stopped answering.
		<-ctx.Done()
		return nil
	})

	before := time.Now()
	ch, beat, err := dialWorker(t.Context(), dial)
	if err != nil {
		t.Fatalf("dialling the worker: %v", err)
	}
	t.Cleanup(ch.Abort)

	got := beat.last()
	if got.IsZero() {
		t.Fatalf("a channel whose handshake has just completed reports no beat at all, so a " +
			"fleet view would paint every new rental as not answering for the first " +
			"HeartbeatEvery of its life")
	}
	if got.Before(before) {
		t.Errorf("the first beat reads %v and the dial started at %v", got, before)
	}
}
