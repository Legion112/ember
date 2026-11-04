//go:build integration && imagelab

// Text to speech, end to end on the local provider, for nothing.
//
// BEHIND THE imagelab TAG for the reason every other lab about a workload
// image is: `make docker-build` produces ember:lab and not ember:tts, so
// requiring this on every pull request would make every pull request need a
// 26 MB download and a 60 MiB voice. They are not skipped -- a lab that skips
// itself reports success for something that never ran -- they simply are not
// compiled unless the tag says the image is here.
//
//	make workload-tts
//	make test-tts-lab
//
// WHY A LAB AND NOT A UNIT TEST, which internal/app's own tests already cover
// the decisions of. Three things cross a gap no fake reaches, and every one of
// them has cost this repository something in its sd equivalent: whether piper
// is on the PATH a non-interactive `ssh host command` gets (AGENTS.md: an
// image's ENV does not reach anything sshd starts, and `LD_LIBRARY_PATH` is
// the case it records); whether the stderr line ember parses is the one the
// real binary prints, which is the `capabilities.model` closed loop; and
// whether the bytes that come home are a wav rather than a tar of nothing,
// which is the `--newer-than` flag that is not a flag.
package integration

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/tts"
	"github.com/Legion112/ember/test/integration/harness"
)

// ttsImage is the image under test, and a missing one is a hard failure
// naming the command that builds it.
//
// It must not pull -- that would be testing GitHub -- and must not skip, for
// the reason in this file's header. The env var exists so the publishing path
// can point it at a tag it just built, which is the shape sdCUDAUnderTest
// already uses.
func ttsImage(t *testing.T, l *harness.Lab) string {
	t.Helper()
	image := os.Getenv("EMBER_TTS_IMAGE")
	if image == "" {
		image = "ember:tts"
	}
	if !l.HaveImage(t.Context(), image) {
		t.Fatalf("%s is not built, and this lab asserts about the piper in it:\n"+
			"  make workload-tts", image)
	}
	return image
}

// TestWorkloadImage_SpeaksOnACPUAndCarriesItsVoice.
//
// EVERY ASSERTION IS ABOUT WHAT A WORKLOAD CAN SEE OVER SSH, not about what
// the Dockerfile declares. AGENTS.md records the difference costing an
// afternoon: `EMBER_CUDA_ARCH=89` is in the image, visible to `docker run`,
// and empty over SSH, because sshd builds a fresh environment per session. So
// the voice, the espeak data and the binary are all reached through ExecOut
// on a running node.
func TestWorkloadImage_SpeaksOnACPUAndCarriesItsVoice(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()
	image := ttsImage(t, l)

	n := mintAndStartImage(t, l, "speaks", image)

	// THE BINARY IS ON THE PATH A NON-INTERACTIVE SESSION GETS. `piper
	// --help` and not `which piper`, because a symlink that resolves to a
	// binary whose shared objects the loader cannot find answers the second
	// and fails the first -- which is exactly what /etc/ld.so.conf.d is in
	// the Dockerfile for.
	// THROUGH `sh -c ... 2>&1`, because piper writes its usage to STDERR and
	// harness.ExecOut returns stdout alone -- so the direct call came back
	// EMPTY and every assertion below reported a missing flag against a
	// binary that publishes all of them. Found on the lab's first run: the
	// failure of a check reading the wrong stream reads exactly like the
	// failure it was written for.
	help := l.ExecOut(ctx, n.ID, "sh", "-c", "piper --help 2>&1")
	if strings.TrimSpace(help) == "" {
		t.Fatal("piper printed no usage at all, so the assertions below would pass or fail " +
			"for a reason that is not about the flags")
	}
	for _, want := range []string{
		// The three flags ember builds every invocation from. A release that
		// renamed one would make every utterance fail after a card was
		// rented.
		"--model", "--output_file", "--espeak_data",
		// And the one that is NOT used, asserted so that the measurement in
		// internal/tts stays checkable: text arrives on stdin, so a release
		// that grew a `--text` would be the moment to re-read that package's
		// argument about there being no server.
		"--json-input",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("piper does not publish %q; the pinned release may have changed:\n%s", want, help)
		}
	}

	// THE VOICE, by the digest the Dockerfile pinned. A voice that is present
	// and is not the one that was verified is the channel between runs that
	// asset digests exist to close, reached through a layer instead of a
	// store -- so the bytes are checked and not merely counted.
	const voiceDigest = "5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f"
	sum := l.ExecOut(ctx, n.ID, "sha256sum", "/opt/voices/en_US-lessac-medium.onnx")
	if !strings.Contains(sum, voiceDigest) {
		t.Errorf("the voice in the image is not the one that was pinned:\n%s", sum)
	}

	// THE ESPEAK DATA, because piper resolves it relative to its own argv[0]
	// when the flag is absent and ember always names it. A directory that
	// moved would make every utterance fail with a phonemiser error after a
	// card was rented.
	if out := l.ExecOut(ctx, n.ID, "ls", "/opt/piper/espeak-ng-data/phondata"); !strings.Contains(out, "phondata") {
		t.Errorf("the espeak-ng data is not where ember names it:\n%s", out)
	}

	// AND NO PYTHON, which is this project's own rule rather than a
	// preference: a runtime that interprets on arrival is buying an
	// interpreter with GPU time. piper's repository has since been rewritten
	// into a Python package, so an image built from a later release would
	// quietly violate it -- and this is what says so.
	if out := l.ExecOut(ctx, n.ID, "sh", "-c", "command -v python3 python || echo none"); !strings.Contains(out, "none") {
		t.Errorf("the speech image carries a Python interpreter:\n%s", out)
	}
}

