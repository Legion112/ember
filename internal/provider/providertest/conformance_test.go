package providertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// An assertion suite is the easiest kind of code to write so that it cannot
// fail, and a regression test that has never failed proves nothing. Both
// providers passed RunConformance on its first run, which means nothing on its
// own -- so every check below is also run against a provider built to violate
// exactly that one invariant, and has to go red.
//
// The reference provider here is not RecordingProvider. RecordingProvider
// answers Inspect for any handle it is given and returns only what Existing
// was set to from List, which is right for the layers above a provider -- a
// test staging "provisioning, provisioning, ready" does not want a machine
// registry -- and is not what a real provider does. Making it conformant would
// change a fake a dozen packages depend on; reproducing it here is a type
// nothing else uses, whose whole job is to be correct so that the broken
// variants differ from it in one place each.

// conformer is a provider written to pass RunConformance.
type conformer struct {
	// caps is a field rather than a literal so a variant can clear one flag
	// without reimplementing the method.
	caps provider.Caps

	mu   sync.Mutex
	next int

	// machines is what exists right now, keyed by handle id. A real provider's
	// Inspect and List both read the provider's own view of the world rather
	// than a slice a test handed over, which is the difference that makes the
	// destroy and label checks mean anything here.
	machines map[string]*conformerMachine

	// leftovers are the objects a run created beside its machine, keyed by
	// their own id. They outlive the machine, which is the entire reason
	// Destroy takes a run id rather than reading one off the thing it is
	// destroying.
	leftovers map[string]node.RunID
}

type conformerMachine struct {
	runID    node.RunID
	deadline time.Time
	hostKey  node.HostKey
}

func newConformer() *conformer {
	return &conformer{
		caps: provider.Caps{
			HostKeyFromAPI: true,
			PublicSSH:      true,
			Leftovers:      true,
			MinBilling:     0,
		},
		machines:  map[string]*conformerMachine{},
		leftovers: map[string]node.RunID{},
	}
}

const conformerName node.ProviderID = "conformer"

func (c *conformer) Name() node.ProviderID { return conformerName }
func (c *conformer) Caps() provider.Caps   { return c.caps }

// offerList is the catalogue, before filtering. Separate from Offers so a
// variant can hand back the unfiltered list and be wrong in exactly that way.
func (c *conformer) offerList() []node.Offer {
	return []node.Offer{{
		Provider: conformerName, Token: "offer-1", GPU: "rtx4090", GPUCount: 1,
		VRAM: 24576, Price: 340000, Region: "conformance-1",
	}}
}

func (c *conformer) Offers(_ context.Context, f node.Filter) ([]node.Offer, error) {
	return provider.FilterOffers(f, c.offerList()), nil
}

func (c *conformer) Create(_ context.Context, req node.CreateRequest) (node.Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := fmt.Sprintf("m%d", c.next)
	c.machines[id] = &conformerMachine{
		runID:    req.RunID,
		deadline: req.Deadline,
		// The node adopts the key ember injected, which is what a provider
		// that supports injection does.
		hostKey: req.HostKeyPublic,
	}
	// Created beside the machine and outliving it, the way local.Create makes
	// a bridge network before the container.
	c.leftovers[id+"-net"] = req.RunID
	return node.Handle{Provider: conformerName, ID: id}, nil
}

// nodeFor is the one place a machine becomes a node, so Inspect and List
// cannot disagree about a run id or a deadline.
func (c *conformer) nodeFor(id string, m *conformerMachine) node.Node {
	n := node.Node{
		Handle:   node.Handle{Provider: conformerName, ID: id},
		RunID:    m.runID,
		Deadline: m.deadline,
		State:    node.StateReady,
		HostKey:  m.hostKey,
		Offer:    c.offerList()[0],
	}
	n = n.WithExposed(c.exposed())
	return n.WithEndpoint(node.Endpoint{Host: "203.0.113.7", Port: 40022, User: "root"})
}

// exposed is what this provider says it published, derived from its own
// capability so that a variant which clears PublicSSH without changing its
// behaviour is the mismatch the check is about.
func (c *conformer) exposed() []node.Exposed {
	return []node.Exposed{{Port: 22, Proto: node.ProtoTCP, URL: "203.0.113.7:40022"}}
}

