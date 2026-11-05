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
	if err := cs.Allow(t.Context(), s, at(17, 12), 8_000_000, 0); err != nil {
		t.Errorf("a rental that stays inside the ceiling was refused: %v", err)
	}

	// $12 more would be $52. Nothing has been overspent yet, which is the
	// point: the refusal happens before the money does.
	err := cs.Allow(t.Context(), s, at(17, 12), 12_000_000, 0)
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
	if err := cs.Allow(t.Context(), s, at(17, 12), 10_000_000, 0); err != nil {
		t.Errorf("a rental landing exactly on the ceiling was refused: %v", err)
	}
	if err := cs.Allow(t.Context(), s, at(17, 12), 10_000_001, 0); err == nil {
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
	err := cs.Allow(t.Context(), s, at(17, 12), 6_000_000, 0)
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
	if err := cs.Allow(t.Context(), s, at(17, 12), 4_000_000, 0); err != nil {
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

	if err := cs.Allow(t.Context(), s, at(17, 23), 2_000_000, 0); err == nil {
		t.Error("a rental over the day's remaining allowance was allowed")
	}
	// Half an hour later, in the next UTC day, the allowance is fresh. This
	// is the documented cost of matching an invoice rather than rolling.
	if err := cs.Allow(t.Context(), s, at(18, 0), 2_000_000, 0); err != nil {
		t.Errorf("the new day did not start with a fresh allowance: %v", err)
	}
	// And the month ceiling is what bounds the pair.
	withMonth := Ceilings{Day: 15_000_000, Month: 15_000_000}
	if err := withMonth.Allow(t.Context(), s, at(18, 0), 2_000_000, 0); err == nil {
		t.Error("the month ceiling did not bound a day that turned over")
	}
}

// A free rental adds nothing to any period, so a money ceiling must never
// refuse one -- that would stop the only kind of run that costs nothing.
func TestCeilings_NeverRefuseAFreeRental(t *testing.T) {
	s := genstore.NewRecordingStore()
	spentOn(t, s, at(17, 9), 999_000_000)

	cs := Ceilings{Day: 1_000, Month: 1_000}
	if err := cs.Allow(t.Context(), s, at(17, 12), 0, 0); err != nil {
		t.Errorf("a free rental was refused by a money ceiling: %v", err)
	}
}

func TestCeilings_NoCeilingReadsNothing(t *testing.T) {
	s := genstore.NewRecordingStore()
	if err := (Ceilings{}).Allow(t.Context(), s, at(17, 12), 50_000_000, 0); err != nil {
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

	err := (Ceilings{Month: 50_000_000}).Allow(t.Context(), s, at(17, 12), 1_000_000, 0)
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

// TestCeilings_BindsOnlyWhenACeilingCouldRefuse.
//
// The predicate both no-ledger refusals ask, over all four quadrants. It
// exists because #165's first form asked only `IsZero` at each of them, so a
// FREE rental with a ceiling configured and no readable ledger was refused --
// `Allow`'s own `projected <= 0` sits PAST the refusal and never gets asked.
// Hoisting the question is what makes the fix one condition rather than two
// copies of one.
func TestCeilings_BindsOnlyWhenACeilingCouldRefuse(t *testing.T) {
	paid, free := node.Rate(1_790_000), node.Rate(0)
	set, unset := Ceilings{Month: 50_000_000}, Ceilings{}

	cases := []struct {
		name  string
		cs    Ceilings
		ttl   time.Duration
		price node.Rate
		want  bool
	}{
		{"a ceiling and a paid rental", set, time.Hour, paid, true},
		{"a ceiling and a FREE rental", set, time.Hour, free, false},
		{"a ceiling and a zero ttl", set, 0, paid, false},
		{"no ceiling and a paid rental", unset, time.Hour, paid, false},
		{"no ceiling and a free rental", unset, time.Hour, free, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cs.Binds(c.ttl, c.price); got != c.want {
				t.Errorf("Binds = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRefuseOverPeriod_NeverRefusesAFreeRentalEvenWithNoLedgerToRead.
//
// The pair, because either half alone reads as working: a nil ledger under a
// configured ceiling refuses a PAID rental and must not refuse a free one.
// Asserted here as well as on the shell path -- where the arm is reachable in
// production -- because this is the arm `ember serve` has and the two share
// the predicate.
func TestRefuseOverPeriod_NeverRefusesAFreeRentalEvenWithNoLedgerToRead(t *testing.T) {
	cs := Ceilings{Month: 50_000_000}

	if err := RefuseOverPeriod(t.Context(), cs, nil, at(17, 12), time.Hour, 0, 0); err != nil {
		t.Errorf("a free rental was refused by a ceiling with no ledger to read it against: %v", err)
	}
	err := RefuseOverPeriod(t.Context(), cs, nil, at(17, 12), time.Hour, node.Rate(1_790_000), 0)
	if err == nil {
		t.Fatal("a paid rental was allowed by a ceiling with no ledger to read it against")
	}
	if !strings.Contains(err.Error(), "will not rent") {
		t.Errorf("the refusal does not say nothing was rented: %v", err)
	}
}

// TestUnreadableLedger_IsOneSentenceWithAnOptionalCause.
//
// One sentence for one money rule: it existed twice, in `RefuseOverPeriod` and
// in `cli.boundAndRecord`, worded differently -- two places for one refusal to
// drift. `why` is the cause where there is one to name and nil where there is
// not, and the cause is wrapped so `errors.Is` still reaches it.
func TestUnreadableLedger_IsOneSentenceWithAnOptionalCause(t *testing.T) {
	cs := Ceilings{Day: 20_000_000, Month: 250_000_000}

	bare := cs.UnreadableLedger(nil)
	for _, want := range []string{"$20", "$250", "will not rent"} {
		if !strings.Contains(bare.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, bare)
		}
	}

	cause := errors.New("file is not a database")
	wrapped := cs.UnreadableLedger(cause)
	if !errors.Is(wrapped, cause) {
		t.Errorf("the cause is not wrapped, so a caller cannot reach it: %v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "will not rent") {
		t.Errorf("the wrapped form lost the sentence: %v", wrapped)
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

	worked, err := q.claimAndRun(t.Context())
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
// **ITS FIRST TWO ARMS USED TO CONTRADICT EACH OTHER AND NOBODY READ THEM
// TOGETHER.** It reached the no-ledger refusal through the LOCAL provider --
// "because the local provider's offers are free and a free rental is never
// refused by a money ceiling", said in this comment -- and asserted that the
// free rental WAS refused when no ledger was wired and was planned when one
// was. A ledger cannot change a free rental's answer: `Allow` returns nil on
// `projected <= 0` whatever is in the store. So the only thing the first arm
// ever asserted is that the nil-ledger branch refused a rental the ceiling
// could not possibly refuse -- the free-rental defect, at this layer,
// UNREACHABLE in production because `cmd_serve` always wires the Ledger, and
// locked in by an assertion whose own comment names the rule it broke. #165's
// first form made the same branch reachable on the shell path, where
// `genstore.Open` really can fail, and review found it there.
//
// So the arms are three now and they discriminate: a free rental is planned
// with a ledger and without one, and a PAID rental with no ledger is refused
// -- which is what carries the join this test is named for, on the branch it
// is named for. The recording Docker engine is what proves nothing was
// created.
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

	// A FREE rental, with a ceiling configured and nothing to read it against.
	// It is planned, because its projection is zero and it adds nothing to any
	// period -- and a ceiling ember cannot check still cannot refuse a run
	// that cannot breach it.
	rental, err := AppPlanner{Config: cfg, Deps: deps}.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("a FREE rental was refused by a ceiling with no ledger to read it against: %v", err)
	}
	if rental == nil {
		t.Fatal("Plan returned no rental and no error")
	}

	// And with a ledger, which is the arm that was always right.
	rental, err = AppPlanner{
		Config: cfg, Deps: deps, Ledger: genstore.NewRecordingStore(),
		Now: func() time.Time { return at(17, 12) },
	}.Plan(t.Context(), req)
	if err != nil {
		t.Fatalf("a free rental was refused by a money ceiling: %v", err)
	}
	if rental == nil {
		t.Fatal("Plan returned no rental and no error")
	}

	// A PAID rental with no ledger IS refused, which is the join: Plan reaches
	// the no-ledger branch, and it is the price that makes the ceiling able to
	// bite. AllowPaid, or refusePaid would stop this one line earlier and the
	// assertion would be about the wrong gate.
	paid := pricedConfig()
	paid.MaxMonthUSD = "50"
	_, err = AppPlanner{Config: paid, Deps: pricedFakes(t), AllowPaid: true}.
		Plan(t.Context(), aPricedRequest())
	if err == nil {
		t.Fatal("a configured period ceiling with nothing to read it against was planned through")
	}
	if !strings.Contains(err.Error(), "will not rent") {
		t.Errorf("Plan = %v, want the period ceiling's own refusal", err)
	}
	if n := len(engine.Containers()); n != 0 {
		t.Errorf("%d container(s) were created before the refusal", n)
	}
}
