package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/internal/tts"
)

// kinds is the sequence of event kinds a sink was given, in order, because an
// operator watching a batch reads it as a sequence. It is a method on the
// `collect` sink lifecycle_test.go already declares rather than a second sink,
// so the two files cannot come to disagree about what a sink does.
func (c *collect) kinds() []report.Kind {
	out := make([]report.Kind, 0, len(c.es))
	for _, e := range c.es {
		out = append(out, e.Kind)
	}
	return out
}

// pipered scripts a runner so that each utterance "succeeds" with piper's own
// stderr line and the fetch returns a wav.
//
// THE LINE IS PIPER'S VERBATIM, captured from a real run of the pinned
// release. A fake that printed something tidier would be a fake tidier than
// the thing it stands in for -- the defect this repository has recorded seven
// times -- and here it would specifically hide whether the parse works at all.
func pipered(t *testing.T, run tts.Run, names []string) *sshx.RecordingRunner {
	t.Helper()
	r := sshx.NewRecordingRunner()
	for i, name := range names {
		remote := RemoteDir + "/" + name + tts.Ext
		argv := run.Jobs[i].Command(remote)
		say := sshx.Cmd(argv[0], argv[1:]...).String()
		r.Outputs[say] = []byte(
			"[2026-11-04 19:32:46.413] [piper] [info] Loaded voice in 0.191540585 second(s)\n" +
				"[2026-11-04 19:32:46.483] [piper] [info] Real-time factor: 0.052 (infer=0.062 sec, audio=1.184 sec)\n")

		cmd, err := transfer.TarCommand(remote)
		if err != nil {
			t.Fatal(err)
		}
		r.Outputs[cmd.String()] = tarOne(t, name+tts.Ext, "RIFFxxxxWAVEfmt ")
	}
	return r
}

func oneUtterance() tts.Run {
	return tts.Run{Jobs: []tts.Job{spokenJob("Hello from ember.", "hello")}}
}

// spokenJob is an utterance with the two paths a catalogue entry supplies.
func spokenJob(text, out string) tts.Job {
	return tts.Job{
		Text: text, Output: out,
		Voice:      "/opt/voices/v.onnx",
		EspeakData: "/opt/piper/espeak-ng-data",
	}
}

func testOptions() session.Options {
	return session.Options{Log: discardLog()}
}

func TestRunSpoken_SaysEachUtteranceAndBringsItsWavHome(t *testing.T) {
	run := oneUtterance()
	names := []string{"hello-01"}
	r := pipered(t, run, names)

	dir := filepath.Join(t.TempDir(), "run")
	var out bytes.Buffer
	sink := &collect{}
	if err := runSpoken(t.Context(), &out, sink, r, run, testOptions(), dir, node.Rate(0), nil); err != nil {
		t.Fatalf("runSpoken: %v", err)
	}

	// The artifact is where the blob key says it is. This is the JOIN that
	// matters: the queue's Recorder keys an images row by the name in Wrote,
	// and the HTTP route opens that key -- so a name here that is not the
	// file on disk is a gallery of broken players.
	if _, err := os.Stat(filepath.Join(dir, "hello-01"+tts.Ext)); err != nil {
		t.Errorf("the wav is not where the key says: %v", err)
	}

	// The event sequence, in order, because an operator watching a batch
	// reads it as one.
	want := []report.Kind{report.KindUnitStarted, report.KindJobImages, report.KindUnitCost}
	got := sink.kinds()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for n := range want {
		if got[n] != want[n] {
			t.Errorf("event %d = %q, want %q", n, got[n], want[n])
		}
	}

	ims := sink.of(report.KindJobImages)
	if len(ims) != 1 || len(ims[0].Wrote) != 1 {
		t.Fatalf("job_images = %+v", ims)
	}
	if ims[0].Wrote[0].Name != "hello-01"+tts.Ext {
		t.Errorf("wrote %q, want hello-01%s", ims[0].Wrote[0].Name, tts.Ext)
	}
	if ims[0].Unit != report.UnitJob {
		t.Errorf("unit = %q, want %q: speech has no step, so the unit is the job",
			ims[0].Unit, report.UnitJob)
	}

	// PIPER'S OWN TWO FIGURES, which are the closest thing speech has to a
	// step count and are the reason the stderr is parsed at all.
	if ims[0].Reported != 1184*time.Millisecond {
		t.Errorf("reported (the audio's duration) = %v, want 1.184s", ims[0].Reported)
	}
	if ims[0].Measured != 62*time.Millisecond {
		t.Errorf("measured (what inference took) = %v, want 62ms", ims[0].Measured)
	}
}

