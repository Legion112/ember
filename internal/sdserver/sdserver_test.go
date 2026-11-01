package sdserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Legion112/ember/internal/httpx"
)

// newTestClient points a real Client at an httptest.Server.
//
// BaseURL plus *http.Client is the seam, the same one the RunPod tests use, so
// every layer under test is the production one: the real httpx client, its
// real body-close behaviour, its real error typing. There is no Recording fake
// in this package for that reason -- the external system is an HTTP API, and
// the house pattern for one of those is a server that answers, not a struct
// that pretends to be a client.
func newTestClient(t *testing.T, h http.HandlerFunc) Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return Client{HTTP: httpx.Client{BaseURL: s.URL, HTTP: s.Client()}}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func serveFile(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	raw := fixture(t, name)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}
}

// serveJSON answers one literal body with one status.
func serveJSON(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func TestCapabilities_ReadsTheTrimmedSubset(t *testing.T) {
	c := newTestClient(t, serveFile(t, "capabilities.json"))

	caps, err := c.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Model != "flux2-dev" {
		t.Errorf("model = %q, want flux2-dev", caps.Model)
	}
	if len(caps.Samplers) != 6 || caps.Samplers[0] != "euler" {
		t.Errorf("samplers = %v", caps.Samplers)
	}
	if caps.Limits.MaxWidth != 2048 || caps.Limits.MinWidth != 64 {
		t.Errorf("width limits = %d..%d, want 64..2048", caps.Limits.MinWidth, caps.Limits.MaxWidth)
	}
	if caps.Limits.MaxBatchCount != 8 {
		t.Errorf("max_batch_count = %d, want 8", caps.Limits.MaxBatchCount)
	}
}

func TestCapabilities_ReportsATransportFailure(t *testing.T) {
	c := Client{HTTP: httpx.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}}
	if _, err := c.Capabilities(t.Context()); err == nil {
		t.Fatal("want an error when nothing is listening")
	}
}

// Every method has to report a transport failure as one. The node is at the
// far end of an SSH channel: a dropped connection is the ordinary case, not an
// exotic one, and "the model is not ready" would be the wrong thing to say
// about it.
func TestEveryCall_ReportsATransportFailure(t *testing.T) {
	// Port 1 on loopback: nothing listens there, and the refusal is immediate
	// rather than a timeout, so this costs nothing.
	c := Client{HTTP: httpx.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}}

	if _, err := c.ModelStatus(t.Context()); err == nil {
		t.Error("ModelStatus reported no error")
	}
	if _, err := c.Submit(t.Context(), Job{Prompt: "x"}, PreviewNone, 0); err == nil {
		t.Error("Submit reported no error")
	}
	if _, err := c.Poll(t.Context(), "j"); err == nil {
		t.Error("Poll reported no error")
	}
	if err := c.Cancel(t.Context(), "j"); err == nil {
		t.Error("Cancel reported no error")
	}
}

func TestModelStatus_EveryState(t *testing.T) {
	for _, tc := range []struct {
		file  string
		want  ModelState
		load  time.Duration
		hasEr bool
	}{
		{"model_status_empty.json", ModelEmpty, 0, false},
		{"model_status_loading.json", ModelLoading, 0, false},
		{"model_status_ready.json", ModelReady, 12470 * time.Millisecond, false},
		{"model_status_error.json", ModelError, 310 * time.Millisecond, true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			c := newTestClient(t, serveFile(t, tc.file))
			st, err := c.ModelStatus(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.want {
				t.Errorf("state = %v, want %v", st.State, tc.want)
			}
			if st.LoadElapsed != tc.load {
				t.Errorf("load elapsed = %v, want %v", st.LoadElapsed, tc.load)
			}
			if (st.Err != "") != tc.hasEr {
				t.Errorf("err = %q, want set=%v", st.Err, tc.hasEr)
			}
		})
	}
}

// A state ember has never heard of is an error at the boundary, not a zero
// value that flows onward. The alternative is WaitModel treating "whatever
// that was" as "not ready yet" and burning its whole timeout on it.
func TestModelStatus_RefusesAnUnknownState(t *testing.T) {
	c := newTestClient(t, serveJSON(200, `{"state":"warming-up"}`))
	_, err := c.ModelStatus(t.Context())
	if err == nil || !strings.Contains(err.Error(), "warming-up") {
		t.Fatalf("err = %v, want it to name the unknown state", err)
	}
}