func (c *conformer) Inspect(_ context.Context, h node.Handle) (node.Node, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.machines[h.ID]
	if !ok {
		return node.Node{}, fmt.Errorf("machine %s: %w", h.ID, node.ErrNotFound)
	}
	return c.nodeFor(h.ID, m), nil
}

func (c *conformer) List(_ context.Context) ([]node.Node, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]node.Node, 0, len(c.machines))
	for id, m := range c.machines {
		out = append(out, c.nodeFor(id, m))
	}
	return out, nil
}

func (c *conformer) Destroy(_ context.Context, h node.Handle, runID node.RunID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The machine's own record wins where there is one and the argument fills
	// the gap it leaves, which is the ordering local.Destroy uses: the machine
	// cannot be wrong about which run it belongs to and the argument can, and
	// believing a wrong argument removes a live run's network.
	found := runID
	if m, ok := c.machines[h.ID]; ok {
		found = m.runID
	}
	// A missing machine is success: the goal of destroy is that it is gone.
	delete(c.machines, h.ID)
	if found == "" {
		return nil
	}
	for id, r := range c.leftovers {
		if r == found {
			delete(c.leftovers, id)
		}
	}
	return nil
}

func (c *conformer) LeftoverList(_ context.Context) ([]node.Leftover, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	liveRuns := map[node.RunID]bool{}
	for _, m := range c.machines {
		liveRuns[m.runID] = true
	}
	var out []node.Leftover
	for id, r := range c.leftovers {
		if liveRuns[r] {
			continue
		}
		out = append(out, node.Leftover{
			Handle: node.LeftoverHandle{Provider: conformerName, ID: node.LeftoverID(id)},
			Kind:   "network", Name: id, RunID: r,
		})
	}
	return out, nil
}

func (c *conformer) LeftoverDestroy(_ context.Context, h node.LeftoverHandle) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.leftovers, string(h.ID))
	return nil
}

var (
	_ provider.Provider    = (*conformer)(nil)
	_ provider.LeftoverAPI = (*conformer)(nil)
)

// TestRunConformance_AProviderWrittenToPassDoes is the control.
//
// Without it, a suite where every check errored unconditionally would still
// satisfy the table below, which only asks that a defect be caught.
func TestRunConformance_AProviderWrittenToPassDoes(t *testing.T) {
	RunConformance(t, newConformer())
}

// The variants. Each embeds a correct provider and overrides one method, so
// the diff between passing and failing is the invariant itself and nothing
// else.

type nameless struct{ *conformer }

func (nameless) Name() node.ProviderID { return "" }

type foreignOffers struct{ *conformer }

func (p foreignOffers) Offers(_ context.Context, _ node.Filter) ([]node.Offer, error) {
	o := p.offerList()
	o[0].Provider = "somebody-else"
	return o, nil
}

type invalidOffers struct{ *conformer }

func (p invalidOffers) Offers(_ context.Context, _ node.Filter) ([]node.Offer, error) {
	o := p.offerList()
	o[0].Token = ""
	return o, nil
}

type noOffers struct{ *conformer }

func (noOffers) Offers(_ context.Context, _ node.Filter) ([]node.Offer, error) { return nil, nil }

type ignoresFilter struct{ *conformer }

func (p ignoresFilter) Offers(_ context.Context, _ node.Filter) ([]node.Offer, error) {
	return p.offerList(), nil
}

type freeButPriced struct{ *conformer }

func (p freeButPriced) Caps() provider.Caps {
	c := p.conformer.Caps()
	c.Free = true
	return c
}

type paidAndUnpriced struct{ *conformer }

func (p paidAndUnpriced) Offers(_ context.Context, f node.Filter) ([]node.Offer, error) {
	o := p.offerList()
	o[0].Price = 0
	return provider.FilterOffers(f, o), nil
}

// capsWith is the shortest way to say "this provider, with these capabilities".
type capsWith struct {
	*conformer
	with provider.Caps
}

func (p capsWith) Caps() provider.Caps { return p.with }

