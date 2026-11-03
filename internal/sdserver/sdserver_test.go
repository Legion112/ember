package sdserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	// The fixture is a captured response, so these are the server's own
	// values. `model` is an object there and not the string api.md's field
	// list implies; ember reads its name.
	if caps.Model != "sd15.gguf" {
		t.Errorf("model = %q, want the name out of the model object", caps.Model)
	}
	if len(caps.Samplers) < 10 || caps.Samplers[0] != "euler" {
		t.Errorf("samplers = %v", caps.Samplers)
	}
	if !slices.Contains(caps.Schedulers, "karras") {
		t.Errorf("schedulers = %v, want karras among them", caps.Schedulers)
	}
	if !slices.Contains(caps.PreviewModes, PreviewProj) {
		t.Errorf("preview modes = %v, want %s among them", caps.PreviewModes, PreviewProj)
	}
	if caps.Limits.MaxWidth != 4096 || caps.Limits.MinWidth != 64 {
		t.Errorf("width limits = %d..%d, want 64..4096", caps.Limits.MinWidth, caps.Limits.MaxWidth)
	}
	if caps.Limits.MaxBatchCount != 8 {
		t.Errorf("max_batch_count = %d, want 8", caps.Limits.MaxBatchCount)
	}
	// features.cancel_generating, out of the same captured response. It is
	// the only feature flag ember acts on, and it is the only one that says
	// what may be asked of a generation that is already billing rather than
	// what a request body may carry.
	if !caps.CancelGenerating {
		t.Error("cancel_generating = false, and the captured response says true")
	}
}

