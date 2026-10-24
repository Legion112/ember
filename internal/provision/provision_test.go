package provision

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/node"
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
	p.HostKey = mintedKey(t)
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
	p.HostKey = mintedKey(t)

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
	p.HostKey = mintedKey(t)

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
	p.HostKey = mintedKey(t)
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
	p.HostKey = mintedKey(t)
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

func TestWithNode_PinsTheMintedHostKey(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	pair, err := hostkey.Mint()
	if err != nil {
		t.Fatal(err)
	}
	p.HostKey = pair.NodeKey()

	err = WithNode(t.Context(), p, spec(t), opts(t), func(_ context.Context, s Session) error {
		// The trust must accept the key the provider reported and refuse any
		// other, without ever having connected.
		if cbErr := hostkey.Callback(s.Trust, "n1")("h", nil, pair.Public); cbErr != nil {
			t.Errorf("the minted key was refused: %v", cbErr)
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
}

// TestWithNode_FallsBackToTOFUWithoutAHostKey, and says so loudly enough that
// `ember ls` can report which nodes were never authenticated.
func TestWithNode_FallsBackToTOFUWithoutAHostKey(t *testing.T) {
	p := providertest.NewRecordingProvider("test") // HostKey left zero
	o := opts(t)

	err := WithNode(t.Context(), p, spec(t), o, func(_ context.Context, s Session) error {
		first, _ := hostkey.Mint()
		if cbErr := hostkey.Callback(s.Trust, "n1")("h", nil, first.Public); cbErr != nil {
			t.Errorf("the first key should pin: %v", cbErr)
		}
		second, _ := hostkey.Mint()
		if cbErr := hostkey.Callback(s.Trust, "n1")("h", nil, second.Public); cbErr == nil {
			t.Error("a rotated key was accepted after the pin")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWithNode_RefusesTOFUWithNoPinStore(t *testing.T) {
	p := providertest.NewRecordingProvider("test")
	o := opts(t)
	o.Pins = nil

	err := WithNode(t.Context(), p, spec(t), o, func(context.Context, Session) error { return nil })
	if err == nil {
		t.Fatal("WithNode proceeded with nothing to verify the host key against")
	}
	if live := p.Live(); len(live) != 0 {
		t.Error("the node was not destroyed after the trust decision failed")
	}
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
	p.HostKey = mintedKey(t)
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
	p.HostKey = mintedKey(t)
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
