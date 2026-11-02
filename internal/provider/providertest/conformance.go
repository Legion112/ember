package providertest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// ConformanceImage is the image a conformance node's spec names.
//
// A constant rather than a parameter, because RunConformance taking nothing
// but a provider is the point: the guarantees are not per-provider, so neither
// is the suite's shape. What is per-provider is the fake underneath, and a
// provider whose create path checks that the image exists -- the local one
// does, because an image missing from the daemon is a hard failure naming
// `make docker-build` rather than a silent pull of somebody else's -- has to
// be told about this one. That is one line in the provider's own test, and a
// provider that forgets it fails the create checks with the daemon's own
// message rather than silently not running them.
const ConformanceImage = "ghcr.io/legion112/ember-conformance:1"

// ConformanceTTL is how long a conformance node asks to live.
//
// Long enough that nothing under test can read the deadline as already past:
// the leftover sweep is bounded by the deadline precisely so that a run still
// inside its create window is not mistaken for wreckage, and a suite that
// rented for a second would be exercising that bound by accident.
const ConformanceTTL = time.Hour

// conformanceKey is the host key material a conformance create injects.
//
// Not a real key, and it does not need to be: no conformance check completes
// an SSH handshake. It is here because node.NewCreateRequest refuses a request
// with no key to inject -- without one the first connection cannot be
// authenticated -- and because the public half is what the "a provider that
// reports a host key reports the one ember injected" check compares against.
var (
	conformanceHostKey = node.NewSecret([]byte(
		"-----BEGIN OPENSSH PRIVATE KEY-----\nconformance\n-----END OPENSSH PRIVATE KEY-----\n"))
	conformancePublic = node.HostKey{
		Source: node.HostKeyMinted,
		Algo:   "ssh-ed25519",
		Base64: "AAAAC3NzaC1lZDI1NTE5AAAAIConformanceHostKeyNotARealOne",
	}
	conformanceAuthorized = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIConformanceOperatorKey ember-conformance"
)

// RunConformance holds p to the guarantees that are not in the Provider
// interface.
//
// The interface is six methods and says nothing about any of this. Every
// invariant below is a defect this repository has already paid for, and before
// this suite existed every one of them was re-proven by hand, per provider --
// so the cost of a new provider was six methods plus rediscovering which
// properties matter, with a leaked node or a public port as the failure mode
// rather than a red test.
//
// Credential-free and network-free by construction: it only ever calls the
// provider handed to it, so whatever transport that provider is wired to is
// the one the checks run against. runpod's own tests point the real provider
// at a stateful stand-in for RunPod's API; local's point it at the recording
// Docker engine. Nothing here knows which.
//
// It asserts that a provider *reports* honestly. It does not decide whether
// the report is acceptable -- except by applying, unchanged, the choice
// provision makes from Caps, which is the one place that decision lives. A
// provider checking its own exposure is a provider grading its own homework,
// and a conformance suite that reimplemented the rule would be a second place
// for it to drift.
//
// Nothing here skips. A check that does not apply to a provider is driven by
// Caps and asserts the thing Caps claims instead, because a suite that
// silently passed over an invariant a provider cannot support would report
// success for something that never ran.
func RunConformance(t *testing.T, p provider.Provider) {
	t.Helper()

	// The capability claims come first and outside the table. Every check
	// below reads Caps to decide what the provider promised, so a Caps field
	// that disagrees with the code would make the rest of the suite assert
	// the wrong things confidently.
	AssertStoreCapsMatch(t, p)
	AssertLeftoverCapsMatch(t, p)
	AssertCatalogueCapsMatch(t, p)

	for _, c := range conformanceChecks {
		t.Run(c.name, func(t *testing.T) {
			if err := c.check(t.Context(), p); err != nil {
				t.Error(err)
			}
		})
	}
}

// conformanceCheck is one invariant.
//
// An error return rather than a *testing.T parameter, so each one is a
// function whose failure is a value. That is what makes the suite itself
// testable in both directions: providertest's own tests run every check
// against a provider built to be correct and then against providers built to
// violate exactly one invariant each, and assert the matching check goes red.
// An assertion suite is the easiest kind of code to write so that it cannot
// fail, and a regression test that has never failed proves nothing.
type conformanceCheck struct {
	name  string
	check func(ctx context.Context, p provider.Provider) error
}

