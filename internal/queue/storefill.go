package queue

import (
	"context"
	"sync"
)

// TWO SLOTS CAN FILL ONE STORE AT ONCE, WHICH max_nodes INTRODUCED AND WHICH
// THE ONE-RENTAL LOOP MADE UNREPRESENTABLE (#204).
//
// `AppPlanner.Plan` rents a machine with no card when the store this run needs
// is missing its weights -- the $0.61-of-a-$0.63-rental measurement that
// `fillStore` exists for. At one rental the two plans were SERIAL, so by the
// time the second ran the store was full and the second fill could not happen.
// With N slots two plans run at once: both ask the store's endpoint, both are
// told the weights are missing, and both go and rent a CPU machine to fetch
// the same fifty-six gigabytes.
//
// WHAT THAT COSTS, so the fix is in proportion. One extra $0.06/hour machine
// for as long as the fetch takes -- measured at eighty-one minutes for 56 GiB
// over a home link, which is about eight cents -- plus two writers into one
// mount. The writers are the half worth checking and they are already safe:
// `embernode fetch` downloads under a temporary name and renames into place
// only once the digest matches, so two fetches of one asset race on an atomic
// rename and whichever lands is the right bytes. So this is money and a
// pointless rental rather than corruption. It is still the defect
// `TestStore_CreateMountRunDestroyRemove` is written against, one layer up:
// that lab's discriminator is ONE asset fetch across two runs, and the thing
// it refuses is a second node re-downloading what the first already placed.
//
// THE MECHANISM IS A LOCK PER STORE AND A RE-ASK UNDER IT, not a record of
// what this process has filled. `Plan` already re-asks the store's own
// endpoint after a fill -- "Asked again rather than assumed" -- and that is
// the authority here too: the second slot waits, then asks, and finds the
// weights that the first one put there. A "filled in this process" flag would
// be a second, invented source of truth for a question the store answers, and
// it would be wrong the first time two requests on one store wanted different
// assets.
//
// WHAT IT DOES NOT CLOSE, for Commitments' reason and with the same honesty:
// another ember process filling the same store is invisible to this one. Two
// `ember serve`s over one store would both fetch, exactly as they do today,
// and the only thing that would close it is a lock on the store itself.
//
// IT IS CONTEXT-AWARE AND THAT IS NOT A DETAIL. A `sync.Mutex` cannot be
// abandoned, so a slot waiting for another slot's fill would hold up
// `queue.Run`'s shutdown for the length of a fetch -- up to eighty-one
// minutes, on a process whose whole shutdown story is that it waits for every
// slot. The wait is therefore a select on the caller's own context, and a
// cancelled one is reported as what it is: this request ends, the fill that is
// running carries on, and the node it rented is destroyed by its own slot.

// StoreFills serialises the store fills this process starts, one store at a
// time.
//
// A NIL *StoreFills SERIALISES NOTHING, which is today's behaviour and what
// every `AppPlanner{}` literal in the suite gets. That is not laziness about
// the wiring: a nil that behaved differently from an empty one would mean the
// tests measured a mechanism production does not have, and at `max_nodes: 1`
// there is nothing to serialise because the plans are serial already. The
// production wiring is held by a guard rather than by this default --
// see TestServe_WiresTheNodeCeilingIntoTheDrainLoop.
type StoreFills struct {
	mu sync.Mutex

	// per is one slot of capacity per store, by name.
	//
	// A BUFFERED CHANNEL AND NOT A sync.Mutex, because the wait has to be
	// abandonable: see the note above about shutdown. A channel is the only
	// lock in Go that can be waited for in a select.
	per map[string]chan struct{}
}

// Fill runs fn with this store's fill held, or reports why it could not wait.
//
// THE RE-ASK IS THE CALLER'S AND NOT THIS FUNCTION'S, which is the division
// that keeps the authority in one place: this decides WHO may be filling a
// store, and `Plan` decides WHETHER it still needs filling -- by asking the
// store's own endpoint, which is the only thing that knows.
func (f *StoreFills) Fill(ctx context.Context, store string, fn func() error) error {
	if f == nil {
		return fn()
	}
	ch := f.slot(store)
	select {
	case ch <- struct{}{}:
	case <-ctx.Done():
		// Reported rather than filled anyway. A request whose context ended
		// while it waited for another slot's fetch is a request the operator
		// or the shutdown has already ended, and classify turns this into a
		// `cancelled` row rather than a failure.
		return ctx.Err()
	}
	defer func() { <-ch }()
	return fn()
}

// slot is this store's one place, created on demand.
func (f *StoreFills) slot(store string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.per == nil {
		f.per = map[string]chan struct{}{}
	}
	if f.per[store] == nil {
		// Buffered by one: a send is the acquire and a receive is the
		// release, which is the idiom and is what makes the acquire a case in
		// a select.
		f.per[store] = make(chan struct{}, 1)
	}
	return f.per[store]
}
