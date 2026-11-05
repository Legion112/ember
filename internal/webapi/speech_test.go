package webapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/tts"
)

// labVoices is a catalogue a harness publishes.
func labVoices() tts.Catalogue {
	return tts.Catalogue{
		{ID: "lessac", Name: "Lessac (en_US)", Voice: "/opt/voices/lessac.onnx", EspeakData: "/e"},
	}
}

// TestSpeech_ARequestNamesAVoiceIdAndNeverAPath is the seal, asserted over the
// wire rather than argued.
//
// WHAT IS BEING CHECKED is not that a path is refused -- there is no field for
// one, which is stronger -- but that the two things which ARE on the wire do
// what they claim: an id resolves against the operator's list, and the stored
// row carries the id rather than the resolution.
func TestSpeech_ARequestNamesAVoiceIdAndNeverAPath(t *testing.T) {
	h := newHarness(t, withVoices)
	srv := live(t, h)

	body := `{"workload":"tts","provider":"local","jobs":[
		{"tts":{"text":"Hello from ember.","voice_id":"lessac","output":"greeting"}}]}`
	res := h.wire(t, srv, "POST", "/api/requests", "application/json", []byte(body))
	if res.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", res.Status, res.Body)
	}

	rs, err := h.store.List(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("%d row(s) stored, want 1", len(rs))
	}
	r := rs[0]
	if r.Kind != genstore.KindTTS {
		t.Errorf("kind = %q, want %q", r.Kind, genstore.KindTTS)
	}
	if len(r.Jobs) != 1 || r.Jobs[0].TTS == nil {
		t.Fatalf("the stored row carries no utterance: %+v", r.Jobs)
	}
	ask := r.Jobs[0].TTS
	if ask.VoiceID != "lessac" || ask.Text != "Hello from ember." {
		t.Errorf("stored ask = %+v", ask)
	}

	// THE RESOLUTION IS NOT STORED, which is the half that makes the id a
	// seal rather than a spelling: the row read back out of the store has no
	// path in it, so a future reader of this table cannot find one to trust.
	b, err := json.Marshal(r.Jobs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/opt/voices/lessac.onnx", "/e"} {
		if strings.Contains(string(b), path) {
			t.Errorf("the stored job carries %q; a request's row names an id and never a path:\n%s",
				path, b)
		}
	}
}

// TestSpeech_RefusesAVoiceTheOperatorDidNotPublish. The only realistic way
// here is a stale page, and the refusal costs nothing: the same mistake found
// at the node costs the create call, the wait for sshd and the image pull.
func TestSpeech_RefusesAVoiceTheOperatorDidNotPublish(t *testing.T) {
	h := newHarness(t, withVoices)
	srv := live(t, h)

	body := `{"workload":"tts","provider":"local","jobs":[{"tts":{"text":"x","voice_id":"nope"}}]}`
	res := h.wire(t, srv, "POST", "/api/requests", "application/json", []byte(body))
	if res.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Status, res.Body)
	}
	// It lists what IS published, because a browser cannot read the config.
	if !strings.Contains(string(res.Body), "lessac") {
		t.Errorf("body = %q, want it to name the voices that exist", res.Body)
	}
	if n, _ := h.store.List(t.Context(), genstore.Filter{}); len(n) != 0 {
		t.Errorf("%d row(s) were stored over a refused request", len(n))
	}
}

// TestSpeech_RefusesSpeechWhenThisProcessPublishesNone, naming the key.
//
// This is the state an operator running `image: ember:sd` with no `tts:` block
// is in, and it is why the catalogue is not inferred from `tts.voice`: an
// implicit entry would offer a voice that cannot work and the refusal would
// arrive after a card was rented.
func TestSpeech_RefusesSpeechWhenThisProcessPublishesNone(t *testing.T) {
	h := newHarness(t) // no Voices
	srv := live(t, h)

	body := `{"workload":"tts","provider":"local","jobs":[{"tts":{"text":"x","voice_id":"lessac"}}]}`
	res := h.wire(t, srv, "POST", "/api/requests", "application/json", []byte(body))
	// A 409 AND NOT A 400, which is the status the sd arm of this gate always
	// had right: the body is well-formed and the request is one ember
	// understands -- what is missing is a block in the operator's own file,
	// which no edit to the request can fix. A 400 would tell a page to change
	// what it sent.
	if res.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Status, res.Body)
	}
	if !strings.Contains(string(res.Body), "tts.voices") {
		t.Errorf("body = %q, want it to name tts.voices", res.Body)
	}
	// AND THE PAGE IS NEVER OFFERED IT, which is the half a 409 alone cannot
	// prove: one method decides what `/api/config` publishes and what this
	// route refuses, so a speech tab cannot exist on a server with no voices.
	// Over the socket and not through the recorder: `live` moved this
	// harness's port, so `do`'s hard-coded Host would be refused by
	// checkHost and the decode would read a zero value as an answer.
	cv := h.wire(t, srv, "GET", "/api/config", "", nil)
	var got configView
	if err := json.Unmarshal(cv.Body, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, cv.Body)
	}
	if len(got.Workloads) != 1 || got.Workloads[0].Driver != config.DriverSD {
		t.Errorf("workloads = %+v, want only the image one", got.Workloads)
	}
}