var conformanceChecks = []conformanceCheck{
	{"the provider has a name", checkName},
	{"every offer belongs to this provider", checkOffersBelongHere},
	{"every offer matches the filter it was given", checkOffersMatchTheFilter},
	{"an offer is priced as the capability says", checkOfferPriceMatchesFree},
	{"every offer says whether it can be reclaimed", checkOffersSayWhetherTheyCanBeReclaimed},
	{"every tolerated platform port is named", checkPlatformPorts},
	{"create returns a handle that names a machine", checkCreateReturnsANamedHandle},
	{"list finds by label what create made", checkListFindsWhatCreateMade},
	{"inspect reports the node create returned", checkInspectAfterCreate},
	{"inspect of an unknown handle is a decidable not-found", checkInspectUnknown},
	{"a ready node has reported its exposure", checkExposureIsReported},
	{"exposure matches the capability", checkExposureMatchesCaps},
	{"a reported host key is the one ember injected", checkReportedHostKeyIsTheInjectedOne},
	{"destroying an unknown machine is success", checkDestroyUnknownIsSuccess},
	{"destroy is idempotent", checkDestroyIsIdempotent},
	{"destroy accepts an empty run id", checkDestroyAcceptsAnEmptyRunID},
	{"a destroyed node stops being reported as billable", checkDestroyedNodeStopsBilling},
	{"destroy takes the run id and leaves nothing of the run behind", checkDestroyLeavesNoLeftovers},
}

func checkName(_ context.Context, p provider.Provider) error {
	// Registry.Add refuses a nameless provider, so a provider that reached a
	// conformance run with no name would fail later and further away -- and
	// node.Handle carries the name, so every handle it minted would name
	// nobody and `ember down provider/id` could not route.
	if p.Name() == "" {
		return errors.New("Name() is empty: a provider with no name cannot be registered, " +
			"and every handle it mints names nobody")
	}
	return nil
}

func checkOffersBelongHere(ctx context.Context, p provider.Provider) error {
	offers, err := p.Offers(ctx, node.Filter{})
	if err != nil {
		return fmt.Errorf("Offers: %w", err)
	}
	if len(offers) == 0 {
		return errors.New("Offers returned nothing for an empty filter, so no check below can rent anything; " +
			"the fake this provider is wired to has to have capacity")
	}
	for _, o := range offers {
		// Create refuses an offer belonging to another provider, which is
		// right -- but the refusal arrives after the operator has chosen what
		// to rent, and a mapper that copy-pasted another provider's id
		// produces offers that look perfectly good in `ember offers` and can
		// never be created.
		if o.Provider != p.Name() {
			return fmt.Errorf("offer %s claims provider %q but came from %q; Create refuses it, "+
				"so it is an offer that lists and can never be rented", o.Token, o.Provider, p.Name())
		}
		// FilterOffers drops an invalid offer silently, which is a cost
		// control and not a validator: an offer that fails Validate here
		// means the mapper produced something the layers above would quietly
		// discard, and the operator reads "no capacity" for hardware the
		// provider said it had. That is the NormalizeGPU family of bug,
		// reached from the provider's own side.
		if err := o.Validate(); err != nil {
			return fmt.Errorf("offer %s does not validate, so FilterOffers discards it "+
				"and the operator reads no-capacity for a card the provider offered: %w", o.Token, err)
		}
	}
	return nil
}

func checkOffersMatchTheFilter(ctx context.Context, p provider.Provider) error {
	// A card no provider has. Deliberately not a plausible model name: the
	// filter has to match nothing on any real catalogue, and "a1000" is
	// exactly the string GPUModel.Accepts was taught to distinguish from
	// "a100" at the capacity boundary.
	f := node.Filter{GPU: "ember-conformance-no-such-card"}

	offers, err := p.Offers(ctx, f)
	if err != nil {
		// A provider that answers an unsatisfiable filter with ErrNoCapacity
		// is answering, not misbehaving. ember ls and ember up both read that
		// sentinel, and the local provider returns it when the machine has a
		// card the daemon cannot pass through.
		if errors.Is(err, node.ErrNoCapacity) {
			return nil
		}
		return fmt.Errorf("Offers with a filter nothing can match: %w", err)
	}
	for _, o := range offers {
		// provision re-applies the filter, so this cannot reach a create on
		// its own. It can still cost the operator the thing they came for in
		// the other direction -- an `ember offers` listing hardware that
		// cannot be had -- and a provider that ignores the filter entirely is
		// one whose own narrowing is dead code nobody notices.
		if !f.Match(o) {
			return fmt.Errorf("Offers returned %s for a filter asking for %q; "+
				"a provider that hands back what was not asked for is one whose narrowing is never exercised",
				o, f.GPU)
		}
	}
	return nil
}

