package webapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/node"
)

// TestConfig_PublishesTheCardsAProviderHolds.
//
// THE FIELD THIS REPLACES WAS A PLACEHOLDER. The page's card input said `any`
// and nothing else: the cards a provider actually sells were knowable and were
// not shown, so an operator who wanted one pressed `what is available`, read a
// table and typed back what it said -- and one who typed a card RunPod does not
// sell was accepted here and refused after. The list is collected once when
// `ember serve` starts, because the set of card MODELS does not change for the
// life of a process; what does change is what each costs and whether it is free
// right now, which is what that button still answers.
func TestConfig_PublishesTheCardsAProviderHolds(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Cards = []app.Card{
			{Provider: "runpod", ID: node.GPUModel("a100-80g"), Name: "A100 80GB", VRAMGiB: 80},
			{Provider: "runpod", ID: node.GPUModel("rtx4090"), Name: "RTX 4090", VRAMGiB: 24},
		}
	})
	srv := live(t, h)

	var v struct {
		Cards []struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
			Name     string `json:"name"`
			VRAMGiB  int    `json:"vram_gib"`
		} `json:"cards"`
		CardErrors map[string]string `json:"card_errors"`
	}
	res := h.wire(t, srv, "GET", "/api/config", "", nil)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Status, res.Body)
	}
	if err := json.Unmarshal(res.Body, &v); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Body)
	}
	if len(v.Cards) != 2 {
		t.Fatalf("got %d card(s), want 2:\n%s", len(v.Cards), res.Body)
	}
	if v.Cards[0].ID != "a100-80g" || v.Cards[0].VRAMGiB != 80 {
		t.Errorf("first card = %+v, want the A100 with its memory", v.Cards[0])
	}
	if v.Cards[0].Name == "" {
		t.Error("a card reached the page with no name for a person to recognise")
	}
}

// TestConfig_SendsTheCardListAsAnEmptyListAndNeverNull.
//
// Go marshals a nil slice as `null` and a nil map as `null`, serde's `default`
// covers a field that is MISSING rather than one that is present and null, and
// this repository has already lost a whole websocket envelope to the
// distinction. A server that was asked for no cards -- every test harness, and
// every `ember serve` whose providers keep no catalogue -- has to answer `[]`
// and `{}`.
func TestConfig_SendsTheCardListAsAnEmptyListAndNeverNull(t *testing.T) {
	h := newHarness(t)
	srv := live(t, h)

	res := h.wire(t, srv, "GET", "/api/config", "", nil)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Status, res.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(res.Body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"cards", "card_errors"} {
		got, ok := raw[k]
		if !ok {
			t.Errorf("%s is missing from the answer entirely", k)
			continue
		}
		if string(got) == "null" {
			t.Errorf("%s is null, which fails the client's whole response rather than one "+
				"field of it", k)
		}
	}
}

// TestConfig_SaysWhichProviderCouldNotBeAskedForItsCards.
//
// A PROVIDER THAT COULD NOT BE REACHED IS A FINDING, which is this
// repository's rule everywhere a provider is reached and is the half that
// keeps a short list honest. `ember serve` starts anyway -- the local provider
// needs no credential and a queue draining onto it is unaffected -- so without
// this the only symptom would be a card field that quietly offered nothing,
// indistinguishable from a provider that has nothing.
func TestConfig_SaysWhichProviderCouldNotBeAskedForItsCards(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.CardErrors = map[node.ProviderID]string{
			"runpod": "dial tcp: lookup rest.runpod.io: no such host",
		}
	})
	srv := live(t, h)

	var v struct {
		CardErrors map[string]string `json:"card_errors"`
	}
	res := h.wire(t, srv, "GET", "/api/config", "", nil)
	if err := json.Unmarshal(res.Body, &v); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Body)
	}
	why, ok := v.CardErrors["runpod"]
	if !ok {
		t.Fatalf("the unreachable provider is not named: %s", res.Body)
	}
	if why == "" {
		t.Error("the provider is named with no reason, so the page can say only that it failed")
	}
}
