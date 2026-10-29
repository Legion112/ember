package provision

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
	"github.com/Legion112/ember/internal/statestore"
)

func spec(t *testing.T) node.Spec {
	t.Helper()
	s, err := node.NewSpec(node.SpecOptions{
		Image: "ghcr.io/example/comfyui:v1",
		TTL:   time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func opts(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	store, err := statestore.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	pins, err := hostkey.OpenStore(filepath.Join(dir, "pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := keys.EnsureIdentity(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Store: store, Pins: pins, Identity: id,
		ReadyTimeout: time.Minute, PollEvery: time.Second,
	}
}

func mintedKey(t *testing.T) node.HostKey {
	t.Helper()
	p, err := hostkey.Mint()
	if err != nil {
		t.Fatal(err)
	}
	return p.NodeKey()
}

func TestWithNode_RunsTheClosureAndDestroysAfterwards(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	o := opts(t)

	var sawEndpoint string
	err := WithNode(t.Context(), p, spec(t), o, func(_ context.Context, s Session) error {
		sawEndpoint = s.Node.Endpoint().String()
		if s.Trust == nil {
			t.Error("the session carries no host key policy")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawEndpoint == "" {
		t.Error("the closure never saw a node")
	}
	if live := p.Live(); len(live) != 0 {
		t.Fatalf("%d node(s) survived the run", len(live))
	}
	// A completed run leaves no record: the node is gone and accounted for.
	recs, err := o.Store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("a clean run left %d records behind", len(recs))
	}
}

// TestWithNode_DestroysEvenWhenTheClosureFails. The node's lifetime is the
// closure's, not the happy path's.
func TestWithNode_DestroysEvenWhenTheClosureFails(t *testing.T) {
	p := providertest.NewRecordingProvider("test")

	boom := errors.New("the workload exploded")
	err := WithNode(t.Context(), p, spec(t), opts(t), func(context.Context, Session) error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the closure's error", err)
	}
	if live := p.Live(); len(live) != 0 {
		t.Fatalf("a failing closure leaked %d node(s)", len(live))
	}
}

// TestWithNode_DestroysAfterCancellation is the single most important test in
// this package. Ctrl-C cancels ctx, and a destroy call on a cancelled context
// fails instantly -- which is the most likely way to leak a GPU. The defer
// runs on context.WithoutCancel precisely so this passes.
func TestWithNode_DestroysAfterCancellation(t *testing.T) {
	p := providertest.NewRecordingProvider("test")

	ctx, cancel := context.WithCancel(t.Context())
	err := WithNode(ctx, p, spec(t), opts(t), func(context.Context, Session) error {
		cancel() // the operator hits Ctrl-C mid-run
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if live := p.Live(); len(live) != 0 {
		t.Fatalf("cancellation leaked %d node(s); the destroy context was cancelled too", len(live))
	}
}

// TestWithNode_RecordsIntentBeforeCreating: the window between asking for a
// machine and learning its id is where an orphan with no local trace is born.
func TestWithNode_RecordsIntentBeforeCreating(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.FailOn = "Create"
	o := opts(t)

	if err := WithNode(t.Context(), p, spec(t), o, func(context.Context, Session) error {
		t.Error("the closure ran despite a failed create")
		return nil
	}); err == nil {
		t.Fatal("WithNode reported success after Create failed")
	}

	recs, err := o.Store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want the intent record to survive a failed create", len(recs))
	}
	r := recs[0]
	if r.Phase != statestore.PhaseIntent {
		t.Errorf("phase = %q, want intent", r.Phase)
	}
	if r.NodeID != "" {
		t.Error("an intent record must not claim a node id it never learned")
	}
	if r.LastErr == "" {
		t.Error("the failure should be recorded so ls can explain the record")
	}
	if !r.Phase.Billable() {
		t.Error("an intent record is billable: the call failing does not mean nothing was created")
	}
}

// TestWithNode_DestroyFailureIsSticky. The machine may still be running, and
// only the operator can say otherwise.
func TestWithNode_DestroyFailureIsSticky(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Errors["Destroy n1"] = errors.New("provider unreachable")
	o := opts(t)

	err := WithNode(t.Context(), p, spec(t), o, func(context.Context, Session) error { return nil })
	if err == nil {
		t.Fatal("a failed destroy reported success")
	}

	recs, err := o.Store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want the destroy_failed record kept", len(recs))
	}
	if recs[0].Phase != statestore.PhaseDestroyFailed {
		t.Errorf("phase = %q, want destroy_failed", recs[0].Phase)
	}
	if recs[0].LastErr == "" {
		t.Error("the destroy failure should say what went wrong")
	}
}

// TestWithNode_AMissingNodeDestroysCleanly: destroy is idempotent, so a node
// the provider already lost is a success, not a sticky failure.
func TestWithNode_AMissingNodeDestroysCleanly(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Errors["Destroy n1"] = node.ErrNotFound
	o := opts(t)

	if err := WithNode(t.Context(), p, spec(t), o, func(context.Context, Session) error { return nil }); err != nil {
		t.Fatalf("a node that was already gone should destroy cleanly: %v", err)
	}
	recs, _ := o.Store.List()
	if len(recs) != 0 {
		t.Errorf("record not cleared: %+v", recs)
	}
}

// TestWithNode_PinsTheKeyEmberMintedNotTheOneTheProviderReports.
//
// ember generates the host key and injects it through the create call, so it
// already knows which key the node must present and never has to ask the
// provider. Reading the provider's answer first was a real bug: the local
// provider echoes the key back on a container label so it worked there, while
// RunPod reports nothing, and ember's first real cloud provision fell through
// to trust-on-first-use against a brand-new IP -- the one case TOFU cannot
// defend, and the exact thing minting the key exists to avoid.
func TestWithNode_PinsTheKeyEmberMintedNotTheOneTheProviderReports(t *testing.T) {
	p := providertest.NewRecordingProvider("test")

	var sawWarning bytes.Buffer
	o := opts(t)
	o.Log = slog.New(slog.NewTextHandler(&sawWarning, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := WithNode(t.Context(), p, spec(t), o, func(_ context.Context, s Session) error {
		injected := p.InjectedKey("n1")
		if injected.IsZero() {
			t.Fatal("the fake recorded no injected key")
		}
		pub := parseHostKey(t, injected)

		// The trust accepts the injected key and refuses any other, without
		// ever having connected.
		if cbErr := hostkey.Callback(s.Trust, "n1")("h", nil, pub); cbErr != nil {
			t.Errorf("the injected key was refused: %v", cbErr)
		}
		other, _ := hostkey.Mint()
		if cbErr := hostkey.Callback(s.Trust, "n1")("h", nil, other.Public); cbErr == nil {
			t.Error("a different host key was accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// And it is a pin, not trust-on-first-use.
	if strings.Contains(sawWarning.String(), "first use") {
		t.Errorf("ember fell back to TOFU despite minting a key:\n%s", sawWarning.String())
	}
}

// TestWithNode_RefusesAProviderThatSubstitutesTheHostKey. A provider handing
// back a different key than the one ember supplied has swapped something, and
// continuing would pin the swap.
func TestWithNode_RefusesAProviderThatSubstitutesTheHostKey(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	// An unrelated key, reported as though it were the node's.
	p.HostKey = mintedKey(t)

	err := WithNode(t.Context(), p, spec(t), opts(t), func(context.Context, Session) error {
		t.Error("the workload ran against a substituted host key")
		return nil
	})
	if err == nil {
		t.Fatal("a substituted host key was accepted")
	}
	if !strings.Contains(err.Error(), "substituted") {
		t.Errorf("got %v", err)
	}
	if live := p.Live(); len(live) != 0 {
		t.Errorf("%d node(s) survived", len(live))
	}
}

// parseHostKey turns ember's wire form back into something a callback takes.
func parseHostKey(t *testing.T, k node.HostKey) ssh.PublicKey {
	t.Helper()
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Algo + " " + k.Base64))
	if err != nil {
		t.Fatalf("parse %s %s: %v", k.Algo, k.Base64, err)
	}
	return pub
}

func TestWithNode_RefusesAnUnparseableHostKey(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.HostKey = node.HostKey{Source: node.HostKeyMinted, Algo: "ssh-ed25519", Base64: "!!!"}

	if err := WithNode(t.Context(), p, spec(t), opts(t), func(context.Context, Session) error {
		t.Error("the closure ran with an unusable host key")
		return nil
	}); err == nil {
		t.Fatal("an unparseable host key was accepted")
	}
	if live := p.Live(); len(live) != 0 {
		t.Error("the node leaked")
	}
}

func TestWithNode_RefusesWithoutAStore(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	o := opts(t)
	o.Store = nil
	if err := WithNode(t.Context(), p, spec(t), o, func(context.Context, Session) error { return nil }); err == nil {
		t.Fatal("WithNode ran with nowhere to record the node")
	}
	if len(p.Recorded()) != 0 {
		t.Error("it talked to the provider before checking it could record the result")
	}
}

// TestWaitReady_PollsUntilTheNodeIsUsable uses synctest, so a ten-minute
// timeout and a two-second poll are exercised instantly and deterministically
// without a Clock interface existing anywhere in this package.
func TestWaitReady_PollsUntilTheNodeIsUsable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := providertest.NewRecordingProvider("test")
		p.States = []node.State{
			node.StatePending,
			node.StateProvisioning,
			node.StateProvisioning,
			node.StateReady,
		}
		o := Options{ReadyTimeout: 10 * time.Minute, PollEvery: 2 * time.Second}

		start := time.Now()
		ready, err := WaitReady(t.Context(), p, node.Handle{Provider: "test", ID: "n1"}, o)
		if err != nil {
			t.Fatal(err)
		}
		if ready.Handle().ID != "n1" {
			t.Errorf("handle = %v", ready.Handle())
		}
		// Three gaps before the fourth Inspect returned ready.
		if got, want := time.Since(start), 6*time.Second; got != want {
			t.Errorf("waited %v, want %v", got, want)
		}
	})
}

func TestWaitReady_GivesUpAtTheTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := providertest.NewRecordingProvider("test")
		p.States = []node.State{node.StateProvisioning}
		o := Options{ReadyTimeout: 30 * time.Second, PollEvery: time.Second}

		_, err := WaitReady(t.Context(), p, node.Handle{Provider: "test", ID: "n1"}, o)
		if err == nil {
			t.Fatal("WaitReady waited forever")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("want a deadline error, got %v", err)
		}
		if !strings.Contains(err.Error(), "30s") {
			t.Errorf("the message should name the timeout, got %v", err)
		}
	})
}

// TestWaitReady_KeepsPollingWhileRunningButUnreachable: a provider reports
// running before it has published a port, and that transient must not be read
// as a failure.
func TestWaitReady_KeepsPollingWhileRunningButUnreachable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := providertest.NewRecordingProvider("test")
		p.States = []node.State{node.StateReady, node.StateReady}
		p.Endpoint = node.Endpoint{Host: "1.2.3.4", Port: 0, User: "root"} // no port yet
		o := Options{ReadyTimeout: 5 * time.Second, PollEvery: time.Second}

		_, err := WaitReady(t.Context(), p, node.Handle{Provider: "test", ID: "n1"}, o)
		if err == nil {
			t.Fatal("a node that never published a port was reported ready")
		}
		if len(p.Recorded()) < 3 {
			t.Errorf("it should have kept polling, made %d calls", len(p.Recorded()))
		}
	})
}

func TestWaitReady_StopsOnTerminalStates(t *testing.T) {
	for name, tc := range map[string]struct {
		state   node.State
		wantErr error
	}{
		"interrupted": {node.StateInterrupted, node.ErrInterrupted},
		"failed":      {node.StateFailed, nil},
		"destroyed":   {node.StateDestroyed, nil},
	} {
		p := providertest.NewRecordingProvider("test")
		p.States = []node.State{tc.state}
		o := Options{ReadyTimeout: time.Minute, PollEvery: time.Millisecond}

		_, err := WaitReady(t.Context(), p, node.Handle{Provider: "test", ID: "n1"}, o)
		if err == nil {
			t.Errorf("%s: WaitReady kept waiting for a node that will never be ready", name)
			continue
		}
		if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: got %v, want %v", name, err, tc.wantErr)
		}
	}
}

