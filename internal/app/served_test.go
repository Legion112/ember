package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/httpx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
)

// fakeSD is an sd-server good enough to drive the orchestration.
//
// An httptest.Server rather than a hand-written client fake, for the reason
// internal/sdserver's own tests use one: the external system is an HTTP API,
// so the layers under test should be the production client, its real decoding
// and its real error typing. What this adds over those tests is time -- a job
// that is queued, then generating with a preview, then completed -- which is
// what the orchestration is made of.
type fakeSD struct {
	t *testing.T

	// polls per job id, so a job can take several polls to finish.
	pollsBeforeDone int

	// failJob makes the job end as failed with sd-server's object-shaped
	// error, which is the shape the job endpoint uses.
	failJob bool

	// cancelJob makes it end as cancelled, which is terminal and is neither a
	// success nor a server-reported failure.
	cancelJob bool

	// modelState is what /models/status reports.
	modelState string

	// samplers is what /capabilities reports; empty means the server did not
	// report a list.
	samplers []string

	// cancelGenerating is features.cancel_generating.
	cancelGenerating bool

	// cancelFails makes POST /jobs/{id}/cancel answer 500, which is the ask
	// failing while the generation carries on billing.
	cancelFails bool

	submitted []map[string]any
	polled    map[string]int

	// mu guards the three fields a cancel touches, because a cancel arrives
	// on the goroutine that asked for it while the poll loop is reading.
	mu sync.Mutex

	// cancelled is the ids the server was asked to stop, and asking is what
	// MAKES a job end as cancelled here.
	//
	// A FAKE THAT TOOK THE CANCEL AND THEN COMPLETED THE JOB ANYWAY would
	// let ember's "it ended as cancelled because we asked" branch pass
	// without the ask ever being made, which is the preview fake that never
	// repeated a frame, one package over: a fake tidier than the thing it
	// stands in for tests a program that does not exist.
	cancelled []string
}

// cancels is the ids the server was asked to stop, in order.
func (f *fakeSD) cancels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

func newFakeSD(t *testing.T) *fakeSD {
	return &fakeSD{
		t:               t,
		pollsBeforeDone: 2,
		modelState:      "ready",
		samplers:        []string{"euler", "heun"},
		polled:          map[string]int{},
	}
}

func (f *fakeSD) handler() http.HandlerFunc {
	png := base64.StdEncoding.EncodeToString([]byte("generated-pixels"))
	prev := base64.StdEncoding.EncodeToString([]byte("preview-pixels"))

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/sdcpp/v1/models/status":
			_, _ = fmt.Fprintf(w, `{"state":%q,"load_elapsed_seconds":12.5,"error":null}`, f.modelState)

		case r.URL.Path == "/sdcpp/v1/capabilities":
			b, _ := json.Marshal(map[string]any{
				// An object, as the real server sends it. It was a string
				// here until a live sd-server rejected that, which is the
				// whole argument for capturing fixtures rather than writing
				// them from a field list.
				"model":         map[string]any{"name": "sd15.gguf", "path": "/models/sd15.gguf", "stem": "sd15"},
				"samplers":      f.samplers,
				"schedulers":    []string{"karras"},
				"preview_modes": []string{"none", "proj"},
				"limits":        map[string]any{"min_width": 64, "max_width": 2048},
				"features":      map[string]any{"cancel_generating": f.cancelGenerating},
			})
			_, _ = w.Write(b)

		case r.URL.Path == "/sdcpp/v1/img_gen":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				f.t.Error(err)
			}
			f.submitted = append(f.submitted, body)
			id := fmt.Sprintf("job-%d", len(f.submitted))
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprintf(w, `{"id":%q,"kind":"img_gen","status":"queued"}`, id)

		case strings.HasSuffix(r.URL.Path, "/cancel"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sdcpp/v1/jobs/"), "/cancel")
			if f.cancelFails {
				// The ask failed and the generation is still running. ember
				// must keep polling: abandoning here would lose the image the
				// card is about to produce AND the record of what it cost.
				w.WriteHeader(500)
				return
			}
			f.mu.Lock()
			f.cancelled = append(f.cancelled, id)
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{}`))

		case strings.HasPrefix(r.URL.Path, "/sdcpp/v1/jobs/"):
			id := strings.TrimPrefix(r.URL.Path, "/sdcpp/v1/jobs/")
			// Under the lock, because a cancel test reads the count from its
			// own goroutine in order to know the job is actually being polled
			// -- and before that there was one goroutine here and the map
			// needed no guard.
			f.mu.Lock()
			f.polled[id]++
			n := f.polled[id]
			cancelled := slices.Contains(f.cancelled, id)
			f.mu.Unlock()
			switch {
			// Asking is what makes it end as cancelled, which is what the
			// real server does -- and it takes precedence over the step
			// count, so a job that "never finishes" still ends when stopped.
			case cancelled:
				// WITH THE ERROR OBJECT, which is what a real server sends
				// and what this fake did not, until the preview lab captured
				// one: testdata/job_cancelled.json carries
				// {"code":"cancelled","message":"job cancelled by client"}
				// beside the status. Without it ember's refusal check read
				// every live cancel as "sd-server refused it", the queue read
				// that as a failed batch, and the rental was destroyed --
				// taking every other request on it. A fake tidier than the
				// thing it stands in for tests a program that does not exist,
				// and this one cost a rental.
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"cancelled","cancel_requested":true,`+
					`"error":{"code":"cancelled","message":"job cancelled by client"}}`, id)
			case n < f.pollsBeforeDone:
				// The same frame is returned to two consecutive polls before
				// the step advances, which is what a real server does: a
				// frame is produced every preview_interval steps while ember
				// polls more often than that. Without the repeat, a dedupe
				// that does nothing would pass -- the step would advance on
				// its own every time and each line would appear once anyway.
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"generating","preview":`+
					`{"pass":1,"step":%d,"total_steps":4,"b64_json":%q}}`, id, (n+1)/2, prev)
			case f.failJob:
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"failed","error":`+
					`{"code":"invalid_request","message":"width must be a multiple of 16"}}`, id)
			case f.cancelJob:
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"cancelled"}`, id)
			default:
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"completed","result":`+
					`{"images":[{"index":0,"b64_json":%q}]}}`, id, png)
			}

		default:
			http.NotFound(w, r)
		}
	}
}

