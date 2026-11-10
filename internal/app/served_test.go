package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/control/controltest"
	"github.com/Legion112/ember/internal/httpx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/worker"
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

	// refuseSubmit and refusePoll answer the two calls that can fail on their
	// own. Both carry the server's own error envelope, because the sentence
	// inside it is the only thing that explains a refusal and it used to
	// reach the operator as raw JSON inside a sentence about a status code.
	refuseSubmit bool
	refusePoll   bool

	// wrap, when set, is what the node's generation server answers with
	// instead of handler(). It exists so a test can observe the ORDER of
	// calls -- whether a batch was submitted one job at a time -- from
	// outside, which is a question about what the worker did and not about
	// what it answered.
	wrap http.HandlerFunc

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
			if f.refuseSubmit {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, `{"code":"model_not_loaded","error":"No model is loaded"}`)
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				f.t.Error(err)
			}
			// UNDER THE LOCK NOW, because the worker is a separate goroutine
			// from the test that reads this: a batch ember cancelled is a
			// batch whose worker is still finishing one job while the
			// assertions run, and an unguarded append there is a data race
			// the race detector fails the suite on.
			f.mu.Lock()
			f.submitted = append(f.submitted, body)
			id := fmt.Sprintf("job-%d", len(f.submitted))
			f.mu.Unlock()
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
			if f.refusePoll {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
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

// --- the node ---

// fakeNode is a worker on a node ember can reach, with no node.
//
// EVERYTHING IN IT IS THE PRODUCTION PATH except the two ends of the wire. A
// real worker.Session runs behind a real control channel -- real handshake,
// real codec, real read limits -- over net.Pipe instead of over an SSH
// direct-tcpip channel, and it drives the real sdserver.Client against
// fakeSD's httptest.Server. The results land on a real directory and are
// brought home by the real transfer.Fetch.
//
// WHY NOT A HAND-WRITTEN FRAME SCRIPT at ember's far end, which would have
// been less rig. Because the events ember emits are the join between two
// programs, and this repository has paid six times for a fake tidier than the
// thing it stands in for: the preview fake that never repeated a frame, the
// cancel fake with no error object. A script would have let ember's loop agree
// with a worker nobody ran -- the identical closed loop, one layer further
// out. What this cannot reach, a worker saying something a worker never says,
// is reached deliberately below with controltest.Pair.
type fakeNode struct {
	sd      *fakeSD
	dial    control.DialFunc
	results string

	mu    sync.Mutex
	asked []string
}

// startNode brings up the worker and returns the node ember talks to.
func startNode(t *testing.T, f *fakeSD) *fakeNode {
	t.Helper()
	h := f.handler()
	if f.wrap != nil {
		h = f.wrap
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	n := &fakeNode{sd: f, results: t.TempDir()}
	o := worker.Options{
		Gen:     sdserver.Client{HTTP: httpx.Client{BaseURL: srv.URL, HTTP: srv.Client()}},
		Results: n.results,
		Log:     controltest.Log(t),
	}
	n.dial = controltest.Node(t, func(ctx context.Context, c *control.Conn) error {
		return worker.Session(ctx, c, o)
	})
	return n
}

// DialContext records what ember asked for and reaches the worker.
//
// EVERY DIAL IS SUPPOSED TO BE TO THE NODE'S OWN LOOPBACK ON ONE PORT and
// nowhere else: direct-tcpip reaches whatever the node can, including the
// provider's metadata service, so what ember names is the whole of that seal
// -- and after this change there is one address rather than two.
func (n *fakeNode) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	n.mu.Lock()
	n.asked = append(n.asked, addr)
	n.mu.Unlock()
	return n.dial(ctx, network, addr)
}

// dialled is every address ember opened a connection to.
func (n *fakeNode) dialled() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.asked...)
}

// served is ember's end of the channel, as withServer would hand it to a
// drain loop.
func (n *fakeNode) served(t *testing.T, rate node.Rate) served {
	t.Helper()
	ch, err := control.Open(t.Context(), n.DialContext)
	if err != nil {
		t.Fatalf("open the control channel: %v", err)
	}
	t.Cleanup(ch.Abort)

	caps, err := n.prepare(t, ch)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return served{ch: ch, caps: caps, rate: rate, runner: &nodeShell{t: t}}
}

// prepare is what withServer does before handing a Served to anybody.
func (n *fakeNode) prepare(t *testing.T, ch *control.Conn) (sdserver.Capabilities, error) {
	t.Helper()
	_, raw, err := prepareWorker(t.Context(), io.Discard, ch, time.Minute)
	if err != nil {
		return sdserver.Capabilities{}, err
	}
	var caps sdserver.Capabilities
	err = json.Unmarshal(raw, &caps)
	return caps, err
}

// nodeShell is the node's own shell: it runs the generation server and it
// runs tar.
//
// THE TAR HALF IS WHAT THE RESULTS PATH NEEDS. A worker writes its images to
// the node's own disk and names them, and ember fetches exactly those paths --
// so a test of that path has to answer a real transfer.TarCommand with a real
// gzipped archive, or what it exercises is a fetch that never happened.
// Reading the file off this machine's disk is honest here: the worker in this
// rig really did write it there.
//
// A FAKE AND NOT /usr/bin/tar, because `make test` needs no external binary
// and because the one thing that matters about the archive -- that ember's
// own Extract accepts it and lands the member under the name the event gave
// -- is a property of the bytes rather than of GNU tar.
type nodeShell struct {
	t *testing.T

	// server is how the generation server's own process behaves. The zero
	// value lingers, which is what a server that stays up does.
	exit   error
	out    []byte
	noWait bool

	mu    sync.Mutex
	calls []string
}

func (s *nodeShell) Run(ctx context.Context, cmd sshx.Command) (io.ReadCloser, func() error, error) {
	s.mu.Lock()
	s.calls = append(s.calls, cmd.String())
	s.mu.Unlock()

	if cmd.Path == "tar" {
		body, err := s.tarOf(cmd)
		if err != nil {
			return io.NopCloser(bytes.NewReader(nil)), func() error { return err }, nil
		}
		return io.NopCloser(bytes.NewReader(body)), func() error { return nil }, nil
	}
	return io.NopCloser(bytes.NewReader(s.out)), func() error {
		if !s.noWait && s.exit == nil {
			// A process whose exit arrives once ember stops it, which is what
			// the deferred reaper waits for.
			<-ctx.Done()
		}
		return s.exit
	}, nil
}

