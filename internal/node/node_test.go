package node

import (
	"strings"
	"testing"
	"time"
)

func readyish() Node {
	return Node{
		Handle:   Handle{Provider: "runpod", ID: "abc123"},
		State:    StateReady,
		Offer:    validOffer(),
		Deadline: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}.WithEndpoint(Endpoint{Host: "1.2.3.4", Port: 22, User: "root"})
}

// TestReady_RefusesANodeWithNoEndpoint is the invariant the whole type exists
// for. RunPod reports a pod as RUNNING before it has published a public
// mapping for 22/tcp; for the first few seconds it always does. Without this
// check that window surfaces as a nil dereference inside the forwarder, which
// is a long way from the fact that caused it.
func TestReady_RefusesANodeWithNoEndpoint(t *testing.T) {
	n := Node{Handle: Handle{Provider: "runpod", ID: "abc"}, State: StateReady}
	if _, err := Ready(n); err == nil {
		t.Fatal("Ready() = nil error for a node with no endpoint")
	} else if !strings.Contains(err.Error(), "no ssh endpoint") {
		t.Errorf("error should name the missing endpoint, got %v", err)
	}
}

func TestReady_RefusesEveryNonReadyState(t *testing.T) {
	for s := StateInvalid; s <= StateDestroyed; s++ {
		if s == StateReady {
			continue
		}
		n := readyish()
		n.State = s
		if _, err := Ready(n); err == nil {
			t.Errorf("Ready() accepted a node in state %v", s)
		}
	}
}

func TestReady_RefusesAnUndialableEndpoint(t *testing.T) {
	// Port 0 is the specific trap: it is the zero value, it means "any" to a
	// listener and "refused" to a dialler.
	n := Node{Handle: Handle{Provider: "p", ID: "i"}, State: StateReady}.
		WithEndpoint(Endpoint{Host: "1.2.3.4", Port: 0, User: "root"})
	if _, err := Ready(n); err == nil {
		t.Fatal("Ready() accepted an endpoint on port 0")
	}
}

func TestReady_AcceptsAndExposes(t *testing.T) {
	n := readyish()
	r, err := Ready(n)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Endpoint(); got.Host != "1.2.3.4" || got.Port != 22 || got.User != "root" {
		t.Errorf("Endpoint() = %v", got)
	}
	if r.Handle() != n.Handle {
		t.Errorf("Handle() = %v", r.Handle())
	}
	if r.Node().State != StateReady {
		t.Errorf("Node() lost the state")
	}
	if !r.HostKey().IsZero() {
		t.Errorf("HostKey() should round-trip the node's key")
	}
}

func TestNode_EndpointReportsAbsence(t *testing.T) {
	if _, ok := (Node{}).Endpoint(); ok {
		t.Fatal("a Node built without WithEndpoint must report no endpoint")
	}
	if _, ok := readyish().Endpoint(); !ok {
		t.Fatal("WithEndpoint must be visible through Endpoint")
	}
}

// TestNode_ExpiredTreatsAMissingDeadlineAsExpired: every node ember creates
// gets a deadline, so a zero one means either a node ember did not create or a
// record that lost it — both of which the operator needs shown, not hidden.
func TestNode_ExpiredTreatsAMissingDeadlineAsExpired(t *testing.T) {
	now := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	n := Node{State: StateReady}
	if !n.Expired(now) {
		t.Fatal("a billable node with no deadline must read as expired")
	}
}

func TestNode_Expired(t *testing.T) {
	deadline := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	before := deadline.Add(-time.Minute)
	after := deadline.Add(time.Minute)

	live := Node{State: StateReady, Deadline: deadline}
	if live.Expired(before) {
		t.Error("not expired before the deadline")
	}
	if !live.Expired(after) {
		t.Error("expired after the deadline")
	}
	if live.Expired(deadline) {
		t.Error("the deadline instant itself is not yet past")
	}

	dead := Node{State: StateDestroyed, Deadline: deadline}
	if dead.Expired(after) {
		t.Error("a destroyed node is never expired: there is nothing left to reap")
	}
}

func TestHandle(t *testing.T) {
	if !(Handle{}).IsZero() {
		t.Error("the zero Handle must report IsZero")
	}
	h := Handle{Provider: "vastai", ID: "99"}
	if h.IsZero() {
		t.Error("a populated Handle must not report IsZero")
	}
	if got, want := h.String(), "vastai/99"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