func checkOfferPriceMatchesFree(ctx context.Context, p provider.Provider) error {
	offers, err := p.Offers(ctx, node.Filter{})
	if err != nil {
		return fmt.Errorf("Offers: %w", err)
	}
	free := p.Caps().Free
	for _, o := range offers {
		switch {
		case free && o.Price != 0:
			// Caps.Free is how provision skips the budget check honestly
			// rather than by comparing a real price against zero. A Free
			// provider quoting a price means the budget is being skipped over
			// a number nobody checked.
			return fmt.Errorf("%s says Caps.Free and quotes %v for offer %s; "+
				"Free is what lets provision skip the budget check, so a priced offer there is a spend nothing bounds",
				p.Name(), o.Price, o.Token)
		case !free && o.Price == 0:
			// A zero price on a paid provider is the provider declining to
			// quote, not a free machine -- RunPod does exactly this -- and a
			// budget check against zero waves through whatever it really
			// costs.
			return fmt.Errorf("%s is not free and offer %s is priced at zero; "+
				"on a paid provider that is a declined quote rather than a free machine, and every budget check against it passes",
				p.Name(), o.Token)
		}
	}
	return nil
}

// checkOffersSayWhetherTheyCanBeReclaimed is the check whose absence was the
// bug.
//
// node.Offer carried an `Interruptible bool` that no provider ever assigned
// while provider.Caps carried a different field of the same name that was
// assigned correctly and read by nothing, so Filter.Match's reclaim guard
// compared against a constant false for the life of the code. It ran on every
// offer, passed on every offer and refused none -- and `runpod_cloud:
// COMMUNITY` rented reclaimable hardware with `allow_interruptible` at its
// default of false. Nothing could have caught it but a check that crosses the
// two layers, which is what this suite is for.
//
// Network-free like everything else here: it asks the provider it was handed
// what it offers and what it claims, and compares the two. Caps.Interruptible
// was left out of this suite on the belief that confirming it needed a
// reclaim to actually happen; it does not. What is checkable without renting
// anything is whether the provider's answers agree with each other, and that
// is the half that was wrong.
func checkOffersSayWhetherTheyCanBeReclaimed(ctx context.Context, p provider.Provider) error {
	offers, err := p.Offers(ctx, node.Filter{AllowInterruptible: true})
	if err != nil {
		return fmt.Errorf("Offers: %w", err)
	}
	tier := p.Caps().InterruptibleTier
	for _, o := range offers {
		// The direction that leaks, and the one that produced this issue. A
		// provider that never populates the field hands back offers ember
		// refuses by default -- so the symptom is `ember up` reading "no
		// capacity" against a full catalogue, or, with the opt-in set,
		// renting reclaimable hardware the operator was never warned about.
		if o.Reclaim == node.ReclaimUnknown {
			return fmt.Errorf("offer %s leaves Reclaim at ReclaimUnknown; "+
				"a provider that does not say is treated as reclaimable, so the filter refuses it "+
				"unless allow_interruptible is set -- which reads as no capacity against a full catalogue. "+
				"Say node.ReclaimNever or node.ReclaimAnyTime", o.Token)
		}
		// Caps is a claim about the whole tier, so an offer disagreeing with
		// it means one of the two is lying and the filter reads the offer.
		// The claim being true while an offer says ReclaimNever is the
		// expensive direction: that offer is rented with no opt-in and can
		// vanish mid-render.
		if tier && o.Reclaim != node.ReclaimAnyTime {
			return fmt.Errorf("%s says Caps.InterruptibleTier and offer %s does not say ReclaimAnyTime; "+
				"Filter.Match reads the offer and not the capability, so that machine is rented "+
				"without allow_interruptible and can be taken back mid-generation", p.Name(), o.Token)
		}
	}
	return nil
}

