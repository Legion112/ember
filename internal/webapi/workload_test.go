package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
)

// declaring is a harness whose config declares two workloads of its own.
//
// THROUGH THE CONFIG AND NOT THROUGH `Options.Workloads`, so the harness drives
// the same derivation cmd_serve does -- one reading of the file, handed to the
// server and to the queue. A test that set the resolved list directly could
// not tell "the derivation works" from "this test wrote the answer".
func declaring(o *Options) {
	o.Config.Image = ""
	o.Config.SD = config.SDConfig{}
	o.Config.Workloads = []config.Workload{
		{
			ID: "image", Label: "Images", Driver: config.DriverSD,
			Image: "ghcr.io/example/sd:v1",
			SD: config.SDConfig{
				Serve: []string{"sd-server", "--model", "/m.gguf"},
				// This entry's own defaults, which a UI starts its controls
				// from -- and which another sd entry could set differently.
				Preview: "tae", PreviewInterval: 3,
			},
		},
		{
			ID: "speech", Label: "Speech", Driver: config.DriverTTS,
			Image: "ghcr.io/example/tts:v1",
			TTS: config.TTSConfig{Voices: []config.TTSVoice{
				{ID: "lessac", Name: "Lessac (en_US)", Voice: "/opt/voices/lessac.onnx"},
			}},
		},
	}
}

// TestConfig_PublishesTheDeclaredEntriesWithTheOperatorsOwnLabels.
//
// THE ANSWER IS ABOUT THIS SERVER, which is the whole of #200: the list used to
// be `genstore.Kinds()`, a literal in ember's source, so a page read "what this
// binary knows" as "what this server can do". Each entry carries the label the
// operator wrote and the driver a form is drawn from, and NOT the image.
func TestConfig_PublishesTheDeclaredEntriesWithTheOperatorsOwnLabels(t *testing.T) {
	h := newHarness(t, declaring)
	got := decode[configView](t, h.do(t, "GET", "/api/config", ""))

	if len(got.Workloads) != 2 {
		t.Fatalf("workloads = %+v, want the two this file declares", got.Workloads)
	}
	// IN THE FILE'S OWN ORDER, because the nav prints them in it and the
	// landing address renders the first.
	if got.Workloads[0].ID != "image" || got.Workloads[1].ID != "speech" {
		t.Errorf("order = %q, %q, want the file's", got.Workloads[0].ID, got.Workloads[1].ID)
	}
	if got.Workloads[0].Label != "Images" || got.Workloads[1].Label != "Speech" {
		t.Errorf("labels = %q, %q, want the operator's own",
			got.Workloads[0].Label, got.Workloads[1].Label)
	}
	// PER ENTRY, which the image being per entry forces: the voices and the
	// preview defaults belong to one workload and not to the process.
	if got.Workloads[0].Preview != "tae" || got.Workloads[0].PreviewInterval != 3 {
		t.Errorf("preview defaults = %q every %d, want this entry's",
			got.Workloads[0].Preview, got.Workloads[0].PreviewInterval)
	}
	if len(got.Workloads[0].Voices) != 0 {
		t.Errorf("the image entry carries voices: %+v", got.Workloads[0].Voices)
	}
	if len(got.Workloads[1].Voices) != 1 || got.Workloads[1].Voices[0].ID != "lessac" {
		t.Errorf("voices = %+v, want the speech entry's own", got.Workloads[1].Voices)
	}

	// AND NOT THE IMAGES, which is this file's projection rule: a UI needs an
	// id to send, a label to print and a driver to draw a form from, and the
	// operator knows what is behind them.
	body := h.do(t, "GET", "/api/config", "").Body.String()
	for _, ref := range []string{"ghcr.io", "/opt/voices/", "sd-server"} {
		if strings.Contains(body, ref) {
			t.Errorf("the config view publishes %q:\n%s", ref, body)
		}
	}
}