// TestRunSpoken_EveryValueReachesTheNodeAsAPositionalParameter asserts on the
// command the runner was actually handed, which is the half a unit test of
// tts.Command cannot give: that this driver builds the command through that
// function rather than assembling one of its own.
func TestRunSpoken_EveryValueReachesTheNodeAsAPositionalParameter(t *testing.T) {
	run := oneUtterance()
	run.Jobs[0].Text = `"; rm -rf / #`
	r := pipered(t, run, []string{"hello-01"})

	if err := runSpoken(t.Context(), &bytes.Buffer{}, &collect{}, r, run,
		testOptions(), filepath.Join(t.TempDir(), "run"), node.Rate(0), nil); err != nil {
		t.Fatalf("runSpoken: %v", err)
	}
	calls := r.Calls()
	if len(calls) == 0 {
		t.Fatal("nothing ran")
	}
	// The text is quoted as one word and the script body is not where it
	// landed. sshx.Quote wraps in single quotes, so the hostile text appears
	// inside a quoted span and never as bare shell.
	if !strings.Contains(calls[0], `'"; rm -rf / #'`) {
		t.Errorf("the text did not arrive as one quoted word: %s", calls[0])
	}
}

// TestRunSpoken_StopsAtTheFirstFailureAndSaysWhichUtterance is runSteps' rule:
// a batch where job 2 failed and 3 through 8 ran anyway bills for work the
// operator no longer wants.
func TestRunSpoken_StopsAtTheFirstFailureAndSaysWhichUtterance(t *testing.T) {
	run := tts.Run{Jobs: []tts.Job{
		spokenJob("alpha", ""), spokenJob("bravo", ""), spokenJob("charlie", ""),
	}}
	names := []string{"speech-01", "speech-02", "speech-03"}
	r := pipered(t, run, names)
	// The second utterance fails. FailOn is a substring of the rendered
	// command, and the text is what distinguishes the three -- a bare word,
	// because sshx.Quote leaves one unquoted and a FailOn of "'bravo'" would
	// match nothing and report a green batch as the thing being tested.
	r.FailOn = "bravo"

	sink := &collect{}
	err := runSpoken(t.Context(), &bytes.Buffer{}, sink, r, run,
		testOptions(), filepath.Join(t.TempDir(), "run"), node.Rate(0), nil)
	if err == nil {
		t.Fatal("a failed utterance was not reported")
	}
	if !strings.Contains(err.Error(), "job 2/3") {
		t.Errorf("error = %q, want it to name job 2/3", err)
	}
	// Exactly two started: the third was never asked for.
	if n := len(sink.of(report.KindUnitStarted)); n != 2 {
		t.Errorf("%d utterance(s) started, want 2", n)
	}
	// And the one that failed produced no artifact row, because there is no
	// wav: piper opens its output file after synthesis.
	for _, e := range sink.of(report.KindJobImages) {
		if e.Index == 2 {
			t.Errorf("a failed utterance recorded an artifact: %+v", e)
		}
	}
}

// TestRunSpoken_CostsAreCardTimeAndAccumulate is the money half: an operator
// watching a batch decides whether to stop paying for the rest, which is the
// reason the artifacts come home as they are produced rather than at the end.
func TestRunSpoken_CostsAreCardTimeAndAccumulate(t *testing.T) {
	run := tts.Run{Jobs: []tts.Job{spokenJob("one", ""), spokenJob("two", "")}}
	r := pipered(t, run, []string{"speech-01", "speech-02"})

	sink := &collect{}
	// A real rate, because at zero every cost is zero and the accumulation
	// would be asserted against a column of zeros -- which is the defect
	// AGENTS.md records about every lab renting from the free provider.
	rate := node.Rate(3_600_000_000) // $3600/hour: $1 per second, so a span is readable
	if err := runSpoken(t.Context(), &bytes.Buffer{}, sink, r, run,
		testOptions(), filepath.Join(t.TempDir(), "run"), rate, nil); err != nil {
		t.Fatalf("runSpoken: %v", err)
	}
	costs := sink.of(report.KindUnitCost)
	if len(costs) != 2 {
		t.Fatalf("%d cost event(s), want 2", len(costs))
	}
	if costs[0].Spent != costs[0].Cost {
		t.Errorf("the first spent (%d) is not its own cost (%d)", costs[0].Spent, costs[0].Cost)
	}
	if costs[1].Spent != costs[0].Cost+costs[1].Cost {
		t.Errorf("spent %d does not accumulate %d and %d",
			costs[1].Spent, costs[0].Cost, costs[1].Cost)
	}
	for _, c := range costs {
		if c.Unit != report.UnitJob {
			t.Errorf("unit = %q, want %q", c.Unit, report.UnitJob)
		}
	}
}

