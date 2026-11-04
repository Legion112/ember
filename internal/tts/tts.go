// Package tts is the speech driver: one narrow interface onto piper, the way
// internal/sdserver is one onto stable-diffusion.cpp.
//
// WHY THERE IS NO SERVER HERE, which is the whole shape of this package and
// the one thing a reader should take from it. internal/sdserver exists because
// sd-server is an HTTP server ember can hold open, submit to and poll -- and
// that is what buys the resident model and the progress stream. piper has no
// server mode at all: the pinned release's own `--help` lists an output file,
// an output directory, `--output_raw` and `--json-input`, and every one of
// them reads its text from STDIN. So the resident shape is reachable only by
// holding a process open and writing to its stdin, and ember's remote-command
// seam (sshx.Runner) hands back stdout, stderr and a wait and has no stdin --
// deliberately, which AGENTS.md records as the line not crossed for a test's
// convenience.
//
// MEASURED RATHER THAN ASSUMED, because #36 says the crossover decides whether
// a typed path is worth writing at all. en_US-lessac-medium, eight utterances
// of about 4.6 s of audio each, in one container on this machine:
//
//	one piper process per utterance   4740 ms   (592 ms each)
//	one piper process for all eight   1822 ms   (185 ms load + ~205 ms each)
//
// So a fresh process costs about 400 ms of fixed overhead -- 185 ms of it the
// voice load, the rest process start and onnxruntime init -- against inference
// of 170-220 ms per utterance. The resident shape is therefore worth about
// 2.9x per utterance and the crossover is at TWO, not the ~5 the sd15 run
// found: piper's fixed cost is a sixth of sd15's and its per-job work is a
// tenth. #36 guessed "probably the bigger win proportionally" and was right.
//
// IT IS STILL NOT BUILT, and the reason is a seam rather than a measurement.
// 400 ms per utterance against a RENTAL whose own overhead AGENTS.md measures
// at ~27 s is noise until a batch is large; the resident version needs stdin
// on sshx.Runner, which is a production interface widened for one driver, and
// the honest order is to take the one-shot measurement on a real batch first.
// What this package is careful about is that the shape above is a FACT with a
// number beside it rather than a thing nobody tried.
//
// WHAT PROGRESS LOOKS LIKE, which #190 names as the part that cannot be
// generalised from a config, and the answer is that for speech there is none
// WITHIN a job. piper emits, per utterance and after it is finished:
//
//	[piper] [info] Real-time factor: 0.0367 (infer=0.173 sec, audio=4.713 sec)
//
// and nothing at all between the start of an utterance and that line. There is
// no step, no pass and no percentage: a TTS job is atomic and its only
// intra-job observable is that it has not finished yet. So the unit of
// progress is the JOB, which is what report.UnitJob already is -- and the two
// numbers on that line are worth carrying because they are the artifact's own
// duration and what it cost to make, which is the speech equivalent of a step
// count and is not derivable from anything else ember sees.
//
// THOSE TWO NUMBERS ARE CUMULATIVE WITHIN A PROCESS AND PER-JOB ACROSS ONE,
// measured on the eight-utterance run above: `audio=` walked 4.71, 9.18,
// 13.68 ... 36.58 and `infer=` 0.173, 0.311, 0.507 ... 1.530, which are
// running totals and not per-utterance figures. One process per job is
// therefore not only the simplest shape, it is the one in which the line means
// what it reads as -- and a future resident driver has to subtract, which is
// written here because the obvious reading of that line is wrong.
package tts

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// Run is a batch of utterances.
//
// THE VOICE IS PER JOB AND NOT PER RUN, which is a consequence of the
// measurement rather than a preference. It was on the Run first, with the
// reason that `sd.serve` is one server per rental -- true of a RESIDENT
// driver, which would have one process to hold open and therefore one voice
// loaded. One-shot has no such process: every invocation names its own
// `--model`, so a batch mixing voices costs exactly nothing and a rider
// claimed onto the same rental needs no compatibility check for it. A field
// on the Run would have been a constraint inherited from a design that was
// measured and not taken.
type Run struct {
	// Jobs is the batch, in the order it will be spoken.
	Jobs []Job
}

