package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/internal/tts"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/workload"
)

// spokenBatch is text to speech, one piper invocation per utterance.
//
// WHY ONE-SHOT AND NOT A SERVER, in one sentence here and at length in
// internal/tts: piper has no server mode, so the resident shape is reachable
// only over a held-open stdin that ember's remote-command seam does not have.
// The measurement that says what that costs -- 400 ms of fixed overhead per
// utterance against inference of about 200 ms -- is in that package's doc.
//
// IT IS THEREFORE SHAPED LIKE runSteps AND NOT LIKE runServed, and the three
// things it borrows from runSteps are the three that are about a batch rather
// than about stable diffusion: the per-unit cost event so an operator watching
// can stop paying for the rest, the fetch after every unit so a dropped
// connection loses everything *so far* rather than everything, and stopping at
// the first failure because a batch where job 3 failed and 4 through 10 ran
// anyway is a batch nobody can trust.
//
// WHAT IT DOES NOT BORROW IS THE MARKER. runSteps bounds each fetch by a file
// it touches on the node, because a `cmds` step's output is a whole directory
// whose contents ember cannot name in advance. Here ember composed the
// filename, so each fetch names exactly one file and there is nothing for a
// marker to bound -- which also means this path cannot re-send what is already
// home, and does not need the "a marker that cannot be touched is wasteful,
// not fatal" concession.
type spokenBatch struct{ run tts.Run }

func (b spokenBatch) Describe() string {
	return fmt.Sprintf("tts (piper, %d utterance(s), voice %s)", len(b.run.Jobs), b.run.Voice)
}

// RemoteDir is where a spoken artifact is written on the node, before it is
// fetched home.
//
// UNDER /workspace/out AND NOT UNDER /tmp, for two reasons that are both about
// something else being unable to take it away. `node.Mount` refuses a store
// mounted over /workspace -- AGENTS.md: a store there "bills for every render
// for as long as the store exists" -- so this path cannot be shadowed by a
// mount, which /tmp also cannot but only by accident. And both ember images
// create /workspace/out, so the directory's parent is one the node image owns.
//
// A SUBDIRECTORY, so a config that also fetches /workspace/out through
// `outputs:` brings home its own files and not ember's speech as well -- which
// would double every wav in the run directory and make the artifact rows
// disagree with what is on disk.
//
// The script creates it, rather than this resting on the image: an image that
// predates the directory is exactly the case AGENTS.md records for
// /workspace/out itself, where a workload "failed at the tar with Cannot open:
// No such file or directory".
const RemoteDir = "/workspace/out/speech"

func (b spokenBatch) Run(ctx context.Context, out io.Writer, sink report.Sink, _ tunnel.Dialer,
	r sshx.Runner, _ node.Spec, so session.Options, runDir string, rate node.Rate,
) error {
	return runSpoken(ctx, out, sink, r, b.run, so, runDir, rate)
}

