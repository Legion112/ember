package queue

import (
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/tts"
)

func queueVoices() tts.Catalogue {
	return tts.Catalogue{
		{ID: "lessac", Name: "Lessac", Voice: "/opt/voices/lessac.onnx", EspeakData: "/e"},
	}
}

// speechWorkloads is the one implicit speech entry such a config describes.
//
// DERIVED FROM A CONFIG AND NOT WRITTEN AS A CATALOGUE, which is the harness
// doing what cmd_serve does -- one reading of the file, handed to the loop.
// The id is `tts` because that is what the implicit derivation names an entry
// after, and it is what `genstore.WorkloadOf` resolves a row with no workload
// and kind `tts` to.
func speechWorkloads(t *testing.T) []config.Workload {
	t.Helper()
	v := queueVoices()[0]
	ws, err := config.Config{
		Image: "ghcr.io/example/tts@sha256:" + strings.Repeat("a", 64),
		TTS: config.TTSConfig{Voices: []config.TTSVoice{
			{ID: v.ID, Name: v.Name, Voice: v.Voice, EspeakData: v.EspeakData},
		}},
	}.WorkloadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func spokenRequest(text, voice string) genstore.Request {
	return genstore.Request{
		Kind: genstore.KindTTS, Provider: "local", TTL: time.Hour,
		Jobs: []genstore.Job{{TTS: &tts.Ask{Text: text, VoiceID: voice, Output: "greeting"}}},
	}
}

// TestDrain_ASpeechRequestReachesTheSpeechRentalAndResolvesItsVoice.
//
// THE JOIN, which is what no unit test of either half can see: the drain loop
// reads a kind off a stored row, opens the rental that kind needs, and the
// runner turns a voice **id** into the two paths out of the catalogue the
// process was started with. Every one of those is correct in isolation and
// the question is whether they are joined -- the closed loop AGENTS.md records
// shipping three times.
func TestDrain_ASpeechRequestReachesTheSpeechRentalAndResolvesItsVoice(t *testing.T) {
	st := genstore.NewRecordingStore()
	id, err := st.Enqueue(t.Context(), spokenRequest("Hello from ember.", "lessac"))
	if err != nil {
		t.Fatal(err)
	}

	sp := newRecordingSpoken()
	pl := &fakePlanner{served: &fakeServed{}, spoken: sp}
	q, _ := newQueueOver(t, st, pl)
	q.Workloads = speechWorkloads(t)

	if _, err := q.once(t.Context()); err != nil {
		t.Fatalf("once: %v", err)
	}

	// IT WENT TO THE SPEECH RENTAL. Without the kind in Compat and the branch
	// in `once`, this request would have been handed to a resident sd-server
	// that has no route for it -- after a card had been paid for.
	if len(*sp.Calls) != 1 {
		t.Fatalf("the speech rental was asked %d time(s), want 1", len(*sp.Calls))
	}
	run := (*sp.Calls)[0]
	if len(run.Jobs) != 1 {
		t.Fatalf("%d utterance(s) reached the driver", len(run.Jobs))
	}

	// AND THE ID BECAME THE OPERATOR'S PATHS. This is the seal's live half:
	// the stored row names an id, the catalogue names the files, and this is
	// the only place the two meet.
	j := run.Jobs[0]
	if j.Voice != "/opt/voices/lessac.onnx" || j.EspeakData != "/e" {
		t.Errorf("resolved to voice=%q espeak=%q", j.Voice, j.EspeakData)
	}
	if j.Text != "Hello from ember." {
		t.Errorf("text = %q", j.Text)
	}

	// The row ended, and with no model: piper publishes none, which both
	// readers print as a dash and the ledger's footnote already explains.
	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusDone {
		t.Errorf("status = %q, want done", got.Status)
	}
	if got.Model != "" {
		t.Errorf("model = %q; piper publishes none, and a value here would be a reading of "+
			"the config rather than of a response", got.Model)
	}
}

// TestDrain_ASpeechRequestWhoseVoiceVanishedFailsAndSaysSo.
//
// The realistic way here is an operator editing `tts.voices` between the
// enqueue and the claim -- a queued request naming an id that no longer
// exists. It must fail with the list rather than with a resolution of
// whichever entry happens to be first.
func TestDrain_ASpeechRequestWhoseVoiceVanishedFailsAndSaysSo(t *testing.T) {
	st := genstore.NewRecordingStore()
	id, err := st.Enqueue(t.Context(), spokenRequest("x", "gone"))
	if err != nil {
		t.Fatal(err)
	}

	sp := newRecordingSpoken()
	pl := &fakePlanner{served: &fakeServed{}, spoken: sp}
	q, _ := newQueueOver(t, st, pl)
	q.Workloads = speechWorkloads(t)

	// The rental is opened and then fails, which is the honest shape: the
	// catalogue is this process's and the id was stored earlier, so nothing
	// before the claim can know the two disagree.
	if _, err := q.once(t.Context()); err == nil {
		t.Fatal("an unresolvable voice id did not fail the request")
	}

	if len(*sp.Calls) != 0 {
		t.Errorf("an unresolvable utterance reached the driver: %+v", *sp.Calls)
	}
	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Err, "lessac") {
		t.Errorf("reason = %q, want it to list the voices that exist", got.Err)
	}
}