// TestRunSpoken_AMissingReportLineCostsTheFiguresAndNothingElse: the job
// worked, because the exit status said so. Reading a status and not a log is
// the rule AGENTS.md records for sd-server, and the consequence of applying it
// here is that an upstream reword loses two numbers rather than a generation.
func TestRunSpoken_AMissingReportLineCostsTheFiguresAndNothingElse(t *testing.T) {
	run := oneUtterance()
	r := pipered(t, run, []string{"hello-01"})
	// Overwrite what piper "printed" with a line that is not the report.
	argv := run.Jobs[0].Command(RemoteDir + "/hello-01" + tts.Ext)
	say := sshx.Cmd(argv[0], argv[1:]...).String()
	r.Outputs[say] = []byte("[piper] [info] something upstream reworded\n")

	sink := &collect{}
	if err := runSpoken(t.Context(), &bytes.Buffer{}, sink, r, run,
		testOptions(), filepath.Join(t.TempDir(), "run"), node.Rate(0), nil); err != nil {
		t.Fatalf("runSpoken: %v", err)
	}
	ims := sink.of(report.KindJobImages)
	if len(ims) != 1 {
		t.Fatalf("%d artifact event(s), want 1", len(ims))
	}
	if ims[0].Reported != 0 || ims[0].Measured != 0 {
		t.Errorf("figures were invented from a line that was not there: %+v", ims[0])
	}
	if len(ims[0].Wrote) != 1 {
		t.Error("the artifact was not recorded")
	}
}

// TestSpokenReport_TakesTheLastLineBecauseThePerProcessFiguresAreCumulative.
// Measured on a real eight-utterance run: `audio=` walked 4.71, 9.18, 13.68
// ... 36.58 within one process. A first-match reader would report the first
// utterance's figures for the whole batch the day somebody holds a piper open.
func TestSpokenReport_TakesTheLastLineBecauseThePerProcessFiguresAreCumulative(t *testing.T) {
	said := strings.Join([]string{
		"Real-time factor: 0.03 (infer=0.17 sec, audio=4.71 sec)",
		"Real-time factor: 0.03 (infer=0.31 sec, audio=9.18 sec)",
	}, "\n")
	got, ok := spokenReport(said)
	if !ok {
		t.Fatal("did not parse")
	}
	if got.Audio < 9*time.Second {
		t.Errorf("audio = %v, want the LAST line's 9.18s", got.Audio)
	}
	if _, ok := spokenReport("nothing here"); ok {
		t.Error("found a report in output that has none")
	}
}

func TestSpokenStem_NumbersEveryUtteranceIncludingANamedOne(t *testing.T) {
	// Numbered even when named, because two jobs may legitimately carry the
	// same `output` -- and without the index the second overwrites the first:
	// one wav on disk, two artifact rows pointing at it, and an operator
	// billed for two utterances with one to listen to.
	a := SpokenStem(tts.Job{Output: "greeting"}, 0)
	b := SpokenStem(tts.Job{Output: "greeting"}, 1)
	if a == b {
		t.Fatalf("two jobs named %q produced one filename: %q", "greeting", a)
	}
	if a != "greeting-01" || b != "greeting-02" {
		t.Errorf("stems = %q and %q, want greeting-01 and greeting-02", a, b)
	}
	// Unnamed jobs are numbered from the same scheme, so a script reading the
	// directory needs one case rather than two.
	if got := SpokenStem(tts.Job{}, 0); got != "speech-01" {
		t.Errorf("an unnamed job's stem = %q, want speech-01", got)
	}
	if got := SpokenStem(tts.Job{Output: "  "}, 2); got != "speech-03" {
		t.Errorf("a blank output's stem = %q, want speech-03", got)
	}
}

