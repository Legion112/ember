// Package node is ember's domain core: what a rented machine is, what it
// costs, and what ember asked for.
//
// It imports nothing from internal/. That is what keeps it unit-testable with
// no provider, no Docker and no network, and it is enforced by
// TestNodeImportsNothing rather than by review.
package node

import "fmt"

// State is where a node is in its life.
//
// StateInvalid is the zero value and is never valid: a State nobody set is a
// State nobody decided, and the only safe reading of "nobody decided" is that
// the node might be running. See Billable.
type State uint8

const (
	StateInvalid State = iota
	StatePending
	StateProvisioning
	StateReady
	StateStopped
	StateInterrupted
	StateFailed
	StateDestroyed
)

var stateNames = map[State]string{
	StateInvalid:      "invalid",
	StatePending:      "pending",
	StateProvisioning: "provisioning",
	StateReady:        "ready",
	StateStopped:      "stopped",
	StateInterrupted:  "interrupted",
	StateFailed:       "failed",
	StateDestroyed:    "destroyed",
}

func (s State) String() string {
	if n, ok := stateNames[s]; ok {
		return n
	}
	return fmt.Sprintf("state(%d)", uint8(s))
}

// Billable reports whether a node in this state may still be costing money.
//
// It is true for everything except StateDestroyed, deliberately including
// StateInvalid, StateFailed and StateStopped:
//
//   - StateStopped is the expensive surprise. A stopped RunPod pod is not
//     running a GPU and is still billed for its volume, and a stopped Vast
//     instance is billed until it is destroyed. "Not running" is not "not
//     charging".
//   - StateFailed means the provider could not start it. Whether we are
//     charged for the attempt is the provider's business, and assuming we are
//     not is how a failed node sits in an account for a month.
//   - StateInvalid means we never learned anything. No information must never
//     render as good news.
//
// A state added to this package later is billable by default for the same
// reason: the author has to think about it to make it free, not to make it
// cost money.
func (s State) Billable() bool { return s != StateDestroyed }

// Terminal reports whether this state can still change on its own.
func (s State) Terminal() bool {
	return s == StateDestroyed || s == StateFailed || s == StateInterrupted
}

// Valid reports whether s is a state this package defined.
func (s State) Valid() bool {
	_, ok := stateNames[s]
	return ok && s != StateInvalid
}

// ParseState maps ember's own on-disk spelling back to a State.
//
// Provider status strings do NOT come through here: each provider maps its own
// vocabulary in its map.go, because "EXITED" means different things to RunPod
// and to Docker and a shared parser would have to guess.
func ParseState(s string) (State, error) {
	for st, name := range stateNames {
		if name == s && st != StateInvalid {
			return st, nil
		}
	}
	return StateInvalid, fmt.Errorf("unknown node state %q", s)
}
