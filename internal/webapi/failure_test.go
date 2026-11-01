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

// TestNodes_ProjectsReconcilesOwnFindings.
//
// internal/reconcile's answer, projected -- never a second scan with its own
// rules. Two scanners is two answers to "what am I paying for", and the one
// that disagrees is the one that misses a node.
func TestNodes_ProjectsReconcilesOwnFindings(t *testing.T) {
	h := newHarness(t)

	h.scan = func(context.Context) (app.Scanned, error) {
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
			},
		}, nil
	}

	got := decode[nodesView](t, h.do(t, "GET", "/api/nodes", ""))
	if len(got.Findings) != 2 {
		t.Fatalf("got %d finding(s), want 2", len(got.Findings))
	}

	first := got.Findings[0]
	if first.ID != "local/n1" {
		t.Errorf("id = %q, want the node's handle -- which is what `ember ls` prints", first.ID)
	}
	// The rendered line as well as the parts, so an operator reading the UI
	// and `ember ls` sees the same sentence in each.
	if first.Line == "" || !strings.Contains(first.Line, "past its deadline") {
		t.Errorf("line = %q, want reconcile's own rendering", first.Line)
	}
	if first.Accrued == nil || first.Accrued.Micro != 11333 {
		t.Errorf("accrued = %+v, want what the node has cost so far", first.Accrued)
	}

	// A record with no node id still names something, or `ember reap` and the
	// UI would be talking about different findings.
	if got.Findings[1].ID != "local/abc123" {
		t.Errorf("id = %q, want the run id from the record", got.Findings[1].ID)
	}

	// The number `ember ls` exits 2 for.
	if got.Attention == 0 {
		t.Error("attention = 0 for an expired node and an orphan")
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
