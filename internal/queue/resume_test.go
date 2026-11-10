package queue

import (
	"testing"
	"time"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
)

// TestSinkFor_ContinuesTheNumberingARetryLeftBehind.
//
// THE DEFECT, MEASURED ON A REAL RETRY BEFORE THIS TEST EXISTED. A retry
// reuses the row rather than queueing a copy, so a second attempt writes into
// an event stream the first attempt already filled -- and `AppendEvents`
// REPLACES on seq, which is right for a re-delivered event and ruinous for a
// restarted counter. The second attempt emitted the same eight kinds at seqs 1
// to 8 and overwrote the first attempt's eight exactly; on a request whose
// only event was the `requeued` marker at seq 1, the retried run's first event
// replaced the marker. The history the event stream exists to keep was being
// deleted by the thing meant to preserve it.
//
// AND IT LOOKED LIKE SUCCESS. The row read `done`, the image was there, and
// the event list was the right length -- it was the right length because the
// second attempt had as many events as the first. Nothing in the suite could
// see it, which is why this asserts the SEQUENCE and not the count.
func TestSinkFor_ContinuesTheNumberingARetryLeftBehind(t *testing.T) {
	st := genstore.NewRecordingStore()
	q := &Queue{Store: st, Now: func() time.Time { return time.Unix(0, 0).UTC() }}

	id, err := st.Enqueue(t.Context(), genstore.Request{
		Kind: genstore.KindSD, Provider: "local", TTL: time.Hour,
		Jobs: []genstore.Job{{Prompt: "one", Width: 64, Height: 64, Steps: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// An attempt's worth of history, and the requeued marker a retry appends.
	if err := st.AppendEvents(t.Context(), id, []report.Event{
		{Seq: 1, Kind: report.KindNodeReady},
		{Seq: 2, Kind: report.KindUnitStarted},
		{Seq: 3, Kind: report.KindRequeued, Text: "failed"},
	}); err != nil {
		t.Fatal(err)
	}

	q.sinkFor(id).Emit(report.Event{Kind: report.KindNodeReady})

	evs, err := st.Events(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("got %d event(s), want 4: the retried attempt overwrote the first one's "+
			"history rather than following it: %+v", len(evs), evs)
	}
	if evs[3].Seq != 4 {
		t.Errorf("the retry's first event is seq %d, want 4", evs[3].Seq)
	}
	// THE MARKER IS THE ONE THAT MATTERS. Without it a request that failed,
	// was retried and then succeeded reads as one that simply succeeded --
	// which is the whole reason reusing the row needed an event at all.
	var marker bool
	for _, e := range evs {
		if e.Kind == report.KindRequeued {
			marker = true
		}
	}
	if !marker {
		t.Errorf("the requeued marker was overwritten, so this request's history now reads as "+
			"one that never failed: %+v", evs)
	}
}

// TestSinkFor_AFirstAttemptStillNumbersFromOne.
//
// The control at the other end: a request with no history must behave exactly
// as it did before the seeding existed, because that is every request ember
// has ever run. A seed read from an empty list is zero and the first event is
// seq 1, which is also what `report.Stream`'s own comment promises -- "numbers
// start at 1, so the zero value means this never went through a Stream".
func TestSinkFor_AFirstAttemptStillNumbersFromOne(t *testing.T) {
	st := genstore.NewRecordingStore()
	q := &Queue{Store: st, Now: func() time.Time { return time.Unix(0, 0).UTC() }}

	id, err := st.Enqueue(t.Context(), genstore.Request{
		Kind: genstore.KindSD, Provider: "local", TTL: time.Hour,
		Jobs: []genstore.Job{{Prompt: "one", Width: 64, Height: 64, Steps: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}

	q.sinkFor(id).Emit(report.Event{Kind: report.KindNodeReady})

	evs, err := st.Events(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Seq != 1 {
		t.Errorf("a first attempt's first event is %+v, want one event at seq 1", evs)
	}
}