func checkPlatformPorts(_ context.Context, p provider.Provider) error {
	seen := map[string]bool{}
	for _, pp := range p.Caps().PlatformPorts {
		if pp.Port == 0 {
			return errors.New("a platform port is declared with port 0, which matches nothing: " +
				"the concession reads as configured and tolerates nothing, so the first node carrying the real port is destroyed")
		}
		// ProtoUnknown is the zero value and never passes an exposure
		// assertion, so a platform port left at it is a concession that
		// tolerates nothing while reading as one that does.
		if pp.Proto == node.ProtoUnknown {
			return fmt.Errorf("platform port %d declares no protocol; an http mapping on RunPod is served "+
				"through an unauthenticated proxy and a tcp one is not, and a tolerated port that cannot tell "+
				"them apart tolerates both", pp.Port)
		}
		if pp.Why == "" {
			return fmt.Errorf("platform port %d/%s is tolerated with no reason given; an operator reading an "+
				"exposure report has to be told which ports ember decided to live with and on whose authority",
				pp.Port, pp.Proto)
		}
		// 22/tcp is counted by AssertSSHOnly before the platform list is
		// consulted, so declaring it changes nothing at all. Same defect as
		// skip-layer guidance configured with no scale: it reads as a setting
		// that applies and does not, and here what it would appear to do is
		// relax the one requirement that keeps a node reachable.
		if pp.Port == 22 && pp.Proto == node.ProtoTCP {
			return errors.New("22/tcp is declared as a platform port, which AssertSSHOnly counts before it " +
				"consults the platform list: the entry applies to nothing while reading as a tolerated SSH mapping")
		}
		key := fmt.Sprintf("%d/%s", pp.Port, pp.Proto)
		if seen[key] {
			return fmt.Errorf("platform port %s is declared twice; two entries for one mapping is two places "+
				"for the reason to drift and one of them is already unread", key)
		}
		seen[key] = true
	}
	return nil
}

func checkCreateReturnsANamedHandle(ctx context.Context, p provider.Provider) error {
	req, h, err := createConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = p.Destroy(ctx, h, req.RunID) }()

	if h.Provider != p.Name() {
		return fmt.Errorf("Create returned a handle for provider %q from %q; the handle is what routes a "+
			"later Destroy, so one naming the wrong provider is a node nothing can stop", h.Provider, p.Name())
	}
	if h.ID == "" {
		// A create that succeeded and named nothing is the worst case there
		// is: a machine may exist, it is billing, and ember cannot say which
		// one it is. RunPod answers 2xx with no id, which is why that
		// provider turns it into a loud error rather than an empty handle.
		return errors.New("Create succeeded and returned an empty id; a machine may exist and be billing " +
			"with nothing able to name it, which is the one failure the label in the journal has to cover for")
	}
	return nil
}

func checkListFindsWhatCreateMade(ctx context.Context, p provider.Provider) error {
	// Snapshotted rather than assumed empty, so the check says nothing about
	// what else is on the fake and holds whatever order the table runs in.
	before, err := listIDs(ctx, p)
	if err != nil {
		return err
	}

	req, h, err := createConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = p.Destroy(ctx, h, req.RunID) }()

	after, err := p.List(ctx)
	if err != nil {
		return fmt.Errorf("List after Create: %w", err)
	}

	var got *node.Node
	for i := range after {
		if after[i].Handle.ID == h.ID {
			got = &after[i]
		}
	}
	if got == nil {
		// List is what makes an orphan visible after the local record is
		// lost, so a node it cannot find is a node nothing will ever reap.
		// It is found by label, not from a local record, which is the whole
		// reason the label is the authority.
		return fmt.Errorf("List does not report the node Create just made (%s); it was found before by %d id(s) "+
			"and after by %d, so an orphan here would survive every reap", h.ID, len(before), len(after))
	}
	// Both names, because a record is joined to a live node by provider node
	// id *or* by the run id in the label. Matching on the id alone meant a run
	// killed between Create returning and the id being written produced two
	// findings -- an orphaned record that could not name its node and an
	// untracked node no record mentioned -- and reap could act on neither,
	// although both carried the same run id the whole time.
	if got.RunID != req.RunID {
		return fmt.Errorf("List reports node %s with run id %q, not the %q Create was given; "+
			"a node reconcile cannot join to its record by run id becomes two findings reap can act on neither of",
			h.ID, got.RunID, req.RunID)
	}
	// The deadline is reap's authority to destroy, and it travels in the
	// label precisely so that it survives the loss of every local copy. A
	// provider that drops it leaves a node whose expiry nothing can decide.
	if got.Deadline.Unix() != req.Deadline.Unix() {
		return fmt.Errorf("List reports node %s with deadline %s, not the %s in the label; "+
			"the deadline is what reap decides on, and the label is the copy that outlives the journal",
			h.ID, got.Deadline.UTC(), req.Deadline.UTC())
	}
	return nil
}