// tarOf answers transfer.TarCommand with the file it names.
func (s *nodeShell) tarOf(cmd sshx.Command) ([]byte, error) {
	// `tar -czf - -C <dir> -- <base>`, which is the one shape
	// transfer.TarCommand produces; anything else is a command this rig does
	// not model and is better a loud failure than an empty archive, because
	// GNU tar over a missing path writes a valid EMPTY archive and that reads
	// as a successful fetch of nothing.
	if len(cmd.Args) != 6 || cmd.Args[2] != "-C" || cmd.Args[4] != "--" {
		return nil, fmt.Errorf("this rig does not model %q", cmd.String())
	}
	dir, base := cmd.Args[3], cmd.Args[5]
	body, err := os.ReadFile(filepath.Join(dir, base)) //nolint:gosec // a path this rig built
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: base, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body)),
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(body); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ran is every command the node was asked to run.
func (s *nodeShell) ran() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func testRun(jobs ...sdserver.Job) sdserver.Run {
	return sdserver.Run{Preview: sdserver.PreviewProj, PreviewInterval: 5, Jobs: jobs}
}

// idleLikeAWorker is how a frame script ends: still reading.
//
// IT WAS `<-ctx.Done()`, AND THAT MADE THE FAKE LESS CAPABLE THAN THE THING IT
// STANDS IN FOR -- which this repository has paid for six times in the other
// direction and once, here, in this one. A real worker always has a reader
// goroutine on the channel; a script that stopped reading after its last frame
// meant every send ember makes afterwards waited out its own bound, and the
// package's suite went from 17 to 44 seconds the moment ember learned to tell
// a worker it had given up on a batch.
//
// So the fakes read to the end, like a worker, and the seconds were a fake's
// behaviour rather than a timeout that needed shortening.
func idleLikeAWorker(ctx context.Context, c *control.Conn) error {
	for {
		if _, err := c.Recv(ctx); err != nil {
			return nil
		}
	}
}

// --- what ember does with a batch ---

func TestGenerate_CollectsEveryImageAndPricesEachJob(t *testing.T) {
	f := newFakeSD(t)
	n := startNode(t, f)
	s := n.served(t, node.Rate(2_000_000))
	dir := filepath.Join(t.TempDir(), "run")

	run := testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat", Width: 512, Height: 512, Steps: 4, Seed: 1},
		sdserver.Job{Prompt: "a dog", Output: "dog"},
	)

	var out strings.Builder
	if err := s.Generate(t.Context(), report.Text(&out), run, dir, nil); err != nil {
		t.Fatal(err)
	}

	// The bytes came off the card, were written on the node, were named in an
	// event and were fetched by that name. Every one of those steps is real
	// in this rig, which is the point of asserting on the file's content.
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

// TestGenerate_SubmitsOneJobAtATime.
//
// THE WHOLE BATCH IS PUSHED AND THE SERVER STILL SEES ONE JOB, which is the
// arrangement the two constraints together require and the thing that changed
// here. sd-server's async_jobs.cpp purges a finished result 600s after it
// completed, so queueing ten jobs THERE leaves eight paid-for images one lapse
// away from being deleted -- and that argument is about the generation
// server's queue, not the worker's. The worker holds the batch in memory,
// which is what lets ember stop driving; the submission stays one at a time,
// which is what keeps the TTL unreachable.
func TestGenerate_SubmitsOneJobAtATime(t *testing.T) {
	f := newFakeSD(t)
	f.pollsBeforeDone = 3

	// Counted at the first poll: if the batch were submitted up front, the
	// fake would hold three bodies before anything was polled.
	var submittedWhenFirstPolled int
	orig := f.handler()
	var mu sync.Mutex
	f.wrap = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if strings.HasPrefix(r.URL.Path, "/sdcpp/v1/jobs/") && submittedWhenFirstPolled == 0 {
			submittedWhenFirstPolled = f.submits()
		}
		mu.Unlock()
		orig(w, r)
	}

	n := startNode(t, f)
	s := n.served(t, 0)

	run := testRun(
		sdserver.Job{Prompt: "one", Output: "a"},
		sdserver.Job{Prompt: "two", Output: "b"},
		sdserver.Job{Prompt: "three", Output: "c"},
	)
	if err := s.Generate(t.Context(), report.Discard, run, filepath.Join(t.TempDir(), "r"), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if submittedWhenFirstPolled != 1 {
		t.Errorf("%d job(s) had been submitted when the first poll happened, want 1; "+
			"a queued batch puts finished results inside sd-server's 600s purge window",
			submittedWhenFirstPolled)
	}
	if f.submits() != 3 {
		t.Errorf("submitted %d job(s), want 3", f.submits())
	}
}

// A job that names nothing still produces files an operator can tell apart.
//
// The numbering is the worker's now and the name travels on the landed frame,
// so what this asserts is that the number ember fetched by is the number the
// node wrote.
func TestGenerate_NumbersAJobThatNamesNothing(t *testing.T) {
	f := newFakeSD(t)
	n := startNode(t, f)
	s := n.served(t, 0)
	dir := filepath.Join(t.TempDir(), "run")

	if err := s.Generate(t.Context(), report.Discard, testRun(
		sdserver.Job{Prompt: "a"}, sdserver.Job{Prompt: "b"},
	), dir, nil); err != nil {
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
func TestGenerate_KeepsOnlyTheLatestPreviewFrame(t *testing.T) {
	f := newFakeSD(t)
	f.pollsBeforeDone = 4 // three preview frames, then completed
	n := startNode(t, f)
	s := n.served(t, 0)
	dir := filepath.Join(t.TempDir(), "run")

	if err := s.Generate(t.Context(), report.Discard, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), dir, nil); err != nil {
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
	// It travelled on the control channel rather than being fetched, because
	// the point of a preview is latency and a preview nobody sees until the
	// batch ends is not one.
	if string(raw) != "preview-pixels" {
		t.Errorf("preview = %q", raw)
	}
}

// Progress is reported as pass, step and total and never as one percentage:
// step/total_steps describe the pass, and passes restart, so a bar built from
// them runs to the end and starts again, which reads as a hang.
//
// THE DEDUPE IS THE WORKER'S NOW, which is where dropping a repeated frame
// also saves the bytes rather than only the line. What is asserted is
// unchanged, because what has to be true is about what a reader sees.
func TestGenerate_ReportsEachDistinctStepOnce(t *testing.T) {
	f := newFakeSD(t)
	// Five polls: steps 1,1,2,2 then completed, so step 1 is returned twice
	// and a dedupe that does nothing reports it twice.
	f.pollsBeforeDone = 5
	n := startNode(t, f)
	s := n.served(t, 0)

	var out strings.Builder
	if err := s.Generate(t.Context(), report.Text(&out), testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), filepath.Join(t.TempDir(), "r"), nil); err != nil {
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

func TestGenerate_StopsAtAFailedJobAndSaysWhatTheServerSaid(t *testing.T) {
	f := newFakeSD(t)
	f.failJob = true
	n := startNode(t, f)
	s := n.served(t, 0)

	err := s.Generate(t.Context(), report.Discard, testRun(
		sdserver.Job{Prompt: "a", Output: "a"},
		sdserver.Job{Prompt: "b", Output: "b"},
	), filepath.Join(t.TempDir(), "r"), nil)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"job 1/2", "multiple of 16", "invalid_request"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	// And the second job was never submitted: a batch whose first job the
	// server refused is not a batch to keep paying for. It was PUSHED -- the
	// worker holds the batch -- and the worker stopped, which is the same
	// money either way and is the thing worth asserting.
	if f.submits() != 1 {
		t.Errorf("submitted %d job(s) after a failure, want 1", f.submits())
	}
}

// Cancelled is terminal and is neither a success nor a server-reported
// failure, so it needs its own arm or it reads as a completed job with no
// images.
func TestGenerate_ACancelledJobIsNotASuccess(t *testing.T) {
	f := newFakeSD(t)
	f.cancelJob = true
	n := startNode(t, f)
	s := n.served(t, 0)

	err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), nil)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want it to name the status", err)
	}
}

