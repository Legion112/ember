// Package provision turns a spec into a running node and takes it away again.
//
// Everything in here exists because a node that outlives the run costs money
// by the hour in silence. The ordering is the design: the intent record is
// written before the create call, destroy runs on a context that survives
// cancellation, and the only ordinary way to get a node is a closure whose
// exit destroys it.
package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/statestore"
)

// Budget is the ceiling a run may not cross.
type Budget struct {
	MaxRate node.Rate     // per hour, whole offer
	MaxRun  node.MicroUSD // for the whole TTL
}

// Allow reports whether renting o for ttl is within budget.
//
// Integer money throughout, so "exactly at the ceiling" is a decidable
// question rather than a floating-point coin toss.
func (b Budget) Allow(o node.Offer, ttl time.Duration) error {
	if b.MaxRate > 0 && o.Price > b.MaxRate {
		return fmt.Errorf("%w: %s costs %v, ceiling is %v", node.ErrBudget, o.Token, o.Price, b.MaxRate)
	}
	if b.MaxRun > 0 {
		cost := o.Price.Cost(ttl)
		if cost > b.MaxRun {
			return fmt.Errorf("%w: %s for %v would cost %v, ceiling is %v", node.ErrBudget, o.Token, ttl, cost, b.MaxRun)
		}
	}
	return nil
}

// Options are the collaborators and knobs for one run.
type Options struct {
	Store    *statestore.Store
	Pins     *hostkey.Store
	Identity keys.Identity
	Budget   Budget
	Log      *slog.Logger

	// ReadyTimeout bounds how long a node may take to become usable.
	ReadyTimeout time.Duration

	// PollEvery is the gap between Inspect calls.
	PollEvery time.Duration

	// DestroyTimeout bounds the teardown that runs after the context is
	// already cancelled.
	DestroyTimeout time.Duration

	// Now is injected only where a timestamp is recorded or compared, never
	// where something is waited on: Go 1.27's testing/synctest makes the
	// waiting testable without a clock interface, so one does not exist here.
	Now func() time.Time
}

const (
	defaultReadyTimeout   = 10 * time.Minute
	defaultPollEvery      = 2 * time.Second
	defaultDestroyTimeout = 2 * time.Minute
)

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (o Options) readyTimeout() time.Duration {
	if o.ReadyTimeout > 0 {
		return o.ReadyTimeout
	}
	return defaultReadyTimeout
}

func (o Options) pollEvery() time.Duration {
	if o.PollEvery > 0 {
		return o.PollEvery
	}
	return defaultPollEvery
}

func (o Options) destroyTimeout() time.Duration {
	if o.DestroyTimeout > 0 {
		return o.DestroyTimeout
	}
	return defaultDestroyTimeout
}

// Session is what a run gets once its node is usable.
type Session struct {
	Node  node.ReadyNode
	Trust hostkey.Trust
	RunID node.RunID
}

