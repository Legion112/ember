package queue

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/genstore"
)

// The rental-failed-after-the-node-existed tests.
//
// fakeRental has carried a `rentErr` since it was written -- a rental that
// counts as started, registers its deferred destroy, and then returns before
// calling fn -- and until these tests NOTHING SET IT. The field was plumbed
// through fakePlanner, reached the fake, and no test in the package assigned
// it, so the whole span between ClaimNext writing `running` and drain writing
// a terminal status had never been exercised. That is why a request could sit
// `running` for the life of the process with its rental long gone: not a
// missed assertion but a case the suite could not produce, although the fake
// was already able to.
//
// The real failures in that span are the create call, waiting for sshd, the
// GPU probe, the asset verify and the model load -- every one of them after
// money has been committed, which is what makes a row that says `running`
// and a requests.err that says nothing the expensive kind of wrong.

// TestQueue_ARentalThatBreaksBeforeGeneratingEndsTheRequest.
func TestQueue_ARentalThatBreaksBeforeGeneratingEndsTheRequest(t *testing.T) {
	served := &fakeServed{}
	pl := &fakePlanner{served: served, rentErr: errors.New("gpu probe refused the node")}
	q, st := newQueue(t, pl)

	id := enqueue(t, st, "rtx2080ti", "a paper boat")

	if _, err := q.once(t.Context()); err == nil {
		t.Fatal("a rental that never generated anything was reported as success")
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusFailed {
		t.Errorf("the request ended as %q, want failed; a row that still says running "+
			"outlives the rental and only a restart clears it", got.Status)
	}
	if !strings.Contains(got.Err, "gpu probe refused the node") {
		t.Errorf("the reason the rental broke is not on the row: %q", got.Err)
	}
	if got.Ended.IsZero() {
		// Without this the request is excluded from the "missing part of its
		// history" count, so a run that definitely happened reads as one that
		// has not happened yet.
		t.Error("the request has no end time, so every reader treats it as still in flight")
	}

	// And nothing generated, which is what separates this from a batch that
	// failed partway: there was never a server to ask.
	if n := served.count(); n != 0 {
		t.Errorf("%d batch(es) generated on a rental that never came up", n)
	}
}

// TestQueue_ARentalThatBreaksAfterGeneratingKeepsWhatDrainWrote is the
// negative control, and it is the assertion this change could most easily
// have broken.
//
// drain calls finish for every request it generates, so once must not write
// over it on the way out. A teardown that fails after a successful batch is
// the ordinary way to get here -- a destroy that errored, a log fetch that
// did not -- and an unconditional write would turn a done request into a
// failed one, which is this repository's two-writers rule: the loser
// overwrites the one that knew.
//
// Without this test, "always fail the request when the rental returned an
// error" passes every other assertion in this file.
func TestQueue_ARentalThatBreaksAfterGeneratingKeepsWhatDrainWrote(t *testing.T) {
	served := &fakeServed{}
	pl := &fakePlanner{served: served, teardownErr: errors.New("destroy failed: node may still be billing")}
	q, st := newQueue(t, pl)

	id := enqueue(t, st, "rtx2080ti", "a paper boat")

	if _, err := q.once(t.Context()); err == nil {
		t.Fatal("a teardown failure was swallowed; the operator must hear about a node that may still bill")
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusDone {
		t.Errorf("a request that generated its batch ended as %q, want done: the rental's "+
			"teardown error was written over what drain knew", got.Status)
	}
	if got.Err != "" {
		t.Errorf("a successful request carries a failure reason: %q", got.Err)
	}
	if n := served.count(); n != 1 {
		t.Errorf("generated %d batch(es), want 1", n)
	}
}

// TestQueue_ARentalEndedByCtrlCIsCancelledAndNotFailed.
//
// The same classification finish already makes, and it has to be the same
// one: a context cancelled during provisioning is the operator pressing
// Ctrl-C or the node reaching its deadline, and recording that as a failed
// generation sends them looking for a bug that is not there. Two copies of
// that switch would be two accounts of what Ctrl-C means, which is why there
// is one.
func TestQueue_ARentalEndedByCtrlCIsCancelledAndNotFailed(t *testing.T) {
	served := &fakeServed{}
	pl := &fakePlanner{served: served, rentErr: context.Canceled}
	q, st := newQueue(t, pl)

	id := enqueue(t, st, "rtx2080ti", "a paper boat")

	if _, err := q.once(t.Context()); err == nil {
		t.Fatal("a cancelled rental was reported as success")
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusCancelled {
		t.Errorf("a rental ended by Ctrl-C left the request %q, want cancelled", got.Status)
	}
}