type createReturnsNoID struct{ *conformer }

func (p createReturnsNoID) Create(ctx context.Context, req node.CreateRequest) (node.Handle, error) {
	if _, err := p.conformer.Create(ctx, req); err != nil {
		return node.Handle{}, err
	}
	return node.Handle{Provider: conformerName}, nil
}

type createNamesAnotherProvider struct{ *conformer }

func (p createNamesAnotherProvider) Create(ctx context.Context, req node.CreateRequest) (node.Handle, error) {
	h, err := p.conformer.Create(ctx, req)
	h.Provider = "somebody-else"
	return h, err
}

type listFindsNothing struct{ *conformer }

func (listFindsNothing) List(_ context.Context) ([]node.Node, error) { return nil, nil }

type listForgetsTheRunID struct{ *conformer }

func (p listForgetsTheRunID) List(ctx context.Context) ([]node.Node, error) {
	ns, err := p.conformer.List(ctx)
	for i := range ns {
		ns[i].RunID = ""
	}
	return ns, err
}

type listForgetsTheDeadline struct{ *conformer }

func (p listForgetsTheDeadline) List(ctx context.Context) ([]node.Node, error) {
	ns, err := p.conformer.List(ctx)
	for i := range ns {
		ns[i].Deadline = time.Time{}
	}
	return ns, err
}

type inspectAnswersUnderAnotherHandle struct{ *conformer }

func (p inspectAnswersUnderAnotherHandle) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	n, err := p.conformer.Inspect(ctx, h)
	n.Handle.ID += "-elsewhere"
	return n, err
}

type inspectCannotTellTheState struct{ *conformer }

func (p inspectCannotTellTheState) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	n, err := p.conformer.Inspect(ctx, h)
	n.State = node.StateInvalid
	return n, err
}

type inspectInventsMachines struct{ *conformer }

func (p inspectInventsMachines) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	if n, err := p.conformer.Inspect(ctx, h); err == nil {
		return n, nil
	}
	// The dangerous answer: a confident one. StateProvisioning is billable,
	// so every caller above now has a machine it believes is coming up.
	return node.Node{Handle: h, State: node.StateProvisioning}, nil
}

type inspectLosesNotFound struct{ *conformer }

func (p inspectLosesNotFound) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	n, err := p.conformer.Inspect(ctx, h)
	if err != nil {
		return n, errors.New("unexpected status 404")
	}
	return n, nil
}

type exposureUnreported struct{ *conformer }

func (p exposureUnreported) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	n, err := p.conformer.Inspect(ctx, h)
	if err != nil {
		return n, err
	}
	// A node that never had WithExposed called on it. "Published nothing" and
	// "never reported" are the same empty slice, which is why only a fake can
	// produce the second one deliberately.
	return node.Node{
		Handle: n.Handle, RunID: n.RunID, Deadline: n.Deadline,
		State: n.State, HostKey: n.HostKey, Offer: n.Offer,
	}.WithEndpoint(node.Endpoint{Host: "203.0.113.7", Port: 40022, User: "root"}), nil
}

type publishesSomethingNobodyAskedFor struct{ *conformer }

func (p publishesSomethingNobodyAskedFor) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	n, err := p.conformer.Inspect(ctx, h)
	if err != nil {
		return n, err
	}
	// RunPod's unauthenticated proxy, which is what made ember refuse and
	// destroy its first real node.
	return n.WithExposed(append(p.exposed(),
		node.Exposed{Port: 8188, Proto: node.ProtoHTTP, URL: "https://x-8188.proxy.example.net"})), nil
}

type substitutesTheHostKey struct{ *conformer }

func (p substitutesTheHostKey) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	n, err := p.conformer.Inspect(ctx, h)
	n.HostKey.Base64 = "AAAASomebodyElsesKeyEntirely"
	return n, err
}

type reportsATrustedOnSightKey struct{ *conformer }

func (p reportsATrustedOnSightKey) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	n, err := p.conformer.Inspect(ctx, h)
	// Pinned: a key ember had never seen, accepted and recorded. The node was
	// never authenticated on its first connection.
	n.HostKey.Source = node.HostKeyPinned
	return n, err
}

