package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/control/controltest"
	"github.com/Legion112/ember/internal/httpx"
	"github.com/Legion112/ember/internal/sdserver"
)

// afterShort and afterLong are the two waits every test here uses.
//
// EVERY WAITING TEST CARRIES ITS OWN DEADLINE, because a wedge prints no
// --- FAIL: a test that hangs is a suite with no exit code, which is worse
// than a red one. Short is "long enough that a thing which was going to
// happen has"; long is the bound on a thing that must happen.
func afterShort() <-chan time.Time { return time.After(50 * time.Millisecond) }
func afterLong() <-chan time.Time  { return time.After(10 * time.Second) }

// started runs a worker session over a real control channel and returns
// ember's end of it.
//
// THE REAL CHANNEL AND THE REAL CLIENT. The handshake, the codec and the read
// limits are control's own over net.Pipe; the generation server is an
// httptest.Server, for the reason internal/sdserver's own tests use one -- the
// external system is an HTTP API, so what is driven here is the production
// client with its real decoding and its real error typing.
func started(t *testing.T, o Options) (*control.Conn, <-chan error) {
	t.Helper()
	if o.Results == "" {
		o.Results = t.TempDir()
	}
	if o.Log == nil {
		o.Log = quiet()
	}
	done := make(chan error, 1)
	ember := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		err := Session(ctx, c, o)
		done <- err
		return err
	})
	return ember, done
}

// TestSession_ExitsWhenItsQueueIsEmptyAndEmberSaysSo.
//
// TODAY'S RULE RELOCATED, not a new one: internal/queue's drain returns when
// nothing compatible is left, "no idle timeout, because a node with nothing to
// do is a node billing for nothing". The worker's version of that sentence
// needs ember's half, because the queue being empty is not the same fact as
// there being no more work -- see TestQueue_AnEmptyQueueIsNotTheEndOfTheWork.
func TestSession_ExitsWhenItsQueueIsEmptyAndEmberSaysSo(t *testing.T) {
	ember, done := started(t, Options{})

	if err := ember.Send(t.Context(), control.Frame{Kind: control.KindDrained}); err != nil {
		t.Fatal(err)
	}

	f, err := ember.Recv(t.Context())
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Kind != control.KindDone {
		t.Errorf("the worker's last frame is %q, want %q", f.Kind, control.KindDone)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a worker that drained its queue exited with %v", err)
		}
	case <-afterLong():
		t.Fatal("the worker did not exit after being drained")
	}
}

// TestSession_DoesNotExitWhileEmberHasSaidNothing.
//
// The other half of the rule, and the expensive one: a worker that exited on
// an empty queue would end the rental between two requests of one drain loop
// and throw away the resident weights.
func TestSession_DoesNotExitWhileEmberHasSaidNothing(t *testing.T) {
	_, done := started(t, Options{})
	select {
	case err := <-done:
		t.Fatalf("the worker exited on an empty queue nobody drained: %v", err)
	case <-afterShort():
	}
}

// TestSession_StopsWhenEmberHangsUp.
//
// "A node ember cannot reach is a node ember cannot destroy, so there is no
// value in a workload outliving the connection." This is what makes a laptop
// that went to sleep mid-batch end the worker without waiting out the TTL.
func TestSession_StopsWhenEmberHangsUp(t *testing.T) {
	ember, done := started(t, Options{})
	if err := ember.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a tidy goodbye ended the worker with %v, which would read as a failed rental", err)
		}
	case <-afterLong():
		t.Fatal("the worker did not notice ember hanging up")
	}
}