func TestGenerate_ReportsAnUnusableOutputDirectory(t *testing.T) {
	f := newFakeSD(t)
	n := startNode(t, f)
	s := n.served(t, 0)

	// A file where the run directory wants to be.
	dir := t.TempDir()
	blocked := filepath.Join(dir, "run")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}), blocked, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if f.submits() != 0 {
		t.Error("a job was pushed before ember knew where to put its output")
	}
}

func TestGenerate_ReportsASubmissionFailure(t *testing.T) {
	f := newFakeSD(t)
	f.refuseSubmit = true
	n := startNode(t, f)
	s := n.served(t, 0)

	err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), nil)
	if err == nil || !strings.Contains(err.Error(), "job 1/1") {
		t.Fatalf("err = %v, want the job named", err)
	}
	// The server's own sentence, which is the only thing that explains it.
	if !strings.Contains(err.Error(), "No model is loaded") {
		t.Errorf("err = %v, want the server's own explanation", err)
	}
}

func TestGenerate_ReportsAPollFailure(t *testing.T) {
	f := newFakeSD(t)
	f.refusePoll = true
	n := startNode(t, f)
	s := n.served(t, 0)

	err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), nil)
	if err == nil || !strings.Contains(err.Error(), "job 1/1") {
		t.Fatalf("err = %v, want the job named", err)
	}
}

// TestGenerate_TheProgressEventNamesTheFrameItWrote is what lets a reader
// above this package join a step to the picture of that step.
//
// The name travels on the progress event rather than on one of its own,
// because a UI that learns there is a step 4 before it learns there is a
// frame for it shows step 4 beside step 3's picture -- and because the name
// is safeStem's to decide, so re-deriving it anywhere else is a second
// spelling of the same rule.
func TestGenerate_TheProgressEventNamesTheFrameItWrote(t *testing.T) {
	f := newFakeSD(t)
	f.pollsBeforeDone = 4
	n := startNode(t, f)
	s := n.served(t, 0)
	dir := filepath.Join(t.TempDir(), "run")

	var rec recordingSink
	if err := s.Generate(t.Context(), &rec, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), dir, nil); err != nil {
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

// TestGenerate_TheImagesEventNamesWhatWasFetched.
//
// NEW HERE, because the images no longer arrive in a poll response: the
// worker writes them on the node and names them, and ember fetches those
// paths. So there are now two claims that have to agree -- what the node said
// it wrote, and what landed on this disk -- and a row that named a file the
// fetch did not bring home is a gallery of broken images.
func TestGenerate_TheImagesEventNamesWhatWasFetched(t *testing.T) {
	f := newFakeSD(t)
	n := startNode(t, f)
	s := n.served(t, 0)
	dir := filepath.Join(t.TempDir(), "run")

	var rec recordingSink
	if err := s.Generate(t.Context(), &rec, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
	), dir, nil); err != nil {
		t.Fatal(err)
	}

	e := findEvent(t, rec.events, report.KindJobImages)
	if e.Files != 1 {
		t.Errorf("files = %d, want the one that arrived", e.Files)
	}
	if e.Bytes != int64(len("generated-pixels")) {
		t.Errorf("bytes = %d, want the length of what landed", e.Bytes)
	}
	if len(e.Wrote) != 1 || e.Wrote[0].Name != "cat-00.png" {
		t.Fatalf("wrote = %+v", e.Wrote)
	}
	if _, err := os.Stat(filepath.Join(dir, e.Wrote[0].Name)); err != nil {
		t.Errorf("the event named a file that is not on this disk: %v", err)
	}
}

// --- cancel ---

