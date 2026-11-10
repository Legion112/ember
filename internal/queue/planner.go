package queue

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/logx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// AppPlanner turns a stored request into a rental, against one config.
//
// THIS IS WHERE THE CEILINGS ARE APPLIED, and it is the only place a
// request's numbers meet the operator's. `ember up`'s applyFlags *replaces*:
// typing --max-hourly-usd 999 raises the ceiling, because the person typing it
// is the person who set it. A request is not that person, so Narrow below only
// ever takes the smaller of the two -- and internal/app receives a
// provision.Budget it has no way to widen.
//
// THERE ARE NOW TWO KINDS OF CEILING HERE, and the difference is what they
// bound. max_hourly_usd and max_run_usd bound one card and one rental, and a
// request may narrow them. max_day_usd and max_month_usd bound every rental in
// a calendar period together, and a request is not consulted about them at all
// -- there is no field for either on the wire type, so the limit that stops a
// browser is one the browser cannot name. See ceiling.go.
//
// WHAT A REQUEST MAY NOT TOUCH stays on the config: the image and the images:
// map, the store and its mount, the ports, sd.serve's argv, the gpu probe, the
// assets and the verify policy. That is what keeps every existing seal
// upstream of anything a request can influence -- node.MountAt's refusal of
// /root, ValidateImage's refusal of :latest, sdserver.ServeCommand's refusal
// of a listen flag. A request body cannot reach any of them because it is
// never consulted about them.
type AppPlanner struct {
	// Config is the ceiling and everything a request may not choose. Read
	// once at startup by the caller and never from a request, or the request
	// would be naming which file sets its own limits.
	Config config.Config

	Deps app.Deps

	// AllowPaid gates every provider that is not free.
	//
	// Checked here as well as wherever the request was accepted, because
	// this is the last point before a create call: a UI bug, or a request
	// that reached the store some other way, must not be able to spend money
	// because one check was in the wrong layer.
	AllowPaid bool

	// Out carries byte streams from the node.
	Out io.Writer

	// Ledger is what the period ceilings are checked against.
	//
	// A Spender and not a Store, so what this layer can do to the database is
	// visible in its type: it decides whether to spend money, and handing it
	// a writer would mean reading the body to know it does not enqueue
	// anything. Nil means no ceiling can be checked, which is why Plan
	// refuses rather than proceeding when one is configured and this is not
	// set.
	Ledger Spender

	// Runs is where a rental this planner starts ON ITS OWN INITIATIVE is
	// recorded, and the only rental it starts is the CPU machine that fills an
	// empty store.
	//
	// A SECOND FIELD RATHER THAN WIDENING Ledger, because the two answer
	// different questions and Ledger's own argument still holds for Ledger: a
	// Spender decides whether to spend and must not be able to write. This one
	// is the write that renting forces -- see RunStore, which can record a run
	// and cannot enqueue or claim one.
	//
	// NIL IS A REFUSAL TO FILL THE STORE, not a sync that goes unrecorded.
	//
	// THE RULE IS out()'s AND NOT refuseOverPeriod's, which this comment used
	// to cite and which is the weaker of the two: that one refuses a nil
	// ledger only when a ceiling is CONFIGURED, because what it honours is an
	// operator who wrote max_month_usd. This keys on wiring rather than on
	// configuration, and the reason it can is the same as out()'s -- a nil
	// interface here is a crash. The first call recordSync makes is Adopt, so
	// the panic lands in the drain goroutine before the sync's node exists:
	// it takes `ember serve` down rather than leaving a machine running, and
	// a panic unwinds provision.WithNode's deferred destroy on the way out in
	// any case. Answering it is therefore cheap and the answer is the useful
	// one: an unrecorded rental is precisely the defect this field exists to
	// fix, so a planner that cannot record what it is about to rent does not
	// rent it. `ember serve` always wires it, so nothing an operator can type
	// reaches this -- it guards the shape.
	Runs RunStore

	// Fills serialises the store fills this process starts, one store at a
	// time, so two slots cannot both rent a machine to fetch the same
	// weights.
	//
	// Nil serialises nothing, which is what max_nodes: 1 needs: the plans are
	// serial there and the second one finds the store full. See storefill.go.
	Fills *StoreFills

	// Commit is what this process has agreed to spend and not yet recorded,
	// shared with the drain loop and with DirectRunner.
	//
	// A FIELD HERE BECAUSE THIS IS WHERE THE PROJECTION IS KNOWN: it needs the
	// offer, and the offer is chosen in Plan. Nil is today's arithmetic -- see
	// commit.go -- which is what keeps every existing literal in the suite
	// measuring what production measures at max_nodes: 1.
	Commit *Commitments

	// Now is injected because a ceiling is a question about a calendar
	// period, and a test that could not choose the day could not ask it.
	Now func() time.Time

	// Log is the level and the format the operator chose on `ember serve`.
	//
	// IT IS NOT A CEILING AND IT IS NOT FROM THE REQUEST, which is why it
	// sits here beside the config rather than on genstore.Request: a browser
	// has no business deciding how verbosely the process it is talking to
	// writes to the operator's terminal. It is carried through only so that
	// a rental's own lines -- which come from internal/provision, inside
	// app.Plan.Do -- obey the same flag as the server's.
	Log logx.Options
}

