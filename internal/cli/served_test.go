package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/httpx"
	"github.com/Legion112/ember/internal/node"
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

	submitted []map[string]any
	polled    map[string]int
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
				"model":         "test",
				"samplers":      f.samplers,
				"schedulers":    []string{"karras"},
				"preview_modes": []string{"none", "proj"},
				"limits":        map[string]any{"min_width": 64, "max_width": 2048},
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
			_, _ = w.Write([]byte(`{}`))

		case strings.HasPrefix(r.URL.Path, "/sdcpp/v1/jobs/"):
			id := strings.TrimPrefix(r.URL.Path, "/sdcpp/v1/jobs/")
			f.polled[id]++
			n := f.polled[id]
			switch {
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
	was := jobPoll
	jobPoll = time.Millisecond
	t.Cleanup(func() { jobPoll = was })
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
	if err := runJobs(t.Context(), &out, c, run, dir, node.Rate(2_000_000)); err != nil {
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
	if err := runJobs(t.Context(), io.Discard, c, run, filepath.Join(t.TempDir(), "r"), 0); err != nil {
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

	if err := runJobs(t.Context(), io.Discard, c, testRun(
		sdserver.Job{Prompt: "a"}, sdserver.Job{Prompt: "b"},
	), dir, 0); err != nil {
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

	if err := runJobs(t.Context(), io.Discard, c, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), dir, 0); err != nil {
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
	if err := runJobs(t.Context(), &out, c, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), filepath.Join(t.TempDir(), "r"), 0); err != nil {
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

	err := runJobs(t.Context(), io.Discard, c, testRun(
		sdserver.Job{Prompt: "a", Output: "a"},
		sdserver.Job{Prompt: "b", Output: "b"},
	), filepath.Join(t.TempDir(), "r"), 0)
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

	err := runJobs(t.Context(), io.Discard, c, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), 0)
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
	err := runJobs(t.Context(), io.Discard, c, testRun(sdserver.Job{Prompt: "a"}), blocked, 0)
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

	err := runJobs(t.Context(), io.Discard, c, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), 0)
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

	err := runJobs(t.Context(), io.Discard, c, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), 0)
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
		_, err := awaitJob(ctx, io.Discard, c, "j", "job 1/1", "a", root)
		done <- err
	}()
	cancel()
	if err := <-done; err == nil {
		t.Error("awaitJob ignored a cancelled context")
	}
}

