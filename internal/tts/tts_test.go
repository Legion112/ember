package tts

import (
	"strings"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }
func i(v int) *int         { return &v }

// goodRun is a batch every check accepts, so a test that breaks one field is
// breaking exactly that field.
func goodRun() Run {
	return Run{
		Voice:      "/opt/voices/en_US-lessac-medium.onnx",
		EspeakData: "/opt/piper/espeak-ng-data",
		Jobs:       []Job{{Text: "Hello from ember.", Output: "hello"}},
	}
}

func TestRun_ValidateAcceptsABatchAndRefusesEachWayItCanBeWrong(t *testing.T) {
	// The acceptance is asserted BESIDE the refusals, because a refusal alone
	// is consistent with a Validate that refuses every batch there is -- which
	// is this repository's own rule about an assertion that cannot fail.
	if err := goodRun().Validate(); err != nil {
		t.Fatalf("a good run was refused: %v", err)
	}

	cases := []struct {
		name string
		edit func(*Run)
		want string
	}{
		{"no voice", func(r *Run) { r.Voice = "  " }, "tts.voice is empty"},
		{"no espeak data", func(r *Run) { r.EspeakData = "" }, "tts.espeak_data is empty"},
		{"no jobs", func(r *Run) { r.Jobs = nil }, "tts.jobs is empty"},
		{"a job with no text", func(r *Run) { r.Jobs[0].Text = " \n" }, "text is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := goodRun()
			c.edit(&r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestRun_ValidateNamesTheJobThatIsWrong asserts the index, because a batch of
// twenty with a typo in the nineteenth is the case this exists for: an error
// that says "a job is wrong" sends the operator to read all twenty.
func TestRun_ValidateNamesTheJobThatIsWrong(t *testing.T) {
	r := goodRun()
	r.Jobs = append(r.Jobs, Job{Text: "ok"}, Job{Text: ""})
	err := r.Validate()
	if err == nil {
		t.Fatal("a batch with an empty third utterance was accepted")
	}
	if !strings.Contains(err.Error(), "tts.jobs[2]") {
		t.Errorf("error = %q, want it to name tts.jobs[2]", err)
	}
}

func TestJob_ValidateBoundsEveryNumberPiperPublishes(t *testing.T) {
	cases := []struct {
		name string
		job  Job
		want string
	}{
		{"length_scale zero", Job{Text: "x", LengthScale: f(0)}, "length_scale"},
		{"length_scale negative", Job{Text: "x", LengthScale: f(-1)}, "length_scale"},
		{"length_scale too large", Job{Text: "x", LengthScale: f(10.5)}, "length_scale"},
		{"noise_scale negative", Job{Text: "x", NoiseScale: f(-0.1)}, "noise_scale"},
		{"noise_scale too large", Job{Text: "x", NoiseScale: f(11)}, "noise_scale"},
		{"noise_w negative", Job{Text: "x", NoiseW: f(-1)}, "noise_w"},
		{"noise_w too large", Job{Text: "x", NoiseW: f(10.1)}, "noise_w"},
		{"sentence_silence negative", Job{Text: "x", SentenceSilence: f(-1)}, "sentence_silence"},
		{"sentence_silence too large", Job{Text: "x", SentenceSilence: f(61)}, "sentence_silence"},
		{"speaker negative", Job{Text: "x", Speaker: i(-1)}, "speaker"},
		// Through Job.Validate and not only through ValidateOutput directly,
		// because a check that is written and not called is this
		// repository's own recurring defect.
		{"an output that is a path", Job{Text: "x", Output: "../a"}, "not a plain filename"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.job.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name %s", err, c.want)
			}
		})
	}
}

// TestJob_ZeroIsAValueWhereverPiperIsDefaultIsNot is the pointer rule's own
// assertion, and it is a test of what is ACCEPTED rather than refused: the
// defect it guards is a zero sentinel making these unrequestable.
func TestJob_ZeroIsAValueWhereverPipersDefaultIsNot(t *testing.T) {
	for _, j := range []Job{
		{Text: "x", Speaker: i(0)},
		{Text: "x", NoiseScale: f(0)},
		{Text: "x", NoiseW: f(0)},
		{Text: "x", SentenceSilence: f(0)},
	} {
		if err := j.Validate(); err != nil {
			t.Errorf("a deliberate zero was refused: %v", err)
		}
	}
}

func TestJob_ValidateBoundsTheText(t *testing.T) {
	j := Job{Text: strings.Repeat("a", MaxText+1)}
	err := j.Validate()
	if err == nil {
		t.Fatal("text past the bound was accepted")
	}
	// The bound and the size are both in the message, because "too long" with
	// no figures is a refusal an operator cannot act on.
	for _, want := range []string{"8001", "8000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	// Exactly the bound is accepted: "too large" and "exactly the bound" are
	// different answers, which is internal/s3's `do` rule one package over.
	if err := (Job{Text: strings.Repeat("a", MaxText)}).Validate(); err != nil {
		t.Errorf("text at exactly the bound was refused: %v", err)
	}
}

func TestValidateOutput_RefusesAnythingThatIsNotAPlainFilename(t *testing.T) {
	// An empty stem is fine: the caller numbers a job that does not name
	// itself, which app.SpokenStem does.
	if err := ValidateOutput(""); err != nil {
		t.Errorf("an empty stem was refused: %v", err)
	}
	for _, ok := range []string{"hello", "hello.world", "a_b-c", "001"} {
		if err := ValidateOutput(ok); err != nil {
			t.Errorf("stem %q was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"a/b", "../a", "/a", `a\b`, ".", "..", "-o", "a\nb", "a\x00b",
		// A traversal that path.Clean would ABSORB. internal/transfer shipped
		// the version that cleaned first, where the check could never fire,
		// so the assertion is that this is refused rather than rewritten.
		"a/../b",
	} {
		if err := ValidateOutput(bad); err == nil {
			t.Errorf("stem %q was accepted", bad)
		}
	}
}

// TestCommand_EveryValueIsAPositionalParameterAndNeverScriptText is the
// injection assertion: a prompt is arbitrary English, so a text that looks
// like shell has to arrive as data.
func TestCommand_EveryValueIsAPositionalParameterAndNeverScriptText(t *testing.T) {
	hostile := `"; rm -rf / #$(whoami)` + "`id`" + ` '\'' $HOME`
	r := goodRun()
	j := Job{Text: hostile, Output: "x"}
	cmd := r.Command(j, "/workspace/out/speech/x.wav")

	if cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("command does not start with sh -c: %q", cmd[:2])
	}
	if cmd[2] != Script {
		t.Error("the script body is not the constant: a value reached the script text")
	}
	if strings.Contains(Script, hostile) {
		t.Error("the hostile text is inside the script body")
	}
	// $0, then the four positional parameters in the order the script reads
	// them. Asserted by position, because the script names them $1..$4 and a
	// reordering here would be silent.
	want := []string{"ember-tts", hostile, r.Voice, r.EspeakData, "/workspace/out/speech/x.wav"}
	for n, w := range want {
		if cmd[3+n] != w {
			t.Errorf("argv[%d] = %q, want %q", 3+n, cmd[3+n], w)
		}
	}
}

func TestCommand_SendsOnlyTheFlagsThatWereSet(t *testing.T) {
	r := goodRun()

	// Nothing set: piper's own defaults apply and ember names none of them.
	// A flag carrying the value piper would have chosen is a flag that can
	// come to disagree with it.
	bare := r.Command(Job{Text: "x"}, "/o.wav")
	for _, flag := range []string{"--speaker", "--length_scale", "--noise_scale", "--noise_w", "--sentence_silence"} {
		for _, a := range bare {
			if a == flag {
				t.Errorf("%s was sent for a job that did not set it", flag)
			}
		}
	}

	full := r.Command(Job{
		Text: "x", Speaker: i(3), LengthScale: f(1.5), NoiseScale: f(0.5),
		NoiseW: f(0.25), SentenceSilence: f(0),
	}, "/o.wav")
	joined := strings.Join(full, " ")
	for _, want := range []string{
		"--speaker 3", "--length_scale 1.5", "--noise_scale 0.5",
		"--noise_w 0.25", "--sentence_silence 0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv does not carry %q: %s", want, joined)
		}
	}
}

// TestNum_WritesNoExponent is the one formatting decision with a failure
// behind it: strconv's %g writes 1e-07, which a reader of the log cannot
// match against what they typed.
func TestNum_WritesNoExponent(t *testing.T) {
	for _, v := range []float64{0.0000001, 1e20, 0.667} {
		if got := num(v); strings.ContainsAny(got, "eE") {
			t.Errorf("num(%v) = %q, which carries an exponent", v, got)
		}
	}
	if got := num(1); got != "1" {
		t.Errorf("num(1) = %q, want 1", got)
	}
}

// TestParseReport_ReadsPipersOwnLine uses the line verbatim from the pinned
// release, captured from a real run rather than written from documentation --
// which is the calibration habit the RunPod volume API set and the mistake
// `capabilities.model` made.
func TestParseReport_ReadsPipersOwnLine(t *testing.T) {
	const line = `[2026-11-04 19:32:46.483] [piper] [info] Real-time factor: 0.05235811090877757 (infer=0.062003401 sec, audio=1.18421768707483 sec)`
	got, ok := ParseReport(line)
	if !ok {
		t.Fatal("piper's own line did not parse")
	}
	// Compared with a tolerance, because the figures are seconds as a float
	// and the assertion is about the conversion rather than the last bit.
	if d := got.Audio - 1184*time.Millisecond; d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("audio = %v, want about 1.184s", got.Audio)
	}
	if d := got.Infer - 62*time.Millisecond; d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("infer = %v, want about 62ms", got.Infer)
	}
}