// out is the byte stream, never nil.
//
// `ember serve` always sets one, so this is not about the ordinary case: it is
// that the one writer here is reached from inside the drain goroutine, where a
// nil would panic with a node running -- and the panic would unwind past
// provision.WithNode's deferred destroy. A leaked rental is the one failure
// this package exists to prevent, so the nil is answered rather than
// documented. The same reason `now` is a method and not a field read.
func (p AppPlanner) out() io.Writer {
	if p.Out == nil {
		return io.Discard
	}
	return p.Out
}

func (p AppPlanner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Plan resolves a request, and rents nothing EXCEPT a machine with no card
// when the store this run needs is missing its weights.
//
// This sentence used to read "and rents nothing", which stopped being true the
// day fillStore landed below and is the shape of comment this repository keeps
// finding: an argument that persuades about a mechanism nobody re-read. The
// exception matters to a reader because it is the one place a plan can cost
// money before any node the request asked for exists -- and it is why this
// type holds a Runs as well as a Ledger.
func (p AppPlanner) Plan(ctx context.Context, r genstore.Request) (Rental, error) {
	cfg, budget, err := Narrow(p.Config, r)
	if err != nil {
		return nil, err
	}

	jobs := make([]sdserver.Job, 0, len(r.Jobs))
	for _, j := range r.Jobs {
		jobs = append(jobs, j.SD())
	}

	req := app.Request{
		Config:          cfg,
		Deps:            p.Deps,
		Jobs:            jobs,
		Preview:         r.Preview,
		PreviewInterval: r.PreviewInterval,
		Log:             p.Log,
		Budget:          &budget,
	}

	plan, err := app.NewPlan(ctx, req, p.out())
	if missing, ok := app.IsAssetsMissing(err); ok {
		// THE WHOLE SAVING IS HERE, and it is a second rental rather than a
		// flag. A card whose weights are not on the store spends its first
		// minutes downloading them at the card's rate: measured on
		// 2026-11-03, 14m47s of an RTX PRO 6000 at $2.49/hr, 97.9% of the
		// rental, GPU at 0% throughout. `ember store sync` already rents a
		// machine with no card for exactly this work, at $0.06/hr, and
		// nothing on the rental path had ever called it.
		//
		// ONLY FROM THE QUEUE. `ember up` refuses instead, because an
		// interactive command must not go and rent a second machine the
		// operator did not ask for. A queue drains unattended, which is the
		// case this is worth money in and the case nobody is watching to
		// type the command.
		//
		// AND ONE STORE IS FILLED BY ONE SLOT AT A TIME (#204). At
		// max_nodes>1 two plans run at once, so both can be told the weights
		// are missing and both can go and fetch the same fifty-six
		// gigabytes -- a thing the one-rental loop made unrepresentable
		// because the two plans were serial. See storefill.go for what the
		// duplicate costs and why the lock is context-aware rather than a
		// mutex.
		if ferr := p.Fills.Fill(ctx, missing.Store, func() error {
			return p.fillLocked(ctx, cfg, req, missing, &plan, &err)
		}); ferr != nil {
			return nil, ferr
		}
	}
	if err != nil {
		return nil, err
	}

	// After the offer is chosen, because the refusal should name the price it
	// refused -- "runpod is not free" is less useful than the rate an
	// operator was about to be charged.
	if err := refusePaid(p.AllowPaid, plan.Provider, plan.Offer); err != nil {
		return nil, err
	}

	// And the period ceilings, here for the same reason and in the same
	// place: this is the last statement before anything is created, and the
	// projection needs the offer's own price. A refusal here costs the offer
	// lookup and not a node of its own -- although a store fill above may
	// already have rented and recorded one, which fillStore projects against
	// these same ceilings before it does.
	//
	// AND IT IS THE ADMITTING FORM, which is the last statement before this
	// function hands back something that can rent: the check and the record
	// happen under one lock, so a second slot planning concurrently cannot
	// also see a half-spent day. Nothing stands between this and the return,
	// so a commitment is never recorded for a plan that then failed; the slot
	// that runs the rental releases it in a defer. See commit.go.
	if err := p.admitOverPeriod(ctx, r.ID, cfg.TTL, plan.Offer.Price); err != nil {
		return nil, err
	}

	return appRental{plan: plan, out: p.out()}, nil
}

// fillLocked asks again, fills if it still has to, and asks once more.
//
// THE FIRST RE-ASK IS WHAT max_nodes ADDED, and it is the whole point of
// holding the store's fill while this runs: another slot may have been
// fetching these exact weights while this one waited, so the first thing to do
// with the lock is ask the store's own endpoint rather than act on an answer
// that is now minutes old. That is "Asked again rather than assumed" moved one
// step earlier -- and it is why nothing here records what this process has
// filled: the store is the authority, a flag would be a second one, and a
// second one would be wrong the first time two requests on one store wanted
// different assets.
//
// IT COSTS ONE CATALOGUE READ AND A HEAD PER ASSET ON A PATH THAT IS ABOUT TO
// RENT FOR MINUTES, which is why the re-ask is unconditional rather than
// guarded on whether anybody waited: a condition on "did I block" is a
// condition somebody has to keep true, and the saving it buys is one HTTP
// round trip in the case where a store is empty, which happens once per store.
//
// `plan` and `err` ARE WRITTEN THROUGH POINTERS because this runs inside a
// closure that Fill calls, and what the caller needs back is the plan the
// second ask produced. A return value would have meant Fill knowing what a
// plan is.
func (p AppPlanner) fillLocked(
	ctx context.Context,
	cfg config.Config,
	req app.Request,
	missing *app.AssetsMissingError,
	plan *app.Plan,
	err *error,
) error {
	if _, ok := app.IsAssetsMissing(*err); !ok {
		// Cannot happen -- the caller only enters here with a missing-assets
		// error -- and is answered rather than asserted, because the
		// alternative is a nil dereference inside a closure on a path that
		// rents.
		return nil
	}
	*plan, *err = app.NewPlan(ctx, req, p.out())
	if second, ok := app.IsAssetsMissing(*err); ok {
		// Still missing with the fill held, so this slot is the one that
		// fills it -- and it fills what the RE-ASK found rather than what the
		// caller's own plan did, which is the point of asking again: another
		// slot may have put some of these assets there while this one waited.
		if ferr := p.fillStore(ctx, cfg, second); ferr != nil {
			return ferr
		}
		// Asked again rather than assumed. The sync verified every digest on
		// the node, so this is not in doubt about the bytes -- what it
		// confirms is that the store's own endpoint now lists them, which is
		// the thing this planner acted on and the only claim it has made. A
		// second miss is reported as what it is rather than fallen through,
		// because falling through is the rental this exists to prevent.
		*plan, *err = app.NewPlan(ctx, req, p.out())
		if third, ok := app.IsAssetsMissing(*err); ok {
			return fmt.Errorf("filled the store and %d asset(s) are still not listed on it (%w). "+
				"The node verified their digests, so the bytes are there and the store's endpoint does not show them yet; "+
				"nothing has been rented", len(third.Missing), *err)
		}
		return nil
	}
	// ANOTHER SLOT FILLED IT WHILE THIS ONE WAITED, which is the saving: no
	// second machine, no second fetch of the same weights, and `missing` is
	// named in the line so an operator reading two requests on one store can
	// see why only one of them rented a downloader.
	_, _ = fmt.Fprintf(p.out(), "the store %q was filled by another rental while this request "+
		"waited; nothing more to fetch\n", missing.Store)
	return nil
}

// fillStore rents a machine with no card and has it fetch this run's weights.
//
// IT IS A RENTAL AND IS BOUNDED LIKE ONE. --allow-paid gates it and the
// calendar ceilings are projected against it, by the same two calls Plan makes
// for the rental it is standing in front of -- a cheap machine is still money,
// and a queue that could spend outside the ceilings by calling a different
// function would be a hole in the one gate `ember serve` has.
//
// THE FILTER IS CPUOnly AND THAT IS EXCLUSIVE IN BOTH DIRECTIONS, so this
// cannot quietly land on a card and reproduce the defect it exists to remove.
// It is also what stops this recursing: a CPU-only plan is not asked whether
// the store is full, because filling it is the job.
//
// NO FALLBACK. A sync that fails returns its error and the request fails with
// it. Carrying on would rent the GPU and download on it, which is the $0.61
// this was written for, arrived at quietly.
//
// AND IT GETS A LEDGER ROW OF ITS OWN, which is the money half and was missing
// from the first version of this function. See recordSync.
func (p AppPlanner) fillStore(ctx context.Context, cfg config.Config, missing *app.AssetsMissingError) error {
	// FIRST, AND BEFORE THE PLAN, because it is free and because a rental
	// ember cannot account for is the thing this whole path is bounded by. The
	// refusals below each cost a catalogue read; this one costs nothing, and
	// what it answers is out()'s question rather than refuseOverPeriod's --
	// see the field, where the difference is written down: a nil here is a
	// crash in the drain goroutine, and the answer worth giving it is that an
	// unattended process which cannot write down what it spent must not
	// spend.
	if p.Runs == nil {
		return fmt.Errorf("the store %q is missing %d asset(s) and nothing here can record what a "+
			"machine rented to fetch them would cost, so none is rented: ember serve wires the "+
			"generation history, and a rental missing from `ember history` and `ember spend` is "+
			"money an operator cannot account for", missing.Store, len(missing.Missing))
	}

	names := make([]string, 0, len(missing.Missing))
	for _, a := range missing.Missing {
		names = append(names, a.As)
	}
	_, _ = fmt.Fprintf(p.out(), "the store %q is missing %d asset(s): %s\n"+
		"renting a machine with no card to fetch them, so the card is not paid to download\n",
		missing.Store, len(names), strings.Join(names, ", "))

	// No Budget override. The request's narrowed ceiling bounds the request's
	// own rental; this one is the config's, which is the operator's.
	sync, err := app.NewPlan(ctx, app.Request{
		Config:  cfg,
		Deps:    p.Deps,
		CPUOnly: true,
		Log:     p.Log,
	}, p.out())
	if err != nil {
		return fmt.Errorf("plan a machine to fill the store: %w", err)
	}
	if err := refusePaid(p.AllowPaid, sync.Provider, sync.Offer); err != nil {
		return err
	}
	if err := p.refuseOverPeriod(ctx, cfg.TTL, sync.Offer.Price); err != nil {
		return err
	}
	if err := p.recordSync(ctx, sync, missing.Store); err != nil {
		return fmt.Errorf("fill the store %q: %w", missing.Store, err)
	}
	return nil
}

// recordSync runs the store fill with its own row in the generation history.
//
// A ROW OF ITS OWN, AND NOT A COST ON THE REQUEST THAT TRIGGERED IT. This is
// the design call #148 was filed for, and the two alternatives are both wrong
// in ways this repository has already paid for.
//
// Charging it to the triggering request is the cheap answer: one more figure
// folded into `requests.cost`. But that column is documented, read and summed
// as ONE rental's cost -- "a rental is counted once, on the request that
// opened it" -- so a column holding two rentals makes `ember spend`'s
// cost-per-image divide a bill nobody can decompose, and it is wrong for every
// request after the first, which gets the store for free. A figure that looks
// plausible and cannot be checked against a second copy is exactly the shape
// of wrong the subquery in Spend exists to avoid.
//
// A `rentals` table is the thorough answer and buys nothing today: every
// rental ember has ever recorded is already a request row, `Entry.Rode`
// already says which rows carry a rental's cost, and a second thing for
// `ember spend` to sum is a second place for the total to be wrong. It would
// also be a migration, a fake method and conformance rows for a shape with one
// reader.
//
// So the sync is what it is: a rental nobody queued, which is precisely what
// genstore.Adopt and AdoptRun already exist for -- the same row `ember up`
// writes, with no jobs, which `ember history` already prints with a dash for
// images and card time and counts in the footnote that says how many rows it
// left figures out of. No new column, no migration, and the readers needed no
// teaching.
//
// WHAT IS NOT RECORDED is which request caused it: there is no column for a
// parent and inventing one would be a migration for a field nothing reads.
// What an operator sees is two rentals, which is what happened.
//
// THE ROW GOES DOWN BEFORE THE CREATE CALL, which is why Adopt is here and not
// after SyncAssets returns: the window between asking a provider for a machine
// and reading the answer is where a rental nothing recorded is born, so a sync
// whose create failed and a sync whose node billed without ever becoming ready
// both have a row. finish is a statement rather than a defer for AdoptRun's
// callers' reason: provision's destroy is deferred INSIDE SyncAssets, so by the
// time it returns the run_cost event has been emitted and written through.
//
// AND AN ADOPT THAT FAILS STOPS THE RENTAL, which is the opposite of
// `recordRental`'s "it cannot fail the run" and is deliberate rather than an
// oversight. That rule is for a command the operator typed: refusing to rent
// because ember could not take notes would be ember declining to do the thing
// it is for. This rental nobody asked for, it happens with nobody watching,
// and its whole justification is that a queue drains unattended -- so the
// request fails with the reason rather than quietly paying for a machine no
// ledger will remember. It is also before the create call, so nothing has
// been rented when it fires, which is what makes refusing cheap enough to be
// the obvious answer rather than a trade.
func (p AppPlanner) recordSync(ctx context.Context, sync app.Plan, store string) error {
	// logx.New over the same two inputs `cmd_serve` builds the queue's logger
	// from, rather than a third copy carried on this struct: Out is that
	// terminal and Log is the flag the operator chose, so this is the same
	// logger the drain loop has. It is only ever used to say that a write to
	// the history failed, which must not be silent on the one rental nobody is
	// watching.
	log := logx.New(p.out(), p.Log)

	id, sink, finish, err := AdoptRun(ctx, p.Runs, sync, log, p.now)
	if err != nil {
		return fmt.Errorf("record the machine rented to fill it: %w", err)
	}
	log.Info("renting a machine with no card to fill a store, recorded as its own run",
		"request", id, "store", store, "offer", sync.Offer.Token, "price", sync.Offer.Price.String())

	runErr := sync.SyncAssets(ctx, p.out(), sink)
	finish(runErr)
	return runErr
}

// refuseOverPeriod is this planner's half of the max_day_usd and
// max_month_usd gate: its config, its ledger, its clock and what this process
// has committed.
//
// THE RULE ITSELF IS RefuseOverPeriod's, and it is a package function rather
// than a method here because three other rental paths need the same one --
// `ember up` in a shell, `ember up` over the socket and `ember store sync`.
// This method serves two of the five: Plan's own rental, and the store fill
// above. A copy per caller is a copy of a money rule, and the one thing this
// repository records about those is that the copies drift.
//
// IT READS THE COMMITTED TOTAL AND RECORDS NOTHING, which is the store fill's
// half of commit.go's split. See admitOverPeriod for the other.
func (p AppPlanner) refuseOverPeriod(ctx context.Context, ttl time.Duration, price node.Rate) error {
	cs, err := CeilingsOf(p.Config)
	if err != nil {
		return err
	}
	return RefuseOverPeriod(ctx, cs, p.Ledger, p.now(), ttl, price, p.Commit.Committed())
}

// admitOverPeriod is the same gate for the rental this plan is FOR, and it
// records what that rental will cost before another create can read the
// ledger.
//
// TWO METHODS AND NOT A FLAG, because the two callers want different things
// from one rule and the difference is money. The rental below is the one a
// concurrent create must be able to see, so it is admitted under this
// request's id and released by the slot that holds it; the store fill above is
// deliberately NOT a commitment -- it is a $0.06/hour machine inside this same
// Plan call, and committing it would mean nesting an Admit inside an Admit or
// holding two entries under one request id. Both of those, and what the
// residue costs, are written at the top of commit.go.
//
// THE PROJECTION IS DERIVED FROM THE PRICE AND THE TTL HERE AS WELL AS INSIDE
// `Binds`, and that is one expression repeated rather than two rules: #157
// measured a literal zero in either half as a gate that cannot fail, so there
// is nowhere a caller could pass the answer instead of the two facts.
func (p AppPlanner) admitOverPeriod(
	ctx context.Context,
	id genstore.ReqID,
	ttl time.Duration,
	price node.Rate,
) error {
	cs, err := CeilingsOf(p.Config)
	if err != nil {
		return err
	}
	return p.Commit.Admit(id, price.Cost(ttl), func(committed node.MicroUSD) error {
		return RefuseOverPeriod(ctx, cs, p.Ledger, p.now(), ttl, price, committed)
	})
}

// refusePaid is the --allow-paid gate.
//
// CHECKED HERE as well as wherever the request was accepted, and the
// duplication is the point: this is the last statement before a create call.
// A UI bug, or a request that reached the store some other way, must not be
// able to spend money because the only check was in a layer it went around.
// "Enforced twice, by two different mechanisms" is the rule internal/transfer
// already follows for tar members; this is the money version of it.
//
// It reads Caps().Free rather than a list of provider names, so a provider
// added later is paid by default. The zero value of that field is false, which
// means "not free" -- so an author who forgets it gets the refusal rather than
// a bill.
func refusePaid(allowPaid bool, p provider.Provider, o node.Offer) error {
	if allowPaid || p.Caps().Free {
		return nil
	}
	return fmt.Errorf("provider %s is not free and ember serve was started without --allow-paid, "+
		"so it will not rent anything: %s at %v was selected. Restart ember serve with "+
		"--allow-paid to let the web UI spend money", p.Name(), o.Token, o.Price)
}

// Narrow applies a request's own limits to the config's, never the other way.
//
// Each limit is the smaller of the two, and the reason is the same for all of
// them: a request arrives from somewhere the operator is not, so it may ask
// for less than they allowed and never for more. A request asking for more
// gets the config's number rather than a refusal, because refusing would make
// a UI that offers a slider also have to know every ceiling before it can
// offer anything -- and the honest behaviour is that the ceiling is the
// ceiling.
//
// THE BUDGET COMES BACK AS INTEGERS and is not written into the returned
// config. config.Config holds budgets as decimal strings, so putting a
// narrowed value back would mean rendering microdollars -- and
// node.MicroUSD.String is "$%.6g", which is scientific notation for small
// values: one microdollar is "$1e-06", which node.ParseUSD refuses. There is
// no round trip, so there is nothing to lose in one.
func Narrow(cfg config.Config, r genstore.Request) (config.Config, provision.Budget, error) {
	if r.MaxHourly < 0 || r.MaxRun < 0 {
		return config.Config{}, provision.Budget{},
			fmt.Errorf("a request asked for a negative budget (%v/hr, %v); nothing can satisfy it",
				r.MaxHourly, r.MaxRun)
	}

	// The selection. These are not ceilings -- there is nothing to compare
	// them against -- so a request simply chooses, and the provider either
	// has the card or reports no capacity.
	if r.Provider != "" {
		cfg.Provider = r.Provider
	}
	cfg.GPU = r.GPU

	if r.TTL > 0 && (cfg.TTL <= 0 || r.TTL < cfg.TTL) {
		cfg.TTL = r.TTL
	}

	hourly, run, err := cfg.Budgets()
	if err != nil {
		// The operator's own file, so this is their error and not the
		// request's -- and it is found before anything is rented.
		return config.Config{}, provision.Budget{}, fmt.Errorf("the configured budget: %w", err)
	}
	return cfg, provision.Budget{
		MaxRate: smaller(hourly, r.MaxHourly),
		MaxRun:  smallerUSD(run, r.MaxRun),
	}, nil
}

// smaller is the tighter of two rates, where zero means "no ceiling".
//
// Zero has to mean unlimited on both sides or the arithmetic goes the wrong
// way: a request that named no budget would otherwise narrow the config's to
// nothing and refuse every offer.
func smaller(a, b node.Rate) node.Rate {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case b < a:
		return b
	}
	return a
}