// TestGenerate_CancelStopsTheJobInFlightAndDropsTheRest is the whole feature.
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
//
// THE JOBS BEHIND IT ARE NOW IN THE WORKER'S QUEUE rather than in ember's
// loop, which is what the cancel frame has to reach -- and it is also why the
// cancel is sent from a second goroutine while this loop is blocked reading
// events, the one place in the program where two goroutines write to a
// control channel.
func TestGenerate_CancelStopsTheJobInFlightAndDropsTheRest(t *testing.T) {
	f := newFakeSD(t)
	f.pollsBeforeDone = 1 << 20 // sampling, and would never finish on its own
	f.cancelGenerating = true
	n := startNode(t, f)
	s := n.served(t, node.Rate(2_000_000))

	stop := make(chan struct{})
	var rec recordingSink
	errc := make(chan error, 1)
	go func() {
		errc <- s.Generate(t.Context(), &rec, testRun(
			sdserver.Job{Prompt: "a", Output: "a"},
			sdserver.Job{Prompt: "b", Output: "b"},
			sdserver.Job{Prompt: "c", Output: "c"},
		), filepath.Join(t.TempDir(), "r"), stop)
	}()

	// Wait until the first job is actually being polled, so the cancel lands
	// mid-generation rather than before anything started.
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
	if f.submits() != 1 {
		t.Errorf("%d job(s) were submitted, want 1: the two behind it are the money", f.submits())
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
	if cost.Cost <= 0 {
		t.Error("a cancelled job cost nothing, which a card that sampled cannot have")
	}
}

// TestGenerate_ACancelThatLandedBetweenTwoJobsNamesNoJobAndEndsTheBatch.
//
// A FRAME SCRIPT AND NOT THE REAL WORKER, which is a change from the test this
// replaces and is about what is now true rather than about convenience. The
// gap between two jobs used to be a window in ember's own loop, and a
// gateSink could close the stop channel inside it. The worker holds the batch
// now, so it starts job 2 the instant job 1 ends -- ember cannot aim at that
// gap and has no business trying. Which case it was is the WORKER's
// determination (internal/worker's queue tests are where it is made), and what
// is ember's is the mapping: a cancellation with no job in flight names no
// job, and Index is then the count that FINISHED, because "job 2/3" over a
// job whose image is already on disk would name the one thing that was not
// cancelled.
//
// It also ends the batch, which is the half that stops ember waiting for ever:
// the frames about jobs 2 and 3 are never coming.
func TestGenerate_ACancelThatLandedBetweenTwoJobsNamesNoJobAndEndsTheBatch(t *testing.T) {
	ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		// Three jobs arrive.
		for i := 0; i < 3; i++ {
			if _, err := c.Recv(ctx); err != nil {
				return err
			}
		}
		// Job 1 runs and finishes.
		for _, f := range []control.Frame{
			{Kind: control.KindStarted, Index: 1, Total: 3},
			{Kind: control.KindEnded, Index: 1, Total: 3, Status: sdserver.StatusCompleted.String()},
			// And then the cancel took effect with nothing in flight.
			{Kind: control.KindCancelled},
		} {
			if err := c.Send(ctx, f); err != nil {
				return err
			}
		}
		return idleLikeAWorker(ctx, c)
	})
	s := served{ch: ch, rate: node.Rate(2_000_000), runner: &nodeShell{t: t}}

	var rec recordingSink
	err := s.Generate(t.Context(), &rec, testRun(
		sdserver.Job{Prompt: "a", Output: "a"},
		sdserver.Job{Prompt: "b", Output: "b"},
		sdserver.Job{Prompt: "c", Output: "c"},
	), filepath.Join(t.TempDir(), "r"), nil)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
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
//
// THE CAPABILITY IS THE WORKER'S TO READ NOW, off the same document ember
// reads: both decode the server's own bytes with sdserver.ParseCapabilities,
// so this also proves the flag TRAVELS rather than being written by hand at
// either end.
func TestGenerate_CancelWithoutTheCapabilityDropsTheRestAndSaysWhatItCannotStop(t *testing.T) {
	f := newFakeSD(t)
	f.pollsBeforeDone = 4
	f.cancelGenerating = false
	n := startNode(t, f)
	s := n.served(t, 0)
	if s.caps.CancelGenerating {
		t.Fatal("the fake reported no cancel_generating and ember read one")
	}

	stop := make(chan struct{})
	var out strings.Builder
	rec := &recordingSink{}
	both := &teeSink{a: report.Text(&out), b: rec}
	errc := make(chan error, 1)
	go func() {
		errc <- s.Generate(t.Context(), both, testRun(
			sdserver.Job{Prompt: "a", Output: "a"},
			sdserver.Job{Prompt: "b", Output: "b"},
			sdserver.Job{Prompt: "c", Output: "c"},
		), filepath.Join(t.TempDir(), "r"), stop)
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
	if f.submits() != 1 {
		t.Errorf("%d job(s) submitted, want 1: the ones behind it are ember's to drop", f.submits())
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
func TestGenerate_AnAskTheServerRefusedIsReportedAndThePollCarriesOn(t *testing.T) {
	f := newFakeSD(t)
	f.pollsBeforeDone = 4
	f.cancelFails = true
	f.cancelGenerating = true
	n := startNode(t, f)
	s := n.served(t, 0)
	dir := filepath.Join(t.TempDir(), "r")

	stop := make(chan struct{})
	var rec recordingSink
	errc := make(chan error, 1)
	go func() {
		errc <- s.Generate(t.Context(), &rec, testRun(
			sdserver.Job{Prompt: "a", Output: "a"},
			sdserver.Job{Prompt: "b", Output: "b"},
		), dir, stop)
	}()

	waitFor(t, func() bool { return f.polls("job-1") > 0 })
	close(stop)

	if err := <-errc; !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	// The image the card had already paid for arrived anyway -- written on
	// the node, named in an event, and fetched.
	if _, serr := os.Stat(filepath.Join(dir, "a-00.png")); serr != nil {
		t.Errorf("the in-flight job's image was lost when the cancel failed: %v", serr)
	}
	e := findEvent(t, rec.events, report.KindJobCancelled)
	if e.Err == "" {
		t.Error("the failed ask was not reported, so the operator thinks it stopped")
	}
}

// A job the SERVER cancelled with nobody asking is still an error.
//
// Without the distinction, a server that cancelled a job of its own accord --
// or because something else on the node asked -- would be filed under "the
// operator pressed cancel", and a request nobody stopped would be recorded as
// cancelled rather than as the thing ember could not account for. The frame
// carries `cancelled` as the worker's own statement about whether IT asked,
// which is the fact ember cannot work out for itself.
func TestGenerate_AJobTheServerCancelledWithNobodyAskingIsAFailure(t *testing.T) {
	f := newFakeSD(t)
	f.cancelJob = true
	n := startNode(t, f)
	s := n.served(t, 0)

	err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), make(chan struct{}))
	if err == nil || errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want a plain failure naming the status", err)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v, want it to name the status", err)
	}
}

// TestGenerate_RefusesAFrameForAJobItNeverPushed.
//
// A NODE IS UNTRUSTED INPUT, which is the rule internal/transfer enforces
// twice over a tar stream, and the index on every frame is a number a rented
// machine chose. `run.Jobs[f.Index-1]` on it is an out-of-range panic in the
// process that holds the operator's provider credentials -- so the check is
// in one place and every frame that names a job goes through it.
func TestGenerate_RefusesAFrameForAJobItNeverPushed(t *testing.T) {
	for _, bad := range []int{0, 2, -1, 1 << 30} {
		t.Run(fmt.Sprintf("job %d of a batch of one", bad), func(t *testing.T) {
			ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
				if _, err := c.Recv(ctx); err != nil {
					return err
				}
				if err := c.Send(ctx, control.Frame{
					Kind: control.KindStarted, Index: bad, Total: 1,
				}); err != nil {
					return err
				}
				return idleLikeAWorker(ctx, c)
			})
			s := served{ch: ch, runner: &nodeShell{t: t}}
			err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
				filepath.Join(t.TempDir(), "r"), nil)
			if err == nil {
				t.Fatalf("a frame for job %d was accepted", bad)
			}
			if !strings.Contains(err.Error(), "ember never pushed") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// TestGenerate_RefusesALandedFrameThatNamesAPathRatherThanAFile.
//
// What this builds out of the name is a REMOTE path for a tar command, so a
// name with a slash in it is a directory nobody asked for rather than the
// file ember is about to report by that name. transfer.Extract's os.Root
// would hold either way; the refusal is here because the message about a root
// is about the wrong thing.
func TestGenerate_RefusesALandedFrameThatNamesAPathRatherThanAFile(t *testing.T) {
	for _, bad := range []string{"../escape.png", "sub/dir.png", ""} {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
				if _, err := c.Recv(ctx); err != nil {
					return err
				}
				if err := c.Send(ctx, control.Frame{
					Kind: control.KindLanded, Index: 1, Total: 1,
					Dir: "/tmp/ember-results/1", Files: []control.File{{Name: bad, Bytes: 1}},
				}); err != nil {
					return err
				}
				return idleLikeAWorker(ctx, c)
			})
			s := served{ch: ch, runner: &nodeShell{t: t}}
			err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
				filepath.Join(t.TempDir(), "r"), nil)
			if err == nil {
				t.Fatalf("a landed file called %q was accepted", bad)
			}
			if !strings.Contains(err.Error(), "single filename") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// TestGenerate_RefusesALandedFrameWithNothingInIt.
