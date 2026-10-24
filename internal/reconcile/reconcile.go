// Package reconcile compares what the providers hold against what ember
// recorded, and says what is wrong.
//
// Scan is a pure function of its inputs. That is deliberate: this is the code
// that decides whether money is burning, and a pure function of (listings,
// errors, records, now) is one a table test can exercise exhaustively without
// a provider, a network or a clock.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/statestore"
)

// Kind classifies a finding.
type Kind uint8

const (
	// KindOK is a node ember knows about that is behaving.
	KindOK Kind = iota

	// KindUntracked is a node at a provider wearing ember's label that the
	// local store has no record of. It is what a lost state directory looks
	// like, and it is still billing.
	KindUntracked

	// KindOrphan is a record with no node at the provider, or an intent that
	// never got an id. Either the create failed and nothing exists, or it
	// succeeded and ember never learned the id -- and those are not
	// distinguishable from here, so it is reported rather than assumed.
	KindOrphan

	// KindExpired is past its deadline and still billable.
	KindExpired

	// KindDestroyFailed is a node whose destroy returned an error. The record
	// is never cleared automatically, because only the operator can confirm
	// the machine is gone.
	KindDestroyFailed

	// KindUnreachable means a provider could not be asked. "I could not check
	// RunPod" must never render as a clean bill of health.
	KindUnreachable
)

var kindNames = map[Kind]string{
	KindOK:            "ok",
	KindUntracked:     "untracked",
	KindOrphan:        "orphan",
	KindExpired:       "expired",
	KindDestroyFailed: "destroy-failed",
	KindUnreachable:   "unreachable",
}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// NeedsAttention reports whether a finding of this kind should make
// `ember ls` exit non-zero.
func (k Kind) NeedsAttention() bool { return k != KindOK }

// Finding is one thing worth saying about one node or provider.
type Finding struct {
	Kind     Kind
	Provider node.ProviderID
	Node     node.Node
	Record   *statestore.Record
	Reason   string
}

func (f Finding) String() string {
	id := "-"
	if !f.Node.Handle.IsZero() {
		id = f.Node.Handle.String()
	} else if f.Record != nil {
		id = string(f.Provider) + "/" + string(f.Record.RunID)
	}
	return fmt.Sprintf("%-14s %-28s %s", f.Kind, id, f.Reason)
}

// Scan classifies everything known about every provider.
//
// errs is a parameter rather than something swallowed inside: a provider that
// could not be reached is a finding, and the whole point of `ember ls --all`
// is that it cannot report "nothing is running" when it does not know.
func Scan(
	listings map[node.ProviderID][]node.Node,
	errs map[node.ProviderID]error,
	records []statestore.Record,
	now time.Time,
) []Finding {
	var out []Finding

	for prov, err := range errs {
		if err == nil {
			continue
		}
		out = append(out, Finding{
			Kind: KindUnreachable, Provider: prov,
			Reason: fmt.Sprintf("could not be asked what it is running: %v", err),
		})
	}

	// Index the live nodes so a record can be matched to one.
	live := map[string]node.Node{}
	for prov, nodes := range listings {
		for _, n := range nodes {
			live[string(prov)+"/"+n.Handle.ID] = n
		}
	}

	seen := map[string]bool{}
	for i := range records {
		rec := records[i]
		h, hasHandle := rec.Handle()

		if !hasHandle {
			// An intent that never learned an id. The create call may still
			// have created something, and only the provider can say.
			if rec.Phase.Billable() {
				out = append(out, Finding{
					Kind: KindOrphan, Provider: rec.Provider, Record: &records[i],
					Reason: "recorded before creating, then never got an id; ask the provider whether a node with label " + rec.Label + " exists",
				})
			}
			continue
		}

		key := string(rec.Provider) + "/" + h.ID
		seen[key] = true

		n, found := live[key]
		switch {
		case rec.Phase == statestore.PhaseDestroyFailed:
			out = append(out, Finding{
				Kind: KindDestroyFailed, Provider: rec.Provider, Node: n, Record: &records[i],
				Reason: "destroy failed and was never confirmed: " + rec.LastErr,
			})
		case !found:
			// The provider was reachable (it is not in errs) and does not
			// have it. That is the good case for a record left over from a
			// crash, but it still needs saying so the record can be cleared.
			if _, unreachable := errs[rec.Provider]; unreachable {
				continue
			}
			out = append(out, Finding{
				Kind: KindOrphan, Provider: rec.Provider, Record: &records[i],
				Reason: "recorded locally but the provider does not have it; the record can be forgotten",
			})
		case n.Expired(now):
			out = append(out, Finding{
				Kind: KindExpired, Provider: rec.Provider, Node: n, Record: &records[i],
				Reason: fmt.Sprintf("past its deadline (%s) and still %s", rec.Deadline.Format(time.RFC3339), n.State),
			})
		default:
			out = append(out, Finding{
				Kind: KindOK, Provider: rec.Provider, Node: n, Record: &records[i],
				Reason: fmt.Sprintf("%s until %s", n.State, rec.Deadline.Format(time.RFC3339)),
			})
		}
	}

	// Anything at a provider that no record mentions.
	for prov, nodes := range listings {
		for _, n := range nodes {
			if seen[string(prov)+"/"+n.Handle.ID] {
				continue
			}
			if !n.State.Billable() {
				continue
			}
			kind, reason := KindUntracked, "wears ember's label but there is no local record of it"
			if n.Expired(now) {
				kind, reason = KindExpired, "wears ember's label, has no local record, and is past its deadline"
			}
			out = append(out, Finding{Kind: kind, Provider: prov, Node: n, Reason: reason})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind > out[j].Kind })
	return out
}

// Attention counts the findings that are not KindOK.
func Attention(fs []Finding) int {
	n := 0
	for _, f := range fs {
		if f.Kind.NeedsAttention() {
			n++
		}
	}
	return n
}

// Collect asks every provider what it is holding, returning partial results
// and the failures side by side.
//
// It never returns early on a provider error. One unreachable provider must
// not hide what the others said, and it must not look like success either.
func Collect(ctx context.Context, reg *provider.Registry) (map[node.ProviderID][]node.Node, map[node.ProviderID]error) {
	listings := map[node.ProviderID][]node.Node{}
	errs := map[node.ProviderID]error{}

	for _, p := range reg.All() {
		nodes, err := p.List(ctx)
		if err != nil {
			errs[p.Name()] = err
			continue
		}
		listings[p.Name()] = nodes
	}
	return listings, errs
}

// Reap destroys everything a scan found past its deadline.
//
// Only KindExpired is acted on. Untracked nodes are reported and left alone:
// a node wearing ember's label with no local record might belong to another
// machine of the operator's, and destroying somebody's running work because
// this laptop had not heard of it is not a trade worth making automatically.
func Reap(ctx context.Context, reg *provider.Registry, store *statestore.Store, fs []Finding) (int, error) {
	destroyed := 0
	var firstErr error

	for _, f := range fs {
		if f.Kind != KindExpired {
			continue
		}
		p, err := reg.Get(f.Provider)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := p.Destroy(ctx, f.Node.Handle); err != nil && !errors.Is(err, node.ErrNotFound) {
			if firstErr == nil {
				firstErr = fmt.Errorf("destroy %s: %w", f.Node.Handle, err)
			}
			continue
		}
		destroyed++
		if f.Record != nil && store != nil {
			if err := store.Forget(f.Record.RunID); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return destroyed, firstErr
}
