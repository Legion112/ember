// Package providertest holds the fakes every layer above a provider is tested
// against.
package providertest

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// RecordingProvider is a provider that does nothing and remembers everything.
//
// Field names follow go-tun's linux.RecordingRunner — Calls, Errors, FailOn —
// so the fakes in this repository all read the same way.
type RecordingProvider struct {
	mu sync.Mutex

	ProviderName node.ProviderID
	Capabilities provider.Caps

	// Calls records every operation, e.g. "Create" or "Destroy local/c1".
	Calls []string

	// Errors fails a specific call, keyed by the same string Calls records.
	Errors map[string]error

	// FailOn fails any call whose record contains this substring.
	FailOn string

	// OfferList is what Offers returns before filtering.
	OfferList []node.Offer

	// States is the sequence Inspect walks, one entry per call, repeating the
	// last. It is how "provisioning, provisioning, ready" is staged without
	// anything sleeping.
	States []node.State

	// HostKey is reported on a ready node. Leave it zero to exercise the
	// trust-on-first-use fallback.
	HostKey node.HostKey

	// Endpoint is reported once the node is ready. The zero value yields a
	// usable one.
	Endpoint node.Endpoint

	// Existing is what List returns.
	Existing []node.Node

	inspects  int
	created   []node.Handle
	destroyed []node.Handle
	nextID    int
}

// NewRecordingProvider returns a provider that creates nodes which become
// ready on the first Inspect.
func NewRecordingProvider(name node.ProviderID) *RecordingProvider {
	return &RecordingProvider{
		ProviderName: name,
		Errors:       map[string]error{},
		OfferList: []node.Offer{{
			Provider: name, Token: "offer-1", GPU: "rtx4090", GPUCount: 1,
			VRAM: 24576, Price: 340000,
		}},
		States: []node.State{node.StateReady},
	}
}

func (p *RecordingProvider) Name() node.ProviderID { return p.ProviderName }
func (p *RecordingProvider) Caps() provider.Caps   { return p.Capabilities }

// record notes a call and reports whether it was configured to fail. The
// caller holds p.mu.
func (p *RecordingProvider) record(op, arg string) error {
	call := strings.TrimSpace(op + " " + arg)
	p.Calls = append(p.Calls, call)
	if err, ok := p.Errors[call]; ok {
		return err
	}
	if p.FailOn != "" && strings.Contains(call, p.FailOn) {
		return fmt.Errorf("recording provider refusing %q (FailOn %q)", call, p.FailOn)
	}
	return nil
}

// Recorded returns every call made so far.
func (p *RecordingProvider) Recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.Calls...)
}

// Live reports handles created but not destroyed. A test asserting that no
// node leaked asks this.
func (p *RecordingProvider) Live() []node.Handle {
	p.mu.Lock()
	defer p.mu.Unlock()
	gone := map[string]bool{}
	for _, h := range p.destroyed {
		gone[h.ID] = true
	}
	var out []node.Handle
	for _, h := range p.created {
		if !gone[h.ID] {
			out = append(out, h)
		}
	}
	return out
}

func (p *RecordingProvider) Offers(_ context.Context, f node.Filter) ([]node.Offer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("Offers", ""); err != nil {
		return nil, err
	}
	return provider.FilterOffers(f, p.OfferList), nil
}

func (p *RecordingProvider) Create(_ context.Context, req node.CreateRequest) (node.Handle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("Create", string(req.RunID)); err != nil {
		return node.Handle{}, err
	}
	p.nextID++
	h := node.Handle{Provider: p.ProviderName, ID: fmt.Sprintf("n%d", p.nextID)}
	p.created = append(p.created, h)
	return h, nil
}

func (p *RecordingProvider) Inspect(_ context.Context, h node.Handle) (node.Node, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("Inspect", h.ID); err != nil {
		return node.Node{}, err
	}

	state := node.StateReady
	if len(p.States) > 0 {
		i := p.inspects
		if i >= len(p.States) {
			i = len(p.States) - 1
		}
		state = p.States[i]
	}
	p.inspects++

	n := node.Node{Handle: h, State: state, HostKey: p.HostKey}
	if state != node.StateReady {
		return n, nil
	}
	ep := p.Endpoint
	if ep.Host == "" {
		ep = node.Endpoint{Host: "127.0.0.1", Port: 2222, User: "root"}
	}
	return n.WithEndpoint(ep), nil
}

func (p *RecordingProvider) Destroy(_ context.Context, h node.Handle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("Destroy", h.ID); err != nil {
		return err
	}
	p.destroyed = append(p.destroyed, h)
	return nil
}

func (p *RecordingProvider) List(_ context.Context) ([]node.Node, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("List", ""); err != nil {
		return nil, err
	}
	return p.Existing, nil
}

// DryProvider forwards only the operations that cannot spend money.
//
// It is an allow-list, not a deny-list: a method added to provider.Provider
// later is refused by default rather than silently forwarded. go-tun makes the
// same choice for its dry-run runner, and the stakes are higher here — there
// an unrecognised command reconfigures a router, here it rents a GPU.
type DryProvider struct {
	Inner provider.Provider
	Out   io.Writer
}

func (d DryProvider) Name() node.ProviderID { return d.Inner.Name() }
func (d DryProvider) Caps() provider.Caps   { return d.Inner.Caps() }

func (d DryProvider) Offers(ctx context.Context, f node.Filter) ([]node.Offer, error) {
	return d.Inner.Offers(ctx, f)
}
func (d DryProvider) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	return d.Inner.Inspect(ctx, h)
}
func (d DryProvider) List(ctx context.Context) ([]node.Node, error) { return d.Inner.List(ctx) }

func (d DryProvider) Create(_ context.Context, req node.CreateRequest) (node.Handle, error) {
	d.say("would create a node from offer %s for run %s (deadline %s)",
		req.Offer.Token, req.RunID, req.Deadline.Format("15:04:05"))
	return node.Handle{}, fmt.Errorf("dry run: refusing to create a node")
}

func (d DryProvider) Destroy(_ context.Context, h node.Handle) error {
	d.say("would destroy %s", h)
	return nil
}

func (d DryProvider) say(format string, a ...any) {
	if d.Out == nil {
		return
	}
	_, _ = fmt.Fprintf(d.Out, "dry-run: "+format+"\n", a...)
}

var (
	_ provider.Provider = (*RecordingProvider)(nil)
	_ provider.Provider = DryProvider{}
)
