package queue

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
)

// The ledger race at max_nodes>1 (#204), and what Commitments does and does
// not close.
//
// `requests.cost` is written from provision.WithNode's deferred teardown, so a
// rental in flight contributes ZERO to the ledger a ceiling reads. #165 closed
// that for one rental by re-checking on the drain loop, which works because
// the loop is serial; with N slots planning at once the overshoot is bounded by
// N rather than by 1.

// TestAdmit_TwoConcurrentCreatesCannotBothSeeAHalfSpentDay is the whole
// mechanism, as the arithmetic rather than as a race.
//
// A $20 day with $19 recorded has room for exactly one $0.89 rental. Two
// creates that both read the ledger before either is recorded both see $19.89
// and both admit, and the day ends at $20.78 -- which is the overshoot this
// type exists to refuse. Admitted serially here because that is what the lock
// makes of two concurrent calls, and a test that raced for it would be the
// flake this package has already been corrected for once.
func TestAdmit_TwoConcurrentCreatesCannotBothSeeAHalfSpentDay(t *testing.T) {
	c := &Commitments{}
	const projected = node.MicroUSD(890_000)

	// The check a real caller passes: the ledger says $19 and the ceiling is
	// $20, so what decides is whether `committed` is in the sum.
	check := func(committed node.MicroUSD) error {
		if 19_000_000+committed+projected > 20_000_000 {
			return errors.New("over the ceiling")
		}
		return nil
	}

	if err := c.Admit("first", projected, check); err != nil {
		t.Fatalf("the first create was refused with the day's headroom intact: %v", err)
	}
	if err := c.Admit("second", projected, check); err == nil {
		t.Error("a second create was admitted against a ledger in which the first is worth " +
			"nothing: that is the overshoot bounded by N rather than by 1, and it is the whole " +
			"reason this type exists")
	}

	// AND THE SECOND IS NOT RECORDED, which is the half that stops a refusal
	// subtracting headroom from every later rental for the life of the
	// process: an operator would read that as a ceiling that had stopped
	// working.
	if got := c.Committed(); got != projected {
		t.Errorf("Committed() = %v after one admit and one refusal, want %v", got, projected)
	}
}

// TestRelease_GivesTheHeadroomBack, because a commitment that outlived its
// rental would shrink every later one.
//
// The handover is lossless by ORDER rather than by arithmetic: the slot
// releases after the deferred teardown has emitted run_cost and the recorder
// has flushed it, so `requests.cost` holds what the entry was standing in for
// by the time it goes.
func TestRelease_GivesTheHeadroomBack(t *testing.T) {
	c := &Commitments{}
	if err := c.Admit("a", 500_000, func(node.MicroUSD) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.Admit("b", 300_000, func(node.MicroUSD) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := c.Committed(); got != 800_000 {
		t.Fatalf("Committed() = %v, want 800000", got)
	}

	c.Release("a")
	if got := c.Committed(); got != 300_000 {
		t.Errorf("Committed() = %v after releasing one of two, want 300000", got)
	}

	// IDEMPOTENT, AND A KEY NOBODY ADMITTED IS A NO-OP. The release is a
	// defer at every site and the admit may have been refused, so a
	// subtraction that ran twice -- or for a rental that was never
	// admitted -- would leave this process believing it had headroom it does
	// not.
	c.Release("a")
	c.Release("never-admitted")
	if got := c.Committed(); got != 300_000 {
		t.Errorf("Committed() = %v after releasing an absent key twice, want 300000", got)
	}
}

// TestCommitments_ANilIsTodaysArithmetic.
//
// Every `queue.Queue{}` and `AppPlanner{}` literal in the suite predates this
// type, and a nil that behaved differently from an empty one would mean the
// tests measured a mechanism production does not have. So a nil calls the
// check with zero and records nothing -- which is exactly what every caller
// got before N>1 -- and neither method panics.
func TestCommitments_ANilIsTodaysArithmetic(t *testing.T) {
	var c *Commitments

	var saw node.MicroUSD = -1
	if err := c.Admit("a", 500_000, func(committed node.MicroUSD) error {
		saw = committed
		return nil
	}); err != nil {
		t.Fatalf("a nil Commitments refused an admit: %v", err)
	}
	if saw != 0 {
		t.Errorf("a nil Commitments offered a committed total of %v, want 0", saw)
	}
	if got := c.Committed(); got != 0 {
		t.Errorf("a nil Commitments reports %v committed", got)
	}
	c.Release("a") // must not panic

	// AND THE REFUSAL STILL REACHES THE CALLER, so a nil is not a bypass of
	// the ceiling itself: what it drops is the concurrency term.
	want := errors.New("over the ceiling")
	if err := c.Admit("b", 1, func(node.MicroUSD) error { return want }); !errors.Is(err, want) {
		t.Errorf("a nil Commitments swallowed the ceiling's refusal: %v", err)
	}
}

// TestAdmit_RunsTheCheckUnderOneLock, which is the property the arithmetic
// above cannot see.
//
// A read of the committed total, then a check, then a record is three
// statements with two windows in them -- and at max_nodes>1 both windows are a
// second create. This drives two goroutines through a check that blocks until
// it has been entered twice, and the assertion is that the second one never
// gets in: they are serialised, so the second sees the first's figure.
//
// IT CARRIES ITS OWN DEADLINE, because what it is driving is a wait: a check
// that really did run concurrently would be reported by the first goroutine
// seeing a zero total, and a check that is serialised makes the second
// goroutine wait -- which with no bound is a wedge, and a wedge prints no
// `--- FAIL`.
func TestAdmit_RunsTheCheckUnderOneLock(t *testing.T) {
	c := &Commitments{}

	var mu sync.Mutex
	var entered int
	var sawConcurrent bool
	in := make(chan struct{}, 2)

	admit := func(id genstore.ReqID) {
		_ = c.Admit(id, 100, func(node.MicroUSD) error {
			mu.Lock()
			entered++
			if entered > 1 {
				sawConcurrent = true
			}
			mu.Unlock()
			in <- struct{}{}
			// Held long enough that a second check running concurrently
			// would be inside this body at the same time.
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			entered--
			mu.Unlock()
			return nil
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); admit("a") }()
	go func() { defer wg.Done(); admit("b") }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("two admits did not finish in 20s")
	}

	if sawConcurrent {
		t.Error("two ceiling checks were inside Admit at the same time, so each could read a " +
			"total the other was about to change: the check has to run with the lock held, " +
			"which is ClaimNext's one-statement argument with a mutex in place of SQLite")
	}
	if got := c.Committed(); got != 200 {
		t.Errorf("Committed() = %v after two admits, want 200", got)
	}
	// Both checks really ran, which is what stops the assertion above being
	// satisfied by an Admit that never called one.
	if len(in) != 2 {
		t.Errorf("%d check(s) ran, want 2", len(in))
	}
}