//
// "0 file(s), 0 B" against a job reads as one that finished and produced
// nothing, which is a row an operator would believe.
func TestGenerate_RefusesALandedFrameWithNothingInIt(t *testing.T) {
	for _, f := range []control.Frame{
		{Kind: control.KindLanded, Index: 1, Total: 1, Dir: "/tmp/x"},
		{Kind: control.KindLanded, Index: 1, Total: 1, Files: []control.File{{Name: "a.png"}}},
	} {
		frame := f
		t.Run(fmt.Sprintf("%d file(s), dir %q", len(frame.Files), frame.Dir), func(t *testing.T) {
			ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
				if _, err := c.Recv(ctx); err != nil {
					return err
				}
				if err := c.Send(ctx, frame); err != nil {
					return err
				}
				return idleLikeAWorker(ctx, c)
			})
			s := served{ch: ch, runner: &nodeShell{t: t}}
			if err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
				filepath.Join(t.TempDir(), "r"), nil); err == nil {
				t.Fatalf("%+v was accepted", frame)
			}
		})
	}
}

// TestGenerate_TellsTheWorkerToDropABatchItGaveUpOn.
//
// THE CI FAILURE'S RULE APPLIED TO THE CASES THE WORKER CANNOT SEE. A job that
// failed, it polled -- so it drops the rest of the batch itself. But ember also
// abandons a batch for reasons only ember has: a landed file whose fetch
// failed, a frame it refuses as untrusted, a worker that said something in the
// middle of a batch that does not belong there. In every one of those the
// worker is still working through a queue nobody is reading, on a card that is
// still billing, for a request that has already ended.
//
// The three rows are the three shapes, and the fourth is the control: a batch
// that FINISHED must not be told to drop anything, or every successful request
// would end with ember cancelling a worker that had nothing left to cancel.
func TestGenerate_TellsTheWorkerToDropABatchItGaveUpOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// script is what the worker sends after it has taken the one job.
		script []control.Frame
		want   bool
	}{
		{
			"a landed file ember refuses",
			[]control.Frame{{
				Kind: control.KindLanded, Index: 1, Total: 1,
				Dir: "/tmp/ember-results/1", Files: []control.File{{Name: "../escape.png", Bytes: 1}},
			}},
			true,
		},
		{
			"a frame for a job ember never pushed",
			[]control.Frame{{Kind: control.KindStarted, Index: 9, Total: 1}},
			true,
		},
		{
			"a frame that does not belong in a batch",
			[]control.Frame{{Kind: control.KindPrepared, Index: 1, Total: 1}},
			true,
		},
		{
			"a batch that finished",
			[]control.Frame{
				{Kind: control.KindStarted, Index: 1, Total: 1},
				{Kind: control.KindEnded, Index: 1, Total: 1, Status: sdserver.StatusCompleted.String()},
			},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			told := make(chan control.Kind, 4)
			ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
				if _, err := c.Recv(ctx); err != nil {
					return err
				}
				for _, f := range tc.script {
					if err := c.Send(ctx, f); err != nil {
						return err
					}
				}
				// Reading like a worker, which is what makes this assertion
				// possible at all: a script that stopped reading would make
				// ember's send time out rather than arrive.
				for {
					f, err := c.Recv(ctx)
					if err != nil {
						return nil
					}
					told <- f.Kind
				}
			})
			s := served{ch: ch, runner: &nodeShell{t: t}}

			err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
				filepath.Join(t.TempDir(), "r"), nil)
			if tc.want && err == nil {
				t.Fatal("ember accepted a batch it should have given up on")
			}
			if !tc.want && err != nil {
				t.Fatalf("a batch that finished returned %v", err)
			}

			// The two waits differ because the two claims do. A frame that
			// must arrive gets a bound generous enough not to flake; a frame
			// that must NOT arrive only needs long enough that it would have.
			// ember's send happens in a defer inside pushBatch, so by the
			// time Generate has returned it has either gone or never will --
			// all that is left is the trip down a net.Pipe to a reader that
			// is already waiting.
			settle := 10 * time.Second
			if !tc.want {
				settle = 2 * time.Second
			}
			select {
			case k := <-told:
				if !tc.want {
					t.Errorf("ember sent %q after a batch that finished; there was nothing left to drop", k)
				} else if k != control.KindCancel {
					t.Errorf("ember sent %q, want %q", k, control.KindCancel)
				}
			case <-time.After(settle):
				if tc.want {
					t.Error("ember gave up on the batch and never told the worker, so the jobs " +
						"behind it stay on a card nobody is reading the results of")
				}
			}
		})
	}
}

// TestGenerate_ReportsAWorkerThatStopped.
//
// The worker's own failure reaches ember as a frame rather than as a closed
// connection, so the reason is in the error an operator reads instead of in a
// log on a machine that is about to stop existing.
func TestGenerate_ReportsAWorkerThatStopped(t *testing.T) {
	ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		if _, err := c.Recv(ctx); err != nil {
			return err
		}
		return c.Send(ctx, control.Frame{
			Kind: control.KindFailed, Err: "create this job's results directory: read-only file system",
		})
	})
	s := served{ch: ch, runner: &nodeShell{t: t}}
	err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "read-only file system") {
		t.Errorf("err = %v, want the worker's own explanation", err)
	}
}

// TestGenerate_ReportsAChannelThatDropped, because a rental whose channel
// went away mid-batch has failed and the operator needs the reason -- as
// opposed to one whose worker said goodbye, which has finished.
func TestGenerate_ReportsAChannelThatDropped(t *testing.T) {
	ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		if _, err := c.Recv(ctx); err != nil {
			return err
		}
		c.Abort()
		return nil
	})
	s := served{ch: ch, runner: &nodeShell{t: t}}
	err := s.Generate(t.Context(), report.Discard, testRun(sdserver.Job{Prompt: "a"}),
		filepath.Join(t.TempDir(), "r"), nil)
	if err == nil {
		t.Fatal("a batch whose channel dropped reported success")
	}
	if !strings.Contains(err.Error(), "the worker's events") {
		t.Errorf("err = %v, want it to say reading the worker's events failed", err)
	}
}