// startServer is the first asynchronous use of the sshx seam in ember: every
// other caller copies the stream and waits inline.
func TestStartServer_StreamsTheOutputAndReaps(t *testing.T) {
	r := sshx.NewRecordingRunner()
	argv := []string{"sd-server", "--diffusion-model", "/models/x.gguf"}
	r.Outputs[sshx.Cmd(argv[0], argv[1:]...).String()] = []byte("loading weights\n")

	var out strings.Builder
	wait, err := startServer(t.Context(), &out, r, argv)
	if err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
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

func TestRunServed_TheWholeLifecycle(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	_, d := f.start(t)

	r := sshx.NewRecordingRunner()
	spec := servedSpec(t, []string{"sd-server", "--diffusion-model", "/models/x.gguf"})
	dir := filepath.Join(t.TempDir(), "run")

	var out strings.Builder
	err := runServed(t.Context(), &out, d, r, spec,
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
	err := runServed(t.Context(), &out, d, sshx.NewRecordingRunner(), spec,
		testRun(sdserver.Job{Prompt: "a"}, sdserver.Job{Prompt: "b"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "paid once rather than 2 times") {
		t.Errorf("output = %q, want the resident-weights saving named", out.String())
	}

	// One job saves nothing, so it says nothing.
	var single strings.Builder
	f2 := newFakeSD(t)
	_, d2 := f2.start(t)
	if err := runServed(t.Context(), &single, d2, sshx.NewRecordingRunner(), spec,
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

	err := runServed(t.Context(), io.Discard, d, sshx.NewRecordingRunner(), spec,
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
	// A config error, so the exit code says so.
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
}

func TestRunServed_RefusesWhenTheWeightsNeverLoad(t *testing.T) {
	f := newFakeSD(t)
	f.modelState = "error"
	_, d := f.start(t)

	err := runServed(t.Context(), io.Discard, d, sshx.NewRecordingRunner(),
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

	err := runServed(ctx, io.Discard, d, sshx.NewRecordingRunner(),
		servedSpec(t, []string{"sd-server"}), testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "never accepted a connection") {
		t.Errorf("err = %v, want it to name what failed", err)
	}
}

func TestRunServed_ReportsAServerItCannotStart(t *testing.T) {
	f := newFakeSD(t)
	_, d := f.start(t)
	r := sshx.NewRecordingRunner()
	r.FailOn = "sd-server"

	err := runServed(t.Context(), io.Discard, d, r,
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
	finished, err := sdserver.ServeCommand(serve)
	if err != nil {
		t.Fatal(err)
	}
	s, err := node.NewSpec(node.SpecOptions{
		Image:        "ghcr.io/example/x@sha256:" + strings.Repeat("a", 64),
		TTL:          time.Hour,
		Serve:        finished,
		ServeTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDescribeJob(t *testing.T) {
	long := strings.Repeat("x", 100)
	for _, tc := range []struct {
		name string
		job  sdserver.Job
		want []string
	}{
		{"everything", sdserver.Job{Prompt: "a cat", Width: 512, Height: 256, Steps: 4, Seed: 9},
			[]string{`"a cat"`, "512x256", "4 steps", "seed 9"}},
		{"a prompt from extra", sdserver.Job{Extra: map[string]any{"prompt": "from extra"}},
			[]string{`"from extra"`}},
		{"nothing at all", sdserver.Job{}, []string{`""`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeJob(tc.job)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("describeJob = %q, want it to contain %q", got, w)
				}
			}
		})
	}

	// A FLUX.2 prompt is a thousand characters; the first words are what
	// identifies it in a log.
	got := describeJob(sdserver.Job{Prompt: long})
	if len(got) > 70 {
		t.Errorf("describeJob did not truncate a long prompt: %d chars", len(got))
	}
	if !strings.Contains(got, "...") {
		t.Errorf("a truncated prompt must say so: %q", got)
	}
}

// The dry run is where an operator checks a served config before paying for
// it, so it has to show what ember added as clearly as what they wrote.
func TestUp_DryRunShowsTheServedShape(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	cfg := imagesCfg(t, `
image: ember:lab
sd:
  serve: [sd-server, --diffusion-model, /models/x.gguf]
  jobs:
    - prompt: a lovely orange cat
      width: 512
      height: 512
      steps: 20
      seed: 7
      output: cat
    - prompt: a dog
`)
	var out bytes.Buffer
	if err := runUp([]string{"--provider", "local", "--config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"would serve: sd-server --diffusion-model /models/x.gguf",
		// ember's, not the config's, and visible for that reason.
		"--listen-ip 127.0.0.1",
		`job 1/2: "a lovely orange cat" 512x512 20 steps seed 7`,
		`job 2/2: "a dog"`,
		"previews proj every 5 step(s)",
		"nothing was created",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// EMBER OWNS THE LISTEN ADDRESS, and the refusal happens before anything is
// rented. A serve command choosing 0.0.0.0 would publish an unauthenticated
// generator, with arbitrary file paths in its request body, on a rented
// machine's public interface.
func TestUp_RefusesAServeCommandThatChoosesItsOwnAddress(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	e := d.Engine.(*dockerengine.RecordingEngine)
	cfg := imagesCfg(t, `
image: ember:lab
sd:
  serve: [sd-server, --listen-ip, 0.0.0.0]
  jobs:
    - prompt: a cat
`)
	var out bytes.Buffer
	err := runUp([]string{"--provider", "local", "--config", cfg}, &out, d)
	if err == nil {
		t.Fatal("up accepted a serve command that chose its own listen address")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit = %d, want %d for a config error", ExitCode(err), ExitUsage)
	}
	if !strings.Contains(err.Error(), "public interface") {
		t.Errorf("err = %v, want it to say why", err)
	}
	if len(e.Containers()) != 0 {
		t.Error("something was created before the refusal")
	}
}

// Half a served run is refused before anything is rented, in both directions.
func TestUp_RefusesHalfAServedRunBeforeRenting(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"jobs with no server", "image: ember:lab\nsd:\n  jobs:\n    - prompt: a cat\n",
			"no server to send them to"},
		{"a server with no jobs", "image: ember:lab\nsd:\n  serve: [sd-server]\n",
			"generate nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := labDepsWithCard(t, a2080Ti)
			e := d.Engine.(*dockerengine.RecordingEngine)
			var out bytes.Buffer
			err := runUp([]string{"--provider", "local", "--config", imagesCfg(t, tc.body)}, &out, d)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if ExitCode(err) != ExitUsage {
				t.Errorf("exit = %d, want %d", ExitCode(err), ExitUsage)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if len(e.Containers()) != 0 {
				t.Error("something was created before the refusal")
			}
		})
	}
}

// `ember store sync` shares a config with the runs that generate, and a CPU
// box has no card to hold weights resident for. NewSpec refuses the
// combination, so buildSpec has to drop the server the way it drops the probe
// -- otherwise a config with an sd: block cannot sync its own weights.
func TestStoreSync_DropsTheServerOnACPUOnlyRun(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	cfg := imagesCfg(t, `
image: ember:lab
store:
  name: models
  mount: /models
assets:
  - url: https://example.invalid/w.gguf
    sha256: `+strings.Repeat("a", 64)+`
sd:
  serve: [sd-server, --diffusion-model, /models/w.gguf]
  jobs:
    - prompt: a cat
`)
	var out bytes.Buffer
	// It will fail for want of a store on this daemon, but it must not fail
	// because a server was configured.
	err := runStore([]string{"sync", "--config", cfg, "--dry-run"}, &out, d)
	if err != nil && strings.Contains(err.Error(), "no GPU") {
		t.Fatalf("store sync refused because of the sd: block: %v", err)
	}
	if strings.Contains(out.String(), "would serve") {
		t.Errorf("store sync would have started a server:\n%s", out.String())
	}
}

// A geometry with only one dimension set is not printed, because the other
// comes from the server's own default and "512x" would be a guess.
func TestDescribeJob_OmitsAHalfSpecifiedGeometry(t *testing.T) {
	if got := describeJob(sdserver.Job{Prompt: "a", Width: 512}); strings.Contains(got, "512") {
		t.Errorf("describeJob = %q, want no geometry when only one side is set", got)
	}
}
