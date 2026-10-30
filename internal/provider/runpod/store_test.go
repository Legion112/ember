package runpod

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
)

func TestStoreList_SkipsVolumesEmberDoesNotOwn(t *testing.T) {
	p := newTestProvider(t, serveFile(t, "volumes_list.json"))

	got, err := p.StoreList(t.Context())
	if err != nil {
		t.Fatalf("StoreList = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d stores, want 2 (the third volume is somebody else's): %+v", len(got), got)
	}

	// The names come back without the prefix, and "wan-2-2" survives intact --
	// a label parsed by splitting on "-" would have shredded it.
	names := []string{got[0].Name, got[1].Name}
	if names[0] != "models" || names[1] != "wan-2-2" {
		t.Errorf("names = %v, want [models wan-2-2]", names)
	}
	if got[0].Region != "EU-RO-1" {
		t.Errorf("region = %q", got[0].Region)
	}
	if got[0].Handle.ID != "agv6w2qcg7" {
		t.Errorf("handle = %s", got[0].Handle)
	}

	// The rate is read off the volume's own type and size, not assumed:
	// 40 GiB standard at $0.07, 2048 GiB high-performance at $0.14.
	if got[0].Price != standardRate {
		t.Errorf("standard price = %v, want %v", got[0].Price, standardRate)
	}
	if got[1].Price != highPerformanceRate {
		t.Errorf("high-performance price = %v, want %v", got[1].Price, highPerformanceRate)
	}
	if want := node.MicroUSD(2_800_000); got[0].Monthly() != want {
		t.Errorf("monthly = %d, want %d", got[0].Monthly(), want)
	}
}

func TestStoreList_FollowsTheCursor(t *testing.T) {
	// The leak this prevents: a client that read only the first page would
	// under-report stores, and a store it never mentions is one nobody
	// decides to remove.
	pages := 0
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"v1","name":"ember-store-a","size":10,"dataCenter":"EU-RO-1"}],`+
				`"pagination":{"nextCursor":"page2","hasNextPage":true}}`)
		case "page2":
			_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"v2","name":"ember-store-b","size":10,"dataCenter":"EU-RO-1"}],`+
				`"pagination":{"nextCursor":null,"hasNextPage":false}}`)
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	})

	got, err := p.StoreList(t.Context())
	if err != nil {
		t.Fatalf("StoreList = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d stores across %d pages, want 2", len(got), pages)
	}
	if pages != 2 {
		t.Errorf("made %d requests, want 2", pages)
	}
}

func TestStoreList_StopsWhenThereIsNoCursorToFollow(t *testing.T) {
	// hasNextPage true with no cursor is a page ember cannot ask for.
	// Following it would mean re-reading the same page forever.
	calls := 0
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"networkVolumes":[],"pagination":{"nextCursor":null,"hasNextPage":true}}`)
	})
	if _, err := p.StoreList(t.Context()); err != nil {
		t.Fatalf("StoreList = %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d requests, want 1", calls)
	}
}

func TestStoreList_RefusesToLoopForever(t *testing.T) {
	// A server that always says "more" must end in an error that says the
	// listing is incomplete, not in a request loop.
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"networkVolumes":[],"pagination":{"nextCursor":"always","hasNextPage":true}}`)
	})
	_, err := p.StoreList(t.Context())
	if err == nil {
		t.Fatal("an endless pagination was accepted")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("err = %q, want it to say the listing is incomplete", err)
	}
}

func TestStoreCreate_SendsWhatTheLiveAPIWants(t *testing.T) {
	var body map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != VolumePath {
			t.Errorf("path = %s, want %s", r.URL.Path, VolumePath)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":"agv6w2qcg7","name":"ember-store-models","size":40,`+
			`"dataCenter":"EU-RO-1","type":"STANDARD"}`)
	})

	got, err := p.StoreCreate(t.Context(), "models", 40, "EU-RO-1")
	if err != nil {
		t.Fatalf("StoreCreate = %v", err)
	}

	// dataCenter, not dataCenterId. The documented spelling belongs to the
	// other API and this one answers it with a 422.
	if _, wrong := body["dataCenterId"]; wrong {
		t.Error("the create body used dataCenterId, which this API refuses")
	}
	if body["dataCenter"] != "EU-RO-1" {
		t.Errorf("dataCenter = %v", body["dataCenter"])
	}
	if body["name"] != "ember-store-models" {
		t.Errorf("name = %v, want the label rather than the bare name", body["name"])
	}
	if body["type"] != "STANDARD" {
		t.Errorf("type = %v", body["type"])
	}
	if got.Name != "models" || got.Handle.ID != "agv6w2qcg7" {
		t.Errorf("got %+v", got)
	}
}