// Job is one utterance.
//
// EVERY FIELD IS A VALUE PIPER PUBLISHES IN ITS OWN --help AND NOTHING ELSE,
// which is AGENTS.md's "a value set nobody has observed is not typed" applied
// to a binary rather than to an API. piper's flags are the whole surface there
// is: there is no /capabilities to read, so the list cannot grow by reading a
// response and a field invented here would be a field nothing accepts.
type Job struct {
	// Text is what to say. Arbitrary operator text, which is why it reaches
	// the node as a positional parameter and never as script text.
	Text string

	// Output is the filename stem, without an extension.
	Output string

	// Voice is the .onnx on the node. piper finds the .json beside it by
	// appending the suffix itself, which is why there is one field.
	//
	// IT IS SET FROM A CATALOGUE ENTRY AND NEVER FROM A REQUEST. `Ask` is the
	// shape a request stores and it has no field for this; Ask.Resolve is the
	// one place a path enters, and it takes it from the list the operator
	// published. See catalogue.go.
	Voice string

	// EspeakData is the espeak-ng data directory on the node.
	//
	// NAMED AND NEVER LEFT TO A DEFAULT. piper resolves it relative to its own
	// argv[0] when the flag is absent, so it works from /opt/piper and not
	// from anywhere else -- and `ssh host command` runs from the home
	// directory. The image sets ESPEAK_DATA_PATH too, and that is belt and
	// braces rather than the mechanism: AGENTS.md records that an image's ENV
	// does not reach anything sshd starts.
	EspeakData string

	// Speaker is which voice of a multi-speaker model to use.
	//
	// A POINTER, because piper's own default is 0 and 0 is a real speaker id:
	// zero-is-unset would make the first speaker unrequestable while looking
	// exactly like a field that works, which is clip_skip's rule a fourth
	// time.
	Speaker *int

	// LengthScale is phoneme length: above 1 is slower, below is faster.
	// NoiseScale is generator noise and NoiseW phoneme-width noise.
	// SentenceSilence is seconds of silence after each sentence.
	//
	// All pointers, and all for the same reason as Speaker: piper's defaults
	// are 1.0, 0.667, 0.8 and 0.2, so none of them is zero and a zero
	// sentinel would make "no silence at all" and "the fastest speech this
	// model will do" unrepresentable.
	LengthScale     *float64
	NoiseScale      *float64
	NoiseW          *float64
	SentenceSilence *float64
}

// Describe is one line identifying this job, for a progress event.
//
// The text is truncated because an utterance can be a paragraph and this is a
// line in a terminal, and on a rune boundary because a prompt is arbitrary
// text and cutting a multi-byte character in half puts a replacement
// character in an operator's log.
func (j Job) Describe() string {
	t := strings.Join(strings.Fields(j.Text), " ")
	const max = 60
	if r := []rune(t); len(r) > max {
		t = string(r[:max]) + "…"
	}
	if j.Output != "" {
		return fmt.Sprintf("%q -> %s%s", t, j.Output, Ext)
	}
	return fmt.Sprintf("%q", t)
}

// Ext is the extension every artifact this driver produces carries.
//
// A CONSTANT AND NOT A FORMAT OPTION, because piper writes a RIFF/WAVE file
// and has no flag that changes that: `--output_raw` is headerless PCM to
// stdout, which is a different mechanism rather than a format. An
// `output_format` field here would be the allowedCudaVersions mistake -- a
// value read from nowhere -- and a browser offered a choice of one is a
// control that does nothing.
const Ext = ".wav"

// NameMax is the longest single filename every filesystem ember writes to
// accepts.
//
// Not a figure ember chose: NAME_MAX is 255 bytes on ext4, xfs, btrfs and
// overlayfs, which is every filesystem a node ember rents writes to.
const NameMax = 255

// suffixReserve is how much of a filename ember composes around the stem.
//
// `SpokenStem` writes "<stem>-%02d" and the extension is four bytes, so the
// suffix is a dash, the index's digits and ".wav". TEN DIGITS ARE RESERVED
// rather than the two the format pads to, because `%02d` is a MINIMUM width
// and nothing bounds how many jobs a batch holds -- so the index's width is
// the batch's size, and reserving for the two-digit case is a bound that is
// correct until somebody queues a hundred utterances.
//
// FOUND BY THE ARITHMETIC TEST BESIDE IT AND NOT BY REVIEW: the first version
// of this reserved EIGHT, derived from "-100.wav", and
// TestMaxOutput_LeavesRoomForTheSuffixEmberComposes went red on "-1000.wav"
// being nine. Over-reserving is the safe direction and costs nine bytes of a
// name nobody types, which is why it is ten and not four.
const suffixReserve = len("-") + 10 + len(Ext)

