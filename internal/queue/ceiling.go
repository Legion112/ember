package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
)

// The ceiling nothing had, and why it is not the mid-run meter that was
// refused.
//
// `max_hourly_usd` bounds a card. `max_run_usd` bounds a rental. NOTHING
// BOUNDED A SEQUENCE OF RENTALS, and `ember serve` is the first thing here
// that can produce one without the operator being at the keyboard for each:
// thirty queued requests at $1.79/hour on a one-hour TTL are inside every
// ceiling ember had and are $54.
//
// AGENTS.md already refuses a related request and was right to -- "max_run_usd
// cannot name the step it bit on, because it bites before any step exists" --
// and this is not that. A mid-run meter needs a clock, a price and a running
// total kept correct WHILE a rental is in flight, which is the thing that was
// declined. A period ceiling needs the completed cost of finished rentals,
// which is already recorded, numbered and durable, and it is checked at
// exactly the moment `max_run_usd` is: after the offer is chosen and before
// anything is created. No new meter and no new clock -- one more SUM over rows
// that already exist.
//
// WHAT IT READS IS COMPLETE, which is worth stating because `ember spend` has
// to warn that its per-job figures are not. `requests.cost` was a column
// before the execution columns existed, so the period total has no gap in it:
// every rental ember has ever recorded contributes.
//
// IT REFUSES RATHER THAN QUIETLY RENTING SOMETHING CHEAPER. Narrowing the
// run ceiling to the remaining headroom was the first design, and it would
// have meant an operator who asked for an H100 getting a 4090 because the
// month was nearly spent. ember exists to measure what a card costs, and a
// run that silently happened on different hardware is a measurement nobody
// can use.
//
// IT COUNTS RENTALS AND NOT STORE STANDING CHARGES. A store bills by the
// gigabyte-month whether or not anything runs, and `reconcile.MonthlyTotal`
// already prices one -- so a ceiling that ignores a $40/month store is wrong
// by a known amount. Counting it would mean a provider scan inside the drain
// loop, and a provider that could not be reached would leave two choices:
// block the queue, which is worse than the gap, or carry on without that
// store's charge, which is the gap with extra steps and a false sense that it
// was covered. `ember ls` is where a store's standing charge is reported, and
// it exits non-zero over a provider it could not ask.

// Period is a calendar period a ceiling covers.
//
// CALENDAR AND NOT ROLLING, because the thing an operator is defending
// against is an invoice and an invoice is calendar. The cost is a real hole,
// written down rather than hidden: a day ceiling resets at midnight UTC, so a
// queue can spend one day's allowance twice in two hours across the boundary.
// What bounds that is the month ceiling, which is why both exist and why the
// month is the one to set if only one is wanted.
//
// UTC, for the reason `ember spend`'s day bucket is UTC: a boundary in local
// time puts the same spend in different periods depending on where the
// operator was sitting, and a ceiling that moves with a laptop is a ceiling
// that cannot be reasoned about.
type Period string

const (
	PeriodDay   Period = "day"
	PeriodMonth Period = "month"
)

