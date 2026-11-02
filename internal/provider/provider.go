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

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/exposure"
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

	// Destroy removes the machine and everything this provider created
	// alongside it. It must treat "no such machine" as success: the goal of
	// destroy is that the node is gone, and it is.
	//
	// runID is passed rather than re-derived from the machine, because the
	// machine is the one thing that may already be gone. The local provider
	// used to read the run id off the container's own label in order to find
	// the bridge network beside it; a container removed by `docker rm`, a
	// prune or a crashed lab made that inspect return not-found, which is
	// success, so the run id stayed empty and Destroy returned nil over a
	// network that is still there. Every caller has the run id in hand
	// already -- provision is destroying a run it created, reap has it from
	// the finding, and `ember down` reads it off the record -- so nothing is
	// being asked for that was not already known.
	//
	// It may be empty, and then a provider may only clean up what it can
	// still see. That is `ember down` on a handle with no local record and no
	// container left to inspect, which is the one case where nothing anywhere
	// knows which run the leftovers belong to; LeftoverAPI is what closes it.
	// A provider with nothing beside the node ignores it.
	Destroy(ctx context.Context, h node.Handle, runID node.RunID) error

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

	// PlatformPorts are public mappings this provider adds to every node of
	// its own accord, which no request shape can prevent. Empty is the right
	// answer for any provider that does not do this, and the zero value
	// therefore tolerates nothing.
	PlatformPorts []exposure.PlatformPort

	// PublicSSH means nodes are reached over a public SSH port, so exactly
	// one public mapping is expected and anything else is a violation.
	//
	// False -- the zero value -- means the provider publishes nothing at all
	// and is held to the stricter assertion. That direction is deliberate: a
	// provider author who forgets this field gets the strict check, and the
	// failure is a destroyed node with a loud message rather than a public
	// port nobody looks at. See exposure.AssertSSHOnly.
	PublicSSH bool

	// Stores means this provider offers persistent storage that outlives a
	// node, and that it implements StoreAPI.
	//
	// False -- the zero value -- means it does not, so a provider author who
	// forgets the field gets "this provider cannot mount a store" rather than
	// a nil type assertion at the moment a run is about to start. Same
	// direction as PublicSSH above, for the same reason.
	Stores bool

	// Catalogue means this provider reports point-in-time capacity worth
	// recording, and that it implements CatalogueAPI.
	//
	// False -- the zero value -- means there is nothing here to collect, so a
	// provider author who forgets the field gets "nothing to sample" rather
	// than a nil type assertion from an hourly timer nobody is watching.
	// Same direction as the three fields above, and
	// providertest.AssertCatalogueCapsMatch fails when the claim and the
	// interface disagree in either direction.
	Catalogue bool

	// Leftovers means this provider creates objects beside a node that can
	// outlive it, and that it implements LeftoverAPI.
	//
	// False -- the zero value -- means a node is the only thing a run creates
	// here, so there is nothing for a sweep to find. That is the right default
	// in the same direction as the two fields above: a provider author who
	// forgets this gets "nothing to sweep" rather than a nil type assertion,
	// and a provider that genuinely has leftovers cannot hide them by
	// accident, because providertest.AssertLeftoverCapsMatch fails when the
	// claim and the interface disagree in either direction.
	Leftovers bool
}

// StoreAPI is the subset of a provider that manages persistent stores.
//
// It is separate from Provider rather than three more methods on it, because
// most providers will not have stores and a Provider that must stub them is a
// Provider that can stub them wrongly. Caps.Stores says whether a given
// provider satisfies this, and providertest.AssertStoreCapsMatch -- which
// every provider's own test calls -- checks that the two agree, so the claim
// cannot drift from the fact in either direction.
//
// Nothing here destroys a node, and nothing in Provider destroys a store. The
// split is deliberate: these are the two resources in ember with different
// lifetimes, and a single Destroy that could mean either is one typo away from
// deleting forty gigabytes of weights instead of a pod.
type StoreAPI interface {
	// StoreCreate makes a store and returns it. It is never called except
	// from an explicit `ember store create`: a store is a standing monthly
	// charge, and nothing that rents a node may bring one into existence as a
	// side effect.
	StoreCreate(ctx context.Context, name string, sizeGiB int, region string) (node.Store, error)

	// StoreList returns every store at this provider, ember's or not.
	//
	// Every store, because the question `ember ls` asks is "what am I paying
	// for", and a listing filtered to what ember recognises cannot answer it.
	// Filtering happens above, where the answer can say "this one is not
	// ember's" out loud.
	StoreList(ctx context.Context) ([]node.Store, error)

	// StoreDestroy removes a store and everything on it. Not found counts as
	// success, as node destruction does.
	StoreDestroy(ctx context.Context, h node.StoreHandle) error
}

