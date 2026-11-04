package config

import (
	"strings"
	"testing"
)

func TestParseTTS_EmptyIsNotAnError(t *testing.T) {
	// The shape ParseSD and ParseAssets follow: a config that does not use
	// this block is not a config with a mistake in it.
	run, err := Config{}.ParseTTS()
	if err != nil {
		t.Fatalf("an empty tts block was refused: %v", err)
	}
	if len(run.Jobs) != 0 {
		t.Errorf("an empty tts block produced %d job(s)", len(run.Jobs))
	}
}

// TestParseTTS_AVoiceWithNoJobsIsNotRefused is the asymmetry with
// `sd.serve`-with-no-`sd.jobs` and is asserted rather than left to be read out
// of the code: that one rents a card, loads the weights and generates nothing,
// which costs the whole TTL. This one starts no process and costs nothing.
func TestParseTTS_AVoiceWithNoJobsIsNotRefused(t *testing.T) {
	c := Config{TTS: TTSConfig{Voice: "/opt/voices/x.onnx"}}
	run, err := c.ParseTTS()
	if err != nil {
		t.Fatalf("a voice with no jobs was refused: %v", err)
	}
	if len(run.Jobs) != 0 {
		t.Errorf("got %d job(s)", len(run.Jobs))
	}
	// And nothing was defaulted either: a Run nobody will use must not look
	// like a configured one to a reader of a dry run.
	if len(run.Jobs) != 0 {
		t.Errorf("a block with no jobs produced %d", len(run.Jobs))
	}
}

func TestParseTTS_DefaultsToEmbersOwnImagePaths(t *testing.T) {
	c := Config{TTS: TTSConfig{Jobs: []TTSJob{{Text: "hello"}}}}
	run, err := c.ParseTTS()
	if err != nil {
		t.Fatalf("ParseTTS: %v", err)
	}
	if run.Jobs[0].Voice != DefaultVoice {
		t.Errorf("voice = %q, want %q", run.Jobs[0].Voice, DefaultVoice)
	}
	if run.Jobs[0].EspeakData != DefaultEspeakData {
		t.Errorf("espeak_data = %q, want %q", run.Jobs[0].EspeakData, DefaultEspeakData)
	}
}

func TestParseTTS_AConfiguredPathWins(t *testing.T) {
	c := Config{TTS: TTSConfig{
		Voice:      "/mnt/weights/de_DE-thorsten-high.onnx",
		EspeakData: "/usr/share/espeak-ng-data",
		Jobs:       []TTSJob{{Text: "hallo"}},
	}}
	run, err := c.ParseTTS()
	if err != nil {
		t.Fatalf("ParseTTS: %v", err)
	}
	if run.Jobs[0].Voice != "/mnt/weights/de_DE-thorsten-high.onnx" {
		t.Errorf("voice = %q", run.Jobs[0].Voice)
	}
	if run.Jobs[0].EspeakData != "/usr/share/espeak-ng-data" {
		t.Errorf("espeak_data = %q", run.Jobs[0].EspeakData)
	}
}

// TestParseTTS_BlankIsNotAPath, because " " in a YAML scalar is what a hand
// edit leaves behind and a voice of one space would reach piper as a file it
// cannot open -- after the node was rented.
func TestParseTTS_BlankIsNotAPath(t *testing.T) {
	c := Config{TTS: TTSConfig{Voice: "   ", EspeakData: "\t", Jobs: []TTSJob{{Text: "x"}}}}
	run, err := c.ParseTTS()
	if err != nil {
		t.Fatalf("ParseTTS: %v", err)
	}
	if run.Jobs[0].Voice != DefaultVoice || run.Jobs[0].EspeakData != DefaultEspeakData {
		t.Errorf("a blank path did not fall back: voice=%q espeak=%q",
			run.Jobs[0].Voice, run.Jobs[0].EspeakData)
	}
}