// Start is the beginning of the period containing t.
func (p Period) Start(t time.Time) time.Time {
	t = t.UTC()
	if p == PeriodMonth {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Key names the period in a message, as the date its allowance began.
func (p Period) Key(t time.Time) string {
	if p == PeriodMonth {
		return p.Start(t).Format("2006-01")
	}
	return p.Start(t).Format("2006-01-02")
}

// Ceilings are the configured per-period limits, zero for no limit.
//
// They are the config's and a request cannot narrow them, let alone widen
// one: there is no field for either on the wire type, so a browser is never
// consulted about the limit that stops it. That is the structural version of
// "a request may narrow the config's ceilings and never widen them" -- the
// request is not asked.
type Ceilings struct {
	Day   node.MicroUSD
	Month node.MicroUSD
}

// IsZero reports whether no period is bounded.
func (c Ceilings) IsZero() bool { return c.Day == 0 && c.Month == 0 }

// CeilingsOf reads the two period ceilings out of a config.
//
// ONE PARSE FOR THE WHOLE PROGRAM, which is the "one name for a path, not one
// per call site" rule applied to a money figure. config.PeriodCeilings had two
// non-test callers when #165 was filed -- this gate and a startup PRINT in
// cmd_serve -- so the key was read in exactly two places and enforced in one.
// Every path that rents now goes through this function and RefuseOverPeriod
// below, including the announcement, so a second spelling of "what does
// max_day_usd mean" cannot appear without deleting one of them.
//
// The wrapping names the file rather than the request: as Narrow says of the
// budgets, a ceiling that will not parse is the OPERATOR's error, and it is
// found before anything is rented.
func CeilingsOf(cfg config.Config) (Ceilings, error) {
	day, month, err := cfg.PeriodCeilings()
	if err != nil {
		return Ceilings{}, fmt.Errorf("the configured period ceiling: %w", err)
	}
	return Ceilings{Day: day, Month: month}, nil
}

// Tightest is the stricter of two configured ceilings, per period.
//
// IT EXISTS FOR ONE CALLER AND ONE REASON: a run submitted over the unix
// socket names its own config, and the LEDGER it is bounded against is the
// daemon's. Two operator statements reach one total, so both bind it and
// NEITHER IS WIDENED BY THE OTHER -- which is why this is a minimum and not a
// replacement.
//
// It is not `Narrow`'s rule reached again, and the difference matters because
// `ember up`'s overrides deliberately REPLACE. That rule is about a request,
// or a flag, against the file; the period ceilings have no flag and no field
// on any wire type, so there is nothing here that could replace anything. What
// is being composed is two files an operator wrote, and taking the smaller of
// two numbers an operator wrote cannot give them a limit they did not ask for.
//
// Zero means unbounded on both sides, through the same smallerUSD that
// Narrow's budgets use -- a config that sets neither must not narrow the
// other's to nothing and refuse every rental there is.
func (c Ceilings) Tightest(o Ceilings) Ceilings {
	return Ceilings{Day: smallerUSD(c.Day, o.Day), Month: smallerUSD(c.Month, o.Month)}
}

// RefuseOverPeriod is the max_day_usd and max_month_usd gate, for every path
// that rents.
//
// IT WAS A METHOD ON AppPlanner AND TWO OF FIVE RENTAL PATHS HAD IT (#165).
// Five paths write `requests.cost`, the column `genstore.Spend` sums with no
// status filter, all of them through AdoptRun or the queue's own Recorder --
// the queued request, the queue's store fill, `ember up` submitted over the
// socket, `ember up` in a shell, and `ember store sync` in a shell. The last
// three reached no ceiling at all, so `max_day_usd` read as a bound on a day
// and was a bound on the queue. That is a function on the package now, and the
// callers are the five.
//
// THE PROJECTION IS THE OFFER'S PRICE OVER THE TTL THE NODE WILL ACTUALLY
// GET, which is exactly what provision.Budget.Allow compares max_run_usd
// against -- so the figure a period is charged and the figure one rental is
// bounded by are the same arithmetic, and the two ceilings cannot come to
// disagree about what a rental may come to. Both halves of it are gates that
// have been measured unable to fail: replacing the price with 0, or the TTL
// with 0, left `go test ./...` at exit 0 before #157 (see moneygate_test.go),
// so the price and the ttl are parameters rather than fields read here, and
// every caller passes the plan's own.
//
// A CEILING WITH NO LEDGER TO READ IS A REFUSAL AND NOT A PASS -- see
// UnreadableLedger, which is that sentence and is shared with the shell paths.
//
// AND THE FREE CHECK IS Binds, UPSTREAM OF THE LEDGER QUESTION RATHER THAN
// INSIDE Allow. The first version of this function asked `cs.IsZero()` and
// then `led == nil`, so a FREE rental with a ceiling configured and no ledger
// was refused -- `Allow`'s own `projected <= 0` sits past the refusal and
// never gets asked. That is the defect this file's own comment four lines up
// names, written one line below it, and it shipped in #165's first form on the
// shell path where the branch is reachable. Review found it.
func RefuseOverPeriod(
	ctx context.Context,
	cs Ceilings,
	led Spender,
	now time.Time,
	ttl time.Duration,
	price node.Rate,
) error {
	if !cs.Binds(ttl, price) {
		return nil
	}
	if led == nil {
		return cs.UnreadableLedger(nil)
	}
	return cs.Allow(ctx, led, now, price.Cost(ttl))
}

// Binds reports whether these ceilings could refuse a rental projected at this
// price over this ttl.
//
// ONE PLACE ANSWERS "COULD THIS RENTAL BE REFUSED AT ALL", and the reason it
// is a named predicate rather than two conditions written out twice is that
// both refusals which happen WITHOUT reading a ledger have to ask it: the
// `led == nil` arm above, and `cli.boundAndRecord`'s arm for a generation
// history that will not open. The first form of #165 asked only `IsZero` at
// both, so `ember up --provider local` -- price zero, projection zero, adds
// nothing to any period -- was refused outright by a configured ceiling it
// could not check. Two copies of a money condition, and both were missing the
// same half.
//
// A FREE RENTAL IS NEVER REFUSED BY A MONEY CEILING, which is what the second
// half is: the local provider's price is zero, so its projection is zero and
// it adds nothing to any period. Refusing one because a paid month is spent
// would stop the one kind of run that costs nothing.
//
// The projection is derived here from the price and the ttl rather than taken
// as a figure, for RefuseOverPeriod's own reason: #157 measured a literal zero
// in either half as a gate that cannot fail, so a caller hands over the two
// facts and never the answer.
func (c Ceilings) Binds(ttl time.Duration, price node.Rate) bool {
	return !c.IsZero() && price.Cost(ttl) > 0
}

// UnreadableLedger is the refusal for a configured ceiling ember cannot check.
//
// ONE SENTENCE FOR ONE MONEY RULE. It existed twice with different wording --
// here and in `cli.boundAndRecord` -- which is two places for one refusal to
// drift and one place for the next reader to believe the other one. why is the
// cause when there is one to name (a database that would not open) and nil
// when there is not (nothing was wired).
//
// IT DOES NOT ASK WHETHER THE RENTAL IS FREE, and that is deliberate: Binds is
// the one place that question is answered, and a second copy of it here is the
// defect this pair was built to remove. Every caller asks Binds first.
func (c Ceilings) UnreadableLedger(why error) error {
	const s = "this configuration sets a period ceiling (%v a day, %v a month) and ember cannot " +
		"read what has been spent against it, so it cannot tell whether this rental is within " +
		"it and will not rent"
	if why == nil {
		return fmt.Errorf(s, c.Day, c.Month)
	}
	return fmt.Errorf(s+": %w", c.Day, c.Month, why)
}

// Spender is the one read a ceiling needs.
//
// One method rather than genstore.Store, so what the planner can do to the
// database is visible in its type: this is the layer that decides whether to
// spend money, and handing it a writer would mean reading the body to know it
// does not enqueue anything.
type Spender interface {
	Spend(ctx context.Context, q genstore.SpendQuery) ([]genstore.Bucket, error)
}

// Spent is what rentals in a period have come to so far.
//
// RunCost and not JobCost: the ceiling is about the invoice, and a rental is
// counted once on the request that opened it -- which is exactly what a
// period total must do, since every request that rode along was served by a
// node already paid for.
func Spent(ctx context.Context, s Spender, since time.Time) (node.MicroUSD, error) {
	bs, err := s.Spend(ctx, genstore.SpendQuery{Since: since})
	if err != nil {
		return 0, fmt.Errorf("read what has been spent since %s: %w", since.Format(time.DateOnly), err)
	}
	if len(bs) == 0 {
		// Nothing recorded in the period. An ungrouped aggregate over no rows
		// is no rows rather than a zero row, which is the honest shape and
		// the one `ember spend` prints "no generations recorded" for.
		return 0, nil
	}
	return bs[0].RunCost, nil
}

// CeilingError is a rental refused because a period would be carried past its
// ceiling.
//
// Typed, because the message is a contract: an operator reading it has to be
// able to tell which period bit, what had already been spent and what the
// limit is, without going to look any of it up. A bare "over budget" would
// send them to `ember spend` to find out which of two ceilings they had hit.
type CeilingError struct {
	Period    Period
	Since     time.Time
	Spent     node.MicroUSD
	Projected node.MicroUSD
	Ceiling   node.MicroUSD
}

func (e *CeilingError) Error() string {
	return fmt.Sprintf("the %s beginning %s has spent %v of its %s ceiling of %v, and this rental "+
		"is projected at %v -- which would take it to %v. Nothing was rented. Raise %s in "+
		"ember.yaml, or wait for the %s to turn over",
		e.Period, e.Period.Key(e.Since), e.Spent, e.key(), e.Ceiling,
		e.Projected, e.Spent+e.Projected, e.key(), e.Period)
}

// key is the config key that set this ceiling, so the remediation names the
// line to edit rather than the concept.
func (e *CeilingError) key() string { return "max_" + string(e.Period) + "_usd" }

// Allow refuses a rental projected to cost projected when it would carry a
// period past its ceiling.
//
// PROJECTED AND NOT SPENT, which is the whole difference between a ceiling and
// a report. A check that refuses only once a period has already been exceeded
// is a ceiling every period breaches by one rental -- and one rental is $54 in
// the case this exists for. The projection is the offer's price over the TTL,
// the same arithmetic provision.Budget.Allow uses for max_run_usd, so the two
// cannot disagree about what a rental may come to.
//
// THIS IS NOT THE AUTHORITY ON THE FREE-RENTAL RULE AND IT USED TO READ LIKE
// ONE. `Binds` is what the gate asks, before it decides whether to read a
// ledger at all; the `projected <= 0` below is this function's own floor for a
// DIRECT caller, because `Allow` is reachable with a ceiling already
// overspent, where `spent + 0 > ceiling` would refuse a rental that costs
// nothing. The two cannot disagree -- they are the same comparison on the same
// figure -- and the division of labour is that one decides whether the
// question is worth asking and the other answers it. Measured: deleting this
// one turns `TestCeilings_NeverRefuseAFreeRental` red, so it is live rather
// than a second copy kept for symmetry.
//
// THE `c.IsZero()` DISJUNCT IS GONE, and it was redundant with the loop below
// rather than merely unreachable -- which is the stronger reason and is why it
// could go without a behaviour change. With no ceiling set, `p.ceiling <= 0`
// skips both periods and nothing is read; measured, with the disjunct deleted,
// `go test ./...` is exit 0 AND `TestCeilings_NoCeilingReadsNothing` still
// passes, so the "an unconfigured ceiling reads the store" property was never
// resting on it. It was also dead from the only production caller once `Binds`
// landed in front of it, which review found and nothing reported: the line
// executes and is simply never true, so no coverage gate can see it.
//
// The periods are checked tightest first, because a day is the one an operator
// hits before a month and is the more useful thing to be told about.
func (c Ceilings) Allow(ctx context.Context, s Spender, now time.Time, projected node.MicroUSD) error {
	if projected <= 0 {
		return nil
	}
	for _, p := range []struct {
		period  Period
		ceiling node.MicroUSD
	}{{PeriodDay, c.Day}, {PeriodMonth, c.Month}} {
		if p.ceiling <= 0 {
			continue
		}
		since := p.period.Start(now)
		spent, err := Spent(ctx, s, since)
		if err != nil {
			// Returned and not swallowed. A ceiling that cannot read what has
			// been spent does not know whether this rental is allowed, and
			// renting on "probably fine" is the one answer a spend ceiling
			// must never give -- the same rule as `ember ls` exiting
			// non-zero over a provider it could not ask.
			return err
		}
		if spent+projected > p.ceiling {
			return &CeilingError{
				Period: p.period, Since: now, Spent: spent,
				Projected: projected, Ceiling: p.ceiling,
			}
		}
	}
	return nil
}