// --- the whole rental ---

func TestRunServed_TheWholeLifecycle(t *testing.T) {
	f := newFakeSD(t)
	n := startNode(t, f)
	shell := &nodeShell{t: t}
	spec := servedSpec(t, []string{"sd-server", "--diffusion-model", "/models/x.gguf"})
	dir := filepath.Join(t.TempDir(), "run")

	var out strings.Builder
	err := runServed(t.Context(), &out, report.Text(&out), n, shell, spec,
		testRun(sdserver.Job{Prompt: "a cat", Output: "cat", Sampler: "euler"}),
		session.Options{Log: discardLog()}, dir, node.Rate(2_000_000))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "cat-00.png")); err != nil {
		t.Error(err)
	}

	// EVERY DIAL IS TO ONE ADDRESS NOW, which is the seal this change
	// tightens rather than loosens: direct-tcpip reaches whatever the node
	// can, including a metadata service, and after this there is a single
	// port on the node's own loopback that ember ever names. It used to be
	// sd-server's; the port-forward to it is gone.
	asked := n.dialled()
	if len(asked) == 0 {
		t.Fatal("nothing was dialled")
	}
	want := fmt.Sprintf("127.0.0.1:%d", control.Port)
	for _, a := range asked {
		if a != want {
			t.Errorf("dialled %q, want only %q", a, want)
		}
	}
	if !strings.Contains(out.String(), "starting sd-server") {
		t.Errorf("output = %q", out.String())
	}

	// ember still starts the generation server itself, over `ssh exec`, with
	// argv ember built -- which is what keeps "nothing an operator writes in
	// a config reaches a node as a command" true after the arrival of a
	// daemon on the node: the worker has no argv.
	var startedServer bool
	for _, c := range shell.ran() {
		if strings.Contains(c, "sd-server") {
			startedServer = true
		}
	}
	if !startedServer {
		t.Errorf("the generation server was never started over SSH: %v", shell.ran())
	}
}

