package control

import (
	"encoding/json"
	"fmt"
	"time"
)

// Kind is what a frame is.
type Kind string

const (
	// KindHello is the worker's first frame, carrying its protocol version.
	// Nothing else may precede it: ember refuses a version it does not know
	// before it has pushed any work, which is the only moment the refusal is
	// cheap.
	KindHello Kind = "hello"

	// KindPrepare asks the worker to wait for the generation server on its own
	// loopback and report what that server can do.
	//
	// EMBER STILL STARTS THAT SERVER ITSELF, over `ssh exec`, with argv ember
	// built from the operator's config -- and that is why this frame exists
	// rather than a `serve` frame carrying the argv. The worker speaks a closed
	// set of typed jobs and never an argv, which is what makes "nothing an
	// operator writes in a config reaches a node as a command" survive the
	// arrival of a daemon on the node.
	KindPrepare Kind = "prepare"

	// KindPrepared answers KindPrepare: the server is up, here is how long its
	// weights took and here is its capabilities document, relayed verbatim.
	KindPrepared Kind = "prepared"

	// KindJob pushes one job onto the worker's in-memory queue.
	KindJob Kind = "job"

	// KindCancel is the operator asking for the batch in flight to stop.
	//
	// IT TRAVELS DOWN THE CHANNEL RATHER THAN BEING A SECOND CONNECTION,
	// which is the whole reason a cancel can be prompt: the worker is already
	// blocked on a read, so it learns in one round trip instead of at the next
	// poll.
	KindCancel Kind = "cancel"

	// KindDrained tells the worker that no more jobs are coming.
	//
	// WITHOUT IT, "the worker exits when its in-memory queue empties" would
	// end the rental between two requests of one drain loop and throw away the
	// resident weights -- 80-96 s per image to load them again, which is the
	// cost the whole resident-server shape exists to pay once. The queue being
	// empty is not the same fact as there being no more work, and only ember
	// holds the second one.
	KindDrained Kind = "drained"

	// KindStarted is the worker having put one job on the card.
	//
	// IT IS A FACT ONLY THE NODE HAS, now that ember pushes a batch and stops
	// driving it: ember knows when it sent a job and the worker knows when it
	// submitted one, and in a batch of eight those differ by however long the
	// seven before it took. The per-job price is timed from this frame, so a
	// job's cost is card time rather than card time plus its wait in a queue
	// -- which would bill the same seconds to two jobs.
	KindStarted Kind = "started"

	// KindLog is a line for the operator's terminal.
	//
	// The weights take minutes to become resident and that wait used to stream
	// to the terminal from ember's own poll loop. The poll moved to the node,
	// so the line has to come back or the operator watches nothing happen for
	// four minutes.
	KindLog Kind = "log"

	// KindProgress is one preview frame: where the sampler is, and the picture
	// so far.
	//
	// THE BYTES TRAVEL ON THE CHANNEL AND NOT TO THE STORE, because the point
	// of a preview is latency and a preview written on the node is a preview
	// nobody sees until the batch ends. They are base64 inside the frame,
	// which is exactly the encoding sd-server already uses to hand ember the
	// same bytes, so nothing on the wire got bigger.
	KindProgress Kind = "progress"

	// KindCancelled is the worker having acted on a cancel, and what the
	// generation server will do about it.
	//
	// IT IS NOT THE JOB'S CONCLUSION and KindEnded still follows when a job
	// was in flight, because the server's own answer decides the outcome: it
	// may come back `cancelled` with nothing, or `completed` because the last
	// step finished while the request was in flight, and an image that exists
	// was paid for either way.
	KindCancelled Kind = "cancelled"

	// KindLanded names what the worker wrote, so ember knows what to fetch.
	KindLanded Kind = "landed"

	// KindEnded is one job's conclusion, whatever it was.
	KindEnded Kind = "ended"

	// KindFailed is the worker reporting that it cannot continue.
	KindFailed Kind = "failed"

	// KindDone is the worker's last frame: the queue is empty, nothing more is
	// coming, and this process is exiting.
	KindDone Kind = "done"
)