func checkInspectAfterCreate(ctx context.Context, p provider.Provider) error {
	req, h, err := createConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = p.Destroy(ctx, h, req.RunID) }()

	n, err := p.Inspect(ctx, h)
	if err != nil {
		return fmt.Errorf("Inspect of the handle Create returned: %w", err)
	}
	if n.Handle != h {
		return fmt.Errorf("Inspect of %s reported node %s; the handle is what a Destroy is routed by, "+
			"so a node answering under a different one is a node the caller cannot stop", h, n.Handle)
	}
	// StateInvalid is what a provider returns when it does not recognise its
	// own machine's status, and it is Billable() on purpose: not knowing
	// whether money is burning has to read as "it is".
	if n.State == node.StateInvalid {
		return fmt.Errorf("Inspect of %s reports an invalid state right after Create, so ember cannot tell "+
			"whether the machine it just rented is costing money", h)
	}
	return nil
}

func checkInspectUnknown(ctx context.Context, p provider.Provider) error {
	h := unknownHandle(p)

	n, err := p.Inspect(ctx, h)
	if err == nil {
		// The dangerous answer is not an error, it is a confident one: a zero
		// node with a nil error is a machine reported as existing in whatever
		// state the zero value happens to be, and StateProvisioning is
		// billable. WaitReady would then poll a machine that is not there
		// until the deadline.
		return fmt.Errorf("Inspect of the unknown handle %s succeeded and reported state %q; "+
			"a provider that answers for a machine it does not have gives every caller above it a node "+
			"that cannot be told from a real one", h, n.State)
	}
	// node.ErrNotFound and nothing else, because reconcile and `ember down`
	// both branch on it: a not-found that arrives as an opaque transport
	// error is a finding the operator has to interpret, and the one thing it
	// reads as is "the provider could not be reached" -- which must never
	// render as a clean bill of health.
	if !errors.Is(err, node.ErrNotFound) {
		return fmt.Errorf("Inspect of the unknown handle %s failed with %v, which does not wrap "+
			"node.ErrNotFound; callers cannot tell a missing machine from a provider they could not reach, "+
			"and those have opposite remedies", h, err)
	}
	return nil
}

func checkExposureIsReported(ctx context.Context, p provider.Provider) error {
	req, h, n, err := readyConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = p.Destroy(ctx, h, req.RunID) }()

	// "Published nothing" and "never reported" are the same empty slice in Go
	// and have to be different answers here, because treating the second as
	// the first is how a provider whose mapping code forgets to populate
	// exposure gets waved through: the assertion still runs, still passes and
	// verifies nothing.
	if _, reported := n.Exposure(); !reported {
		return fmt.Errorf("node %s is ready and has never reported its exposure; an exposure assertion over "+
			"it would pass while verifying nothing, which is the state a provider whose mapping forgot the "+
			"field is in", h)
	}
	return nil
}

func checkExposureMatchesCaps(ctx context.Context, p provider.Provider) error {
	req, h, n, err := readyConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = p.Destroy(ctx, h, req.RunID) }()

	// The same choice provision makes, from the same field, by calling the
	// same functions. Not a reimplementation: a second copy of the rule is a
	// second place for it to drift, and the exposure decision deliberately
	// lives above the providers because a provider checking its own exposure
	// is a provider grading its own homework.
	//
	// Both directions are covered by the choice itself. PublicSSH false is
	// the stricter check, so a provider author who forgets the field gets
	// AssertNoPublishedPorts here and a loud failure, rather than a public
	// port nobody looks at; and a provider that claims PublicSSH while
	// publishing nothing fails AssertSSHOnly for being unreachable.
	caps := p.Caps()
	if caps.PublicSSH {
		if err := exposure.AssertSSHOnly(n, caps.PlatformPorts...); err != nil {
			return fmt.Errorf("%s says Caps.PublicSSH and the node it reported does not satisfy the assertion "+
				"provision applies to that claim: %w", p.Name(), err)
		}
		return nil
	}
	if err := exposure.AssertNoPublishedPorts(n); err != nil {
		return fmt.Errorf("%s does not say Caps.PublicSSH, so provision holds it to the stricter assertion, "+
			"and the node it reported fails it: %w", p.Name(), err)
	}
	return nil
}

