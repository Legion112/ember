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
func WithNode(
	ctx context.Context,
	p provider.Provider,
	spec node.Spec,
	o Options,
	fn func(context.Context, Session) error,
) (err error) {
	if o.Store == nil {
		return errors.New("provision needs a state store: a node nobody recorded is a node nobody can reap")
	}

	offer, err := PickOffer(ctx, p, spec, o)
	if err != nil {
		return err
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

	trust, err := trustFor(ready, o)
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

// PickOffer chooses what to rent.
func PickOffer(ctx context.Context, p provider.Provider, spec node.Spec, o Options) (node.Offer, error) {
	offers, err := p.Offers(ctx, spec.Filter)
	if err != nil {
		return node.Offer{}, fmt.Errorf("list offers from %s: %w", p.Name(), err)
	}
	// Re-filtered our side regardless of what the API was asked: see
	// provider.FilterOffers for why that is a cost control.
	offers = provider.FilterOffers(spec.Filter, offers)
	if len(offers) == 0 {
		return node.Offer{}, fmt.Errorf("%w: %s has nothing matching the filter", node.ErrNoCapacity, p.Name())
	}

	free := p.Caps().Free
	var budgetErr error
	for _, candidate := range offers {
		if !free {
			if err := o.Budget.Allow(candidate, spec.TTL); err != nil {
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
func trustFor(r node.ReadyNode, o Options) (hostkey.Trust, error) {
	k := r.Node().HostKey
	if !k.IsZero() {
		fp, err := hostkey.FromNodeKey(k)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", r.Handle(), err)
		}
		return hostkey.Pinned(fp)
	}
	if o.Pins == nil {
		return nil, fmt.Errorf("node %s published no host key and there is no pin store to fall back on", r.Handle())
	}
	// Nothing can authenticate the first connection to an address that has
	// never had a key. The node is recorded as pinned rather than minted so
	// `ember ls` can show which nodes were never properly authenticated.
	o.log().Warn("no host key from the provider; trusting this one on first use", "handle", r.Handle().String())
	return o.Pins.TrustOnFirstUse(r.Handle().ID), nil
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
	if ferr := o.Store.Forget(runID); ferr != nil {
		return fmt.Errorf("forget record %s: %w", runID, ferr)
	}
	o.log().Info("node destroyed", "handle", h.String())
	return nil
}