func TestStoreCreate_RefusesWhatTheServerWould(t *testing.T) {
	// Refused here rather than by the server. A round trip to be told
	// "minimum: got 1, want 10" teaches the same thing more slowly, and the
	// bounds are measured rather than guessed.
	called := false
	p := newTestProvider(t, func(http.ResponseWriter, *http.Request) { called = true })

	for _, tc := range []struct {
		name    string
		store   string
		size    int
		region  string
		wantMsg string
	}{
		{"too small", "models", 1, "EU-RO-1", "outside RunPod's range"},
		{"below the measured minimum", "models", node.MinStoreGiB - 1, "EU-RO-1", "outside RunPod's range"},
		{"too large", "models", node.MaxStoreGiB + 1, "EU-RO-1", "outside RunPod's range"},
		{"no name", "", 40, "EU-RO-1", "empty"},
		{"a name that would not round-trip", "ember-store-x", 40, "EU-RO-1", "prefix ember uses"},
		{
			// There is no sensible default datacentre: the choice decides
			// where every future run of this store must be placed.
			name: "no region", store: "models", size: 40, region: "", wantMsg: "needs a datacentre",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.StoreCreate(t.Context(), tc.store, tc.size, tc.region)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
	if called {
		t.Error("a refusal still made an API call")
	}
}

func TestStoreCreate_RefusesAVolumeItCannotRecogniseAfterwards(t *testing.T) {
	// The label went out and did not come back. A store ember cannot
	// recognise is one `ember ls` will not report and `ember store rm` will
	// not remove: a leak created by the command meant to be the careful one.
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":"v1","name":"something-else","size":40,"dataCenter":"EU-RO-1"}`)
	})
	_, err := p.StoreCreate(t.Context(), "models", 40, "EU-RO-1")
	if err == nil {
		t.Fatal("a volume ember cannot recognise was accepted")
	}
	if !strings.Contains(err.Error(), "console") {
		t.Errorf("err = %q, want it to tell the operator where to clean up", err)
	}
}

func TestStoreDestroy(t *testing.T) {
	var path, method string
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	if err := p.StoreDestroy(t.Context(), node.StoreHandle{Provider: Name, ID: "agv6w2qcg7"}); err != nil {
		t.Fatalf("StoreDestroy = %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %s", method)
	}
	if want := VolumePath + "/agv6w2qcg7"; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
}

func TestStoreDestroy_NotFoundIsSuccess(t *testing.T) {
	// The caller asked for the store to be gone and it is. Same rule as a
	// node, and the reason idempotent teardown works at all.
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := p.StoreDestroy(t.Context(), node.StoreHandle{Provider: Name, ID: "gone"}); err != nil {
		t.Errorf("StoreDestroy on a missing volume = %v, want success", err)
	}
}

func TestStoreDestroy_RefusesAnEmptyHandle(t *testing.T) {
	called := false
	p := newTestProvider(t, func(http.ResponseWriter, *http.Request) { called = true })
	if err := p.StoreDestroy(t.Context(), node.StoreHandle{Provider: Name}); err == nil {
		t.Error("an empty handle was accepted")
	}
	if called {
		t.Error("an empty handle still made a DELETE")
	}
}

func TestStoreList_ReportsAFailure(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := p.StoreList(t.Context()); err == nil {
		t.Error("a 401 listing reported success")
	}
}

func TestStoreCreate_ReportsAFailure(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprint(w, `{"errors":["$.size: minimum: got 1, want 10"]}`)
	})
	_, err := p.StoreCreate(t.Context(), "models", 40, "EU-RO-1")
	if err == nil {
		t.Fatal("a 422 reported success")
	}
	if !strings.Contains(err.Error(), "create store models") {
		t.Errorf("err = %q, want it to name the store", err)
	}
}