// TestParseTTS_CarriesEveryFieldAcross is the one assertion that catches a
// field added to TTSJob and not to the conversion -- which is the "built end
// to end and never connected" defect in miniature: the YAML would parse, the
// value would be dropped, and the operator's utterance would be spoken at the
// default while the config says otherwise.
func TestParseTTS_CarriesEveryFieldAcross(t *testing.T) {
	sp, ls, ns, nw, ss := 2, 1.25, 0.5, 0.75, 0.1
	c := Config{TTS: TTSConfig{Jobs: []TTSJob{{
		Text: "hello", Output: "greeting",
		Speaker: &sp, LengthScale: &ls, NoiseScale: &ns, NoiseW: &nw, SentenceSilence: &ss,
	}}}}
	run, err := c.ParseTTS()
	if err != nil {
		t.Fatalf("ParseTTS: %v", err)
	}
	j := run.Jobs[0]
	if j.Text != "hello" || j.Output != "greeting" {
		t.Errorf("text/output = %q/%q", j.Text, j.Output)
	}
	for name, got := range map[string]*float64{
		"length_scale": j.LengthScale, "noise_scale": j.NoiseScale,
		"noise_w": j.NoiseW, "sentence_silence": j.SentenceSilence,
	} {
		if got == nil {
			t.Errorf("%s was dropped on the way across", name)
		}
	}
	if j.Speaker == nil || *j.Speaker != 2 {
		t.Errorf("speaker = %v", j.Speaker)
	}
}

// TestParseTTS_RefusesThroughTheOneValidate asserts the key path rather than
// the wording, because the wording is internal/tts's and a second copy of it
// here is two places for one refusal to drift.
func TestParseTTS_RefusesThroughTheOneValidate(t *testing.T) {
	c := Config{TTS: TTSConfig{Jobs: []TTSJob{{Text: "ok"}, {Text: ""}}}}
	_, err := c.ParseTTS()
	if err == nil {
		t.Fatal("a batch with an empty utterance was accepted")
	}
	if !strings.Contains(err.Error(), "tts.jobs[1]") {
		t.Errorf("error = %q, want it to name tts.jobs[1]", err)
	}
}

// TestLoad_ReadsTheTTSBlock drives it through YAML rather than through a
// struct literal, because a wrong `yaml:` tag is invisible to every test that
// builds the struct in Go -- and KnownFields(true) means a tag that does not
// match is a refusal rather than a silent zero.
func TestLoad_ReadsTheTTSBlock(t *testing.T) {
	const y = `
provider: local
image: ember:tts
ttl: 10m
tts:
  voice: /opt/voices/v.onnx
  espeak_data: /opt/piper/espeak-ng-data
  jobs:
    - text: "the first thing"
      output: first
      speaker: 0
      length_scale: 1.1
      noise_scale: 0.6
      noise_w: 0.8
      sentence_silence: 0.25
`
	c, err := Load(write(t, y))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.TTS.Jobs) != 1 {
		t.Fatalf("got %d job(s)", len(c.TTS.Jobs))
	}
	j := c.TTS.Jobs[0]
	if j.Text != "the first thing" || j.Output != "first" {
		t.Errorf("text/output = %q/%q", j.Text, j.Output)
	}
	// A speaker of zero through YAML, which is the pointer rule's own case:
	// decoded as Some(0) and not as absent.
	if j.Speaker == nil {
		t.Fatal("speaker: 0 decoded as absent")
	}
	if *j.Speaker != 0 {
		t.Errorf("speaker = %d", *j.Speaker)
	}
	if c.TTS.Voice != "/opt/voices/v.onnx" {
		t.Errorf("voice = %q", c.TTS.Voice)
	}
	run, err := c.ParseTTS()
	if err != nil {
		t.Fatalf("ParseTTS over a loaded file: %v", err)
	}
	if len(run.Jobs) != 1 {
		t.Errorf("ParseTTS produced %d job(s)", len(run.Jobs))
	}
}