func TestSubmit_SendsTheBodyAndReadsTheID(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != pathImageGen {
			t.Errorf("path = %s, want %s", r.URL.Path, pathImageGen)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "job_accepted.json"))
	})

	id, err := c.Submit(t.Context(), Job{Prompt: "a cat", Width: 512, Steps: 20, Sampler: "euler"}, PreviewProj, 5)
	if err != nil {
		t.Fatal(err)
	}
	if id != "job_7f3a" {
		t.Errorf("id = %q, want job_7f3a", id)
	}
	if got["prompt"] != "a cat" {
		t.Errorf("prompt = %v", got["prompt"])
	}
	sp, ok := got["sample_params"].(map[string]any)
	if !ok {
		t.Fatalf("sample_params missing: %v", got)
	}
	if sp["sample_method"] != "euler" {
		t.Errorf("sample_method = %v, want euler", sp["sample_method"])
	}
	if got["preview"] != PreviewProj {
		t.Errorf("preview = %v, want %s", got["preview"], PreviewProj)
	}
}

// A job that ran on a card ember paid for and whose id never came back cannot
// be polled, cancelled or collected: it generates, bills, and is purged.
func TestSubmit_RefusesAnAcceptanceWithNoID(t *testing.T) {
	c := newTestClient(t, serveJSON(202, `{"kind":"img_gen","status":"queued"}`))
	_, err := c.Submit(t.Context(), Job{Prompt: "x"}, PreviewNone, 0)
	if err == nil || !strings.Contains(err.Error(), "without returning an id") {
		t.Fatalf("err = %v, want a refusal naming the missing id", err)
	}
}

func TestSubmit_ReportsABadJob(t *testing.T) {
	c := newTestClient(t, serveJSON(202, `{"id":"x"}`))
	_, err := c.Submit(t.Context(), Job{Prompt: "x", Extra: map[string]any{"preview": "vae"}}, PreviewProj, 5)
	if err == nil || !strings.Contains(err.Error(), "preview") {
		t.Fatalf("err = %v, want the body builder's refusal", err)
	}
}

func TestPoll_EveryStatus(t *testing.T) {
	for _, tc := range []struct {
		file string
		want Status
	}{
		{"job_queued.json", StatusQueued},
		{"job_generating.json", StatusGenerating},
		{"job_completed.json", StatusCompleted},
		{"job_failed.json", StatusFailed},
	} {
		t.Run(tc.file, func(t *testing.T) {
			c := newTestClient(t, serveFile(t, tc.file))
			st, err := c.Poll(t.Context(), "job_7f3a")
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != tc.want {
				t.Errorf("status = %v, want %v", st.Status, tc.want)
			}
		})
	}
}

func TestPoll_DecodesThePreviewFrameAndItsPass(t *testing.T) {
	c := newTestClient(t, serveFile(t, "job_generating.json"))
	st, err := c.Poll(t.Context(), "job_7f3a")
	if err != nil {
		t.Fatal(err)
	}
	if st.Preview == nil {
		t.Fatal("no preview")
	}
	// The pass is carried because step/total_steps describe the pass and not
	// the job: without it, a bar runs to 28/28 and then starts again at 1.
	if st.Preview.Pass != 1 || st.Preview.Step != 14 || st.Preview.TotalSteps != 28 {
		t.Errorf("preview = %+v, want pass 1 step 14/28", *st.Preview)
	}
	if string(st.Preview.PNG) != "fake-preview-frame" {
		t.Errorf("frame = %q, want the decoded bytes", st.Preview.PNG)
	}
}

func TestPoll_DecodesTheFinishedImage(t *testing.T) {
	c := newTestClient(t, serveFile(t, "job_completed.json"))
	st, err := c.Poll(t.Context(), "job_7f3a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(st.Images))
	}
	if string(st.Images[0].Data) != "fake-png-bytes" {
		t.Errorf("image = %q, want the decoded bytes", st.Images[0].Data)
	}
}

