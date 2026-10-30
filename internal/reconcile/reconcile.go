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

	// KindStore is a persistent store and what it costs per month.
	//
	// Informational, and deliberately not attention-worthy. A store existing
	// is the expected state -- it was created on purpose, by a command the
	// operator had to type -- and ember cannot tell one that is finished with
	// from one being kept between runs. Making every store exit `ember ls`
	// non-zero would teach the operator to ignore exit 2, which is worse than
	// not flagging: the same reasoning that leaves an untracked *node*
	// reported but unreaped.
	//
	// What this kind does guarantee is that the charge is said out loud on
	// every run. Visibility is the control here, not classification.
	//
	// It is last in this list, which gives it the highest value, and Scan
	// sorts worst-first by that value -- so a store would sort above a
	// destroy-failed node if the two lists were ever merged and sorted
	// together. Scan never emits this kind and ScanStores sorts by
	// NeedsAttention rather than by value, so today nothing does; anyone
	// merging them later has to sort deliberately.
	KindStore
)

var kindNames = map[Kind]string{
	KindOK:            "ok",
	KindUntracked:     "untracked",
	KindOrphan:        "orphan",
	KindExpired:       "expired",
	KindDestroyFailed: "destroy-failed",
	KindUnreachable:   "unreachable",
	KindStore:         "store",
}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// NeedsAttention reports whether a finding of this kind should make
// `ember ls` exit non-zero.
//
// A switch rather than `k != KindOK`, so that a kind added later needs
// attention by default and its author has to decide otherwise in writing.
// The exemptions are the two states that are not problems: a node behaving,
// and a store existing.
func (k Kind) NeedsAttention() bool {
	switch k {
	case KindOK, KindStore:
		return false
	default:
		return true
	}
}

// Finding is one thing worth saying about one node or provider.
type Finding struct {
	Kind     Kind
	Provider node.ProviderID
	Node     node.Node
	Record   *statestore.Record
	Reason   string

	// Accrued is what this node has cost so far, where ember knows enough to
	// say. Nil when it does not: an untracked node has no record, so ember
	// has the provider's rate only if the listing carried one, and no idea
	// when it started unless the label's deadline implies it.
	Accrued *node.Billing

	// Store is the store this finding is about, where it is about one.
	Store *node.Store

	// Monthly is what that store costs every month.
	//
	// Separate from Accrued because the two are different claims and reading
	// one as the other would be the wrong kind of wrong. Accrued is money
	// already spent, and it stops when the node is destroyed. Monthly is
	// money that keeps being spent until somebody removes the store -- there
	// is no deadline, no reaper and no Ctrl-C that ends it.
	Monthly node.MicroUSD
}

