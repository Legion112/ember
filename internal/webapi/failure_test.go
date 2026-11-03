package webapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/reconcile"
	"github.com/Legion112/ember/internal/statestore"
)

// TestHandlers_ReportAStoreThatBroke.
//
// Every read here can fail, and what matters is that the operator is told
// which one did. "Something went wrong" over an error that names the exact
// table is the failure mode this server's fail() exists to avoid -- it is
// reachable only from their own machine, so there is nobody to withhold the
// message from.
func TestHandlers_ReportAStoreThatBroke(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	for _, c := range []struct {
		failOn, method, path string
		want                 int
	}{
		{"List", "GET", "/api/requests", http.StatusInternalServerError},
		{"Get", "GET", "/api/requests/" + id, http.StatusInternalServerError},
		{"Images", "GET", "/api/requests/" + id, http.StatusInternalServerError},
		{"Events", "GET", "/api/requests/" + id + "/events", http.StatusInternalServerError},
		{"Images", "GET", "/api/requests/" + id + "/images/1/0", http.StatusInternalServerError},
		{"Enqueue", "POST", "/api/requests", http.StatusBadRequest},
	} {
		t.Run(c.failOn+" "+c.path, func(t *testing.T) {
			h.store.FailOn = c.failOn
			defer func() { h.store.FailOn = "" }()

			body := ""
			if c.method == "POST" {
				body = oneJob
			}
			w := h.do(t, c.method, c.path, body)
			if w.Code != c.want {
				t.Errorf("status = %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
			if got := decode[map[string]string](t, w)["error"]; got == "" {
				t.Error("the response carries no reason")
			}
		})
	}
}

// TestCancel_ReportsAStoreItCannotWriteTo. The request is still queued, so
// reporting success would leave an operator believing they had stopped
// something that is about to rent a card.
func TestCancel_ReportsAStoreItCannotWriteTo(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	h.store.FailOn = "Update"
	defer func() { h.store.FailOn = "" }()

	w := h.do(t, "POST", "/api/requests/"+id+"/cancel", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
}

// TestConfig_SurvivesACapabilityCacheItCannotRead.
//
// A UI without the capability list offers free-text controls instead of
// menus, which is worse than ideal and much better than a blank page.
func TestConfig_SurvivesACapabilityCacheItCannotRead(t *testing.T) {
	h := newHarness(t)
	h.store.FailOn = "Caps"
	defer func() { h.store.FailOn = "" }()

	w := h.do(t, "GET", "/api/config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := decode[configView](t, w); got.Caps != nil {
		t.Error("caps are reported although the cache could not be read")
	}
}

// TestPostRequest_QueuesAnywayWhenItCannotCheckTheSampler.
//
// The alternative is refusing every request because a cache read failed,
// which turns a nuisance into an outage. app.NewPlan still checks it after
// the node is up, which is where it was always checked.
func TestPostRequest_QueuesAnywayWhenItCannotCheckTheSampler(t *testing.T) {
	h := newHarness(t)
	h.store.FailOn = "Caps"

	w := h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a","sampler":"whatever"}]}`)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
}

// aSweep is one `app.Scanned` with something of every shape in it.
//
// Built once and shared by the projection tests below, because the thing being
// asserted is that ALL THREE of Scanned's slices arrive: a sweep that only
// carried nodes is what the projection was written against, and it is why a
// leftover was dropped for the life of the type.
func aSweep() app.Scanned {
	return app.Scanned{
		Nodes: []reconcile.Finding{
			{
				Kind:     reconcile.KindExpired,
				Provider: "local",
				Node:     node.Node{Handle: node.Handle{Provider: "local", ID: "n1"}},
				Reason:   "past its deadline",
				Accrued:  &node.Billing{Rate: node.Rate(340000), Cost: node.MicroUSD(11333)},
			},
			{
				Kind:     reconcile.KindOrphan,
				Provider: "local",
				Record:   &statestore.Record{RunID: "abc123"},
				Reason:   "recorded with no node id",
			},
			{
				// The finding with no object behind it at all, which is why
				// Provider cannot be trimmed from the wire type.
				Kind:     reconcile.KindUnreachable,
				Provider: "runpod",
				Reason:   "could not be asked what it is running: dial tcp: lookup rest.runpod.io: no such host",
			},
		},
		Leftovers: []reconcile.Finding{
			{
				Kind:     reconcile.KindLeftover,
				Provider: "local",
				Leftover: &node.Leftover{
					Handle: node.LeftoverHandle{Provider: "local", ID: "9f3c"},
					Kind:   "network",
					Name:   "ember-abc123",
					RunID:  "abc123",
				},
				Reason: "network left behind by run abc123, whose node is gone",
			},
		},
		Stores: []reconcile.Finding{
			{
				Kind:     reconcile.KindStore,
				Provider: "runpod",
				Store: &node.Store{
					Handle:  node.StoreHandle{Provider: "runpod", ID: "vol1"},
					Name:    "flux2",
					SizeGiB: 100,
				},
				Reason:  "a store, billing by the month",
				Monthly: node.MicroUSD(7_000_000),
			},
		},
	}
}

// TestNodes_ProjectsReconcilesOwnFindings.
//
// internal/reconcile's answer, projected -- never a second scan with its own
// rules. Two scanners is two answers to "what am I paying for", and the one
// that disagrees is the one that misses a node.
func TestNodes_ProjectsReconcilesOwnFindings(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) { return aSweep(), nil }

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if len(got.Findings) != 3 {
		t.Fatalf("got %d finding(s), want 3", len(got.Findings))
	}

	first := got.Findings[0]
	if first.ID != "local/n1" {
		t.Errorf("id = %q, want the node's handle -- which is what `ember ls` prints", first.ID)
	}
	if first.Kind != "expired" || first.Reason == "" {
		t.Errorf("kind = %q reason = %q, want reconcile's own words", first.Kind, first.Reason)
	}
	if first.Accrued == nil || first.Accrued.Micro != 11333 {
		t.Errorf("accrued = %+v, want what the node has cost so far", first.Accrued)
	}
	// Money already spent, and nothing standing. A $0/month beside an expired
	// node would read as a charge ember had priced at nothing.
	if first.Monthly != nil {
		t.Errorf("monthly = %+v on an expired node, want absent", first.Monthly)
	}

	// A record with no node id still names something, or `ember reap` and the
	// UI would be talking about different findings.
	if got.Findings[1].ID != "local/abc123" {
		t.Errorf("id = %q, want the run id from the record", got.Findings[1].ID)
	}

	// THE UNREACHABLE PROVIDER IS NAMED. It has no node, no record, no store
	// and no leftover, so its ID is empty by construction and Provider is the
	// only thing that says who could not be asked -- "I could not check
	// RunPod" must never render anonymously, which is a listing an operator
	// cannot act on.
	third := got.Findings[2]
	if third.Kind != "unreachable" || third.Provider != "runpod" {
		t.Errorf("third finding = %+v, want the unreachable provider named", third)
	}
}

// TestNodes_ReportsWhatADestroyedNodeLeftBehind is the third of app.Scanned's
// slices, which this projection dropped for the life of the type.
//
// `ember ls` prints a leftover with the nodes and counts it towards exit 2;
// nodesView carried Findings and Stores alone, so a bridge network whose node
// is gone was absent from the UI and absent from the attention count. Nothing
// bills for one, which is why it stayed invisible here as it did in the lab
// harness -- what it costs is every local run failing at once, weeks later,
// with "all predefined address pools have been fully subnetted", an error
// naming Docker instead of ember.
func TestNodes_ReportsWhatADestroyedNodeLeftBehind(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) { return aSweep(), nil }

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if len(got.Leftovers) != 1 {
		t.Fatalf("got %d leftover(s), want 1", len(got.Leftovers))
	}
	// The name and not the handle, which is reconcile.Finding.String's own
	// choice: the handle's id is the daemon's hash, and `ember-abc123` is what
	// an operator matches against `docker network ls`.
	if got.Leftovers[0].ID != "local/ember-abc123" {
		t.Errorf("id = %q, want what `ember ls` prints for a leftover", got.Leftovers[0].ID)
	}

	// THE COUNT IS OVER ALL THREE SLICES, which is what runLs sums. Two
	// node findings need attention, the unreachable provider needs attention,
	// the leftover needs attention, and the store deliberately does not -- so
	// four, and a projection that forgot the leftover would answer three.
	if got.Attention != 4 {
		t.Errorf("attention = %d, want 4: two node findings, an unreachable "+
			"provider and a leftover, and a store that is not one", got.Attention)
	}
}

// TestNodes_PricesTheChargeNothingEverEnds.
//
// A store bills by the gibibyte-month whether or not anything is attached, and
// it is the first resource here that can leak while every node is correctly
// destroyed. An operator in front of the browser had no path to that number at
// all: `ember ls` prints it and this route answered 501.
func TestNodes_PricesTheChargeNothingEverEnds(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) { return aSweep(), nil }

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if got.MonthlyStorage.Micro != 7_000_000 {
		t.Errorf("monthly_storage = %+v, want the total reconcile prices", got.MonthlyStorage)
	}
	if got.MonthlyStorage.Display == "" {
		t.Error("monthly_storage has no rendering: money crosses this wire twice " +
			"so the browser is never a second formatter of it")
	}
	if len(got.Stores) != 1 || got.Stores[0].Monthly == nil {
		t.Fatalf("stores = %+v, want one priced store", got.Stores)
	}
	if got.Stores[0].Monthly.Micro != 7_000_000 {
		t.Errorf("the store's own monthly = %+v", got.Stores[0].Monthly)
	}
	// A store existing is the expected state and is deliberately not
	// attention-worthy: making every store exit non-zero would teach the
	// operator to ignore the count.
	if got.Stores[0].Kind != "store" {
		t.Errorf("kind = %q, want store", got.Stores[0].Kind)
	}
}