// start returns a client pointed at the fake, and a dialer that reaches it the
// way the production one reaches the node's loopback.
func (f *fakeSD) start(t *testing.T) (sdserver.Client, *countingDialer) {
	t.Helper()
	s := httptest.NewServer(f.handler())
	t.Cleanup(s.Close)
	addr := strings.TrimPrefix(s.URL, "http://")
	d := &countingDialer{addr: addr}
	return sdserver.Client{HTTP: httpx.Client{BaseURL: s.URL, HTTP: s.Client()}}, d
}

// countingDialer stands in for the SSH tunnel: every dial ember makes is
// supposed to be to the node's own loopback and nowhere else, and this records
// what was asked for so a test can assert that.
type countingDialer struct {
	addr  string
	asked []string
}

func (d *countingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.asked = append(d.asked, addr)
	var n net.Dialer
	return n.DialContext(ctx, network, d.addr)
}

// shrinkPoll makes the job poll immediate for one test.
//
// The production interval is two seconds, which is right against a sampling
// step that takes a second or more -- and wrong in a unit suite, where it
// turned forty polls into forty real seconds. internal/s3 shrinks PartSize
// for the same reason.
func shrinkPoll(t *testing.T) {
	t.Helper()
	wasMin, wasMax := jobPollMin, jobPollMax
	jobPollMin, jobPollMax = time.Millisecond, time.Millisecond
	t.Cleanup(func() { jobPollMin, jobPollMax = wasMin, wasMax })
}

func testRun(jobs ...sdserver.Job) sdserver.Run {
	return sdserver.Run{Preview: sdserver.PreviewProj, PreviewInterval: 5, Jobs: jobs}
}

func TestRunJobs_CollectsEveryImageAndPricesEachJob(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	c, _ := f.start(t)
	dir := filepath.Join(t.TempDir(), "run")

	run := testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat", Width: 512, Height: 512, Steps: 4, Seed: 1},
		sdserver.Job{Prompt: "a dog", Output: "dog"},
	)

	var out strings.Builder
	if err := runJobs(t.Context(), report.Text(&out), c, run, dir, node.Rate(2_000_000), cancelSignal{}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"cat-00.png", "dog-00.png"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != "generated-pixels" {
			t.Errorf("%s = %q", name, raw)
		}
	}

	got := out.String()
	for _, want := range []string{
		`job 1/2: "a cat" 512x512`, // geometry and prompt identify it in a log
		"seed 1",
		"job 1/2: pass 1, step 1/4",
		"job 2/2",
		"of card time",
		"so far",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// ONE JOB IN FLIGHT. A completed result is erased 600s after it completed, so
// queueing ten up front leaves eight paid-for images one dropped connection
// away from being purged.
func TestRunJobs_SubmitsOneJobAtATime(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.pollsBeforeDone = 3

	// Three jobs. If they were all submitted first, the fake would have three
	// bodies before the first poll.
	var submittedWhenFirstPolled int
	orig := f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sdcpp/v1/jobs/") && submittedWhenFirstPolled == 0 {
			submittedWhenFirstPolled = len(f.submitted)
		}
		orig(w, r)
	}))
	defer srv.Close()
	c := sdserver.Client{HTTP: httpx.Client{BaseURL: srv.URL, HTTP: srv.Client()}}

	run := testRun(
		sdserver.Job{Prompt: "one", Output: "a"},
		sdserver.Job{Prompt: "two", Output: "b"},
		sdserver.Job{Prompt: "three", Output: "c"},
	)
	if err := runJobs(t.Context(), report.Discard, c, run, filepath.Join(t.TempDir(), "r"), 0, cancelSignal{}); err != nil {
		t.Fatal(err)
	}
	if submittedWhenFirstPolled != 1 {
		t.Errorf("%d job(s) had been submitted when the first poll happened, want 1; "+
			"a queued batch puts finished results inside sd-server's 600s purge window",
			submittedWhenFirstPolled)
	}
	if len(f.submitted) != 3 {
		t.Errorf("submitted %d job(s), want 3", len(f.submitted))
	}
}

// A job that names nothing still produces files an operator can tell apart.
func TestRunJobs_NumbersAJobThatNamesNothing(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	c, _ := f.start(t)
	dir := filepath.Join(t.TempDir(), "run")

	if err := runJobs(t.Context(), report.Discard, c, testRun(
		sdserver.Job{Prompt: "a"}, sdserver.Job{Prompt: "b"},
	), dir, 0, cancelSignal{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001-00.png", "002-00.png"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Error(err)
		}
	}
}

