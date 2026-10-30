package provider_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
)

func TestStoreLabelRoundTrip(t *testing.T) {
	for _, name := range []string{
		"models",
		"wan-2-2",     // dashes, which a Label-style 3-way split would shred
		"sd15",        //
		"a",           // the shortest useful name
		"Models_2024", // mixed case and an underscore
		"模型",          // a provider's name field is free text, so this is legal
	} {
		t.Run(name, func(t *testing.T) {
			if err := provider.ValidateStoreName(name); err != nil {
				t.Fatalf("ValidateStoreName(%q) = %v", name, err)
			}
			label := provider.StoreLabel(name)
			got, err := provider.ParseStoreLabel(label)
			if err != nil {
				t.Fatalf("ParseStoreLabel(%q) = %v", label, err)
			}
			if got != name {
				t.Errorf("round trip gave %q, want %q", got, name)
			}
		})
	}
}

func TestStoreLabel_DashesSurvive(t *testing.T) {
	// The reason this is a prefix and not a Label-style packed string.
	// ParseLabel splits on "-" and demands exactly three parts, so a store
	// called "wan-2-2" would arrive as six and parse as nothing ember owns --
	// which, for a function `ember store rm` acts on, means a store that
	// cannot be removed by the name that created it.
	const name = "wan-2-2"
	got, err := provider.ParseStoreLabel(provider.StoreLabel(name))
	if err != nil {
		t.Fatalf("ParseStoreLabel = %v", err)
	}
	if got != name {
		t.Errorf("got %q, want %q", got, name)
	}
}

func TestParseStoreLabel_RefusesWhatEmberDidNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label string
	}{
		{"somebody else's volume", "my-training-data"},
		{"a node label, not a store label", "ember-abc123-1730000000"},
		{"the prefix alone names no store", "ember-store-"},
		{"empty", ""},
		{"nearly right", "ember-stores-models"},
		{
			// Never a substring match. `ember store rm` destroys what this
			// identifies, and a volume that merely contains the prefix
			// somewhere is not ember's to delete.
			name: "the prefix in the middle", label: "backup-of-ember-store-models",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := provider.ParseStoreLabel(tc.label); err == nil {
				t.Errorf("ParseStoreLabel(%q) = %q, want an error", tc.label, got)
			}
		})
	}
}

func TestValidateStoreName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantMsg string
	}{
		{"empty", "", "empty"},
		{"leading space", " models", "leading or trailing space"},
		{"trailing space", "models ", "leading or trailing space"},
		{"slash", "a/b", "path separator"},
		{"backslash", `a\b`, "path separator"},
		{"nul", "a\x00b", "path separator"},
		{
			// A name carrying the prefix produces a label that reads back as
			// a different store than the one created, which is how `rm`
			// removes the wrong thing.
			name: "contains the prefix", in: "ember-store-models", wantMsg: "prefix ember uses",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := provider.ValidateStoreName(tc.in)
			if err == nil {
				t.Fatalf("ValidateStoreName(%q) = nil, want an error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("ValidateStoreName(%q) = %q, want it to mention %q", tc.in, err, tc.wantMsg)
			}
		})
	}
}

func TestValidateStoreName_RefusesWhatWouldNotRoundTrip(t *testing.T) {
	// The property the validator exists to protect: every name it accepts
	// must come back out of ParseStoreLabel unchanged.
	for _, name := range []string{"models", "wan-2-2", "a.b", "x_1", "UPPER"} {
		if err := provider.ValidateStoreName(name); err != nil {
			t.Fatalf("ValidateStoreName(%q) = %v", name, err)
		}
		got, err := provider.ParseStoreLabel(provider.StoreLabel(name))
		if err != nil || got != name {
			t.Errorf("%q round-tripped to (%q, %v)", name, got, err)
		}
	}
}

