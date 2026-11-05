// Package report turns a run's progress into data, and back into the lines
// ember has always printed.
//
// WHY IT EXISTS. Every progress line in ember was a fmt.Fprintf to an
// io.Writer: "job 1/2: pass 1, step 14/28" went to the operator's terminal and
// nowhere else. A web UI cannot read prose, and a database cannot store it, so
// the same information has to exist as a value before it exists as a sentence.
//
// THE RULE THAT KEEPS IT HONEST. The event is the source of truth and the text
// is derived from it: every line lives in exactly one place, Text's renderer.
// The alternative -- emitting an event *and* printing a line at each call site
// -- is two sources of truth for one fact, and the one that drifts is the one
// nobody is reading. That is the same mistake "one name for a path, not one
// per call site" is written about in AGENTS.md, where a journal subdirectory
// spelled twice meant a lab asserted against a directory nothing wrote to.
//
// WHAT IS NOT TYPED, AND WHY. A node's own output -- a workload's stdout, an
// sd-server's complaint, an asset fetch running on the node -- is a byte
// stream from a machine ember rented. It is not ember's prose and there is
// nothing to type about it, so it travels as KindLog through Writer and comes
// back out of Text byte for byte. The terminator travels with it for exactly
// that reason; see Writer.
//
// PAYLOADS STAY PRIMITIVE. Ints, strings, durations and node.MicroUSD, never a
// sdserver.Job or a node.Spec. internal/sdserver would have to be imported
// here to carry one, and making sdserver emit its own progress would then make
// sdserver import this package -- a cycle that cannot form while the payload
// is primitive.
package report