// MaxOutput bounds a filename stem, and the bound is DERIVED rather than
// chosen.
//
// WHY THERE HAS TO BE ONE AT ALL, which is ValidateOutput's own stated purpose
// read back: it exists so that a stem which cannot work is refused "before a
// card is rented", and without a length check a 100,000-character stem passed
// every check here, reached `piper --output_file` on a rented machine and
// failed THERE -- after the create call, the wait for sshd and the image pull
// had all been paid for. The stem is the SECOND request-supplied string that
// reaches a filename, after the voice id, and it was the one with no bound
// while `MaxVoiceID` is 64 and `MaxText` is 8000 and both say why.
//
// NameMax minus what ember composes around it, so the figure is arithmetic
// rather than taste -- a round number here would be ember's preference
// standing in for a filesystem's limit, and the thing being prevented is
// exactly the filesystem's refusal.
//
// It is therefore NOT a judgement about what length of name an operator might
// mean: 240 bytes is longer than any name anybody types, and a stem that long
// is a paste rather than a choice. What the bound buys is that the refusal
// arrives here instead of on the node.
const MaxOutput = NameMax - suffixReserve

// MaxText bounds one utterance.
//
// Not a limit piper publishes: it has none, and it will happily synthesise a
// megabyte of text into a wav nobody wants on a card that is billing for it.
// The bound is ember's and the figure is stated rather than derived -- about a
// thousand words, which at this voice's measured ~4.6 s of audio per
// fifteen-word utterance is on the order of five minutes of speech.
//
// It matters because the text is the one field a request could one day carry,
// and the thing an unbounded one buys is GPU time rather than bytes: the same
// argument blob's axis bound makes about a two-kilobyte PNG declaring
// 100000x100000.
const MaxText = 8000

// Validate refuses a batch before anything is rented.
//
// The cheapest moment to find a typo is before a card is paid for, which is
// the rule sdserver.Validate follows. Every bound here is piper's own, read
// off its --help and its observed refusals, except MaxText and MaxOutput --
// both of which are ember's and say so, the second derived from NAME_MAX
// because what it prevents is a filesystem's refusal on a rented machine.
func (r Run) Validate() error {
	if len(r.Jobs) == 0 {
		return errors.New("tts.jobs is empty: there is nothing to say")
	}
	for i, j := range r.Jobs {
		if err := j.Validate(); err != nil {
			return fmt.Errorf("tts.jobs[%d]: %w", i, err)
		}
	}
	return nil
}

// Validate refuses one job.
func (j Job) Validate() error {
	if strings.TrimSpace(j.Voice) == "" {
		return errors.New("voice is empty: it names the .onnx voice on the node, " +
			"for example /opt/voices/en_US-lessac-medium.onnx")
	}
	if strings.TrimSpace(j.EspeakData) == "" {
		return errors.New("espeak_data is empty: it names piper's espeak-ng data " +
			"directory on the node, for example /opt/piper/espeak-ng-data")
	}
	if strings.TrimSpace(j.Text) == "" {
		return errors.New("text is empty: there is nothing to say")
	}
	if len(j.Text) > MaxText {
		return fmt.Errorf("text is %d bytes, above ember's bound of %d: "+
			"an utterance this long is minutes of speech on a card that is billing for it, "+
			"so split it across jobs", len(j.Text), MaxText)
	}
	if err := ValidateOutput(j.Output); err != nil {
		return err
	}
	// piper's own bounds, from its --help defaults and from what it accepts.
	// A negative scale is the one that is not merely odd: onnxruntime takes it
	// and produces silence or noise, billed either way.
	if j.LengthScale != nil && (*j.LengthScale <= 0 || *j.LengthScale > 10) {
		return fmt.Errorf("length_scale %v is outside 0 to 10 (piper's default is 1.0)", *j.LengthScale)
	}
	if j.NoiseScale != nil && (*j.NoiseScale < 0 || *j.NoiseScale > 10) {
		return fmt.Errorf("noise_scale %v is outside 0 to 10 (piper's default is 0.667)", *j.NoiseScale)
	}
	if j.NoiseW != nil && (*j.NoiseW < 0 || *j.NoiseW > 10) {
		return fmt.Errorf("noise_w %v is outside 0 to 10 (piper's default is 0.8)", *j.NoiseW)
	}
	if j.SentenceSilence != nil && (*j.SentenceSilence < 0 || *j.SentenceSilence > 60) {
		return fmt.Errorf("sentence_silence %v is outside 0 to 60 seconds (piper's default is 0.2)", *j.SentenceSilence)
	}
	if j.Speaker != nil && *j.Speaker < 0 {
		return fmt.Errorf("speaker %d is negative", *j.Speaker)
	}
	return nil
}