// The preview frame is written over the last one rather than accumulating:
// the useful thing is a file an operator can leave open in a viewer, not
// three hundred PNGs of a half-finished cat.
func TestRunJobs_KeepsOnlyTheLatestPreviewFrame(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.pollsBeforeDone = 4 // three preview frames, then completed
	c, _ := f.start(t)
	dir := filepath.Join(t.TempDir(), "run")

	if err := runJobs(t.Context(), report.Discard, c, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), dir, 0, cancelSignal{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var previews []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "preview") {
			previews = append(previews, e.Name())
		}
	}
	if len(previews) != 1 {
		t.Errorf("preview files = %v, want exactly one that gets overwritten", previews)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cat-preview-00.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "preview-pixels" {
		t.Errorf("preview = %q", raw)
	}
}

// Progress is reported as pass, step and total and never as one percentage:
// step/total_steps describe the pass, and passes restart, so a bar built from
// them runs to the end and starts again, which reads as a hang.
func TestRunJobs_ReportsEachDistinctStepOnce(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	// Five polls: steps 1,1,2,2 then completed, so step 1 is returned twice
	// and a dedupe that does nothing reports it twice.
	f.pollsBeforeDone = 5
	c, _ := f.start(t)

	var out strings.Builder
	if err := runJobs(t.Context(), report.Text(&out), c, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), filepath.Join(t.TempDir(), "r"), 0, cancelSignal{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "step 1/4"); n != 1 {
		t.Errorf("step 1 reported %d times, want once:\n%s", n, got)
	}
	if !strings.Contains(got, "step 2/4") {
		t.Errorf("step 2 never reported:\n%s", got)
	}
	if strings.Contains(got, "%") {
		t.Errorf("progress was rendered as a percentage, which a restarting pass makes wrong:\n%s", got)
	}
}

func TestRunJobs_StopsAtAFailedJobAndSaysWhatTheServerSaid(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.failJob = true
	c, _ := f.start(t)

	err := runJobs(t.Context(), report.Discard, c, testRun(
		sdserver.Job{Prompt: "a", Output: "a"},
		sdserver.Job{Prompt: "b", Output: "b"},
	), filepath.Join(t.TempDir(), "r"), 0, cancelSignal{})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"job 1/2", "multiple of 16", "invalid_request"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	// And the second job was never submitted: a batch whose first job the
	// server refused is not a batch to keep paying for.
	if len(f.submitted) != 1 {
		t.Errorf("submitted %d job(s) after a failure, want 1", len(f.submitted))
	}
}

// Cancelled is terminal and is neither a success nor a server-reported
// failure, so it needs its own arm or it reads as a completed job with no
// images.
func TestRunJobs_ACancelledJobIsNotASuccess(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.cancelJob = true
	c, _ := f.start(t)

	err := runJobs(t.Context(), report.Discard, c, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), 0, cancelSignal{})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want it to name the status", err)
	}
}

func TestRunJobs_ReportsAnUnusableOutputDirectory(t *testing.T) {
	f := newFakeSD(t)
	c, _ := f.start(t)

	// A file where the run directory wants to be.
	dir := t.TempDir()
	blocked := filepath.Join(dir, "run")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runJobs(t.Context(), report.Discard, c, testRun(sdserver.Job{Prompt: "a"}), blocked, 0, cancelSignal{})
	if err == nil {
		t.Fatal("want an error")
	}
	if len(f.submitted) != 0 {
		t.Error("a job was submitted before ember knew where to put its output")
	}
}

func TestRunJobs_ReportsASubmissionFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer s.Close()
	c := sdserver.Client{HTTP: httpx.Client{BaseURL: s.URL, HTTP: s.Client()}}

	err := runJobs(t.Context(), report.Discard, c, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), 0, cancelSignal{})
	if err == nil || !strings.Contains(err.Error(), "job 1/1") {
		t.Fatalf("err = %v, want the job named", err)
	}
}