// WithNode rents a node, runs fn against it, and destroys it.
//
// This is the only ordinary way to obtain a node, and the reason is the defer
// below: the node's lifetime is the closure's, so there is no path through
// this function that returns while a machine is still running.
//
// The offer is passed in rather than chosen here, and that is load-bearing.
// spec.Image is selected from the architecture of the card in the offer, so if
// this function listed offers again it would be choosing a *second* time --
// two listings can give two answers, and the image would then have been picked
// for a card that is not the one rented. Which offer to rent is a decision
// made in exactly one place in the program, for the same reason the deadline
// is derived in exactly one place: three copies can disagree, and the one that
// disagrees is the one that does not stop the bill.
func WithNode(
	ctx context.Context,
	p provider.Provider,
	spec node.Spec,
	offer node.Offer,
	o Options,
	fn func(context.Context, Session) error,
) (err error) {
	if o.Store == nil {
		return errors.New("provision needs a state store: a node nobody recorded is a node nobody can reap")
	}

	pair, err := hostkey.Mint()
	if err != nil {
		return err
	}

	runID := node.NewRunID()
	req, err := node.NewCreateRequest(spec, offer, runID, o.now(), o.Identity.AuthorizedKey, pair.Private, pair.NodeKey())
	if err != nil {
		return err
	}

	// The intent record goes down before the create call. The window between
	// asking for a machine and learning its id is where an orphan with no
	// local trace is born, and a crash there otherwise leaves a GPU billing
	// hourly with nothing on this machine to find it by.
	rec := statestore.Record{
		RunID:    runID,
		Provider: p.Name(),
		Phase:    statestore.PhaseIntent,
		Label:    provider.Label(runID, req.Deadline),
		Created:  o.now(),
		Deadline: req.Deadline,
		Offer:    string(offer.Token),
		Price:    offer.Price,
		// Recorded with the intent, before the create call, so a node whose
		// creator is later killed can be told apart from one somebody is
		// still using.
		Holder: statestore.ThisProcess(),
	}
	if err := o.Store.Create(rec); err != nil {
		return err
	}

	h, createErr := p.Create(ctx, req)
	if createErr != nil {
		// The record stays. An intent with no node id is exactly what
		// `ember ls --all` is meant to go and ask the provider about, because
		// "the call failed" does not mean "nothing was created".
		_ = o.Store.Update(runID, func(r *statestore.Record) {
			r.LastErr = createErr.Error()
		})
		return fmt.Errorf("create node: %w", createErr)
	}

	if err := o.Store.Update(runID, func(r *statestore.Record) {
		r.NodeID = h.ID
		r.Phase = statestore.PhaseCreated
	}); err != nil {
		o.log().Error("could not record the node id; destroying it rather than losing track", "handle", h.String(), "err", err)
	}

	defer func() {
		// WithoutCancel is the whole point. Ctrl-C cancels ctx, and a destroy
		// call on a cancelled context fails instantly -- which is the single
		// most likely way to leak a GPU.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.destroyTimeout())
		defer cancel()
		if derr := destroy(dctx, p, o, runID, h); derr != nil && err == nil {
			err = derr
		}
	}()

	ready, err := WaitReady(ctx, p, h, o)
	if err != nil {
		return err
	}

	// Checked before anything connects or runs. A node that is exposed more
	// widely than ember asked for is already wrong, and the window being
	// closed here is the one between "the provider created it" and "the
	// operator reads a warning in their terminal".
	//
	// WithNode's deferred destroy handles the teardown, so returning is
	// enough to make the node go away.
	if err := assertExposure(p, ready.Node()); err != nil {
		return err
	}

	trust, err := trustFor(ready, o, req.HostKeyPublic)
	if err != nil {
		return err
	}

	if err := o.Store.Update(runID, func(r *statestore.Record) { r.Phase = statestore.PhaseReady }); err != nil {
		o.log().Warn("could not record readiness", "err", err)
	}

	o.log().Info("node ready",
		"handle", h.String(), "endpoint", ready.Endpoint().String(),
		"deadline", req.Deadline.Format(time.RFC3339), "price", offer.Price.String())

	// The workload may not outlive the node's deadline. A run that keeps
	// going past it would be using a machine the reaper is entitled to
	// destroy underneath it.
	runCtx, cancel := context.WithDeadline(ctx, req.Deadline)
	defer cancel()

	return fn(runCtx, Session{Node: ready, Trust: trust, RunID: runID})
}

// assertExposure applies the assertion the provider's capabilities call for.
//
// The choice lives here rather than in each provider because a provider
// checking its own exposure is a provider grading its own homework: the point
// of the check is to catch the provider doing something other than what it
// said it would.
func assertExposure(p provider.Provider, n node.Node) error {
	if p.Caps().PublicSSH {
		return exposure.AssertSSHOnly(n, p.Caps().PlatformPorts...)
	}
	return exposure.AssertNoPublishedPorts(n)
}