// TestParseReport_ASecondsFloatDoesNotBecomeNanoseconds is the conversion
// defect this would otherwise have: time.Duration(4.7) is four nanoseconds,
// so a reader would record every utterance as instantaneous.
func TestParseReport_ASecondsFloatDoesNotBecomeNanoseconds(t *testing.T) {
	got, ok := ParseReport("Real-time factor: 0.03 (infer=0.17 sec, audio=4.71 sec)")
	if !ok {
		t.Fatal("did not parse")
	}
	if got.Audio < time.Second {
		t.Errorf("audio = %v, want seconds: a float of seconds was read as nanoseconds", got.Audio)
	}
}

func TestParseReport_RefusesWhatIsNotThatLine(t *testing.T) {
	for _, line := range []string{
		"",
		"[piper] [info] Loaded voice in 0.188778445 second(s)",
		"[piper] [info] Initialized piper",
		"[piper] [info] Terminated piper",
		// The marker with one of the two figures missing. Both or neither:
		// half a report is a figure an operator would read as a measurement.
		"Real-time factor: 0.03 (infer=0.17 sec)",
		"Real-time factor: 0.03 (audio=4.71 sec)",
		"Real-time factor: 0.03 (infer=banana sec, audio=4.71 sec)",
		"Real-time factor: 0.03 (infer=-1 sec, audio=4.71 sec)",
		"Real-time factor: 0.03 (infer= sec, audio=4.71 sec)",
	} {
		if r, ok := ParseReport(line); ok {
			t.Errorf("ParseReport(%q) = %+v, true; want false", line, r)
		}
	}
}

func TestJob_DescribeIsOneLineAndCutsOnARuneBoundary(t *testing.T) {
	j := Job{Text: "  a\n  lovely   cat  ", Output: "cat"}
	got := j.Describe()
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("Describe returned more than one line: %q", got)
	}
	if !strings.Contains(got, "a lovely cat") {
		t.Errorf("Describe = %q, want the text with its whitespace collapsed", got)
	}
	if !strings.Contains(got, "cat"+Ext) {
		t.Errorf("Describe = %q, want it to name the artifact", got)
	}

	// Multi-byte, and longer than the bound: cutting on a byte index would
	// put a replacement character in the operator's log.
	long := Job{Text: strings.Repeat("д", 200)}
	if d := long.Describe(); strings.Contains(d, "�") {
		t.Errorf("Describe cut a rune in half: %q", d)
	}

	// With no output name, the text alone -- app.SpokenStem is what numbers
	// it, and naming a stem here that the caller may not use would be a
	// second spelling of the filename.
	if d := (Job{Text: "x"}).Describe(); strings.Contains(d, Ext) {
		t.Errorf("Describe named an artifact for a job with no output: %q", d)
	}
}
