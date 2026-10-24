package node

import (
	"fmt"
	"time"
)

// Billing is what one node is expected to cost.
//
// "Expected", not "charged". ember knows the rate the provider quoted and how
// long the node existed on this side of the connection; it does not know the
// provider's own clock, its rounding, its minimum, or whether a stopped pod is
// still accruing storage. Every figure here is therefore an estimate, and the
// String method says so -- an operator reconciling against an invoice needs to
// know which number is which.
type Billing struct {
	Rate     Rate
	Elapsed  time.Duration
	Billable time.Duration
	Cost     MicroUSD
}

// Estimate computes what a node that ran from start to end should cost.
//
// min is the provider's smallest billing unit. A node destroyed after four
// seconds on a provider with a one-minute minimum costs a minute, and an
// estimate that reported four seconds would teach the operator that short runs
// are nearly free -- which is the belief that makes a hundred aborted
// provisions look like nothing on a statement.
//
// Rounded up through Rate.Cost, because an estimate that reads low is one that
// lets a run start which the budget would have refused.
func Estimate(rate Rate, start, end time.Time, min time.Duration) Billing {
	elapsed := end.Sub(start)
	if elapsed < 0 {
		// A clock that went backwards, or a record whose timestamps were
		// written by two machines. Zero rather than a negative cost: a
		// negative total silently offsets a real one in any sum.
		elapsed = 0
	}
	billable := elapsed
	if billable < min {
		billable = min
	}
	return Billing{
		Rate:     rate,
		Elapsed:  elapsed,
		Billable: billable,
		Cost:     rate.Cost(billable),
	}
}

// String is one operator-facing line.
func (b Billing) String() string {
	s := fmt.Sprintf("~%s over %s at %s", b.Cost, round(b.Elapsed), b.Rate)
	if b.Billable > b.Elapsed {
		// Said out loud, because "it ran for 4s and cost a minute" is
		// surprising exactly once and then only if nobody mentioned it.
		s += fmt.Sprintf(" (billed as %s, the provider's minimum)", round(b.Billable))
	}
	return s
}

// round trims a duration to something readable in one line.
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute)
	case d >= time.Minute:
		return d.Round(time.Second)
	default:
		return d.Round(time.Millisecond)
	}
}

// Free reports whether this came to nothing.
//
// Used to decide whether to print a cost line at all: the local provider is
// genuinely free, and a run against it reporting "~$0" invites the reader to
// wonder whether the accounting is broken.
func (b Billing) Free() bool { return b.Rate == 0 }