// The saving said out loud, and only when there is one: a process per image
// would pay this load again for every job after the first.
func TestRunServed_SaysWhatBeingResidentSaves(t *testing.T) {
	f := newFakeSD(t)
	n := startNode(t, f)
	spec := servedSpec(t, []string{"sd-server"})

	var out strings.Builder
	err := runServed(t.Context(), &out, report.Text(&out), n, &nodeShell{t: t}, spec,
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
	// FLUX.2 model on the first real run while the other arm of the same
	// measurement was spending 80-96s per image re-reading those weights, so
	// the field names something other than the cost of becoming resident.
	//
	// The span is still ember's clock although the wait is now the worker's:
	// it is measured from the moment ember started the process to the moment
	// the worker answered that the weights were loaded, which is the same
	// span as before plus one frame of latency -- and it is still exactly
	// what a fresh sd-cli pays on every invocation.
	if strings.Contains(out.String(), "12.5s") {
		t.Errorf("reported the server's load_elapsed_seconds instead of the measured span: %q", out.String())
	}

	// One job saves nothing, so it says nothing -- AND STILL RECORDS THE
	// FACT, which is the half only a database reader sees.
	//
	// The gate used to be on the emitter, so a batch of one sent no event at
	// all: no load span and no model name reached the row, in the columns #30
	// and #32 added to explain exactly this -- and `ember up` with one job is
	// how most of README's measurements were taken. It is in report.Text now,
	// where a decision about a line of prose belongs. Moving it back makes
	// the second half of this test fail while the first still passes, which
	// is the shape of the bug: the output is identical either way.
	var single strings.Builder
	var events []report.Event
	collect := report.SinkFunc(func(e report.Event) { events = append(events, e) })
	n2 := startNode(t, newFakeSD(t))
	if err := runServed(t.Context(), &single, report.Multi(report.Text(&single), collect),
		n2, &nodeShell{t: t}, spec, testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r2"), 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(single.String(), "paid once rather than") {
		t.Errorf("a single-job run claimed a saving: %q", single.String())
	}
	var resident *report.Event
	for i := range events {
		if events[i].Kind == report.KindModelResident {
			resident = &events[i]
		}
	}
	if resident == nil {
		t.Fatalf("a single-job run emitted no model_resident event, so nothing recorded "+
			"its load span or which model served it: %v", events)
	}
	if resident.Measured <= 0 {
		t.Error("the event carries no measured load span")
	}
	// The name off /capabilities and never off sd.serve: an operator can edit
	// that file between two rentals. It is relayed verbatim by the worker and
	// decoded here, which is the same claim one hop further out.
	if resident.Model != "sd15.gguf" {
		t.Errorf("model = %q, want the name the server reported", resident.Model)
	}
}

// The cheapest refusal in the feature: a mistyped sampler costs the model load
// and nothing else, instead of surfacing at job seven.
func TestRunServed_RefusesEveryBadJobBeforeGeneratingAnything(t *testing.T) {
	f := newFakeSD(t)
	n := startNode(t, f)

	err := runServed(t.Context(), io.Discard, report.Discard, n, &nodeShell{t: t},
		servedSpec(t, []string{"sd-server"}),
		// The bad one is last, which is the point: it must still refuse
		// before the first is pushed.
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
	if f.submits() != 0 {
		t.Errorf("%d job(s) were generated before the refusal", f.submits())
	}
	// A config error, which is what makes internal/cli exit 3. Asserted as
	// the sentinel rather than as the status, because the status is the
	// mapping internal/cli owns and this is the fact it maps.
	if !errors.Is(err, ErrUsage) {
		t.Errorf("err = %v, want it to be an ErrUsage", err)
	}
}

// TestRunServed_RefusesWhenTheWeightsNeverLoad.
//
// The wait is the worker's now, which is why the refusal travels as a frame --
// and what has to survive the move is that ember says the weights never
// loaded rather than something about a channel.
func TestRunServed_RefusesWhenTheWeightsNeverLoad(t *testing.T) {
	f := newFakeSD(t)
	f.modelState = "error"
	n := startNode(t, f)

	err := runServed(t.Context(), io.Discard, report.Discard, n, &nodeShell{t: t},
		servedSpec(t, []string{"sd-server"}), testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "never loaded the weights") {
		t.Errorf("err = %v", err)
	}
	if f.submits() != 0 {
		t.Error("a job was submitted against a server with no weights")
	}
}

// TestRunServed_ReportsANodeWhoseWorkerIsNotThere.
//
// THIS IS WHERE THE SUBJECT MOVED. It used to be "sd-server never opened its
// port", because ember waited for that port itself. It waits for the WORKER
// now, and a worker that is not listening is the one failure that really
// needs a rebuilt node image -- so the message names the worker and points at
// the node's own log, which is the only thing that knows whether it started.
func TestRunServed_ReportsANodeWhoseWorkerIsNotThere(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := runServed(ctx, io.Discard, report.Discard, &refusingDialer{}, &nodeShell{t: t},
		servedSpec(t, []string{"sd-server"}), testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("worker is not listening on 127.0.0.1:%d", control.Port)) {
		t.Errorf("err = %v, want it to name the worker and the address", err)
	}
	// And not blamed on the process: this server was alive the whole time,
	// and ember only stopped it on the way out.
	if strings.Contains(err.Error(), "exited before") {
		t.Errorf("err = %v, blames a cancellation on the server dying", err)
	}
}

func TestRunServed_ReportsAServerItCannotStart(t *testing.T) {
	r := sshx.NewRecordingRunner()
	r.FailOn = "sd-server"

	err := runServed(t.Context(), io.Discard, report.Discard, &refusingDialer{}, r,
		servedSpec(t, []string{"sd-server"}), testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil || !strings.Contains(err.Error(), "start sd-server") {
		t.Fatalf("err = %v", err)
	}
}

// A server that died is noticed at once, not at ready_timeout.
//
// Measured before this existed: sd-server rejected a flag, printed its
// complaint, exited after about a second, and ember polled for the full
// ten-minute default before reporting it. On an A100 at $1.79/hour that is
// thirty cents for a program that was already gone, and the explanation had
// scrolled past ten minutes earlier.
//
// STILL EMBER'S RACE AFTER THE WAIT MOVED, and that is the half worth saying:
// the worker waits for the weights and CANNOT see that the process died,
// because that process is ember's child over `ssh exec`. So orExited is still
// here, racing a frame round trip instead of a poll loop.
func TestRunServed_ADeadServerIsNoticedAtOnce(t *testing.T) {
	f := newFakeSD(t)
	// Loading for ever, so nothing but the process's exit can end the wait.
	f.modelState = "loading"
	n := startNode(t, f)

	// Exactly what a real sd-server did when handed --diffusion-model for a
	// monolithic checkpoint: it printed this and exited.
	shell := &nodeShell{
		t:      t,
		out:    []byte("[E] get sd version from file failed: ''\n[E] new_sd_ctx_t failed\n"),
		exit:   &sshx.ExitError{Cmd: "sd-server", Code: 1},
		noWait: true,
	}

	// A generous timeout: the point is that it is not waited out.
	spec := servedSpecWithTimeout(t, []string{"sd-server", "--diffusion-model", "/models/x.gguf"}, time.Hour)

	start := time.Now()
	var out strings.Builder
	err := runServed(t.Context(), &out, report.Text(&out), n, shell, spec,
		testRun(sdserver.Job{Prompt: "a"}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	took := time.Since(start)

	if err == nil {
		t.Fatal("want an error")
	}
	if took > 30*time.Second {
		t.Errorf("took %v to notice a dead server against a 1h timeout", took)
	}
	if !strings.Contains(err.Error(), "exited before the weights finished loading") {
		t.Errorf("err = %v, want it to say the process ended rather than that the wait timed out", err)
	}
	// The server's own words are what explain it, so they have to have
	// reached the operator.
	if !strings.Contains(out.String(), "get sd version from file failed") {
		t.Errorf("output = %q, want the server's own complaint", out.String())
	}
}

// TestPrepareWorker_RefusesAPreparedFrameWithNoCapabilities.
//
// ember's whole job refusal rests on that document: an empty one would make
// Validate wave every job through, which is "an empty list from the server is
// not a licence to accept anything" reached one layer out -- and the refusal
// would then arrive after a card had been paid to discover it.
func TestPrepareWorker_RefusesAPreparedFrameWithNoCapabilities(t *testing.T) {
	for _, f := range []control.Frame{
		{Kind: control.KindPrepared, Elapsed: time.Second},
		{Kind: control.KindEnded, Index: 1, Total: 1},
	} {
		frame := f
		t.Run(string(frame.Kind), func(t *testing.T) {
			ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
				if _, err := c.Recv(ctx); err != nil {
					return err
				}
				return c.Send(ctx, frame)
			})
			_, _, err := prepareWorker(t.Context(), io.Discard, ch, time.Minute)
			if err == nil {
				t.Fatalf("%+v was accepted", frame)
			}
		})
	}
}

// TestPrepareWorker_WritesTheWeightWaitToTheOperatorsTerminal.
//
// The line used to be printed by ember's own poll loop, on the operator's
// machine. The loop is on the node now, so it travels back as a log frame --
// and without it an operator watches nothing happen for the four minutes a
// 56 GiB model takes, which is the one state where they cannot tell slow from
// stuck.
func TestPrepareWorker_WritesTheWeightWaitToTheOperatorsTerminal(t *testing.T) {
	ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		if _, err := c.Recv(ctx); err != nil {
			return err
		}
		if err := c.Send(ctx, control.Frame{Kind: control.KindLog, Text: "the weights loaded in 12s\n"}); err != nil {
			return err
		}
		return c.Send(ctx, control.Frame{
			Kind: control.KindPrepared, Elapsed: 12 * time.Second,
			Caps: json.RawMessage(`{"model":{"name":"sd15"}}`),
		})
	})
	var out strings.Builder
	load, raw, err := prepareWorker(t.Context(), &out, ch, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if load != 12*time.Second {
		t.Errorf("load = %v", load)
	}
	if !strings.Contains(out.String(), "the weights loaded in 12s") {
		t.Errorf("output = %q, want the node's own line", out.String())
	}
	if !strings.Contains(string(raw), "sd15") {
		t.Errorf("caps = %s", raw)
	}
}

// TestDrainWorker_TellsTheWorkerThereIsNoMoreWorkAndWaitsForItToGo.
//
// "The worker exits when its queue empties" needs both halves and only ember
// holds the second: an empty queue between two requests of one drain loop is
// not the end of a rental, and a worker that exited there would throw away
// the resident weights. So the frame is what ends the worker, and the wait is
// what puts "the queue is empty and ember says there is no more work" in the
// node's own log -- the line that distinguishes a tidy rental from a worker
// that was cut off.
func TestDrainWorker_TellsTheWorkerThereIsNoMoreWorkAndWaitsForItToGo(t *testing.T) {
	got := make(chan control.Kind, 4)
	ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		f, err := c.Recv(ctx)
		if err != nil {
			return err
		}
		got <- f.Kind
		// A late frame from a batch that has already ended, which ember has
		// to read past rather than record: a request's row must not change
		// after it ended.
		if err := c.Send(ctx, control.Frame{Kind: control.KindProgress, Index: 1, Total: 1}); err != nil {
			return err
		}
		return c.Send(ctx, control.Frame{Kind: control.KindDone})
	})

	drainWorker(t.Context(), ch, discardLog())
	select {
	case k := <-got:
		if k != control.KindDrained {
			t.Errorf("the worker was sent %q, want %q", k, control.KindDrained)
		}
	default:
		t.Fatal("the worker was never told the queue was drained, so it would wait for work nobody is going to send")
	}
}