type destroyRefusesWhatIsAlreadyGone struct{ *conformer }

func (p destroyRefusesWhatIsAlreadyGone) Destroy(ctx context.Context, h node.Handle, runID node.RunID) error {
	p.mu.Lock()
	_, ok := p.machines[h.ID]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such machine %s", h.ID)
	}
	return p.conformer.Destroy(ctx, h, runID)
}

type destroyNeedsARunID struct{ *conformer }

func (p destroyNeedsARunID) Destroy(ctx context.Context, h node.Handle, runID node.RunID) error {
	if runID == "" {
		return errors.New("destroy needs a run id")
	}
	return p.conformer.Destroy(ctx, h, runID)
}

type destroyLeavesTheMachine struct{ *conformer }

func (destroyLeavesTheMachine) Destroy(_ context.Context, _ node.Handle, _ node.RunID) error {
	return nil
}

type destroyRederivesTheRunID struct{ *conformer }

// Destroy here is the original defect, exactly: the run id is read off the
// machine and the argument is ignored, so once the machine is gone -- removed
// by anything but ember -- the run id is empty and Destroy returns nil over
// objects that are still there.
func (p destroyRederivesTheRunID) Destroy(_ context.Context, h node.Handle, _ node.RunID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.machines[h.ID]
	if !ok {
		return nil
	}
	delete(p.machines, h.ID)
	for id, r := range p.leftovers {
		if r == m.runID {
			delete(p.leftovers, id)
		}
	}
	return nil
}

type destroyForgetsWhatIsBesideTheMachine struct{ *conformer }

func (p destroyForgetsWhatIsBesideTheMachine) Destroy(_ context.Context, h node.Handle, _ node.RunID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.machines, h.ID)
	return nil
}

