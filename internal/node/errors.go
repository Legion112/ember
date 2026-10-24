package node

import "errors"

// Sentinel errors every provider maps its own failures onto, so callers above
// the provider can branch without knowing whose API said no.
//
// They are matched with errors.Is and always wrapped with context by the
// provider, because "not found" with no node id in it sends the reader to the
// wrong machine.
var (
	// ErrNotFound means the provider has no such node. Destroy treats it as
	// success: the goal of destroy is that the node is gone, and it is.
	ErrNotFound = errors.New("node not found")

	// ErrNoCapacity means the offer evaporated between listing and creating.
	// On a marketplace this is ordinary, not exceptional — another bidder got
	// the ask — so callers retry with the next offer rather than failing.
	ErrNoCapacity = errors.New("no capacity for this offer")

	// ErrRateLimited means back off. httpx retries this one; nothing above
	// the provider should.
	ErrRateLimited = errors.New("rate limited by provider")

	// ErrUnauthorized means the credential is missing, wrong or expired.
	// Never retried: retrying a bad key is how an account gets locked.
	ErrUnauthorized = errors.New("provider rejected the credential")

	// ErrInterrupted means the provider took the node back. Vast.ai does this
	// to interruptible instances whenever someone bids higher.
	ErrInterrupted = errors.New("node was interrupted by the provider")

	// ErrBudget means the run would cost more than the configured ceiling. It
	// is returned before anything is created, never after.
	ErrBudget = errors.New("over budget")
)