// TestDrainWorker_IsBoundedAgainstAWorkerThatSaysNothing.
//
// It runs on the path where the node is being torn down, so a stalled wait
// here would hold up a destroy -- and a destroy that is held up is a bill
// that keeps running. A WEDGE PRINTS NO `--- FAIL`, so this carries its own
// deadline rather than being left to the package timeout.
func TestDrainWorker_IsBoundedAgainstAWorkerThatSaysNothing(t *testing.T) {
	if workerDrainTimeout <= 0 || workerDrainTimeout > time.Minute {
		t.Errorf("workerDrainTimeout = %v, which is either unbounded or long enough to hold up a destroy",
			workerDrainTimeout)
	}
	ch := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		return idleLikeAWorker(ctx, c)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		drainWorker(ctx, ch, discardLog())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("WEDGED: drainWorker did not return against a worker that says nothing, " +
			"so a destroy would be held up by a goodbye")
	}
}

// --- startServer, which did not move ---

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

// TestWithServer_TheDeferredReaperIsBoundedAgainstAWaitThatNeverReturns.
//
// AGENTS.md names the bare `wait()` in this defer as the worst concurrency bug
// this repository has had -- it wedged for twenty-three minutes after SIGTERM,
// never printing the error it was in the middle of returning, so the run
// looked like a hang with no diagnosis, and it was found with SIGQUIT and a
// goroutine dump because nothing else could have found it. The bound is also
// cited as the precedent for a future OpenTelemetry `Shutdown`. Measured
// before this test existed: deleting the cap outright left
// `go test -race ./internal/app/ ./internal/queue/` at exit 0.
//
// A WEDGE PRINTS NO `--- FAIL` OF ITS OWN. CI greps the unit log for
// `--- SKIP` and reads `go test`'s status, and neither sees a hang until the
// package's own `-timeout` expires -- which prints a goroutine dump rather
// than a failure naming anything. That is why this carries its own deadline
// and its own message instead of being left to the package timeout, and it is
// the rule the browser labs already follow: race the thing you are waiting for
// against the thing that means it will never happen.
func TestWithServer_TheDeferredReaperIsBoundedAgainstAWaitThatNeverReturns(t *testing.T) {
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })

	// Built out here rather than in the goroutine below, because these call
	// t.Fatal and that belongs to the test's own goroutine.
	spec := servedSpec(t, []string{"sd-server"})
	run := testRun(sdserver.Job{Prompt: "a"})
	dir := filepath.Join(t.TempDir(), "r")

	// A dialer that always refuses plus a short deadline, which is the
	// cheapest way to reach the defer: ember gives up on the worker's port
	// and returns, and the reaper is then the only thing left.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = runServed(ctx, io.Discard, report.Discard, &refusingDialer{},
			wedgedServerRunner{released: released}, spec, run,
			session.Options{Log: discardLog()}, dir, 0)
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		// Generous: what is being distinguished is seconds from never.
		if took > 30*time.Second {
			t.Fatalf("runServed returned after %v, want the five-second cap to have released it", took)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("WEDGED: runServed did not return within 40s against a wait() that never arrives, " +
			"so the deferred reaper has no bound -- which is a hang rather than a failure and is " +
			"why this assertion carries its own deadline")
	}
}

// wedgedServerRunner is the one behaviour nodeShell cannot express: a wait
// that never returns at all.
//
// A SECOND FAKE AND NOT A FLAG ON THE FIRST. nodeShell's default is
// `<-ctx.Done()`, which is the tidier behaviour and is correct for every test
// that uses it -- a process whose exit always arrives once ember stops it. The
// bug the five-second cap exists for is the opposite: "cancelling the context
// closes the session, sd-server keeps running, and no exit status ever
// arrives". So every test that drove the deferred reaper handed it an exit it
// was always going to get, and the cap was never the thing that released it.
// A fake tidier than the thing it stands in for tests a program that does not
// exist, which this repository has now paid for six times -- runpodfake's 200
// on a deleted pod, the preview fake that never repeated a frame, the queue
// fake that stamped Seq itself, the cancel fake with no error object, and
// RecordingProvider echoing the injected host key.
//
// `released` is closed by the test's own cleanup rather than being a bare
// `select{}`: it has to never return for as long as the assertion lasts, and
// leaving a goroutine parked for the rest of the test binary buys nothing.
type wedgedServerRunner struct{ released chan struct{} }

func (r wedgedServerRunner) Run(_ context.Context, _ sshx.Command) (io.ReadCloser, func() error, error) {
	return io.NopCloser(bytes.NewReader(nil)), func() error {
		// Deliberately not watching the context: that is the whole point.
		<-r.released
		return nil
	}, nil
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

// --- helpers ---

// recordingSink keeps every event, because an assertion about what a reader
// above this package can join needs the values and not the rendered lines.
type recordingSink struct {
	mu     sync.Mutex
	events []report.Event
}

func (r *recordingSink) Emit(e report.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// submits is how many jobs reached the generation server.
//
// AN ACCESSOR AND NOT THE FIELD, because the worker is a separate goroutine
// from the test that reads it and a cancelled batch is asserted while that
// goroutine is still finishing the job in flight -- which is an unguarded
// read the race detector fails the suite on, correctly.
func (f *fakeSD) submits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submitted)
}

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

// teeSink sends every event to both.
type teeSink struct{ a, b report.Sink }

func (t *teeSink) Emit(e report.Event) {
	t.a.Emit(e)
	t.b.Emit(e)
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
//
// ITS SUBJECT IS THE ROOT FILES AND IS DELIBERATELY NOT WIDENED TO THE TWO
// SHAPES #144 DEMOTED, and that loses nothing measurable. Those two moved
// into docs/configurations.md because neither can be run, and the assertion
// below was already vacuous over both: `ember store sync` on the video one
// fails at ParseAssets over REPLACE_WITH_THE_DIGEST_PUSH_PRINTS, and on the
// code-model one at ValidateImage over an image: that is a sentence with no
// tag -- so each was resolving an image for a sync that could never happen.
// The live subjects are the root files that really can sync. The typo half of
// the check still reaches both blocks, through internal/config's
// TestExampleConfigs_LoadAndParse, which reads every ```yaml fence in docs/.
// The names in the paragraph above are the record of the bug rather than a
// claim about what this glob now sees.
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
