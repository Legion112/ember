package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

type stub struct {
	name node.ProviderID
	caps Caps
}

func (s stub) Name() node.ProviderID { return s.name }
func (s stub) Caps() Caps            { return s.caps }
func (s stub) Offers(context.Context, node.Filter) ([]node.Offer, error) {
	return nil, nil
}
func (s stub) Create(context.Context, node.CreateRequest) (node.Handle, error) {
	return node.Handle{}, nil
}
func (s stub) Inspect(context.Context, node.Handle) (node.Node, error) { return node.Node{}, nil }
func (s stub) Destroy(context.Context, node.Handle) error              { return nil }
func (s stub) List(context.Context) ([]node.Node, error)               { return nil, nil }

func offer(provider node.ProviderID, token string, price node.Rate) node.Offer {
	return node.Offer{
		Provider: provider, Token: node.OfferToken(token),
		GPU: "rtx4090", GPUCount: 1, VRAM: 24576, Price: price,
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()

	if err := r.Add(stub{name: "runpod"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(stub{name: "vastai"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(stub{name: "runpod"}); err == nil {
		t.Error("the same provider name was registered twice")
	}
	if err := r.Add(stub{name: ""}); err == nil {
		t.Error("a nameless provider was registered")
	}

	if _, err := r.Get("runpod"); err != nil {
		t.Errorf("Get: %v", err)
	}
	_, err := r.Get("nope")
	if err == nil {
		t.Fatal("Get returned an unregistered provider")
	}
	// The message lists what is configured, because "no provider called
	// runpdo" with no list is a typo the reader cannot see.
	if !strings.Contains(err.Error(), "runpod") || !strings.Contains(err.Error(), "vastai") {
		t.Errorf("the error should name the configured providers, got %v", err)
	}

	if got := r.Names(); len(got) != 2 || got[0] != "runpod" || got[1] != "vastai" {
		t.Errorf("Names() = %v, want sorted", got)
	}
	if got := r.All(); len(got) != 2 || got[0].Name() != "runpod" {
		t.Errorf("All() is not name-sorted: %v", got)
	}
}

// TestFilterOffers_EnforcesTheCeilingOnOurSide is a cost control, not tidiness.
// A marketplace answering a max-price query with a dearer offer cannot cause a
// spend when the ceiling is re-applied here.
func TestFilterOffers_EnforcesTheCeilingOnOurSide(t *testing.T) {
	in := []node.Offer{
		offer("vastai", "cheap", 100000),
		offer("vastai", "dear", 900000), // over the ceiling, as if the API ignored it
	}
	got := FilterOffers(node.Filter{MaxPrice: 500000}, in)
	if len(got) != 1 || got[0].Token != "cheap" {
		t.Fatalf("got %v, want only the offer within budget", got)
	}
}

func TestFilterOffers_SortsCheapestFirstAndBreaksTiesReproducibly(t *testing.T) {
	in := []node.Offer{
		offer("p", "b", 200000),
		offer("p", "a", 200000),
		offer("p", "c", 100000),
	}
	got := FilterOffers(node.Filter{}, in)
	if len(got) != 3 {
		t.Fatalf("got %d offers", len(got))
	}
	if got[0].Token != "c" {
		t.Errorf("cheapest first: got %v", got)
	}
	// Equal prices must order the same way every run, or an operator
	// comparing two runs cannot tell whether the price moved or the sort did.
	if got[1].Token != "a" || got[2].Token != "b" {
		t.Errorf("ties not broken on token: %v", got)
	}
}

func TestFilterOffers_DropsInvalidOffers(t *testing.T) {
	bad := offer("p", "", 1) // no token
	got := FilterOffers(node.Filter{}, []node.Offer{bad, offer("p", "ok", 1)})
	if len(got) != 1 || got[0].Token != "ok" {
		t.Fatalf("an unusable offer survived: %v", got)
	}
}

func TestFilterOffers_EmptyInput(t *testing.T) {
	if got := FilterOffers(node.Filter{}, nil); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestLabel_RoundTrips(t *testing.T) {
	id := node.NewRunID()
	deadline := time.Now().Add(time.Hour).Truncate(time.Second).UTC()

	gotID, gotDeadline, err := ParseLabel(Label(id, deadline))
	if err != nil {
		t.Fatal(err)
	}
	if gotID != id {
		t.Errorf("run id = %q, want %q", gotID, id)
	}
	if !gotDeadline.Equal(deadline) {
		t.Errorf("deadline = %v, want %v", gotDeadline, deadline)
	}
}

// TestParseLabel_IsStrict: `ember reap` destroys what this identifies, so a
// label ember did not write must never parse as one it did.
func TestParseLabel_IsStrict(t *testing.T) {
	good := Label(node.NewRunID(), time.Now())
	for name, in := range map[string]string{
		"empty":            "",
		"someone else's":   "myapp-0123456789abcdef-123",
		"no deadline":      "ember-0123456789abcdef",
		"bad run id":       "ember-nothex-123",
		"bad deadline":     "ember-0123456789abcdef-later",
		"extra field":      good + "-extra",
		"prefix only":      "ember",
		"substring of one": strings.TrimPrefix(good, "ember-"),
	} {
		if id, dl, err := ParseLabel(in); err == nil {
			t.Errorf("%s: ParseLabel(%q) = %q, %v — want an error", name, in, id, dl)
		}
	}
}