// A build that does not report the flag reads as false, which is the
// expensive answer and therefore the one to be sure of.
//
// Every other list here refuses a job when it is absent, because a refusal
// before a card is rented costs nothing. This one is read while a rented card
// is sampling, so false must mean "do not ask the server" and not "do not
// cancel" -- see Capabilities.CancelGenerating and app.runJobs.
func TestCapabilities_AnUnreportedCancelFlagIsFalse(t *testing.T) {
	c := newTestClient(t, serveJSON(200, `{"model":{"name":"m.gguf"},"samplers":["euler"]}`))
	caps, err := c.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if caps.CancelGenerating {
		t.Error("a response with no features object reported cancel_generating")
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
		// Captured: a server up with no weights. Its load_elapsed_seconds is
		// JSON **null** where the guessed fixture said 0 -- which decodes
		// into a float64 as a no-op rather than an error, so the guess passed
		// for a reason that had nothing to do with being right. The same
		// null-is-not-zero asymmetry that took a websocket envelope down one
		// layer up, where serde's `default` covers a missing field and not a
		// present null.
		{"model_status_empty.json", ModelEmpty, 0, false},
		{"model_status_loading.json", ModelLoading, 0, false},
		// Captured: a 1.76 GB sd15 checkpoint off a warm page cache.
		{"model_status_ready.json", ModelReady, 248 * time.Millisecond, false},
		// Captured: `models/load` handed a file that is not a model. 1.5ms,
		// because the loader gives up as soon as it cannot read a version out
		// of the header -- nothing like the third of a second the guess
		// invented, and the reason the figure is pinned is that it is the one
		// an operator reads beside "never loaded the weights".
		{"model_status_error.json", ModelError, 2 * time.Millisecond, true},
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
		{"job_cancelled.json", StatusCancelled},
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
	// the job: without it, a bar runs to 6/6 and then starts again at 1.
	//
	// The numbers are a real server's: the fixture was captured from
	// sd-server generating a 64x64 image in six steps with preview_interval
	// at 1, which is the first frame of the first pass.
	if st.Preview.Pass != 1 || st.Preview.Step != 1 || st.Preview.TotalSteps != 6 {
		t.Errorf("preview = %+v, want pass 1 step 1/6", *st.Preview)
	}

	// A real PNG, not bytes. The previous fixture carried "fake-preview-frame"
	// and this asserted the string back, which proves base64 decoding and
	// nothing about what a frame is.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(st.Preview.PNG))
	if err != nil {
		t.Fatalf("the frame does not decode: %v", err)
	}

	// Eight by eight for a 64x64 image: `proj` is a projection of the latent
	// and the latent is one eighth of the image per side. Worth pinning,
	// because anyone reading "preview frame" expects a thumbnail of the
	// output and would size a UI for one.
	if format != "png" || cfg.Width != 8 || cfg.Height != 8 {
		t.Errorf("frame = %s %dx%d, want png 8x8 (the latent, not the image)", format, cfg.Width, cfg.Height)
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
	// The captured fixture's image is the 64x64 PNG that run produced, so
	// this decodes rather than comparing a placeholder string to itself.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(st.Images[0].Data))
	if err != nil {
		t.Fatalf("the finished image does not decode: %v", err)
	}
	if format != "png" || cfg.Width != 64 || cfg.Height != 64 {
		t.Errorf("image = %s %dx%d, want png 64x64", format, cfg.Width, cfg.Height)
	}
}

// The object form of `error`, from a captured failure. The string form is
// covered by TestSubmit_ReportsTheServersOwnRefusal, which reads it on the
// path an operator meets it rather than by unmarshalling a fixture.
//
// Both halves of the guess this fixture replaced were invented: the code
// `invalid_request` appears nowhere in the server binary, and the message
// described a width check it does not perform -- it rounds a width it does not
// like. What a real failed job says is `generation_failed` and
// `generate_image returned no results`, which is deliberately generic: the
// reason is in the server's own log, and ember surfaces that through KindLog.
func TestPoll_ReportsAFailedJobsOwnReason(t *testing.T) {
	c := newTestClient(t, serveFile(t, "job_failed.json"))
	st, err := c.Poll(t.Context(), "job_7f3a")
	if err != nil {
		t.Fatal(err) // a failed job is a successful request
	}
	if st.Err == nil {
		t.Fatal("no error on a failed job")
	}
	if st.Err.Code != "generation_failed" || !strings.Contains(st.Err.Message, "no results") {
		t.Errorf("err = %+v", *st.Err)
	}
	// Beside the error object, and this is the combination no guess had: a
	// failed job carries `result: null` and no images. The captured cancel
	// carries the same-shaped error object beside a status that is *not* a
	// failure, which is why the status is read first and the error second --
	// reading the error first made every successful cancel look like a crash.
	if len(st.Images) != 0 {
		t.Errorf("a failed job yielded %d image(s)", len(st.Images))
	}
}

// TestPoll_ALoraThatNeverLoadedReadsAsACleanSuccess is the finding of #79
// stated at the layer that would have to act on it.
//
// The fixture is a real completed job whose request named a LoRA sd-server
// published and could not read. Everything this package decodes from it says
// the generation went fine -- `StatusCompleted`, no error, one image that is
// a real PNG -- and the preview lab has measured that the pixels are the
// no-LoRA pixels exactly. So the assertion is that ember's own view carries no
// signal, which is why #79's option 1 is not built: there is nothing here to
// branch on. The server's account is two `[W]` lines on its stderr, which
// reach the operator through KindLog and are recorded nowhere.
//
// It is NOT a test of agreement with itself. A fixture decoded by hand proves
// the shape is readable and nothing about the service; what makes this one
// evidence is that it is bytes a server sent, captured by
// TestPreview_ALoraThatDidNotLoadIsNowhereInTheResponse, which asserts against
// the live exchange that this is still what one looks like. The day that lab
// goes red because the response grew a load status, this test is the one that
// changes.
func TestPoll_ALoraThatNeverLoadedReadsAsACleanSuccess(t *testing.T) {
	c := newTestClient(t, serveFile(t, "job_lora_not_loaded_completed.json"))
	st, err := c.Poll(t.Context(), "job_7f3a")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusCompleted {
		t.Errorf("status = %v, want completed", st.Status)
	}
	if st.Err != nil {
		t.Errorf("err = %+v, want none: a registration failure that reached this field would "+
			"be the machine-readable signal #79 asked for, and ember could refuse the "+
			"generation instead of reporting it", *st.Err)
	}
	if len(st.Images) != 1 {
		t.Fatalf("images = %d, want 1 -- the generation was paid for in full", len(st.Images))
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(st.Images[0].Data))
	if err != nil {
		t.Fatalf("the image does not decode: %v", err)
	}
	if format != "png" || cfg.Width != 64 || cfg.Height != 64 {
		t.Errorf("image = %s %dx%d, want png 64x64", format, cfg.Width, cfg.Height)
	}

	// And the one place the response does mention it, which is the request
	// echoed back. ember reads none of `generation_info`, so this is asserted
	// against the raw bytes: the point is that the only carrier of the LoRA's
	// name states it was used, so a reader of the response would conclude the
	// opposite of what happened.
	var env struct {
		Result struct {
			Images []struct {
				GenerationInfo struct {
					Loras []struct {
						Path string `json:"path"`
					} `json:"loras"`
				} `json:"generation_info"`
			} `json:"images"`
		} `json:"result"`
	}
	if err := json.Unmarshal(fixture(t, "job_lora_not_loaded_completed.json"), &env); err != nil {
		t.Fatal(err)
	}
	loras := env.Result.Images[0].GenerationInfo.Loras
	if len(loras) != 1 || loras[0].Path != "unloadable.safetensors" {
		t.Errorf("generation_info.loras = %+v, want the request's own entry. An empty list "+
			"would be the report #79 wanted", loras)
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

// The string form, on the path an operator meets it: a 503 whose body is
// sd-server's own refusal.
//
// WHAT THIS FIXES. wireErrorEnvelope was typed, fixtured and decoded by
// nothing but a test that unmarshalled the fixture into it -- so a submission
// to a server with no weights reached the operator as httpx's raw body, with
// the reason sitting inside a sentence about a status code. The body here is
// the captured one, byte for byte, which is also the only reason this test is
// evidence rather than ember agreeing with itself.
//
// The guess held, and that is worth recording too: api.md's field list
// implied exactly `{"error":"No model is loaded","code":"model_not_loaded"}`
// and a real 503 says the same thing. A convention that only ever reports its
// own vindication stops being read, and so does one that only reports
// corrections.
func TestSubmit_ReportsTheServersOwnRefusal(t *testing.T) {
	body := fixture(t, "error_model_not_loaded.json")
	c := newTestClient(t, serveJSON(http.StatusServiceUnavailable, string(body)))

	_, err := c.Submit(t.Context(), Job{Prompt: "a paper boat"}, PreviewNone, 0)
	if err == nil {
		t.Fatal("want an error")
	}
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("the refusal does not reach the caller as an *APIError: %v", err)
	}
	if api.Code != "model_not_loaded" || api.Message != "No model is loaded" {
		t.Errorf("refusal = %+v", *api)
	}
	// The reason leads, because that is what the operator does something
	// about. The status code is kept because 503 and 4xx mean different
	// next moves.
	if !strings.Contains(err.Error(), "No model is loaded") || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %q, want the server's sentence and the status", err)
	}
	// The response is still reachable, so nothing that wants the code or the
	// raw body has lost it.
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Errorf("the status error is no longer in the chain: %v", err)
	}
}

// Every method translates it, because every one of them can be refused: a
// capability read before the weights are in, a poll of a job the server has
// purged, a cancel for one that finished. A method that forgot would print
// JSON while its neighbours printed a reason.
func TestEveryCall_ReportsTheServersOwnRefusal(t *testing.T) {
	c := newTestClient(t, serveJSON(http.StatusServiceUnavailable,
		`{"code":"model_not_loaded","error":"No model is loaded"}`))

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Capabilities", func() error { _, err := c.Capabilities(t.Context()); return err }},
		{"ModelStatus", func() error { _, err := c.ModelStatus(t.Context()); return err }},
		{"Submit", func() error { _, err := c.Submit(t.Context(), Job{Prompt: "x"}, PreviewNone, 0); return err }},
		{"Poll", func() error { _, err := c.Poll(t.Context(), "j"); return err }},
		{"Cancel", func() error { return c.Cancel(t.Context(), "j") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var api *APIError
			if err := tc.call(); !errors.As(err, &api) {
				t.Errorf("err = %v, want the server's own refusal", err)
			}
		})
	}
}

