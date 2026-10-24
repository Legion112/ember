package node

import (
	"fmt"
	"time"
)

// ProviderID names a provider ("runpod", "vastai", "local").
type ProviderID string

// Handle identifies one node at one provider.
//
// Create returns a Handle and not a Node on purpose. A machine that was
// created a millisecond ago is never ready, has no SSH endpoint, and may not
// even have an IP yet; returning a Node would hand the caller a value whose
// Endpoint is nil and leave it to them to remember that. With a Handle, the
// only way to obtain something with an endpoint is Inspect followed by Ready,
// and both of those can refuse.
type Handle struct {
	Provider ProviderID
	ID       string
}

func (h Handle) String() string { return string(h.Provider) + "/" + h.ID }

// IsZero reports whether h names nothing.
func (h Handle) IsZero() bool { return h.Provider == "" && h.ID == "" }

// Node is what a provider says about a machine right now.
//
// endpoint is unexported so that the only way to reach a dialable address is
// through Ready, which checks the state first. See ReadyNode.
type Node struct {
	Handle Handle

	// RunID is ember's own identifier for the run that created this node,
	// parsed back out of the provider-side label.
	//
	// It is here because the label is the source of truth and the local
	// journal is a cache. A process killed between Create returning and the
	// node id being written leaves a record that cannot name its node -- and
	// without this field the node and its record are two unrelated findings
	// that nothing can reconcile or reap, which defeats the entire purpose of
	// writing the record before the create call.
	RunID RunID

	State    State
	Offer    Offer
	HostKey  HostKey
	Created  time.Time
	Deadline time.Time
	Message  string // the provider's own status detail, kept for failure reports

	endpoint *Endpoint
	exposed  []Exposed
}

// WithEndpoint returns a copy of n carrying ep.
//
// Providers build Nodes through this rather than through a struct literal,
// which is why endpoint can stay unexported without making the providers
// reach into this package.
func (n Node) WithEndpoint(ep Endpoint) Node {
	n.endpoint = &ep
	return n
}

// Endpoint reports the node's SSH endpoint, if the provider has published one.
func (n Node) Endpoint() (Endpoint, bool) {
	if n.endpoint == nil {
		return Endpoint{}, false
	}
	return *n.endpoint, true
}

// Expired reports whether the node is past its deadline and might still be
// charging.
//
// A node with no deadline counts as expired. Every node ember creates is given
// one by NewCreateRequest, so a zero deadline means either a node ember did
// not create or a record that lost its deadline, and both of those are things
// the operator needs shown rather than hidden.
func (n Node) Expired(now time.Time) bool {
	if !n.State.Billable() {
		return false
	}
	if n.Deadline.IsZero() {
		return true
	}
	return now.After(n.Deadline)
}

// ReadyNode is a Node proved to be running with a usable SSH endpoint.
//
// Everything that needs to talk to a machine — the tunnel, the workload
// runner, the output transfer — takes a ReadyNode. Passing a node that is
// still provisioning is then a compile error rather than a nil dereference at
// the point where the forward is set up, which is where it used to surface:
// "RUNNING" from the RunPod API does not imply that a public mapping for
// 22/tcp exists yet, and for the first few seconds it usually does not.
type ReadyNode struct {
	n  Node
	ep Endpoint
}

// Ready is the only constructor for ReadyNode.
func Ready(n Node) (ReadyNode, error) {
	if n.State != StateReady {
		return ReadyNode{}, fmt.Errorf("node %s is %s, not ready", n.Handle, n.State)
	}
	ep, ok := n.Endpoint()
	if !ok {
		return ReadyNode{}, fmt.Errorf("node %s reports ready but has no ssh endpoint", n.Handle)
	}
	if err := ep.Validate(); err != nil {
		return ReadyNode{}, fmt.Errorf("node %s: %w", n.Handle, err)
	}
	return ReadyNode{n: n, ep: ep}, nil
}

func (r ReadyNode) Node() Node         { return r.n }
func (r ReadyNode) Handle() Handle     { return r.n.Handle }
func (r ReadyNode) Endpoint() Endpoint { return r.ep }
func (r ReadyNode) HostKey() HostKey   { return r.n.HostKey }
