package node

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestEstimate_AnOrdinaryRun(t *testing.T) {
	// $0.69/hr for 30 minutes.
	b := Estimate(Rate(690_000), t0, t0.Add(30*time.Minute), time.Minute)

	if b.Elapsed != 30*time.Minute {
		t.Errorf("elapsed = %v", b.Elapsed)
	}
	if b.Billable != 30*time.Minute {
		t.Errorf("billable = %v", b.Billable)
	}
	if b.Cost != MicroUSD(345_000) {
		t.Errorf("cost = %v (%d micro), want $0.345", b.Cost, int64(b.Cost))
	}
}

// TestEstimate_ShortRunsCostTheProviderMinimum. A node destroyed after four
// seconds on a provider with a one-minute minimum costs a minute, and an
// estimate reporting four seconds would teach the operator that short runs
// are nearly free -- which is the belief that makes a hundred aborted
// provisions look like nothing on a statement.
func TestEstimate_ShortRunsCostTheProviderMinimum(t *testing.T) {
	b := Estimate(Rate(690_000), t0, t0.Add(4*time.Second), time.Minute)

	if b.Elapsed != 4*time.Second {
		t.Errorf("elapsed = %v", b.Elapsed)
	}
	if b.Billable != time.Minute {
		t.Errorf("billable = %v, want the provider minimum", b.Billable)
	}
	if b.Cost != MicroUSD(11_500) {
		t.Errorf("cost = %v (%d micro), want a minute at $0.69/hr", b.Cost, int64(b.Cost))
	}
	// And it says so, because that is surprising exactly once.
	if !strings.Contains(b.String(), "minimum") {
		t.Errorf("the minimum was applied silently: %q", b.String())
	}
}

func TestEstimate_NoMinimum(t *testing.T) {
	b := Estimate(Rate(3_600_000), t0, t0.Add(time.Second), 0)
	if b.Billable != time.Second {
		t.Errorf("billable = %v", b.Billable)
	}
	// $3.60/hr for one second is one tenth of a cent.
	if b.Cost != MicroUSD(1000) {
		t.Errorf("cost = %v (%d micro)", b.Cost, int64(b.Cost))
	}
	if strings.Contains(b.String(), "minimum") {
		t.Errorf("a minimum was reported where none applies: %q", b.String())
	}
}

// TestEstimate_AClockThatWentBackwardsCostsZero, not a negative. A negative
// total silently offsets a real one in any sum, which is the worst shape an
// accounting bug can take.
func TestEstimate_AClockThatWentBackwardsCostsZero(t *testing.T) {
	b := Estimate(Rate(690_000), t0, t0.Add(-time.Hour), 0)
	if b.Elapsed != 0 {
		t.Errorf("elapsed = %v, want 0", b.Elapsed)
	}
	if b.Cost != 0 {
		t.Errorf("cost = %v, want 0", b.Cost)
	}
	if b.Cost < 0 {
		t.Fatal("a negative cost would offset a real one in any sum")
	}
}

// TestEstimate_RoundsUp. An estimate that reads low is one that lets a run
// start which the budget would have refused.
func TestEstimate_RoundsUp(t *testing.T) {
	// A rate and duration whose product is not a whole microdollar.
	b := Estimate(Rate(1), t0, t0.Add(time.Second), 0)
	if b.Cost != MicroUSD(1) {
		t.Errorf("cost = %d micro, want 1: a fraction of a microdollar must round up", int64(b.Cost))
	}
}

func TestEstimate_AFreeProviderIsFree(t *testing.T) {
	b := Estimate(0, t0, t0.Add(time.Hour), time.Minute)
	if !b.Free() {
		t.Error("a zero rate does not report itself as free")
	}
	if b.Cost != 0 {
		t.Errorf("cost = %v", b.Cost)
	}
	if (Billing{Rate: 1}).Free() {
		t.Error("a priced run reports itself as free")
	}
}

func TestBilling_String(t *testing.T) {
	b := Estimate(Rate(690_000), t0, t0.Add(90*time.Minute), time.Minute)
	got := b.String()
	// A tilde, because ember knows the quoted rate and its own elapsed time,
	// not the provider's clock or its rounding.
	if !strings.HasPrefix(got, "~") {
		t.Errorf("the estimate does not present itself as one: %q", got)
	}
	for _, want := range []string{"1h30m", "/hr"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing %q", got, want)
		}
	}
}

func TestRound(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		2*time.Hour + 30*time.Minute + 29*time.Second: 2*time.Hour + 30*time.Minute,
		90*time.Second + 400*time.Millisecond:         90 * time.Second,
		1500 * time.Microsecond:                       2 * time.Millisecond,
	} {
		if got := round(in); got != want {
			t.Errorf("round(%v) = %v, want %v", in, got, want)
		}
	}
}