// PickOffer chooses what to rent.
//
// A Filter and a TTL rather than the whole Spec, which is the only two fields
// it ever read. The narrowing is the point: the image is now chosen from the
// offer this returns, so a PickOffer that could see spec.Image would close a
// loop between the two -- and the compiler is what keeps it open.
func PickOffer(
	ctx context.Context,
	p provider.Provider,
	f node.Filter,
	ttl time.Duration,
	o Options,
) (node.Offer, error) {
	offers, err := p.Offers(ctx, f)
	if err != nil {
		return node.Offer{}, fmt.Errorf("list offers from %s: %w", p.Name(), err)
	}
	// Re-filtered our side regardless of what the API was asked: see
	// provider.FilterOffers for why that is a cost control.
	offers = provider.FilterOffers(f, offers)
	if len(offers) == 0 {
		return node.Offer{}, fmt.Errorf("%w: %s has nothing matching the filter", node.ErrNoCapacity, p.Name())
	}

	free := p.Caps().Free
	var budgetErr error
	for _, candidate := range offers {
		if !free {
			if err := o.Budget.Allow(candidate, ttl); err != nil {
				if budgetErr == nil {
					budgetErr = err
				}
				continue
			}
		}
		return candidate, nil
	}
	if budgetErr != nil {
		return node.Offer{}, budgetErr
	}
	return node.Offer{}, fmt.Errorf("%w: no offer from %s was usable", node.ErrNoCapacity, p.Name())
}

// WaitReady polls until the node is usable, or gives up.
//
// Written against real time and tested inside testing/synctest, so a
// ten-minute timeout is exercised instantly and no Clock interface exists to
// be threaded through everything.
func WaitReady(ctx context.Context, p provider.Provider, h node.Handle, o Options) (node.ReadyNode, error) {
	ctx, cancel := context.WithTimeout(ctx, o.readyTimeout())
	defer cancel()

	for {
		n, err := p.Inspect(ctx, h)
		if err != nil {
			return node.ReadyNode{}, fmt.Errorf("inspect node %s: %w", h, err)
		}

		switch {
		case n.State == node.StateReady:
			ready, err := node.Ready(n)
			if err == nil {
				return ready, nil
			}
			// Ready-but-unusable is a normal transient: the provider says
			// running before it has published a port. Keep polling.
			o.log().Debug("node running but not yet reachable", "handle", h.String(), "err", err)

		case n.State == node.StateInterrupted:
			return node.ReadyNode{}, fmt.Errorf("node %s: %w", h, node.ErrInterrupted)

		case n.State.Terminal():
			return node.ReadyNode{}, fmt.Errorf("node %s became %s before it was usable: %s", h, n.State, n.Message)
		}

		select {
		case <-ctx.Done():
			return node.ReadyNode{}, fmt.Errorf("node %s was not usable within %v: %w", h, o.readyTimeout(), ctx.Err())
		case <-time.After(o.pollEvery()):
		}
	}
}

