package queue

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/local"
)

// One store is filled by one slot at a time (#204).
//
// At one rental two plans were SERIAL, so the second could not be told a store
// was empty that the first had just filled. With max_nodes>1 they run at once,
// and the duplicate is a second $0.06/hour machine fetching the same
// fifty-six gigabytes -- which is what `TestStore_CreateMountRunDestroyRemove`
// refuses one layer up, with ONE asset fetch across two runs as its
// discriminator.

// TestFill_SerialisesOneStoreAndNotTwo.
//
// THE UNIT, because the Plan-level test below cannot tell "it waited" from
// "it was quick". Two fills of ONE store must not overlap; two fills of
// DIFFERENT stores must, or the lock would serialise a fleet's whole fetching
// for no reason -- which is the assertion that stops this being satisfied by a
// single global lock.
func TestFill_SerialisesOneStoreAndNotTwo(t *testing.T) {
	f := &StoreFills{}

	var mu sync.Mutex
	inside := map[string]int{}
	peak := map[string]int{}

	enter := func(store string) {
		mu.Lock()
		defer mu.Unlock()
		inside[store]++
		if inside[store] > peak[store] {
			peak[store] = inside[store]
		}
	}
	leave := func(store string) {
		mu.Lock()
		defer mu.Unlock()
		inside[store]--
	}

	var wg sync.WaitGroup
	for _, store := range []string{"models", "models", "voices", "voices"} {
		wg.Add(1)
		go func(store string) {
			defer wg.Done()
			_ = f.Fill(context.Background(), store, func() error {
				enter(store)
				time.Sleep(30 * time.Millisecond)
				leave(store)
				return nil
			})
		}(store)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("four fills did not finish in 20s")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, store := range []string{"models", "voices"} {
		if peak[store] != 1 {
			t.Errorf("%d fills of %q were in flight at once, want 1: two slots both renting a "+
				"machine to fetch one store's weights is the duplicate this exists to remove",
				peak[store], store)
		}
	}
}

// TestFill_TwoStoresDoNotWaitForEachOther is the other half, and without it
// the test above is satisfied by one global lock.
//
// A fleet's whole point is work that cannot share a node, which is often work
// that cannot share a store either. Serialising those against each other would
// turn two independent fetches into one queue for no reason at all.
//
// THE FIRST VERSION OF THIS PASSED AGAINST A GLOBAL LOCK, which is the
// expected-value trap: it waited twenty seconds for the second fill to start
// and the held fill gave up after ten, so a global lock delivered the second
// at t=10s -- INSIDE the deadline, and green. The two bounds are now an order
// of magnitude apart in the other direction: the fill is held until the test
// says otherwise (a minute, which only a wedge reaches) and the second has
// three seconds to arrive. With one lock per store it arrives at once; with a
// global one it cannot arrive until the first is released, which this test
// never does before it has looked.
func TestFill_TwoStoresDoNotWaitForEachOther(t *testing.T) {
	f := &StoreFills{}
	both := make(chan struct{}, 2)
	release := make(chan struct{})

	var wg sync.WaitGroup
	for _, store := range []string{"models", "voices"} {
		wg.Add(1)
		go func(store string) {
			defer wg.Done()
			_ = f.Fill(context.Background(), store, func() error {
				both <- struct{}{}
				select {
				case <-release:
				case <-time.After(time.Minute):
				}
				return nil
			})
		}(store)
	}

	// BOTH WITHIN THREE SECONDS, AND NEITHER FILL HAS BEEN RELEASED. A global
	// lock cannot deliver the second here at all, because the first is still
	// inside its own fn.
	deadline := time.After(3 * time.Second)
	for i := range 2 {
		select {
		case <-both:
		case <-deadline:
			close(release)
			wg.Wait()
			t.Fatalf("only %d of two different stores was being filled after 3s with neither "+
				"fill released, so the lock is global rather than per store: a fleet's two "+
				"independent fetches would be one queue", i)
		}
	}
	close(release)
	wg.Wait()
}

// TestFill_AWaitIsAbandonableAndNotAMutex.
//
// A `sync.Mutex` cannot be abandoned, so a slot waiting on another slot's
// fetch would hold up `queue.Run`'s shutdown for the length of it -- measured
// at eighty-one minutes for 56 GiB over a home link, on a process whose whole
// shutdown story is that it waits for every slot. So the wait is a select on
// the caller's own context, and this is the exit code behind that sentence.
//
// IT CARRIES ITS OWN DEADLINE, because what it tests is a wait: a mutex would
// hang here rather than fail, and a wedge prints no `--- FAIL`.
func TestFill_AWaitIsAbandonableAndNotAMutex(t *testing.T) {
	f := &StoreFills{}
	held := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = f.Fill(context.Background(), "models", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	ctx, cancel := context.WithCancel(context.Background())
	ran := false
	out := make(chan error, 1)
	go func() {
		out <- f.Fill(ctx, "models", func() error { ran = true; return nil })
	}()
	cancel()

	select {
	case err := <-out:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled wait returned %v, want the caller's own cancellation: a mutex "+
				"would have returned nothing at all and held up the shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a cancelled wait did not return, so the lock cannot be abandoned and a " +
			"shutdown waits out another slot's fetch")
	}
	if ran {
		t.Error("the abandoned fill ran anyway")
	}

	close(release)
	wg.Wait()
}

// TestFill_ANilSerialisesNothingAndStillRuns, which is what max_nodes: 1 and
// every literal in the suite get.
func TestFill_ANilSerialisesNothingAndStillRuns(t *testing.T) {
	var f *StoreFills
	ran := 0
	for range 2 {
		if err := f.Fill(t.Context(), "models", func() error { ran++; return nil }); err != nil {
			t.Fatalf("a nil StoreFills refused a fill: %v", err)
		}
	}
	if ran != 2 {
		t.Errorf("a nil StoreFills ran %d fill(s), want 2", ran)
	}
	// AND THE ERROR REACHES THE CALLER, so a nil is not a bypass of the fill
	// itself: what it drops is the serialisation.
	want := errors.New("the fetch failed")
	if err := f.Fill(t.Context(), "models", func() error { return want }); !errors.Is(err, want) {
		t.Errorf("a nil StoreFills swallowed the fill's error: %v", err)
	}
}

// TestAppPlanner_TwoSlotsDoNotBothRentAMachineToFillOneStore is the join, and
// the only thing here that can see the production wiring.
//
// THE DISCRIMINATOR IS THE FAKE'S OWN CPU CATALOGUE. A fill's first act after
// the gates is `app.NewPlan` with CPUOnly, which reads `/v2/catalog/cpus` --
// so a handler that counts how many of those are in flight at once counts
// fills in flight at once, which is `rentalGate`'s shape with an HTTP handler
// in place of a rental. The handler blocks until the test lets it go, so
// without the lock both plans are certainly inside it together: the red is
// deterministic rather than raced.
//
// BOTH PLANS REACH THE FILL, which is what stops this being satisfied by a
// planner that fills nothing: the store is empty for both, and the gate sees
// at least one.
//
// AND THE SECOND ASSERTION IS THE RE-ASK, WHICH THE FIRST CANNOT SEE. A
// planner that serialised and then filled anyway keeps the peak at ONE and
// fetches the weights twice -- which is the whole cost, serially instead of
// concurrently. Measured: with the re-ask deleted, the peak assertion is
// GREEN. So the fake's store FILLS ITSELF the moment a fill starts, which is
// what "another slot put the weights there" looks like from the store's own
// endpoint, and the discriminator is the TOTAL number of fills: one with the
// re-ask, two without.
//
// THE FAKE'S FILL FAILS, which is why the two assertions are independent
// rather than one. There is no `/v2/pods` route, so the first slot's sync
// errors at the create -- and the second slot still skips, because what it
// asks is the STORE and not the other slot. That is the design: a flag saying
// "this process filled it" would be a second source of truth for a question
// the store answers, and it would be wrong here, where the fill failed and the
// weights arrived anyway.
//
// IT CARRIES ITS OWN DEADLINE for the reason above.
func TestAppPlanner_TwoSlotsDoNotBothRentAMachineToFillOneStore(t *testing.T) {
	gate := &rentalGate{}
	inFlight := make(chan struct{}, 4)
	release := make(chan struct{})
	d, fills := gatedStoreFakes(t, gate, inFlight, release)

	p := AppPlanner{
		Config: storeConfig(), Deps: d, AllowPaid: true,
		Runs:  genstore.NewRecordingStore(),
		Fills: &StoreFills{},
	}

	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := aRequest()
			r.ID = genstore.ReqID(fmt.Sprintf("slot-%d", i))
			_, _ = p.Plan(context.Background(), r)
		}(i)
	}

	// One fill is in flight. Held there, so a second could join it.
	select {
	case <-inFlight:
	case <-time.After(20 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("neither plan reached the store fill, so this test says nothing about the lock")
	}
	// Long enough that a second fill with no lock would have arrived: it is
	// one HTTP request away and the first is parked in the handler.
	time.Sleep(250 * time.Millisecond)

	if max, _ := gate.peak(); max != 1 {
		close(release)
		wg.Wait()
		t.Fatalf("%d store fills were in flight at once, want 1: two slots both renting a "+
			"machine with no card to fetch one store's weights is a second $0.06/hour "+
			"machine and two writers into one mount", max)
	}

	close(release)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the two plans did not finish")
	}

	// AND ONLY ONE OF THEM FETCHED. The store's endpoint answers for the asset
	// from the moment the first fill starts, so the second slot's re-ask under
	// the lock finds the weights there -- and a planner that acted on the
	// answer its own plan got minutes earlier would rent a second $0.06/hour
	// machine to fetch fifty-six gigabytes that are already on the mount.
	if n := fills(); n != 1 {
		t.Errorf("%d machines were rented to fill one store, want 1: the waiting slot has to "+
			"ask the store's own endpoint under the lock rather than act on the answer its own "+
			"plan got before it waited", n)
	}
}