// A non-2xx that is not the envelope is handed back exactly as httpx built it.
//
// The body is then the only text that says what happened -- an HTML error page
// from something in front of the server, a bare 502, a truncated reply -- and
// replacing it with a guessed sentence would lose it. That is the same failure
// this translation exists to end, reached from the other direction.
func TestRefusal_LeavesEverythingElseAlone(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"not JSON at all", "<html>502 Bad Gateway</html>", 502},
		{"JSON that is not the envelope", `{"detail":"nope"}`, 400},
		{"an envelope with nothing in it", `{"error":"","code":""}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, serveJSON(tc.code, tc.body))
			err := c.Cancel(t.Context(), "j")
			var api *APIError
			if errors.As(err, &api) {
				t.Fatalf("invented a refusal from %q: %+v", tc.body, *api)
			}
			if err == nil || !strings.Contains(err.Error(), tc.body) {
				t.Errorf("err = %v, want httpx's own message carrying the body", err)
			}
		})
	}

	// A transport failure is not a refusal either: nothing was answered, so
	// there is no envelope to read and no status to report.
	c := Client{HTTP: httpx.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}}
	err := c.Cancel(t.Context(), "j")
	var api *APIError
	if err == nil || errors.As(err, &api) {
		t.Errorf("err = %v, want a transport failure and not a refusal", err)
	}
}

func TestDecodeAPIError_ReadsBothShapesSDServerUses(t *testing.T) {
	// The whole reason this function exists: one API, two JSON types for one
	// field name. A client written against either alone reports a JSON
	// complaint in place of the reason the other endpoint gave.
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
		{"width too large", Job{Prompt: "a", Width: 8192}, "above this server's maximum"},
		{"height too large", Job{Prompt: "a", Height: 8192}, "above this server's maximum"},
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

// The two feature maps, out of the two captured responses that disagree.
//
// `capabilities.json` is a server with a command-line model, where every
// generation feature is true; `capabilities_no_model.json` is a server with no
// weights, where every one of them is false AND the per-mode `features` map is
// absent entirely -- so `hires` there is nil rather than false. One assertion
// over both files, because what has to hold is that ember tells three states
// apart, and a test against either file alone would pass against code that
// collapsed them.
func TestCapabilities_ReadsBothFeatureMaps(t *testing.T) {
	loaded := testCaps(t)
	for _, f := range []struct {
		name string
		flag *bool
	}{
		{"clip_skip", loaded.Supported.ClipSkip},
		{"flow_shift", loaded.Supported.FlowShift},
		{"shifted_timestep", loaded.Supported.ShiftedTimestep},
		{"slg", loaded.Supported.SLG},
		{"vae_tiling", loaded.Supported.VAETiling},
		{"hires", loaded.Supported.Hires},
	} {
		if f.flag == nil || !*f.flag {
			t.Errorf("%s = %v on a server with a model loaded, and the captured response "+
				"says true", f.name, f.flag)
		}
	}

	empty := capsFrom(t, "capabilities_no_model.json")
	for _, f := range []struct {
		name string
		flag *bool
	}{
		{"clip_skip", empty.Supported.ClipSkip},
		{"flow_shift", empty.Supported.FlowShift},
		{"shifted_timestep", empty.Supported.ShiftedTimestep},
		{"slg", empty.Supported.SLG},
		{"vae_tiling", empty.Supported.VAETiling},
	} {
		if f.flag == nil {
			t.Errorf("%s is absent on a server with no model, and the captured response "+
				"publishes it as false", f.name)
			continue
		}
		if *f.flag {
			t.Errorf("%s = true on a server with no model loaded", f.name)
		}
	}
	// And the silence beside the denials, in the same response: with no model
	// there are no supported modes, so the per-mode map is never built and
	// `features` carries only the two cancel flags.
	if empty.Supported.Hires != nil {
		t.Errorf("hires = %v on a server with no model, want absent", *empty.Supported.Hires)
	}
}

// A parameter the server said no to is refused before a card is rented.
//
// WHAT HAPPENS WITHOUT IT is not a 400 -- it is a generation that runs and
// quietly drops the parameter, which reads as a model that ignores clip skip.
// That is the same defect as a skip-layer block sent with its scale at zero,
// one layer out, and it costs a rental to see.
func TestValidate_RefusesAParameterTheServerDenies(t *testing.T) {
	caps := capsFrom(t, "capabilities_no_model.json")
	// Hires is not in this table: the no-model response does not publish the
	// flag at all, and the test below is the one about that.
	for _, tc := range []struct {
		name  string
		param Params
		flag  string
	}{
		{"clip_skip", Params{ClipSkip: intPtr(2)}, "generation_features.clip_skip"},
		{"flow_shift", Params{FlowShift: floatPtr(3)}, "generation_features.flow_shift"},
		{"shifted_timestep", Params{ShiftedTimestep: 250}, "generation_features.shifted_timestep"},
		{"slg", Params{SLG: SLG{Scale: 2.5}}, "generation_features.slg"},
		{"vae_tiling", Params{VAETiling: VAETiling{Enabled: true}}, "generation_features.vae_tiling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Job{Prompt: "a", Params: tc.param}, caps)
			if err == nil {
				t.Fatalf("%s was accepted against a server that published it false", tc.name)
			}
			// The flag's own key is in the message, because the remediation is
			// to change the model and not the request -- and because the two
			// maps disagree, so an operator checking for themselves has to be
			// told which one said no.
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("err = %v, want it to name %s", err, tc.flag)
			}
		})
	}
}

// The gate only refuses what Body would have sent.
//
// A zero shifted_timestep, an slg block with no scale and a hires block that
// is off are all omitted from the request already, so refusing them would
// refuse a job that asks for nothing -- and for skip-layer guidance the scale
// IS the switch, because two ways to say "off" can disagree. This is the pair
// to the table above: the same all-denying capabilities, and nothing set.
func TestValidate_ADeniedFeatureRefusesNothingThatIsNotSent(t *testing.T) {
	caps := capsFrom(t, "capabilities_no_model.json")
	for _, tc := range []struct {
		name  string
		param Params
	}{
		{"nothing set", Params{}},
		{"a zero shifted_timestep", Params{ShiftedTimestep: 0}},
		{"slg layers with no scale", Params{SLG: SLG{Layers: []int{7, 8, 9}, LayerStart: 0.01}}},
		{"a hires block that is off", Params{Hires: Hires{Scale: 2}}},
		{"vae tiling tiles with the switch off", Params{VAETiling: VAETiling{TileSizeW: 512}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(Job{Prompt: "a", Params: tc.param}, caps); err != nil {
				t.Errorf("err = %v, want a parameter that is never sent to be refused by nothing", err)
			}
		})
	}
}

// A flag the server did not publish is not a refusal.
//
// The opposite of how the lists above read an absent one, deliberately: an
// empty sampler list refuses because a name has to match something, while a
// missing boolean means some build -- or some `caps` row cached before ember
// read these maps -- simply has nothing to say. Collapsing them would make
// every one of these parameters unusable against such a server, which is the
// shape of the bug where an old cached row decoded to empty lists and refused
// a correct output_format.
func TestValidate_AnUnpublishedFeatureFlagGatesNothing(t *testing.T) {
	silent := Capabilities{}
	j := Job{Prompt: "a", Params: Params{
		ClipSkip:        intPtr(2),
		FlowShift:       floatPtr(3),
		ShiftedTimestep: 250,
		SLG:             SLG{Scale: 2.5},
		VAETiling:       VAETiling{Enabled: true},
		Hires:           Hires{Enabled: true},
	}}
	if err := Validate(j, silent); err != nil {
		t.Fatalf("err = %v, want a server that published no feature map to gate nothing", err)
	}
	// And the same job against the captured no-model response, where the hires
	// flag is the only absent one: the five published denials fire and hires
	// is not the reason.
	err := Validate(j, capsFrom(t, "capabilities_no_model.json"))
	if err == nil {
		t.Fatal("the published denials refused nothing")
	}
	if strings.Contains(err.Error(), "features.hires") {
		t.Errorf("err = %v, want the refusal to come from a flag the server published", err)
	}
}

// A refusal ember has now seen a third spelling of.
//
// `error_lora_not_found.json` is what a real server answers to a `lora[].path`
// that is not one of its published ids: an `error` string with **no `code`
// beside it**. decodeAPIError and refusal both read the string form, and
// APIError.Error has a branch for a message with no code -- which until this
// capture was a branch written for a shape nobody had observed.
func TestAPIError_AMessageWithNoCodeIsStillTheReason(t *testing.T) {
	var env wireErrorEnvelope
	if err := json.Unmarshal(fixture(t, "error_lora_not_found.json"), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == "" {
		t.Fatal("the captured refusal carries no error string")
	}
	if env.Code != "" {
		t.Errorf("code = %q; the capture had none, so the no-code branch is covered by "+
			"nothing again", env.Code)
	}
	got := (&APIError{Code: env.Code, Message: env.Error}).Error()
	if got != env.Error {
		t.Errorf("Error() = %q, want the server's own sentence %q", got, env.Error)
	}
}

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

// capsFrom maps one captured capabilities response the way the client does.
func capsFrom(t *testing.T, name string) Capabilities {
	t.Helper()
	var w wireCapabilities
	if err := json.Unmarshal(fixture(t, name), &w); err != nil {
		t.Fatal(err)
	}
	return mapCapabilities(w)
}

func testCaps(t *testing.T) Capabilities {
	t.Helper()
	return capsFrom(t, "capabilities.json")
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

// Empty is ember's default and not upstream's. An unlisted value refuses
// before anything is rented rather than as a 400 from a server that was about
// to generate something.
func TestParsePreview(t *testing.T) {
	got, err := ParsePreview("")
	if err != nil || got != DefaultPreview {
		t.Errorf("ParsePreview(\"\") = %q, %v, want %q", got, err, DefaultPreview)
	}
	for _, m := range previewModes {
		if got, err := ParsePreview(m); err != nil || got != m {
			t.Errorf("ParsePreview(%q) = %q, %v", m, got, err)
		}
	}
	if got, err := ParsePreview("  proj  "); err != nil || got != PreviewProj {
		t.Errorf("ParsePreview with padding = %q, %v", got, err)
	}
	_, err = ParsePreview("full")
	if err == nil || !strings.Contains(err.Error(), "proj") {
		t.Errorf("err = %v, want it to list what is allowed", err)
	}
}

// Negative is refused and not clamped. Upstream clamps a non-positive
// interval to 1 -- a frame every step, the most expensive setting there is --
// so a config written to mean "off" would buy the opposite.
func TestParsePreviewInterval(t *testing.T) {
	if got, err := ParsePreviewInterval(0); err != nil || got != DefaultPreviewInterval {
		t.Errorf("ParsePreviewInterval(0) = %d, %v, want %d", got, err, DefaultPreviewInterval)
	}
	if got, err := ParsePreviewInterval(12); err != nil || got != 12 {
		t.Errorf("ParsePreviewInterval(12) = %d, %v", got, err)
	}
	_, err := ParsePreviewInterval(-1)
	if err == nil {
		t.Fatal("want a refusal")
	}
	// The message has to say what to write instead, or the reader turns it
	// into 1 and pays for it.
	for _, want := range []string{"clamp", "every step", "preview: none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

func TestValidateOutput(t *testing.T) {
	// Empty is fine: the caller numbers jobs that do not name themselves.
	for _, ok := range []string{"", "   ", "station", "a/b", "shot-01"} {
		if err := ValidateOutput(ok); err != nil {
			t.Errorf("ValidateOutput(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"..", "/", "../..", "."} {
		if err := ValidateOutput(bad); err == nil {
			t.Errorf("ValidateOutput(%q) accepted a name that cleans away to nothing", bad)
		}
	}
}

// A pointer field holding zero is SENT, and an unset one is not.
//
// This is the whole reason ClipSkip, Eta and FlowShift are pointers: the
// server's defaults for them are -1, null and null, so the zero-is-unset
// convention every other field uses would make `clip_skip: 0` and `eta: 0`
// unrepresentable while looking exactly like a field that works.
func TestBody_AZeroPointerIsSentAndAnUnsetOneIsNot(t *testing.T) {
	zero, zerof := 0, 0.0
	body, err := Job{
		Prompt: "a cat",
		Params: Params{ClipSkip: &zero, Eta: &zerof},
	}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := body["clip_skip"]; !ok || got != 0 {
		t.Errorf("clip_skip = %v (present %v), want 0 present", got, ok)
	}
	sp, ok := body["sample_params"].(map[string]any)
	if !ok {
		t.Fatalf("no sample_params: %v", body)
	}
	if got, ok := sp["eta"]; !ok || got != 0.0 {
		t.Errorf("eta = %v (present %v), want 0 present", got, ok)
	}

	body, err = Job{Prompt: "a cat"}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["clip_skip"]; ok {
		t.Errorf("clip_skip sent for a job that did not set it: %v", body)
	}
	if sp, ok := body["sample_params"].(map[string]any); ok {
		if _, ok := sp["eta"]; ok {
			t.Errorf("eta sent for a job that did not set it: %v", sp)
		}
	}
}

// Each optional parameter lands on the dotted path upstream names, and not on
// ember's own spelling of it.
//
// Four of these are renamed on the way out -- steps to sample_steps, cfg to
// txt_cfg, vae_tiling to vae_tiling_params, slg into guidance rather than
// beside it -- so a field that compiled and went to the wrong key would be a
// parameter the operator set and the server never saw. That failure is silent
// in both directions: the generation succeeds, at the server's default.
func TestBody_PutsEachParameterWhereUpstreamNamesIt(t *testing.T) {
	skip := 2
	shift := 3.5
	body, err := Job{
		Prompt: "a cat",
		Params: Params{
			ClipSkip:          &skip,
			FlowShift:         &shift,
			ShiftedTimestep:   4,
			OutputFormat:      "webp",
			OutputCompression: 90,
			SLG:               SLG{Layers: []int{7, 8}, LayerStart: 0.01, LayerEnd: 0.2, Scale: 2.5},
			Hires: Hires{
				Enabled: true, Upscaler: "Latent", Scale: 2, TargetWidth: 1024,
				TargetHeight: 768, Steps: 10, DenoisingStrength: 0.7, UpscaleTileSize: 128,
			},
			VAETiling: VAETiling{
				Enabled: true, TileSizeW: 512, TileSizeH: 256,
				TargetOverlap: 0.5, RelSizeW: 0.25, RelSizeH: 0.125,
			},
		},
	}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Walked by dotted path, so the test reads as the API's own field list
	// rather than as a pile of type assertions.
	at := func(path string) any {
		cur := any(body)
		for _, part := range strings.Split(path, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("%s: %q is not an object", path, part)
			}
			cur, ok = m[part]
			if !ok {
				t.Fatalf("%s: nothing at %q", path, part)
			}
		}
		return cur
	}
	for path, want := range map[string]any{
		"clip_skip":                              2,
		"output_format":                          "webp",
		"output_compression":                     90,
		"sample_params.flow_shift":               3.5,
		"sample_params.shifted_timestep":         4,
		"sample_params.guidance.slg.scale":       2.5,
		"sample_params.guidance.slg.layer_start": 0.01,
		"sample_params.guidance.slg.layer_end":   0.2,
		"hires.enabled":                          true,
		"hires.upscaler":                         "Latent",
		"hires.scale":                            2.0,
		"hires.target_width":                     1024,
		"hires.target_height":                    768,
		"hires.steps":                            10,
		"hires.denoising_strength":               0.7,
		"hires.upscale_tile_size":                128,
		"vae_tiling_params.enabled":              true,
		"vae_tiling_params.tile_size_w":          512,
		"vae_tiling_params.tile_size_h":          256,
		"vae_tiling_params.target_overlap":       0.5,
		"vae_tiling_params.rel_size_w":           0.25,
		"vae_tiling_params.rel_size_h":           0.125,
	} {
		if got := at(path); got != want {
			t.Errorf("%s = %v (%T), want %v (%T)", path, got, got, want, want)
		}
	}
	if got := at("sample_params.guidance.slg.layers"); !slices.Equal(got.([]int), []int{7, 8}) {
		t.Errorf("slg.layers = %v", got)
	}
}

// A switch that is off sends NO BLOCK, rather than a block the server reads as
// off.
//
// Upstream's own defaults are `hires.enabled` false and `slg.scale` 0, so
// sending the block with the switch off is sending settings nothing applies --
// and in the slg case it is worse than useless: layers with no scale look
// configured on screen and change nothing in the image, which is a bug that
// presents as "the model ignores skip-layer guidance".
func TestBody_AnUnsetSwitchSendsNoBlock(t *testing.T) {
	body, err := Job{
		Prompt: "a cat",
		Params: Params{
			// Everything set except the switch, in all three blocks.
			SLG:       SLG{Layers: []int{7, 8, 9}, LayerStart: 0.01, LayerEnd: 0.2},
			Hires:     Hires{Upscaler: "Latent", Scale: 2, Steps: 10},
			VAETiling: VAETiling{TileSizeW: 512},
		},
	}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["hires"]; ok {
		t.Errorf("hires sent with enabled false: %v", body["hires"])
	}
	if _, ok := body["vae_tiling_params"]; ok {
		t.Errorf("vae_tiling_params sent with enabled false: %v", body["vae_tiling_params"])
	}
	if sp, ok := body["sample_params"].(map[string]any); ok {
		if g, ok := sp["guidance"].(map[string]any); ok {
			if _, ok := g["slg"]; ok {
				t.Errorf("slg sent with scale 0: %v", g["slg"])
			}
		}
	}
}

// extra colliding with one of the NEW nested paths is refused by that path's
// own name.
//
// The collision check works off a dotted path registered as each key is set,
// so a block attached to the body without registering its paths would accept
// an extra that silently overwrote it. "Something clashed" is not a useful
// error here: the operator has to be told which of thirty fields to remove.
func TestBody_RefusesExtraThatShadowsANestedParameter(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  Job
		want string
	}{
		{
			"hires.scale",
			Job{Prompt: "a", Params: Params{Hires: Hires{Enabled: true, Scale: 2}},
				Extra: map[string]any{"hires": map[string]any{"scale": 4}}},
			"extra sets hires.scale",
		},
		{
			"slg.scale",
			Job{Prompt: "a", Params: Params{SLG: SLG{Scale: 2}},
				Extra: map[string]any{"sample_params": map[string]any{
					"guidance": map[string]any{"slg": map[string]any{"scale": 9}}}}},
			"extra sets sample_params.guidance.slg.scale",
		},
		{
			"vae_tiling_params.tile_size_w",
			Job{Prompt: "a", Params: Params{VAETiling: VAETiling{Enabled: true, TileSizeW: 512}},
				Extra: map[string]any{"vae_tiling_params": map[string]any{"tile_size_w": 64}}},
			"extra sets vae_tiling_params.tile_size_w",
		},
		{
			"clip_skip",
			func() Job {
				two := 2
				return Job{Prompt: "a", Params: Params{ClipSkip: &two},
					Extra: map[string]any{"clip_skip": 1}}
			}(),
			"extra sets clip_skip",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.job.Body(PreviewProj, 5)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// A name outside the server's own list is refused, and so is a name that
// cannot be checked because the server published no list.
//
// The upscaler is the one that matters: api.md resolves a model-backed name as
// `--hires-upscalers-dir / (name + ext)`, so the name reaches a path join on
// the node. Nothing in this repository knows what sd-server does with a
// hostile one, which is exactly why the check is an allow-list rather than a
// pattern.
func TestValidate_ChecksTheParameterNamesAgainstTheServersLists(t *testing.T) {
	full := Capabilities{
		Samplers:      []string{"euler"},
		Schedulers:    []string{"karras"},
		OutputFormats: []string{"png", "jpeg"},
		Upscalers:     []string{"Latent", "Lanczos"},
	}
	for _, tc := range []struct {
		name string
		caps Capabilities
		p    Params
		want string
	}{
		{"an unknown format", full, Params{OutputFormat: "tiff"},
			`output_format "tiff" is not one this server has`},
		{"an unknown upscaler", full, Params{Hires: Hires{Enabled: true, Upscaler: "../etc/passwd"}},
			`hires upscaler "../etc/passwd" is not one this server has`},
		{"no format list", Capabilities{}, Params{OutputFormat: "png"},
			"the server reported no format list"},
		{"no upscaler list", Capabilities{}, Params{Hires: Hires{Enabled: true, Upscaler: "Latent"}},
			"the server reported no upscaler list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Job{Prompt: "a cat", Params: tc.p}, tc.caps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// And the names the server did publish are accepted.
	ok := Job{Prompt: "a cat", Params: Params{
		OutputFormat: "jpeg",
		Hires:        Hires{Enabled: true, Upscaler: "Latent"},
	}}
	if err := Validate(ok, full); err != nil {
		t.Fatalf("a published name was refused: %v", err)
	}
}

// A hires target size is checked against the same limits the main size is.
//
// It is a generation size like any other -- the second pass samples at it --
// and a target above the server's maximum is a job that fails after the first
// pass has already been paid for.
func TestValidate_ChecksTheHiresTargetAgainstTheSameLimits(t *testing.T) {
	caps := Capabilities{
		Upscalers: []string{"Latent"},
		Limits:    Limits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048},
	}
	j := Job{Prompt: "a cat", Params: Params{Hires: Hires{Enabled: true, TargetWidth: 4096}}}
	err := Validate(j, caps)
	if err == nil || !strings.Contains(err.Error(), "hires target_width 4096 is above") {
		t.Fatalf("err = %v, want the hires width refused by name", err)
	}
	j = Job{Prompt: "a cat", Params: Params{Hires: Hires{Enabled: true, TargetHeight: 16}}}
	err = Validate(j, caps)
	if err == nil || !strings.Contains(err.Error(), "hires target_height 16 is below") {
		t.Fatalf("err = %v, want the hires height refused by name", err)
	}
}

// Every parameter that is wrong on its face is refused with no server at all.
//
// None of these needs a capability list, so none of them should cost a
// rental. The negative cases are the ones that earn this: upstream clamps a
// non-positive preview_interval to 1, so a habit of writing -1 to mean "off"
// buys the most expensive setting there is -- and the same keystroke in a
// denoising strength or a scale would be applied rather than clamped.
func TestValidateStatic_RefusesParametersThatNeedNoServer(t *testing.T) {
	below := -2
	for _, tc := range []struct {
		name string
		p    Params
		want string
	}{
		{"clip_skip below -1", Params{ClipSkip: &below}, "clip_skip -2 is below -1"},
		{"slg layer_start above 1", Params{SLG: SLG{LayerStart: 1.5}}, "slg layer_start 1.5 is outside 0 to 1"},
		{"slg layer_end negative", Params{SLG: SLG{LayerEnd: -0.5}}, "slg layer_end -0.5 is outside 0 to 1"},
		{"an empty slg range", Params{SLG: SLG{LayerStart: 0.5, LayerEnd: 0.2}}, "is after layer_end"},
		{"a negative slg layer", Params{SLG: SLG{Layers: []int{-1}}}, "slg layer -1 is negative"},
		{"a negative hires scale", Params{Hires: Hires{Scale: -2}}, "hires scale -2 is negative"},
		{"negative hires steps", Params{Hires: Hires{Steps: -1}}, "hires steps -1 is negative"},
		{"denoising above 1", Params{Hires: Hires{DenoisingStrength: 1.2}}, "hires denoising_strength 1.2 is outside 0 to 1"},
		{"a negative tile", Params{Hires: Hires{UpscaleTileSize: -1}}, "hires upscale_tile_size -1 is negative"},
		{"a negative tile size", Params{VAETiling: VAETiling{TileSizeW: -8}}, "vae tiling tile size -8x0 is negative"},
		{"overlap above 1", Params{VAETiling: VAETiling{TargetOverlap: 2}}, "vae tiling target_overlap 2 is outside 0 to 1"},
		{"rel_size_w above 1", Params{VAETiling: VAETiling{RelSizeW: 1.1}}, "vae tiling rel_size_w 1.1 is outside 0 to 1"},
		{"rel_size_h negative", Params{VAETiling: VAETiling{RelSizeH: -1}}, "vae tiling rel_size_h -1 is outside 0 to 1"},
		{"compression above 100", Params{OutputCompression: 101}, "output_compression 101 is outside 0 to 100"},
		{"compression negative", Params{OutputCompression: -1}, "output_compression -1 is outside 0 to 100"},
		// The hires target is a geometry, and the geometry's own bound was
		// the thing that needed a response. See MaxAxis.
		{"a hires target past the published upscale maximum",
			Params{Hires: Hires{TargetWidth: MaxAxis + 1}}, "hires target_width 8193 is above 8192"},
		{"a negative hires target height",
			Params{Hires: Hires{TargetHeight: -64}}, "hires target_height -64 is negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStatic(Job{Prompt: "a cat", Output: "x", Params: tc.p}, PreviewNone, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// -1 is the one negative clip_skip with a meaning: the model's own.
	minusOne := -1
	err := ValidateStatic(Job{Prompt: "a cat", Output: "x",
		Params: Params{ClipSkip: &minusOne}}, PreviewNone, 0)
	if err != nil {
		t.Fatalf("clip_skip -1 refused: %v", err)
	}
}

// The capability lists are read off a real capture, not off api.md.
//
// output_formats is an array of strings and upscalers is an array of OBJECTS
// in the same response, which is the kind of difference a doc-derived wire
// type gets wrong and then passes its own test on -- capabilities.model was
// exactly that, an object read as a string.
func TestCapabilities_ReadsTheFormatAndUpscalerLists(t *testing.T) {
	caps := testCaps(t)
	if !slices.Contains(caps.OutputFormats, "png") || !slices.Contains(caps.OutputFormats, "webp") {
		t.Errorf("output formats = %v, want the captured list", caps.OutputFormats)
	}
	// Flattened from objects to names, in order.
	if len(caps.Upscalers) == 0 || caps.Upscalers[0] != "None" {
		t.Errorf("upscalers = %v, want the captured names", caps.Upscalers)
	}
	if !slices.Contains(caps.Upscalers, "Latent") {
		t.Errorf("upscalers = %v, want Latent among them", caps.Upscalers)
	}
}

// A CANCELLED JOB CARRIES AN ERROR OBJECT, which is the one thing the capture
// of this response corrected and it was worth a rental.
//
// `{"code":"cancelled","message":"job cancelled by client"}` sits beside
// status "cancelled", so a caller that tested State.Err before the status --
// which app.runJobs did -- read every successful cancel as a refusal. One
// layer up that made the operator's own cancel look like a failed batch, and
// the queue destroys the rental for a failed batch: every other request on
// that node would have gone with it, by pressing the button the feature
// added. The fake in internal/app answered a cancelled status with no error
// at all, so the whole unit suite agreed with itself.
//
// `result` is null and there are no images: a cancelled generation is
// sampling that stopped, not an image withheld.
func TestPoll_ACancelledJobCarriesTheServersOwnExplanation(t *testing.T) {
	c := newTestClient(t, serveFile(t, "job_cancelled.json"))
	st, err := c.Poll(t.Context(), "job_1a19daf5ace_00000000")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusCancelled {
		t.Fatalf("status = %v, want cancelled", st.Status)
	}
	if !st.Status.Done() {
		t.Error("a cancelled job is not terminal, so a poller would spin on a card it has stopped")
	}
	if st.Err == nil {
		t.Fatal("no error object: this fixture is here because a real cancelled job carries one")
	}
	if st.Err.Code != "cancelled" {
		t.Errorf("error code = %q, want cancelled", st.Err.Code)
	}
	if len(st.Images) != 0 {
		t.Errorf("a cancelled job came back with %d image(s)", len(st.Images))
	}
}

// A geometry no response could permit is refused with no response.
//
// THE DEFECT IS THE 202. `Validate` reads `limits.max_width`, which is a
// capability list and therefore exists only once a rental has loaded weights
// -- so `webapi.validateJobs` skips it before the first rental and a width of
// 3,000,000,000 was queued, stored, and paid for with a card. The browser's
// listing decodes into an `i32` and could not read the row, which took every
// healthy request in the same envelope with it.
//
// THE PUBLISHED MAXIMUM IS ACCEPTED BESIDE EACH REFUSAL, because a bound that
// refused every geometry would satisfy the refusals on its own -- and 8192 is
// taken from `limits.max_upscale_width` precisely so that it cannot contradict
// a server ember has observed. The captured generation maxima are 4096 and
// `Validate` is what applies those; this one is the outer envelope.
func TestValidateStatic_RefusesAGeometryNoResponsePublishes(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  Job
		want string
	}{
		{"a width an i32 cannot hold", Job{Width: 3000000000}, "width 3000000000 is above 8192"},
		{"a height an i32 cannot hold", Job{Height: 3000000000}, "height 3000000000 is above 8192"},
		{"one past the published upscale maximum", Job{Width: MaxAxis + 1}, "width 8193 is above 8192"},
		{"a negative width", Job{Width: -1}, "width -1 is negative"},
		{"a negative height", Job{Height: -1}, "height -1 is negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := tc.job
			j.Prompt, j.Output = "a cat", "x"
			err := ValidateStatic(j, PreviewNone, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	for _, j := range []Job{
		{Prompt: "a cat", Output: "x", Width: MaxAxis, Height: MaxAxis},
		// Zero is unset and stays unset: most jobs name no geometry at all
		// and take the server's own default.
		{Prompt: "a cat", Output: "x"},
		{Prompt: "a cat", Output: "x", Params: Params{Hires: Hires{
			TargetWidth: MaxAxis, TargetHeight: MaxAxis}}},
	} {
		if err := ValidateStatic(j, PreviewNone, 0); err != nil {
			t.Errorf("%dx%d refused: %v; the bound IS limits.max_upscale_width, so a server "+
				"ember has observed must be reachable through it", j.Width, j.Height, err)
		}
	}

	// AND THE CAPS HALF STILL APPLIES THE TIGHTER FIGURE, which is what makes
	// the static bound an envelope rather than a replacement: 8192 is a
	// legitimate hires target and is twice what the captured server will
	// generate at.
	caps := testCaps(t)
	err := Validate(Job{Prompt: "a cat", Width: MaxAxis, Height: 512}, caps)
	if err == nil || !strings.Contains(err.Error(), "above this server's maximum") {
		t.Errorf("err = %v, want the published limits.max_width to still refuse 8192", err)
	}
}
