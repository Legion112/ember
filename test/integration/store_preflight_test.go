//go:build integration

// The store pre-flight, end to end, with nothing standing in for the join.
//
// WHAT THIS EXISTS FOR. `ember serve` asks a store whether this run's weights
// are on it before it rents a card, and fills the store on a machine with no
// card when they are not. The measurement that bought the feature: an RTX PRO
// 6000 at $2.49/hr spent 14m47s of a 15.1-minute rental pulling 56.16 GiB from
// HuggingFace with the GPU at 0% -- 97.9% of the bill, $0.61 of $0.63, and no
// images. The same download on the CPU machine `ember store sync` rents is
// $0.06/hr.
//
// AND WHY NO LAB COULD SEE IT. Every store lab in this tree runs on the local
// provider, because `harness.Lab` writes `provider: local` -- and
// `local.go`'s store reports no region, correctly, since a Docker volume is
// not in a datacentre. `CheckStoreAssets` skips on an empty region by design,
// so on every lab here it returned `Asked: false` and did nothing: the suite
// could not tell the pre-flight from its own absence. Its unit tests go red on
// reinstatement; the labs would not have noticed it being deleted.
//
// WHAT MAKES IT A JOIN RATHER THAN TWO FAKES AGREEING. `internal/queue` and
// `internal/cli` already stand a store up with an httptest handler answering a
// size out of a map, which is enough to test a decision and cannot test this:
// the sync FILLS the store by running `embernode fetch` on a rented machine,
// over SSH, into a mounted directory, and the next plan asks the store's own
// endpoint whether that worked. Two maps agree whatever either half does --
// the closed loop `capabilities.model` sat in. Here `runpodfake` backs each
// volume with one host directory, bind-mounts it into the container the way
// `mounts.network` attaches it on RunPod, and serves S3 object calls by
// reading that same directory. The asset the node downloads is the asset the
// endpoint lists, or this lab fails.
//
// WHY IT DRIVES THE QUEUE IN PROCESS RATHER THAN THE BINARY. Every other lab
// here runs the compiled `ember` as a subprocess, and this one cannot:
// `app.Deps.CloudBaseURL` and `S3BaseURL` are Go fields, so a subprocess
// cannot be pointed at a fake provider -- which `AGENTS.md` already records as
// the reason the offers panel's error path has no lab. What is built below is
// `cmd_serve.go`'s own wiring, field for field: the same `queue.Queue`, the
// same `queue.AppPlanner`, the same `genstore`.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/queue"
	"github.com/Legion112/ember/internal/s3"
	"github.com/Legion112/ember/test/integration/harness"
	"github.com/Legion112/ember/test/integration/runpodfake"
)

// storeAsset is what the lab puts on the store: a sentence, because the
// question is whether the bytes travel and not how many of them there are.
const (
	storeAsset     = "these are the weights for a lab"
	storeAssetName = "sd15.gguf"
	storeMount     = "/models"
	storeName      = "models"
)

// drainReport is what `Queue.Run` logs when a rental ends badly.
//
// Matched as a string because it is the only thing ember does with that
// failure: `once` returns it to `Run`, which logs it and carries on, and
// nothing writes it to the request's row. A lab waiting for a terminal status
// would wait for a write this code does not make.
const drainReport = "a rental ended badly"

// storeRegion is the datacentre the lab's store lives in.
//
// EU-RO-1 because it is the site the one live store run actually used, so the
// signing region in this lab is a region RunPod serves an S3 endpoint for
// rather than a string nobody has seen. The captured CPU catalogue has
// capacity there; the captured GPU catalogue does not, which is what
// gpuCatalogueIn is for.
const storeRegion = runpodfake.VolumeDataCenter

