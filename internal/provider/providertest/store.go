package providertest

import (
	"context"
	"fmt"
	"testing"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// RecordingStores is a provider.StoreAPI that does nothing and remembers
// everything, embedded in a RecordingProvider to give it stores.
//
// Separate from RecordingProvider's own fields so that a fake *without*
// stores stays expressible: Caps.Stores and the StoreAPI implementation must
// be able to disagree in a test, because that disagreement is the bug
// AssertStoreCapsMatch exists to catch in real providers.
type RecordingStores struct {
	// Stores is what StoreList returns, and what Create and Destroy mutate.
	//
	// Failures go through the embedded RecordingProvider's Errors and FailOn,
	// keyed as Calls records them ("StoreCreate models"). A second error map
	// here would be a second thing to learn for no gain, and go-tun's
	// RecordingRunner set the convention these field names follow.
	Stores []node.Store

	nextStoreID int
}

// WithStores returns a RecordingProvider that claims and implements stores.
//
// The pairing is the point: a caller cannot get the capability without the
// implementation, which is the invariant AssertStoreCapsMatch checks on the
// real providers.
func WithStores(name node.ProviderID, stores ...node.Store) *StoreProvider {
	p := NewRecordingProvider(name)
	p.Capabilities.Stores = true
	return &StoreProvider{
		RecordingProvider: p,
		RecordingStores:   RecordingStores{Stores: stores},
	}
}

// StoreProvider is a RecordingProvider that also has stores.
type StoreProvider struct {
	*RecordingProvider
	RecordingStores
}

func (p *StoreProvider) StoreCreate(_ context.Context, name string, sizeGiB int, region string) (node.Store, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("StoreCreate", name); err != nil {
		return node.Store{}, err
	}
	p.nextStoreID++
	s := node.Store{
		Handle:  node.StoreHandle{Provider: p.ProviderName, ID: node.StoreID(fmt.Sprintf("vol%d", p.nextStoreID))},
		Name:    name,
		SizeGiB: sizeGiB,
		Region:  region,
	}
	p.Stores = append(p.Stores, s)
	return s, nil
}

func (p *StoreProvider) StoreList(_ context.Context) ([]node.Store, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("StoreList", ""); err != nil {
		return nil, err
	}
	return append([]node.Store(nil), p.Stores...), nil
}

func (p *StoreProvider) StoreDestroy(_ context.Context, h node.StoreHandle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("StoreDestroy", string(h.ID)); err != nil {
		return err
	}
	out := p.Stores[:0]
	for _, s := range p.Stores {
		if s.Handle != h {
			out = append(out, s)
		}
	}
	// Not found counts as success, as node destruction does: a store that is
	// already gone is the state the caller asked for.
	p.Stores = out
	return nil
}

// AssertStoreCapsMatch checks that a provider's Caps.Stores matches whether it
// actually implements StoreAPI.
//
// Every provider's own test calls this. The two can drift in both directions
// and both are silent: a provider that implements StoreAPI without setting the
// capability can never be asked for a store, and one that sets the capability
// without implementing it fails a type assertion at the moment a run was about
// to start -- after the operator has already decided to spend money.
func AssertStoreCapsMatch(t *testing.T, p provider.Provider) {
	t.Helper()
	_, implements := p.(provider.StoreAPI)
	claims := p.Caps().Stores
	switch {
	case claims && !implements:
		t.Errorf("%s says Caps.Stores but does not implement provider.StoreAPI; "+
			"ember would decide to use a store and then fail the type assertion", p.Name())
	case implements && !claims:
		t.Errorf("%s implements provider.StoreAPI but does not say Caps.Stores, "+
			"so nothing will ever ask it for one", p.Name())
	}
}

var (
	_ provider.Provider = (*StoreProvider)(nil)
	_ provider.StoreAPI = (*StoreProvider)(nil)
)
