//go:build integration && imagelab

// The served path against a real sd-server, with the progress stream as the
// thing under test.
//
// WHY THIS EXISTS. Previews are the reason ember learned the sd-server API at
// all. Every other workload here is opaque argv on purpose, and that position
// does not survive progress reporting: a program that refuses to read its own
// payload cannot say that job two is fourteen steps into its first pass. So
// the preview stream is the justification for the one place ember knows what
// a sampler is -- and until this file, it was the least-verified thing in the
// feature.
//
// The shape of that gap matters more than its size. `job_generating.json` was
// written from examples/server/api.md, the fake replayed that same guess, and
// the unit tests confirmed ember was self-consistent with it. Nothing had seen
// a preview frame on the wire: no lab touched the served path, and the live
// FLUX.2 measurement ran `preview: none` in both arms. That is the identical
// closed loop `capabilities.model` sat in until a real server answered with an
// object instead of a string -- after a node was already up and billing.
//
// WHY THE CPU IMAGE. sd-server in the CUDA image needs libcuda.so.1 to start,
// so there is no GPU-less runner on which it proves anything. The CPU image is
// built from the same upstream revision -- TestWorkloadImage_SupportsVideoGeneration
// already asserts the two pins agree before asserting anything else -- and the
// release zip has always carried sd-server beside sd-cli. It just was not on
// PATH until workload-sd.Dockerfile put it there. So this runs on a laptop,
// for nothing, against the same program that goes to RunPod.
//
// It is not skipped. A missing image is a hard failure naming the command that
// builds it, because a lab that skips itself reports success for something
// that never ran.
//
//	make test-preview-lab

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/sshx/sshexec"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
	"github.com/Legion112/ember/test/integration/harness"
)

// The model baked into ember:sd by `make workload-sd MODEL=...`, at the path
// that Dockerfile copies it to.
const labModel = "/opt/models/sd15.gguf"

// Small on purpose. The question is whether frames arrive and what shape they
// are in, not whether the picture is any good, and every second of CPU
// sampling here is a second on every run of this lab.
//
// Six steps rather than one or two: a preview stream needs something to
// stream. With preview_interval at 1 a six-step pass is six frames, which is
// enough to see a step advance and enough to see the same frame returned
// twice between intervals.
const (
	labWidth  = 64
	labHeight = 64
	labSteps  = 6
)

// TestPreview_AServerStreamsFramesEmberCanDecode is the whole point of the
// file: a real sd-server, a real generation, and ember's own wire types
// reading what comes back.
//
// Everything asserted here was previously believed on the strength of a
// documentation page.
func TestPreview_AServerStreamsFramesEmberCanDecode(t *testing.T) {
	l := harness.New(t)
	srv := startLabServer(t, l)

	caps, err := srv.client.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	// ember refuses a preview mode the server does not list, so the mode this
	// lab is about has to be one it offers. A server that dropped `proj`
	// would otherwise fail further down with something less specific.
	if !contains(caps.PreviewModes, "proj") {
		t.Fatalf("preview modes = %v, want one of them to be proj", caps.PreviewModes)
	}

	job := sdserver.Job{
		Prompt: "a paper boat",
		Width:  labWidth, Height: labHeight,
		Steps:   labSteps,
		Sampler: "euler",
		Seed:    1234,
		Output:  "boat",
	}
	if err := sdserver.Validate(job, caps); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// interval 1: a frame every step. Upstream's own default, and the
	// expensive one on a billed card -- which is exactly why ember's default
	// is 5 and why this lab does not inherit ember's default either. The lab
	// wants the most frames it can get in the fewest steps.
	id, err := srv.client.Submit(t.Context(), job, "proj", 1)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if id == "" {
		t.Fatal("submit returned an empty job id")
	}

	obs := pollToCompletion(t, srv, id)

	// 1. Frames arrived at all. Everything below is a detail of something
	//    that has to have happened first.
	if len(obs.previews) == 0 {
		t.Fatal("no preview frame was ever reported: the progress stream is " +
			"the reason this package exists, and nothing observed one")
	}

	// 2. total_steps is the job's own step count, not some internal number.
	//    ember prints "step 3/6" from it, so a server counting something else
	//    would make every progress line a lie.
	for i, p := range obs.previews {
		if p.TotalSteps != labSteps {
			t.Errorf("frame %d: total_steps = %d, want %d", i, p.TotalSteps, labSteps)
		}
		if p.Pass < 1 {
			t.Errorf("frame %d: pass = %d, want at least 1", i, p.Pass)
		}
		if p.Step < 1 || p.Step > p.TotalSteps {
			t.Errorf("frame %d: step = %d, outside 1..%d", i, p.Step, p.TotalSteps)
		}
	}

	// 3. The step advances. A stream stuck on frame one is indistinguishable
	//    from a hang, and it is what a misread field looks like.
	if first, last := obs.previews[0], obs.previews[len(obs.previews)-1]; last.Step <= first.Step {
		t.Errorf("step did not advance across %d frames: first %d, last %d",
			len(obs.previews), first.Step, last.Step)
	}

	// 4. The body is an image. b64_json decoding to bytes proves nothing --
	//    a truncated frame decodes to bytes too. This is the assertion that
	//    would have caught a server switching format.
	withPNG := 0
	for i, p := range obs.previews {
		if len(p.PNG) == 0 {
			// Legal: previews on, nothing rendered yet. Reported, not failed.
			continue
		}
		withPNG++
		cfg, format, err := image.DecodeConfig(strings.NewReader(string(p.PNG)))
		if err != nil {
			t.Errorf("frame %d: %d bytes that are not a decodable image: %v", i, len(p.PNG), err)
			continue
		}
		if cfg.Width == 0 || cfg.Height == 0 {
			t.Errorf("frame %d: decoded %s is %dx%d", i, format, cfg.Width, cfg.Height)
		}
		t.Logf("frame %d: pass %d step %d/%d, %d bytes of %s, %dx%d",
			i, p.Pass, p.Step, p.TotalSteps, len(p.PNG), format, cfg.Width, cfg.Height)
	}
	if withPNG == 0 {
		t.Error("every preview frame carried an empty body: ember would print " +
			"progress and never have a frame to keep")
	}

	// 5. The job finished and produced its image, so none of the above came
	//    from a run that fell over.
	if obs.final.Status != sdserver.StatusCompleted {
		t.Fatalf("final status = %v, want completed (err: %v)", obs.final.Status, obs.final.Err)
	}
	if len(obs.final.Images) != 1 {
		t.Fatalf("completed with %d images, want 1", len(obs.final.Images))
	}
	if _, _, err := image.DecodeConfig(strings.NewReader(string(obs.final.Images[0].Data))); err != nil {
		t.Errorf("the finished image does not decode: %v", err)
	}

	t.Logf("observed %d frames (%d with a body) over %d polls", len(obs.previews), withPNG, obs.polls)
}

