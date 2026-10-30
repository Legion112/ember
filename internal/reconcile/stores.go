package reconcile

import (
	"context"
	"fmt"
	"sort"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// ScanStores reports what persistent storage is costing, as findings.
//
// Separate from Scan, and not merged into it, for two reasons. Scan's
// signature and its table tests are about nodes, and widening a four-argument
// pure function to six to carry a second resource makes both harder to read.
// More importantly the two answer different questions: Scan asks "is anything
// running that should not be", and this asks "what is being charged for while
// nothing runs at all".
//
// It takes no clock. A node's cost is elapsed time against a rate; a store's
// is a standing monthly charge that does not depend on when you look.
//
// configured maps a provider to the store name this configuration names, so a
// store nobody here mentions can be said out loud. It is not a leak detector:
// see the comment on KindStore for why ember does not claim to know which
// stores are finished with.
func ScanStores(
	stores map[node.ProviderID][]node.Store,
	errs map[node.ProviderID]error,
	configured map[node.ProviderID]string,
) []Finding {
	var out []Finding

	// Unreachable first, for the same reason Scan does it: "I could not ask
	// RunPod what volumes it is billing me for" must not render as "nothing
	// is being billed". The reason names stores explicitly, because a
	// provider can fail one listing and answer the other, and two identical
	// lines would look like a duplicate rather than two separate gaps.
	for prov, err := range errs {
		if err == nil {
			continue
		}
		out = append(out, Finding{
			Kind:     KindUnreachable,
			Provider: prov,
			Reason:   fmt.Sprintf("could not be asked what stores it holds: %v", err),
		})
	}

	for prov, list := range stores {
		for _, s := range list {
			out = append(out, Finding{
				Kind:     KindStore,
				Provider: prov,
				Store:    &s,
				Monthly:  s.Monthly(),
				Reason:   storeReason(s, configured[prov] == s.Name),
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
		return storeName(out[i]) < storeName(out[j])
	})
	return out
}

func storeName(f Finding) string {
	if f.Store == nil {
		return ""
	}
	return f.Store.Name
}

// storeReason says whether this configuration knows about the store.
//
// Both readings are reported and neither is a failure. ember cannot tell a
// store that is finished with from one being kept between runs, and a line
// that said "leaked" about weights somebody deliberately uploaded would be
// wrong in the expensive direction -- it invites an `ember store rm` that
// costs a re-upload.
func storeReason(s node.Store, named bool) string {
	where := ""
	if s.Region != "" {
		where = " in " + s.Region
	}
	if named {
		return fmt.Sprintf("%d GiB%s, named in this configuration", s.SizeGiB, where)
	}
	return fmt.Sprintf("%d GiB%s, and nothing in this configuration names it; "+
		"remove it with `ember store rm %s` once it is finished with", s.SizeGiB, where, s.Name)
}

// CollectStores asks every provider that has stores what it is holding.
//
// A provider without stores is skipped rather than recorded as an error: not
// having the feature is not a failure to answer, and an error there would make
// `ember ls` exit non-zero on every run against the local provider.
//
// Like Collect, it never returns early. One provider that cannot be asked must
// not hide what the others said, and must not look like success either.
func CollectStores(ctx context.Context, reg *provider.Registry) (map[node.ProviderID][]node.Store, map[node.ProviderID]error) {
	stores := map[node.ProviderID][]node.Store{}
	errs := map[node.ProviderID]error{}

	for _, p := range reg.All() {
		sp, ok := p.(provider.StoreAPI)
		if !ok || !p.Caps().Stores {
			continue
		}
		list, err := sp.StoreList(ctx)
		if err != nil {
			errs[p.Name()] = err
			continue
		}
		stores[p.Name()] = list
	}
	return stores, errs
}

// MonthlyTotal is what every store in these findings costs per month.
//
// Printed even when it is the only thing to say. The whole reason this phase
// exists before anything can create a store is that a standing charge with no
// node attached is invisible otherwise, and an operator who never sees the
// number has no moment at which to decide it is too high.
func MonthlyTotal(fs []Finding) node.MicroUSD {
	var total node.MicroUSD
	for _, f := range fs {
		if f.Kind != KindStore {
			continue
		}
		if node.MaxMicroUSD-total < f.Monthly {
			return node.MaxMicroUSD
		}
		total += f.Monthly
	}
	return total
}