// TestStore_ServeFetchesOntoACheapMachineBeforeItRentsTheCard is the whole
// sequence.
//
// `ember store create`, then a queued request against an EMPTY store, and the
// order that comes out: a machine with no card first, which downloads the
// weights onto the mount, and the card afterwards, which finds them already
// there. Five assertions cross the gap between the two rentals and no two of
// them read the same half -- the create bodies say which machine was rented,
// the file on this disk says the node wrote it, the S3 endpoint says the
// store's own listing shows it, and the GPU node's own digest check says the
// bytes are the ones the config named.
func TestStore_ServeFetchesOntoACheapMachineBeforeItRentsTheCard(t *testing.T) {
	lab := newStoreLab(t)

	// The store is created and EMPTY. Nothing seeds it: that is the premise.
	if keys := lab.objectKeys(t); len(keys) != 0 {
		t.Fatalf("the store already holds %v, so this lab would prove nothing", keys)
	}

	lab.queueOneAndWait(t)

	// 1. TWO RENTALS, THE CHEAP ONE FIRST. Read off the create bodies rather
	// than the fake's pod state, so what is asserted is the JSON that went to
	// RunPod -- `gpu` and `cpu` are mutually exclusive there and exactly one
	// must be set, so a sync that quietly landed on a card is visible here and
	// nowhere else.
	bodies := createBodies(t, lab.f)
	if len(bodies) != 2 {
		t.Fatalf("got %d create call(s), want exactly 2: a machine with no card to fetch the weights, then the card", len(bodies))
	}
	if _, found := bodies[0]["gpu"]; found {
		t.Errorf("the first rental asked for a card: gpu=%v. That is the $0.61 this exists to stop", bodies[0]["gpu"])
	}
	if _, found := bodies[0]["cpu"]; !found {
		t.Errorf("the first rental is not a CPU machine: %v", bodies[0])
	}
	if _, found := bodies[1]["cpu"]; found {
		t.Errorf("the second rental has no card: cpu=%v, so the generation would have nothing to run on", bodies[1]["cpu"])
	}
	if _, found := bodies[1]["gpu"]; !found {
		t.Errorf("the second rental asked for no card: %v", bodies[1])
	}

	// 2. BOTH MOUNTED THE STORE, at the path the config named and not at
	// RunPod's own /workspace -- where ember writes outputs and a store would
	// bill for every render for as long as it exists.
	for i, b := range bodies {
		id, path := mountOf(t, b)
		if id != lab.storeID {
			t.Errorf("create %d mounted volume %q, want the store %q", i+1, id, lab.storeID)
		}
		if path != storeMount {
			t.Errorf("create %d mounted it at %q, want %q", i+1, path, storeMount)
		}
	}

	// 3. THE NODE WROTE THE BYTES INTO THE MOUNTED DIRECTORY. Stat and not a
	// read: `embernode fetch` runs as root in the container and chmods the
	// asset 0600, so the test process can see that it is there and how big it
	// is and cannot read it. What proves the CONTENT is assertion 5, which is
	// ember's own digest check on a different machine -- a stronger claim than
	// a byte comparison here, because it is the check a real run depends on.
	fi, err := os.Stat(filepath.Join(lab.storeDir, storeAssetName))
	if err != nil {
		t.Fatalf("the cheap machine did not write %s onto the mount: %v", storeAssetName, err)
	}
	if fi.Size() != int64(len(storeAsset)) {
		t.Errorf("%s on the store is %d bytes, want %d", storeAssetName, fi.Size(), len(storeAsset))
	}

	// 4. AND THE STORE'S OWN ENDPOINT SHOWS IT. This is the half the pre-flight
	// reads and the half `ember store ls` and `ember store verify` read: a
	// HEAD per asset, an object listing, and the incomplete-upload listing that
	// finds parts billing for an upload that died with its process.
	cl := lab.s3Client()
	o, found, err := cl.Head(t.Context(), lab.storeID, storeAssetName)
	if err != nil {
		t.Fatalf("HEAD %s on the store: %v", storeAssetName, err)
	}
	if !found {
		t.Fatalf("the store's endpoint does not list %s, which the node has just written to it", storeAssetName)
	}
	if o.Size != int64(len(storeAsset)) {
		t.Errorf("the endpoint reports %s as %d bytes, want %d", storeAssetName, o.Size, len(storeAsset))
	}
	objects, err := cl.ListObjects(t.Context(), lab.storeID, "")
	if err != nil {
		t.Fatalf("list what is on the store: %v", err)
	}
	// Positively, because a misspelled XML element decodes to an empty
	// listing: "no error came back" would pass against a listing this fake
	// could not produce at all.
	if len(objects) != 1 || objects[0].Key != storeAssetName {
		t.Errorf("the store lists %+v, want exactly %s", objects, storeAssetName)
	}
	ups, err := cl.ListIncompleteUploads(t.Context(), lab.storeID)
	if err != nil {
		t.Fatalf("list incomplete uploads: %v", err)
	}
	if len(ups) != 0 {
		t.Errorf("the store reports %d incomplete upload(s) after a run that made none: %+v", len(ups), ups)
	}

	// 5. THE ORDER, IN EMBER'S OWN WORDS, AND THE DIGEST CHECK THAT CROSSES
	// THE GAP. The cheap machine says it fetched; the card says the file is
	// already there AND re-hashes it, which is what `verify: always` is for.
	// A card that printed "fetching" would be the defect.
	out := lab.out.String()
	fetched := "fetched " + storeAssetName
	verified := storeAssetName + " is present and verified"
	iF, iV := strings.Index(out, fetched), strings.Index(out, verified)
	if iF < 0 {
		t.Errorf("nothing fetched the weights:\n%s", out)
	}
	if iV < 0 {
		t.Errorf("the card never verified the weights in place, so it did not read what the cheap machine wrote:\n%s", out)
	}
	if iF >= 0 && iV >= 0 && iF > iV {
		t.Errorf("the weights were verified before they were fetched, which is the wrong rental doing the download:\n%s", out)
	}

	// The asset URL was reached exactly once: by the machine with no card.
	if n := lab.assets.hits(); n != 1 {
		t.Errorf("the asset URL was fetched %d time(s), want 1 -- a second fetch is the card paying to download", n)
	}

	// 6. IT FAILED AT THE SERVER AND NOT AT THE WEIGHTS, which is what makes
	// everything above evidence of the ordering rather than of a run that
	// stopped early. `ember:lab` carries no sd-server -- that is `ember:sd`,
	// and baking a model into this lab would make a store test an sd test --
	// so the rental comes up, verifies the store and then cannot start a
	// generation server. The refusal is asserted positively for the reason
	// `AGENTS.md` gives about the pre-flight's own unit tests: a failure
	// naming the weights would mean the check ran at the wrong moment.
	if !strings.Contains(out, "sd-server") {
		t.Errorf("the run did not end at the generation server, so the order above may not be what it looks like:\n%s", out)
	}
	if strings.Contains(out, "does not match its digest") {
		t.Errorf("the run ended over the weights rather than over the missing server:\n%s", out)
	}

	// 7. THE ROW STILL READS `running`, AND THAT IS A GAP RATHER THAN A
	// PROPERTY. `Queue.once` calls `fail` when `Planner.Plan` fails and
	// records nothing at all when the RENTAL fails: the status is written
	// inside `WithServed`'s closure, by `drain`, so every failure after the
	// plan -- the create, the wait for sshd, the gpu probe, the asset verify,
	// the model load -- leaves the row with the `running` that `ClaimNext`
	// wrote and no rental behind it. The web UI's queue table shows it running
	// for the life of the process and its cancel button reaches the
	// provisioning-window 409, whose message says the rental is coming up when
	// nothing is; `Run`'s `Interrupt` sweep only clears it at the NEXT start.
	//
	// Asserted rather than left to be noticed, the way
	// `TestConformance_CannotCatchARederivedRunID` is: a limitation nobody can
	// read out of the code is a limitation that will be forgotten. A change
	// that records a failed rental turns this red, and that is the moment to
	// read this comment.
	req := lab.request(t)
	if req.Status != genstore.StatusRunning {
		t.Errorf("the request ended %q; a rental that failed after the plan writes no status at all, "+
			"so this read `running` when the gap was measured", req.Status)
	}
	if req.Err != "" {
		t.Errorf("the request carries the reason %q; nothing writes one on this path", req.Err)
	}

	// 8. THE LEDGER SHOWS ONE RENTAL AND TWO WERE PAID FOR, which is a gap
	// rather than an assertion about what is right. `fillStore` calls
	// `SyncAssets` with no sink and writes no row, so `ember history` and
	// `ember spend` are blind to a rental the queue started on its own
	// initiative -- small money at $0.06/hr, and the rule it sits against is
	// this repository's own: "a total that is missing part of its history says
	// so, every time". Asserted rather than left to be noticed, the way
	// `TestConformance_CannotCatchARederivedRunID` is: a limitation nobody can
	// read out of the code is a limitation that will be forgotten. A change
	// that records the cheap rental turns this red, and that is the moment to
	// read this comment.
	ledger, err := lab.store.History(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(ledger) != 1 {
		t.Errorf("the ledger holds %d row(s); two machines were rented and only the request's own is recorded", len(ledger))
	}

	// 9. NO POD SURVIVED AND THE STORE DID. A store outliving every node that
	// mounts it is the whole reason it is worth its standing charge, and a pod
	// outliving the run is the thing this repository is written against.
	if live := lab.f.Live(); len(live) != 0 {
		t.Errorf("%d pod(s) survived the run: %v", len(live), live)
	}
	if vols := lab.f.Volumes(); len(vols) != 1 {
		t.Errorf("the store is %v after a run that should have left it alone", vols)
	}
}

// TestStore_AFullStoreRentsTheCardAndNothingElse is the negative control.
//
// Without it every assertion above is equally consistent with a planner that
// rents a CPU machine before every request there is -- a second rental, every
// time, for nothing. The identical config and the identical code path, with
// the one asset already on the store, must produce exactly ONE create, with a
// card in it, and must not touch the asset URL at all.
//
// The store is seeded by the test process rather than by a run, which is also
// what makes the digest check meaningful: `verify: always` re-hashes what it
// finds, so a seed with the wrong bytes would fail the rental instead.
func TestStore_AFullStoreRentsTheCardAndNothingElse(t *testing.T) {
	lab := newStoreLab(t)

	// Written here and not through ember: a lab seeds state out-of-band, and
	// 0o600 by this process rather than by a container so the file is readable
	// from both sides.
	seeded := filepath.Join(lab.storeDir, storeAssetName)
	if err := os.WriteFile(seeded, []byte(storeAsset), 0o600); err != nil {
		t.Fatal(err)
	}

	lab.queueOneAndWait(t)

	bodies := createBodies(t, lab.f)
	if len(bodies) != 1 {
		t.Fatalf("a full store produced %d create call(s), want exactly 1: the card and nothing else", len(bodies))
	}
	if _, found := bodies[0]["gpu"]; !found {
		t.Errorf("the one rental asked for no card: %v", bodies[0])
	}
	if _, found := bodies[0]["cpu"]; found {
		t.Errorf("a full store still rented a machine with no card: %v", bodies[0]["cpu"])
	}

	out := lab.out.String()
	if strings.Contains(out, "renting a machine with no card") {
		t.Errorf("the queue said it was filling a store that was already full:\n%s", out)
	}
	if strings.Contains(out, "fetched "+storeAssetName) {
		t.Errorf("the weights were downloaded although they were already on the store:\n%s", out)
	}
	if !strings.Contains(out, storeAssetName+" is present and verified") {
		t.Errorf("the card did not verify the seeded weights in place, so it may not have read the store at all:\n%s", out)
	}
	if n := lab.assets.hits(); n != 0 {
		t.Errorf("the asset URL was fetched %d time(s) for a store that already had the file", n)
	}

	// The same positive refusal as above: it got all the way to the server,
	// so none of the assertions here passed because the run stopped early.
	if !strings.Contains(out, "sd-server") {
		t.Errorf("the one rental did not reach the generation server:\n%s", out)
	}
}

// --- the lab ---------------------------------------------------------------

// storeLab is `cmd_serve.go`'s wiring, pointed at the fakes.
type storeLab struct {
	l      *harness.Lab
	f      *runpodfake.Fake
	assets *assetServer
	store  *genstore.SQLite
	q      *queue.Queue
	out    *lockedBuffer

	cfg      config.Config
	deps     app.Deps
	storeID  string
	storeDir string

	// id is the one request this lab queues.
	id genstore.ReqID
}

// newStoreLab creates the store through the provider and builds the queue.
//
// `ember store create` through `provider.StoreAPI` rather than through the
// binary, for the reason this file's header gives: the subprocess cannot be
// pointed at the fake. It is the same method `runStoreCreate` calls, and the
// nothing-creates-a-store-as-a-side-effect rule is what makes it a separate
// step here as well -- a standing monthly charge exists because somebody asked
// for it.
func newStoreLab(t *testing.T) *storeLab {
	t.Helper()

	l := harness.New(t)
	ctx := t.Context()

	// The captured GPU catalogue has capacity in US-IL-1 and CA-MTL-3 and the
	// captured CPU one in EU-RO-1, so no site in the two fixtures has both --
	// and a store pins every node that mounts it to one datacentre. The GPU
	// body is therefore the capture with its sites rewritten, which keeps the
	// field names captured (the whole reason this fake serves the provider's
	// own fixtures) and changes only the string this lab cannot have both
	// halves of. The CPU catalogue is served verbatim.
	f := runpodfake.New(t, dockerNodes{t: t, l: l}, runpodfake.Options{
		ReadyAfter: time.Second,
		SSHAfter:   time.Second,
		Catalogue:  gpuCatalogueIn(t, storeRegion),
	})

	assets := newAssetServer(t, l.BridgeGateway(ctx), storeAsset)

	// The operator's key, inside the lab's own config directory, so nothing
	// here reads or writes the real ~/.config/ember.
	_ = identity(t, l)

	cfg := config.Config{
		Provider: "runpod",
		Image:    harness.LabImage,
		TTL:      10 * time.Minute,
		Store:    config.StoreConfig{Name: storeName, SizeGiB: 40, Region: storeRegion, Mount: storeMount},
		Assets: []config.AssetConfig{{
			URL:    assets.URL,
			SHA256: harness.DigestOf(storeAsset),
			As:     storeAssetName,
		}},
		// always, which is the default and the one that re-hashes what it
		// finds: the store is writable by every node that mounts it.
		Verify: "always",
		SD:     config.SDConfig{Serve: []string{"sd-server", "--model", storeMount + "/" + storeAssetName}},
	}

	deps := app.Deps{
		HomeDir:      l.Dir,
		CloudBaseURL: f.URL,
		S3BaseURL:    f.S3URL,
		Creds: &config.Credentials{
			RunPod: node.NewSecret([]byte("fake-token")),
			S3:     runpodfake.S3Credentials(),
		},
		// Injected, or this reads the developer's own card and the lab behaves
		// differently on a workstation. The second result is "the probe
		// answered", which is not the same as "there is a card".
		DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
	}

	creds, err := deps.Credentials(filepath.Join(l.Dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := app.BuildRegistry(deps, creds, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Get("runpod")
	if err != nil {
		t.Fatal(err)
	}
	sa, ok := p.(provider.StoreAPI)
	if !ok {
		t.Fatalf("the runpod provider does not implement StoreAPI, so `ember store create` has nothing to call")
	}
	st, err := sa.StoreCreate(ctx, storeName, 40, storeRegion)
	if err != nil {
		t.Fatalf("ember store create: %v", err)
	}
	dir, found := f.StoreDir(string(st.Handle.ID))
	if !found {
		t.Fatalf("the fake created store %s and backs it with no directory", st.Handle.ID)
	}

	gs, err := genstore.Open(l.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gs.Close() })

	out := &lockedBuffer{}
	return &storeLab{
		l: l, f: f, assets: assets, store: gs, out: out,
		cfg: cfg, deps: deps,
		storeID:  string(st.Handle.ID),
		storeDir: dir,
		q: &queue.Queue{
			Store: gs,
			Planner: queue.AppPlanner{
				Config: cfg, Deps: deps, AllowPaid: true, Out: out, Ledger: gs,
			},
			Outputs: l.OutputDir(),
			Log:     testLogger(out),
		},
	}
}

// queueOneAndWait enqueues one request, drains until it is terminal, and stops
// the loop.
//
// IT WAITS FOR EITHER OF TWO THINGS, and the second is the one that happens
// here. A request whose `Planner.Plan` fails is recorded terminal by
// `Queue.fail`; a request whose RENTAL fails is recorded as nothing at all --
// `Queue.once` returns the error from `WithServed` to `Run`, which logs it,
// and the row keeps the `running` that `ClaimNext` wrote. So waiting on the
// status alone is waiting for a write this code does not make. See the gap
// asserted in the lab above; `drainReport` is the line `Run` logs instead.
//
// That is `orExited`'s rule one layer out: race the thing you are waiting for
// against the thing that means it will never happen. The first version waited
// on the status only and spent its whole budget, which is how the gap was
// found.
//
// Bounded, and the bound is the lab's own rather than the package timeout: a
// drain loop that never finished a request would otherwise be a wedge, and a
// wedge prints no `--- FAIL` -- it spends the whole binary's budget and ends
// in a goroutine dump that names nothing. Two container starts and a destroy
// is about a minute here.
func (lab *storeLab) queueOneAndWait(t *testing.T) {
	t.Helper()
	ctx := t.Context()

	id, err := lab.store.Enqueue(ctx, genstore.Request{
		Provider: "runpod",
		TTL:      10 * time.Minute,
		Jobs:     []genstore.Job{{Prompt: "a paper boat", Width: 64, Height: 64, Steps: 2}},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	lab.id = id

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := lab.q.Run(runCtx); err != nil {
			t.Logf("the drain loop returned %v", err)
		}
	}()
	lab.q.Kick()

	deadline := time.Now().Add(8 * time.Minute)
	for {
		r, err := lab.store.Get(ctx, id)
		if err != nil {
			stop()
			<-done
			t.Fatalf("read request %s: %v", id, err)
		}
		if r.Status.Terminal() || strings.Contains(lab.out.String(), drainReport) {
			break
		}
		if time.Now().After(deadline) {
			stop()
			<-done
			t.Fatalf("request %s was still %q after 8 minutes and the drain loop reported nothing:\n%s",
				id, r.Status, lab.out.String())
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Stopped and WAITED FOR, because `queue.Run` does not return until
	// `provision.WithNode`'s deferred destroy has: waiting for this goroutine
	// is waiting for the node to be gone, which is what makes the pod and
	// stray assertions below true rather than racy.
	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("the drain loop did not return after the context was cancelled:\n%s", lab.out.String())
	}
}

// request reads the one request back.
func (lab *storeLab) request(t *testing.T) genstore.Request {
	t.Helper()
	r, err := lab.store.Get(t.Context(), lab.id)
	if err != nil {
		t.Fatalf("read request %s: %v", lab.id, err)
	}
	return r
}

// objectKeys is what is on the store, read from this machine's own filesystem.
func (lab *storeLab) objectKeys(t *testing.T) []string {
	t.Helper()
	es, err := os.ReadDir(lab.storeDir)
	if err != nil {
		t.Fatalf("read the directory backing the store: %v", err)
	}
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// s3Client is the client `ember store ls` and `ember store verify` use,
// pointed at the fake's endpoint the way `app.Deps.S3BaseURL` points it.
func (lab *storeLab) s3Client() *s3.Client {
	return &s3.Client{
		Endpoint: lab.f.S3URL,
		Region:   storeRegion,
		Creds:    runpodfake.S3Credentials(),
	}
}

// --- helpers ---------------------------------------------------------------

// createBodies is every create call ember made, in order.
//
// The plural of createBody, which fails unless there is exactly one: the whole
// subject here is how many rentals happened and in which order.
func createBodies(t *testing.T, f *runpodfake.Fake) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range f.Requests() {
		if r.Method == http.MethodPost && r.Path == "/v2/pods" {
			out = append(out, r.Body)
		}
	}
	return out
}

// mountOf reads the one network volume a create body attaches.
func mountOf(t *testing.T, body map[string]any) (id, path string) {
	t.Helper()
	m, ok := body["mounts"].(map[string]any)
	if !ok {
		t.Fatalf("the create body attaches no storage: %v", body)
	}
	entries, ok := m["network"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("mounts.network is %v, want exactly one entry", m["network"])
	}
	e, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("mounts.network[0] is not an object: %v", entries[0])
	}
	s, _ := e["volumeId"].(string)
	p, _ := e["path"].(string)
	return s, p
}

// gpuCatalogueIn is the captured GPU catalogue with every site rewritten to
// one datacentre.
//
// A TRANSFORMATION OF THE CAPTURE AND NOT A SECOND BODY. `runpodfake` serves
// the provider's own fixtures verbatim precisely so that the field shapes the
// fake hands ember are the bytes the mapping tests are written against -- a
// second copy of a field shape is how `dataCenter` against `dataCenterId`
// stayed invisible. So this changes the one value a store makes impossible to
// satisfy from two fixtures at once (no site in them has both a card and a CPU
// machine) and touches nothing else: the names, the prices, the per-site
// availability levels and the RTX 4090's `NONE` summary beside a `LOW` site
// all survive, which is the row this repository records as rentable while its
// summary says otherwise.
func gpuCatalogueIn(t *testing.T, region string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "provider", "runpod", "testdata", "gpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the captured gpu catalogue: %v", err)
	}
	gpus, ok := doc["gpus"].([]any)
	if !ok {
		t.Fatalf("the captured gpu catalogue has no gpus array: %v", doc)
	}
	moved := 0
	for _, g := range gpus {
		entry, ok := g.(map[string]any)
		if !ok {
			continue
		}
		sites, ok := entry["dataCenters"].([]any)
		if !ok {
			continue
		}
		for _, s := range sites {
			site, ok := s.(map[string]any)
			if !ok {
				continue
			}
			site["id"], site["name"] = region, region
			moved++
		}
	}
	// A rewrite that moved nothing would leave the lab asserting "no capacity"
	// against a catalogue it believed it had changed.
	if moved == 0 {
		t.Fatalf("the captured gpu catalogue names no datacentre, so nothing was moved to %s", region)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assetServer serves one asset at an address a node container can reach.
//
// BOUND TO THE DOCKER BRIDGE AND NOT TO LOOPBACK, which is the one place in
// these labs where that is necessary: `embernode fetch` runs INSIDE a
// container, and 127.0.0.1 there is the container. The address is the bridge's
// own gateway, read from the daemon -- so the listener is reachable from
// containers on that bridge and from this machine, and from nothing on the
// LAN. `httptest` binds loopback, which is why this is a listener of its own;
// `internal/loopback`'s own guard exempts test files for exactly this, and
// nothing in ember's binaries is involved.
type assetServer struct {
	URL string

	mu   sync.Mutex
	seen int
}

func newAssetServer(t *testing.T, gateway, body string) *assetServer {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		t.Fatalf("listen on the docker bridge gateway %s: %v\n"+
			"a node container has to be able to reach the asset URL, and loopback there is the container", gateway, err)
	}

	a := &assetServer{URL: "http://" + ln.Addr().String() + "/" + storeAssetName}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			a.mu.Lock()
			a.seen++
			a.mu.Unlock()
			_, _ = w.Write([]byte(body))
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return a
}

func (a *assetServer) hits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen
}

// lockedBuffer is the byte stream the drain goroutine writes and the
// assertions read.
//
// A mutex because those are two goroutines: the queue writes from inside the
// rental and the test reads after it has stopped, and "after" is not a
// guarantee the compiler makes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