func checkReportedHostKeyIsTheInjectedOne(ctx context.Context, p provider.Provider) error {
	req, h, n, err := readyConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = p.Destroy(ctx, h, req.RunID) }()

	// Conditional on the provider having reported one, and that is not a
	// skip: the pin ember builds is made from the key it minted and never
	// from the key the provider reports, so a provider reporting nothing is
	// the correct and common case -- RunPod reports nothing at all. What is
	// checked is the only thing a reported key can be: the same key. A
	// provider answering with a different one has substituted something, and
	// continuing would pin the substitution.
	//
	// The local provider echoes the public half back on a container label,
	// which is exactly why reading the provider's answer first looked right
	// for a year and then dropped ember's first real cloud provision into
	// trust-on-first-use against a brand-new IP.
	if n.HostKey.IsZero() {
		return nil
	}
	if n.HostKey.Algo != req.HostKeyPublic.Algo || n.HostKey.Base64 != req.HostKeyPublic.Base64 {
		return fmt.Errorf("node %s reports host key %s %s but ember injected %s %s; a provider that answers "+
			"with a different key has substituted one, and a run that continued would pin the substitution",
			h, n.HostKey.Algo, truncKey(n.HostKey.Base64), req.HostKeyPublic.Algo, truncKey(req.HostKeyPublic.Base64))
	}
	// Authenticated rather than equal to HostKeyMinted, because a provider
	// that genuinely publishes the fingerprint is HostKeyFromAPI and that
	// path needs no injection -- the field exists for it. What is refused is
	// Unset, which makes the strict trust policy refuse later and further
	// away, and Pinned, which means a key ember had never seen was accepted
	// and recorded: trust-on-first-use against a fresh IP is a free MITM for
	// anyone on the path, and the provider's own Inspect is not where that
	// decision is made.
	if !n.HostKey.Source.Authenticated() {
		return fmt.Errorf("node %s reports key material with source %q, which is not an authenticated one; "+
			"the source is how a pin built from a key ember knows is told from one trusted on sight",
			h, n.HostKey.Source)
	}
	return nil
}

func checkDestroyUnknownIsSuccess(ctx context.Context, p provider.Provider) error {
	h := unknownHandle(p)
	// A run id is passed, because that is the shape every caller has: reap
	// reads it off the finding and `ember down` off the record. The machine
	// being gone must not stop the provider from sweeping what the run left
	// beside it, which is the whole reason the parameter exists rather than
	// being re-derived from the machine.
	if err := p.Destroy(ctx, h, node.NewRunID()); err != nil {
		return fmt.Errorf("Destroy of the unknown handle %s failed with %v; the goal of destroy is that the "+
			"machine is gone and it is, so a not-found that reports failure makes reap report that same failure "+
			"forever over a node nobody can remove", h, err)
	}
	return nil
}

func checkDestroyIsIdempotent(ctx context.Context, p provider.Provider) error {
	req, h, err := createConformanceNode(ctx, p)
	if err != nil {
		return err
	}

	if err := p.Destroy(ctx, h, req.RunID); err != nil {
		return fmt.Errorf("the first Destroy of %s failed: %w", h, err)
	}
	// The second call is where the original defect lived: there is no machine
	// left to read anything off, so everything Destroy still has to do has to
	// come from its arguments. local.Destroy used to take the run id off the
	// container's own label, and a container removed by anything but ember
	// made that inspect return not-found -- which Destroy treats as success,
	// so the run id stayed empty and Destroy returned nil over a bridge
	// network that was still there.
	if err := p.Destroy(ctx, h, req.RunID); err != nil {
		return fmt.Errorf("the second Destroy of %s failed with %v; by then there is no machine to read "+
			"anything off, so a provider that needs one is a provider that cannot finish cleaning up after "+
			"a node something else removed", h, err)
	}
	return nil
}