func TestStoreProvider_Lifecycle(t *testing.T) {
	p := providertest.WithStores("fake")
	ctx := t.Context()

	providertest.AssertStoreCapsMatch(t, p)

	got, err := p.StoreList(ctx)
	if err != nil {
		t.Fatalf("StoreList = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a fresh fake listed %d stores", len(got))
	}

	s, err := p.StoreCreate(ctx, "models", 40, "EU-RO-1")
	if err != nil {
		t.Fatalf("StoreCreate = %v", err)
	}
	if s.Name != "models" || s.SizeGiB != 40 || s.Region != "EU-RO-1" {
		t.Errorf("StoreCreate returned %+v", s)
	}
	if s.Handle.IsZero() {
		t.Error("StoreCreate returned a store with no handle")
	}

	if got, err = p.StoreList(ctx); err != nil || len(got) != 1 {
		t.Fatalf("StoreList = %v, %v; want one store", got, err)
	}

	// A list that aliased the fake's slice would let a caller mutate the
	// fake's state by accident, which is how a test starts passing for a
	// reason nobody wrote down.
	got[0].Name = "mutated"
	if again, _ := p.StoreList(ctx); again[0].Name != "models" {
		t.Error("StoreList handed back its own slice")
	}

	if err := p.StoreDestroy(ctx, s.Handle); err != nil {
		t.Fatalf("StoreDestroy = %v", err)
	}
	if got, _ = p.StoreList(ctx); len(got) != 0 {
		t.Errorf("after destroy, %d stores remain", len(got))
	}

	// Destroying what is already gone is success, as it is for a node.
	if err := p.StoreDestroy(ctx, s.Handle); err != nil {
		t.Errorf("a second StoreDestroy = %v, want success", err)
	}
}

func TestStoreProvider_DestroyKeepsTheOthers(t *testing.T) {
	// Removing one store must leave its neighbours alone. A destroy that
	// cleared the slice would pass every single-store test and silently wipe
	// the rest -- and for the real thing, "the rest" is weights somebody paid
	// to upload.
	p := providertest.WithStores("fake")
	ctx := t.Context()
	keep, err := p.StoreCreate(ctx, "keep", 10, "EU-RO-1")
	if err != nil {
		t.Fatal(err)
	}
	drop, err := p.StoreCreate(ctx, "drop", 10, "EU-RO-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreDestroy(ctx, drop.Handle); err != nil {
		t.Fatal(err)
	}
	got, err := p.StoreList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Handle != keep.Handle {
		t.Fatalf("after destroying %s the list is %+v, want only %s", drop.Name, got, keep.Name)
	}
}

func TestStoreProvider_Errors(t *testing.T) {
	// Failures are configured the way every other fake in this repository
	// configures them: Errors keyed by the call as Calls records it, or
	// FailOn for a substring.
	p := providertest.WithStores("fake")
	p.Errors["StoreCreate boom"] = errBoom
	if _, err := p.StoreCreate(t.Context(), "boom", 10, ""); err == nil {
		t.Error("StoreCreate ignored its configured error")
	}

	p.Errors["StoreDestroy v9"] = errBoom
	if err := p.StoreDestroy(t.Context(), node.StoreHandle{Provider: "fake", ID: "v9"}); err == nil {
		t.Error("StoreDestroy ignored its configured error")
	}

	p.FailOn = "StoreList"
	if _, err := p.StoreList(t.Context()); err == nil {
		t.Error("StoreList ignored FailOn")
	}
}

func TestAssertStoreCapsMatch_CatchesBothDirections(t *testing.T) {
	// The helper must fail for a provider that claims the capability without
	// implementing it, and for one that implements it without claiming it.
	// Both are silent in production, so both are asserted here against a
	// recording *testing.T rather than trusted.
	claimsOnly := providertest.NewRecordingProvider("claims")
	claimsOnly.Capabilities.Stores = true

	var fake testing.T
	providertest.AssertStoreCapsMatch(&fake, claimsOnly)
	if !fake.Failed() {
		t.Error("a provider claiming Caps.Stores without implementing StoreAPI was accepted")
	}

	silent := providertest.WithStores("silent")
	silent.Capabilities.Stores = false
	var fake2 testing.T
	providertest.AssertStoreCapsMatch(&fake2, silent)
	if !fake2.Failed() {
		t.Error("a provider implementing StoreAPI without claiming it was accepted")
	}
}

var errBoom = errors.New("boom")