// TestPreview_TheSameFrameComesBackBetweenIntervals is the premise ember's
// deduplication rests on, checked against a server rather than a fake.
//
// Every poll returns the current frame whether or not it is new, so the poll
// interval is a bandwidth knob and ember dedupes on the step before printing
// or writing anything. The first version of the unit-test fake advanced the
// step on every poll, so each line appeared once whether or not anything
// deduplicated -- the assertion passed against the bug. The fake was fixed to
// repeat a frame; this is the check that the fake was fixed to match reality
// and not to match the assertion.
func TestPreview_TheSameFrameComesBackBetweenIntervals(t *testing.T) {
	l := harness.New(t)
	srv := startLabServer(t, l)

	job := sdserver.Job{
		Prompt: "a paper boat",
		Width:  labWidth, Height: labHeight,
		Steps:   labSteps,
		Sampler: "euler",
		Seed:    1234,
		Output:  "boat",
	}
	id, err := srv.client.Submit(t.Context(), job, "proj", 1)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	obs := pollToCompletion(t, srv, id)
	if len(obs.previews) < 2 {
		t.Fatalf("saw %d frames, need at least 2 to say anything about repeats", len(obs.previews))
	}

	// Polls are much faster than CPU sampling steps, so consecutive polls
	// landing inside one step is the normal case rather than a lucky one. If
	// this ever stops holding, the dedupe is dead weight and should go --
	// which is a thing worth being told.
	repeats := 0
	for i := 1; i < len(obs.previews); i++ {
		if obs.previews[i].Step == obs.previews[i-1].Step && obs.previews[i].Pass == obs.previews[i-1].Pass {
			repeats++
		}
	}
	if repeats == 0 {
		t.Errorf("no frame was returned twice across %d polls: either the server "+
			"now sends each frame once, in which case ember's deduplication is "+
			"dead code, or this lab polled too slowly to tell", len(obs.previews))
	}
	t.Logf("%d of %d consecutive frame pairs were repeats", repeats, len(obs.previews)-1)
}

// labCancelSteps is long enough that there is a generation to interrupt.
//
// The six-step job above is over in a couple of seconds of CPU sampling,
// which is not a window a cancel can be aimed at. Sixty steps at 64x64 is
// still seconds rather than minutes and leaves plenty of middle: the
// assertion below is that the job stopped BEFORE the last step, so the margin
// is what makes the test mean something rather than pass by luck.
const labCancelSteps = 60