// TestSpeech_RefusesAJobBelongingToTheOtherKind, in both directions.
//
// The image-fields-on-a-speech-request half is the one with a smuggling shape
// to it: the values would be stored, read by nothing, and the next reader to
// widen the speech path would find them already there. The other half is the
// quiet one -- an utterance on an image request would be DROPPED, and the
// operator billed for a generation that said nothing about what they asked
// for.
func TestSpeech_RefusesAJobBelongingToTheOtherKind(t *testing.T) {
	h := newHarness(t, withVoices)
	srv := live(t, h)

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"a speech request carrying a prompt",
			`{"workload":"tts","provider":"local","jobs":[
				{"prompt":"a cat","tts":{"text":"x","voice_id":"lessac"}}]}`,
			"also sets image parameters",
		},
		{
			// A field nobody would think of, which is the case the reflective
			// check exists for: a hand-written list of sd field names would
			// not have had `vae_tiling` in it on the day it was added.
			"a speech request carrying a nested image block",
			`{"workload":"tts","provider":"local","jobs":[
				{"vae_tiling":{"enabled":true},"tts":{"text":"x","voice_id":"lessac"}}]}`,
			"also sets image parameters",
		},
		{
			"a speech request carrying no utterance",
			`{"workload":"tts","provider":"local","jobs":[{"prompt":"a cat"}]}`,
			"carries no utterance",
		},
		{
			"an image request carrying an utterance",
			`{"provider":"local","jobs":[{"prompt":"a cat","tts":{"text":"x","voice_id":"lessac"}}]}`,
			"carries an utterance",
		},
		{
			// IT USED TO BE A KIND EMBER DOES NOT RUN, and the field is an
			// id this server does not declare now -- which is the same
			// defect one authority over. A kind was ember's own enum, so the
			// refusal could name the two values the binary held; an id is
			// the operator's, so the refusal names what their file declares.
			"a workload this server does not declare",
			`{"workload":"stt","provider":"local","jobs":[{"prompt":"a cat"}]}`,
			"no workload",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := h.wire(t, srv, "POST", "/api/requests", "application/json", []byte(c.body))
			if res.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", res.Status, res.Body)
			}
			if !strings.Contains(string(res.Body), c.want) {
				t.Errorf("body = %q, want it to contain %q", res.Body, c.want)
			}
		})
	}
}

// TestSpeech_AnImageRequestIsUnchanged is the compatibility assertion, and it
// is here rather than taken on trust because the wire gained a field: a body
// the page has always posted names no kind at all, and an absent kind has to
// keep meaning sd.
func TestSpeech_AnImageRequestIsUnchanged(t *testing.T) {
	h := newHarness(t)
	srv := live(t, h)

	res := h.wire(t, srv, "POST", "/api/requests", "application/json",
		[]byte(`{"provider":"local","jobs":[{"prompt":"a cat","width":64,"height":64}]}`))
	if res.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", res.Status, res.Body)
	}
	rs, err := h.store.List(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("%d row(s), want 1", len(rs))
	}
	if genstore.KindOf(rs[0].Kind) != genstore.KindSD {
		t.Errorf("an absent kind stored as %q, want it to mean %q", rs[0].Kind, genstore.KindSD)
	}
	if rs[0].Jobs[0].Prompt != "a cat" {
		t.Errorf("prompt = %q", rs[0].Jobs[0].Prompt)
	}
	if rs[0].Jobs[0].TTS != nil {
		t.Error("an image job gained an utterance")
	}
}

