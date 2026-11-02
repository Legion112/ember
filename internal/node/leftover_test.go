package node

import (
	"testing"
	"time"
)

func TestLeftoverHandle_StringAndZero(t *testing.T) {
	h := LeftoverHandle{Provider: "local", ID: "net1"}
	if got := h.String(); got != "local/net1" {
		t.Errorf("String() = %q, want local/net1", got)
	}
	if h.IsZero() {
		t.Error("a handle naming a network reported itself as zero")
	}
	if !(LeftoverHandle{}).IsZero() {
		t.Error("the zero handle did not report itself as zero")
	}
	// Half a handle is not zero. A provider with no id would otherwise read as
	// "names nothing" and skip the guard in LeftoverDestroy, which is the one
	// place an empty id is turned into an error rather than a call.
	if (LeftoverHandle{Provider: "local"}).IsZero() {
		t.Error("a handle with a provider and no id reported itself as zero")
	}
}

// TestLeftover_ExpiredTreatsAnUnreadableLabelAsPast is the decision in
// node.Leftover's own comment, asserted rather than written down twice.
//
// A network wearing ember's kind label whose ember label could not be parsed
// has a zero deadline. Refusing to touch it -- the other reading of "unknown"
// -- would mean a network labelled in some earlier spelling leaks for good,
// because nothing else in ember will ever look at it again.
func TestLeftover_ExpiredTreatsAnUnreadableLabelAsPast(t *testing.T) {
	now := time.Now()
	if !(Leftover{}).Expired(now) {
		t.Error("a leftover with no readable deadline was treated as still live, so nothing would ever remove it")
	}
}

func TestLeftover_ExpiredIsBoundedByTheDeadline(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	// The create window: NetworkCreate runs before ContainerCreate, so for a
	// moment a live run's network has no node container and looks exactly
	// like a leftover. A deadline in the future is what tells the two apart,
	// and getting this wrong deletes a working run's network.
	if (Leftover{Deadline: now.Add(time.Hour)}).Expired(now) {
		t.Error("a leftover inside its deadline was reported as expired; reap would remove a live run's network")
	}
	if !(Leftover{Deadline: now.Add(-time.Second)}).Expired(now) {
		t.Error("a leftover past its deadline was not reported as expired")
	}
	// The boundary belongs to the past, as node.Expired treats a deadline
	// that has exactly arrived: the TTL is over.
	if !(Leftover{Deadline: now}).Expired(now) {
		t.Error("a leftover at exactly its deadline was not reported as expired")
	}
}
