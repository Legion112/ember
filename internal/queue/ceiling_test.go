package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/local"
)

// at is a fixed instant in the middle of a month, so a day boundary and a
// month boundary are both a known distance away.
func at(day, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 30, 0, 0, time.UTC)
}

// spentOn seeds one finished rental's cost into a store the ceiling can read.
//
// Through the store's own interface rather than by writing SQL, so the fake
// and the real one answer the same question. The ceiling reads RunCost, which
// is written on the request that OPENED each rental -- so this is what a
// period of spending looks like in the database.
func spentOn(t *testing.T, s genstore.Store, created time.Time, cost node.MicroUSD) {
	t.Helper()
	id, err := s.Enqueue(t.Context(), genstore.Request{
		Provider: "runpod", GPU: "a100-80g", TTL: time.Hour, Created: created,
		Jobs: []genstore.Job{{Prompt: "a cat"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(t.Context(), id, func(r *genstore.Request) {
		r.Status = genstore.StatusDone
		r.RunID = "run-" + string(id)
		r.Started, r.Ended = created, created.Add(time.Hour)
		r.Cost, r.Billable = cost, time.Hour
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPeriod_StartsTheCalendarPeriodInUTC(t *testing.T) {
	// Deliberately an instant that is a different day in most time zones, so
	// a start computed in local time would land in a different period.
	now := time.Date(2026, 11, 17, 23, 45, 0, 0, time.UTC)
	cases := []struct {
		p    Period
		want string
	}{
		{PeriodDay, "2026-11-17T00:00:00Z"},
		{PeriodMonth, "2026-11-01T00:00:00Z"},
	}
	for _, c := range cases {
		if got := c.p.Start(now).Format(time.RFC3339); got != c.want {
			t.Errorf("%s.Start = %s, want %s", c.p, got, c.want)
		}
	}
	if got := PeriodDay.Key(now); got != "2026-11-17" {
		t.Errorf("day key = %q", got)
	}
	if got := PeriodMonth.Key(now); got != "2026-11" {
		t.Errorf("month key = %q", got)
	}
}

// A projection and not what has been spent, which is the whole difference
// between a ceiling and a report: a check that refuses only once a period has
// already been exceeded is a ceiling every period breaches by one rental.
func TestCeilings_RefuseARentalThatWouldCrossTheCeiling(t *testing.T) {
	s := genstore.NewRecordingStore()
	spentOn(t, s, at(17, 9), 40_000_000) // $40 earlier today

	cs := Ceilings{Month: 50_000_000} // $50 this month

	// $8 more is $48, inside the ceiling.
	if err := cs.Allow(t.Context(), s, at(17, 12), 8_000_000); err != nil {
		t.Errorf("a rental that stays inside the ceiling was refused: %v", err)
	}

	// $12 more would be $52. Nothing has been overspent yet, which is the
	// point: the refusal happens before the money does.
	err := cs.Allow(t.Context(), s, at(17, 12), 12_000_000)
	var ce *CeilingError
	if !errors.As(err, &ce) {
		t.Fatalf("Allow = %v, want a CeilingError", err)
	}
	if ce.Period != PeriodMonth {
		t.Errorf("the refusal names the %s, want the month", ce.Period)
	}
	// The message is the contract: which period, what was spent, what the
	// ceiling is, and the key to edit. An operator must not have to go and
	// look any of it up.
	for _, want := range []string{"month", "2026-11", "$40", "$50", "$12", "$52", "max_month_usd"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// Exactly at the ceiling is allowed, which is decidable because money is an
// integer here.
func TestCeilings_AllowARentalExactlyAtTheCeiling(t *testing.T) {
	s := genstore.NewRecordingStore()
	spentOn(t, s, at(17, 9), 40_000_000)

	cs := Ceilings{Month: 50_000_000}
	if err := cs.Allow(t.Context(), s, at(17, 12), 10_000_000); err != nil {
		t.Errorf("a rental landing exactly on the ceiling was refused: %v", err)
	}
	if err := cs.Allow(t.Context(), s, at(17, 12), 10_000_001); err == nil {
		t.Error("one microdollar over the ceiling was allowed")
	}
}

// The day ceiling is the tighter period and the one an operator hits first.
func TestCeilings_CountOnlyTheirOwnPeriod(t *testing.T) {
	s := genstore.NewRecordingStore()
	spentOn(t, s, at(3, 10), 30_000_000)  // earlier this month
	spentOn(t, s, at(17, 10), 10_000_000) // today

	cs := Ceilings{Day: 15_000_000, Month: 100_000_000}

	// $6 more is $16 today: over the day ceiling, nowhere near the month's.
	err := cs.Allow(t.Context(), s, at(17, 12), 6_000_000)
	var ce *CeilingError
	if !errors.As(err, &ce) {
		t.Fatalf("Allow = %v, want a CeilingError", err)
	}
	if ce.Period != PeriodDay {
		t.Errorf("the refusal names the %s, want the day", ce.Period)
	}
	if ce.Spent != 10_000_000 {
		t.Errorf("the day counted %v, want only today's $10", ce.Spent)
	}

	// $4 more is $14 today and $44 this month: inside both.
	if err := cs.Allow(t.Context(), s, at(17, 12), 4_000_000); err != nil {
		t.Errorf("a rental inside both ceilings was refused: %v", err)
	}
}

// The hole in a calendar ceiling, written down rather than hidden: a day's
// allowance can be spent twice in two hours across midnight. What bounds that
// is the month.
func TestCeilings_ADayTurnsOverAtMidnightUTC(t *testing.T) {
	s := genstore.NewRecordingStore()
	spentOn(t, s, at(17, 23), 14_000_000) // 23:30 on the 17th

	cs := Ceilings{Day: 15_000_000}

	if err := cs.Allow(t.Context(), s, at(17, 23), 2_000_000); err == nil {
		t.Error("a rental over the day's remaining allowance was allowed")
	}
	// Half an hour later, in the next UTC day, the allowance is fresh. This
	// is the documented cost of matching an invoice rather than rolling.
	if err := cs.Allow(t.Context(), s, at(18, 0), 2_000_000); err != nil {
		t.Errorf("the new day did not start with a fresh allowance: %v", err)
	}
	// And the month ceiling is what bounds the pair.
	withMonth := Ceilings{Day: 15_000_000, Month: 15_000_000}
	if err := withMonth.Allow(t.Context(), s, at(18, 0), 2_000_000); err == nil {
		t.Error("the month ceiling did not bound a day that turned over")
	}
}

// A free rental adds nothing to any period, so a money ceiling must never
// refuse one -- that would stop the only kind of run that costs nothing.
func TestCeilings_NeverRefuseAFreeRental(t *testing.T) {
	s := genstore.NewRecordingStore()
	spentOn(t, s, at(17, 9), 999_000_000)

	cs := Ceilings{Day: 1_000, Month: 1_000}
	if err := cs.Allow(t.Context(), s, at(17, 12), 0); err != nil {
		t.Errorf("a free rental was refused by a money ceiling: %v", err)
	}
}

func TestCeilings_NoCeilingReadsNothing(t *testing.T) {
	s := genstore.NewRecordingStore()
	if err := (Ceilings{}).Allow(t.Context(), s, at(17, 12), 50_000_000); err != nil {
		t.Fatal(err)
	}
	if len(s.Calls) != 0 {
		t.Errorf("an unconfigured ceiling read the store: %v", s.Calls)
	}
}

// "I could not read what has been spent" is not "it is allowed". The same
// rule that makes `ember ls` exit non-zero over a provider it could not
// ask.
func TestCeilings_RefuseWhenTheSpendCannotBeRead(t *testing.T) {
	s := genstore.NewRecordingStore()
	s.FailOn = "Spend"

	err := (Ceilings{Month: 50_000_000}).Allow(t.Context(), s, at(17, 12), 1_000_000)
	if err == nil {
		t.Fatal("a rental was allowed although the period spend could not be read")
	}
	var ce *CeilingError
	if errors.As(err, &ce) {
		t.Errorf("a failed read was reported as a ceiling refusal: %v", err)
	}
}

// An empty period is zero and not an error: an ungrouped aggregate over no
// rows comes back as no rows.
func TestSpent_IsZeroForAPeriodWithNothingInIt(t *testing.T) {
	s := genstore.NewRecordingStore()
	got, err := Spent(t.Context(), s, at(17, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("Spent = %v over an empty period, want zero", got)
	}
}

// The period total is the invoice, so a rental is counted once -- on the
// request that opened it, however many rode along.
func TestSpent_CountsARentalOnceHoweverManyRequestsRodeAlong(t *testing.T) {
	s := genstore.NewRecordingStore()
	spentOn(t, s, at(17, 9), 17_900_000)

	// A second request on the same rental, as the queue records one: its own
	// card time and no rental cost.
	id, err := s.Enqueue(t.Context(), genstore.Request{
		Provider: "runpod", GPU: "a100-80g", TTL: time.Hour, Created: at(17, 10),
		Jobs: []genstore.Job{{Prompt: "a dog"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(t.Context(), id, func(r *genstore.Request) {
		r.Status = genstore.StatusDone
		r.Started, r.Ended = at(17, 10), at(17, 11)
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Spent(t.Context(), s, PeriodDay.Start(at(17, 12)))
	if err != nil {
		t.Fatal(err)
	}
	if got != 17_900_000 {
		t.Errorf("Spent = %v, want one rental at $17.90", got)
	}
}

func TestPeriodCeilings_ParseAsIntegerMicrodollars(t *testing.T) {
	// 2.01 is a value that makes the float version wrong:
	// ParseFloat("2.01")*1e6 is 2009999.9999999998, which truncates to
	// 2009999. 0.29 -- the value this repository cited for it everywhere --
	// comes out exactly right, which is why the test uses one that does not.
	day, month, err := config.Config{MaxDayUSD: "2.01", MaxMonthUSD: "250"}.PeriodCeilings()
	if err != nil {
		t.Fatal(err)
	}
	if day != 2_010_000 {
		t.Errorf("max_day_usd = %d microdollars, want 2010000", day)
	}
	if month != 250_000_000 {
		t.Errorf("max_month_usd = %d microdollars, want 250000000", month)
	}

	none, _, err := config.Config{}.PeriodCeilings()
	if err != nil || none != 0 {
		t.Errorf("an unset ceiling = %v, %v; want zero and no error", none, err)
	}

	if _, _, err := (config.Config{MaxMonthUSD: "lots"}).PeriodCeilings(); err == nil {
		t.Error("max_month_usd: lots was accepted")
	}
}

// A ceiling with no ledger to read is a refusal, because an operator who
// configured one has said unattended spending must stop somewhere.
func TestPlanner_RefusesAConfiguredCeilingWithNothingToReadItAgainst(t *testing.T) {
	p := AppPlanner{Config: config.Config{MaxMonthUSD: "50"}}
	err := p.refuseOverPeriod(t.Context(), time.Hour, node.Rate(1_790_000))
	if err == nil {
		t.Fatal("a period ceiling with no ledger was treated as satisfied")
	}
	if !strings.Contains(err.Error(), "will not rent") {
		t.Errorf("the refusal does not say nothing was rented: %v", err)
	}
}

func TestPlanner_ReportsTheOperatorsOwnBadCeiling(t *testing.T) {
	p := AppPlanner{Config: config.Config{MaxDayUSD: "~~"}, Ledger: genstore.NewRecordingStore()}
	err := p.refuseOverPeriod(t.Context(), time.Hour, node.Rate(1_790_000))
	if err == nil || !strings.Contains(err.Error(), "max_day_usd") {
		t.Fatalf("refuseOverPeriod = %v, want it to name max_day_usd", err)
	}
}

// The projection is the offer's price over the TTL, which is the same
// arithmetic max_run_usd is compared against.
func TestPlanner_ProjectsThePriceOverTheTTL(t *testing.T) {
	s := genstore.NewRecordingStore()
	spentOn(t, s, at(17, 9), 40_000_000)

	p := AppPlanner{
		Config: config.Config{MaxMonthUSD: "50"},
		Ledger: s,
		Now:    func() time.Time { return at(17, 12) },
	}

	// $1.79/hr for half an hour is $0.895: $40.895, inside $50.
	if err := p.refuseOverPeriod(t.Context(), 30*time.Minute, node.Rate(1_790_000)); err != nil {
		t.Errorf("a half-hour rental was refused: %v", err)
	}
	// The same card for six hours is $10.74: $50.74, over it.
	if err := p.refuseOverPeriod(t.Context(), 6*time.Hour, node.Rate(1_790_000)); err == nil {
		t.Error("a six-hour rental past the ceiling was allowed")
	}
}

// TestOnce_RecordsACeilingRefusalOnTheRequest.
//
// THE OTHER HALF OF THE CHAIN. The arithmetic above is tested against the
// store; this is what the drain loop does with the refusal, and it is what
// #27's done-when asks for: the next selection refuses and the request says
// why, so the operator reads it in the page and in `ember history` rather
// than finding out from an invoice.
//
// Nothing is rented. Plan rents nothing by construction, so the refusal costs
// the offer lookup and not a node.
func TestOnce_RecordsACeilingRefusalOnTheRequest(t *testing.T) {
	ceiling := &CeilingError{
		Period: PeriodMonth, Since: at(17, 12),
		Spent: 48_000_000, Projected: 8_950_000, Ceiling: 50_000_000,
	}
	pl := &fakePlanner{served: &fakeServed{}, planErr: ceiling}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "a")

	worked, err := q.once(t.Context())
	if err == nil {
		t.Fatal("a rental refused by a period ceiling was reported as success")
	}
	if !worked {
		t.Error("the queue claimed it did nothing, but it claimed a request")
	}
	if rentals, _ := pl.counts(); rentals != 0 {
		t.Errorf("rented %d node(s) past a period ceiling", rentals)
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	for _, want := range []string{"month", "2026-11", "$48", "$50", "max_month_usd"} {
		if !strings.Contains(got.Err, want) {
			t.Errorf("the recorded reason does not mention %q: %q", want, got.Err)
		}
	}
}

// TestAppPlanner_ChecksThePeriodCeilingBeforeRenting.
//
// THE LINK BETWEEN THE TWO HALVES, and it is the one a unit test of the
// arithmetic cannot make: that Plan actually reaches the check. A query layer
// and a loop that each agree with themselves across a gap no test crosses is
// the failure this repository has paid for three times -- the unrecorded
// preview frame, capabilities.model, and the eleven of fifty-one parameters
// the UI could send.
//
// It is reached through the no-ledger refusal rather than through a price,
// because the local provider's offers are free and a free rental is never
// refused by a money ceiling. The recording Docker engine is what proves
// nothing was created.
func TestAppPlanner_ChecksThePeriodCeilingBeforeRenting(t *testing.T) {
	engine := dockerengine.NewRecordingEngine()
	engine.GPURuntime = true

	cfg := config.Config{
		Provider: "local",
		Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
		TTL:      time.Hour,
		SD:       config.SDConfig{Serve: []string{"sd-server", "--model", "/m.gguf"}},
		// Configured, with nothing wired to read what has been spent.
		MaxMonthUSD: "50",
	}
	deps := app.Deps{
		Engine: engine, HomeDir: t.TempDir(),
		Creds:     &config.Credentials{},
		DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
	}
	req := genstore.Request{
		Provider: "local", TTL: 30 * time.Minute,
		Jobs: []genstore.Job{{Prompt: "a cat", Width: 512, Height: 512, Steps: 4}},
	}

	_, err := AppPlanner{Config: cfg, Deps: deps}.Plan(t.Context(), req)
	if err == nil {
		t.Fatal("a configured period ceiling with nothing to read it against was planned through")
	}
	if !strings.Contains(err.Error(), "will not rent") {
		t.Errorf("Plan = %v, want the period ceiling's own refusal", err)
	}
	if n := len(engine.Containers()); n != 0 {
		t.Errorf("%d container(s) were created before the refusal", n)
	}

	// And with a ledger it plans, which is what says the check is a check
	// rather than a wall: the local provider is free, so its projection is
	// zero and no period can be exceeded by it.
	rental, err := AppPlanner{
		Config: cfg, Deps: deps, Ledger: genstore.NewRecordingStore(),
		Now: func() time.Time { return at(17, 12) },
	}.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("a free rental was refused by a money ceiling: %v", err)
	}
	if rental == nil {
		t.Fatal("Plan returned no rental and no error")
	}
}