// TestConfig_PublishesTheWorkloadsAndTheirVoicesAndNotTheirPaths.
//
// A UI cannot guess any of it: a request names a workload and a voice by
// **id**, so the lists of ids are something only ember can produce out of the
// operator's config. And the PATHS and the IMAGE are deliberately absent,
// which is this file's own projection rule -- a hand-written view means adding
// a field somewhere cannot publish it by default.
//
// THE VOICES ARE ON THE WORKLOAD AND NOT ON THE CONFIG, which the image moving
// into the entry forces: two declared speech workloads are two images with two
// voice sets on them, and one list for the process would offer each one's ids
// for the other's node.
func TestConfig_PublishesTheWorkloadsAndTheirVoicesAndNotTheirPaths(t *testing.T) {
	h := newHarness(t, withVoices)
	srv := live(t, h)

	res := h.wire(t, srv, "GET", "/api/config", "", nil)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Status, res.Body)
	}
	var v struct {
		Workloads []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Driver string `json:"driver"`
			Voices []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"voices"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal(res.Body, &v); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Body)
	}
	// TWO WORKLOADS AND NOT TWO KINDS: this config has an `sd:` block and a
	// `tts.voices` list, so it describes two implicit entries sharing one
	// image -- which is exactly today's behaviour written down. What has
	// changed is that a config with ONE of those blocks now publishes one
	// entry, where `genstore.Kinds()` published both unconditionally.
	if len(v.Workloads) != 2 {
		t.Fatalf("workloads = %+v, want the two this config describes", v.Workloads)
	}
	var spoke int
	for _, w := range v.Workloads {
		if w.ID == "" || w.Label == "" {
			t.Errorf("workload %+v has no id or no label; a nav has nothing to print", w)
		}
		if w.Driver != config.DriverTTS {
			if len(w.Voices) != 0 {
				t.Errorf("workload %q has driver %q and voices %+v", w.ID, w.Driver, w.Voices)
			}
			continue
		}
		spoke++
		if len(w.Voices) != 1 || w.Voices[0].ID != "lessac" ||
			w.Voices[0].Name != "Lessac (en_US)" {
			t.Errorf("voices = %+v", w.Voices)
		}
	}
	if spoke != 1 {
		t.Errorf("%d speech workload(s) published, want 1", spoke)
	}
	for _, path := range []string{"/opt/voices/lessac.onnx", "espeak", "/e\"", "sha256"} {
		if strings.Contains(string(res.Body), path) {
			t.Errorf("the config view publishes %q; a browser has no use for a path or an "+
				"image on the node:\n%s", path, res.Body)
		}
	}
}

// TestConfig_PublishesAnEmptyVoiceListAndNeverNull. The nil-slice rule, met
// again: a client whose field is a list gets a type error from `null` and
// drops the whole response -- which this repository has already paid a whole
// queue panel for.
//
// IT IS THE SD ENTRY'S OWN LIST NOW. A workload with no speech driver has no
// voices, and that has to arrive as `[]` for exactly the same reason: the page
// reads every one of these through a `nullable`, and the field that is empty
// on every image workload there is is the one most likely to go out as null.
func TestConfig_PublishesAnEmptyVoiceListAndNeverNull(t *testing.T) {
	h := newHarness(t) // no voices
	srv := live(t, h)
	res := h.wire(t, srv, "GET", "/api/config", "", nil)
	if !strings.Contains(string(res.Body), `"voices":[]`) {
		t.Errorf("want \"voices\":[] on a workload publishing none:\n%s", res.Body)
	}
	if !strings.Contains(string(res.Body), `"workloads":[`) {
		t.Errorf("want a workloads list:\n%s", res.Body)
	}
}

// withVoices is how a harness publishes a catalogue.
//
// THROUGH THE CONSTRUCTOR AND NOT BY POKING THE FIELD AFTERWARDS, which is the
// arrangement #105 records as the reason GET /api/nodes answered 501 in every
// shipped `ember serve`: the three assignments to `scan` were all in test
// files reaching past New, so the suite could not tell "the production value
// is missing" from "this test did not set it". A mutator goes through the same
// New the daemon calls.
//
// AND IT WRITES THE CONFIG RATHER THAN THE CATALOGUE, which is one step
// further in the same direction: the harness derives `Options.Workloads` from
// `Options.Config` exactly as cmd_serve does, so a test that publishes voices
// drives the real derivation instead of handing the server an answer nothing
// produced. The voices land on the `tts:` block, which is the implicit entry's
// -- so this harness declares no `workloads:` and is the back-compatible
// shape every committed configuration has.
func withVoices(o *Options) {
	v := labVoices()[0]
	o.Config.TTS.Voices = []config.TTSVoice{
		{ID: v.ID, Name: v.Name, Voice: v.Voice, EspeakData: v.EspeakData},
	}
}