func TestRunJobs_ReportsAPollFailure(t *testing.T) {
	shrinkPoll(t)
	var n int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sdcpp/v1/jobs/") {
			w.WriteHeader(500)
			return
		}
		n++
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"j","status":"queued"}`))
	}))
	defer s.Close()
	c := sdserver.Client{HTTP: httpx.Client{BaseURL: s.URL, HTTP: s.Client()}}

	err := runJobs(t.Context(), report.Discard, c, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), 0, cancelSignal{})
	if err == nil || !strings.Contains(err.Error(), "job 1/1") {
		t.Fatalf("err = %v, want the job named", err)
	}
}

func TestAwaitJob_StopsOnCancel(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.pollsBeforeDone = 1 << 20 // never finishes
	c, _ := f.start(t)

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := awaitJob(ctx, report.Discard, c, "j", 1, 1, "a", root, cancelSignal{})
		done <- err
	}()
	cancel()
	if err := <-done; err == nil {
		t.Error("awaitJob ignored a cancelled context")
	}
}

// fakeServerRunner stands in for a process that stays up.
//
// sshx.RecordingRunner cannot: its wait func returns at once, which is right
// for a workload that runs to completion and wrong for a server. Every
// existing caller of the seam copies-then-waits, so no fake in the repository
// had reason to model a process that lingers -- and with one that returns
// immediately, "the server died" fires before "the port opened" in every
// healthy test. The same reason gpucheck has its own scriptedRunner.
//
// linger: wait blocks until the context ends, which is what ember's deferred
// stopServer does. Otherwise wait returns exit straight away, which is a
// server that rejected its own arguments and quit.
type fakeServerRunner struct {
	out    []byte
	exit   error
	linger bool
	calls  []string
}

func (r *fakeServerRunner) Run(ctx context.Context, cmd sshx.Command) (io.ReadCloser, func() error, error) {
	r.calls = append(r.calls, cmd.String())
	body := io.NopCloser(bytes.NewReader(r.out))
	return body, func() error {
		if r.linger {
			<-ctx.Done()
		}
		return r.exit
	}, nil
}

// liveServer is a runner whose server stays up, which is what every test
// about the happy path needs.
func liveServer() *fakeServerRunner { return &fakeServerRunner{linger: true} }

// The backoff exists because a flat two seconds measured slower than no
// resident model at all: a 0.3s job noticed up to 2s late cost more than the
// 0.8s of tensor loading being saved.
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
}

// startServer is the first asynchronous use of the sshx seam in ember: every
// other caller copies the stream and waits inline.
func TestStartServer_StreamsTheOutputAndReaps(t *testing.T) {
	r := sshx.NewRecordingRunner()
	argv := []string{"sd-server", "--diffusion-model", "/models/x.gguf"}
	r.Outputs[sshx.Cmd(argv[0], argv[1:]...).String()] = []byte("loading weights\n")

	var out strings.Builder
	proc, err := startServer(t.Context(), &out, r, argv)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-proc.Exited(); err != nil {
		t.Fatal(err)
	}
	// The model load's own words reach the operator live, because a load that
	// is about to fail says so here long before the status endpoint admits it.
	if !strings.Contains(out.String(), "loading weights") {
		t.Errorf("output = %q, want the server's own stdout", out.String())
	}
	if len(r.Calls()) != 1 || !strings.Contains(r.Calls()[0], "sd-server") {
		t.Errorf("calls = %v", r.Calls())
	}
}

func TestStartServer_RefusesWhatItCannotSend(t *testing.T) {
	r := sshx.NewRecordingRunner()
	if _, err := startServer(t.Context(), io.Discard, r, nil); err == nil {
		t.Error("startServer accepted an empty argv")
	}
	// A NUL cannot be sent at all, and sshx.Command.Validate is what says so.
	if _, err := startServer(t.Context(), io.Discard, r, []string{"sd-server", "a\x00b"}); err == nil {
		t.Error("startServer accepted an unsendable argument")
	}
	if len(r.Calls()) != 0 {
		t.Errorf("something was sent anyway: %v", r.Calls())
	}
}

func TestStartServer_ReportsAStartFailure(t *testing.T) {
	r := sshx.NewRecordingRunner()
	r.FailOn = "sd-server"
	if _, err := startServer(t.Context(), io.Discard, r, []string{"sd-server"}); err == nil {
		t.Error("startServer reported no error when the command could not start")
	}
}

// A server that died is noticed at once, not at ready_timeout.
//
// Measured before this existed: sd-server rejected a flag, printed its
// complaint, exited after about a second, and ember polled the port for the
// full ten-minute default before reporting "never accepted a connection". On
// an A100 at $1.79/hour that is thirty cents for a program that was already
// gone, and the explanation had scrolled past ten minutes earlier.
func TestRunServed_ADeadServerIsNoticedAtOnce(t *testing.T) {
	// A dialer that never connects, which is what a dead server looks like
	// from here, and a runner whose command ends immediately.
	d := &refusingDialer{}
	argv := []string{"sd-server", "--diffusion-model", "/models/x.gguf"}
	// Exactly what a real sd-server did when handed --diffusion-model for a
	// monolithic checkpoint: it printed this and exited.
	r := &fakeServerRunner{
		out:  []byte("[E] get sd version from file failed: ''\n[E] new_sd_ctx_t failed\n"),
		exit: &sshx.ExitError{Cmd: "sd-server", Code: 1},
	}

	// A generous timeout: the point is that it is not waited out.
	spec := servedSpecWithTimeout(t, argv, time.Hour)

	start := time.Now()
	var out strings.Builder
	err := runServed(t.Context(), &out, report.Text(&out), d, r, spec, testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	took := time.Since(start)

	if err == nil {
		t.Fatal("want an error")
	}
	if took > 30*time.Second {
		t.Errorf("took %v to notice a dead server against a 1h timeout", took)
	}
	if !strings.Contains(err.Error(), "exited before it accepted a connection") {
		t.Errorf("err = %v, want it to say the process ended rather than that the port never opened", err)
	}
	// The server's own words are what explain it, so they have to have
	// reached the operator.
	if !strings.Contains(out.String(), "get sd version from file failed") {
		t.Errorf("output = %q, want the server's own complaint", out.String())
	}
}

func TestRunServed_TheWholeLifecycle(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	_, d := f.start(t)

	r := liveServer()
	spec := servedSpec(t, []string{"sd-server", "--diffusion-model", "/models/x.gguf"})
	dir := filepath.Join(t.TempDir(), "run")

	var out strings.Builder
	err := runServed(t.Context(), &out, report.Text(&out), d, r, spec,
		testRun(sdserver.Job{Prompt: "a cat", Output: "cat", Sampler: "euler"}),
		session.Options{Log: discardLog()}, dir, node.Rate(2_000_000))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "cat-00.png")); err != nil {
		t.Error(err)
	}
	// Every dial is to the node's own loopback on the port ember chose, and
	// never anywhere else: direct-tcpip would otherwise reach whatever the
	// node can, including a metadata service.
	if len(d.asked) == 0 {
		t.Fatal("nothing was dialled")
	}
	want := fmt.Sprintf("127.0.0.1:%d", sdserver.Port)
	for _, a := range d.asked {
		if a != want {
			t.Errorf("dialled %q, want only %q", a, want)
		}
	}
	if !strings.Contains(out.String(), "starting sd-server") {
		t.Errorf("output = %q", out.String())
	}
}

// The saving said out loud, and only when there is one: with cmds this load
// would be paid again for every job after the first.
func TestRunServed_SaysWhatBeingResidentSaves(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	_, d := f.start(t)
	spec := servedSpec(t, []string{"sd-server"})

	var out strings.Builder
	err := runServed(t.Context(), &out, report.Text(&out), d, liveServer(), spec,
		testRun(sdserver.Job{Prompt: "a"}, sdserver.Job{Prompt: "b"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "paid once rather than 2 times") {
		t.Errorf("output = %q, want the resident-weights saving named", out.String())
	}
	// And the span is ember's own, not the server's load_elapsed_seconds,
	// which the fake reports as 12.5s. sd-server reported 551ms for a 56 GiB
	// FLUX.2 model on the first real run while the cmds arm of the same
	// measurement was spending 80-96s per image re-reading those weights, so
	// the field names something other than the cost of becoming resident.
	// Reinstating `load` here puts the fake's 12.5s back in the output.
	if strings.Contains(out.String(), "12.5s") {
		t.Errorf("reported the server's load_elapsed_seconds instead of the measured span: %q", out.String())
	}

	// One job saves nothing, so it says nothing.
	var single strings.Builder
	f2 := newFakeSD(t)
	_, d2 := f2.start(t)
	if err := runServed(t.Context(), &single, report.Text(&single), d2, liveServer(), spec,
		testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r2"), 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(single.String(), "paid once rather than") {
		t.Errorf("a single-job run claimed a saving: %q", single.String())
	}
}

// The cheapest refusal in the feature: a mistyped sampler costs the model load
// and nothing else, instead of surfacing at job seven.
func TestRunServed_RefusesEveryBadJobBeforeGeneratingAnything(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	_, d := f.start(t)
	spec := servedSpec(t, []string{"sd-server"})

	err := runServed(t.Context(), io.Discard, report.Discard, d, liveServer(), spec,
		// The bad one is last, which is the point: it must still refuse
		// before the first is submitted.
		testRun(
			sdserver.Job{Prompt: "a", Sampler: "euler"},
			sdserver.Job{Prompt: "b", Sampler: "eulerr"},
		),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "sd.jobs[1]") {
		t.Errorf("err = %v, want the index named", err)
	}
	if len(f.submitted) != 0 {
		t.Errorf("%d job(s) were generated before the refusal", len(f.submitted))
	}
	// A config error, which is what makes internal/cli exit 3. Asserted as
	// the sentinel rather than as the status, because the status is the
	// mapping internal/cli owns and this is the fact it maps.
	if !errors.Is(err, ErrUsage) {
		t.Errorf("err = %v, want it to be an ErrUsage", err)
	}
}

func TestRunServed_RefusesWhenTheWeightsNeverLoad(t *testing.T) {
	f := newFakeSD(t)
	f.modelState = "error"
	_, d := f.start(t)

	err := runServed(t.Context(), io.Discard, report.Discard, d, liveServer(),
		servedSpec(t, []string{"sd-server"}), testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "never loaded the weights") {
		t.Errorf("err = %v", err)
	}
	if len(f.submitted) != 0 {
		t.Error("a job was submitted against a server with no weights")
	}
}

func TestRunServed_ReportsAServerThatNeverListens(t *testing.T) {
	// A dialer that always refuses, which is what a server that died during
	// its model load looks like from here.
	d := &refusingDialer{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := runServed(ctx, io.Discard, report.Discard, d, liveServer(),
		servedSpec(t, []string{"sd-server"}), testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "is not listening on 127.0.0.1:1234") {
		t.Errorf("err = %v, want it to name the address that never opened", err)
	}
	// And not blamed on the process: this server was alive the whole time,
	// and ember only stopped it on the way out.
	if strings.Contains(err.Error(), "exited before") {
		t.Errorf("err = %v, blames a cancellation on the server dying", err)
	}
}

func TestRunServed_ReportsAServerItCannotStart(t *testing.T) {
	f := newFakeSD(t)
	_, d := f.start(t)
	r := sshx.NewRecordingRunner()
	r.FailOn = "sd-server"

	err := runServed(t.Context(), io.Discard, report.Discard, d, r,
		servedSpec(t, []string{"sd-server"}), testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil || !strings.Contains(err.Error(), "start sd-server") {
		t.Fatalf("err = %v", err)
	}
}

type refusingDialer struct{}

func (refusingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, fmt.Errorf("connection refused")
}

func servedSpec(t *testing.T, serve []string) node.Spec {
	t.Helper()
	return servedSpecWithTimeout(t, serve, time.Minute)
}

func servedSpecWithTimeout(t *testing.T, serve []string, tmo time.Duration) node.Spec {
	t.Helper()
	finished, err := sdserver.ServeCommand(serve)
	if err != nil {
		t.Fatal(err)
	}
	s, err := node.NewSpec(node.SpecOptions{
		Image:        "ghcr.io/example/x@sha256:" + strings.Repeat("a", 64),
		TTL:          time.Hour,
		Serve:        finished,
		ServeTimeout: tmo,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestExampleConfigs_CanSyncTheirOwnStore is the regression test for a bug
// this repository shipped and nobody noticed.
//
// Converting the example configs to per-architecture `images:` maps removed
// their scalar `image:`. That is fine for `ember up`, which picks by the
// card's architecture -- and fatal for `ember store sync`, which rents a CPU
// machine on purpose. A CPU box has no compute capability, so the map cannot
// answer for it and is deliberately not consulted; with no scalar to fall
// back to, the command refuses. The effect was that ember.flux2.yaml,
// ember.video.example.yaml and ember.sd-server.example.yaml could not
// populate the stores they depend on, and the only way to find out was to
// run the command.
//
// Checked against the committed files rather than a fixture, because the
// files are the thing that was wrong.
func TestExampleConfigs_CanSyncTheirOwnStore(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "ember*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 3 {
		t.Fatalf("found %d example config(s), want at least 3; the glob is checking nothing", len(paths))
	}

	// What `ember store sync` rents: no GPU, so no architecture.
	cpu := node.Offer{Provider: "runpod", Token: "cpu3c", VCPU: 4}
	if cpu.HasGPU() {
		t.Fatal("the stand-in offer has a GPU, so it does not exercise the CPU path")
	}

	// Filtered before the subtests rather than skipped inside them: CI greps
	// the unit log for "--- SKIP" and fails on a hit, because a test that
	// skips itself reports success for something that never ran. A config
	// with no assets has nothing to sync and is simply not a case here.
	var withAssets []string
	for _, p := range paths {
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatalf("%s does not load: %v", filepath.Base(p), err)
		}
		if len(cfg.Assets) > 0 {
			withAssets = append(withAssets, p)
		}
	}
	if len(withAssets) == 0 {
		t.Fatal("no committed config lists assets, so this test is checking nothing")
	}

	for _, p := range withAssets {
		t.Run(filepath.Base(p), func(t *testing.T) {
			cfg, err := config.Load(p)
			if err != nil {
				t.Fatal(err)
			}
			ref, from, err := ResolveImage(cfg, cpu)
			if err != nil {
				t.Fatalf("this config lists %d asset(s) but cannot resolve an image for the CPU "+
					"machine `ember store sync` rents, so it cannot populate its own store: %v",
					len(cfg.Assets), err)
			}
			if from != "image:" {
				t.Errorf("resolved from %q, want the scalar image:", from)
			}
			if ref == "" {
				t.Error("resolved an empty reference")
			}
		})
	}
}

// TestRunJobs_TheProgressEventNamesTheFrameItWrote is what lets a reader
// above this package join a step to the picture of that step.
//
// The name travels on the progress event rather than on one of its own,
// because a UI that learns there is a step 4 before it learns there is a
// frame for it shows step 4 beside step 3's picture -- and because the name
// is safeStem's to decide, so re-deriving it anywhere else is a second
// spelling of the same rule.
func TestRunJobs_TheProgressEventNamesTheFrameItWrote(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.pollsBeforeDone = 4
	c, _ := f.start(t)
	dir := filepath.Join(t.TempDir(), "run")

	var rec recordingSink
	if err := runJobs(t.Context(), &rec, c, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), dir, 0, cancelSignal{}); err != nil {
		t.Fatal(err)
	}

	var named int
	for _, e := range rec.events {
		if e.Kind != report.KindJobProgress {
			continue
		}
		if len(e.Wrote) == 0 {
			t.Errorf("step %d reported a frame and did not name it", e.Step)
			continue
		}
		named++
		if e.Wrote[0].Name != "cat-preview-00.png" {
			t.Errorf("frame name = %q, want the name sdserver wrote", e.Wrote[0].Name)
		}
		if e.Wrote[0].Bytes != int64(len("preview-pixels")) {
			t.Errorf("frame bytes = %d, want the file's own length", e.Wrote[0].Bytes)
		}
	}
	if named == 0 {
		t.Fatal("no progress event named a frame; a reader has nothing to join a step to")
	}

	// The file is on disk under the name the event gave, or the row points at
	// nothing and the gallery is broken images.
	if _, err := os.Stat(filepath.Join(dir, "cat-preview-00.png")); err != nil {
		t.Fatal(err)
	}
}

// recordingSink keeps every event, because an assertion about what a reader
// above this package can join needs the values and not the rendered lines.
type recordingSink struct{ events []report.Event }

func (r *recordingSink) Emit(e report.Event) { r.events = append(r.events, e) }

// --- cancel ---

// TestRunJobs_CancelStopsTheJobInFlightAndDropsTheRest is the whole feature.
//
// sdserver.Client.Cancel was written, covered by its own package's tests, and
// called by NOTHING -- "a feature built end to end and never connected passes
// every test it has", for the second time in this repository. This is the
// join: the operator's stop reaches the server, and the jobs behind it never
// start.
//
// BOTH HALVES ARE ASSERTED, and the second is the one with the money in it. A
// cancel that only stopped the job in flight would let job 2 of 3 begin a
// second later on the same card: the button would look like it worked and the
// bill would not change, which is the "reads as working" defect that keeps
// turning up here.
func TestRunJobs_CancelStopsTheJobInFlightAndDropsTheRest(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.pollsBeforeDone = 1 << 20 // sampling, and would never finish on its own
	c, _ := f.start(t)

	stop := make(chan struct{})
	var rec recordingSink
	errc := make(chan error, 1)
	go func() {
		errc <- runJobs(t.Context(), &rec, c, testRun(
			sdserver.Job{Prompt: "a", Output: "a"},
			sdserver.Job{Prompt: "b", Output: "b"},
			sdserver.Job{Prompt: "c", Output: "c"},
		), filepath.Join(t.TempDir(), "r"), node.Rate(2_000_000),
			cancelSignal{on: stop, server: true})
	}()

	// Wait until the first job is actually being polled, so the cancel lands
	// mid-generation rather than before anything started -- which is the
	// other test below.
	waitFor(t, func() bool { return f.polls("job-1") > 0 })
	close(stop)

	err := <-errc
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled: a cancelled batch is not a failed one, and "+
			"the queue reads that difference to decide whether the rental dies", err)
	}
	if got := f.cancels(); len(got) != 1 || got[0] != "job-1" {
		t.Errorf("cancels = %v, want exactly job-1 asked once", got)
	}
	if len(f.submitted) != 1 {
		t.Errorf("%d job(s) were submitted, want 1: the two behind it are the money", len(f.submitted))
	}

	e := findEvent(t, rec.events, report.KindJobCancelled)
	if e.Err != "" {
		t.Errorf("error = %q, want empty: the server took the ask", e.Err)
	}
	if e.Index != 1 || e.Total != 3 {
		t.Errorf("index/total = %d/%d, want 1/3 so a reader can say 2 were dropped", e.Index, e.Total)
	}
	if e.Unit != report.UnitJob {
		t.Errorf("unit = %q, want %q: a job was in flight", e.Unit, report.UnitJob)
	}

	// The card time it spent is still reported. A return that skipped the
	// cost would make a cancelled batch look cheaper than it was, which is
	// the one direction a money figure must never be wrong in.
	cost := findEvent(t, rec.events, report.KindUnitCost)
	if cost.Index != 1 {
		t.Errorf("cost event is for job %d, want 1", cost.Index)
	}
}

// A cancel that lands between two jobs stops the batch and names no job.
//
// Index is then the count that FINISHED, because "job 1/3" over a job whose
// image is already on disk would name the one thing that was not cancelled.
func TestRunJobs_CancelBetweenJobsDropsTheRestAndNamesNoJob(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	c, _ := f.start(t)
	dir := filepath.Join(t.TempDir(), "r")

	// Closed before the second job can start, by cancelling as soon as the
	// first job's images have been written.
	stop := make(chan struct{})
	var rec recordingSink
	gate := &gateSink{sink: &rec, on: report.KindJobImages, fire: func() { close(stop) }}

	err := runJobs(t.Context(), gate, c, testRun(
		sdserver.Job{Prompt: "a", Output: "a"},
		sdserver.Job{Prompt: "b", Output: "b"},
		sdserver.Job{Prompt: "c", Output: "c"},
	), dir, 0, cancelSignal{on: stop, server: true})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}

	// The first job's image survives: it was produced and paid for.
	if _, serr := os.Stat(filepath.Join(dir, "a-00.png")); serr != nil {
		t.Errorf("job 1's image is missing although it completed: %v", serr)
	}
	if len(f.submitted) != 1 {
		t.Errorf("%d job(s) submitted, want 1", len(f.submitted))
	}
	if got := f.cancels(); len(got) != 0 {
		t.Errorf("cancels = %v, want none: nothing was in flight to stop", got)
	}

	e := findEvent(t, rec.events, report.KindJobCancelled)
	if e.Unit != "" {
		t.Errorf("unit = %q, want empty: no job was in flight", e.Unit)
	}
	if e.Index != 1 || e.Total != 3 {
		t.Errorf("index/total = %d/%d, want 1/3 -- one finished, two dropped", e.Index, e.Total)
	}
}

// A server that does not report cancel_generating is NOT ASKED, and NOT A
// REFUSAL EITHER.
//
// "Fail loudly rather than skip quietly" points at refusing, and refusing is
// the wrong answer here: the operator has just asked ember to stop spending,
// and a 409 leaves them with no lever but stopping ember serve -- which
// destroys the rental and takes every other request on it. ember cannot stop
// the job in flight, and it can drop the two behind it, which on a batch is
// most of the money. So it does the part it can and SAYS what it could not,
// which is where the loudness belongs.
func TestRunJobs_CancelWithoutTheCapabilityDropsTheRestAndSaysWhatItCannotStop(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.pollsBeforeDone = 4
	c, _ := f.start(t)

	stop := make(chan struct{})
	var out strings.Builder
	rec := &recordingSink{}
	both := &teeSink{a: report.Text(&out), b: rec}
	errc := make(chan error, 1)
	go func() {
		errc <- runJobs(t.Context(), both, c, testRun(
			sdserver.Job{Prompt: "a", Output: "a"},
			sdserver.Job{Prompt: "b", Output: "b"},
			sdserver.Job{Prompt: "c", Output: "c"},
		), filepath.Join(t.TempDir(), "r"), 0,
			// server: false -- the build did not report the flag.
			cancelSignal{on: stop, server: false})
	}()

	waitFor(t, func() bool { return f.polls("job-1") > 0 })
	close(stop)

	if err := <-errc; !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled: the cancel was honoured as far as it could be", err)
	}
	if got := f.cancels(); len(got) != 0 {
		t.Errorf("cancels = %v, want none: a server that says it cannot would answer 404 and "+
			"ember would report a transport problem instead of a capability", got)
	}
	if len(f.submitted) != 1 {
		t.Errorf("%d job(s) submitted, want 1: the ones behind it are ember's to drop", len(f.submitted))
	}

	e := findEvent(t, rec.events, report.KindJobCancelled)
	if !strings.Contains(e.Err, "cancel_generating") {
		t.Errorf("error = %q, want it to name the missing capability", e.Err)
	}
	// And it is said in terms of what it costs, not of what the server lacks.
	if got := out.String(); !strings.Contains(got, "will finish and be paid for") {
		t.Errorf("output does not say the in-flight job keeps billing:\n%s", got)
	}
}

// An ask the server refused is reported and the poll carries on.
//
// Abandoning here would be worse than not cancelling at all: the generation
// is still running on a card that is still billing, so ember would lose the
// image it is about to produce AND the record of what it cost.
func TestRunJobs_AnAskTheServerRefusedIsReportedAndThePollCarriesOn(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.pollsBeforeDone = 4
	f.cancelFails = true
	c, _ := f.start(t)
	dir := filepath.Join(t.TempDir(), "r")

	stop := make(chan struct{})
	var rec recordingSink
	errc := make(chan error, 1)
	go func() {
		errc <- runJobs(t.Context(), &rec, c, testRun(
			sdserver.Job{Prompt: "a", Output: "a"},
			sdserver.Job{Prompt: "b", Output: "b"},
		), dir, 0, cancelSignal{on: stop, server: true})
	}()

	waitFor(t, func() bool { return f.polls("job-1") > 0 })
	close(stop)

	if err := <-errc; !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	// The image the card had already paid for arrived anyway.
	if _, serr := os.Stat(filepath.Join(dir, "a-00.png")); serr != nil {
		t.Errorf("the in-flight job's image was lost when the cancel failed: %v", serr)
	}
	e := findEvent(t, rec.events, report.KindJobCancelled)
	if e.Err == "" {
		t.Error("the failed ask was not reported, so the operator thinks it stopped")
	}
}

// Asking twice is asking once.
//
// A closed channel is permanently ready, so a loop that re-read it would ask
// every poll -- a request a second against the program ember is waiting for,
// on a 141-second FLUX.2 step.
func TestAwaitJob_AsksToCancelOnceAndNotOncePerPoll(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	// Never ends on its own, and the fake's cancel branch is what ends it --
	// so if the ask were repeated it would have to happen before the next
	// poll reads it, which is why the count is checked as well as the end.
	f.pollsBeforeDone = 1 << 20
	c, _ := f.start(t)

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	stop := make(chan struct{})
	close(stop) // already asked before the first poll
	st, asked, err := awaitJob(t.Context(), report.Discard, c, "job-1", 1, 1, "a", root,
		cancelSignal{on: stop, server: true})
	if err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Error("awaitJob did not report that ember asked, so runJobs would read the " +
			"cancelled status as an unexplained failure")
	}
	if st.Status != sdserver.StatusCancelled {
		t.Errorf("status = %v, want cancelled", st.Status)
	}
	if got := f.cancels(); len(got) != 1 {
		t.Errorf("asked %d time(s), want once", len(got))
	}
}

// A job the SERVER cancelled with nobody asking is still an error.
//
// This is what the `asked` return is for. Without it, a server that cancelled
// a job of its own accord -- or because something else on the node asked --
// would be filed under "the operator pressed cancel", and a request nobody
// stopped would be recorded as cancelled rather than as the thing ember could
// not account for.
func TestRunJobs_AJobTheServerCancelledWithNobodyAskingIsAFailure(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.cancelJob = true
	c, _ := f.start(t)

	err := runJobs(t.Context(), report.Discard, c, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), 0, cancelSignal{on: make(chan struct{}), server: true})
	if err == nil || errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want a plain failure naming the status", err)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v, want it to name the status", err)
	}
}

// served.Generate joins the capability to the signal, and nothing above it has
// to know it did.
//
// The drain loop knows which request the operator stopped and has no business
// knowing what the server build can do; this is the one place that holds
// both. A Generate that dropped the capability on the floor would silently
// never ask the server -- the quietest possible way for this whole path to do
// nothing.
// CAPS COME OFF THE WIRE AND ARE NOT WRITTEN BY HAND, which is the half the
// first version of this test missed. With CancelGenerating set in a literal
// here, unmapping features.cancel_generating in internal/sdserver left this
// test green: the two components agreed with themselves and nothing crossed
// the gap between them -- the same closed loop the unrecorded preview frame
// sat in. Reading the live /capabilities response is what makes this a check
// that the flag TRAVELS.
func TestServed_GenerateAsksTheServerWhenItSaysItCan(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	f.pollsBeforeDone = 1 << 20
	f.cancelGenerating = true
	c, _ := f.start(t)
	caps, err := c.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.CancelGenerating {
		t.Fatal("the server reported cancel_generating and it did not reach Capabilities")
	}
	s := served{client: c, caps: caps}

	stop := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- s.Generate(t.Context(), report.Discard,
			testRun(sdserver.Job{Prompt: "a", Output: "a"}), t.TempDir(), stop)
	}()
	waitFor(t, func() bool { return f.polls("job-1") > 0 })
	close(stop)

	if err := <-errc; !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if got := f.cancels(); len(got) != 1 {
		t.Errorf("cancels = %v, want the capability to have reached the ask", got)
	}
}

// --- helpers for the cancel tests ---

// polls is how many times one job has been asked about.
func (f *fakeSD) polls(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polled[id]
}

// waitFor spins until cond holds, failing rather than hanging.
//
// A condition and not a sleep: the thing being waited for is "the first job is
// being polled", and a fixed sleep would be a guess about how fast an
// httptest server is that is wrong on a loaded CI runner in the direction
// that makes the test flaky.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}

// findEvent is the one event of a kind, failing when there is not exactly one.
//
// Exactly one, because "asked once and not once per poll" is a property of
// this feature and a helper that returned the first would hide its violation.
func findEvent(t *testing.T, es []report.Event, k report.Kind) report.Event {
	t.Helper()
	var found []report.Event
	for _, e := range es {
		if e.Kind == k {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s event(s), want exactly 1", len(found), k)
	}
	return found[0]
}

// gateSink fires once when a kind goes past, then forwards everything.
//
// It exists so a cancel can be made to land in the gap BETWEEN two jobs,
// which is a window a sleep cannot aim at.
type gateSink struct {
	sink report.Sink
	on   report.Kind
	fire func()
	once bool
}

func (g *gateSink) Emit(e report.Event) {
	g.sink.Emit(e)
	if !g.once && e.Kind == g.on {
		g.once = true
		g.fire()
	}
}

// teeSink sends every event to both.
type teeSink struct{ a, b report.Sink }

func (t *teeSink) Emit(e report.Event) {
	t.a.Emit(e)
	t.b.Emit(e)
}
