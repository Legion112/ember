package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/genstore"
)

// TestRun_PacksOntoTheCardARentalAdvertised.
//
// THE MONEY DEFECT THE CARD CLOSES. `Compat` matched the card a request ASKED
// for against the card a live rental's opener asked for, and two asks can
// differ while the silicon agrees: a request that named no card lands on
// whatever the provider had, and a later request naming exactly that card was
// treated as work nothing up could serve -- so ember rented a second node for
// a card it already had warm. The second node pays the whole ~27s per-rental
// overhead and, for a resident model, a reload measured at 80-96s per image.
//
// TWO SLOTS, so the defect has somewhere to go: with max_nodes 1 a second
// rental is impossible and this test would pass against the bug.
func TestRun_PacksOntoTheCardARentalAdvertised(t *testing.T) {
	p := newFleetPlanner()
	// The ask is empty and the provider hands back an rtx4090, which is the
	// ordinary shape of a config with no `gpu:` and a browser that named none.
	p.cards[""] = "rtx4090"
	q, st := fleet(t, p, 2)

	enqueue(t, st, "", "whatever you have")
	enqueue(t, st, "rtx4090", "specifically an rtx4090")

	stop := runLoop(t, q, p)
	defer stop()

	// `batchesOn` is keyed by the ask, so the two requests land on the same
	// rental's counter only if the second rode the first -- and the opener's
	// ask is the empty one.
	deadline := time.Now().Add(20 * time.Second)
	for p.batchesOn("") < 2 {
		if plans := p.plans(); len(plans) > 1 {
			t.Fatalf("a request naming rtx4090 rented a %d%s node while a rental holding that "+
				"exact card was up (%v); the two asks differ and the silicon does not",
				len(plans), "th", plans)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live rental generated %d batch(es) in 20s, want 2; plans=%v",
				p.batchesOn(""), p.plans())
		}
		time.Sleep(2 * time.Millisecond)
	}

	if max, who := p.gate.peak(); max != 1 {
		t.Errorf("%d rentals were open at once (%v), want 1", max, who)
	}
	if plans := p.plans(); len(plans) != 1 || plans[0] != "" {
		t.Errorf("planned %v, want exactly one rental opened by the request that named no card", plans)
	}
}

// TestRun_StillCreatesForACardTheRentalDoesNotHave.
//
// The negative control for the test above, and without it that one is
// satisfied by a scheduler that packs everything onto the first rental it
// opens -- which would hand a request for an h100 to a node holding an
// rtx4090 and generate the batch on the wrong silicon after the card had been
// paid for.
func TestRun_StillCreatesForACardTheRentalDoesNotHave(t *testing.T) {
	p := newFleetPlanner()
	p.cards[""] = "rtx4090"
	q, st := fleet(t, p, 2)

	enqueue(t, st, "", "whatever you have")
	enqueue(t, st, "h100-80g", "specifically an h100")

	stop := runLoop(t, q, p)
	defer stop()

	waitFor(t, 20*time.Second, "a second node for a card the first rental does not have", func() bool {
		return p.batchesOn("") >= 1 && p.batchesOn("h100-80g") >= 1
	})
	if plans := p.plans(); len(plans) != 2 {
		t.Errorf("planned %v, want two rentals: an rtx4090 cannot serve a request for an h100", plans)
	}
}

// leaseStore is the clock and the sweep counter the lease assertions need.
//
// A WRAPPER RATHER THAN A FIELD ON THE FAKE, because what has to be observed
// is the SUPERVISOR's own calls while it is running: `RecordingStore.Calls` is
// written under the fake's lock and read without one, so polling it from a
// test is a data race `go test -race` reports and not an assertion. This
// counts under its own lock and moves the clock under it too.
type leaseStore struct {
	*genstore.RecordingStore

	mu       sync.Mutex
	offset   time.Duration
	sweeps   int
	renewals int
}

func newLeaseStore() *leaseStore {
	s := &leaseStore{RecordingStore: genstore.NewRecordingStore()}
	s.Now = s.now
	return s
}

// leaseEpoch is the fixed start, so that a frozen clock is still a plausible
// time rather than the zero one -- which an `omitempty` timestamp elsewhere
// reads as absent.
var leaseEpoch = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func (s *leaseStore) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return leaseEpoch.Add(s.offset)
}

