package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
)

// The tests for `ember up` writing to the generation history.
//
// They exist because of the failure #47 is one instance of: a feature built
// end to end and never connected passes every test it has. The queue's own
// tests prove the recorder records, and the store's prove Adopt adopts, and
// neither of them would notice that `ember up` never calls either -- which is
// exactly how the preview frame, `capabilities.model` and the unrecorded
// columns all shipped. So these drive runUp and then read the database.

// recorded is what the history holds after a command ran, read through the
// store `ember up` writes to rather than through a path spelled here.
//
// One name for a path: app.ResolvePaths decides where state lives, and a
// second spelling in a test is how a lab ends up asserting against a
// directory nothing wrote to.
func recorded(t *testing.T, d deps) []genstore.Entry {
	t.Helper()
	s, err := genstore.Open(filepath.Join(d.HomeDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.History(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestUp_RecordsTheRentalBeforeTheCreateCall.
//
// The ordering is the point and it is the journal's rule: the window between
// asking a provider for a machine and reading the answer is where a rental
// nothing recorded is born, so the row goes down first. A create call that
// failed is the cheapest proof that it did -- nothing was rented, and the row
// is there anyway, carrying the reason.
//
// It is also what rules out keying the row on the run id, which would have
// been the tidier answer to #47's fifth question: that id is minted inside
// provision.WithNode, after this point, so a row keyed by it could only be
// written from the first event -- and this run has none.
func TestUp_RecordsTheRentalBeforeTheCreateCall(t *testing.T) {
	d := labDeps(t)
	d.Engine.(*dockerengine.RecordingEngine).FailOn = "ContainerCreate"

	var out bytes.Buffer
	err := runUp([]string{"--image", "x:v1", "--ttl", "5m"}, &out, d)
	if err == nil {
		t.Fatal("the create call was supposed to fail")
	}

	rows := recorded(t, d)
	if len(rows) != 1 {
		t.Fatalf("got %d row(s) in the history, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Status != genstore.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Provider != "local" {
		t.Errorf("provider = %q", got.Provider)
	}
	// The reason, because a ledger row that says "failed" and nothing else
	// sends the operator back to reading SQLite by hand.
	if !strings.Contains(got.Err, "x:v1") {
		t.Errorf("err = %q, want the create failure", got.Err)
	}
	if got.Started.IsZero() || got.Ended.IsZero() {
		t.Errorf("start/end = %v/%v; both are written, or the row reads as still running",
			got.Started, got.Ended)
	}
	// NOTHING ABOUT THE OUTPUT CHANGED, which is a hard constraint: `ember
	// up`'s lines are asserted by labs. The only line recording can add is
	// the one that says it could not, and this run recorded fine.
	if strings.Contains(out.String(), "not recording") {
		t.Errorf("recording failed on a writable state directory: %q", out.String())
	}
}

// TestUp_RecordsTheRentalsOwnEvents is the test that the run reports to the
// recorder at all, rather than to a bare terminal sink.
//
// IT IS THE WIRING AND NOT THE WRITE-THROUGH. The recorder's own tests prove
// an event becomes a column; nothing in them would notice `ember up` handing
// `plan.Do` a `report.Text` instead -- the row would still be adopted, still
// be finished, and still carry a status, so every other test here would pass.
// That is the closed loop this file exists to break, so this one reads the
// event stream.
//
// destroy_failed is the event it uses because it is the one an operator must
// not miss -- the node may still be running, and running means billing -- and
// because it is reachable without a node: an inspect that fails leaves the
// rental unconfirmed and the destroy unable to complete. It is now in the
// history as well as on the terminal.
func TestUp_RecordsTheRentalsOwnEvents(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)
	e.AddImage("x:v1")
	e.FailOn = "ContainerInspect"

	var out bytes.Buffer
	if err := runUp([]string{"--image", "x:v1", "--ttl", "5m"}, &out, d); err == nil {
		t.Fatal("the inspect was supposed to fail")
	}

	s, err := genstore.Open(filepath.Join(d.HomeDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.History(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d row(s), want 1", len(rows))
	}
	events, err := s.Events(t.Context(), rows[0].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var destroy *report.Event
	for i := range events {
		if events[i].Kind == report.KindDestroyFailed {
			destroy = &events[i]
		}
	}
	if destroy == nil {
		t.Fatalf("the run's events reached no row, so `ember up` is reporting to a "+
			"terminal and nothing else: %+v", events)
	}
	if destroy.Err == "" {
		t.Error("destroy_failed was recorded without the reason it failed")
	}
	// Numbered, because an unnumbered event overwrites whatever is already at
	// seq 0 -- a whole generation collapsing into one row is how that bug
	// presented in the queue.
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d: the recorder is not behind a report.Stream", i, ev.Seq)
		}
	}
}

// TestUp_RecordsAServedBatchsJobs, before anything is rented.
//
// The jobs are the ask, so they are written with the row rather than
// discovered from events: UpdateJob has nowhere to put a figure for a job that
// is not in the table, so a served run whose jobs were not recorded here would
// log a warning per job and leave every per-job column at zero -- the columns
// #32 added, blind to the command they were added to explain.
func TestUp_RecordsAServedBatchsJobs(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	d.Engine.(*dockerengine.RecordingEngine).FailOn = "ContainerCreate"
	cfg := imagesCfg(t, `
image: ember:lab
sd:
  serve: [sd-server, --diffusion-model, /models/x.gguf]
  jobs:
    - prompt: a lovely orange cat
      width: 512
      steps: 20
      seed: 7
    - prompt: a dog
`)
	var out bytes.Buffer
	if err := runUp([]string{"--provider", "local", "--config", cfg}, &out, d); err == nil {
		t.Fatal("the create call was supposed to fail")
	}

	rows := recorded(t, d)
	if len(rows) != 1 {
		t.Fatalf("got %d row(s), want 1", len(rows))
	}
	if rows[0].Jobs != 2 {
		t.Errorf("jobs = %d, want 2", rows[0].Jobs)
	}
	if rows[0].GPU != "rtx2080ti" {
		t.Errorf("card = %q, want the one the filter named", rows[0].GPU)
	}

	// And the ask round-trips, which is what makes the row re-readable later.
	s, err := genstore.Open(filepath.Join(d.HomeDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	req, err := s.Get(t.Context(), rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Jobs) != 2 || req.Jobs[0].Prompt != "a lovely orange cat" {
		t.Fatalf("jobs = %+v", req.Jobs)
	}
	if req.Jobs[0].Width != 512 || req.Jobs[0].Steps != 20 || req.Jobs[0].Seed != 7 {
		t.Errorf("job 1 = %+v, want the geometry the config asked for", req.Jobs[0])
	}
	// The preview settings travel too: they are part of what was asked for,
	// and `proj` every 5 is ember's default rather than upstream's.
	if req.Preview != "proj" || req.PreviewInterval != 5 {
		t.Errorf("preview = %q every %d", req.Preview, req.PreviewInterval)
	}
}

// TestUp_ARunThatRentsNothingRecordsNothing.
//
// `--dry-run` creates nothing and must therefore record nothing: a row for a
// run that never happened is worse than no row, because `ember spend` would
// count a request against a period in which ember spent zero. The budget
// refusals have the same shape and the same reason -- they happen in NewPlan,
// before this.
func TestUp_ARunThatRentsNothingRecordsNothing(t *testing.T) {
	d := labDeps(t)
	var out bytes.Buffer
	if err := runUp([]string{"--image", "x:v1", "--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if rows := recorded(t, d); len(rows) != 0 {
		t.Errorf("--dry-run recorded %d row(s): %+v", len(rows), rows)
	}
}

// TestUp_AppearsInHistoryAndSpend, which is what #47 asks for in so many
// words: a run made with `ember up` is in the ledger and in the aggregate.
//
// Through the commands rather than through the store, because the readers are
// where a figure is finally wrong: a jobless run has no per-job figures, and
// the rule this repository holds to is that such a cell is a dash and never a
// zero -- a column of zeros in a money ledger reads as free.
func TestUp_AppearsInHistoryAndSpend(t *testing.T) {
	d := labDeps(t)
	d.Engine.(*dockerengine.RecordingEngine).FailOn = "ContainerCreate"

	var out bytes.Buffer
	if err := runUp([]string{"--image", "x:v1", "--ttl", "5m"}, &out, d); err == nil {
		t.Fatal("the create call was supposed to fail")
	}
	id := string(recorded(t, d)[0].ID)

	var hist bytes.Buffer
	if err := runHistory(nil, &hist, d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{id, "local", "no jobs"} {
		if !strings.Contains(hist.String(), want) {
			t.Errorf("`ember history` does not mention %q:\n%s", want, hist.String())
		}
	}
	// The footnote that used to be printed here claimed such a run had run
	// "before ember recorded" its figures, which is false of a run made a
	// second ago. A caption that is true of one mode and wrong for the
	// others reads as working.
	if strings.Contains(hist.String(), "before ember recorded them") {
		t.Errorf("the ledger blames the schema for a run with no jobs:\n%s", hist.String())
	}
	// Nor is the empty model column explained as a run that "never reached a
	// node": a run with no jobs started no generation server, so there was no
	// /capabilities response to read a model from, and the jobless footnote
	// above already covers it. Counting it twice would have printed two
	// explanations for one dash, one of them wrong.
	if strings.Contains(hist.String(), "have no model") {
		t.Errorf("a jobless run is counted as having lost its model name:\n%s", hist.String())
	}

	var spend bytes.Buffer
	if err := runSpend(nil, &spend, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spend.String(), "1 request") {
		t.Errorf("`ember spend` does not count the run:\n%s", spend.String())
	}
}

// TestHistory_TheJoblessFootnoteCoversEveryCauseThereIsNow.
//
// THE PREMISE OF THAT FOOTNOTE CHANGED UNDER IT. It read "`ember up`
// forwarding ports, or running `cmds`", and those two were EXHAUSTIVE while
// the machine with no card that fills a store wrote no row at all -- which was
// #148. The moment that rental is recorded, the same sentence tells an
// operator who has never typed `ember up` that their unexplained $0.06 line is
// an `ember up`: the caption that is true of one mode and wrong for the
// others, in the footnote whose own comment argues against exactly that.
//
// THE ROW IS A STORE FILL AND NOT AN `ember up`, written through the same
// `queue.AdoptRun` the planner uses, so what is asserted is the sentence an
// operator reading THAT row is shown. Nothing in the row distinguishes the
// two -- both are jobless with no card -- which is why the fix covers the
// causes instead of telling them apart, and why this test asserts the
// coverage rather than a label.
//
// It pins the absence as well as the presence. A footnote naming the store
// without dropping the claim that an `ember up` is what the reader is looking
// at would be the defect still shipping with more words.
func TestHistory_TheJoblessFootnoteCoversEveryCauseThereIsNow(t *testing.T) {
	d := labDeps(t)
	d.CloudBaseURL = cpuOnlyRunPod(t)
	d.Creds = &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))}

	cfg := storeConfigFor(t, d, "runpod", storeBlock+"\n"+assetBlock)
	var out bytes.Buffer
	if err := runStore([]string{"sync", "-config", cfg}, &out, d); err == nil {
		t.Fatalf("the create call was supposed to fail:\n%s", out.String())
	}
	rows := recorded(t, d)
	if len(rows) != 1 || rows[0].Jobs != 0 {
		t.Fatalf("the ledger does not hold exactly one jobless row: %+v", rows)
	}

	var hist bytes.Buffer
	if err := runHistory(nil, &hist, d); err != nil {
		t.Fatal(err)
	}
	got := hist.String()
	if !strings.Contains(got, "have no jobs") {
		t.Fatalf("`ember history` printed no jobless footnote for a jobless row:\n%s", got)
	}
	// The cause the operator is actually looking at has to be in there.
	if !strings.Contains(got, "fills a store") {
		t.Errorf("the jobless footnote does not name the machine that fills a store, which is "+
			"what this row is:\n%s", got)
	}
	// And the sentence must not assert the wrong one. Two spellings are
	// forbidden rather than one span, because the first version of this
	// assertion matched the old text INCLUDING its line wrap -- so a footnote
	// that named the store and kept the false claim passed it, which is the
	// defect still shipping with more words. Found by a reinstatement whose
	// patch turned out to be a no-op, which is the trap AGENTS.md records
	// about a patch landing beside the live code.
	//
	// The pair, which asserts the row is one of exactly two things:
	if strings.Contains(got, "`ember up` forwarding ports, or running `cmds`,") {
		t.Errorf("the footnote still offers the operator a closed pair of causes, neither of "+
			"which this row is:\n%s", got)
	}
	// And the claim attached to `ember up` as the subject. The corrected
	// wording says "the rental is the whole record in each case", which is
	// true of all three.
	if strings.Contains(got, "records what it rented") {
		t.Errorf("the footnote still says an `ember up` is what recorded this row:\n%s", got)
	}
	// The per-step clause stays scoped to `cmds`: a store fill has no per-step
	// costs either, so an unscoped promise would be the same mistake one
	// sentence further down.
	if strings.Contains(got, "per-step costs") && !strings.Contains(got, "`cmds` run's\nper-step costs") {
		t.Errorf("the per-step clause is not scoped to a `cmds` run:\n%s", got)
	}
}