// TestPreview_ARealServerStopsAGenerationWhenAsked is the one honest check on
// the whole cancel path.
//
// sdserver.Client.Cancel existed, was covered to 99% by its own package's
// tests, and was called by NOTHING outside them -- so every assertion about
// it was ember agreeing with a fake it had written. That is the closed loop
// capabilities.model sat in until a real server answered with an object, and
// the one the preview frames sat in until this file existed. A cancel checked
// only against a fake would be the same mistake a third time.
//
// Two things are proved here that no unit test can:
//
//  1. The server DECLARES the capability. features.cancel_generating is read
//     out of a live /capabilities, not out of a fixture -- and false is the
//     answer that makes ember leave an in-flight job running, so it is worth
//     knowing which one a real build gives.
//  2. The server HONOURS the ask. POST /jobs/{id}/cancel, mid-sampling,
//     ends the job as `cancelled` with no images -- and does it before the
//     declared step total, which is what distinguishes a cancel from a
//     generation that simply finished.
func TestPreview_ARealServerStopsAGenerationWhenAsked(t *testing.T) {
	l := harness.New(t)
	srv := startLabServer(t, l)

	caps, err := srv.client.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	// Read from a response and not from a schema, which is the rule this
	// repository has now paid for twice -- allowedCudaVersions and
	// capabilities.model. If a real build says false, ember's refusal to ask
	// it is right and this lab is what says so.
	if !caps.CancelGenerating {
		t.Fatalf("features.cancel_generating = false on a live server; ember will leave an "+
			"in-flight job running and only drop the ones behind it. caps = %+v", caps)
	}

	job := sdserver.Job{
		Prompt: "a paper boat",
		Width:  labWidth, Height: labHeight,
		Steps:   labCancelSteps,
		Sampler: "euler",
		Seed:    1234,
		Output:  "boat",
	}
	if err := sdserver.Validate(job, caps); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// interval 1 so the step count moves on every step: the cancel is aimed
	// at a step in the middle, and a sparse preview would mean aiming at a
	// number ember had not been told yet.
	id, err := srv.client.Submit(t.Context(), job, "proj", 1)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	// Sample until it is demonstrably mid-generation, then ask.
	//
	// A step and not a sleep: "the server is two steps in" is the condition
	// the cancel has to land inside, and a fixed wait would be a guess about
	// how fast this machine samples -- wrong in the direction that cancels
	// a job that has already finished and proves nothing.
	var (
		asked    bool
		atStep   int
		polls    int
		lastStep int
		final    sdserver.State
	)
	for {
		st, perr := srv.client.Poll(ctx, id)
		if perr != nil {
			t.Fatalf("poll after %d polls: %v", polls, perr)
		}
		polls++
		if p := st.Preview; p != nil && p.Step > lastStep {
			lastStep = p.Step
		}
		if !asked && lastStep >= 2 {
			if cerr := srv.client.Cancel(ctx, id); cerr != nil {
				t.Fatalf("cancel at step %d: %v", lastStep, cerr)
			}
			asked, atStep = true, lastStep
			t.Logf("asked the server to cancel at step %d/%d", atStep, labCancelSteps)
		}
		if st.Status.Done() {
			// Captured under its own name, opt-in, as pollToCompletion does:
			// a cancelled job is a response shape no fixture here has ever
			// held, and promoting a guess to evidence is a deliberate act
			// with a README row attached.
			if dir := captureDir(t); dir != "" {
				captureRaw(t, srv, id, filepath.Join(dir, "job_cancelled.json"))
			}
			final = st
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %s neither finished nor stopped: last status %v after %d polls",
				id, st.Status, polls)
		case <-time.After(50 * time.Millisecond):
		}
	}

	if !asked {
		t.Fatalf("the job reached a terminal state (%v) before step 2, so nothing was "+
			"cancelled mid-generation; raise labCancelSteps", final.Status)
	}

	// 1. The server ended it as cancelled. sdserver.StatusCancelled and
	//    Status.Done() were written for this and had never seen one.
	if final.Status != sdserver.StatusCancelled {
		t.Errorf("final status = %v after a cancel at step %d/%d (err: %v); the ask was "+
			"accepted with a 200 and the job ran to %v",
			final.Status, atStep, labCancelSteps, final.Err, final.Status)
	}

	// 2. It stopped EARLY. A server that ignored the ask and sampled all
	//    sixty steps would produce an image and report completed, which the
	//    assertion above catches -- but one that reported `cancelled` after
	//    finishing anyway would not, and that is the version that would have
	//    ember charging for an image it told the operator it had stopped.
	if lastStep >= labCancelSteps {
		t.Errorf("the last step observed was %d of %d, so the generation ran to the end "+
			"despite the cancel", lastStep, labCancelSteps)
	}

	// 3. Nothing was produced, which is the point: a cancelled generation is
	//    sampling that stopped, not an image withheld.
	if len(final.Images) != 0 {
		t.Errorf("a cancelled job came back with %d image(s)", len(final.Images))
	}

	t.Logf("cancelled at step %d/%d, ended as %v after %d polls, %d image(s)",
		atStep, labCancelSteps, final.Status, polls, len(final.Images))
}

// A cancel for a job the server has never heard of is an error and not a
// silent success.
//
// ember asks once per job and only while that job is in flight, so this
// should be unreachable in production -- which is exactly why it is worth
// knowing what the server does with it. If a bad id answered 200, a cancel
// sent against a stale id would report success and the real generation would
// carry on billing.
func TestPreview_ACancelForAJobThatDoesNotExistIsRefused(t *testing.T) {
	l := harness.New(t)
	srv := startLabServer(t, l)

	err := srv.client.Cancel(t.Context(), "no-such-job")
	if err == nil {
		t.Error("cancelling an unknown job id succeeded; a stale id would then report a " +
			"stop that did not happen")
		return
	}
	t.Logf("an unknown job id answers: %v", err)
}

// labBadRoot is a model root seeded with a file that is not a model.
//
// Seeded out-of-band through Docker rather than through anything under test,
// for the reason the SIGKILL labs seed state that way: a production interface
// widened so a test can write a file is a production interface shaped by a
// test's convenience.
const labBadRoot = "/tmp/ember-lab-badroot"