// Frame is one message on the channel.
//
// ONE FLAT STRUCT WITH A KIND, rather than a union or a type per message, for
// report.Event's reason: there is one decode in the program, a reader can see
// every field a channel can carry in one place, and adding a field costs
// nobody a new case. Which fields a kind uses is in the comments above, and
// the two ends are the same package on both sides of the wire, so there is no
// second language to keep in step.
//
// EVERY FIELD IS OPTIONAL, AND A ZERO IS NOT A SENTINEL ANYWHERE. The node
// image and the ember driving it ship separately, so a field can always arrive
// missing -- see Version. Index is one-based for exactly that reason: with
// `omitempty` a zero index and an absent one are the same bytes, so a
// zero-based index would make "job 0" and "no job named" indistinguishable.
// report.Event numbers from one for the same reason and the two have to agree,
// because ember stamps one from the other.
type Frame struct {
	Kind Kind `json:"kind"`

	// Version is the worker's protocol version, on KindHello only.
	Version int `json:"version,omitempty"`

	// Index and Total name the job, one-based. See the type comment.
	Index int `json:"index,omitempty"`
	Total int `json:"total,omitempty"`

	// Stem is what the job's files are to be called before numbering, on
	// KindJob. The worker sanitises it the one way sdserver.WriteImages
	// already does, and names what it wrote on KindLanded rather than letting
	// ember derive it a second time -- a second spelling of that rule is a
	// gallery pointing at filenames that are not on disk.
	Stem string `json:"stem,omitempty"`

	// Job is the generation server's own request body, relayed verbatim.
	//
	// RAW AND NOT A DECODED STRUCT, for one reason with a number in it: a
	// job's seed is an int64, and JSON decoded into `map[string]any` arrives
	// as a float64 -- so a seed above 2^53 would be re-encoded as a different
	// number and the node would generate a different picture than the one
	// ember recorded. json.RawMessage never parses it, so there is no
	// round-trip to get wrong. It also means sdserver.Job.Body stays the one
	// place that decides what a job looks like on a wire, and the worker needs
	// to know what a sampler is in order to relay it: nothing.
	Job json.RawMessage `json:"job,omitempty"`

	// Caps is the generation server's own capabilities document, relayed
	// verbatim, on KindPrepared.
	//
	// Raw for Job's second reason: internal/sdserver already owns the decoder
	// -- including the discovery that `capabilities.model` is an object and
	// not the string its field list implies -- and a second decoding of it on
	// the node would be a second place for that to be wrong.
	Caps json.RawMessage `json:"caps,omitempty"`

	// Timeout is how long the weights have to load, on KindPrepare. Elapsed is
	// what the server reported it spent, on KindPrepared.
	Timeout time.Duration `json:"timeout_ns,omitempty"`
	Elapsed time.Duration `json:"elapsed_ns,omitempty"`

	// Pass, Step and TotalSteps are reported exactly as the generation server
	// gives them, and are never combined into a percentage: they describe the
	// current sampling pass and not the whole job, so a bar built from them
	// runs to the end and starts again, which reads as a hang.
	Pass       int `json:"pass,omitempty"`
	Step       int `json:"step,omitempty"`
	TotalSteps int `json:"total_steps,omitempty"`

	// PNG is a preview frame, on KindProgress. It can be absent while the
	// three numbers above are set, which is what a server with previews on and
	// nothing rendered yet looks like.
	PNG []byte `json:"png,omitempty"`

	// Dir and Files are what landed on the node, on KindLanded. Dir is
	// ResultsDir, sent rather than assumed so that ember fetches the path the
	// worker actually wrote to.
	Dir   string `json:"dir,omitempty"`
	Files []File `json:"files,omitempty"`

	// Status is the generation server's own terminal status for the job, on
	// KindEnded. Cancelled says whether the worker had been asked to stop it,
	// which is the one fact ember cannot work out for itself: a job that came
	// back `cancelled` because ember asked is a different outcome from one
	// that came back `cancelled` on its own, and the difference decides
	// whether the rental dies.
	Status    string `json:"status,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`

	// InFlight says whether a job was running when a cancel took effect, on
	// KindCancelled.
	//
	// IT IS THE WHOLE INFORMATION CONTENT OF THAT FRAME, because the two cases
	// are different events to an operator: "job 3 was stopped" names a job,
	// and "the cancel landed in the gap after job 3 finished" names none --
	// the index is then how many finished rather than which one was killed.
	// ember stamps both numbers from its own bookkeeping, because it pushed
	// the batch and every other event in the request is already numbered from
	// there.
	InFlight bool `json:"in_flight,omitempty"`

	// Err is an explanation, on KindEnded and KindFailed. A string and not an
	// error, because the far end of this channel is a different process.
	Err string `json:"error,omitempty"`

	// Text is a line for the operator, on KindLog.
	Text string `json:"text,omitempty"`
}

// File is one file the worker wrote and its length.
//
// The same shape as report.File, and deliberately not that type: report is
// ember's own reporting vocabulary and this is a wire format between two
// binaries that ship separately. Sharing it would mean a field renamed for a
// terminal's benefit silently changing what a node says it produced.
type File struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// checkHello refuses a first frame that is not a hello this build understands.
//
// A FUNCTION SO THAT IT HAS AN EXIT CODE. The version check is the one thing
// on this channel that cannot be exercised against a real node without
// building an old node image, so it is written where a unit test can hand it
// every wrong answer: the wrong kind, a version from the future, a version of
// zero, which is what an `omitempty` field a sender never set decodes to.
func checkHello(f Frame) error {
	if f.Kind != KindHello {
		return fmt.Errorf("the worker's first frame was %q and not %q: this is not an ember control channel, or something else is listening on port %d of the node",
			f.Kind, KindHello, Port)
	}
	if f.Version != Version {
		return fmt.Errorf("the worker on this node speaks control protocol version %d and this ember speaks %d: rebuild and republish the node image from this tree so the two agree",
			f.Version, Version)
	}
	return nil
}