func (f Finding) String() string {
	id := "-"
	switch {
	case f.Store != nil:
		id = f.Store.Handle.String()
	case !f.Node.Handle.IsZero():
		id = f.Node.Handle.String()
	case f.Record != nil:
		id = string(f.Provider) + "/" + string(f.Record.RunID)
	}
	s := fmt.Sprintf("%-14s %-28s %s", f.Kind, id, f.Reason)
	if f.Store != nil && f.Monthly > 0 {
		// Per month, and said so, because every other figure this command
		// prints is a total already spent. A reader who skimmed "$2.80" off
		// a store line and off an orphan line would have read two very
		// different numbers the same way.
		s += fmt.Sprintf(" [%s/month]", f.Monthly)
	}
	if f.Accrued != nil && !f.Accrued.Free() {
		// What it has cost so far, on the same line. "An orphaned H100 burns
		// 175 roubles an hour in silence" is the whole reason this command
		// exists, and a number is what turns that from a sentence in a
		// README into something an operator acts on this afternoon.
		s += fmt.Sprintf(" [%s so far]", f.Accrued.Cost)
	}
	return s
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
	defer func() {
		// Accrued cost is filled in once, here, rather than at each of the
		// several places a Finding is built: one place to be right, and a
		// kind added later cannot forget it.
		for i := range out {
			out[i].Accrued = accrued(out[i], now)
		}
	}()

	for prov, err := range errs {
		if err == nil {
			continue
		}
		out = append(out, Finding{
			Kind: KindUnreachable, Provider: prov,
			Reason: fmt.Sprintf("could not be asked what it is running: %v", err),
		})
	}

	// Index the live nodes so a record can be matched to one, by provider
	// node id and also by ember's own run id.
	//
	// The run-id index is what makes a record written before the create call
	// worth writing. A process killed between Create returning and the node
	// id being stored leaves a record with no node id; matching only on node
	// id reports that record and its very much alive node as two unrelated
	// findings, and reap can act on neither. The label carries the run id, so
	// the two can be joined -- which is what "labels are the source of truth,
	// the store is a cache" has to mean in practice.
	live := map[string]node.Node{}
	byRun := map[string]node.Node{}
	for prov, nodes := range listings {
		for _, n := range nodes {
			live[string(prov)+"/"+n.Handle.ID] = n
			if n.RunID != "" {
				byRun[string(prov)+"/"+string(n.RunID)] = n
			}
		}
	}

	seen := map[string]bool{}
	for i := range records {
		rec := records[i]
		h, hasHandle := rec.Handle()

		if !hasHandle {
			// An intent that never learned an id. Before reporting it as
			// unanswerable, look for a node wearing this run's label: that is
			// the crash-between-create-and-record case, and the node is right
			// there to be classified and reaped like any other.
			if n, found := byRun[string(rec.Provider)+"/"+string(rec.RunID)]; found {
				seen[string(rec.Provider)+"/"+n.Handle.ID] = true
				out = append(out, classify(n, &records[i], rec, now,
					"created but the run was killed before ember recorded its id; found again by its label"))
				continue
			}
			if rec.Phase.Billable() {
				out = append(out, Finding{
					Kind: KindOrphan, Provider: rec.Provider, Record: &records[i],
					Reason: "recorded before creating, then never got an id, and no node wears this run's label; ask the provider whether a node labelled " + rec.Label + " exists",
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
		case rec.Holder.Abandoned():
			// Inside its deadline, so reap still will not touch it -- the
			// deadline is the authority on destruction, and `ember down`
			// exists for the operator who has decided. But it is billing
			// with nobody watching, and that is worth saying out loud
			// rather than printing "ok".
			out = append(out, Finding{
				Kind: KindOrphan, Provider: rec.Provider, Node: n, Record: &records[i],
				Reason: fmt.Sprintf("%s until %s, but the ember process holding it (pid %d) is gone; it is billing with nobody watching. Destroy it with: ember down %s",
					n.State, rec.Deadline.Format(time.RFC3339), rec.Holder.PID, n.Handle),
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

// classify decides what a record paired with a live node amounts to.
//
// found explains how the pairing was made, which matters only for the
// label-matched case: an operator reading "expired" about a node whose record
// has no id deserves to know why ember is confident they are the same node.
func classify(n node.Node, rec *statestore.Record, r statestore.Record, now time.Time, found string) Finding {
	f := Finding{Provider: r.Provider, Node: n, Record: rec}
	switch {
	case n.Expired(now):
		f.Kind = KindExpired
		f.Reason = fmt.Sprintf("past its deadline (%s) and still %s; %s", r.Deadline.Format(time.RFC3339), n.State, found)
	case r.Holder.Abandoned():
		f.Kind = KindOrphan
		f.Reason = fmt.Sprintf("%s until %s, but the ember process holding it (pid %d) is gone; it is billing with nobody watching. Destroy it with: ember down %s",
			n.State, r.Deadline.Format(time.RFC3339), r.Holder.PID, n.Handle)
	default:
		f.Kind = KindOrphan
		// Not expired, so reap deliberately leaves it: the deadline is the
		// authority on when a node may be destroyed, and the node's own
		// self-destruct is what ends it when ember is no longer running.
		f.Reason = fmt.Sprintf("%s until %s, but no ember process is holding it; %s", n.State, r.Deadline.Format(time.RFC3339), found)
	}
	return f
}

// accrued estimates what a finding's node has cost so far.
//
// Only where ember wrote a record, because that is the only place the quoted
// rate and the start time are both known. The provider's minimum is not
// applied: this is a running total for a node that has not been destroyed, and
// a minimum describes the floor on a completed charge rather than something
// already accrued.
func accrued(f Finding, now time.Time) *node.Billing {
	if f.Record == nil || f.Record.Price == 0 {
		return nil
	}
	b := node.Estimate(f.Record.Price, f.Record.Created, now, 0)
	return &b
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
		// A store is never reaped, whatever kind it somehow arrived as.
		// Destroy here takes a node.Handle, and a store finding carries a
		// zero one -- so without this the call would be made against an empty
		// handle and the error blamed on the provider. The guard is cheap and
		// the failure it prevents is the expensive one: these are the two
		// resources with different lifetimes, and the whole reason StoreAPI
		// is a separate interface is that nothing which destroys a node
		// should ever be handed a store.
		if f.Store != nil {
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