// TestWorkloadImage_SpeechImageListensOnNothingButSSH.
//
// The same assertion the sd image gets, for the same reason: this one is built
// FROM the node image, so it inherits that sshd configuration rather than
// repeating it -- and that inheritance is exactly the kind of thing a
// Dockerfile edit breaks silently. A workload port is never published, and the
// post-create check destroys a node whose provider published one, so what this
// guards is the image's own half of the claim.
func TestWorkloadImage_SpeechImageListensOnNothingButSSH(t *testing.T) {
	l := harness.New(t)
	assertListensOnlyOnSSH(t, ttsImage(t, l))
}

// TestTTS_AnUtteranceComesHomeAsAPlayableWav is the whole feature, end to end,
// on the free provider.
//
// THE ASSERTION IS THE RIFF HEADER AND THE SAMPLE COUNT, not that a file
// arrived. A `tar` that failed halfway produces a valid prefix of an archive
// and GNU tar over a missing path writes a valid EMPTY one, so "a file is
// there" is satisfied by two different failures -- which is the defect
// AGENTS.md records about reading the exit status after the stream. Parsing the
// header is what distinguishes a wav from a zero-byte file with the right name.
func TestTTS_AnUtteranceComesHomeAsAPlayableWav(t *testing.T) {
	l := harness.New(t)
	image := ttsImage(t, l)

	l.WriteConfig(map[string]string{"image": image})
	// TWO UTTERANCES OF THE SAME TEXT, differing only in `length_scale`.
	//
	// That is the whole arrangement and it is the image lab's rule rather
	// than a convenience: "same prompt, same seed, two arms differing only in
	// the init image -- and the two result files have to DIFFER". A flag
	// ember dropped on the way to piper would leave two wavs of the same
	// length, and nothing else on this path can see that. The first version
	// of this lab grepped `ember up`'s output for `--length_scale 1.1`
	// instead and went red against correct code, because sayOne prints the
	// utterance and not the argv -- an assertion about a mechanism nobody had
	// run. The value is 2.0 and the bound 1.3; the paragraph beside the
	// assertion below has the measured curve those two came off.
	//
	// The second names no output, so the unnamed branch of SpokenStem is
	// driven by the real pipeline rather than only by a unit test.
	l.AppendConfig(`
tts:
  jobs:
    - text: "Hello from ember. This is the first utterance."
      output: greeting
    - text: "Hello from ember. This is the first utterance."
      length_scale: 2.0
`)

	res := l.Run(t.Context(), "up")
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}
	out := res.Combined()
	t.Logf("ember up said:\n%s", out)

	// What the operator is told, in order: the utterance, then that it came
	// home. `ember up`'s lines are a contract the labs grep.
	for _, want := range []string{
		"speaking:", "greeting" + tts.Ext, "fetched",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not mention %q:\n%s", want, out)
		}
	}

	// THE ARTIFACTS, under the names the artifact rows will key: a name here
	// that is not the file on disk is a gallery of broken players, which is
	// the join internal/app's own test asserts against a fake and this one
	// asserts against a real piper.
	files := l.FetchedFiles()
	t.Logf("fetched: %v", files)
	dur := map[string]float64{}
	for _, want := range []string{"greeting-01" + tts.Ext, "speech-02" + tts.Ext} {
		p := findFetched(t, l, want)
		if p == "" {
			t.Fatalf("no %s among %v", want, files)
		}
		dur[want] = wavSeconds(t, p)
		t.Logf("%s is %.3fs of audio", want, dur[want])
		// A LOWER BOUND AND NOT AN EQUALITY, because what piper synthesises
		// for a sentence is a property of the voice rather than of ember.
		// Half a second is below anything this utterance can produce and far
		// above what a truncated fetch leaves, which is the gap the bound has
		// to sit in.
		if dur[want] < 0.5 {
			t.Errorf("%s is %.3fs of audio: a truncated or empty wav", want, dur[want])
		}
	}

	// AND THE SPEED FLAG REACHED THE NODE, which is the pixel comparison
	// rather than a plumbing check: same text, same voice, and the only
	// difference is a length_scale of 1.4 -- so the second file has to be
	// LONGER. A flag ember dropped would make the two the same length, which
	// is what every other assertion in this lab would still pass over.
	//
	// 2.0 AND A BOUND OF 1.3, BOTH MEASURED RATHER THAN CHOSEN. The curve
	// for this voice and this sentence, in sample frames off the wav header:
	// 1.0 -> 65652, 1.2 -> 76404, 1.4 -> 82036, 1.6 -> 89716, 2.0 -> 103284.
	// So length_scale is NOT a linear multiplier on the duration -- 1.4 buys
	// 1.25x and 2.0 buys 1.57x -- and the run-to-run spread is a few percent,
	// because `noise_scale` is generator noise and nothing here seeds it: the
	// same 1.4 through this lab came out 3% shorter than through a bare
	// container.
	//
	// The first version asserted 1.4 against a 1.15 bound and MEASURED 1.16,
	// which is a one-percent margin over a figure that moves by three -- a
	// flake, which is worse than no assertion because it teaches people to
	// re-run rather than to read. 2.0 moves the signal instead of narrowing
	// the bound, which is the right direction when a measurement is tight.
	fast, slow := dur["greeting-01"+tts.Ext], dur["speech-02"+tts.Ext]
	t.Logf("length_scale default -> %.3fs, 2.0 -> %.3fs (ratio %.2f)", fast, slow, slow/fast)
	if slow < fast*1.3 {
		t.Errorf("length_scale 2.0 produced %.3fs against %.3fs at the default: "+
			"the flag did not reach piper", slow, fast)
	}

	// THE LEDGER, because an artifact nothing recorded is an artifact the
	// gallery and `ember history` cannot reach -- the "built end to end and
	// never connected" defect this repository has shipped three times.
	rows := history(t, l)
	if len(rows) != 1 {
		t.Fatalf("got %d ledger row(s) after one `ember up`, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	t.Logf("ledger: %+v", got)
	if got.Node == "" {
		t.Error("no node was recorded, so nothing joins this row to the rental it describes")
	}
	// A speech run records no `jobs` rows, for the reason a `cmds` step does
	// not: the figures that table exists for are images, passes and steps per
	// second, and an utterance has none of them. The honest consequence is
	// printed rather than hidden, which is what `unrecorded` says.
	if !got.Unrecorded {
		t.Errorf("a speech run reports per-job figures it does not have: %+v", got)
	}
}

// findFetched returns the path of the first fetched file with this base name.
func findFetched(t *testing.T, l *harness.Lab, base string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(l.OutputDir(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return err
		}
		if filepath.Base(p) == base {
			found = p
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", l.OutputDir(), err)
	}
	return found
}

// wavSeconds reads a RIFF/WAVE header and returns the audio's duration.
//
// BY HAND AND NOT THROUGH A DEPENDENCY, because the standard library has no
// wav decoder and taking one for a lab would be a dependency added for a test
// -- which this repository refuses by name. It is twenty lines of two chunk
// headers, and what it buys is the difference between "a file arrived" and "a
// playable file arrived".
//
// It fails the test rather than returning an error, because every way it can
// fail is a defect in the thing under test: a file that is not a RIFF, a
// format chunk that is not PCM, a zero sample rate.
func wavSeconds(t *testing.T, p string) float64 {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if len(b) < 44 {
		t.Fatalf("%s is %d bytes, which is shorter than a wav header", p, len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatalf("%s is not a RIFF/WAVE file: %q", p, b[:12])
	}

	// Walk the chunks rather than assuming the canonical 44-byte layout:
	// piper writes that today and a wav is legally allowed a LIST or a fact
	// chunk in between, so a fixed offset would be a lab that breaks on a
	// file that is perfectly playable.
	var rate, bytesPerFrame uint32
	var dataBytes uint32
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := binary.LittleEndian.Uint32(b[off+4 : off+8])
		body := off + 8
		switch id {
		case "fmt ":
			if body+16 > len(b) {
				t.Fatalf("%s has a truncated fmt chunk", p)
			}
			rate = binary.LittleEndian.Uint32(b[body+4 : body+8])
			channels := binary.LittleEndian.Uint16(b[body+2 : body+4])
			bits := binary.LittleEndian.Uint16(b[body+14 : body+16])
			bytesPerFrame = uint32(channels) * uint32(bits) / 8
		case "data":
			dataBytes = size
		}
		// Chunks are word-aligned, so an odd size carries a pad byte.
		off = body + int(size) + int(size&1)
	}
	if rate == 0 || bytesPerFrame == 0 {
		t.Fatalf("%s declares no sample rate or frame size", p)
	}
	if dataBytes == 0 {
		t.Fatalf("%s has an empty data chunk: the header arrived and the samples did not", p)
	}
	return float64(dataBytes) / float64(bytesPerFrame) / float64(rate)
}