func checkDestroyAcceptsAnEmptyRunID(ctx context.Context, p provider.Provider) error {
	req, h, err := createConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = p.Destroy(ctx, h, req.RunID) }()

	// `ember down provider/id` on a node with no local record has no run id
	// to pass, and that is the documented empty case: a provider may then
	// clean up only what it can still see. It is not an error, because the
	// node is still the thing being destroyed and refusing would leave it
	// running.
	if err := p.Destroy(ctx, h, ""); err != nil {
		return fmt.Errorf("Destroy of %s with an empty run id failed with %v; that is `ember down` on a handle "+
			"with no record, and refusing it leaves the machine running", h, err)
	}
	return nil
}

func checkDestroyedNodeStopsBilling(ctx context.Context, p provider.Provider) error {
	req, h, err := createConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	if err := p.Destroy(ctx, h, req.RunID); err != nil {
		return fmt.Errorf("Destroy of %s: %w", h, err)
	}

	// Every state except StateDestroyed is billable, including Failed and
	// Stopped -- a stopped pod still bills for its volume. So a node that is
	// gone and still reported in any other state is a node `ember ls` prints
	// as ok while nothing is watching it, which is the exact reading that
	// makes a leak invisible.
	n, err := p.Inspect(ctx, h)
	switch {
	case errors.Is(err, node.ErrNotFound):
		// The machine is gone and the provider says so. Nothing to bill.
	case err != nil:
		return fmt.Errorf("Inspect of destroyed node %s failed with %v rather than wrapping node.ErrNotFound; "+
			"a caller cannot tell the machine being gone from the provider being unreachable", h, err)
	case n.State.Billable():
		return fmt.Errorf("Inspect of destroyed node %s still reports %q, which is billable; a node reported "+
			"as live after it was destroyed is one `ember ls` prints ok over", h, n.State)
	}

	listed, err := listIDs(ctx, p)
	if err != nil {
		return err
	}
	if listed[h.ID] {
		return fmt.Errorf("List still reports destroyed node %s; reap would act on it forever and `ember ls` "+
			"would price a machine that is not there", h)
	}
	return nil
}

func checkDestroyLeavesNoLeftovers(ctx context.Context, p provider.Provider) error {
	api, implements := p.(provider.LeftoverAPI)
	if !p.Caps().Leftovers {
		// Not a skip: what is asserted instead is the claim itself. A run
		// here creates one object, so there is nothing a sweep could find --
		// and a provider that implements the interface while saying this is
		// never swept, which is the direction that leaks a bridge network per
		// run until Docker has subnetted its whole address pool.
		if implements {
			return fmt.Errorf("%s implements provider.LeftoverAPI and does not say Caps.Leftovers, "+
				"so nothing will ever ask it what a destroyed node left behind", p.Name())
		}
		return nil
	}
	if !implements {
		return fmt.Errorf("%s says Caps.Leftovers and does not implement provider.LeftoverAPI; "+
			"the sweep would fail a type assertion at the moment it was about to run", p.Name())
	}

	req, h, err := createConformanceNode(ctx, p)
	if err != nil {
		return err
	}
	if err := p.Destroy(ctx, h, req.RunID); err != nil {
		return fmt.Errorf("Destroy of %s: %w", h, err)
	}

	// Nothing bills for a bridge network, which is why this leaked
	// invisibly; what it cost was every local run failing at once, weeks
	// later, with an error naming Docker and not ember.
	//
	// What this does NOT catch is the original defect itself, and that is
	// worth saying out loud. local.Destroy used to read the run id off the
	// container's own label and ignore the argument, which only goes wrong
	// once the container is gone -- removed by `docker rm`, a prune or a
	// crashed lab's cleanup. Staging that means removing the machine behind
	// the provider's back, and the Provider interface has no way to do it:
	// the only route would be a per-provider hook into each fake, which is a
	// hole in the suite's own premise that it holds a provider to its
	// interface. So that half stays where it can be staged honestly, beside
	// the provider and through its own seam --
	// local's TestDestroy_RemovesTheNetworkWhenTheContainerIsAlreadyGone --
	// and what is asserted here is the property that is observable from
	// outside: a destroyed run leaves nothing of itself behind.
	left, err := api.LeftoverList(ctx)
	if err != nil {
		return fmt.Errorf("LeftoverList after destroying %s: %w", h, err)
	}
	for _, l := range left {
		if l.RunID == req.RunID {
			return fmt.Errorf("after Destroy(%s, %s) a %s leftover named %q is still there; the run id was "+
				"passed in precisely so that this does not depend on the machine still being inspectable",
				h, req.RunID, l.Kind, l.Name)
		}
	}
	return nil
}

