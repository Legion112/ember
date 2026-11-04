package config

import (
	"strings"
	"testing"
)

// TestTTSCatalogue_IsEmptyUntilTheOperatorPublishesOne, which is the decision
// rather than a default.
//
// An implicit entry derived from `tts.voice` -- whose own default points at
// ember's own speech image -- would offer a browser a voice that cannot work
// wherever `image:` is not that one, and the refusal would arrive after a card
// was rented. So an empty list means this process serves no speech, and
// `POST /api/requests` says so naming the key.
func TestTTSCatalogue_IsEmptyUntilTheOperatorPublishesOne(t *testing.T) {
	for _, c := range []Config{
		{},
		// A voice and jobs for `ember up`, and still no catalogue: a config
		// file names one voice because it names one run.
		{TTS: TTSConfig{
			Voice: "/opt/voices/v.onnx",
			Jobs:  []TTSJob{{Text: "hello"}},
		}},
	} {
		cat, err := c.TTSCatalogue()
		if err != nil {
			t.Fatalf("TTSCatalogue: %v", err)
		}
		if len(cat) != 0 {
			t.Errorf("got %d published voice(s) from a config that published none: %+v", len(cat), cat)
		}
	}
}

func TestTTSCatalogue_CarriesEveryFieldAndDefaultsTheOnesItMay(t *testing.T) {
	c := Config{TTS: TTSConfig{
		EspeakData: "/shared/espeak",
		Voices: []TTSVoice{
			{ID: "lessac", Name: "Lessac (en_US)", Voice: "/opt/voices/lessac.onnx"},
			{ID: "thorsten", Voice: "/opt/voices/thorsten.onnx", EspeakData: "/own/espeak"},
		},
	}}
	cat, err := c.TTSCatalogue()
	if err != nil {
		t.Fatalf("TTSCatalogue: %v", err)
	}
	if len(cat) != 2 {
		t.Fatalf("%d entries, want 2", len(cat))
	}

	// THE ORDER IS THE OPERATOR'S, because it is what a UI shows -- the same
	// reason WriteConfig sorts its keys rather than letting a map decide.
	if cat[0].ID != "lessac" || cat[1].ID != "thorsten" {
		t.Errorf("order = %q, %q; want the file's own", cat[0].ID, cat[1].ID)
	}

	// The block's espeak_data fills an entry that named none, and an entry's
	// own wins: every entry repeating one path is a list with one typo
	// waiting in it.
	if cat[0].EspeakData != "/shared/espeak" {
		t.Errorf("espeak = %q, want the block's", cat[0].EspeakData)
	}
	if cat[1].EspeakData != "/own/espeak" {
		t.Errorf("espeak = %q, want the entry's own", cat[1].EspeakData)
	}

	// A missing name falls back to the id rather than to an empty string: a
	// UI rendering a blank option is one an operator cannot pick from.
	if cat[1].Name != "thorsten" {
		t.Errorf("name = %q, want the id", cat[1].Name)
	}
	if cat[0].Name != "Lessac (en_US)" {
		t.Errorf("name = %q", cat[0].Name)
	}
}

// TestTTSCatalogue_DefaultsEspeakToTheImagesWhenNobodyNamedOne, so a
// configuration using ember's own speech image needs one line per voice.
func TestTTSCatalogue_DefaultsEspeakToTheImagesWhenNobodyNamedOne(t *testing.T) {
	c := Config{TTS: TTSConfig{Voices: []TTSVoice{{ID: "a", Voice: "/v.onnx"}}}}
	cat, err := c.TTSCatalogue()
	if err != nil {
		t.Fatalf("TTSCatalogue: %v", err)
	}
	if cat[0].EspeakData != DefaultEspeakData {
		t.Errorf("espeak = %q, want %q", cat[0].EspeakData, DefaultEspeakData)
	}
}

// TestTTSCatalogue_RefusesThroughTheOneValidate asserts the key path rather
// than the wording, because the wording is internal/tts's and a second copy
// here is two places for one refusal to drift.
func TestTTSCatalogue_RefusesThroughTheOneValidate(t *testing.T) {
	c := Config{TTS: TTSConfig{Voices: []TTSVoice{
		{ID: "a", Voice: "/v.onnx"},
		{ID: "a", Voice: "/w.onnx"},
	}}}
	_, err := c.TTSCatalogue()
	if err == nil {
		t.Fatal("a duplicate voice id was accepted")
	}
	if !strings.Contains(err.Error(), "tts.voices[1]") {
		t.Errorf("error = %q, want it to name tts.voices[1]", err)
	}
}

// TestLoad_RefusesABadCatalogue, which is the whole reason the validation is
// at load rather than at lookup: a duplicate id's symptom is a UI offering a
// choice that cannot be made, read as ember being broken rather than as the
// operator's own typo.
func TestLoad_RefusesABadCatalogue(t *testing.T) {
	_, err := Load(write(t, `
provider: local
tts:
  voices:
    - id: "Lessac"
      voice: /v.onnx
`))
	if err == nil {
		t.Fatal("a config with an uppercase voice id was loaded")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("error = %q, want it to be about the id", err)
	}
}

// TestLoad_ReadsTheVoicesBlock drives it through YAML, because a wrong `yaml:`
// tag is invisible to every test that builds the struct in Go -- and
// KnownFields(true) makes a tag that does not match a refusal rather than a
// silent zero.
func TestLoad_ReadsTheVoicesBlock(t *testing.T) {
	c, err := Load(write(t, `
provider: local
tts:
  voices:
    - id: lessac
      name: Lessac (en_US, medium)
      voice: /opt/voices/en_US-lessac-medium.onnx
      espeak_data: /opt/piper/espeak-ng-data
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.TTS.Voices) != 1 {
		t.Fatalf("%d voice(s)", len(c.TTS.Voices))
	}
	v := c.TTS.Voices[0]
	if v.ID != "lessac" || v.Name != "Lessac (en_US, medium)" {
		t.Errorf("id/name = %q/%q", v.ID, v.Name)
	}
	if v.Voice != "/opt/voices/en_US-lessac-medium.onnx" || v.EspeakData != "/opt/piper/espeak-ng-data" {
		t.Errorf("voice/espeak = %q/%q", v.Voice, v.EspeakData)
	}
}
