package report

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Text renders events as the lines ember prints.
//
// This function is the only place in the program where a progress line is
// spelled. `ember up`'s output is a contract -- test/integration greps it, and
// TestRunSteps_ASingleStepLooksExactlyLikeItAlwaysDid exists because somebody
// is parsing it -- so the strings below are not free to be improved. Changing
// one is changing an interface.
func Text(w io.Writer) Sink {
	t := &textSink{w: w}
	return t
}

type textSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (t *textSink) Emit(e Event) {
	s := render(e)
	if s == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// The error is discarded deliberately, for the reason cli.outf gives: a
	// failed write to stdout means the pipe is gone -- `ember up | head` is
	// the ordinary way that happens -- and turning a closed pager into a
	// non-zero exit would make it mean "there are findings".
	_, _ = io.WriteString(t.w, s)
}

// render is the whole of ember's progress vocabulary.
//
// The switch is exhaustive over Kinds() and TestTextRendersEveryKind fails if
// it stops being: a kind added without a decision here would render as nothing
// and the line would simply stop appearing, which is the failure mode a
// default case hides.
func render(e Event) string {
	switch e.Kind {
	case KindLog:
		// Verbatim, terminator included. No prefix, no level, no timestamp:
		// these bytes came off a machine ember rented, and
		// TestStartServer_StreamsTheOutputAndReaps asserts they arrive
		// unchanged.
		return e.Text

	case KindServerStarting:
		return fmt.Sprintf("starting %s\n", e.Cmd)

	case KindModelResident:
		// SILENT FOR A SINGLE JOB, AND THAT GATE BELONGS HERE. "paid once
		// rather than 1 times" is not a saving and was not printed, so the
		// emitter used to decide not to send the event at all -- which meant
		// a batch of one recorded no load span and no model name either, in
		// the columns #30 and #32 added to explain exactly this. The event
		// is the fact and the line is derived from it, so a presentation
		// decision moves to the renderer and the fact is always recorded.
		// `ember up`'s output is unchanged, which TestText_* asserts.
		if e.Total <= 1 {
			return ""
		}
		// Rounded here rather than by the emitter, so the rounding is part of
		// the one spelling of this line. Measured by ember and not read from
		// sd-server's load_elapsed_seconds; Event.Measured says why.
		return fmt.Sprintf("the weights are resident; %v of loading is paid once rather than %d times\n",
			e.Measured.Round(100*time.Millisecond), e.Total)

	case KindUnitStarted:
		return fmt.Sprintf("%s: %s\n", Label(e.Unit, e.Index, e.Total), e.Desc)

	case KindJobProgress:
		return fmt.Sprintf("%s: pass %d, step %d/%d\n",
			Label(e.Unit, e.Index, e.Total), e.Pass, e.Step, e.TotalSteps)

	case KindJobImages:
		return fmt.Sprintf("%s: %d file(s), %s\n",
			Label(e.Unit, e.Index, e.Total), e.Files, HumanBytes(e.Bytes))

	case KindUnitCost:
		return fmt.Sprintf("%s took %v of card time, %v so far\n",
			Label(e.Unit, e.Index, e.Total), e.Cost, e.Spent)

	case KindJobCancelled:
		// Two sentences in one line, and the second half is the one that
		// matters: how many jobs stopped being paid for. "cancelled" alone
		// over a batch would leave an operator wondering whether the other
		// nine are still coming, which is the question they pressed the
		// button to answer.
		dropped := e.Total - e.Index
		if e.Unit == "" {
			// Nothing was in flight, so there is no job to name -- see the
			// Kind. Label would print "job 2/5" over a job that had already
			// finished and been paid for.
			return fmt.Sprintf("cancelled: %d of %d job(s) were dropped before they started\n",
				dropped, e.Total)
		}
		if e.Err == "" {
			return fmt.Sprintf("%s: asked the server to stop it; %d job(s) after it were dropped\n",
				Label(e.Unit, e.Index, e.Total), dropped)
		}
		// The in-flight job keeps going and keeps billing. Said as what it
		// costs rather than as a capability the server lacks, because the
		// operator pressed cancel to stop spending and the honest answer is
		// how much of the spending did not stop.
		return fmt.Sprintf("%s: cannot be stopped, so it will finish and be paid for (%s); "+
			"%d job(s) after it were dropped\n",
			Label(e.Unit, e.Index, e.Total), e.Err, dropped)

	case KindPreviewFailed:
		return fmt.Sprintf("%s: could not write the preview frame: %s\n",
			Label(e.Unit, e.Index, e.Total), e.Err)

	case KindNodeMemory:
		// PRINTED, unlike the other lifecycle kinds, because the thing it
		// answers is a question an operator asks while the run is happening:
		// can this node hold the weights, or will every step re-read them off
		// the store. The slog handler does not report it -- nothing did
		// before this kind existed -- so there is no second spelling to drift
		// from.
		//
		// Bytes rather than a verdict. What to DO about a node that cannot
		// hold the model is a decision to make once there are measurements,
		// and a line that said "this will be slow" would be that decision
		// taken from a guess.
		return fmt.Sprintf("node memory: %s limit %s, using %s; host reports %s total, %s cached; weights are %s\n",
			cgroupName(e.MemCgroup), limitName(e.MemLimit), HumanBytes(e.MemCurrent),
			HumanBytes(e.MemTotal), HumanBytes(e.MemCached), HumanBytes(e.Bytes))

	case KindNodeReady, KindRunCost, KindDestroyFailed:
		// Deliberately silent. The CLI already reports all three through its
		// slog handler, and test/integration/lifecycle_test.go greps for the
		// slog spelling of "node ready" -- so printing them here as well
		// would both duplicate `ember up`'s output and leave two spellings of
		// one fact. They exist for a UI and a database, which have no slog
		// handler to read.
		return ""
	}
	return ""
}

// cgroupName names the hierarchy a memory limit came from.
//
// Here and not on the event, because this is the one spelling of a line and
// the event carries the number rather than the sentence -- the rule that
// keeps "every line lives in exactly one place" true.
func cgroupName(version int) string {
	if version <= 0 {
		return "unreadable"
	}
	return fmt.Sprintf("cgroup v%d", version)
}

// limitName is a memory limit, where zero means there is not one.
//
// "none" and not "0 B", because a limit of zero bytes would be a container
// that cannot allocate anything -- the opposite of what an unset limit means,
// and the direction that would make an unbounded node look like the problem.
func limitName(n int64) string {
	if n <= 0 {
		return "none"
	}
	return HumanBytes(n)
}
