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

// Plan resolves a request and rents nothing.
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
		if err := p.fillStore(ctx, cfg, missing); err != nil {
			return nil, err
		}
		// Asked again rather than assumed. The sync verified every digest on
		// the node, so this is not in doubt about the bytes -- what it
		// confirms is that the store's own endpoint now lists them, which is
		// the thing this planner acted on and the only claim it has made. A
		// second miss is reported as what it is rather than fallen through,
		// because falling through is the rental this exists to prevent.
		plan, err = app.NewPlan(ctx, req, p.out())
		if second, ok := app.IsAssetsMissing(err); ok {
			return nil, fmt.Errorf("filled the store and %d asset(s) are still not listed on it (%w). "+
				"The node verified their digests, so the bytes are there and the store's endpoint does not show them yet; "+
				"nothing has been rented", len(second.Missing), err)
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
	// projection needs the offer's own price. Plan rents nothing, so a
	// refusal here costs the offer lookup and not a node.
	if err := p.refuseOverPeriod(ctx, cfg.TTL, plan.Offer.Price); err != nil {
		return nil, err
	}

	return appRental{plan: plan, out: p.out()}, nil
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
func (p AppPlanner) fillStore(ctx context.Context, cfg config.Config, missing *app.AssetsMissingError) error {
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
	if err := sync.SyncAssets(ctx, p.out()); err != nil {
		return fmt.Errorf("fill the store %q: %w", missing.Store, err)
	}
	return nil
}

// refuseOverPeriod is the max_day_usd and max_month_usd gate.
//
// The projection is the offer's price over the TTL, which is exactly what
// provision.Budget.Allow compares max_run_usd against -- so the figure a
// period is charged and the figure one rental is bounded by are the same
// arithmetic, and the two ceilings cannot come to disagree about what a rental
// may come to.
//
// A ceiling with no ledger to read is a REFUSAL and not a pass. An operator
// who wrote max_month_usd into their config has said that unattended spending
// must stop somewhere, and the one answer a spend ceiling must never give is
// "probably fine" -- the same rule that makes `ember ls` exit non-zero
// over a provider it could not ask. It cannot happen through `ember serve`,
// which always wires the store; it is here because a ceiling that silently
// does nothing is worse than one that is not configured.
func (p AppPlanner) refuseOverPeriod(ctx context.Context, ttl time.Duration, price node.Rate) error {
	day, month, err := p.Config.PeriodCeilings()
	if err != nil {
		// The operator's own file, as Narrow says of the budgets: their
		// error, and found before anything is rented.
		return fmt.Errorf("the configured period ceiling: %w", err)
	}
	cs := Ceilings{Day: day, Month: month}
	if cs.IsZero() {
		return nil
	}
	if p.Ledger == nil {
		return fmt.Errorf("this configuration sets a period ceiling (%v a day, %v a month) and "+
			"nothing here can read what has been spent, so ember cannot tell whether this rental "+
			"is within it and will not rent", day, month)
	}
	return cs.Allow(ctx, p.Ledger, p.now(), price.Cost(ttl))
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