// TestPostRequest_ResolvesTheDeclaredIdAndStoresBothFacts.
//
// THE ROW CARRIES THE DRIVER AND THE ENTRY, and they answer different
// questions: the driver is what the jobs' shape was validated against and what
// the ledger can print about a row whose entry has since been renamed; the
// entry id is what resolves to an image and means nothing without the
// operator's file.
func TestPostRequest_ResolvesTheDeclaredIdAndStoresBothFacts(t *testing.T) {
	h := newHarness(t, declaring)

	w := h.do(t, "POST", "/api/requests",
		`{"workload":"speech","provider":"local","jobs":[
			{"tts":{"text":"Hello.","voice_id":"lessac","output":"greeting"}}]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	rs, err := h.store.List(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("%d row(s), want 1", len(rs))
	}
	if rs[0].Workload != "speech" {
		t.Errorf("workload = %q, want the id the request named", rs[0].Workload)
	}
	if rs[0].Kind != genstore.KindTTS {
		t.Errorf("kind = %q, want the entry's driver", rs[0].Kind)
	}
	// AND NOTHING ABOUT AN IMAGE REACHED THE ROW, which is the seal: the wire
	// type has no field for one, so there is nothing to drop.
	b, err := json.Marshal(rs[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ghcr.io") {
		t.Errorf("a stored request names an image: %s", b)
	}
}

// TestPostRequest_RefusesAnIdThisServerDoesNotDeclare, listing what it has.
//
// A 400 AND NOT A 409, and the split is the remedy: an id that is not there is
// something the request got wrong and a different body would fix, and an id
// that IS there and cannot run is an edit to ember.yaml. A reader of the first
// message needs the ids they could have named.
//
// `sd` IS THE INTERESTING ONE, because it is what an absent id resolves to:
// a file that declares entries of its own has no entry called `sd` unless the
// operator wrote one, so a body from a client that still omits the field is
// asked rather than silently planned against the wrong workload.
func TestPostRequest_RefusesAnIdThisServerDoesNotDeclare(t *testing.T) {
	h := newHarness(t, declaring)

	for _, body := range []string{
		`{"workload":"video","provider":"local","jobs":[{"prompt":"a cat"}]}`,
		`{"provider":"local","jobs":[{"prompt":"a cat"}]}`,
	} {
		w := h.do(t, "POST", "/api/requests", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d for %s, want 400: %s", w.Code, body, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), "image, speech") {
			t.Errorf("the refusal does not list what is declared: %s", w.Body.String())
		}
	}
	if n, _ := h.store.List(t.Context(), genstore.Filter{}); len(n) != 0 {
		t.Errorf("%d row(s) were stored over a refused request", len(n))
	}
}

// TestPostRequest_TakesTheEntrysOwnTTLWhenTheBodySentNone.
//
// THE DEFECT THIS WAS WRITTEN AGAINST COST A REAL RUN. `toRequest` took the
// entry for everything except its deadline, so a body with no `ttl` fell
// through to the file's `ttl:` -- and `flux2-local` declares `ttl: 4h`
// precisely because its weights are a 59 GB fetch that takes about two hours.
// A browser sending no ttl got the file's 30m, `Narrow` correctly took the
// smaller of the two, and the node self-destructed 13.3 GB into the download:
// half an hour of a card paid for, nothing generated, and nothing resumable
// left behind.
//
// BOTH ARMS, because the fallback is a chain and either link can rot. An entry
// that declares a ttl decides it; an entry that declares none inherits the
// file's, which is the one value `config.Load` has always refused to leave
// unset -- so the request is never stored with no deadline whichever arm ran.
func TestPostRequest_TakesTheEntrysOwnTTLWhenTheBodySentNone(t *testing.T) {
	const long = 4 * time.Hour

	h := newHarness(t, func(o *Options) {
		declaring(o)
		// The entry's own, longer than the file's hour, the way a workload
		// whose weights take longer to fetch than to use has to be.
		o.Config.Workloads[0].TTL = long
	})

	w := h.do(t, "POST", "/api/requests",
		`{"workload":"image","provider":"local","jobs":[{"prompt":"a cat"}]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	req := stored(t, h, w)
	if req.TTL != long {
		t.Errorf("ttl = %v, want the entry's %v: a deadline the file chose and the entry did not",
			req.TTL, long)
	}

	// AND THE BODY STILL WINS, because the entry is a default and not a
	// substitution -- an operator shortening one run must still be able to.
	w = h.do(t, "POST", "/api/requests",
		`{"workload":"image","provider":"local","ttl":"10m","jobs":[{"prompt":"a cat"}]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if req := stored(t, h, w); req.TTL != 10*time.Minute {
		t.Errorf("ttl = %v, want the body's 10m", req.TTL)
	}
}

// TestPostRequest_FallsBackToTheFilesTTLWhenTheEntryDeclaresNone is the other
// arm: `declaring` sets no ttl on either entry, so the harness's hour is what
// a request has to be stored with. Without this the change above could have
// stored a zero -- which `genstore` refuses, but only after a row has been
// built, and the message an operator would get names the request rather than
// the file.
func TestPostRequest_FallsBackToTheFilesTTLWhenTheEntryDeclaresNone(t *testing.T) {
	h := newHarness(t, declaring)

	w := h.do(t, "POST", "/api/requests",
		`{"workload":"image","provider":"local","jobs":[{"prompt":"a cat"}]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if req := stored(t, h, w); req.TTL != time.Hour {
		t.Errorf("ttl = %v, want the file's 1h", req.TTL)
	}
}

// stored returns the row THIS response's id names, and fails loudly on an
// absent one.
//
// BY ID AND NOT "THE ONE QUEUED ROW", because a test that posts twice to show
// a precedence has two, and reading the first would have asserted the default
// twice while reporting that the override worked.
func stored(t *testing.T, h *harness, w *httptest.ResponseRecorder) genstore.Request {
	t.Helper()
	id := decode[map[string]string](t, w)["id"]
	if id == "" {
		t.Fatal("no id came back, so there is no row to read the deadline off")
	}
	r, err := h.store.Get(t.Context(), genstore.ReqID(id))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
