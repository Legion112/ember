package report

import (
	"testing"
	"time"
)

// TestStream_NumbersFromOne: the zero value has to mean "this never went
// through a Stream" rather than "this was first", because a sink that
// persists events uses Seq as a cursor and a browser asks for everything
// after the last one it saw. A first event numbered 0 is an event that
// reconnecting clients would be served twice.
func TestStream_NumbersFromOne(t *testing.T) {
	c := &collect{}
	s := NewStream(nil, c)

	s.Emit(Event{Kind: KindRunFinished})
	s.Emit(Event{Kind: KindRunFinished})

	if got := c.es[0].Seq; got != 1 {
		t.Errorf("first event has Seq %d, want 1", got)
	}
	if got := c.es[1].Seq; got != 2 {
		t.Errorf("second event has Seq %d, want 2", got)
	}
	if got, want := s.Seq(), int64(2); got != want {
		t.Errorf("Seq() = %d, want %d", got, want)
	}
}

// TestStream_StampsOneNumberForEverySink is the reason stamping is here and
// not in each sink.
//
// The websocket tells a browser "you have seen up to 41" and the browser then
// asks the database for everything after 41. Two numberings make that
// question unanswerable, and the symptom is a UI that silently repeats or
// skips progress after a reconnect.
func TestStream_StampsOneNumberForEverySink(t *testing.T) {
	a, b := &collect{}, &collect{}
	s := NewStream(nil, a, b)

	s.Emit(Event{Kind: KindNodeReady})

	if a.es[0].Seq != b.es[0].Seq {
		t.Errorf("the two sinks saw Seq %d and %d for one event", a.es[0].Seq, b.es[0].Seq)
	}
	if a.es[0].At != b.es[0].At {
		t.Errorf("the two sinks saw different timestamps for one event")
	}
}

// TestStream_UsesTheInjectedClock: a recorded timestamp is compared, so the
// clock is injectable. Nothing here waits on one, which is why there is no
// clock interface -- the same rule internal/provision states.
func TestStream_UsesTheInjectedClock(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := &collect{}
	NewStream(func() time.Time { return at }, c).Emit(Event{Kind: KindNodeReady})

	if got := c.es[0].At; !got.Equal(at) {
		t.Errorf("At = %v, want %v", got, at)
	}
}

// TestStream_KeepsATimestampSomebodyElseSet, so an event replayed from a
// database is not re-dated to now.
func TestStream_KeepsATimestampSomebodyElseSet(t *testing.T) {
	then := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := &collect{}
	NewStream(func() time.Time { return time.Now() }, c).Emit(Event{Kind: KindNodeReady, At: then})

	if got := c.es[0].At; !got.Equal(then) {
		t.Errorf("At = %v, want the timestamp the caller set, %v", got, then)
	}
}

// TestMulti_DoesNotRenumber: a Stream inside a Stream would stamp an event
// twice and the inner number would be overwritten, so the fan-out that does
// not number is a separate thing from the one that does.
func TestMulti_DoesNotRenumber(t *testing.T) {
	a, b := &collect{}, &collect{}
	Multi(a, b).Emit(Event{Kind: KindNodeReady, Seq: 7})

	for _, c := range []*collect{a, b} {
		if got := c.es[0].Seq; got != 7 {
			t.Errorf("Seq = %d, want the 7 it arrived with", got)
		}
	}
}

func TestDiscard(t *testing.T) {
	// Nothing to assert but that it does not panic: it exists so a caller
	// with nothing to report to does not have to invent a sink.
	Discard.Emit(Event{Kind: KindLog, Text: "x"})
}

func TestSinkFunc(t *testing.T) {
	var got Event
	SinkFunc(func(e Event) { got = e }).Emit(Event{Kind: KindRunFailed, Err: "boom"})
	if got.Err != "boom" {
		t.Errorf("Err = %q, want %q", got.Err, "boom")
	}
}

// TestStream_SurvivesConcurrentEmitters. The run goroutine emits progress
// while a destroy hook emits a cost, so two goroutines reach one Stream.
func TestStream_SurvivesConcurrentEmitters(t *testing.T) {
	c := &collect{}
	s := NewStream(nil, c)

	const n = 100
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < n; j++ {
				s.Emit(Event{Kind: KindLog, Text: "x\n"})
			}
		}()
	}
	<-done
	<-done

	if got := c.len(); got != 2*n {
		t.Fatalf("got %d events, want %d", got, 2*n)
	}
	// Every sequence number issued exactly once: a lost or duplicated number
	// is a cursor that skips or repeats.
	seen := map[int64]bool{}
	for _, e := range c.es {
		if seen[e.Seq] {
			t.Fatalf("Seq %d was issued twice", e.Seq)
		}
		seen[e.Seq] = true
	}
	for i := int64(1); i <= 2*n; i++ {
		if !seen[i] {
			t.Errorf("Seq %d was never issued", i)
		}
	}
}