func TestWaitReady_ReportsAnInspectFailure(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.FailOn = "Inspect"
	o := Options{ReadyTimeout: time.Minute, PollEvery: time.Millisecond}
	if _, err := WaitReady(t.Context(), p, node.Handle{Provider: "test", ID: "n1"}, o); err == nil {
		t.Fatal("WaitReady ignored a provider failure")
	}
}

func TestPickOffer_RefusesWhatIsOverBudget(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.OfferList = []node.Offer{
		{Provider: "test", Token: "dear", GPU: "h100", GPUCount: 1, Price: 900000},
		{Provider: "test", Token: "cheap", GPU: "h100", GPUCount: 1, Price: 100000},
	}
	o := opts(t)
	o.Budget = Budget{MaxRate: 200000}

	got, err := PickOffer(t.Context(), p, spec(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "cheap" {
		t.Errorf("picked %q, want the one within budget", got.Token)
	}

	o.Budget = Budget{MaxRate: 1}
	if _, err := PickOffer(t.Context(), p, spec(t), o); !errors.Is(err, node.ErrBudget) {
		t.Errorf("got %v, want ErrBudget", err)
	}
}

func TestPickOffer_EnforcesTheWholeRunCeiling(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.OfferList = []node.Offer{{Provider: "test", Token: "t", GPU: "h100", GPUCount: 1, Price: 1000000}}
	o := opts(t)
	// $1/hr for an hour is $1; a 50-cent run ceiling must refuse it even
	// though the hourly rate is fine.
	o.Budget = Budget{MaxRate: 2000000, MaxRun: 500000}

	if _, err := PickOffer(t.Context(), p, spec(t), o); !errors.Is(err, node.ErrBudget) {
		t.Errorf("got %v, want ErrBudget", err)
	}
}

// TestPickOffer_SkipsTheBudgetWhenTheProviderIsFree, honestly rather than by
// comparing a real price against zero.
func TestPickOffer_SkipsTheBudgetWhenTheProviderIsFree(t *testing.T) {
	p := providertest.NewRecordingProvider("local")
	p.Capabilities.Free = true
	p.OfferList = []node.Offer{{Provider: "local", Token: "local", GPU: "none", GPUCount: 1, Price: 0}}
	o := opts(t)
	o.Budget = Budget{MaxRate: 1, MaxRun: 1}

	if _, err := PickOffer(t.Context(), p, spec(t), o); err != nil {
		t.Fatalf("a free provider was refused on budget: %v", err)
	}
}

func TestPickOffer_ReportsNoCapacity(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.OfferList = nil
	if _, err := PickOffer(t.Context(), p, spec(t), opts(t)); !errors.Is(err, node.ErrNoCapacity) {
		t.Errorf("got %v, want ErrNoCapacity", err)
	}

	p.FailOn = "Offers"
	if _, err := PickOffer(t.Context(), p, spec(t), opts(t)); err == nil {
		t.Error("a failed Offers call was treated as no capacity")
	}
}

func TestBudget_ZeroMeansNoCeiling(t *testing.T) {
	var b Budget
	o := node.Offer{Price: node.MaxRate}
	if err := b.Allow(o, time.Hour); err != nil {
		t.Errorf("an unset budget should allow anything: %v", err)
	}
}

func TestOptions_Defaults(t *testing.T) {
	var o Options
	if o.readyTimeout() != defaultReadyTimeout || o.pollEvery() != defaultPollEvery ||
		o.destroyTimeout() != defaultDestroyTimeout {
		t.Error("zero values must fall back, not mean \"never\"")
	}
	if o.now().IsZero() {
		t.Error("now() returned the zero time")
	}
	if o.log() == nil {
		t.Error("log() must never be nil")
	}
}

// TestOptions_InjectedCollaboratorsAreUsed covers the non-default branches of
// the small accessors. They are two lines each and exactly the kind of thing
// that gets inverted in a refactor, silently sending every log line nowhere.
func TestOptions_InjectedCollaboratorsAreUsed(t *testing.T) {
	var sb strings.Builder
	fixed := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	o := Options{
		Log:            slog.New(slog.NewTextHandler(&sb, nil)),
		Now:            func() time.Time { return fixed },
		DestroyTimeout: 7 * time.Second,
	}
	if !o.now().Equal(fixed) {
		t.Error("the injected clock was ignored")
	}
	if o.destroyTimeout() != 7*time.Second {
		t.Error("the injected destroy timeout was ignored")
	}
	o.log().Info("hello")
	if !strings.Contains(sb.String(), "hello") {
		t.Error("the injected logger was ignored")
	}
}

// TestWithNode_LogsReadinessAndDestruction: these lines are how an operator
// watching a run knows which node is theirs and that it went away.
func TestWithNode_LogsReadinessAndDestruction(t *testing.T) {
	var sb strings.Builder
	p := providertest.NewRecordingProvider("test")
	o := opts(t)
	o.Log = slog.New(slog.NewTextHandler(&sb, nil))

	if err := WithNode(t.Context(), p, spec(t), o, func(context.Context, Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"node ready", "node destroyed", "deadline", "price"} {
		if !strings.Contains(out, want) {
			t.Errorf("the run log is missing %q:\n%s", want, out)
		}
	}
}

// TestWithNode_SurvivesAStoreThatCannotRecordReadiness. A state file that
// cannot be updated must not abandon a running node -- the machine exists and
// still has to be destroyed.
func TestWithNode_SurvivesAStoreThatCannotRecordReadiness(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	p := providertest.NewRecordingProvider("test")
	o := opts(t)

	ran := false
	err := WithNode(t.Context(), p, spec(t), o, func(context.Context, Session) error {
		ran = true
		// Make every later write fail, including the destroy bookkeeping.
		if err := os.Chmod(o.Store.Dir(), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(o.Store.Dir(), 0o700) })
		return nil
	})
	if !ran {
		t.Fatal("the closure never ran")
	}
	if err == nil {
		t.Error("a failure to record the destruction should be reported, not swallowed")
	}
	// The machine itself must still be gone.
	if live := p.Live(); len(live) != 0 {
		t.Errorf("the node leaked because the state file was unwritable")
	}
}

// TestWithNode_DestroysANodeExposedMoreWidelyThanAsked is the test the whole
// exposure package exists for. A provider that publishes an unauthenticated
// 8188/http proxy -- RunPod's real behaviour for any port declared http, in
// front of a ComfyUI that has arbitrary code execution -- must have its node
// destroyed before the workload ever starts. Warning and continuing would
// leave a public shell up for as long as the operator takes to read their
// terminal.
func TestWithNode_DestroysANodeExposedMoreWidelyThanAsked(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Capabilities = provider.Caps{HostKeyFromAPI: true, PublicSSH: true, Free: true}
	p.Exposed = []node.Exposed{
		{Port: 22, Proto: node.ProtoTCP, URL: "203.0.113.7:40022"},
		{Port: 8188, Proto: node.ProtoHTTP, URL: "https://abc-8188.proxy.runpod.net"},
	}

	ran := false
	err := WithNode(t.Context(), p, spec(t), opts(t), func(context.Context, Session) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("a node with a public HTTP proxy port was handed to the workload")
	}
	if ran {
		t.Error("the workload ran on a node that was publicly reachable")
	}

	var v *exposure.Violation
	if !errors.As(err, &v) {
		t.Fatalf("got %T (%v), want *exposure.Violation", err, err)
	}
	if !strings.Contains(err.Error(), "abc-8188.proxy.runpod.net") {
		t.Errorf("the error does not name the public address: %v", err)
	}
	if live := p.Live(); len(live) != 0 {
		t.Fatalf("%d publicly exposed node(s) survived", len(live))
	}
}

// TestWithNode_DestroysANodeWithNoExposureReport. A provider whose Inspect
// does not populate exposure cannot be verified, and an unverifiable node is
// treated as a wrong one. The alternative -- assuming the provider is fine
// because it said nothing -- is how the check would come to mean nothing.
func TestWithNode_DestroysANodeWithNoExposureReport(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Exposed = []node.Exposed{} // reported, but see below

	// Force the unreported case: a node built without WithExposed at all.
	p.SuppressExposure = true

	err := WithNode(t.Context(), p, spec(t), opts(t), func(context.Context, Session) error {
		t.Error("the workload ran on an unverifiable node")
		return nil
	})
	if !errors.Is(err, exposure.ErrUnreported) {
		t.Fatalf("got %v, want ErrUnreported", err)
	}
	if live := p.Live(); len(live) != 0 {
		t.Fatalf("%d unverified node(s) survived", len(live))
	}
}

// TestWithNode_AProviderThatPublishesNothingPassesTheStrictCheck, which is the
// local provider's shape and the default for any provider that forgets to
// declare PublicSSH.
func TestWithNode_AProviderThatPublishesNothingPassesTheStrictCheck(t *testing.T) {
	p := providertest.NewRecordingProvider("test")

	ran := false
	if err := WithNode(t.Context(), p, spec(t), opts(t), func(context.Context, Session) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("a node publishing nothing was refused: %v", err)
	}
	if !ran {
		t.Error("the closure never ran")
	}
}

// TestWithNode_RefusesAPublicSSHProviderThatPublishesNothing. Not a security
// failure, but the node is unreachable and the next error the operator would
// see is an unexplained dial timeout.
func TestWithNode_RefusesAPublicSSHProviderThatPublishesNothing(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Capabilities = provider.Caps{HostKeyFromAPI: true, PublicSSH: true, Free: true}
	p.Exposed = []node.Exposed{}

	err := WithNode(t.Context(), p, spec(t), opts(t), func(context.Context, Session) error {
		t.Error("the workload ran on an unreachable node")
		return nil
	})
	if err == nil {
		t.Fatal("a provider claiming public SSH published nothing and was accepted")
	}
	if !strings.Contains(err.Error(), "no public SSH port") {
		t.Errorf("got %v", err)
	}
}

// TestBillFor_SurvivesAStoreItCannotRead. A failure to estimate the cost is
// not a failure to destroy: the node is gone, which is the thing that
// mattered, and a missing cost line is worth far less than the teardown it
// would otherwise abort.
func TestBillFor_SurvivesAStoreItCannotRead(t *testing.T) {
	o := opts(t)

	// A run id the store has never heard of.
	if got := billFor(o, "ffffffffffffffff", time.Minute); got.Cost != 0 {
		t.Errorf("an unreadable record produced a cost: %v", got)
	}

	// And no store at all, which is how the dry-run and test paths arrive.
	if got := billFor(Options{}, "0123456789abcdef", time.Minute); got.Cost != 0 {
		t.Errorf("a missing store produced a cost: %v", got)
	}
}

// TestBillFor_EstimatesFromTheRecordEmberWrote, which is the only place the
// quoted rate and the start time are both known.
func TestBillFor_EstimatesFromTheRecordEmberWrote(t *testing.T) {
	o := opts(t)
	rec := statestore.Record{
		RunID: "0123456789abcdef", Provider: "test", Phase: statestore.PhaseReady,
		Label: "ember-0123456789abcdef-1", Price: node.Rate(2_690_000),
		Created: time.Now().Add(-30 * time.Minute),
	}
	if err := o.Store.Create(rec); err != nil {
		t.Fatal(err)
	}

	got := billFor(o, rec.RunID, time.Minute)
	if got.Free() {
		t.Fatal("a priced record estimated as free")
	}
	// 30 minutes at $2.69/hr, give or take the test's own elapsed time.
	if got.Cost < node.MicroUSD(1_300_000) || got.Cost > node.MicroUSD(1_400_000) {
		t.Errorf("cost = %v, want about $1.345", got.Cost)
	}
}

// TestWithNode_ReportsTheCostOnTeardown. The figure is read from the record
// before it is forgotten, which is the one moment both halves are in one
// place.
func TestWithNode_ReportsTheCostOnTeardown(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	p.Capabilities = provider.Caps{HostKeyFromAPI: true, PublicSSH: true, MinBilling: time.Minute}
	p.OfferList = []node.Offer{{
		Provider: "test", Token: "h100", GPU: node.NormalizeGPU("h100"),
		GPUCount: 1, Price: node.Rate(2_690_000),
	}}

	var logged bytes.Buffer
	o := opts(t)
	o.Log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))
	o.Budget = Budget{}

	if err := WithNode(t.Context(), p, spec(t), o, func(context.Context, Session) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out := logged.String()
	if !strings.Contains(out, "node destroyed") {
		t.Fatalf("the teardown was not logged:\n%s", out)
	}
	// The cost is on the destroy line, because that is the line an operator
	// reads after a run and the only place the total exists.
	if !strings.Contains(out, "cost=") {
		t.Errorf("the destroy line carries no cost:\n%s", out)
	}
}

// readyNode builds a ReadyNode for testing trustFor directly.
func readyNode(t *testing.T, reported node.HostKey) node.ReadyNode {
	t.Helper()
	n := node.Node{
		Handle:  node.Handle{Provider: "test", ID: "n1"},
		State:   node.StateReady,
		HostKey: reported,
	}.WithEndpoint(node.Endpoint{Host: "127.0.0.1", Port: 22, User: "root"})

	r, err := node.Ready(n)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestTrustFor_RefusesANodeWithNothingToVerifyAgainst.
//
// Unreachable through WithNode, which mints a key for every node and cannot
// build a CreateRequest without one. Tested directly because it is the guard
// that matters if that ever changes: the alternative to refusing here is
// trust-on-first-use against an address that has never had a key, which
// authenticates nothing.
func TestTrustFor_RefusesANodeWithNothingToVerifyAgainst(t *testing.T) {
	_, err := trustFor(readyNode(t, node.HostKey{}), opts(t), node.HostKey{})
	if err == nil {
		t.Fatal("a node with no host key at all was accepted")
	}
	if !strings.Contains(err.Error(), "will not connect") {
		t.Errorf("got %v", err)
	}
}

// TestTrustFor_RefusesAMintedKeyItCannotParse. A key ember cannot read is one
// it cannot pin, and connecting anyway would verify nothing.
func TestTrustFor_RefusesAMintedKeyItCannotParse(t *testing.T) {
	bad := node.HostKey{Source: node.HostKeyMinted, Algo: "ssh-ed25519", Base64: "!!!"}
	_, err := trustFor(readyNode(t, node.HostKey{}), opts(t), bad)
	if err == nil {
		t.Fatal("an unparseable minted key was accepted")
	}
	if !strings.Contains(err.Error(), "minted") {
		t.Errorf("the error does not say which key failed: %v", err)
	}
}

// TestTrustFor_PrefersTheMintedKeyOverTheProvidersWhenTheyAgree, which is the
// ordinary case: a provider supporting injection echoes back what it was
// given.
func TestTrustFor_PrefersTheMintedKeyOverTheProvidersWhenTheyAgree(t *testing.T) {
	pair, err := hostkey.Mint()
	if err != nil {
		t.Fatal(err)
	}
	k := pair.NodeKey()

	trust, err := trustFor(readyNode(t, k), opts(t), k)
	if err != nil {
		t.Fatal(err)
	}
	if cbErr := hostkey.Callback(trust, "n1")("h", nil, pair.Public); cbErr != nil {
		t.Errorf("the agreed key was refused: %v", cbErr)
	}
}
