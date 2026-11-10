package queue

import (
	"context"
	"fmt"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/tts"
)

// runner is what the drain loop needs of a rental, whichever kind it is.
//
// WHY THE SEAM IS HERE AND NOT WIDER. `app.Served` and `app.Spoken` are
// different shapes on purpose -- one has `Caps()` because sd-server publishes
// what it can do, and the other has nothing to publish -- so the loop cannot
// take either of them directly. What the loop actually needs is one sentence:
// "generate this stored request into this directory, and stop if this channel
// closes". That is one method, and the per-kind conversion from a stored row
// to a driver's own batch lives in the adapter rather than in the loop.
//
// The alternative was a `switch req.Kind` inside `generate`, which is the
// dispatch `app.batch` was introduced to remove, one layer up and with the
// cancel channel threaded through it twice.
type runner interface {
	// Generate runs one request's whole batch.
	Generate(ctx context.Context, sink report.Sink, req genstore.Request, dir string, stop <-chan struct{}) error

	// Model is what to record in `requests.model`, from what the thing
	// serving this rental reported about itself.
	Model() string
}

// sdRunner drives a resident sd-server.
//
// It is the body `Queue.generate` used to hold, moved verbatim: the
// conversion from stored rows to an `sdserver.Run` was already here, and this
// is where the kind that needs it keeps it.
type sdRunner struct{ s app.Served }

// Model is what sd-server said it had loaded, from the /capabilities response
// PutCaps caches.
func (d sdRunner) Model() string { return d.s.Caps().Model }

func (d sdRunner) Generate(ctx context.Context, sink report.Sink, req genstore.Request, dir string, stop <-chan struct{}) error {
	run := sdserver.Run{Preview: req.Preview, PreviewInterval: req.PreviewInterval}
	for _, j := range req.Jobs {
		run.Jobs = append(run.Jobs, j.SD())
	}
	return d.s.Generate(ctx, sink, run, dir, stop)
}

// ttsRunner drives piper, one invocation per utterance.
//
// IT HOLDS THE CATALOGUE, AND THAT IS WHERE THE SEAL IS ENFORCED. A stored job
// carries a `tts.Ask` -- a voice **id** and no path -- so this is the layer
// that turns an id into the two filenames, out of the list the operator
// published. There is no other route: `Ask` has no field a path could arrive
// in, and `Resolve` is the only thing that reads the catalogue.
type ttsRunner struct {
	sp  app.Spoken
	cat tts.Catalogue
}

// Model is EMPTY FOR SPEECH, and that is a refusal rather than a gap.
//
// `requests.model` is documented as what the generation server reported, and
// AGENTS.md's reason for the column is that "the model is recorded from a
// RESPONSE, and that is what makes grouping by it possible" -- a value read
// from the config instead would be "a value read from the wrong place and
// believed", which is the mistake that entry exists to refuse. piper publishes
// nothing: it is a binary with six flags and no /capabilities.
//
// The voice is not lost by this -- it is in each job's stored `tts.Ask`, which
// `ember history` can print and a future `--by voice` could group on. What is
// given up is one GROUPING, and what would be bought by filling it is a column
// that says which voice an operator's config named at the moment the run
// started, which is a different question from the one the column asks.
//
// Both readers already print a dash for an empty model, and the ledger's own
// footnote says it means nobody read one -- which is exactly true here.
func (d ttsRunner) Model() string { return "" }

func (d ttsRunner) Generate(ctx context.Context, sink report.Sink, req genstore.Request, dir string, stop <-chan struct{}) error {
	var run tts.Run
	for i, j := range req.Jobs {
		if j.TTS == nil {
			// genstore.Request.Validate refuses this at enqueue and at Adopt,
			// so reaching it means a row was written past both -- by a hand
			// edit of the database, or by a future writer that forgot. A
			// refusal rather than a skip, because an utterance silently
			// dropped is a request the operator paid for and got nothing
			// from.
			return fmt.Errorf("job %d of request %s is a speech job with no utterance", i+1, req.ID)
		}
		j, err := j.TTS.Resolve(d.cat)
		if err != nil {
			// NAMED WITH THE JOB, because the one realistic way here is a
			// stale page: the UI loaded the voice list, the operator edited
			// `tts.voices`, and the id this row names no longer exists.
			// Lookup's own message lists what IS published.
			return fmt.Errorf("job %d of request %s: %w", i+1, req.ID, err)
		}
		run.Jobs = append(run.Jobs, j)
	}
	return d.sp.Say(ctx, sink, run, dir, stop)
}