// TestSession_RefusesAFrameItDoesNotKnow.
//
// Named rather than ignored. The version handshake is what should make this
// unreachable, so arriving here means the two ends disagree about what a
// version means -- and a frame silently dropped is a job ember waits for for
// ever.
func TestSession_RefusesAFrameItDoesNotKnow(t *testing.T) {
	ember, done := started(t, Options{})

	// A frame only the worker ever sends, sent the wrong way.
	if err := ember.Send(t.Context(), control.Frame{Kind: control.KindLanded, Index: 1, Total: 1}); err != nil {
		t.Fatal(err)
	}
	f, err := ember.Recv(t.Context())
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Kind != control.KindFailed || !strings.Contains(f.Err, "landed") {
		t.Errorf("got %+v, want a failure naming the frame", f)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("the worker carried on after a frame it could not act on")
		}
	case <-afterLong():
		t.Fatal("the worker did not stop")
	}
}

// TestSession_RefusesToRunWithNowhereToWrite.
//
// A worker with no results directory would generate on a paid card and then
// have nothing to do with the bytes. Refused before the channel carries any
// work rather than at the first landed file.
func TestSession_RefusesToRunWithNowhereToWrite(t *testing.T) {
	_, done := started(t, Options{Results: "-"})
	// "-" is a directory, so reach the refusal by the one that is not.
	_ = done

	err := Session(t.Context(), nil, Options{Log: quiet()})
	if err == nil {
		t.Fatal("a worker with no results directory was started")
	}
	if !strings.Contains(err.Error(), "results directory") {
		t.Errorf("err = %v", err)
	}
}