func TestCreate_AttachesTheStoreAndPinsTheDatacentre(t *testing.T) {
	var body map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/pods") && r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"pod1","name":"n","status":"PROVISIONING"}`)
	})
	// A configured datacentre list is overridden, not intersected: an
	// intersection could be empty while the store's own site has capacity.
	p.DataCenters = []string{"US-KS-2"}

	req := request(t)
	spec := req.Spec
	spec.Store = node.StoreHandle{Provider: Name, ID: "agv6w2qcg7"}
	spec.Mount = mustMount(t, "/models")
	req.Spec = spec
	// The node goes where the chosen offer is, which for a store run is the
	// store's own site.
	req.Offer.Region = "EU-RO-1"

	if _, err := p.Create(t.Context(), req); err != nil {
		t.Fatalf("Create = %v", err)
	}

	// mounts.network, not a top-level networkVolumeId: that spelling belongs
	// to the other API.
	if _, wrong := body["networkVolumeId"]; wrong {
		t.Error("the create body used networkVolumeId, which this API does not take")
	}
	m, ok := body["mounts"].(map[string]any)
	if !ok {
		t.Fatalf("mounts = %v, want an object", body["mounts"])
	}
	net, ok := m["network"].([]any)
	if !ok || len(net) != 1 {
		t.Fatalf("mounts.network = %v, want one entry", m["network"])
	}
	entry := net[0].(map[string]any)
	if entry["volumeId"] != "agv6w2qcg7" {
		t.Errorf("volumeId = %v", entry["volumeId"])
	}
	// The path has no default on this API, so it must be sent, and it must
	// not be /workspace -- where ember writes outputs.
	if entry["path"] != "/models" {
		t.Errorf("path = %v, want /models", entry["path"])
	}

	dcs, ok := body["dataCenterIds"].([]any)
	if !ok || len(dcs) != 1 || dcs[0] != "EU-RO-1" {
		t.Errorf("dataCenterIds = %v, want exactly the store's site", body["dataCenterIds"])
	}

	// persistent is never sent: the API refuses a pod asking for both, since
	// it drops a persistent disk whenever a volume is attached.
	if _, wrong := m["persistent"]; wrong {
		t.Error("the create body asked for a persistent mount alongside a network one")
	}
}

func TestCreate_WithoutAStoreSendsNoMountsAtAll(t *testing.T) {
	// A pod with no store must produce exactly the body it produced before
	// stores existed -- not an empty mounts object for the API to interpret.
	var raw map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/pods") && r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&raw)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"pod1","name":"n","status":"PROVISIONING"}`)
	})
	if _, err := p.Create(t.Context(), request(t)); err != nil {
		t.Fatalf("Create = %v", err)
	}
	if _, present := raw["mounts"]; present {
		t.Errorf("mounts = %v on a pod with no store", raw["mounts"])
	}
}

func TestOffers_RefusesWhenTheStoreSiteHasNothing(t *testing.T) {
	// "no offers matched" reads as "that GPU is sold out everywhere" and
	// sends the operator to try a different card. The real answer is that
	// their weights are in the wrong site, and the fix is a datacentre.
	p := newTestProvider(t, serveFile(t, "gpus.json"))

	_, err := p.Offers(t.Context(), node.Filter{Region: "XX-NOWHERE-9"})
	if err == nil {
		t.Fatal("an empty region reported no error")
	}
	if !errors.Is(err, node.ErrNoCapacity) {
		t.Errorf("err = %v, want it to wrap ErrNoCapacity", err)
	}
	if !strings.Contains(err.Error(), "XX-NOWHERE-9") {
		t.Errorf("err = %q, want it to name the datacentre", err)
	}
	if !strings.Contains(err.Error(), "store") {
		t.Errorf("err = %q, want it to say the store is what pins placement", err)
	}
}

func TestOffers_WithoutAStoreIsUnchanged(t *testing.T) {
	p := newTestProvider(t, serveFile(t, "gpus.json"))
	withNoStore, err := p.Offers(t.Context(), node.Filter{})
	if err != nil {
		t.Fatalf("Offers = %v", err)
	}
	if len(withNoStore) == 0 {
		t.Fatal("the catalogue produced no offers at all")
	}
}

func mustMount(t *testing.T, p string) node.Mount {
	t.Helper()
	m, err := node.MountAt(p)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStoreList_ReportsAVolumeItCannotMap(t *testing.T) {
	// A volume wearing ember's label but missing its id is a listing ember
	// cannot act on. Reported rather than skipped: skipping would hide a
	// store from `ember ls`, which is the one thing this must not do.
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"","name":"ember-store-models","size":10}],`+
			`"pagination":{"hasNextPage":false}}`)
	})
	if _, err := p.StoreList(t.Context()); err == nil {
		t.Error("a volume with no id was listed as if it were fine")
	}
}

func TestStoreCreate_ReportsAVolumeItCannotMap(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":"","name":"ember-store-models","size":40,"dataCenter":"EU-RO-1"}`)
	})
	if _, err := p.StoreCreate(t.Context(), "models", 40, "EU-RO-1"); err == nil {
		t.Error("a created volume with no id was accepted")
	}
}

func TestStoreDestroy_ReportsAFailure(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := p.StoreDestroy(t.Context(), node.StoreHandle{Provider: Name, ID: "v1"})
	if err == nil {
		t.Fatal("a 500 destroy reported success")
	}
	if !strings.Contains(err.Error(), "runpod/v1") {
		t.Errorf("err = %q, want it to name the store that is still there", err)
	}
}