// TestConformanceChecks_EveryInvariantCanFail reinstates each defect and
// requires the matching check to go red.
//
// The whole suite is run against every variant, not only the named check, for
// two reasons: a check that panicked on a misbehaving provider would be a
// check nobody could read a failure out of, and a defect that trips several
// checks is informative rather than a problem.
func TestConformanceChecks_EveryInvariantCanFail(t *testing.T) {
	for _, tc := range []struct {
		defect string
		check  string
		p      func() provider.Provider
	}{
		{"a provider with no name", "the provider has a name",
			func() provider.Provider { return nameless{newConformer()} }},

		{"offers carrying another provider's id", "every offer belongs to this provider",
			func() provider.Provider { return foreignOffers{newConformer()} }},
		{"an offer FilterOffers would discard", "every offer belongs to this provider",
			func() provider.Provider { return invalidOffers{newConformer()} }},
		{"a provider with no capacity at all", "every offer belongs to this provider",
			func() provider.Provider { return noOffers{newConformer()} }},

		{"a provider that ignores the filter", "every offer matches the filter it was given",
			func() provider.Provider { return ignoresFilter{newConformer()} }},

		{"Caps.Free beside a priced offer", "an offer is priced as the capability says",
			func() provider.Provider { return freeButPriced{newConformer()} }},
		{"a paid provider quoting zero", "an offer is priced as the capability says",
			func() provider.Provider { return paidAndUnpriced{newConformer()} }},

		{"a platform port of 0", "every tolerated platform port is named",
			func() provider.Provider {
				return platformPorts(exposure.PlatformPort{Proto: node.ProtoHTTP, Why: "the web terminal"})
			}},
		{"a platform port with no protocol", "every tolerated platform port is named",
			func() provider.Provider {
				return platformPorts(exposure.PlatformPort{Port: 19123, Why: "the web terminal"})
			}},
		{"a platform port with no reason given", "every tolerated platform port is named",
			func() provider.Provider {
				return platformPorts(exposure.PlatformPort{Port: 19123, Proto: node.ProtoHTTP})
			}},
		{"22/tcp declared as a platform port", "every tolerated platform port is named",
			func() provider.Provider {
				return platformPorts(exposure.PlatformPort{Port: 22, Proto: node.ProtoTCP, Why: "ssh, surely"})
			}},
		{"one platform port declared twice", "every tolerated platform port is named",
			func() provider.Provider {
				return platformPorts(
					exposure.PlatformPort{Port: 19123, Proto: node.ProtoHTTP, Why: "the web terminal"},
					exposure.PlatformPort{Port: 19123, Proto: node.ProtoHTTP, Why: "the web terminal, again"})
			}},

		{"a create that returns no id", "create returns a handle that names a machine",
			func() provider.Provider { return createReturnsNoID{newConformer()} }},
		{"a handle naming another provider", "create returns a handle that names a machine",
			func() provider.Provider { return createNamesAnotherProvider{newConformer()} }},

		{"a list that finds nothing it created", "list finds by label what create made",
			func() provider.Provider { return listFindsNothing{newConformer()} }},
		{"a list that drops the run id", "list finds by label what create made",
			func() provider.Provider { return listForgetsTheRunID{newConformer()} }},
		{"a list that drops the deadline", "list finds by label what create made",
			func() provider.Provider { return listForgetsTheDeadline{newConformer()} }},

		{"a node answering under another handle", "inspect reports the node create returned",
			func() provider.Provider { return inspectAnswersUnderAnotherHandle{newConformer()} }},
		{"a state the provider cannot name", "inspect reports the node create returned",
			func() provider.Provider { return inspectCannotTellTheState{newConformer()} }},

		{"an inspect that invents machines", "inspect of an unknown handle is a decidable not-found",
			func() provider.Provider { return inspectInventsMachines{newConformer()} }},
		{"a not-found arriving as an opaque error", "inspect of an unknown handle is a decidable not-found",
			func() provider.Provider { return inspectLosesNotFound{newConformer()} }},

		{"a node that never reported its exposure", "a ready node has reported its exposure",
			func() provider.Provider { return exposureUnreported{newConformer()} }},
		{"a public port nobody asked for", "exposure matches the capability",
			func() provider.Provider { return publishesSomethingNobodyAskedFor{newConformer()} }},
		{"a provider that forgot Caps.PublicSSH", "exposure matches the capability",
			func() provider.Provider {
				c := newConformer()
				caps := c.Caps()
				caps.PublicSSH = false
				return capsWith{c, caps}
			}},

		{"a substituted host key", "a reported host key is the one ember injected",
			func() provider.Provider { return substitutesTheHostKey{newConformer()} }},
		{"a key reported as trusted on sight", "a reported host key is the one ember injected",
			func() provider.Provider { return reportsATrustedOnSightKey{newConformer()} }},

		{"a destroy that refuses a missing machine", "destroying an unknown machine is success",
			func() provider.Provider { return destroyRefusesWhatIsAlreadyGone{newConformer()} }},
		{"a destroy that is not idempotent", "destroy is idempotent",
			func() provider.Provider { return destroyRefusesWhatIsAlreadyGone{newConformer()} }},
		{"a destroy that demands a run id", "destroy accepts an empty run id",
			func() provider.Provider { return destroyNeedsARunID{newConformer()} }},
		{"a destroy that destroys nothing", "a destroyed node stops being reported as billable",
			func() provider.Provider { return destroyLeavesTheMachine{newConformer()} }},

		// There is no row here for the defect this check is named after -- a
		// Destroy that re-derives the run id from the machine rather than
		// taking the argument. It is in destroyRederivesTheRunID below, and
		// the suite *cannot* catch it: the re-derivation only goes wrong once
		// the machine is gone, and removing the machine behind the provider's
		// back is not something the Provider interface can do. Recorded as a
		// test rather than as a comment, because a check that silently cannot
		// catch the thing it is named after is exactly what this file exists
		// to prevent. See TestConformance_CannotCatchARederivedRunID.
		{"a destroy that forgets what is beside the machine",
			"destroy takes the run id and leaves nothing of the run behind",
			func() provider.Provider { return destroyForgetsWhatIsBesideTheMachine{newConformer()} }},
		{"a sweep nothing will ever ask for", "destroy takes the run id and leaves nothing of the run behind",
			func() provider.Provider {
				c := newConformer()
				caps := c.Caps()
				caps.Leftovers = false
				return capsWith{c, caps}
			}},
		{"a sweep that would fail its type assertion", "destroy takes the run id and leaves nothing of the run behind",
			func() provider.Provider { return claimsLeftoversWithoutTheAPI{newConformer()} }},

		{"a destroy that leaves the machine in the listing",
			"a destroyed node stops being reported as billable",
			func() provider.Provider { return &destroyLeavesItListed{conformer: newConformer()} }},
	} {
		t.Run(tc.defect, func(t *testing.T) {
			p := tc.p()
			found := map[string]error{}
			for _, c := range conformanceChecks {
				if err := c.check(t.Context(), p); err != nil {
					found[c.name] = err
				}
			}
			if found[tc.check] == nil {
				t.Errorf("%q passed %q; the check cannot catch the defect it is written for.\n"+
					"what did fail: %v", tc.defect, tc.check, names(found))
			}
		})
	}
}

