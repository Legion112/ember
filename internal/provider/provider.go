// Package provider is the seam between ember and whoever is renting out the
// hardware.
//
// The interface is deliberately small. Polling, backoff, budget enforcement
// and the destroy discipline are identical for every provider, so they live
// once in internal/provision rather than three times behind an interface
// method — three copies of a poll loop is three places to get the backoff
// wrong, and the one that is wrong is the one that leaves a node running.
package provider

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// Provider rents machines.
type Provider interface {
	Name() node.ProviderID
	Caps() Caps

	// Offers lists what can be rented. Implementations narrow server-side
	// where the API allows and need not filter exactly: provision re-applies
	// the filter on our side regardless.
	Offers(ctx context.Context, f node.Filter) ([]node.Offer, error)

	// Create starts a machine and returns a handle. It deliberately does not
	// return a node.Node: a machine created a millisecond ago is not ready
	// and has no endpoint.
	Create(ctx context.Context, req node.CreateRequest) (node.Handle, error)

	// Inspect reports what the provider says about one machine right now, and
	// nothing else. No waiting, no retrying.
	Inspect(ctx context.Context, h node.Handle) (node.Node, error)

	// Destroy removes the machine. It must treat "no such machine" as
	// success: the goal of destroy is that the node is gone, and it is.
	Destroy(ctx context.Context, h node.Handle) error

	// List reports every machine this provider holds that ember created,
	// found by label rather than by local records. It is what makes an orphan
	// visible after the local state is lost.
	List(ctx context.Context) ([]node.Node, error)
}

// Caps describes what a provider can do, as data.
//
// A value rather than optional interfaces and type assertions: branching on
// p.Caps().Interruptible reads, and `if x, ok := p.(interface{ ... })` does
// not.
type Caps struct {
	// HostKeyFromAPI means the provider can report or accept a host key, so
	// the first connection is authenticated rather than trusted on sight.
	HostKeyFromAPI bool

	// Interruptible means instances can be taken back mid-run.
	Interruptible bool

	// Tags means the provider has a real tag API. Without it ember encodes
	// its label into whatever free-text field exists, usually the name.
	Tags bool

	// Free means renting costs nothing, which is true only of the local
	// provider and lets the budget check be skipped honestly rather than by
	// comparing against zero.
	Free bool

	// MinBilling is the smallest unit the provider charges for.
	MinBilling time.Duration
}

// Registry holds the providers this run knows about.
type Registry struct{ byName map[node.ProviderID]Provider }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byName: map[node.ProviderID]Provider{}} }

// Add registers p.
func (r *Registry) Add(p Provider) error {
	name := p.Name()
	if name == "" {
		return fmt.Errorf("provider has no name")
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("provider %q registered twice", name)
	}
	r.byName[name] = p
	return nil
}

// Get returns the provider called name.
func (r *Registry) Get(name node.ProviderID) (Provider, error) {
	p, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("no provider called %q is configured; known: %s", name, strings.Join(r.Names(), ", "))
	}
	return p, nil
}

// All returns every provider, name-sorted so `ember ls` is deterministic.
func (r *Registry) All() []Provider {
	out := make([]Provider, 0, len(r.byName))
	for _, n := range r.Names() {
		out = append(out, r.byName[node.ProviderID(n)])
	}
	return out
}

// Names returns the registered provider names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, string(n))
	}
	sort.Strings(out)
	return out
}

// FilterOffers re-applies f and sorts the survivors cheapest first.
//
// Providers already narrow server-side, so this looks redundant. It is a cost
// control: a marketplace that answers a max-price query with a dearer offer —
// because the field meant something else, or because the ask changed between
// question and answer — cannot cause a spend when the ceiling is also
// enforced here, by a pure function with full test coverage, on our side of
// the wire.
func FilterOffers(f node.Filter, in []node.Offer) []node.Offer {
	out := make([]node.Offer, 0, len(in))
	for _, o := range in {
		if o.Validate() != nil {
			continue
		}
		if f.Match(o) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Price != out[j].Price {
			return out[i].Price < out[j].Price
		}
		// Ties broken on the token so the choice is reproducible: an operator
		// comparing two runs should not have to wonder whether the price
		// changed or the sort did.
		return out[i].Token < out[j].Token
	})
	return out
}

// LabelPrefix marks everything ember creates.
const LabelPrefix = "ember"

// Label encodes the run id and the deadline into one string.
//
// This is ember's ownership marker, and it is the authority rather than the
// local state file: a node ember created is findable from the provider alone,
// which is what makes an orphan visible after a crash that lost the record.
func Label(runID node.RunID, deadline time.Time) string {
	return fmt.Sprintf("%s-%s-%d", LabelPrefix, runID, deadline.Unix())
}

// ParseLabel reads back what Label wrote.
//
// Strict, and never a substring match. `ember reap` destroys what this
// identifies, so a label ember did not write must not parse as one it did —
// somebody else's container is not an ember orphan.
func ParseLabel(s string) (node.RunID, time.Time, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 || parts[0] != LabelPrefix {
		return "", time.Time{}, fmt.Errorf("label %q was not written by ember", s)
	}
	id, err := node.ParseRunID(parts[1])
	if err != nil {
		return "", time.Time{}, fmt.Errorf("label %q: %w", s, err)
	}
	unix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("label %q has an unreadable deadline: %w", s, err)
	}
	return id, time.Unix(unix, 0).UTC(), nil
}
