package queue

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
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

	plan, err := app.NewPlan(ctx, app.Request{
		Config:          cfg,
		Deps:            p.Deps,
		Jobs:            jobs,
		Preview:         r.Preview,
		PreviewInterval: r.PreviewInterval,
		Budget:          &budget,
	}, p.Out)
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

	return appRental{plan: plan, out: p.Out}, nil
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
// "probably fine" -- the same rule that makes `ember ls --all` exit non-zero
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