// TestConformance_CannotCatchARederivedRunID records the boundary of the
// suite, as an assertion rather than a comment.
//
// destroyRederivesTheRunID is the defect local.Destroy actually had: the run
// id comes off the machine's own record and the argument is ignored, so once
// the machine is gone -- `docker rm`, a prune, a crashed lab's cleanup -- the
// run id is empty and Destroy returns nil over objects that are still there.
// Every conformance check passes it, because reproducing the state it goes
// wrong in means removing the machine behind the provider's back, and the
// Provider interface cannot. A hook per fake would close it and would also be
// the escape hatch that makes a suite stop holding a provider to its
// interface, so the invariant is left out here and tested beside the provider,
// through its own seam, where the removal is expressible.
//
// If this test ever fails, the suite has grown the ability to catch it: delete
// this and add the row to the table above.
func TestConformance_CannotCatchARederivedRunID(t *testing.T) {
	p := destroyRederivesTheRunID{newConformer()}
	for _, c := range conformanceChecks {
		if err := c.check(t.Context(), p); err != nil {
			t.Fatalf("%q now catches a Destroy that re-derives the run id: %v\n"+
				"that is good news, and this test is the record that it used not to -- "+
				"delete it and add the row to TestConformanceChecks_EveryInvariantCanFail", c.name, err)
		}
	}
}

// platformPorts builds a provider whose Caps declare exactly these tolerated
// mappings, so a defect in the declaration is the only thing wrong with it.
func platformPorts(ports ...exposure.PlatformPort) provider.Provider {
	c := newConformer()
	caps := c.Caps()
	caps.PlatformPorts = ports
	return capsWith{c, caps}
}

func names(m map[string]error) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRunConformance_FailsLoudlyWhenTheFakeStaggersReadiness: a provider wired
// to a fake that does not bring a node to ready at once must fail rather than
// pass over the checks that need one. A suite that gave up quietly there would
// report success for the exposure and host-key invariants without having run
// them, which is the failure the --- SKIP gate exists to prevent, reached
// without skipping anything.
func TestRunConformance_FailsLoudlyWhenTheFakeStaggersReadiness(t *testing.T) {
	p := neverReady{newConformer()}
	for _, name := range []string{
		"a ready node has reported its exposure",
		"exposure matches the capability",
		"a reported host key is the one ember injected",
	} {
		err := runOneCheck(t, name, p)
		if err == nil {
			t.Fatalf("%q passed against a provider whose node never becomes ready", name)
		}
		if !strings.Contains(err.Error(), "nothing to wait on") {
			t.Errorf("%q failed with %v, which does not name what the fake has to do", name, err)
		}
	}
}

type neverReady struct{ *conformer }

func (p neverReady) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	n, err := p.conformer.Inspect(ctx, h)
	if err != nil {
		return n, err
	}
	n.State = node.StateProvisioning
	return n, nil
}