func smallerUSD(a, b node.MicroUSD) node.MicroUSD {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case b < a:
		return b
	}
	return a
}

// appRental is an app.Plan that has not been rented.
type appRental struct {
	plan app.Plan
	out  io.Writer
}

func (a appRental) WithServed(ctx context.Context, sink report.Sink, fn func(context.Context, app.Served) error) error {
	return a.plan.WithServed(ctx, a.out, sink, fn)
}

func (a appRental) WithSpoken(ctx context.Context, sink report.Sink, fn func(context.Context, app.Spoken) error) error {
	return a.plan.WithSpoken(ctx, a.out, sink, fn)
}

// Card is the card this plan's offer carries.
//
// THROUGH HasGPU, so a CPU-only plan answers "" rather than whatever a
// provider happened to put in a field it does not fill. The same reading
// RunGPU already uses for the stored column, which is what keeps the card a
// rental advertises and the card its row records one fact.
func (a appRental) Card() node.GPUModel {
	if !a.plan.Offer.HasGPU() {
		return ""
	}
	return a.plan.Offer.GPU
}

// Image is the workload image this plan will run, off the resolved spec.
//
// FROM THE SPEC AND NOT FROM THE CONFIG FILE, which is the same reading every
// other fact about a rental uses: NewPlan has already applied whatever this
// request's kind selects, so this is the image that will actually be pulled
// rather than the one `images:` lists first.
func (a appRental) Image() string { return a.plan.Spec.Image }