// The object form of `error`. The string form is covered by
// TestDecodeAPIError_ReadsBothShapesSDServerUses.
func TestPoll_ReportsAFailedJobsOwnReason(t *testing.T) {
	c := newTestClient(t, serveFile(t, "job_failed.json"))
	st, err := c.Poll(t.Context(), "job_7f3a")
	if err != nil {
		t.Fatal(err) // a failed job is a successful request
	}
	if st.Err == nil {
		t.Fatal("no error on a failed job")
	}
	if st.Err.Code != "invalid_request" || !strings.Contains(st.Err.Message, "multiple of 16") {
		t.Errorf("err = %+v", *st.Err)
	}
}

func TestPoll_RefusesAnUnknownStatus(t *testing.T) {
	c := newTestClient(t, serveJSON(200, `{"id":"j","status":"thinking"}`))
	_, err := c.Poll(t.Context(), "j")
	if err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("err = %v, want it to name the unknown status", err)
	}
}

func TestPoll_ReportsUndecodableImageBytes(t *testing.T) {
	c := newTestClient(t, serveJSON(200,
		`{"id":"j","status":"completed","result":{"images":[{"index":0,"b64_json":"not base64 !!"}]}}`))
	_, err := c.Poll(t.Context(), "j")
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("err = %v, want a decode failure", err)
	}
}

func TestPoll_ReportsAnUndecodablePreview(t *testing.T) {
	c := newTestClient(t, serveJSON(200,
		`{"id":"j","status":"generating","preview":{"pass":1,"step":1,"total_steps":4,"b64_json":"%%%"}}`))
	_, err := c.Poll(t.Context(), "j")
	if err == nil || !strings.Contains(err.Error(), "preview frame") {
		t.Fatalf("err = %v, want the preview named", err)
	}
}

func TestPollAndCancel_RefuseAnEmptyID(t *testing.T) {
	c := Client{}
	if _, err := c.Poll(t.Context(), ""); err == nil {
		t.Error("Poll accepted an empty id")
	}
	if err := c.Cancel(t.Context(), ""); err == nil {
		t.Error("Cancel accepted an empty id")
	}
}