// LeftoverAPI is the subset of a provider that can see and remove what a run
// created beside its node.
//
// Separate from Provider rather than two more methods on it, for the reason
// StoreAPI is separate: most providers rent one object per run and have
// nothing to sweep, and a Provider that must stub these is a Provider that can
// stub them wrongly. Caps.Leftovers says whether a given provider satisfies
// this, and providertest.AssertLeftoverCapsMatch -- which every provider's own
// test calls -- checks that the two agree, so the claim cannot drift from the
// fact in either direction.
//
// Nothing here destroys a node or a store. These are the three resources in
// ember with three different lifetimes, and the split is what stops one
// Destroy from being able to mean any of them.
type LeftoverAPI interface {
	// LeftoverList reports every object ember created beside a node whose
	// node is no longer there.
	//
	// Found by label and never from a local record, because a leftover is by
	// definition what is left when the node and usually the record are both
	// gone. It reports them whether or not their deadline has passed:
	// deciding what may be removed is reconcile's job and the deadline is how
	// it decides, so a lister that filtered would be a second place for that
	// rule to live.
	LeftoverList(ctx context.Context) ([]node.Leftover, error)

	// LeftoverDestroy removes one. Not found counts as success, as node and
	// store destruction do.
	LeftoverDestroy(ctx context.Context, h node.LeftoverHandle) error
}

// CatalogueAPI is the subset of a provider that can report what it currently
// has, in a shape worth keeping.
//
// MODELLED ON ONE PROVIDER, ON PURPOSE. RunPod publishes availability as a
// bare level per card and per datacentre, with no timestamp and no history of
// its own, and that is the shape catalogue.Sample has: two cloud tiers with
// separate prices and separate per-pod ceilings, one level per site, and a
// global summary kept because it disagrees with the sites. None of that is a
// fact about renting GPUs in general -- it is a fact about this provider's
// product, and a closed interface invented from one example is the mistake
// node.Leftover's Kind field and the driver-interface-from-one-server warning
// in AGENTS.md both name.
//
// What would have to be true before this became general: a second provider
// reporting capacity as a graded level per site rather than as a count of
// machines or a spot price, and no history of its own to read instead. Then
// the tiers would be the field to fix first, since "secure and community" is
// the part most likely to be meaningless elsewhere. Until then this is an
// optional interface gated on Caps.Catalogue -- the shape StoreAPI and
// LeftoverAPI already use -- so a provider that has nothing to sample says so
// in a field rather than by stubbing a method it cannot answer.
type CatalogueAPI interface {
	// CatalogueSnapshot reads the provider's catalogue once and returns one
	// sample per type per site.
	//
	// The clock is a parameter and not time.Now() inside. ObservedAt is the
	// only field the provider does not supply and the entire reason the file
	// is kept, so it is passed in by the caller that is about to record it --
	// which also means a test can pin the instant, and the instant is what
	// decides which day file a sample lands in.
	//
	// It is free to call: a catalogue read runs no machine and spends
	// nothing, which is the argument for sampling hourly from a timer.
	CatalogueSnapshot(ctx context.Context, now time.Time) ([]catalogue.Sample, error)
}

// StorePrefix marks a store ember created.
//
// Distinct from a node's label, and parsed differently: Label packs three
// dash-separated fields and ParseLabel splits on "-" expecting exactly three,
// which a store named "wan-2-2" would break into six. A store has no deadline
// and no run id -- outliving runs is the whole point -- so its label is a
// prefix and a name, and reading it back is a CutPrefix rather than a split.
const StorePrefix = LabelPrefix + "-store-"

// StoreLabel is the provider-visible name of a store ember owns.
func StoreLabel(name string) string { return StorePrefix + name }

// ParseStoreLabel reads back what StoreLabel wrote.
//
// Strict and never a substring match, for the same reason ParseLabel is:
// `ember store rm` destroys what this identifies, and somebody else's volume
// in the same account is not ember's to delete.
func ParseStoreLabel(s string) (string, error) {
	name, ok := strings.CutPrefix(s, StorePrefix)
	if !ok || name == "" {
		return "", fmt.Errorf("store %q was not created by ember", s)
	}
	return name, nil
}

// ValidateStoreName refuses a name that would not survive the round trip.
//
// A name is checked here rather than wherever it is typed, because the thing
// being protected is ParseStoreLabel: a name containing the prefix, or
// leading and trailing space the provider silently trims, produces a label
// that reads back as a different store than the one created.
func ValidateStoreName(name string) error {
	if name == "" {
		return fmt.Errorf("store name is empty")
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("store name %q has leading or trailing space", name)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("store name %q contains a path separator", name)
	}
	if strings.Contains(name, StorePrefix) {
		return fmt.Errorf("store name %q contains %q, which is the prefix ember uses to recognise its own stores", name, StorePrefix)
	}
	return nil
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
