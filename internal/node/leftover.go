package node

import "time"

// A leftover is something ember created for a run, beside the node, that the
// node's destruction should have taken with it and did not.
//
// There is exactly one kind today: the local provider gives every node its own
// Docker bridge network, and `local.Destroy` used to find the run id by
// inspecting the container. A container removed by anything other than ember
// -- `docker rm`, a prune, a crashed lab's cleanup -- made that inspect return
// not-found, which Destroy treats as success, so the run id stayed empty and
// the network was never looked for. Destroy returned nil over a network that
// is still there.
//
// Nothing bills for a bridge network, which is exactly why this needed a type
// rather than a comment. The failure is not an invoice: it is every local run
// failing at once, weeks later, with "all predefined address pools have been
// fully subnetted" -- an error that names Docker and not ember. That has
// already happened once here, to the lab harness, which removed containers and
// not the networks beside them.
//
// So a leftover is reported by `ember ls` and removed by `ember reap`, the
// command whose job is cleaning up orphans, rather than being discovered by an
// exhausted address pool.

// LeftoverID is a provider's own identifier for a leftover.
type LeftoverID string

// LeftoverHandle names one leftover at one provider.
//
// Separate from Handle and from StoreHandle for the reason those two are
// separate from each other: these are three resources with three different
// lifetimes, and a single handle type that could mean any of them is one typo
// away from `ember reap` destroying weights instead of a bridge network.
type LeftoverHandle struct {
	Provider ProviderID
	ID       LeftoverID
}

func (h LeftoverHandle) String() string {
	return string(h.Provider) + "/" + string(h.ID)
}

// IsZero reports whether the handle names nothing.
func (h LeftoverHandle) IsZero() bool { return h.Provider == "" && h.ID == "" }

// Leftover is one object ember created for a run whose node is gone.
type Leftover struct {
	Handle LeftoverHandle

	// Kind is what sort of object this is, in the provider's own vocabulary --
	// "network" for the only one that exists today. It is a string because it
	// is read from the label the provider wrote (local.LabelKind holds
	// "node" | "network" | "store") and is only ever displayed. A typed enum
	// here would be a closed set invented from one case, which is how an
	// abstraction ends up shaped by its single example.
	Kind string

	// Name is what the provider calls it, for the operator to recognise in
	// `docker network ls`. The handle's ID is the daemon's own id, which is
	// what removal needs and not what a person reads.
	Name string

	// RunID is the run this was created for, read from the label rather than
	// from any local record. The label is the source of truth: a leftover
	// exists precisely in the cases where the journal and the node are both
	// gone, so a leftover ember could only recognise from a record would be
	// one ember could never recognise.
	RunID RunID

	// Deadline is the run's deadline, from the same label.
	//
	// It is carried because it is what makes removal safe. Create makes the
	// network before the container, so for a moment every live run looks
	// exactly like a leftover -- and a sweep that acted on that would delete
	// a working run's network. A deadline in the future cannot be a run that
	// has finished, so reporting and reaping are bounded by it, which is the
	// same authority reap already has over nodes: the deadline decides when
	// something may be destroyed.
	//
	// Zero means the label could not be read. That is treated as past, not as
	// unknown: the object wears ember's own kind label, nothing else will ever
	// clean it up, and refusing to touch it would mean a network ember wrote a
	// label onto in some earlier spelling leaks for good.
	Deadline time.Time
}

// Expired reports whether this leftover may be removed.
func (l Leftover) Expired(now time.Time) bool { return !now.Before(l.Deadline) }