// labBadModel is the id sd-server's own scan gives that file: a relative path
// under the model root and its category directory, which is what `models/load`
// accepts in place of a filesystem path.
const labBadModel = "checkpoints/notamodel.gguf"

// TestPreview_AModelThatCannotLoadIsAnErrorStateAndNotASilence replaces the
// two fixtures that decide what an operator reads when a load fails.
//
// `model_status_error.json` and `error_model_not_loaded.json` were both
// written from examples/server/api.md, and they are the pair that matters
// most, because sd-server spells `error` two ways in one API: a string beside
// a separate `code` for a request it rejects outright, an object for a job
// that failed after being accepted. A client written against one alone
// reports a JSON unmarshalling complaint in place of the reason -- which is
// the whole argument for decodeAPIError, held up until now by two guesses.
//
// WHAT THE CAPTURE ROUTE HAS TO BE, AND WHY. ember hands the weights to
// sd-server as argv, and a `--model` that is not a model makes the process
// *exit before it listens* -- asserted below, because it is the reason this
// lab cannot simply point ember's own serve command at a bad path and read
// the status. `state: "error"` lives on the far side of a port that never
// opens. So the lab starts a server with no weights at all, which is a state
// ember's own argv can produce too (a config whose sd.serve names no model),
// and then drives `POST /sdcpp/v1/models/load` -- a route ember never calls
// -- to make the loader fail while the server is still answering. No bypass
// of Validate, no hole in sdserver: a real sd-server failing to read a real
// file, over the same SSH channel ember uses.
func TestPreview_AModelThatCannotLoadIsAnErrorStateAndNotASilence(t *testing.T) {
	l := harness.New(t)
	n, client := labNodeAndDialer(t, l, "loadfail")

	// 1. ember's own startup path cannot reach state=error at all: the server
	//    dies rather than reporting. AGENTS.md records this failure from a
	//    live run ("get sd version from file failed"), and `orExited` exists
	//    because of it -- a server that died has to be noticed at once rather
	//    than at ready_timeout, which on a rented A100 was thirty cents of
	//    polling a process that was already gone. Nothing had checked it.
	doomed, err := sdserver.ServeCommand([]string{"sd-server", "--model", "/etc/hostname"})
	if err != nil {
		t.Fatal(err)
	}
	exitCtx, stopDoomed := context.WithTimeout(t.Context(), 2*time.Minute)
	defer stopDoomed()
	stdout, wait, err := sshexec.New(client.SSH()).Run(exitCtx, sshx.Command{
		Path: doomed[0], Args: doomed[1:],
	})
	if err != nil {
		t.Fatalf("starting the doomed sd-server: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	// THE EXIT IS COLLECTED ON A CHANNEL AND NOT WAITED FOR INLINE, and the
	// reinstatement pass is why. `ssh.Session.Wait` does not return while the
	// remote process is alive, so a bare wait() here cannot time out:
	// cancelling the context closes the session and no exit status ever
	// arrives -- the defect that wedged `ember up` for twenty-three minutes
	// after a SIGTERM, and that sdserver's own exit goroutine exists for. The
	// outcome this assertion is about is "the server did *not* exit", so the
	// short spelling makes the one result worth reporting the one that hangs:
	// handed a real model instead of /etc/hostname, the inline version stopped
	// answering and go test killed it fifteen minutes later with no assertion
	// printed at all. The goroutine outlives the timeout branch, which is what
	// the deferred cancel above is for.
	done := make(chan error, 1)
	go func() { done <- wait() }()

	var exit *sshx.ExitError
	select {
	case err := <-done:
		switch {
		case err == nil:
			t.Error("sd-server accepted a file that is not a model and exited cleanly; " +
				"ember's model wait would then be the thing that reports it, and the " +
				"status it polls would be the fixture this lab provokes another way")
		case !errors.As(err, &exit):
			t.Fatalf("sd-server failed in a way that is not an exit status: %v", err)
		default:
			// The loader's own sentence, on stderr. ember surfaces it through
			// KindLog, and it is the only text that says which path was wrong
			// -- `models/status` never gets the chance to.
			if !strings.Contains(exit.Stderr, "get sd version from file failed") {
				t.Errorf("exit %d, but stderr does not carry the loader's reason:\n%s",
					exit.Code, exit.Stderr)
			}
			t.Logf("a --model that is not a model exits %d before listening: %s",
				exit.Code, firstErrorLine(exit.Stderr))
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("sd-server was handed a file that is not a model and is still running "+
			"thirty seconds later; the load failure this lab rests on has stopped being "+
			"one, so `state: %q` may now be reachable the way an operator would meet it",
			sdserver.ModelError)
	}
	// Nothing was left listening, which is what makes the state unreachable
	// rather than merely awkward to reach.
	if out := l.ExecOut(t.Context(), n.ID, "ss", "-H", "-ltn"); strings.Contains(out, fmt.Sprint(sdserver.Port)) {
		t.Errorf("something is listening on %d after the load failed:\n%s", sdserver.Port, out)
	}

	// 2. A model root holding a file that is not a model, seeded through
	//    Docker before the server starts so sd-server's own scan finds it.
	l.Exec(t.Context(), n.ID, "mkdir", "-p", path.Join(labBadRoot, "checkpoints"))
	l.Exec(t.Context(), n.ID, "sh", "-c",
		"printf 'this is not a model\\n' > "+path.Join(labBadRoot, labBadModel))

	srv := startServerOn(t, n, client, []string{"sd-server", "--model-dir", labBadRoot}, false)
	capture := captureDir(t)

	// 3. A server up with no weights. The guessed fixture said
	//    `load_elapsed_seconds: 0`; a real one says **null**, which decodes
	//    into ember's float64 as a no-op rather than an error -- true of Go
	//    and not of serde, which is the asymmetry that took a websocket
	//    envelope down one layer up. Worth a captured fixture precisely
	//    because it passes by accident.
	st, err := srv.client.ModelStatus(t.Context())
	if err != nil {
		t.Fatalf("model status on a server with no weights: %v", err)
	}
	if st.State != sdserver.ModelEmpty {
		t.Errorf("state = %v, want empty", st.State)
	}
	if capture != "" {
		_, body, rerr := labRequest(t, srv, http.MethodGet, "/sdcpp/v1/models/status", nil)
		if rerr != nil {
			t.Fatalf("capturing the empty status: %v", rerr)
		}
		captureBody(t, filepath.Join(capture, "model_status_empty.json"), body)
	}

	// 4. THE STRING FORM, on the path an operator actually meets it. A job
	//    submitted to a server with no weights is refused with
	//    `{"code":"model_not_loaded","error":"No model is loaded"}` and a
	//    503. Until this lab, wireErrorEnvelope was decoded by nothing but
	//    its own unit test: every non-2xx reached the operator as httpx's raw
	//    body, so the reason arrived as JSON inside a sentence about a status
	//    code. sdserver.refusal is what reads it, and this is the assertion
	//    that the shape it reads is the shape a server sends.
	_, serr := srv.client.Submit(t.Context(), sdserver.Job{
		Prompt: "a paper boat",
		Width:  labWidth, Height: labHeight,
		Steps: labSteps,
	}, sdserver.PreviewNone, 0)
	if serr == nil {
		t.Fatal("a server with no weights accepted a generation")
	}
	var api *sdserver.APIError
	if !errors.As(serr, &api) {
		t.Errorf("submit error does not carry the server's own refusal, so an "+
			"operator reads JSON instead of a reason: %v", serr)
	} else if api.Code != "model_not_loaded" || api.Message == "" {
		t.Errorf("refusal = %+v, want a code and a message", *api)
	}
	t.Logf("a generation with no weights is refused: %v", serr)
	if capture != "" {
		code, body, rerr := labRequest(t, srv, http.MethodPost, "/sdcpp/v1/img_gen",
			[]byte(`{"prompt":"a paper boat","width":64,"height":64}`))
		if rerr != nil {
			t.Fatalf("capturing the refusal: %v", rerr)
		}
		if code != http.StatusServiceUnavailable {
			t.Errorf("a generation with no weights answered %d, want 503", code)
		}
		captureBody(t, filepath.Join(capture, "error_model_not_loaded.json"), body)
	}

	// 5. A load that fails, with the server still up to be asked about it.
	load := fmt.Sprintf(`{"recipe":"sd1-checkpoint","models":{"model":%q}}`, labBadModel)
	code, body, err := labRequest(t, srv, http.MethodPost, "/sdcpp/v1/models/load", []byte(load))
	if err != nil {
		t.Fatalf("asking the server to load a file that is not a model: %v", err)
	}
	if code == http.StatusOK {
		t.Fatalf("the server loaded a file that is not a model: %s", body)
	}
	t.Logf("models/load answered %d: %s", code, strings.TrimSpace(string(body)))

	last, err := sdserver.WaitModel(t.Context(), srv.client, 30*time.Second, io.Discard, testLogger(io.Discard))
	if err == nil {
		t.Fatalf("the weights became ready after a failed load (%v)", last)
	}
	var notReady *sdserver.NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("a failed load is not reported as a model that never loaded: %v", err)
	}
	// ONE poll, not thirty seconds of them. ModelError is a refusal and not a
	// retry: a load that failed has already read what it was going to read,
	// and waiting costs the whole timeout on a card that will never generate.
	if notReady.Polls != 1 {
		t.Errorf("polls = %d, want 1: a failed load must not be retried on a billing card",
			notReady.Polls)
	}
	if notReady.Last != sdserver.ModelError {
		t.Errorf("last state = %v, want error", notReady.Last)
	}
	// The server's own words reach the operator. An empty Message here is the
	// failure this fixture exists to pin: NotReadyError would then print "last
	// state error" and nothing about which file or why.
	if notReady.Message == "" {
		t.Error("the refusal carries no message, so the operator reads a state and no reason")
	}
	t.Logf("WaitModel refused after %d poll(s): %v", notReady.Polls, err)

	// 6. THE JSON TYPE of that message, which is the thing nobody had
	//    checked. mapModelStatus reads `error` as a *string, while a job's
	//    error is read through decodeAPIError because it is an object. If a
	//    loader failure ever spelled it the job's way, ember would not print a
	//    worse message -- it would fail to decode the response at all and say
	//    so about Go structs. This asserts against the bytes, because the
	//    decoded struct cannot tell the two apart after the fact.
	_, raw, err := labRequest(t, srv, http.MethodGet, "/sdcpp/v1/models/status", nil)
	if err != nil {
		t.Fatalf("reading the error status: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("the error status is not an object: %v", err)
	}
	if got := string(fields["error"]); !strings.HasPrefix(got, `"`) {
		t.Errorf("models/status spells `error` as %s, and ember reads it as a string; "+
			"a non-string there is a decode failure for the whole response rather than "+
			"a worse message", got)
	}
	if capture != "" {
		captureBody(t, filepath.Join(capture, "model_status_error.json"), raw)
	}
}

// labFailingJob is a job every check ember has accepts and sd-server cannot
// finish.
//
// WHY THIS ONE. A job failure had to be provoked without reaching around
// Validate -- an escape hatch for a test is an escape hatch, which this
// package decided once already for the RunPod mapper's sealed branch. So the
// input is one ember's own types permit and its own validation passes: vae
// tiling switched on with a one-pixel tile. ValidateStatic refuses a negative
// tile and nothing else, because the server publishes no tile bound in
// `limits`; Validate has no list to check it against. The server samples the
// whole job and then fails in the decoder, where the tiles are used.
//
// It is left representable on purpose. A minimum of 32 is in this build's log
// and in no response: refusing below it here would be reading a bound out of
// an error string, which is the `allowedCudaVersions` mistake -- and it would
// make the only honest route to a real failed job unreachable, trading
// evidence for an invented limit.
func labFailingJob() sdserver.Job {
	return sdserver.Job{
		Prompt: "a paper boat",
		Width:  labWidth, Height: labHeight,
		Steps:   labSteps,
		Sampler: "euler",
		Seed:    1234,
		Output:  "boat",
		Params: sdserver.Params{
			VAETiling: sdserver.VAETiling{Enabled: true, TileSizeW: 1, TileSizeH: 1},
		},
	}
}

// TestPreview_AFailedJobCarriesTheObjectFormOfError replaces the last
// job-lifecycle guess that matters.
//
// `job_failed.json` carried `{"code":"invalid_request","message":"width must
// be a multiple of 16"}`, and both halves of that were invented: this build
// has no `invalid_request` code anywhere in it, and it rounds a width it does
// not like rather than refusing one. What a real failure says is
// `{"code":"generation_failed","message":"generate_image returned no
// results"}` -- the object form the fixture guessed right, around a code and a
// sentence it guessed wrong.
//
// The combination is what a fixture cannot be reasoned into: an error object
// beside `status: "failed"`, `result: null`, `cancel_requested: false` and a
// `started` the guess did not have. It is the mirror of what the cancel
// capture found -- an error object beside a status that is *not* a failure,
// which made every successful cancel read as a crash -- and the two together
// are why the status is read before the error rather than after.
func TestPreview_AFailedJobCarriesTheObjectFormOfError(t *testing.T) {
	l := harness.New(t)
	srv := startLabServer(t, l)

	caps, err := srv.client.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	job := labFailingJob()
	// Both of ember's refusals pass it, which is the point: no bypass was
	// needed and none was added. If a later Validate learns to refuse this,
	// this lab fails here and says what it was for.
	if err := sdserver.ValidateStatic(job, sdserver.PreviewProj, 1); err != nil {
		t.Fatalf("the failing job is refused by ValidateStatic, so it no longer provokes "+
			"a server-side failure: %v", err)
	}
	if err := sdserver.Validate(job, caps); err != nil {
		t.Fatalf("the failing job is refused by Validate, so it no longer provokes "+
			"a server-side failure: %v", err)
	}

	id, err := srv.client.Submit(t.Context(), job, sdserver.PreviewProj, 1)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	obs := pollToCompletion(t, srv, id)

	// 1. It failed, and it is not a cancel. The two carry the same-shaped
	//    error object and differ only in the status, so a client reading the
	//    error first cannot tell them apart -- which is exactly what
	//    app.runJobs did until a real cancel was captured.
	if obs.final.Status != sdserver.StatusFailed {
		t.Fatalf("status = %v, want failed (err: %v)", obs.final.Status, obs.final.Err)
	}

	// 2. The server's own reason reached ember, as an object.
	if obs.final.Err == nil {
		t.Fatal("a failed job carried no error: the operator is told a generation failed " +
			"and nothing about why")
	}
	if obs.final.Err.Code == "" || obs.final.Err.Message == "" {
		t.Errorf("err = %+v, want both a code and a message", *obs.final.Err)
	}

	// 3. Nothing was produced. A failed job that carried images would mean
	//    ember writing a partial result to disk under the name the operator
	//    asked for.
	if len(obs.final.Images) != 0 {
		t.Errorf("a failed job came back with %d image(s)", len(obs.final.Images))
	}

	t.Logf("a real failed job says: %v (after %d polls, %d preview frames)",
		obs.final.Err, obs.polls, len(obs.previews))
}

// firstErrorLine picks the loader's first [E] line out of a server's stderr.
//
// For the log line only: the whole stderr is in the assertion's message when
// something is wrong, and a one-line summary is what a passing lab should
// print.
func firstErrorLine(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[E]") {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(stderr)
}

// TestPreview_TheServerListensOnLoopbackOnly closes a deferral that was
// recorded rather than quietly skipped.
//
// AGENTS.md held this as "no GPU-less runner can start sd-server", which the
// CPU image makes false. sdserver.ServeCommand appends --listen-ip 127.0.0.1
// and refuses a serve command naming either listen flag; this is the other
// half, which no amount of unit testing reaches: that the binary honours it.
// A server on 0.0.0.0 is an unauthenticated image generator, with arbitrary
// file paths in its request body, on a rented machine's public interface.
func TestPreview_TheServerListensOnLoopbackOnly(t *testing.T) {
	l := harness.New(t)
	srv := startLabServer(t, l)

	out := l.ExecOut(t.Context(), srv.node.ID, "ss", "-H", "-ltn")
	var found bool
	for _, addr := range listeningAddrs(out) {
		port := addr[strings.LastIndex(addr, ":")+1:]
		if port != fmt.Sprint(sdserver.Port) {
			continue
		}
		found = true
		host := addr[:strings.LastIndex(addr, ":")]
		if host != "127.0.0.1" {
			t.Errorf("sd-server listens on %q, want 127.0.0.1 only", addr)
		}
	}
	if !found {
		t.Fatalf("nothing is listening on %d, although the model loaded:\n%s", sdserver.Port, out)
	}
}

// labServer is a started sd-server and the ways to reach it.
type labServer struct {
	node   labNode
	client sdserver.Client
	dialer *sshdial.Client
}

// startLabServer brings a node up, starts sd-server on it the way ember does,
// and waits for the weights.
//
// Deliberately the production pieces rather than a reimplementation:
// sdserver.ServeCommand builds the argv, sshexec starts it asynchronously,
// tunnel.WaitReady watches the port and sdserver.WaitModel watches the model.
// A lab that dialled the port itself would be proving that this file works.
func startLabServer(t *testing.T, l *harness.Lab) labServer {
	t.Helper()
	n, client := labNodeAndDialer(t, l, "preview")
	return startServerOn(t, n, client, []string{"sd-server", "--model", labModel}, true)
}

// labNodeAndDialer brings a node up and opens the connection to it.
//
// Split out of startLabServer because the failure labs below need the node
// before the server: one of them runs a doomed sd-server that exits instead of
// listening, and then a second one on the same node. A node per server would
// pay for an image start and an SSH handshake to prove a thing about a process.
func labNodeAndDialer(t *testing.T, l *harness.Lab, name string) (labNode, *sshdial.Client) {
	t.Helper()

	n := mintAndStartImage(t, l, name, sdImageUnderTest(t))

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return n, client
}

// startServerOn runs one sd-server on an existing node and waits for its port.
//
// waitModel is false for a server started with no weights at all, which is
// what the load-failure lab needs: WaitModel would sit on `empty` until its
// timeout, and `empty` is the state that lab is there to assert.
func startServerOn(t *testing.T, n labNode, client *sshdial.Client, serve []string, waitModel bool) labServer {
	t.Helper()

	argv, err := sdserver.ServeCommand(serve)
	if err != nil {
		t.Fatal(err)
	}

	// Cancelled by the cleanup below, which is what stops the remote process:
	// closing the session is the only signal that reaches it.
	srvCtx, stop := context.WithCancel(context.Background())
	runner := sshexec.New(client.SSH())
	stdout, wait, err := runner.Run(srvCtx, sshx.Command{
		Path: argv[0], Args: argv[1:],
		// The model load is on stderr, and a load that fails is the
		// explanation for everything after it. Surfaced through t.Logf so a
		// failing lab carries the server's own words.
		Stderr: &testWriter{t: t, prefix: "sd-server"},
	})
	if err != nil {
		stop()
		t.Fatalf("starting sd-server: %v", err)
	}
	go func() { _, _ = io.Copy(&testWriter{t: t, prefix: "sd-server"}, stdout) }()

	// Reaped by cancelling and then waiting with a cap. ssh.Session.Wait does
	// not return while the remote process lives, so a bare wait() here would
	// hang the lab rather than fail it -- the same defect that wedged
	// `ember up` for twenty-three minutes after a SIGTERM.
	t.Cleanup(func() {
		stop()
		done := make(chan error, 1)
		go func() { done <- wait() }()
		select {
		case err := <-done:
			// Logged, and never a failure. Stopping the server *is*
			// cancelling the context, so ssh reports the session going away
			// and a non-nil error here is the stop having worked -- the same
			// reason sdserver's orExited prefers the operation's own answer
			// when the context is already cancelled, or Ctrl-C reports a
			// server that crashed. It is printed rather than discarded
			// because the exit of a server that had already died for its own
			// reasons arrives here too, and that status is then the
			// explanation for every assertion above it.
			if err != nil {
				t.Logf("sd-server exited: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Log("sd-server did not exit promptly after being stopped")
		}
	})

	remote := tunnel.RemoteLoopback(sdserver.Port)
	readyCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := tunnel.WaitReady(readyCtx, client, remote, 250*time.Millisecond); err != nil {
		t.Fatalf("sd-server never accepted a connection on %v: %v", remote, err)
	}

	c := sdserver.New(client.DialContext)
	if waitModel {
		if _, err := sdserver.WaitModel(t.Context(), c, 5*time.Minute, io.Discard, testLogger(io.Discard)); err != nil {
			t.Fatalf("the model never became ready: %v", err)
		}
	}
	return labServer{node: n, client: c, dialer: client}
}

// observed is everything one generation told us.
type observed struct {
	previews []sdserver.Preview
	final    sdserver.State
	polls    int
}

// pollToCompletion polls hard and keeps every frame.
//
// A fixed short interval rather than ember's production backoff: this is
// measuring what the server emits, and a backoff would be measuring ember's
// patience. The cap is wall-clock rather than a poll count, because a CPU
// sampling step is seconds and a count would be a guess about the machine.
func pollToCompletion(t *testing.T, srv labServer, id string) observed {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	var obs observed
	capture := captureDir(t)
	for {
		st, err := srv.client.Poll(ctx, id)
		if err != nil {
			t.Fatalf("poll after %d polls: %v", obs.polls, err)
		}
		obs.polls++
		if st.Preview != nil {
			obs.previews = append(obs.previews, *st.Preview)
			// The bytes, not the decoded struct: a fixture is evidence about
			// what the server sent, and a struct is evidence about what ember
			// made of it.
			if capture != "" && len(obs.previews) == 2 {
				captureRaw(t, srv, id, filepath.Join(capture, "job_generating.json"))
			}
		}
		switch st.Status {
		case sdserver.StatusCompleted, sdserver.StatusFailed, sdserver.StatusCancelled:
			if capture != "" {
				captureRaw(t, srv, id, filepath.Join(capture, "job_"+st.Status.String()+".json"))
			}
			obs.final = st
			return obs
		}

		select {
		case <-ctx.Done():
			t.Fatalf("job %s did not finish: last status %v after %d polls", id, st.Status, obs.polls)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// captureDir is where observed responses are written, or "" for the usual run.
//
// Opt-in, because promoting a fixture from guess to evidence is a deliberate
// act with a README row attached -- see internal/sdserver/testdata/README.md.
// A lab that rewrote committed fixtures on every run would make the
// distinction that file exists to draw meaningless.
func captureDir(t *testing.T) string {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv("EMBER_CAPTURE_DIR"))
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("capture dir: %v", err)
	}
	return dir
}

// captureRaw saves one job response exactly as the server sent it.
//
// A second request rather than a tee of the one Poll made: Poll returns a
// decoded State and widening it to hand back bytes would be a production
// signature shaped by a test's convenience, which is the same rule that kept
// stdin off sshx.Runner.
func captureRaw(t *testing.T, srv labServer, id, path string) {
	t.Helper()

	_, body, err := labRequest(t, srv, http.MethodGet, "/sdcpp/v1/jobs/"+id, nil)
	if err != nil {
		t.Logf("capture %s: %v", path, err)
		return
	}
	captureBody(t, path, body)
}

// labRequest sends one request to sd-server over the SSH channel and hands
// back the status and the body exactly as they arrived.
//
// It exists for two things ember's own client cannot do and should not learn
// to: reading the *bytes* of a response -- a fixture is evidence about what
// the server sent, and a decoded struct is evidence about what ember made of
// it -- and reaching `POST /sdcpp/v1/models/load`, which ember never calls
// because it hands the weights to sd-server as argv. Widening sdserver.Client
// for either would be a production interface shaped by a test's convenience,
// the rule that kept stdin off sshx.Runner.
func labRequest(t *testing.T, srv labServer, method, path string, body []byte) (int, []byte, error) {
	t.Helper()

	hc := &http.Client{Transport: &http.Transport{DialContext: srv.dialer.DialContext}}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", sdserver.Port, path)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// captureBody writes one observed response under a fixture's name.
//
// Indented so the committed fixture is readable in a diff. The base64 bodies
// stay as they arrived -- a fixture with the image stripped out would not
// exercise the decode that the stripping hid.
func captureBody(t *testing.T, path string, body []byte) {
	t.Helper()

	var pretty any
	if err := json.Unmarshal(body, &pretty); err == nil {
		if out, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			body = append(out, '\n')
		}
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Logf("captured %s (%d bytes)", path, len(body))
}

// sdImageUnderTest names the CPU workload image, hard-failing when it is
// absent.
//
// Not a skip and not a pull: a skip reports success for something that never
// ran, and reaching the network would be testing GitHub.
func sdImageUnderTest(t *testing.T) string {
	t.Helper()

	image := strings.TrimSpace(os.Getenv("EMBER_SD_IMAGE"))
	if image == "" {
		image = "ember:sd"
	}
	return image
}

// listeningAddrs keeps the whole address column of `ss -H -ltn`.
//
// The sibling of listeningPorts, which discards everything before the colon.
// That is right for "is anything listening on a port nobody asked for" and
// wrong here: the entire question is whether 1234 is bound to loopback or to
// everything, and those two differ only in the part that helper throws away.
func listeningAddrs(out string) []string {
	var addrs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		if !strings.Contains(f[3], ":") {
			continue
		}
		addrs = append(addrs, f[3])
	}
	return addrs
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// testWriter sends a remote stream to the test log, line by line.
type testWriter struct {
	t      *testing.T
	prefix string
	buf    []byte
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			return len(p), nil
		}
		w.t.Logf("%s: %s", w.prefix, strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
}