// TestRunConformance_ReportsAProviderThatCannotBeAskedForOffers: every check
// that rents something goes through Offers, so a provider whose catalogue call
// fails must produce an error naming that rather than a confusing failure
// further in.
func TestRunConformance_ReportsAProviderThatCannotBeAskedForOffers(t *testing.T) {
	p := offersFail{newConformer()}
	for _, name := range []string{
		"every offer belongs to this provider",
		"an offer is priced as the capability says",
		"create returns a handle that names a machine",
		"list finds by label what create made",
	} {
		err := runOneCheck(t, name, p)
		if err == nil {
			t.Fatalf("%q passed against a provider that cannot list offers", name)
		}
		if !strings.Contains(err.Error(), "catalogue is down") {
			t.Errorf("%q failed with %v, which does not carry the provider's own reason", name, err)
		}
	}
	// And the unsatisfiable-filter check, whose non-ErrNoCapacity branch is
	// the one that must not read a transport failure as a tidy refusal.
	if err := runOneCheck(t, "every offer matches the filter it was given", p); err == nil {
		t.Error("an unreachable catalogue read as a filter that matched nothing")
	}
}

type offersFail struct{ *conformer }

func (offersFail) Offers(_ context.Context, _ node.Filter) ([]node.Offer, error) {
	return nil, errors.New("the catalogue is down")
}

// TestRunConformance_ReportsAProviderThatCannotCreateOrList covers the
// remaining transport failures: a create that fails and a list that fails are
// the provider saying so, and the suite must pass the reason through rather
// than report an invariant violation.
func TestRunConformance_ReportsAProviderThatCannotCreateOrList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     provider.Provider
		check string
		want  string
	}{
		{"create fails", createFails{newConformer()}, "create returns a handle that names a machine", "the api said no"},
		{"list fails", listFails{newConformer()}, "list finds by label what create made", "the api said no"},
		{"list fails after destroy", listFails{newConformer()}, "a destroyed node stops being reported as billable", "the api said no"},
		{"destroy fails", destroyFails{newConformer()}, "destroy is idempotent", "the api said no"},
		{"inspect fails", inspectFails{newConformer()}, "inspect reports the node create returned", "the api said no"},
		{"leftovers cannot be listed", leftoverListFails{newConformer()},
			"destroy takes the run id and leaves nothing of the run behind", "the api said no"},
		{"destroy fails before the sweep", destroyFails{newConformer()},
			"destroy takes the run id and leaves nothing of the run behind", "the api said no"},
		{"destroy fails before the billing check", destroyFails{newConformer()},
			"a destroyed node stops being reported as billable", "the api said no"},
		{"the listing breaks between the two snapshots", &listFailsAfterCreate{conformer: newConformer()},
			"list finds by label what create made", "the api said no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runOneCheck(t, tc.check, tc.p)
			if err == nil {
				t.Fatalf("%q passed against a provider whose %s", tc.check, tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, which does not carry the provider's own reason", err)
			}
		})
	}
}

var errAPI = errors.New("the api said no")

// noCapacity answers an unsatisfiable filter the way a provider that has an
// answer does: with the sentinel `ember up` and `ember ls` both read, rather
// than with an empty list.
type noCapacity struct{ *conformer }

func (noCapacity) Offers(_ context.Context, _ node.Filter) ([]node.Offer, error) {
	return nil, fmt.Errorf("%w: nothing is available", node.ErrNoCapacity)
}

// listFailsAfterCreate fails only the second List, which is the one taken
// after a node exists. The first is the before-snapshot, and a provider whose
// listing breaks between the two is a different failure from one that could
// never list at all.
type listFailsAfterCreate struct {
	*conformer
	calls int
}

func (p *listFailsAfterCreate) List(ctx context.Context) ([]node.Node, error) {
	p.calls++
	if p.calls > 1 {
		return nil, errAPI
	}
	return p.conformer.List(ctx)
}

// destroyLeavesItListed is the half-destroy: the machine stops answering
// Inspect and the listing still carries it, so reap acts on it forever and
// `ember ls` prices a machine that is not there.
type destroyLeavesItListed struct {
	*conformer
	gone map[string]bool
}

func (p *destroyLeavesItListed) Destroy(_ context.Context, h node.Handle, _ node.RunID) error {
	if p.gone == nil {
		p.gone = map[string]bool{}
	}
	p.gone[h.ID] = true
	return nil
}

func (p *destroyLeavesItListed) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	if p.gone[h.ID] {
		return node.Node{}, fmt.Errorf("machine %s: %w", h.ID, node.ErrNotFound)
	}
	return p.conformer.Inspect(ctx, h)
}

