package genstore

import (
	"testing"
	"time"
)

// TestRetry_PutsAFailedRequestBackOnTheQueue.
//
// AN OPERATOR ASKED FOR A BUTTON, and the choice behind it is that the row is
// REUSED rather than copied: a new request would have left the failure in the
// ledger untouched, and one row keeps the queue from filling with retries of
// the same work. That makes this statement responsible for undoing an
// attempt's worth of columns without undoing the request.
//
// THE COST SURVIVES AND THE LEASE DOES NOT. A request that failed after
// renting has already spent money, and the row is the total across every
// attempt -- resetting it would lose money somebody paid. The lease is the
// opposite: a queued row carrying a live one is skipped by ClaimNext's `NOT
// (...)` arm and ignored by Reclaim, which only looks at running rows, so the
// retry would appear to work and the request would sit there until the lease
// aged out.
func TestRetry_PutsAFailedRequestBackOnTheQueue(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
		setClock(t, s, func() time.Time { return at })

		id, err := s.Enqueue(t.Context(), req("one"))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, cerr := s.ClaimNext(t.Context(), "run-1", Compat{}); cerr != nil || !ok {
			t.Fatalf("setup claim: ok=%v err=%v", ok, cerr)
		}
		// A failure that already cost money, which is the case the cost rule
		// is about.
		if err := s.Update(t.Context(), id, func(q *Request) {
			q.Status = StatusFailed
			q.Err = "no store named models at local"
			q.Node = "local/n1"
			q.Cost = 1_250_000
		}); err != nil {
			t.Fatal(err)
		}

		ok, err := s.Retry(t.Context(), id)
		if err != nil {
			t.Fatalf("Retry = %v", err)
		}
		if !ok {
			t.Fatal("a failed request was not retried")
		}

		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusQueued {
			t.Errorf("status = %q, want queued", got.Status)
		}
		if got.Cost != 1_250_000 {
			t.Errorf("cost = %d, want the 1250000 the failed attempt spent; a retry adds to "+
				"what this request has cost and does not start from nothing", got.Cost)
		}
		if got.Err != "" || got.Node != "" {
			t.Errorf("err = %q node = %q, want both cleared: they describe the attempt and the "+
				"row now reads queued", got.Err, got.Node)
		}

		// AND IT IS CLAIMABLE AGAIN, which is the whole point and the half a
		// status check alone would miss: a row left queued with a live lease
		// passes every assertion above and is never picked up.
		r, ok, err := s.ClaimNext(t.Context(), "run-2", Compat{})
		if err != nil {
			t.Fatal(err)
		}
		if !ok || r.ID != id {
			t.Errorf("the retried request was not claimable: ok=%v id=%v; its lease outlived "+
				"the retry", ok, r.ID)
		}
	})
}

// TestRetry_RefusesEveryStatusThatIsNotTerminal.
//
// THREE NAMES AND NOT "NOT QUEUED OR RUNNING", because the two readings differ
// on `done` -- a finished request has nothing to retry, and it is the row an
// operator is most likely to press by accident. A queued or running one is
// already live, and putting it back would hand the drain loop a second claim
// on work it is in the middle of.
func TestRetry_RefusesEveryStatusThatIsNotTerminal(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
		setClock(t, s, func() time.Time { return at })

		for _, c := range []struct {
			what   string
			status Status
		}{
			{"a finished request", StatusDone},
			{"a queued request", StatusQueued},
			{"a running request", StatusRunning},
		} {
			id, err := s.Enqueue(t.Context(), req(c.what))
			if err != nil {
				t.Fatal(err)
			}
			if c.status != StatusQueued {
				if err := s.Update(t.Context(), id, func(q *Request) { q.Status = c.status }); err != nil {
					t.Fatal(err)
				}
			}
			ok, err := s.Retry(t.Context(), id)
			if err != nil {
				t.Errorf("%s: Retry = %v", c.what, err)
			}
			if ok {
				t.Errorf("%s was retried", c.what)
			}
			got, gerr := s.Get(t.Context(), id)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got.Status != c.status {
				t.Errorf("%s: status became %q, so the row was written anyway",
					c.what, got.Status)
			}
		}
	})
}

// TestRetry_OfARowThatIsNotThereIsNotAnError.
//
// A request id comes from a URL an operator can edit, and a 404 is the
// handler's answer rather than a store error -- so `false, nil` is what lets
// the two be told apart one layer up.
func TestRetry_OfARowThatIsNotThereIsNotAnError(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		ok, err := s.Retry(t.Context(), ReqID("0123456789abcdef"))
		if err != nil {
			t.Errorf("Retry of a missing row = %v, want no error", err)
		}
		if ok {
			t.Error("a row that is not there was retried")
		}
	})
}

// TestRetry_OnlyOneOfTwoPressesWins.
//
// ONE STATEMENT, which is ClaimNext's discipline in the other direction. Two
// operators pressing the button on one row -- or one pressing twice -- must
// not both succeed, because the second would requeue a request the drain loop
// may already have claimed. The status condition is what decides, and a
// handler that read the status and then wrote would leave exactly the window
// ClaimNext is one statement to avoid.
func TestRetry_OnlyOneOfTwoPressesWins(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("one"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(t.Context(), id, func(q *Request) { q.Status = StatusFailed }); err != nil {
			t.Fatal(err)
		}

		first, err := s.Retry(t.Context(), id)
		if err != nil || !first {
			t.Fatalf("the first press = %v, %v; want true", first, err)
		}
		second, err := s.Retry(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if second {
			t.Error("a second press requeued a request that was already queued, which would " +
				"hand the drain loop a second claim on one row")
		}
	})
}