// ValidateOutput refuses a stem that is not a plain filename.
//
// REFUSED AND NOT SANITISED, which is the opposite of sdserver.ValidateOutput
// and is deliberate rather than an inconsistency. That one flattens, because
// the stem names a file ember writes HERE and "write the file the operator
// meant" is the useful behaviour. This stem reaches `--output_file` on a
// rented machine, as a positional parameter in a path ember composes -- so
// there is a second mechanism in the way (the output directory is ember's) and
// the useful behaviour is to say so before a card is rented.
//
// Refusing is also what keeps the flattening rule in ONE place: a second copy
// of sdserver's safeStem here is two copies of a containment rule, which is
// the defect AGENTS.md records about the journal's "nodes" subdirectory.
//
// A DENY-LIST WHERE THE VOICE ID GOT AN ALLOW-LIST, and the asymmetry is
// deliberate rather than an oversight. An id is a TOKEN: it is compared with
// == , a UI round-trips it, and nothing legitimate needs a space or a capital
// in one -- so the narrow set costs nothing. A stem is a FILENAME an operator
// chose, and `blob.ValidateKey` and `sdserver`'s own stem rule are both
// deny-lists for that reason: an allow-list there would refuse a space, an
// accent or a CJK character, all of which are names somebody means. What the
// two have in common is that neither is cleaned -- a hostile name is refused,
// because path.Clean absorbs a traversal and a check that cleaned first could
// never fire.
//
// An empty stem is fine: the caller numbers a job that does not name itself.
func ValidateOutput(stem string) error {
	if stem == "" {
		return nil
	}
	if len(stem) > MaxOutput {
		return fmt.Errorf("output is %d bytes, above the bound of %d: ember appends up to %d "+
			"more for the index and the extension, and a filename longer than %d bytes is "+
			"refused by the filesystem on the node -- after a card has been paid for",
			len(stem), MaxOutput, suffixReserve, NameMax)
	}
	if stem != path.Base(stem) || strings.ContainsAny(stem, `\/`) {
		return fmt.Errorf("output %q is not a plain filename: a stem names one file in the run's "+
			"output directory, and ember composes the path", stem)
	}
	switch stem {
	case ".", "..":
		return fmt.Errorf("output %q is not a filename", stem)
	}
	if strings.HasPrefix(stem, "-") {
		// piper takes "-" as stdout for --output_file, so a stem starting with
		// one is a name that could be read as a flag by something downstream.
		return fmt.Errorf("output %q starts with a dash, which a program reading it would take "+
			"for a flag", stem)
	}
	if strings.ContainsAny(stem, "\x00\n\r") {
		return fmt.Errorf("output %q contains a NUL or a newline", stem)
	}
	return nil
}

// Script runs one utterance and is the only shell text this package has.
//
// EVERY VALUE IS A POSITIONAL PARAMETER, never script text -- the rule
// nodemem.Script follows and the reason it follows it: a remote command is
// argv handed to the node's shell, so a prompt interpolated into a script body
// is a shell injection whose input is the operator's own config. "$1" is
// arbitrary English and cannot be anything else here.
//
// The text reaches piper on STDIN because that is the only way piper takes it;
// `printf '%s\n'` rather than `echo`, because echo's treatment of a leading
// `-` and of backslashes is shell-dependent and the text is arbitrary.
//
// Everything after "$4" is piper's own flags, forwarded through "$@" so a
// numeric option is a parameter too. The shift is five because $0 is the
// program name ember gives it.
// The directory is created here rather than relied on in the image, because an
// image that predates it is exactly the case AGENTS.md records for
// /workspace/out itself: a workload that "failed at the tar with Cannot open:
// No such file or directory" because the directory was never there. `mkdir -p`
// over an existing directory is a success, which is the "already there has two
// meanings" rule where the two meanings do not differ.
const Script = `
text=$1; model=$2; espeak=$3; outfile=$4
shift 4
mkdir -p "$(dirname "$outfile")"
printf '%s\n' "$text" | piper \
	--model "$model" \
	--espeak_data "$espeak" \
	--output_file "$outfile" \
	"$@"
`