// TestNodes_SendsEveryListAsAnEmptyListAndNeverNull.
//
// Go marshals a nil slice as `null`, serde's `default` covers a field that is
// MISSING rather than one that is present and null, and this repository has
// already lost a whole websocket envelope -- and with it a request's state --
// to exactly that. Three slices here, so three chances.
//
// ASSERTED ON THE BYTES and not on the decoded value, because `[]` and `null`
// both decode into a nil Go slice: a test that unmarshalled this could not
// tell them apart, which is how the first one shipped.
func TestNodes_SendsEveryListAsAnEmptyListAndNeverNull(t *testing.T) {
	h := newHarness(t)
	h.scan = func(context.Context) (app.Scanned, error) { return app.Scanned{}, nil }

	body := h.do(t, "GET", "/api/nodes", "").Body.String()
	if strings.Contains(body, "null") {
		t.Errorf("an empty sweep carries a null on the wire:\n%s", body)
	}
	for _, field := range []string{`"findings":[]`, `"leftovers":[]`, `"stores":[]`} {
		if !strings.Contains(body, field) {
			t.Errorf("want %s in:\n%s", field, body)
		}
	}
}

// TestRequest_CarriesItsTimestampsOnceItHasThem.
//
// A zero time stays absent rather than becoming the epoch: "this never
// started" and "this started in 1970" are different claims, and a UI showing
// the second is a UI nobody believes.
func TestRequest_CarriesItsTimestampsOnceItHasThem(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	before := decode[requestView](t, h.do(t, "GET", "/api/requests/"+id, ""))
	if before.Started != "" || before.Ended != "" {
		t.Errorf("a queued request claims to have started or ended: %+v", before)
	}

	started := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ended := started.Add(90 * time.Second)
	if err := h.store.Update(t.Context(), genstore.ReqID(id), func(r *genstore.Request) {
		r.Status = genstore.StatusDone
		r.Started, r.Ended = started, ended
		r.Offer, r.Node = "offer-1", "local/n1"
		r.Cost = node.MicroUSD(11333)
	}); err != nil {
		t.Fatal(err)
	}

	after := decode[requestView](t, h.do(t, "GET", "/api/requests/"+id, ""))
	if after.Started != "2026-11-01T12:00:00Z" || after.Ended != "2026-11-01T12:01:30Z" {
		t.Errorf("timestamps = %q / %q", after.Started, after.Ended)
	}
	if after.Node != "local/n1" || after.Offer != "offer-1" {
		t.Errorf("the rental is not named: %+v", after)
	}
	// Money twice: the integer to compare, the string to show.
	if after.Cost.Micro != 11333 || after.Cost.Display != node.MicroUSD(11333).String() {
		t.Errorf("cost = %+v", after.Cost)
	}
}