// TestCompat_ASpeechRequestCannotRideAnImageRental.
//
// WITHOUT THE KIND IN Compat the provider and the GPU would match, the claim
// at the bottom of the drain loop would succeed, and an utterance would be
// handed to a resident sd-server -- after a card had been paid for. Asserted
// against the store's own claim rather than through a rental, because that is
// the statement the property lives in.
func TestCompat_ASpeechRequestCannotRideAnImageRental(t *testing.T) {
	st := genstore.NewRecordingStore()
	if _, err := st.Enqueue(t.Context(), spokenRequest("x", "lessac")); err != nil {
		t.Fatal(err)
	}

	// An sd rental is up.
	_, ok, err := st.ClaimNext(t.Context(), "lease-1",
		genstore.Compat{Kind: genstore.KindSD, Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a speech request was claimed onto an image rental")
	}

	// And a speech rental takes it.
	_, ok, err = st.ClaimNext(t.Context(), "lease-2",
		genstore.Compat{Kind: genstore.KindTTS, Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	// THE ACCEPTANCE BESIDE THE REFUSAL, because a refusal alone is
	// consistent with a Compat that refuses everything -- which is what a
	// zero one is.
	if !ok {
		t.Error("a speech rental could not claim a speech request")
	}
}

// TestCompat_TheVoiceIsNotInCompat, which is the measurement rather than an
// omission.
//
// A one-shot piper names its own `--model` per invocation and nothing is held
// open, so two requests wanting different voices are compatible: a rider with
// another voice is free. A resident driver is the only shape that would change
// it, and if one is ever built this test is what goes red.
func TestCompat_TheVoiceIsNotInCompat(t *testing.T) {
	st := genstore.NewRecordingStore()
	if _, err := st.Enqueue(t.Context(), spokenRequest("x", "thorsten")); err != nil {
		t.Fatal(err)
	}
	// A rental that came up for `lessac` claims it anyway.
	_, ok, err := st.ClaimNext(t.Context(), "lease",
		genstore.Compat{Kind: genstore.KindTTS, Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("a speech rental refused a request wanting a different voice; one-shot piper " +
			"names its model per invocation, so there is nothing to be incompatible about")
	}
}

// TestDrain_ASpeechRequestRecordsItsArtifacts, which is the other half of the
// join: an artifact nothing recorded is one the gallery and `ember history`
// cannot reach -- the preview frame's defect, one kind over.
func TestDrain_ASpeechRequestRecordsItsArtifacts(t *testing.T) {
	st := genstore.NewRecordingStore()
	id, err := st.Enqueue(t.Context(), spokenRequest("Hello.", "lessac"))
	if err != nil {
		t.Fatal(err)
	}
	pl := &fakePlanner{served: &fakeServed{}, spoken: newRecordingSpoken()}
	q, _ := newQueueOver(t, st, pl)
	q.Workloads = speechWorkloads(t)

	if _, err := q.once(t.Context()); err != nil {
		t.Fatalf("once: %v", err)
	}

	ims, err := st.Images(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ims) != 1 {
		t.Fatalf("%d artifact row(s), want 1", len(ims))
	}
	if !strings.HasSuffix(ims[0].Key, ".wav") {
		t.Errorf("key = %q, want a .wav: the route declares a content type from this extension",
			ims[0].Key)
	}
	if !strings.HasPrefix(ims[0].Key, string(id)+"/") {
		t.Errorf("key = %q, want it under the request's own prefix", ims[0].Key)
	}

	// AND THE PER-JOB ROW, with passes and steps at zero rather than invented.
	// A TTS job has no sampling pass and no step, so a figure there would be a
	// number with the authority of a measurement and no measurement behind it.
	runs, err := st.JobRuns(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d job row(s), want 1", len(runs))
	}
	r := runs[0]
	if r.Images != 1 || r.Bytes == 0 {
		t.Errorf("images/bytes = %d/%d; the artifact is real and the row should say so",
			r.Images, r.Bytes)
	}
	if r.Passes != 0 || r.Steps != 0 {
		t.Errorf("passes/steps = %d/%d; speech has neither, so a figure here would be invented",
			r.Passes, r.Steps)
	}
}
