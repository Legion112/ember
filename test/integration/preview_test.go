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
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
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

	n := mintAndStartImage(t, l, "preview", sdImageUnderTest(t))

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	argv, err := sdserver.ServeCommand([]string{"sd-server", "--model", labModel})
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
	if _, err := sdserver.WaitModel(t.Context(), c, 5*time.Minute, io.Discard, testLogger(io.Discard)); err != nil {
		t.Fatalf("the model never became ready: %v", err)
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

	hc := &http.Client{Transport: &http.Transport{DialContext: srv.dialer.DialContext}}
	url := fmt.Sprintf("http://127.0.0.1:%d/sdcpp/v1/jobs/%s", sdserver.Port, id)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Logf("capture %s: %v", path, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Logf("capture %s: %v", path, err)
		return
	}

	// Indented so the committed fixture is readable in a diff. The base64
	// bodies stay as they arrived -- a fixture with the image stripped out
	// would not exercise the decode that the stripping hid.
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