// TestBatchOf_ChoosesOneKindAndNothingForTheShapesThatAreNotOne is the
// dispatch this change exists to introduce: the line it replaced asked
// `len(p.Spec.Serve) > 0`, which is a question about sd-server's argv.
func TestBatchOf_ChoosesOneKindAndNothingForTheShapesThatAreNotOne(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{"nothing", Plan{}, ""},
		{"a run with no batch at all", Plan{}, ""},
		{
			"an sd batch",
			Plan{Spec: node.Spec{Serve: []string{"sd-server"}}, Jobs: sdserver.Run{Jobs: []sdserver.Job{{Prompt: "a cat"}}}},
			"sd (a resident sd-server)",
		},
		{
			// A served shape with no jobs is still sd: the kind is what the
			// serve command says, and runServed is what refuses an empty
			// batch -- which is the same answer the code gave before this
			// dispatch existed.
			"a serve command with no jobs",
			Plan{Spec: node.Spec{Serve: []string{"sd-server"}}},
			"sd (a resident sd-server)",
		},
		{
			"a speech batch",
			Plan{Spoken: oneUtterance()},
			"tts (piper, 1 utterance(s), /opt/voices/v.onnx)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := c.plan.batchOf()
			if c.want == "" {
				if b != nil {
					t.Errorf("batchOf() = %q, want nothing", b.Describe())
				}
				return
			}
			if b == nil {
				t.Fatalf("batchOf() chose nothing, want %q", c.want)
			}
			if got := b.Describe(); got != c.want {
				t.Errorf("Describe() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestServedBatch_RefusesJobsWithNoServer covers the belt to NewPlan's
// braces: config.ParseSD already refuses the combination, so this arm says
// which key is missing rather than starting nothing.
func TestServedBatch_RefusesJobsWithNoServer(t *testing.T) {
	b := servedBatch{jobs: sdserver.Run{Jobs: []sdserver.Job{{Prompt: "a cat"}}}}
	err := b.Run(t.Context(), &bytes.Buffer{}, &collect{}, nil, nil,
		node.Spec{}, testOptions(), t.TempDir(), node.Rate(0))
	if err == nil {
		t.Fatal("sd jobs with no serve command were accepted")
	}
	if !strings.Contains(err.Error(), "sd.serve") {
		t.Errorf("error = %q, want it to name sd.serve", err)
	}
}

// TestSpokenBatch_RunDrivesTheDriver is the adapter's own assertion: an
// interface method that dropped its arguments would leave every test above
// passing, because they call runSpoken directly.
func TestSpokenBatch_RunDrivesTheDriver(t *testing.T) {
	run := oneUtterance()
	r := pipered(t, run, []string{"hello-01"})
	dir := filepath.Join(t.TempDir(), "run")

	b := spokenBatch{run: run}
	if err := b.Run(t.Context(), &bytes.Buffer{}, &collect{}, nil, r,
		node.Spec{}, testOptions(), dir, node.Rate(0)); err != nil {
		t.Fatalf("spokenBatch.Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello-01"+tts.Ext)); err != nil {
		t.Errorf("the adapter did not reach the driver: %v", err)
	}
}

// TestRemoteDir_IsUnderWorkspaceOutAndNotWorkspaceItself. Two properties and
// both are about something else being unable to take the path away:
// node.MountAt refuses a store over /workspace, so this cannot be shadowed by
// a mount; and a subdirectory means a config that also fetches /workspace/out
// through `outputs:` does not bring ember's speech home a second time.
func TestRemoteDir_IsUnderWorkspaceOutAndNotWorkspaceItself(t *testing.T) {
	if !strings.HasPrefix(RemoteDir, "/workspace/out/") {
		t.Errorf("RemoteDir = %q, want it under /workspace/out/", RemoteDir)
	}
	if RemoteDir == "/workspace/out" {
		t.Error("RemoteDir is /workspace/out itself, so an outputs: fetch of that " +
			"directory would bring every wav home twice")
	}
	// And the mount refusal really covers its parent, asserted rather than
	// assumed: this is the property the paragraph above rests on.
	if _, err := node.MountAt("/workspace"); err == nil {
		t.Error("a store can be mounted over /workspace, so RemoteDir can be shadowed")
	}
}