// TestRequest_APreviewPathIsDistinctFromTheResults.
//
// They are different images of the same job, so one URL could not serve both
// -- and a gallery opening on a latent is the bug that would follow. A `proj`
// preview of a 64x64 generation is an 8x8 PNG of about 250 bytes.
func TestRequest_APreviewPathIsDistinctFromTheResults(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	for _, im := range []genstore.Image{
		{Req: genstore.ReqID(id), Job: 1, Index: 0, Key: id + "/a-00.png"},
		{Req: genstore.ReqID(id), Job: 1, Index: 0, Key: id + "/a-preview-00.png", Preview: true},
	} {
		if err := h.store.AddImage(t.Context(), im); err != nil {
			t.Fatal(err)
		}
	}

	got := decode[requestView](t, h.do(t, "GET", "/api/requests/"+id, ""))
	if len(got.Images) != 2 {
		t.Fatalf("got %d image(s), want 2", len(got.Images))
	}
	var result, preview string
	for _, im := range got.Images {
		if im.Preview {
			preview = im.Path
		} else {
			result = im.Path
		}
	}
	if result == "" || preview == "" {
		t.Fatalf("one of the two is missing: %+v", got.Images)
	}
	if result == preview {
		t.Errorf("both images share the path %q", result)
	}
	if !strings.Contains(preview, "preview=1") {
		t.Errorf("the preview path %q does not say so", preview)
	}
}

// TestImage_SaysSoWithNoBlobStore. A server built without one cannot serve a
// file, and pretending it is missing would send an operator looking for a
// deleted directory.
func TestImage_SaysSoWithNoBlobStore(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Blobs = nil })
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
	if err := h.store.AddImage(t.Context(), genstore.Image{
		Req: genstore.ReqID(id), Job: 1, Index: 0, Key: id + "/a-00.png",
	}); err != nil {
		t.Fatal(err)
	}
	if w := h.do(t, "GET", "/api/requests/"+id+"/images/1/0", ""); w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

// TestNew_AServerWithNoRegistryStillAnswers. A process that could reach no
// provider is a process with an empty provider list, not a crash -- and the
// UI showing none is the honest answer.
func TestNew_AServerWithNoRegistryStillAnswers(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Registry = nil })
	got := decode[configView](t, h.do(t, "GET", "/api/config", ""))
	if len(got.Providers) != 0 {
		t.Errorf("providers = %+v, want none", got.Providers)
	}
}