// TestSession_PreparesTheGenerationServerAndReportsWhatItCanDo.
//
// EMBER STARTED THAT SERVER, over `ssh exec`, with argv ember built; the
// worker waits for it. What comes back is the DECODED value and not the
// server's own document, because the generation server is on this node's
// loopback and the worker is the one thing that can read it -- so sdserver's
// mapper runs once, here, rather than at both ends. It is that mapper which
// learned `capabilities.model` is an object and not the string api.md's field
// list implies, and a second run of it at ember's end would be a second place
// for that to be wrong.
func TestSession_PreparesTheGenerationServerAndReportsWhatItCanDo(t *testing.T) {
	const capsDoc = `{"samplers":["euler"],"model":{"name":"sd15"},"features":{"cancel_generating":true}}`
	gen := fakeGen(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sdcpp/v1/models/status":
			_, _ = fmt.Fprint(w, `{"state":"ready","load_elapsed_seconds":12.5}`)
		case "/sdcpp/v1/capabilities":
			_, _ = fmt.Fprint(w, capsDoc)
		default:
			t.Errorf("the worker asked for %s", r.URL.Path)
		}
	})

	ember, _ := started(t, Options{Gen: gen})
	if err := ember.Send(t.Context(), control.Frame{Kind: control.KindPrepare, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	f, err := ember.Recv(t.Context())
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Kind != control.KindPrepared {
		t.Fatalf("got %+v", f)
	}
	if f.Elapsed != 12500*time.Millisecond {
		t.Errorf("Elapsed = %v, want the server's own 12.5s", f.Elapsed)
	}
	// What ember reads off the frame: the model name out of the model
	// OBJECT, the server's own sampler list, and the one feature flag the
	// worker itself acts on.
	var parsed sdserver.Capabilities
	if err := json.Unmarshal(f.Caps, &parsed); err != nil {
		t.Fatalf("decode what the worker sent: %v", err)
	}
	if parsed.Model != "sd15" || !parsed.CancelGenerating {
		t.Errorf("parsed = %+v", parsed)
	}
	if len(parsed.Samplers) != 1 || parsed.Samplers[0] != "euler" {
		t.Errorf("samplers = %v, want the server's own list", parsed.Samplers)
	}
}

// TestSession_WhatTravelsSurvivesTheRoundTripExACTLY.
//
// THE ASYMMETRY BETWEEN THE TWO BIG FIELDS IS A DECISION, and this is the
// measurement behind it. sdserver.Capabilities is strings, numbers, bools and
// *bool with no map[string]any anywhere, so marshalling it and reading it back
// is lossless -- including the nil-versus-false distinction in Supported,
// where nil is "this build published no such flag" and false is "the server
// says no", and collapsing them would grey a control against every build that
// publishes nothing.
//
// A JOB'S BODY IS NOT LIKE THAT, which is why it crosses as bytes nobody
// parses. The second half here is the proof: the same round trip through
// map[string]any -- which is what a decoded-and-re-encoded body would be --
// changes a number. A seed is an int64 and `extra:` can hold anything an
// operator typed.
func TestSession_WhatTravelsSurvivesTheRoundTripExactly(t *testing.T) {
	no, yes := false, true
	in := sdserver.Capabilities{
		Model: "sd15.gguf", Samplers: []string{"euler", "heun"},
		Schedulers: []string{"karras"}, PreviewModes: []string{"none", "proj"},
		Limits:        sdserver.Limits{MinWidth: 64, MaxWidth: 4096, MaxBatchCount: 8},
		OutputFormats: []string{"png"}, Upscalers: []string{"realesrgan"},
		CancelGenerating: true,
		// One of each: published-and-no, published-and-yes, and not
		// published at all.
		Supported: sdserver.Supported{ClipSkip: &no, InitImage: &yes},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out sdserver.Capabilities
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("capabilities changed in transit:\n got %+v\nwant %+v", out, in)
	}
	if out.Supported.FlowShift != nil {
		t.Error("a flag the server never published came back as one it did")
	}
	if out.Supported.ClipSkip == nil || *out.Supported.ClipSkip {
		t.Error("a published false came back as nil or true")
	}

	// And the number a body would have lost. 2^53+1 is the first integer a
	// float64 cannot hold, and a seed is an int64.
	const big = 9007199254740993
	var viaMap map[string]any
	if err := json.Unmarshal([]byte(`{"seed":9007199254740993}`), &viaMap); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(viaMap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(again), fmt.Sprint(big)) {
		t.Errorf("a body through map[string]any kept %d, so the reason Submit takes raw bytes "+
			"no longer holds and this test is the thing that is wrong: %s", big, again)
	}
}

// TestSession_SendsTheWeightWaitBackAsItHappens.
//
// The line sdserver.WaitModel prints when the load took more than one poll
// used to go straight to the operator's terminal, because the loop was on
// their machine. It is on the node now, so it has to travel back or an
// operator watches nothing happen for the four minutes a 56 GiB model takes --
// the one state where slow and stuck look the same.
//
// TWO POLLS IS THE WHOLE SETUP, because the line is only printed when the
// first poll did not find the weights ready: a fake that answered "ready"
// immediately would leave this path untested while every assertion about
// `prepared` still passed.
func TestSession_SendsTheWeightWaitBackAsItHappens(t *testing.T) {
	var polls int
	gen := fakeGen(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sdcpp/v1/models/status":
			polls++
			if polls < 2 {
				_, _ = fmt.Fprint(w, `{"state":"loading"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"state":"ready","load_elapsed_seconds":2}`)
		default:
			_, _ = fmt.Fprint(w, `{"model":{"name":"sd15"}}`)
		}
	})

	ember, _ := started(t, Options{Gen: gen})
	if err := ember.Send(t.Context(), control.Frame{Kind: control.KindPrepare, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	var sawLog bool
	for {
		f, err := ember.Recv(t.Context())
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch f.Kind {
		case control.KindLog:
			sawLog = true
			if !strings.Contains(f.Text, "the weights loaded in") {
				t.Errorf("the log frame says %q", f.Text)
			}
		case control.KindPrepared:
			if !sawLog {
				t.Error("the weights took two polls and nothing about the wait reached ember, " +
					"so an operator sees a silent terminal for as long as the model takes")
			}
			return
		default:
			t.Fatalf("got %+v", f)
		}
	}
}

// TestSession_ReportsAGenerationServerThatNeverLoadsItsWeights.
//
// The worker is the only thing that can see this, and a wait that failed
// silently would be ember holding a card against a server that will never
// answer.
func TestSession_ReportsAGenerationServerThatNeverLoadsItsWeights(t *testing.T) {
	gen := fakeGen(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state":"error","error":"could not read /opt/models/sd15.gguf"}`)
	})
	ember, done := started(t, Options{Gen: gen})
	if err := ember.Send(t.Context(), control.Frame{Kind: control.KindPrepare, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	f, err := ember.Recv(t.Context())
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Kind != control.KindFailed || !strings.Contains(f.Err, "sd15.gguf") {
		t.Errorf("got %+v, want a failure carrying the server's own explanation", f)
	}
	select {
	case err := <-done:
		// Returned as well as reported: the frame is for ember and the
		// return is what the node's own log records, which is the copy that
		// survives the node.
		if err == nil {
			t.Error("the worker exited cleanly after a server that will never be ready")
		}
	case <-afterLong():
		t.Fatal("the worker did not stop after a server that will never be ready")
	}
}

// TestSession_WritesEachJobIntoItsOwnDirectoryAndNamesWhatLanded.
//
// A FRESH DIRECTORY PER JOB, and the defect it prevents is the worst of the
// three possible ones: a rental serves several requests, two of them can name
// the same output, and without this ember's fetch of `boat-00.png` could bring
// home the previous request's picture -- a result that is plausible, paid for,
// and wrong.
//
// The names travel on the landed frame rather than being derived twice:
// sdserver.safeStem decides what a file is called, and a second spelling of
// that rule is a gallery pointing at filenames that are not on disk.
func TestSession_WritesEachJobIntoItsOwnDirectoryAndNamesWhatLanded(t *testing.T) {
	results := t.TempDir()
	gen := fakeGen(t, genScript(t))

	ember, _ := started(t, Options{Gen: gen, Results: results})
	push(t, ember, 1, 2, "boat")
	push(t, ember, 2, 2, "boat")

	dirs := map[string]bool{}
	landed := 0
	for landed < 2 {
		f, err := ember.Recv(t.Context())
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if f.Kind != control.KindLanded {
			continue
		}
		landed++
		if len(f.Files) != 1 || f.Files[0].Name != "boat-00.png" {
			t.Fatalf("landed %+v", f.Files)
		}
		if dirs[f.Dir] {
			t.Errorf("two jobs named the same output and landed in one directory: %s", f.Dir)
		}
		dirs[f.Dir] = true
		if !strings.HasPrefix(f.Dir, results) {
			t.Errorf("Dir = %q, which is not under the results directory %q", f.Dir, results)
		}
		on, err := os.ReadFile(filepath.Join(f.Dir, f.Files[0].Name)) //nolint:gosec // a path this test made
		if err != nil {
			t.Fatalf("the worker named a file it did not write: %v", err)
		}
		if f.Files[0].Bytes != int64(len(on)) {
			t.Errorf("landed says %d bytes and the file is %d", f.Files[0].Bytes, len(on))
		}
	}
}

// TestSession_NumbersAJobThatNamesNothing, so a request that names no outputs
// still produces files an operator can tell apart.
func TestSession_NumbersAJobThatNamesNothing(t *testing.T) {
	gen := fakeGen(t, genScript(t))
	ember, _ := started(t, Options{Gen: gen})
	push(t, ember, 2, 3, "")

	for {
		f, err := ember.Recv(t.Context())
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if f.Kind != control.KindLanded {
			continue
		}
		if len(f.Files) != 1 || f.Files[0].Name != "002-00.png" {
			t.Fatalf("landed %+v, want the job's own one-based position", f.Files)
		}
		return
	}
}

// TestSession_AFailedSubmissionStopsTheWorker.
//
// NOT RETRIED, anywhere: a transport failure on a submission is ambiguous --
// the request may have reached the server and created a job whose id nobody
// learned -- and sending it again would generate the image twice and bill for
// both, with the second one invisible.
func TestSession_AFailedSubmissionStopsTheWorker(t *testing.T) {
	gen := fakeGen(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"code":"model_not_loaded","error":"No model is loaded"}`)
	})
	ember, done := started(t, Options{Gen: gen})
	push(t, ember, 1, 1, "boat")

	f, err := ember.Recv(t.Context())
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Kind != control.KindFailed || !strings.Contains(f.Err, "No model is loaded") {
		t.Errorf("got %+v, want a failure carrying the server's own sentence", f)
	}
	// And the index travels with it, so ember can put its own label on the
	// sentence: a failure that could not name which job it was about is the
	// message an operator reads off a batch of eight.
	if f.Index != 1 || f.Total != 1 {
		t.Errorf("the failure names job %d of %d, want 1 of 1", f.Index, f.Total)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("the worker exited cleanly after a submission the server refused")
		}
	case <-afterLong():
		t.Fatal("the worker did not stop")
	}
}

// TestSession_RelaysTheJobBodyWithoutParsingIt.
//
// THE SEED IS WHY. A body relayed through map[string]any comes back with every
// number as a float64, and a seed above 2^53 is then re-encoded as a different
// number -- so the node would generate a picture ember did not ask for while
// recording the seed it did. The assertion is on the bytes sd-server received.
func TestSession_RelaysTheJobBodyWithoutParsingIt(t *testing.T) {
	const body = `{"prompt":"a paper boat","seed":9007199254740993,"sample_steps":6}`

	got := make(chan string, 1)
	gen := fakeGen(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sdcpp/v1/img_gen" {
			b := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(b)
			got <- string(b)
			_, _ = fmt.Fprint(w, `{"id":"j1"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"j1","status":"completed","images":[]}`)
	})
	ember, _ := started(t, Options{Gen: gen})
	if err := ember.Send(t.Context(), control.Frame{
		Kind: control.KindJob, Index: 1, Total: 1, Stem: "boat",
		Job: json.RawMessage(body),
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case sent := <-got:
		if sent != body {
			t.Errorf("sd-server received\n %s\nwant\n %s", sent, body)
		}
	case <-afterLong():
		t.Fatal("nothing was submitted")
	}
}

// push sends one job frame with a body the fake accepts.
func push(t *testing.T, ember *control.Conn, index, total int, stem string) {
	t.Helper()
	if err := ember.Send(t.Context(), control.Frame{
		Kind: control.KindJob, Index: index, Total: total, Stem: stem,
		Job: json.RawMessage(`{"prompt":"a paper boat"}`),
	}); err != nil {
		t.Fatal(err)
	}
}

// fakeGen is a generation server good enough to drive the worker.
//
// An httptest.Server rather than a hand-written client fake, for the reason
// internal/sdserver's own tests use one: the external system is an HTTP API,
// so the layer under test should be the production client, its real decoding
// and its real error typing.
func fakeGen(t *testing.T, h http.HandlerFunc) sdserver.Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return sdserver.Client{HTTP: httpx.Client{BaseURL: s.URL, HTTP: s.Client()}}
}

// genScript is one job that is generating with a preview and then completes.
//
// The polls are counted under a mutex because the worker submits the next job
// while the httptest server is still serving the last poll of the previous
// one, and a counter raced is a fake that answers a completed job to a poll
// about a job that has not started.
func genScript(t *testing.T) http.HandlerFunc {
	t.Helper()
	var (
		mu    sync.Mutex
		polls int
	)
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/sdcpp/v1/img_gen":
			polls = 0
			_, _ = fmt.Fprint(w, `{"id":"j1","status":"queued"}`)
		case strings.HasPrefix(r.URL.Path, "/sdcpp/v1/jobs/"):
			polls++
			if polls < 2 {
				_, _ = fmt.Fprint(w, `{"id":"j1","status":"generating","preview":{"pass":1,"step":1,"total_steps":6,"b64_json":"cHJldmlldw=="}}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"id":"j1","status":"completed","result":{"images":[{"index":0,"b64_json":"Z2VuZXJhdGVk"}]}}`)
		default:
			t.Errorf("the worker asked for %s", r.URL.Path)
		}
	}
}