// gatedStoreFakes is storeFakes with the store's fill counted, held, and
// FILLING ITSELF. It hands back the deps and a count of fills started.
//
// A SECOND FUNCTION RATHER THAN A FLAG ON storeFakes, because what this one
// does to the catalogue route -- block until a test says otherwise -- would be
// wrong for every other caller of that fake, and a fake whose behaviour
// depends on a flag is a fake whose readers have to know which mode they are
// in.
//
// THE STORE FILLS ITSELF THE MOMENT A FILL STARTS, which is the half that
// makes the re-ask testable: the S3 endpoint answers 404 for the asset until
// then and 200 afterwards, so the second slot asking under the lock sees
// exactly what it would see if another slot's `embernode fetch` had put the
// weights on the mount. A fake that stayed empty for ever could not tell a
// planner that re-asks from one that does not, which is what the first version
// of this did -- measured GREEN against the missing re-ask.
func gatedStoreFakes(
	t *testing.T, gate *rentalGate, inFlight chan struct{}, release chan struct{},
) (app.Deps, func() int) {
	t.Helper()

	cpus, err := os.ReadFile(filepath.Join("..", "provider", "runpod", "testdata", "cpus.json"))
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	fills := 0

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "network-volumes") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"vol_abc","name":"ember-store-models",`+
				`"size":40,"dataCenter":"EU-RO-1","type":"STANDARD"}],`+
				`"pagination":{"hasNextPage":false}}`)
			return
		}
		if req.URL.Path == "/v2/catalog/cpus" {
			// THE FILL IS IN FLIGHT FROM HERE. A CPU catalogue read happens
			// on no other path in this test, so this is the fill and nothing
			// else.
			mu.Lock()
			fills++
			mu.Unlock()
			gate.enter("fill")
			defer gate.leave()
			select {
			case inFlight <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-time.After(20 * time.Second):
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cpus)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)

	// The asset is absent until a fill starts and present afterwards, which
	// is what another slot having fetched it looks like from here.
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		started := fills
		mu.Unlock()
		if started == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", "60306914804")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s3srv.Close)

	// A NAMED VALUE AND NOT A COMPOSITE LITERAL IN THE RETURN. The literal
	// inline is legal and `gofmt` indents it to its own taste;
	// golangci-lint's gofmt disagrees with the toolchain's about that
	// indentation, so the two gates would have contradicted each other over a
	// file neither had an opinion about.
	deps := app.Deps{
		Engine:       dockerengine.NewRecordingEngine(),
		HomeDir:      t.TempDir(),
		CloudBaseURL: api.URL,
		S3BaseURL:    s3srv.URL,
		Creds: &config.Credentials{
			RunPod: node.NewSecret([]byte("rp-token")),
			S3: awssig.Credentials{
				AccessKey: "user_test", Secret: node.NewSecret([]byte("rps_secret")),
			},
		},
		DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
	}
	return deps, func() int {
		mu.Lock()
		defer mu.Unlock()
		return fills
	}
}