func (s *leaseStore) advance(d time.Duration) {
	s.mu.Lock()
	s.offset += d
	s.mu.Unlock()
}

func (s *leaseStore) Reclaim(ctx context.Context) (int, error) {
	n, err := s.RecordingStore.Reclaim(ctx)
	s.mu.Lock()
	s.sweeps++
	s.mu.Unlock()
	return n, err
}

func (s *leaseStore) Renew(ctx context.Context, ids []genstore.ReqID) (int, error) {
	n, err := s.RecordingStore.Renew(ctx, ids)
	s.mu.Lock()
	if n > 0 {
		s.renewals++
	}
	s.mu.Unlock()
	return n, err
}

func (s *leaseStore) counts() (sweeps, renewals int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweeps, s.renewals
}

// TestRun_RenewsTheLeaseOfARentalItIsStillHolding.
//
// THE DEFECT IN THE SHAPE BEFORE THIS. The sweep ran once at startup over
// every running row, so a second ember marked the first one's live rentals
// interrupted; keying it on the lease fixes that only if something actually
// renews the lease of a rental in flight. Nothing renewing it would be the
// same defect with a two-minute delay instead of a restart -- and worse,
// because the sweep now runs on the supervisor's own loop, so this process
// would reclaim ITS OWN rentals.
//
// ITS OWN NEGATIVE CONTROL IN THE SAME BODY: a row claimed by a lease nobody
// holds is reclaimed by the same sweep that leaves the live one alone. Without
// it this test is satisfied by a sweep that does nothing at all.
func TestRun_RenewsTheLeaseOfARentalItIsStillHolding(t *testing.T) {
	p := newFleetPlanner()
	st := newLeaseStore()
	q := &Queue{Store: st, Planner: p, Workloads: speechWorkloads(t), Outputs: t.TempDir(), MaxNodes: 2}

	// A row claimed by an ember that is gone: it has a lease and nothing will
	// renew it.
	orphan := enqueue(t, st, "h100-80g", "claimed by an ember that is gone")
	if _, ok, err := st.ClaimNext(t.Context(), "a-dead-lease", genstore.Compat{}); err != nil || !ok {
		t.Fatalf("setup: ok=%v err=%v", ok, err)
	}

	inside := make(chan struct{})
	var once sync.Once
	proceed := make(chan struct{})
	q.Planner = generatingPlanner{p: p, inGenerate: func() {
		once.Do(func() { close(inside) })
		select {
		case <-proceed:
		case <-time.After(30 * time.Second):
		}
	}}

	live := enqueue(t, st, "rtx2080ti", "generating right now")

	stop := runLoop(t, q, p)
	defer func() { close(proceed); stop() }()

	waitFor(t, 20*time.Second, "the rental to reach its generation", func() bool {
		select {
		case <-inside:
			return true
		default:
			return false
		}
	})

	// Past the term. Everything claimed before now is expired unless the
	// supervisor pushed it forward.
	st.advance(genstore.LeaseTerm + time.Minute)

	before, _ := st.counts()
	// TWO SWEEPS AND NOT ONE, because the first may have been issued before
	// the clock moved: a loop iteration that read the old time proves nothing
	// about the new one.
	waitFor(t, 20*time.Second, "two sweeps after the clock moved", func() bool {
		q.Kick()
		n, _ := st.counts()
		return n >= before+2
	})

	got, err := st.Get(t.Context(), live)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusRunning {
		t.Errorf("the request this process is generating became %q: the sweep reclaimed a "+
			"rental its own supervisor is holding, and the row is the only local record of a "+
			"node that is still billing", got.Status)
	}
	if _, renewals := st.counts(); renewals == 0 {
		t.Error("nothing renewed a lease while a rental was in flight, so the row above " +
			"survived by luck rather than by the heartbeat")
	}

	gone, err := st.Get(t.Context(), orphan)
	if err != nil {
		t.Fatal(err)
	}
	if gone.Status != genstore.StatusInterrupted {
		t.Errorf("the orphaned request is %q, want interrupted: the sweep that spared the live "+
			"row has to take this one, or it spares everything", gone.Status)
	}
}