// runSpoken says each utterance in turn and brings its wav home before the
// next one starts.
//
// IT IGNORES TWO OF THE PARAMETERS THE SEAM CARRIES, and both omissions are
// decisions rather than oversights.
//
// THE DIALER: no forwards. A `cmds` run holds `ports:` open because a workload
// may want to reach a service on the node and a ComfyUI run is exactly that;
// piper reads stdin and writes a file, so a forward here would be a listener
// opened for nothing -- and the rule that every listener is loopback is a rule
// about listeners that exist.
//
// THE SPEC: nothing on it describes a speech run. `Serve` and `ServeTimeout`
// are sd-server's, `Cmds` is the opaque-argv path, `Ports` is the forward this
// does not open, and `Outputs` is the `outputs:` fetch -- which this path
// deliberately does not use, because ember composed each filename and fetches
// them one at a time. That absence is the same seal `Plan.Spoken` rests on:
// `node.Spec` has no speech field, so a request can never be consulted about
// one, and a parameter read here would be the first step towards giving it
// one. The seam carries it because `servedBatch` needs it, which is what an
// interface with two implementations looks like.
func runSpoken(
	ctx context.Context,
	out io.Writer,
	sink report.Sink,
	r sshx.Runner,
	run tts.Run,
	so session.Options,
	runDir string,
	rate node.Rate,
) error {
	total := len(run.Jobs)
	var errs []error
	var spent node.MicroUSD

	for i, j := range run.Jobs {
		stem := SpokenStem(j, i)
		remote := path.Join(RemoteDir, stem+tts.Ext)

		sink.Emit(report.Event{
			Kind: report.KindUnitStarted, Unit: report.UnitJob,
			Index: i + 1, Total: total, Desc: j.Describe(),
		})

		// Timed from here and not from workload.Run's own duration, for the
		// reason runSteps is: the fetch below is billed too, because the card
		// stays rented while the bytes come over the wire. "Card time" has to
		// mean all of the card's time or the figure is simply wrong, and a
		// money figure must never be wrong in the cheap direction.
		jobStart := time.Now()

		// piper's own account of the utterance is on its stderr, so the stream
		// is tee'd: the operator sees it as it happens, which is what every
		// other workload here does, and ember also reads it for the two
		// figures below. A second invocation to ask the same question would be
		// a second utterance on a rented card.
		var said bytes.Buffer
		runErr := sayOne(ctx, io.MultiWriter(out, &said), r, run, j, remote)

		var res transfer.Result
		var ferr error
		if runErr == nil {
			// On a context that survives Ctrl-C, for the reason every other
			// fetch here is: the node is about to be destroyed either way and
			// whatever it produced is about to stop existing.
			//
			// ONLY WHEN THE UTTERANCE SUCCEEDED, which is the one place this
			// differs from runSteps and is not an oversight. There, a step
			// that failed is often the one whose partial output explains why.
			// Here a piper that exited non-zero wrote no wav at all -- it
			// opens the output file after synthesis -- so the fetch would be a
			// tar of a missing path, which GNU tar answers with a non-zero
			// exit over a valid EMPTY archive: a second error about a file
			// that was never going to exist, stacked on the real one.
			fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outputTimeout)
			res, ferr = transfer.Fetch(fetchCtx, r,
				[]node.OutputPath{{Remote: remote}}, runDir, "")
			cancel()
		}

		// Reported before any error is returned, because "0 files" over a
		// successful synthesis is the thing an operator needs to see.
		if runErr == nil {
			outf(out, "fetched %s, %s\n", stem+tts.Ext, report.HumanBytes(res.Bytes))
		}

		// THE ARTIFACT EVENT IS WHAT JOINS THIS TO EVERYTHING DOWNSTREAM, and
		// it is deliberately the same kind the served path emits. The queue's
		// Recorder turns a KindJobImages into rows in the images table, keyed
		// by the name in Wrote, and the HTTP route serves those rows -- so a
		// wav reaches `ember history`, the gallery and the blob store through
		// mechanisms that already exist rather than through a second set.
		//
		// A kind named for images carrying a wav is the one thing about this
		// that reads wrong, and it is named rather than renamed: the kind is
		// on the wire, in every stored event body and in `web/`'s decoder, so
		// renaming it is a migration and a client change for a word. What
		// `report.Kind` means is "an artifact a job produced", and the
		// extension on the name says which -- which is also how the route
		// decides the content type.
		if ferr == nil && res.Files > 0 {
			e := report.Event{
				Kind: report.KindJobImages, Unit: report.UnitJob,
				Index: i + 1, Total: total,
				Files: res.Files, Bytes: res.Bytes,
				Wrote: []report.File{{Name: stem + tts.Ext, Bytes: res.Bytes}},
			}
			// piper's two figures, when it printed them. Measured rather than
			// derived: Reported is the duration of the audio this job made and
			// Measured is what piper said inference took, which is the closest
			// thing speech has to a step count. The span ember timed is the
			// unit cost below and is a different number -- it includes the
			// process start, the voice load and the fetch.
			if rep, ok := spokenReport(said.String()); ok {
				e.Reported, e.Measured = rep.Audio, rep.Infer
			} else {
				// A line that stopped parsing costs the two figures and
				// nothing else -- the job worked, because the exit status
				// said so. At debug, because an upstream that reworded its
				// own log is not a thing an operator can act on, and a
				// warning on every utterance over a cosmetic change is how a
				// real one gets ignored.
				so.Log.Debug("piper printed no real-time-factor line for this utterance",
					"job", i+1, "of", total)
			}
			sink.Emit(e)
		}

		if runErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", report.Label(report.UnitJob, i+1, total), runErr))
		}
		if ferr != nil {
			errs = append(errs, ferr)
		}

		job := rate.Cost(time.Since(jobStart))
		spent += job
		sink.Emit(report.Event{
			Kind: report.KindUnitCost, Unit: report.UnitJob,
			Index: i + 1, Total: total, Cost: job, Spent: spent,
		})

		// Stop at the first failure, for runSteps' reason: the remaining
		// utterances would bill for work the operator no longer wants.
		if runErr != nil || ferr != nil {
			break
		}
	}
	return errors.Join(errs...)
}

// sayOne runs one piper invocation on the node.
func sayOne(ctx context.Context, out io.Writer, r sshx.Runner, run tts.Run, j tts.Job, remote string) error {
	cmd := run.Command(j, remote)
	outf(out, "speaking: %s\n", j.Describe())
	// workload.Run reports a non-zero exit as an error as well as a code, so
	// the error is the whole answer -- the distinction gpucheck.Wait draws
	// between a transport failure and a refusal is not one this path acts on.
	_, err := workload.Run(ctx, r, cmd, out)
	return err
}

// SpokenStem is the filename this job's artifact gets, without the extension.
//
// NUMBERED EVEN WHEN THE OPERATOR NAMED IT, for the reason sdserver.WriteImages
// numbers a batch_count of one: two jobs may legitimately carry the same
// `output`, and without the index the second would overwrite the first -- one
// wav on disk, two image rows pointing at it, and an operator billed for two
// utterances with one to listen to. A batch of one still gets the index so a
// script reading the directory needs one case rather than two.
func SpokenStem(j tts.Job, i int) string {
	name := strings.TrimSpace(j.Output)
	if name == "" {
		name = "speech"
	}
	return fmt.Sprintf("%s-%02d", name, i+1)
}

// spokenReport finds piper's per-utterance line in what it printed.
//
// LAST MATCH AND NOT FIRST, although one invocation says one utterance and so
// prints one line. The reason is the measurement in internal/tts: within one
// process those figures are RUNNING TOTALS, so the day somebody holds a piper
// open the last line is the only one whose numbers are the batch's -- and a
// first-match reader would then silently report the first utterance's figures
// for every job. Cheaper to be right now than to be found later.
func spokenReport(said string) (tts.Report, bool) {
	var got tts.Report
	var ok bool
	for line := range strings.SplitSeq(said, "\n") {
		if r, found := tts.ParseReport(line); found {
			got, ok = r, true
		}
	}
	return got, ok
}
