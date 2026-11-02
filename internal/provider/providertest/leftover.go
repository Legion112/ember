package providertest

import (
	"context"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// RecordingLeftovers is a provider.LeftoverAPI that does nothing and
// remembers everything, embedded in a RecordingProvider to give it leftovers.
//
// Separate from RecordingProvider's own fields for the reason RecordingStores
// is: a fake *without* leftovers has to stay expressible, because the
// disagreement between Caps.Leftovers and the interface is exactly what
// AssertLeftoverCapsMatch exists to catch in a real provider.
type RecordingLeftovers struct {
	// Leftovers is what LeftoverList returns, and what LeftoverDestroy
	// removes from.
	//
	// Failures go through the embedded RecordingProvider's Errors and FailOn,
	// keyed as Calls records them ("LeftoverDestroy net1"), so there is one
	// error map to learn rather than two.
	Leftovers []node.Leftover
}

// WithLeftovers returns a RecordingProvider that claims and implements
// leftovers.
//
// The pairing is the point, as it is for WithStores: a caller cannot get the
// capability without the implementation.
func WithLeftovers(name node.ProviderID, ls ...node.Leftover) *LeftoverProvider {
	p := NewRecordingProvider(name)
	p.Capabilities.Leftovers = true
	return &LeftoverProvider{
		RecordingProvider:  p,
		RecordingLeftovers: RecordingLeftovers{Leftovers: ls},
	}
}

// LeftoverProvider is a RecordingProvider that also has leftovers.
type LeftoverProvider struct {
	*RecordingProvider
	RecordingLeftovers
}

func (p *LeftoverProvider) LeftoverList(_ context.Context) ([]node.Leftover, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("LeftoverList", ""); err != nil {
		return nil, err
	}
	return append([]node.Leftover(nil), p.Leftovers...), nil
}

func (p *LeftoverProvider) LeftoverDestroy(_ context.Context, h node.LeftoverHandle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("LeftoverDestroy", string(h.ID)); err != nil {
		return err
	}
	// Removed from the slice rather than counted, so a test can assert on what
	// is *left* -- which is the question a sweep is about. A fake that only
	// counted calls would agree with a sweep that removed the same network
	// twice and missed another.
	out := p.Leftovers[:0]
	for _, l := range p.Leftovers {
		if l.Handle != h {
			out = append(out, l)
		}
	}
	p.Leftovers = out
	return nil
}

var (
	_ provider.Provider    = (*LeftoverProvider)(nil)
	_ provider.LeftoverAPI = (*LeftoverProvider)(nil)
)