// trustFor decides how the node's host key will be judged.
// trustFor decides what the dial must verify the node against.
//
// minted is the public half of the key ember generated and injected through
// the create call. It takes precedence over anything the provider reports,
// and that ordering is the whole point: ember already knows which key the
// node should present, so it never has to ask the provider to tell it.
//
// Reading the provider's answer first was a real bug. The local provider
// echoes the key back on a container label, so it worked there; RunPod has no
// label mechanism and reports nothing, so ember's first real cloud provision
// fell through to trust-on-first-use -- against a brand-new IP, which is the
// one case TOFU cannot defend and the exact thing minting the key exists to
// avoid. It warned, and a warning in a log is not a security control.
func trustFor(r node.ReadyNode, o Options, minted node.HostKey) (hostkey.Trust, error) {
	reported := r.Node().HostKey

	if !minted.IsZero() {
		fp, err := hostkey.FromNodeKey(minted)
		if err != nil {
			return nil, fmt.Errorf("node %s: the host key ember minted: %w", r.Handle(), err)
		}
		// If the provider also reports one, the two must agree. A provider
		// handing back a different key than the one ember supplied has
		// substituted something, and continuing would pin the substitution.
		if !reported.IsZero() {
			rfp, err := hostkey.FromNodeKey(reported)
			if err != nil {
				return nil, fmt.Errorf("node %s: the host key the provider reported: %w", r.Handle(), err)
			}
			if !fp.Equal(rfp) {
				return nil, fmt.Errorf(
					"node %s reports host key %s but ember injected %s; the provider has substituted a key, so ember will not connect",
					r.Handle(), rfp, fp)
			}
		}
		return hostkey.Pinned(fp)
	}

	// Unreachable through WithNode, which mints a key for every node and
	// cannot build a CreateRequest without one. It is an error rather than a
	// fallback to trust-on-first-use on purpose: TOFU against an address
	// that has never had a key authenticates nothing, and reaching here
	// would mean ember had stopped injecting -- which is worth a refusal,
	// not a warning. hostkey.Store.TrustOnFirstUse remains for a provider
	// that genuinely cannot accept an injected key; nothing is one today.
	return nil, fmt.Errorf(
		"node %s: ember did not mint a host key for this node, so there is nothing to verify it against; "+
			"it will not connect rather than trust whatever answers", r.Handle())
}

// destroy removes the node and records what happened.
//
// A failure is sticky: the record moves to destroy_failed and is never cleared
// automatically, because the machine may still be running and only the
// operator can say otherwise.
func destroy(ctx context.Context, p provider.Provider, o Options, runID node.RunID, h node.Handle) error {
	err := p.Destroy(ctx, h)
	if err != nil && !errors.Is(err, node.ErrNotFound) {
		o.log().Error("destroy failed; the node may still be running and billing", "handle", h.String(), "err", err)
		_ = o.Store.Update(runID, func(r *statestore.Record) {
			r.Phase = statestore.PhaseDestroyFailed
			r.LastErr = err.Error()
		})
		return fmt.Errorf("destroy node %s: %w", h, err)
	}

	if o.Pins != nil {
		// The pin dies with the node it identifies.
		if ferr := o.Pins.Forget(h.ID); ferr != nil {
			o.log().Warn("could not forget the host key pin", "handle", h.String(), "err", ferr)
		}
	}
	if uerr := o.Store.Update(runID, func(r *statestore.Record) { r.Phase = statestore.PhaseDestroyed }); uerr != nil {
		return fmt.Errorf("record the destruction of %s: %w", h, uerr)
	}
	// The estimate is read from the record *before* it is forgotten, which
	// is the only moment both the rate and the elapsed time are in one place.
	bill := billFor(o, runID, p.Caps().MinBilling)

	if ferr := o.Store.Forget(runID); ferr != nil {
		return fmt.Errorf("forget record %s: %w", runID, ferr)
	}
	if bill.Free() {
		o.log().Info("node destroyed", "handle", h.String())
	} else {
		o.log().Info("node destroyed", "handle", h.String(), "cost", bill.String())
	}
	return nil
}

// billFor estimates what the run cost, from the record ember wrote itself.
//
// A failure to read the record is not a failure to destroy: the node is gone,
// which is the thing that mattered, and a missing cost line is worth far less
// than the teardown it would otherwise abort. It is logged rather than
// returned for exactly that reason.
func billFor(o Options, runID node.RunID, min time.Duration) node.Billing {
	if o.Store == nil {
		return node.Billing{}
	}
	rec, err := o.Store.Get(runID)
	if err != nil {
		o.log().Debug("could not read the record to estimate the cost", "run", runID, "err", err)
		return node.Billing{}
	}
	return node.Estimate(rec.Price, rec.Created, o.now(), min)
}
