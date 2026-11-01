package provision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/providertest"
)

// TestOnReady_ReportsWhatTheOperatorNeedsToKnow.
//
// The hook exists because a slog record is not a thing a UI or a database can
// read, and these four values are what somebody watching a rental needs: which
// node, where it is, how long they have, and what it is costing while they
// have it.
func TestOnReady_ReportsWhatTheOperatorNeedsToKnow(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	o := opts(t)

	type ready struct {
		h        node.Handle
		endpoint node.Endpoint
		deadline time.Time
		price    node.Rate
	}
	var got []ready
	o.OnReady = func(h node.Handle, e node.Endpoint, d time.Time, r node.Rate) {
		got = append(got, ready{h, e, d, r})
	}

	if err := withNode(t.Context(), p, spec(t), o, func(context.Context, Session) error { return nil }); err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("OnReady fired %d time(s), want 1", len(got))
	}
	r := got[0]
	if r.h.ID == "" {
		t.Error("OnReady was given no node")
	}
	if r.endpoint.String() == "" {
		t.Error("OnReady was given no endpoint")
	}
	// The deadline is what bounds the bill, so a zero one would be a UI
	// telling an operator their node lives forever.
	if r.deadline.IsZero() {
		t.Error("OnReady was given no deadline")
	}
}

// TestOnDestroyed_CarriesTheCost is the number this whole program exists to
// report, at the one moment it is knowable.
//
// Unreachable without the hook: WithNode's teardown is deferred, so it runs
// after the caller's closure has already returned. The cost reached slog and
// nothing else.
func TestOnDestroyed_CarriesTheCost(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	o := opts(t)

	var bills []node.Billing
	var errs []error
	o.OnDestroyed = func(_ node.Handle, b node.Billing, err error) {
		bills = append(bills, b)
		errs = append(errs, err)
	}

	// The recording provider's own offer, which is priced, so the estimate is
	// not trivially zero.
	want := p.OfferList[0].Price
	if want == 0 {
		t.Fatal("the stand-in offer is free, so this test cannot see a cost")
	}
	if err := withNode(t.Context(), p, spec(t), o,
		func(context.Context, Session) error { return nil }); err != nil {
		t.Fatal(err)
	}

	if len(bills) != 1 {
		t.Fatalf("OnDestroyed fired %d time(s), want 1", len(bills))
	}
	if errs[0] != nil {
		t.Errorf("a successful destroy reported err = %v", errs[0])
	}
	if bills[0].Rate != want {
		t.Errorf("rate = %v, want the rented offer's %v", bills[0].Rate, want)
	}
	if bills[0].Cost == 0 {
		t.Error("a priced rental reported a cost of nothing")
	}
	// Billable and not Elapsed: a node destroyed in four seconds on a
	// provider with a one-minute minimum costs a minute, and the hook has to
	// carry the figure that is actually charged rather than the one that
	// would teach an operator that short runs are nearly free.
	if bills[0].Billable < bills[0].Elapsed {
		t.Errorf("billable %v is less than elapsed %v", bills[0].Billable, bills[0].Elapsed)
	}
}

// TestOnDestroyed_ReportsTheFailureThatMattersMost.
//
// "destroy failed; the node may still be running and billing" is the one line
// an operator must not miss. A hook that only fired on the happy path would be
// a reader that silently misses the leak -- which is the failure mode every
// other control in this package is written against.
func TestOnDestroyed_ReportsTheFailureThatMattersMost(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Errors["Destroy n1"] = errors.New("provider unreachable")
	o := opts(t)

	var sawErr error
	var fired int
	o.OnDestroyed = func(h node.Handle, _ node.Billing, err error) {
		fired++
		sawErr = err
		if h.ID == "" {
			t.Error("the failure did not name the node that may still be running")
		}
	}

	if err := withNode(t.Context(), p, spec(t), o, func(context.Context, Session) error { return nil }); err == nil {
		t.Fatal("a failed destroy reported success")
	}

	if fired != 1 {
		t.Fatalf("OnDestroyed fired %d time(s) on a failed destroy, want 1", fired)
	}
	if sawErr == nil {
		t.Fatal("OnDestroyed reported no error for a destroy that failed")
	}
	if !errors.Is(sawErr, p.Errors["Destroy n1"]) {
		t.Errorf("err = %v, want the provider's own failure", sawErr)
	}
}

// TestHooks_NilIsTheOrdinaryCase. `ember up` sets neither, and a nil hook must
// not be a nil dereference inside a deferred teardown -- which would panic on
// the way out of every run.
func TestHooks_NilIsTheOrdinaryCase(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	o := opts(t)
	o.OnReady, o.OnDestroyed = nil, nil

	if err := withNode(t.Context(), p, spec(t), o, func(context.Context, Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