// claimsLeftoversWithoutTheAPI embeds the interface rather than the concrete
// type, which is the only way to get a provider that says Caps.Leftovers and
// does not satisfy provider.LeftoverAPI: embedding *conformer would promote
// its methods and make the claim true.
type claimsLeftoversWithoutTheAPI struct{ provider.Provider }

func (p claimsLeftoversWithoutTheAPI) Caps() provider.Caps {
	c := p.Provider.Caps()
	c.Leftovers = true
	return c
}

type createFails struct{ *conformer }

func (createFails) Create(_ context.Context, _ node.CreateRequest) (node.Handle, error) {
	return node.Handle{}, errAPI
}

type listFails struct{ *conformer }

func (listFails) List(_ context.Context) ([]node.Node, error) { return nil, errAPI }

type destroyFails struct{ *conformer }

func (destroyFails) Destroy(_ context.Context, _ node.Handle, _ node.RunID) error { return errAPI }

type inspectFails struct{ *conformer }

func (inspectFails) Inspect(_ context.Context, _ node.Handle) (node.Node, error) {
	return node.Node{}, errAPI
}

type leftoverListFails struct{ *conformer }

func (leftoverListFails) LeftoverList(_ context.Context) ([]node.Leftover, error) {
	return nil, errAPI
}

// TestRunConformance_RefusesASpecItCannotBuild guards the one failure in
// createConformanceNode that is not the provider's fault: a spec or a create
// request this package built wrongly. It is here because the error would
// otherwise read as a provider defect, and an operator debugging a new
// provider would start in the wrong place.
func TestRunConformance_RefusesASpecItCannotBuild(t *testing.T) {
	// An offer that does not validate reaches NewCreateRequest, which refuses
	// it -- the suite must say so as its own failure rather than blame the
	// create call it never made.
	p := unvalidatableOffer{newConformer()}
	err := runOneCheck(t, "create returns a handle that names a machine", p)
	if err == nil {
		t.Fatal("the suite built a create request from an offer that does not validate")
	}
	if !strings.Contains(err.Error(), "conformance create request") {
		t.Errorf("got %v, which does not say the suite could not build the request", err)
	}
}

type unvalidatableOffer struct{ *conformer }

func (p unvalidatableOffer) Offers(_ context.Context, _ node.Filter) ([]node.Offer, error) {
	// Past FilterOffers rather than through it: FilterOffers drops an invalid
	// offer, and what is being staged is an offer that reaches the create
	// path anyway.
	o := p.offerList()
	o[0].Price = -1
	return o, nil
}

// TestConformance_AnUnsatisfiableFilterMayBeAnswered: ErrNoCapacity is an
// answer and not a misbehaviour. The local provider returns it when the
// machine has a card the daemon cannot pass through, and RunPod when a store
// pins placement to a datacentre with nothing in it -- so a check that
// insisted on an empty list would fail two providers for being helpful.
func TestConformance_AnUnsatisfiableFilterMayBeAnswered(t *testing.T) {
	if err := runOneCheck(t, "every offer matches the filter it was given", noCapacity{newConformer()}); err != nil {
		t.Errorf("a provider answering with node.ErrNoCapacity was reported as a defect: %v", err)
	}
}

// TestTruncKey_LeavesAShortKeyAlone: the truncation is only so a mismatch
// reads as a mismatch, and an ellipsis on a key that already fits would be
// noise in the one message an operator has to compare two keys from.
func TestTruncKey_LeavesAShortKeyAlone(t *testing.T) {
	if got := truncKey("AAAA"); got != "AAAA" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("A", 40)
	if got := truncKey(long); got == long || !strings.HasSuffix(got, "...") {
		t.Errorf("got %q", got)
	}
}

// runOneCheck runs the named conformance check and returns what it found.
func runOneCheck(t *testing.T, name string, p provider.Provider) error {
	t.Helper()
	for _, c := range conformanceChecks {
		if c.name == name {
			return c.check(t.Context(), p)
		}
	}
	t.Fatalf("no conformance check is called %q; the table and the suite have drifted", name)
	return nil
}
