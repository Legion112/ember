package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// ScanLeftovers classifies what a destroyed node left behind.
//
// Pure, for the reason Scan is: this is the code that decides what `ember
// reap` deletes, and a pure function of (leftovers, errors, now) is one a
// table test can exercise without a daemon or a clock.
func ScanLeftovers(
	leftovers map[node.ProviderID][]node.Leftover,
	errs map[node.ProviderID]error,
	now time.Time,
) []Finding {
	var out []Finding

	// Unreachable first, as both the other scans do. "I could not ask the
	// daemon what ember left on it" must not render as "nothing was left",
	// and the reason names leftovers explicitly because a provider can fail
	// one listing and answer another -- three identical lines would read as a
	// duplicate rather than as three separate gaps.
	for prov, err := range errs {
		if err == nil {
			continue
		}
		out = append(out, Finding{
			Kind:     KindUnreachable,
			Provider: prov,
			Reason:   fmt.Sprintf("could not be asked what its destroyed nodes left behind: %v", err),
		})
	}

	for prov, list := range leftovers {
		for i := range list {
			l := list[i]
			if !l.Expired(now) {
				// Not reported at all, rather than reported and not reaped.
				//
				// Create makes the network before the container, so for a
				// moment every live run looks exactly like a leftover -- and
				// a finding that is sometimes false is worse than one that
				// arrives late: it teaches an operator to ignore exit 2,
				// which is the reasoning that keeps a store out of the
				// attention count. The deadline cannot be in the future for
				// a run that has finished, so waiting for it costs at most
				// one TTL of visibility and buys a report that is never
				// wrong. It is also the authority reap already has over
				// nodes, used here for the same reason.
				continue
			}
			out = append(out, Finding{
				Kind:     KindLeftover,
				Provider: prov,
				Leftover: &list[i],
				Reason:   leftoverReason(l),
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].Kind.NeedsAttention(), out[j].Kind.NeedsAttention(); a != b {
			return a
		}
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return leftoverName(out[i]) < leftoverName(out[j])
	})
	return out
}

func leftoverName(f Finding) string {
	if f.Leftover == nil {
		return ""
	}
	return f.Leftover.Name
}

// leftoverReason says what this is and what removes it.
//
// It names `ember reap` because the operator who reads this line has no other
// way to act on it: the object is not a node, so `ember down` cannot take it,
// and its run is over, so nothing will ever destroy it as a side effect.
func leftoverReason(l node.Leftover) string {
	if l.RunID == "" {
		// No readable ember label. Reported anyway -- it wears ember's kind
		// label, so it is ember's to clean up -- but the run cannot be named,
		// and claiming one would be a guess about what this belonged to.
		return fmt.Sprintf("%s wearing ember's label, left behind by a node that is gone; "+
			"ember could not read which run it belonged to. Remove it with: ember reap", l.Kind)
	}
	return fmt.Sprintf("%s left behind by run %s, whose node is gone; "+
		"nothing bills for it and nothing else will remove it. Remove it with: ember reap", l.Kind, l.RunID)
}

// CollectLeftovers asks every provider that has any what its destroyed nodes
// left behind.
//
// Gated on the cap and the interface together, as CollectStores is. A provider
// with nothing beside a node is skipped rather than reported as having
// nothing, because the two are different answers and only one of them is
// worth a line.
func CollectLeftovers(ctx context.Context, reg *provider.Registry) (map[node.ProviderID][]node.Leftover, map[node.ProviderID]error) {
	leftovers := map[node.ProviderID][]node.Leftover{}
	errs := map[node.ProviderID]error{}

	for _, p := range reg.All() {
		lp, ok := p.(provider.LeftoverAPI)
		if !ok || !p.Caps().Leftovers {
			continue
		}
		list, err := lp.LeftoverList(ctx)
		if err != nil {
			errs[p.Name()] = err
			continue
		}
		leftovers[p.Name()] = list
	}
	return leftovers, errs
}

// ReapLeftovers removes everything a leftover scan found.
//
// Separate from Reap, and not a branch inside it, because the two counts mean
// different things to the operator reading them: "destroyed 2 of 3 expired
// nodes" is about money, and a bridge network folded into that number would
// overstate what was burning. It is also the guard: Reap cannot be handed a
// leftover by mistake if Reap never looks at one.
//
// Every finding is attempted even after one fails. A single network Docker
// refuses to remove -- something else is still attached to it -- must not
// leave the rest of them on the daemon, because the whole failure this exists
// to prevent is an accumulation.
func ReapLeftovers(ctx context.Context, reg *provider.Registry, fs []Finding) (int, error) {
	removed := 0
	var firstErr error

	for _, f := range fs {
		if f.Kind != KindLeftover || f.Leftover == nil {
			continue
		}
		p, err := reg.Get(f.Provider)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		lp, ok := p.(provider.LeftoverAPI)
		if !ok {
			// Unreachable while CollectLeftovers is what produced these
			// findings, and an error rather than a skip anyway: a leftover
			// that arrived from a provider which cannot remove one is a
			// finding nothing will ever act on, and silently counting zero
			// would report a clean sweep over an object still sitting there.
			if firstErr == nil {
				firstErr = fmt.Errorf("provider %s reported a leftover but cannot remove one", f.Provider)
			}
			continue
		}
		if err := lp.LeftoverDestroy(ctx, f.Leftover.Handle); err != nil && !errors.Is(err, node.ErrNotFound) {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove %s %s: %w", f.Leftover.Kind, f.Leftover.Name, err)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