import (
	"fmt"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// Kind is what happened. The set is closed: Text's renderer switches over all
// of it and TestTextRendersEveryKind fails the build if a kind is added
// without deciding what it looks like.
type Kind string

const (
	// KindLog is a byte stream from the node, or any text ember has not
	// typed. Text is verbatim, terminator included.
	KindLog Kind = "log"

	// KindServerStarting is the resident sd-server being started.
	KindServerStarting Kind = "server_starting"

	// KindModelResident is the weights being loaded once rather than once per
	// job, which is the whole reason the served shape exists.
	KindModelResident Kind = "model_resident"

	// KindUnitStarted is one job or one step beginning.
	KindUnitStarted Kind = "unit_started"

	// KindJobProgress is a sampling pass and a step within it.
	//
	// Never a percentage. sd-server's step and total_steps describe the
	// current pass and not the whole job, and passes restart for each batch
	// item, each high- and low-noise stage and each highres pass -- so a bar
	// built from them runs to the end and then starts again, which reads as a
	// hang.
	KindJobProgress Kind = "job_progress"

	// KindJobImages is what one job wrote.
	KindJobImages Kind = "job_images"

	// KindUnitCost is what one job or step cost of card time, and the running
	// total.
	KindUnitCost Kind = "unit_cost"

	// KindJobCancelled is the operator's stop reaching one job.
	//
	// EMITTED WHETHER OR NOT THE IN-FLIGHT JOB COULD BE STOPPED, and Err is
	// what tells them apart: empty means the server was asked, set means it
	// was not asked or would not answer. Two kinds would have let a reader
	// handle one and silently drop the other, and the one that would get
	// dropped is the one that says a card is still sampling something the
	// operator has already paid to stop wanting.
	//
	// Total is the batch size and Index is the job the cancel reached, so
	// Total-Index is how many were dropped -- a reader can say the thing the
	// operator pressed the button to find out without a fourth number.
	//
	// Unit is UnitJob when a job was in flight and EMPTY when none was: a
	// cancel can land in the gap between one job finishing and the next
	// starting, and there "job 2/5" would name a job that had already
	// completed. Index is then the count that finished, which keeps the
	// arithmetic above true in both cases.
	KindJobCancelled Kind = "job_cancelled"

	// KindPreviewFailed is a preview frame that could not be written. Best
	// effort: a preview is not a reason to abandon a generation that is
	// already paid for.
	KindPreviewFailed Kind = "preview_failed"

	// The lifecycle kinds below are emitted for a UI and a database, and Text
	// renders nothing for three of the four, because the CLI already reports
	// those through its slog handler -- "node ready" at provision.go is what
	// test/integration/lifecycle_test.go greps for. Printing them twice would
	// change `ember up`'s output, and a lab asserts it has not. The exception
	// is node_memory, which no slog line ever carried and which Text
	// therefore prints; the count was "four" while there were six of these,
	// two of them emitted by nothing, which is the deletion recorded below.

	// KindNodeReady is a node that is usable.
	KindNodeReady Kind = "node_ready"

	// KindNodeMemory is what the node says about its memory, both views, on
	// every rental.
	//
	// IT EXISTS FOR 81 SECONDS AND $0.056 PER IMAGE NOBODY COULD EXPLAIN.
	// Two runs of the same configuration against the same card in the same
	// datacentre differed by that much, depending on whether the node kept
	// 56 GiB of weights in page cache -- and all eight nodes reported
	// 620-708 GiB of free RAM, because `free` inside a container reports the
	// HOST's memory and not the limit the container is bounded by. The
	// figures are recorded rather than printed, because a number printed once
	// into a terminal is not evidence.
	KindNodeMemory Kind = "node_memory"

	// KindRunCost is what the whole rental came to, reported after the node
	// is destroyed.
	KindRunCost Kind = "run_cost"

	// KindDestroyFailed is the one event an operator must not miss: the node
	// may still be running, and running means billing.
	KindDestroyFailed Kind = "destroy_failed"

	// THERE WAS A KindRunFinished AND A KindRunFailed HERE, declared, listed
	// in Kinds(), rendered as deliberately silent by Text, and named in the
	// queue's `urgent` list beside run_cost -- the list reserved for events
	// that would be LOST rather than late. Nothing ever emitted either. The
	// one consumer anywhere was `web/src/model.rs`'s
	// `"destroy_failed" | "run_failed"` arm, which was therefore half
	// unreachable, and the comment above said they "end a run" when nothing
	// in the program ended one by saying so.
	//
	// DELETED RATHER THAN WIRED UP, which is the opposite of the call made
	// for sdserver.Client.Cancel and the same one made for queue.Cost. The
	// test is whether a second live mechanism exists for the fact. For Cancel
	// there was none, so deleting it would have deleted the mechanism; here
	// the fact is a request's status and its reason, which are COLUMNS --
	// `requests.status` and `requests.err`, written by queue.fail for a
	// request refused before anything was rented and by queue.finish for one
	// that broke on a card. Both already reach every JSON route and the
	// socket as `requestView.Status` and `requestView.Error`. An event for
	// the same fact is the "two carriers of one figure" mistake, and it is
	// the worse carrier of the two: current state survives a reconnect with
	// no backlog to replay, which is exactly the case that made this visible.
	//
	// AND EMITTING run_failed WOULD HAVE PRINTED A FALSE ALARM. Its only
	// renderer shares `Progress::failure` with destroy_failed, whose view
	// says "THE NODE MAY STILL BE RUNNING AND BILLING -- check ember ls". A
	// request a money ceiling refused never had a node, so that sentence over
	// it is the caption that is true of one kind and wrong for the other --
	// the `proj` preview caption, on the page an operator reads to decide
	// whether to go and check their bill. Splitting the arm would have been
	// possible and would still have been a second renderer for a reason
	// `Request::error` already carries.
	//
	// What is NOT recorded by a column is "the stream is over", which is the
	// only thing KindRunFinished could have meant. A terminal status is that
	// answer and `Request::is_terminal` already reads it.
)

// Kinds is every kind, so a renderer can be checked for completeness rather
// than trusted to be complete.
func Kinds() []Kind {
	return []Kind{
		KindLog, KindServerStarting, KindModelResident,
		KindUnitStarted, KindJobProgress, KindJobImages, KindUnitCost,
		KindJobCancelled, KindPreviewFailed,
		KindNodeReady, KindNodeMemory, KindRunCost, KindDestroyFailed,
	}
}

// UnitJob is what a rental does one of at a time.
//
// A named constant and not a literal, because `Label` renders it into the one
// sentence every unit line shares -- "job 1/2 took $0.03 of card time" -- and
// a second spelling of that is one sentence maintained twice.
const UnitJob = "job"

// Event is one thing that happened during a run.
//
// Flat, with omitempty, rather than a payload interface per kind. Over the
// wire a flat struct is one serde type on the other side instead of thirteen,
// and in Go the alternative is a type switch at every reader. The cost is
// fields that are meaningless for most kinds, which the Kind documents and
// the renderer enforces.
type Event struct {
	// Seq and At are stamped by Stream, not by the emitter, so every sink in
	// a fan-out sees the same numbering. An event with Seq 0 reached a sink
	// without passing through a Stream.
	Seq int64     `json:"seq"`
	At  time.Time `json:"at"`

	Kind Kind `json:"kind"`

	// Text is the verbatim bytes of a KindLog event, terminator included.
	Text string `json:"text,omitempty"`

	// Cmd is a command as it will be sent, quoting included.
	Cmd string `json:"cmd,omitempty"`

	// Unit is UnitJob when a unit is in flight; Index is 1-based and Total is
	// how many there are.
	Unit  string `json:"unit,omitempty"`
	Index int    `json:"index,omitempty"`
	Total int    `json:"total,omitempty"`

	// Desc is one line identifying what this unit is -- a truncated prompt
	// and its geometry, or a command.
	Desc string `json:"desc,omitempty"`

	Pass       int `json:"pass,omitempty"`
	Step       int `json:"step,omitempty"`
	TotalSteps int `json:"total_steps,omitempty"`

	Files int   `json:"files,omitempty"`
	Bytes int64 `json:"bytes,omitempty"`

	// Wrote names the files a job produced, so a reader recording where the
	// bytes went does not re-derive the naming. One spelling of a filename,
	// for the reason Label exists: two would drift, and a database pointing
	// at names that are not the ones on disk is a gallery of broken images.
	Wrote []File `json:"wrote,omitempty"`

	// Cost and Spent are integer microdollars, never a float. $0.1+$0.2 is
	// not $0.3 in float64, and a price that is sometimes wrong by a rounding
	// error is a price nobody trusts.
	Cost  node.MicroUSD `json:"cost_micro_usd,omitempty"`
	Spent node.MicroUSD `json:"spent_micro_usd,omitempty"`

	// Model is what the generation server said it had loaded, as its own
	// /capabilities response names it.
	//
	// ON THE EVENT BECAUSE THE SINK IS ALL SOME READERS HAVE. The queue holds
	// an app.Served and reads the model off it directly; `ember up` holds a
	// report.Sink and nothing else, so without this the one command every
	// measurement in README was taken with would record a dash in the column
	// `ember spend --by model` groups on -- and the ledger's footnote, which
	// says a dash means the run never reached a node, would be wrong about
	// exactly those runs. A caption that is true of one mode and wrong for
	// another reads as working, which is the defect this field exists to
	// avoid rather than one it fixes.
	//
	// It is a RESPONSE and never a reading of `sd.serve`: an operator can
	// edit that file between two rentals, so the config answers "what would
	// run now" where the question is "what ran then".
	Model string `json:"model,omitempty"`

	// Measured is a span ember timed itself; Reported is what the thing being
	// measured claimed.
	//
	// Both, because they disagree and the disagreement matters. sd-server
	// reported 551ms of load for weights that are 56 GiB, so ember printed
	// "551ms of loading is paid once" while the other arm of the same
	// measurement spent 80-96s per image re-reading those weights. Carrying
	// one number would mean picking which lie to tell.
	Measured time.Duration `json:"measured_ns,omitempty"`
	Reported time.Duration `json:"reported_ns,omitempty"`

	// Node identifies the node a lifecycle event is about, and Endpoint is
	// where ember reaches it.
	Node     string `json:"node,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`

	// Deadline is when the node destroys itself, and Rate is what it costs
	// per hour. Both belong to a node becoming ready: they are what an
	// operator needs to know how long they have and what it is costing while
	// they have it.
	//
	// omitzero AND NOT omitempty, and the difference is the whole comment.
	// omitempty has no effect on a struct -- encoding/json has no empty value
	// for one -- so this field was on EVERY event ember emits, as
	// "deadline":"0001-01-01T00:00:00Z": on the websocket, in
	// GET /api/requests/{id}/events, and in every row of the events table,
	// the one that grows per step per job per rental. A FLUX.2 batch of eight
	// at 28 steps is 224 progress rows each asserting a deadline of year one,
	// in the stream this repository treats as the source of truth and the
	// columns as a derived index -- so a reader going there to settle
	// something saw a field named `deadline` on a job_progress and had to
	// already know it meant nothing. A value that looks like a fact.
	//
	// Rate, one line down, has the same tag, is an int64 underneath, and
	// works: two tags on consecutive lines behaving differently is why
	// nothing noticed. At sixteen lines up carries no omitempty deliberately,
	// because At is always meaningful -- so the author knew the distinction
	// and this was a slip rather than a decision.
	//
	// WHY NOT A POINTER OR A MarshalJSON. *time.Time is this repository's
	// answer where zero is a legitimate value -- ClipSkip, Eta, FlowShift --
	// and it is the wrong one here: the zero time is not a deadline anybody
	// can mean, so a pointer would buy nilness ember has no use for and spend
	// it at every reader of a struct carried on the hot path. A MarshalJSON
	// on Event is worse: it is a hand-maintained list of thirty-odd fields,
	// which is the "a comment that justifies the entry somebody remembered
	// cannot catch the entry somebody forgets" failure with a marshaller in
	// place of the comment. omitzero calls time.Time's own IsZero, needs no
	// reader to change, and cannot silently stop working the way omitempty
	// silently never started: for a type without IsZero it falls back to
	// comparing the zero value, which is still the right answer.
	Deadline time.Time `json:"deadline,omitzero"`
	Rate     node.Rate `json:"rate_micro_usd_per_hour,omitempty"`

	// Billable is the time a rental was charged for, which is not Elapsed: a
	// node destroyed after four seconds on a provider with a one-minute
	// minimum costs a minute, and reporting the four seconds would teach an
	// operator that short runs are nearly free.
	Elapsed  time.Duration `json:"elapsed_ns,omitempty"`
	Billable time.Duration `json:"billable_ns,omitempty"`

	// MemLimit and MemCurrent are the node's own cgroup memory limit and
	// usage; MemTotal and MemCached are the host's, as /proc/meminfo reports
	// them inside a container.
	//
	// ALL FOUR, BECAUSE THE WHOLE POINT IS THAT THEY DISAGREE and only one of
	// them bounds the page cache. Recording the one ember currently believes
	// would be the load_elapsed_seconds mistake a second time: a single
	// figure, believed, that was wrong by two orders of magnitude. A zero
	// means "not reported", which is why node.Memory computes the derived
	// answers on the way out rather than anything storing them -- and why
	// there is one of it: the same subtraction existed on both sides of the
	// store, and the copy on the live-node side was never called.
	//
	// MemLimit zero means there is no cgroup limit at all, not a limit of
	// nothing: cgroup v2 writes the literal "max" and v1 a sentinel near the
	// top of int64, and both come through as zero.
	MemLimit   int64 `json:"mem_limit_bytes,omitempty"`
	MemCurrent int64 `json:"mem_current_bytes,omitempty"`
	MemTotal   int64 `json:"mem_total_bytes,omitempty"`
	MemCached  int64 `json:"mem_cached_bytes,omitempty"`

	// MemCgroup is which hierarchy MemLimit came from: 2, 1, or 0 when
	// neither could be read. Kept because the numbers are diagnostic and the
	// first question about a surprising one is where it came from.
	MemCgroup int `json:"mem_cgroup,omitempty"`

	// Err is a failure, as a string, because an event is data that crosses a
	// wire and gets stored.
	Err string `json:"error,omitempty"`
}

// File is one file an event reports, and its length.
type File struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// Sink receives events.
//
// One method, so a database writer, a websocket fan-out and a terminal
// renderer are the same thing to an emitter. Emit must not block for long: it
// is called from the goroutine driving a rented GPU.
type Sink interface {
	Emit(Event)
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(Event)

// Emit calls f.
func (f SinkFunc) Emit(e Event) { f(e) }

// Discard throws events away. Useful where a caller has nothing to report to.
var Discard Sink = SinkFunc(func(Event) {})

// Stream stamps events and fans them out.
//
// Stamping happens here rather than in each sink because two sinks given the
// same event must agree about its sequence number: the websocket tells a
// browser "you have seen up to 41" and the browser asks the database for
// everything after 41. Two numberings make that question unanswerable.
type Stream struct {
	mu   sync.Mutex
	seq  int64
	now  func() time.Time
	outs []Sink
}

// NewStream returns a Stream feeding outs.
//
// now is injected because a recorded timestamp is compared in tests; nothing
// here waits on a clock, so there is no clock interface, as in
// internal/provision.
func NewStream(now func() time.Time, outs ...Sink) *Stream {
	return &Stream{now: now, outs: outs}
}

// Emit numbers e, timestamps it and passes it to every sink.
//
// Sequence numbers start at 1, so the zero value of Event.Seq means "this
// never went through a Stream" rather than "this was first".
func (s *Stream) Emit(e Event) {
	s.mu.Lock()
	s.seq++
	e.Seq = s.seq
	if e.At.IsZero() {
		e.At = s.at()
	}
	outs := s.outs
	s.mu.Unlock()

	for _, o := range outs {
		o.Emit(e)
	}
}

// Seq reports the last sequence number issued.
func (s *Stream) Seq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *Stream) at() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Multi fans one event out to several sinks.
//
// Separate from Stream because not every fan-out should renumber: a Stream
// inside a Stream would stamp an event twice and the second number would win.
func Multi(outs ...Sink) Sink {
	return SinkFunc(func(e Event) {
		for _, o := range outs {
			o.Emit(e)
		}
	})
}

// Label is how a unit is named in a line of output and in an error.
//
// One function rather than a format string here and another in the error
// wrapping beside it: "job 1/2" appears in `job 1/2: pass 1, step 3/4` and in
// `job 1/2: sd-server refused it`, and two spellings is how the UI starts
// grouping a job's progress under a name its failure does not use.
func Label(unit string, index, total int) string {
	return fmt.Sprintf("%s %d/%d", unit, index, total)
}

// HumanBytes is for one line of operator-facing output.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[exp-1])
}
