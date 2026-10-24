package providertest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// TestDryProvider_RefusesToSpendMoney is the point of the type. A dry run that
// created a node would be the most expensive bug in this program.
func TestDryProvider_RefusesToSpendMoney(t *testing.T) {
	inner := NewRecordingProvider("test")
	var out strings.Builder
	d := DryProvider{Inner: inner, Out: &out}

	req := node.CreateRequest{
		Offer:    node.Offer{Provider: "test", Token: "offer-1"},
		RunID:    node.NewRunID(),
		Deadline: time.Now().Add(time.Hour),
	}
	if _, err := d.Create(t.Context(), req); err == nil {
		t.Fatal("a dry run created a node")
	}
	if len(inner.Recorded()) != 0 {
		t.Fatalf("the dry run reached the real provider: %v", inner.Recorded())
	}
	if !strings.Contains(out.String(), "would create") {
		t.Errorf("a dry run must say what it would have done, got %q", out.String())
	}
}

func TestDryProvider_ForwardsOnlyTheReadOnlyOperations(t *testing.T) {
	inner := NewRecordingProvider("test")
	d := DryProvider{Inner: inner}

	if _, err := d.Offers(t.Context(), node.Filter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect(t.Context(), node.Handle{ID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.Name() != "test" {
		t.Errorf("Name() = %q", d.Name())
	}
	_ = d.Caps()

	got := strings.Join(inner.Recorded(), "|")
	for _, want := range []string{"Offers", "Inspect n1", "List"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was not forwarded: %q", want, got)
		}
	}
	// And nothing that mutates reached the inner provider.
	if strings.Contains(got, "Create") || strings.Contains(got, "Destroy") {
		t.Errorf("a mutating call was forwarded: %q", got)
	}
}

func TestDryProvider_DestroyIsAnnouncedNotPerformed(t *testing.T) {
	inner := NewRecordingProvider("test")
	var out strings.Builder
	d := DryProvider{Inner: inner, Out: &out}

	if err := d.Destroy(t.Context(), node.Handle{Provider: "test", ID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if len(inner.Recorded()) != 0 {
		t.Error("destroy reached the real provider during a dry run")
	}
	if !strings.Contains(out.String(), "would destroy") {
		t.Errorf("got %q", out.String())
	}
}

func TestDryProvider_SurvivesWithoutAWriter(t *testing.T) {
	d := DryProvider{Inner: NewRecordingProvider("test")} // Out is nil
	if _, err := d.Create(t.Context(), node.CreateRequest{}); err == nil {
		t.Fatal("a dry run with no writer still must not create")
	}
	if err := d.Destroy(t.Context(), node.Handle{}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingProvider_StatesAdvanceThenRepeat(t *testing.T) {
	p := NewRecordingProvider("test")
	p.States = []node.State{node.StatePending, node.StateReady}
	h := node.Handle{Provider: "test", ID: "n1"}

	first, err := p.Inspect(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != node.StatePending {
		t.Errorf("first Inspect = %v", first.State)
	}
	for range 3 {
		n, err := p.Inspect(t.Context(), h)
		if err != nil {
			t.Fatal(err)
		}
		// The last entry repeats, so a test does not have to predict how many
		// times the code under test will poll.
		if n.State != node.StateReady {
			t.Fatalf("later Inspect = %v, want the last state to repeat", n.State)
		}
	}
}

func TestRecordingProvider_LiveTracksCreatesAndDestroys(t *testing.T) {
	p := NewRecordingProvider("test")
	h, err := p.Create(t.Context(), node.CreateRequest{RunID: node.NewRunID()})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Live()) != 1 {
		t.Fatal("a created node is not live")
	}
	if err := p.Destroy(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if len(p.Live()) != 0 {
		t.Fatal("a destroyed node is still live; every leak assertion above this would pass wrongly")
	}
}

func TestRecordingProvider_FaultInjection(t *testing.T) {
	p := NewRecordingProvider("test")
	want := errors.New("boom")
	p.Errors["List"] = want
	if _, err := p.List(t.Context()); !errors.Is(err, want) {
		t.Errorf("Errors did not fire: %v", err)
	}

	p = NewRecordingProvider("test")
	p.FailOn = "Offers"
	if _, err := p.Offers(t.Context(), node.Filter{}); err == nil {
		t.Error("FailOn did not fire")
	}
}

func TestRecordingProvider_ListReturnsWhatItWasGiven(t *testing.T) {
	p := NewRecordingProvider("test")
	p.Existing = []node.Node{{Handle: node.Handle{Provider: "test", ID: "orphan"}}}
	got, err := p.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Handle.ID != "orphan" {
		t.Errorf("List = %v", got)
	}
}