// Command is one utterance as argv.
//
// outPath is absolute and ember composed it; the stem it came from has already
// been through ValidateOutput. A METHOD ON THE JOB and not on the Run, because
// the voice is the job's -- so there is no shape of this call that can take
// one job's text with another's model.
func (j Job) Command(outPath string) []string {
	// $0 is named rather than empty, so a `ps` on the node during a run says
	// which program is speaking.
	cmd := []string{"sh", "-c", Script, "ember-tts", j.Text, j.Voice, j.EspeakData, outPath}

	// Only what was set. piper's defaults are its own and a flag carrying the
	// value piper would have chosen is a flag that can come to disagree with
	// it -- the "a block with a switch sends nothing when the switch is off"
	// rule, one program over.
	if j.Speaker != nil {
		cmd = append(cmd, "--speaker", strconv.Itoa(*j.Speaker))
	}
	if j.LengthScale != nil {
		cmd = append(cmd, "--length_scale", num(*j.LengthScale))
	}
	if j.NoiseScale != nil {
		cmd = append(cmd, "--noise_scale", num(*j.NoiseScale))
	}
	if j.NoiseW != nil {
		cmd = append(cmd, "--noise_w", num(*j.NoiseW))
	}
	if j.SentenceSilence != nil {
		cmd = append(cmd, "--sentence_silence", num(*j.SentenceSilence))
	}
	return cmd
}

// num formats a flag value without an exponent.
//
// 'f' and not 'g': strconv's %g writes 1e-07 for a small scale, and a C++
// std::stof reads that correctly while a reader of the log does not. -1
// precision keeps the shortest exact representation.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Report is what piper said about one utterance.
//
// Audio is the duration of the artifact and Infer is what it took to make.
// Both are read off the one line piper emits per utterance, and the ratio it
// prints beside them is not kept: a stored quotient is a number that can be
// wrong while the terms it came from are right, which is why nothing derived
// is stored anywhere else here either.
type Report struct {
	Audio time.Duration
	Infer time.Duration
}

// ParseReport reads piper's per-utterance line, if that is what this is.
//
// A PARSE OF ITS STDERR AND NOT A STATUS, which is the one place this driver
// is weaker than the sd one and is stated rather than hidden. sd-server
// answers a JSON object with a status in it; piper's only account of a
// finished utterance is a log line, so what ember records about a job's own
// timing rests on a format upstream never promised. What it is NOT used for is
// deciding whether the job worked: that is the exit status, which is the rule
// AGENTS.md records about reading a status and not a log -- a line that stops
// parsing costs the two figures and nothing else.
//
// The line, verbatim from the pinned release:
//
//	[2026-11-04 19:32:46.483] [piper] [info] Real-time factor: 0.052 (infer=0.062 sec, audio=1.184 sec)
func ParseReport(line string) (Report, bool) {
	const marker = "Real-time factor:"
	i := strings.Index(line, marker)
	if i < 0 {
		return Report{}, false
	}
	infer, iok := secondsAfter(line[i:], "infer=")
	audio, aok := secondsAfter(line[i:], "audio=")
	if !iok || !aok {
		return Report{}, false
	}
	return Report{Audio: audio, Infer: infer}, true
}

// secondsAfter reads `<key><float> sec` out of s.
func secondsAfter(s, key string) (time.Duration, bool) {
	i := strings.Index(s, key)
	if i < 0 {
		return 0, false
	}
	rest := s[i+len(key):]
	j := strings.IndexFunc(rest, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.' && r != '-' && r != '+' && r != 'e' && r != 'E'
	})
	if j <= 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(rest[:j], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	// Seconds as a float into a Duration, which is where a naive conversion
	// loses everything below a second: time.Duration(v) would make 4.7 seconds
	// four nanoseconds.
	return time.Duration(v * float64(time.Second)), true
}