// createConformanceNode rents one node through the provider under test.
//
// The offer comes from the provider's own Offers rather than from a literal,
// because Create refuses an offer belonging to somebody else and because an
// offer ember invented is not one the provider said it had. That makes the
// create path under test the real one on every provider, with no per-provider
// knowledge here.
func createConformanceNode(ctx context.Context, p provider.Provider) (node.CreateRequest, node.Handle, error) {
	offers, err := p.Offers(ctx, node.Filter{})
	if err != nil {
		return node.CreateRequest{}, node.Handle{}, fmt.Errorf("Offers before creating a conformance node: %w", err)
	}
	if len(offers) == 0 {
		return node.CreateRequest{}, node.Handle{}, errors.New(
			"Offers returned nothing, so there is nothing to rent: the fake this provider is pointed at " +
				"has to serve a catalogue with capacity in it")
	}

	spec, err := node.NewSpec(node.SpecOptions{Image: ConformanceImage, TTL: ConformanceTTL})
	if err != nil {
		return node.CreateRequest{}, node.Handle{}, fmt.Errorf("build the conformance spec: %w", err)
	}
	req, err := node.NewCreateRequest(spec, offers[0], node.NewRunID(), time.Now(),
		conformanceAuthorized, conformanceHostKey, conformancePublic)
	if err != nil {
		return node.CreateRequest{}, node.Handle{}, fmt.Errorf("build the conformance create request: %w", err)
	}

	h, err := p.Create(ctx, req)
	if err != nil {
		return req, node.Handle{}, fmt.Errorf("Create a conformance node from offer %s: %w", offers[0].Token, err)
	}
	return req, h, nil
}

// readyConformanceNode rents a node and insists the provider reports it ready
// at once.
//
// No polling, deliberately. A suite that waited would be measuring the fake's
// clock rather than the provider's mapping, and a suite that gave up quietly
// when readiness did not arrive would report success for the exposure and
// host-key checks without having run them. So a provider wired to a fake that
// staggers readiness fails here, loudly, naming what the fake has to do --
// the staggering itself is a transition each provider's own tests cover,
// because RunPod leaving ssh.direct null for a while after the status reads
// RUNNING is a behaviour that cost money to find and belongs beside the fake
// that reproduces it.
func readyConformanceNode(ctx context.Context, p provider.Provider) (node.CreateRequest, node.Handle, node.Node, error) {
	req, h, err := createConformanceNode(ctx, p)
	if err != nil {
		return req, h, node.Node{}, err
	}

	n, err := p.Inspect(ctx, h)
	if err != nil {
		_ = p.Destroy(ctx, h, req.RunID)
		return req, h, node.Node{}, fmt.Errorf("Inspect the conformance node: %w", err)
	}
	if n.State != node.StateReady {
		_ = p.Destroy(ctx, h, req.RunID)
		return req, h, node.Node{}, fmt.Errorf(
			"node %s reports %q on the first Inspect and the conformance suite has nothing to wait on: "+
				"the fake this provider is pointed at must bring a node to %q at once, because a suite that "+
				"polled would be asserting the fake's clock",
			h, n.State, node.StateReady)
	}
	return req, h, n, nil
}

// unknownHandle names a machine no provider has.
//
// The id carries the word conformance rather than being random, so that a
// provider which somehow acted on it leaves a trace an operator can grep for.
func unknownHandle(p provider.Provider) node.Handle {
	return node.Handle{Provider: p.Name(), ID: "ember-conformance-no-such-machine"}
}

// listIDs is List as a set of ids, for the checks that compare before against
// after rather than assuming the fake started empty.
func listIDs(ctx context.Context, p provider.Provider) (map[string]bool, error) {
	ns, err := p.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("List: %w", err)
	}
	out := make(map[string]bool, len(ns))
	for _, n := range ns {
		out[n.Handle.ID] = true
	}
	return out, nil
}

// truncKey shortens a public key for an error message.
//
// Public material, so there is nothing to redact -- this is only so that a
// mismatch reads as a mismatch instead of two walls of base64 the reader has
// to diff by eye.
func truncKey(b64 string) string {
	const keep = 16
	if len(b64) <= keep {
		return b64
	}
	return b64[:keep] + "..."
}