func TestCancel_PostsToTheJob(t *testing.T) {
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{}`)
	})
	if err := c.Cancel(t.Context(), "job_7f3a"); err != nil {
		t.Fatal(err)
	}
	if want := pathJobs + "job_7f3a/cancel"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestCancel_ReportsAFailure(t *testing.T) {
	c := newTestClient(t, serveJSON(409, `{"error":"already finished","code":"job_done"}`))
	if err := c.Cancel(t.Context(), "j"); err == nil {
		t.Fatal("want an error")
	}
}

// decodeB64 accepts a data URL because api.md promises one on the request side
// and does not say which the response uses. Guessing and being wrong writes a
// file whose first bytes are a MIME prefix.
func TestDecodeB64_AcceptsADataURL(t *testing.T) {
	got, err := decodeB64("data:image/png;base64,ZmFrZS1wbmctYnl0ZXM=")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fake-png-bytes" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeAPIError_ReadsBothShapesSDServerUses(t *testing.T) {
	// The whole reason this function exists: one API, two JSON types for one
	// field name. A client written against either alone reports a JSON
	// complaint in place of the reason the other endpoint gave.
	var env wireErrorEnvelope
	if err := json.Unmarshal(fixture(t, "error_model_not_loaded.json"), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error != "No model is loaded" || env.Code != "model_not_loaded" {
		t.Fatalf("envelope = %+v", env)
	}

	for _, tc := range []struct {
		name, raw, want string
	}{
		{"object", `{"code":"c","message":"m"}`, "c: m"},
		{"object with only a message", `{"message":"m"}`, "m"},
		{"object with only a code", `{"code":"c"}`, "c"},
		{"string", `"No model is loaded"`, "No model is loaded"},
		{"neither shape", `[1,2]`, "[1,2]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := decodeAPIError(json.RawMessage(tc.raw))
			if e == nil {
				t.Fatal("want an error")
			}
			if e.Error() != tc.want {
				t.Errorf("Error() = %q, want %q", e.Error(), tc.want)
			}
		})
	}

	// All three spellings of "nothing went wrong" appear in real responses.
	for _, raw := range []string{"", "null", `""`, `"  "`} {
		if e := decodeAPIError(json.RawMessage(raw)); e != nil {
			t.Errorf("decodeAPIError(%q) = %v, want nil", raw, e)
		}
	}

	if got := (&APIError{}).Error(); got == "" {
		t.Error("an empty APIError must still say something")
	}
}

func TestStatusAndModelState_RoundTripAndDone(t *testing.T) {
	for s, want := range map[Status]bool{
		StatusQueued: false, StatusGenerating: false,
		StatusCompleted: true, StatusFailed: true, StatusCancelled: true,
	} {
		if s.Done() != want {
			t.Errorf("%v.Done() = %v, want %v", s, s.Done(), want)
		}
	}
	for name, s := range statuses {
		if s.String() != name {
			t.Errorf("%d.String() = %q, want %q", int(s), s.String(), name)
		}
	}
	for name, m := range modelStates {
		if m.String() != name {
			t.Errorf("%d.String() = %q, want %q", int(m), m.String(), name)
		}
	}
	if got := Status(99).String(); !strings.Contains(got, "99") {
		t.Errorf("Status(99) = %q, want it to name the number", got)
	}
	if got := ModelState(99).String(); !strings.Contains(got, "99") {
		t.Errorf("ModelState(99) = %q, want it to name the number", got)
	}
}

func TestBody_MergesExtraIntoTheNestedObjects(t *testing.T) {
	j := Job{
		Prompt: "a cat", Width: 512, Height: 512, Steps: 20, Seed: 7,
		BatchCount: 2, NegativePrompt: "blurry",
		Sampler: "euler", Scheduler: "karras", CFG: 1, Guidance: 4,
		// eta lives inside sample_params and distilled guidance inside
		// sample_params.guidance -- both beside keys ember set. A merge that
		// replaced instead of merging would drop sample_method.
		Extra: map[string]any{
			"sample_params": map[string]any{
				"eta":      0.3,
				"guidance": map[string]any{"img_cfg": 2.5},
			},
			"output_format": "png",
		},
	}
	body, err := j.Body(PreviewProj, 5)
	if err != nil {
		t.Fatal(err)
	}
	if body["output_format"] != "png" {
		t.Errorf("top-level extra lost: %v", body)
	}
	sp := body["sample_params"].(map[string]any)
	if sp["sample_method"] != "euler" || sp["scheduler"] != "karras" || sp["sample_steps"] != 20 {
		t.Errorf("ember's own sample_params keys lost: %v", sp)
	}
	if sp["eta"] != 0.3 {
		t.Errorf("nested extra lost: %v", sp)
	}
	g := sp["guidance"].(map[string]any)
	if g["txt_cfg"] != 1.0 || g["distilled_guidance"] != 4.0 {
		t.Errorf("guidance lost ember's keys: %v", g)
	}
	if g["img_cfg"] != 2.5 {
		t.Errorf("guidance lost the extra key: %v", g)
	}
}

// A collision is a refusal and not a precedence rule: a seed or a step count
// resolved the wrong way makes two runs that claim to be the same measurement
// produce different images.
func TestBody_RefusesExtraThatShadowsATypedField(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  Job
		want string
	}{
		{"top level", Job{Prompt: "a", Extra: map[string]any{"prompt": "b"}}, "extra sets prompt"},
		{"nested", Job{Prompt: "a", Steps: 4, Extra: map[string]any{
			"sample_params": map[string]any{"sample_steps": 9},
		}}, "sample_params.sample_steps"},
		{"twice nested", Job{Prompt: "a", CFG: 1, Extra: map[string]any{
			"sample_params": map[string]any{"guidance": map[string]any{"txt_cfg": 9}},
		}}, "sample_params.guidance.txt_cfg"},
		// ember owns the progress stream because it owns the poll cadence, so
		// these are always set and therefore always a collision.
		{"preview", Job{Prompt: "a", Extra: map[string]any{"preview": "vae"}}, "extra sets preview"},
		{"preview interval", Job{Prompt: "a", Extra: map[string]any{"preview_interval": 1}}, "preview_interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.job.Body(PreviewProj, 5)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// A field the typed struct did not set is one extra may set freely. That is
// what keeps every one of the API's forty fields reachable from a config
// without ember mirroring the schema.
func TestBody_ExtraReachesWhatTheTypedFieldsDoNotSet(t *testing.T) {
	body, err := Job{Extra: map[string]any{"prompt": "from extra", "width": 768}}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if body["prompt"] != "from extra" || body["width"] != 768 {
		t.Errorf("body = %v", body)
	}
	if _, ok := body["sample_params"]; ok {
		t.Errorf("an empty sample_params was sent: %v", body)
	}
}

// Sorted, so a config with two collisions names the same one every run.
func TestBody_ReportsTheSameCollisionEveryTime(t *testing.T) {
	j := Job{Prompt: "a", Width: 1, Extra: map[string]any{"width": 2, "prompt": "b"}}
	for i := 0; i < 20; i++ {
		_, err := j.Body(PreviewNone, 0)
		if err == nil {
			t.Fatal("want a refusal")
		}
		if !strings.Contains(err.Error(), "extra sets prompt") {
			t.Fatalf("err = %v, want the first key in sorted order", err)
		}
	}
}

// A non-map extra value over a key whose destination is a map must not panic.
func TestBody_ExtraScalarOverAnObjectKey(t *testing.T) {
	body, err := Job{Prompt: "a", Extra: map[string]any{"hires": "yes"}}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if body["hires"] != "yes" {
		t.Errorf("body = %v", body)
	}
	// And the reverse: an object where nothing exists yet.
	body, err = Job{Prompt: "a", Extra: map[string]any{"hires": map[string]any{"enabled": true}}}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := body["hires"].(map[string]any); !ok || h["enabled"] != true {
		t.Errorf("body = %v", body)
	}
}

// scriptedStatus answers a prepared sequence of model statuses, in process.
//
// Needed for the same reason gpucheck has scriptedRunner rather than reusing
// the shared recording fake: what matters here is a sequence over time, and
// the shared seam cannot express "loading, loading, then ready". In process
// because these tests run inside a synctest bubble, where a goroutine blocked
// on a real socket stops the fake clock advancing and the test hangs until the
// package timeout.
//
// The last entry repeats forever, so a one-element script means "always this".
type scriptedStatus struct {
	states []ModelStatus
	errs   []error
	polls  int
}

func (s *scriptedStatus) ModelStatus(context.Context) (ModelStatus, error) {
	i := s.polls
	s.polls++
	if i < len(s.errs) && s.errs[i] != nil {
		return ModelStatus{}, s.errs[i]
	}
	if len(s.states) == 0 {
		return ModelStatus{}, errors.New("no scripted state")
	}
	if i >= len(s.states) {
		i = len(s.states) - 1
	}
	return s.states[i], nil
}

var (
	loadingState = ModelStatus{State: ModelLoading}
	readyState   = ModelStatus{State: ModelReady, LoadElapsed: 12470 * time.Millisecond}
)

func TestWaitModel_ReadyFirstTimeSaysNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scriptedStatus{states: []ModelStatus{readyState}}
		var out strings.Builder
		load, err := WaitModel(t.Context(), s, time.Minute, &out, nil)
		if err != nil {
			t.Fatal(err)
		}
		if load != 12470*time.Millisecond {
			t.Errorf("load = %v, want the server's own figure", load)
		}
		// A line on every run trains the operator to ignore it.
		if out.String() != "" {
			t.Errorf("printed %q on a first-poll success", out.String())
		}
	})
}

func TestWaitModel_RetriesWhileLoading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scriptedStatus{states: []ModelStatus{loadingState, loadingState, readyState}}
		var out strings.Builder
		if _, err := WaitModel(t.Context(), s, time.Minute, &out, nil); err != nil {
			t.Fatal(err)
		}
		if s.polls != 3 {
			t.Errorf("polled %d times, want 3", s.polls)
		}
		if !strings.Contains(out.String(), "weights loaded in") {
			t.Errorf("printed %q, want the one line a slow load earns", out.String())
		}
		// The server's own figure is reported beside ember's, because ember's
		// also contains the SSH round trips.
		if !strings.Contains(out.String(), "12.47s") {
			t.Errorf("printed %q, want the server's reported load time", out.String())
		}
	})
}

// A load that failed has already read what it was going to read. Retrying
// costs the whole timeout on a card that will never generate anything.
func TestWaitModel_RefusesAnErrorStateWithoutWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scriptedStatus{states: []ModelStatus{{
			State: ModelError,
			Err:   "failed to open /models/flux2-dev-Q8_0.gguf: No such file or directory",
		}}}
		start := time.Now()
		_, err := WaitModel(t.Context(), s, time.Hour, io.Discard, nil)

		var nr *NotReadyError
		if !errors.As(err, &nr) {
			t.Fatalf("err = %v, want a *NotReadyError", err)
		}
		if nr.Polls != 1 {
			t.Errorf("polled %d times, want 1", nr.Polls)
		}
		if !strings.Contains(nr.Message, "No such file") {
			t.Errorf("message = %q, want the server's own reason", nr.Message)
		}
		if waited := time.Since(start); waited > time.Second {
			t.Errorf("waited %v on a failed load", waited)
		}
		if !strings.Contains(nr.Error(), "cost the wait and no more") {
			t.Errorf("Error() = %q", nr.Error())
		}
		if !strings.Contains(nr.Error(), "sd.serve") {
			t.Errorf("Error() = %q, want the remediation to name the config key", nr.Error())
		}
	})
}

// A status ember cannot read is not a model that failed: sd-server answers
// before it has finished reading the weights, and that window looks like this.
func TestWaitModel_RetriesAnUnreadableStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scriptedStatus{
			states: []ModelStatus{readyState},
			errs:   []error{errors.New("connection refused"), errors.New("503")},
		}
		if _, err := WaitModel(t.Context(), s, time.Minute, io.Discard, nil); err != nil {
			t.Fatal(err)
		}
		if s.polls != 3 {
			t.Errorf("polled %d times, want 3", s.polls)
		}
	})
}

// An unreadable status at the deadline must still name something useful.
func TestWaitModel_ReportsTheLastTransportFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scriptedStatus{states: []ModelStatus{loadingState}, errs: []error{
			errors.New("connection refused"), errors.New("connection refused"),
			errors.New("connection refused"), errors.New("connection refused"),
			errors.New("connection refused"), errors.New("connection refused"),
		}}
		_, err := WaitModel(t.Context(), s, 4*time.Second, io.Discard, nil)
		var nr *NotReadyError
		if !errors.As(err, &nr) {
			t.Fatalf("err = %v, want a *NotReadyError", err)
		}
		if !strings.Contains(nr.Message, "connection refused") {
			t.Errorf("message = %q, want the last thing that went wrong", nr.Message)
		}
	})
}

func TestWaitModel_GivesUpAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scriptedStatus{states: []ModelStatus{loadingState}}
		start := time.Now()
		_, err := WaitModel(t.Context(), s, 7*time.Second, io.Discard, nil)

		var nr *NotReadyError
		if !errors.As(err, &nr) {
			t.Fatalf("err = %v, want a *NotReadyError", err)
		}
		if nr.Last != ModelLoading {
			t.Errorf("last state = %v, want loading", nr.Last)
		}
		// 0s, 2s, 4s, 6s, then a 1s clamped sleep to 7s: four polls and then
		// the refusal, with no overshoot past the budget the operator is
		// paying for.
		if nr.Polls != 5 {
			t.Errorf("polled %d times, want 5 at 0,2,4,6,7s", nr.Polls)
		}
		if waited := time.Since(start); waited > 7*time.Second {
			t.Errorf("waited %v, want no more than the 7s budget", waited)
		}
	})
}

func TestWaitModel_ZeroAndNegativeTimeoutsUseTheDefault(t *testing.T) {
	for _, tmo := range []time.Duration{0, -time.Minute} {
		t.Run(tmo.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &scriptedStatus{states: []ModelStatus{loadingState}}
				start := time.Now()
				if _, err := WaitModel(t.Context(), s, tmo, io.Discard, nil); err == nil {
					t.Fatal("want a refusal")
				}
				if waited := time.Since(start); waited < DefaultModelTimeout {
					t.Errorf("waited %v for a %v timeout, want at least the %v default",
						waited, tmo, DefaultModelTimeout)
				}
			})
		})
	}
}

func TestWaitModel_StopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scriptedStatus{states: []ModelStatus{loadingState}}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := WaitModel(ctx, s, time.Hour, io.Discard, nil)
			done <- err
		}()
		time.Sleep(3 * time.Second)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled; Ctrl-C must not wait out the timeout", err)
		}
	})
}

func TestWaitModel_ToleratesANilWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Two polls, so the printing path is the one taken.
		s := &scriptedStatus{states: []ModelStatus{loadingState, readyState}}
		if _, err := WaitModel(t.Context(), s, time.Minute, nil, nil); err != nil {
			t.Fatal(err)
		}
	})
}

// The production Client satisfies the seam WaitModel takes.
func TestClient_IsAModelStatuser(t *testing.T) {
	var _ ModelStatuser = Client{}
}

func TestValidate_AcceptsAJobTheServerCanRun(t *testing.T) {
	caps := testCaps(t)
	j := Job{Prompt: "a cat", Width: 512, Height: 512, Steps: 20, BatchCount: 2,
		Sampler: "euler", Scheduler: "karras"}
	if err := Validate(j, caps); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_RefusesBeforeAnyGPUTimeIsSpent(t *testing.T) {
	caps := testCaps(t)
	for _, tc := range []struct {
		name string
		job  Job
		want string
	}{
		{"no prompt", Job{Width: 512}, "no prompt"},
		{"bad sampler", Job{Prompt: "a", Sampler: "eulerr"}, `sampler "eulerr" is not one`},
		{"bad scheduler", Job{Prompt: "a", Scheduler: "karas"}, `scheduler "karas" is not one`},
		{"width too small", Job{Prompt: "a", Width: 32}, "below this server's minimum"},
		{"width too large", Job{Prompt: "a", Width: 4096}, "above this server's maximum"},
		{"height too large", Job{Prompt: "a", Height: 4096}, "above this server's maximum"},
		{"negative width", Job{Prompt: "a", Width: -1}, "negative"},
		{"negative height", Job{Prompt: "a", Height: -1}, "negative"},
		{"negative steps", Job{Prompt: "a", Steps: -1}, "negative"},
		{"batch too large", Job{Prompt: "a", BatchCount: 99}, "above this server's maximum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.job, caps)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A prompt supplied through extra is still a prompt.
func TestValidate_AcceptsAPromptFromExtra(t *testing.T) {
	if err := Validate(Job{Extra: map[string]any{"prompt": "a cat"}}, testCaps(t)); err != nil {
		t.Fatal(err)
	}
}

// An empty list is "this build did not report it", not "anything goes". Saying
// so beats silently accepting a sampler nothing will recognise.
func TestValidate_SaysSoWhenTheServerReportedNoList(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  Job
	}{
		{"sampler", Job{Prompt: "a", Sampler: "euler"}},
		{"scheduler", Job{Prompt: "a", Scheduler: "karras"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.job, Capabilities{})
			if err == nil || !strings.Contains(err.Error(), "cannot be checked") {
				t.Fatalf("err = %v, want it to say the list was not reported", err)
			}
		})
	}
}

// A zero maximum is "not reported" and not "nothing is allowed". A server that
// published no limits must not make every job refuse.
func TestValidate_UnreportedLimitsBoundNothing(t *testing.T) {
	j := Job{Prompt: "a", Width: 99999, Height: 99999, BatchCount: 99999}
	if err := Validate(j, Capabilities{}); err != nil {
		t.Fatalf("err = %v, want unreported limits to bound nothing", err)
	}
}

func testCaps(t *testing.T) Capabilities {
	t.Helper()
	var w wireCapabilities
	if err := json.Unmarshal(fixture(t, "capabilities.json"), &w); err != nil {
		t.Fatal(err)
	}
	return mapCapabilities(w)
}

func TestWriteImages_WritesEveryImageNumbered(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	got, err := WriteImages(root, "station", []Image{
		{Index: 0, Data: []byte("one")},
		{Index: 1, Data: []byte("two!")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Files != 2 || got.Bytes != 7 {
		t.Errorf("written = %+v, want 2 files and 7 bytes", got)
	}
	// Numbered even when there is one, so a reader of the directory needs one
	// case and not two.
	for name, want := range map[string]string{"station-00.png": "one", "station-01.png": "two!"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != want {
			t.Errorf("%s = %q, want %q", name, raw, want)
		}
	}
}

func TestWriteImages_NothingToWriteIsNoWork(t *testing.T) {
	got, err := WriteImages(nil, "x", nil)
	if err != nil || got.Files != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestWriteImages_RefusesAStemThatIsNotAName(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	for _, stem := range []string{"", "   ", "..", "/", "../.."} {
		if _, err := WriteImages(root, stem, []Image{{Data: []byte("x")}}); err == nil {
			t.Errorf("WriteImages accepted stem %q", stem)
		}
	}
}

func TestWriteImages_ReportsAFileItCannotCreate(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	// A directory where the file wants to be.
	if err := os.Mkdir(filepath.Join(dir, "x-00.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteImages(root, "x", []Image{{Index: 0, Data: []byte("x")}}); err == nil {
		t.Fatal("want an error when the name is taken by a directory")
	}
}

// A stem is a filename, not a path. Flattening first means the error names the
// real problem rather than complaining about a root.
func TestSafeStem_FlattensAPathIntoAName(t *testing.T) {
	for in, want := range map[string]string{
		"station":          "station",
		"a/b/c":            "c",
		"../../etc/passwd": "passwd",
		"with space":       "with-space",
		"semi;colon&amp":   "semi-colon-amp",
		"  padded  ":       "padded",
		".hidden":          "hidden",
		"keep-_.chars":     "keep-_.chars",
		`back\slash\name`:  "name",
		"unicode-é":        "unicode--",
	} {
		if got := safeStem(in); got != want {
			t.Errorf("safeStem(%q) = %q, want %q", in, got, want)
		}
	}
}

// EMBER OWNS THE LISTEN ADDRESS. A serve command choosing its own would put an
// unauthenticated image generator on a rented machine's public interface.
func TestServeCommand_AppendsTheLoopbackAddress(t *testing.T) {
	got, err := ServeCommand([]string{"sd-server", "--diffusion-model", "/models/x.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("--listen-ip 127.0.0.1 --listen-port %d", Port)
	if !strings.Contains(strings.Join(got, " "), want) {
		t.Errorf("got %v, want it to end with %s", got, want)
	}
	if got[0] != "sd-server" || got[2] != "/models/x.gguf" {
		t.Errorf("the operator's own argv was disturbed: %v", got)
	}
}

func TestServeCommand_RefusesAnAddressOfItsOwn(t *testing.T) {
	for _, argv := range [][]string{
		{"sd-server", "--listen-ip", "0.0.0.0"},
		{"sd-server", "--listen-ip=0.0.0.0"},
		{"sd-server", "--listen-port", "8188"},
		{"sd-server", "--listen-port=8188"},
	} {
		_, err := ServeCommand(argv)
		if err == nil {
			t.Fatalf("ServeCommand accepted %v", argv)
		}
		if !strings.Contains(err.Error(), "public interface") {
			t.Errorf("err = %v, want it to say why", err)
		}
	}
}

func TestServeCommand_EmptyIsNotAServe(t *testing.T) {
	got, err := ServeCommand(nil)
	if err != nil || got != nil {
		t.Fatalf("got %v, %v, want nil, nil", got, err)
	}
}

func TestNew_BuildsALoopbackClientThatDialsThroughTheTunnel(t *testing.T) {
	// The production constructor: no local listener, a direct-tcpip dial to
	// the node's own loopback. Here the "tunnel" is a dialer that records the
	// address it was asked for and connects to a local test server instead.
	s := httptest.NewServer(serveFile(t, "capabilities.json"))
	defer s.Close()

	var asked string
	c := New(func(ctx context.Context, network, addr string) (net.Conn, error) {
		asked = addr
		var d net.Dialer
		return d.DialContext(ctx, network, strings.TrimPrefix(s.URL, "http://"))
	})
	if _, err := c.Capabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("127.0.0.1:%d", Port); asked != want {
		t.Errorf("dialled %q, want %q", asked, want)
	}
}
