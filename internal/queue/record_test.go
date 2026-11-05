package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
)

// The tests for the write-through on its own, as `ember up` uses it.
//
// The queue's own tests already drive it through a whole drain loop, which is
// what proves the extraction changed nothing. These drive it the way the other
// caller does -- a stream over one adopted run -- because what `ember up`
// reaches is a different set of events: no claim, no rider, and in two of its
// three shapes no jobs at all.

// adopted is a store holding one run `ember up` is in the middle of, and a
// recorder wired to it the way recordRental does.
func adopted(t *testing.T, jobs ...string) (*genstore.RecordingStore, *Recorder, *report.Stream) {
	t.Helper()
	st := genstore.NewRecordingStore()
	r := genstore.Request{Provider: "local", GPU: "rtx2080ti", TTL: time.Hour}
	for _, p := range jobs {
		r.Jobs = append(r.Jobs, genstore.Job{Prompt: p})
	}
	id, err := st.Adopt(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{Store: st, Req: id}
	return st, rec, report.NewStream(time.Now, rec)
}

func reqOf(t *testing.T, st genstore.Store, rec *Recorder) genstore.Request {
	t.Helper()
	got, err := st.Get(context.Background(), rec.Req)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestRecorder_WritesTheRentalsFactsThrough.
//
// These are the columns #30 and #32 added and `ember up` could not reach: what
// the rental cost, what it was charged for against what the clock said, what
// the node said about its memory, how long the weights took to load measured
// both ways, and which model served it. Every one arrives as an event, so this
// is the whole of what `ember spend` can say about a measurement run.
func TestRecorder_WritesTheRentalsFactsThrough(t *testing.T) {
	st, rec, stream := adopted(t, "a cat")

	stream.Emit(report.Event{Kind: report.KindNodeReady, Node: "local/n1"})
	stream.Emit(report.Event{
		Kind: report.KindNodeMemory, MemCgroup: 2,
		MemLimit: 32 << 30, MemCurrent: 31 << 30,
		MemTotal: 700 << 30, MemCached: 29 << 30, Bytes: 56 << 30,
	})
	stream.Emit(report.Event{
		Kind: report.KindModelResident, Total: 1,
		Measured: 90 * time.Second, Reported: 551 * time.Millisecond,
		Model: "flux2-dev-Q8_0.gguf",
	})
	stream.Emit(report.Event{
		Kind: report.KindRunCost, Node: "local/n1",
		Cost: node.MicroUSD(1790000), Rate: node.Rate(2490000),
		Elapsed: 50 * time.Minute, Billable: time.Hour,
	})

	got := reqOf(t, st, rec)
	if got.Node != "local/n1" {
		t.Errorf("node = %q", got.Node)
	}
	if got.Cost != node.MicroUSD(1790000) || got.Rate != node.Rate(2490000) {
		t.Errorf("cost/rate = %v/%v", got.Cost, got.Rate)
	}
	// Billable beside elapsed, not instead of it: a provider minimum makes
	// them differ, and an overhead figure computed against the wrong one
	// credits ember with a saving the invoice does not show.
	if got.Elapsed != 50*time.Minute || got.Billable != time.Hour {
		t.Errorf("elapsed/billable = %v/%v", got.Elapsed, got.Billable)
	}
	if got.LoadMeasured != 90*time.Second || got.LoadReported != 551*time.Millisecond {
		t.Errorf("load measured/reported = %v/%v -- both, because they disagreed by two "+
			"orders of magnitude on the one measurement anybody has made",
			got.LoadMeasured, got.LoadReported)
	}
	if got.MemCgroup != 2 || got.MemLimit != 32<<30 || got.AssetBytes != 56<<30 {
		t.Errorf("memory = v%d, limit %d, weights %d", got.MemCgroup, got.MemLimit, got.AssetBytes)
	}
	// THE MODEL OFF A RESPONSE, which is what makes `ember spend --by model`
	// mean anything for a run `ember up` made: Total is 1 here, and the event
	// used not to be emitted at all for a batch of one.
	if got.Model != "flux2-dev-Q8_0.gguf" {
		t.Errorf("model = %q, want the name the server reported", got.Model)
	}
}

// TestRecorder_NumbersEveryEvent.
//
// The bug this is about left ONE row for a whole generation: emitters do not
// set Seq, so without a Stream around the recorder every event arrives as 0
// and genstore's INSERT OR REPLACE on (request, seq) makes each overwrite the
// last. It was found by reading the database, and the unit test that should
// have caught it could not, because its fake set Seq itself.
func TestRecorder_NumbersEveryEvent(t *testing.T) {
	st, rec, stream := adopted(t, "a cat")

	const n = 6
	for i := 0; i < n; i++ {
		stream.Emit(report.Event{
			Kind: report.KindJobProgress, Unit: report.UnitJob,
			Index: 1, Total: 1, Pass: 1, Step: i + 1, TotalSteps: n,
		})
	}
	rec.Flush(context.Background())

	es, err := st.Events(context.Background(), rec.Req, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != n {
		t.Fatalf("stored %d event(s) of %d: a numbering that repeats makes each one "+
			"overwrite the last", len(es), n)
	}
	for i, e := range es {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
	}
}

// TestRecorder_RecordsTheJobsExecutionColumns, which is what `ember spend`
// divides into images and steps.
func TestRecorder_RecordsTheJobsExecutionColumns(t *testing.T) {
	st, rec, stream := adopted(t, "a cat")
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	stream.Emit(report.Event{
		Kind: report.KindUnitStarted, Unit: report.UnitJob,
		Index: 1, Total: 1, At: at,
	})
	// Two passes, which is why Passes and Steps are two columns: a pass
	// restarts for each batch item and each highres pass, so one number
	// would lie.
	stream.Emit(report.Event{Kind: report.KindJobProgress, Index: 1, Pass: 1, Step: 4, TotalSteps: 4})
	stream.Emit(report.Event{Kind: report.KindJobProgress, Index: 1, Pass: 2, Step: 4, TotalSteps: 4})
	stream.Emit(report.Event{
		Kind: report.KindJobImages, Index: 1, Total: 1,
		Files: 2, Bytes: 4096,
		Wrote: []report.File{{Name: "a-00.png", Bytes: 2048}, {Name: "a-01.png", Bytes: 2048}},
	})
	stream.Emit(report.Event{
		Kind: report.KindUnitCost, Unit: report.UnitJob,
		Index: 1, Total: 1, Cost: node.MicroUSD(140800), At: at.Add(141 * time.Second),
	})

	runs, err := st.JobRuns(context.Background(), rec.Req)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d job run(s), want 1", len(runs))
	}
	j := runs[0]
	if j.Images != 2 || j.Bytes != 4096 {
		t.Errorf("images/bytes = %d/%d", j.Images, j.Bytes)
	}
	if j.Cost != node.MicroUSD(140800) {
		t.Errorf("cost = %v", j.Cost)
	}
	if j.Passes != 2 || j.Steps != 8 {
		t.Errorf("passes/steps = %d/%d, want 2/8 -- summed from each pass's own declared "+
			"total, never counted from the frames", j.Passes, j.Steps)
	}
	if j.Sampling() != 141*time.Second {
		t.Errorf("sampling = %v, want 141s", j.Sampling())
	}
}

// TestRecorder_RecordsNoImageRowWithoutAKey.
//
// `ember up` writes into a timestamped run directory it prints to the
// operator, which no gallery serves and which `ember history rm` must not
// offer to remove -- so there is no blob key for its files and no row naming
// one. The COUNTS still arrive, because they are columns on the job, which is
// what the ledger and the aggregate read. README's `history du` output rests
// on this: it tells the operator how much of the outputs tree no image row
// names, and `ember up`'s run directories are exactly that.
func TestRecorder_RecordsNoImageRowWithoutAKey(t *testing.T) {
	st, rec, stream := adopted(t, "a cat")

	images := report.Event{
		Kind: report.KindJobImages, Index: 1, Total: 1, Files: 1, Bytes: 2048,
		Wrote: []report.File{{Name: "a-00.png", Bytes: 2048}},
	}
	stream.Emit(images)

	ims, err := st.Images(context.Background(), rec.Req)
	if err != nil {
		t.Fatal(err)
	}
	if len(ims) != 0 {
		t.Errorf("recorded %d image row(s) for a run whose files no store can serve: %+v", len(ims), ims)
	}
	runs, err := st.JobRuns(context.Background(), rec.Req)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Images != 1 {
		t.Errorf("the image COUNT was lost too: %+v", runs)
	}

	// And with a key -- the queue's wiring -- the row is there, named the way
	// the blob store names the file.
	keyed, krec, kstream := adopted(t, "a cat")
	krec.Key = func(name string) string { return BlobKey(krec.Req, name) }
	kstream.Emit(images)
	ims, err = keyed.Images(context.Background(), krec.Req)
	if err != nil {
		t.Fatal(err)
	}
	if len(ims) != 1 {
		t.Fatalf("got %d image row(s) with a key, want 1", len(ims))
	}
	if want := string(krec.Req) + "/a-00.png"; ims[0].Key != want {
		t.Errorf("key = %q, want %q", ims[0].Key, want)
	}
}

// TestRecorder_FinishClassifiesHowARunEnded.
//
// A cancelled context is NOT a failure: the operator pressed Ctrl-C, or the
// node reached its deadline, and calling that a failed generation sends them
// looking for a bug that is not there.
func TestRecorder_FinishClassifiesHowARunEnded(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want genstore.Status
	}{
		{"a clean run", nil, genstore.StatusDone},
		{"ctrl-c", context.Canceled, genstore.StatusCancelled},
		{"the node's deadline", context.DeadlineExceeded, genstore.StatusCancelled},
		{"a workload that failed", errors.New("exit 1"), genstore.StatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, rec, stream := adopted(t)
			// Buffered and never flushed by the caller, which is the ordering
			// Finish has to get right: a status written over an unflushed
			// buffer is a row saying "done" above events that never arrived.
			stream.Emit(report.Event{Kind: report.KindUnitStarted, Unit: report.UnitJob, Index: 1, Total: 1})
			rec.Finish(context.Background(), c.err)

			got := reqOf(t, st, rec)
			if got.Status != c.want {
				t.Errorf("status = %q, want %q", got.Status, c.want)
			}
			if got.Ended.IsZero() {
				t.Error("no end was recorded, so the row reads as a run still in flight")
			}
			if (c.err == nil) != (got.Err == "") {
				t.Errorf("err = %q for cause %v", got.Err, c.err)
			}
			es, err := st.Events(context.Background(), rec.Req, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(es) != 1 {
				t.Errorf("Finish stored %d event(s), want the buffered one", len(es))
			}
		})
	}
}
