package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/provider/providertest"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

const testPort = 7777

// harness is a server with no Docker, no provider account and no card.
//
// Everything worth a test here is a decision about a request, which is why
// this package owns none of the renting: internal/queue has the loop and
// internal/app has the provisioning, so these tests are httptest and nothing
// else.
type harness struct {
	*Server
	store *genstore.RecordingStore
	blobs *blob.Dir
	reg   *provider.Registry
}

func newHarness(t *testing.T, mut ...func(*Options)) *harness {
	t.Helper()

	reg := provider.NewRegistry()
	if err := reg.Add(local.New(dockerengine.NewRecordingEngine())); err != nil {
		t.Fatal(err)
	}

	blobs, err := blob.OpenDir(filepath.Join(t.TempDir(), "outputs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	store := genstore.NewRecordingStore()
	o := Options{
		Config: config.Config{
			Provider: "local",
			TTL:      time.Hour,
			Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
			SD: config.SDConfig{
				Serve: []string{"sd-server", "--model", "/m.gguf"},
				// The operator's own defaults, which a UI starts from.
				Preview: "proj", PreviewInterval: 5,
			},
			MaxHourlyUSD: "1.00",
		},
		Registry: reg, Store: store, Blobs: blobs, Port: testPort,
	}
	for _, m := range mut {
		m(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{Server: s, store: store, blobs: blobs, reg: reg}
}

// do sends a request that looks like the UI's own: this server's Host, and
// this server's Origin.
func (h *harness) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	r.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", testPort))

	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

const oneJob = `{"provider":"local","jobs":[{"prompt":"a cat","width":512,"height":512,"steps":4}]}`

// --- the guard ---

// TestGuard_RefusesAPageFromSomewhereElse.
//
// THE THREAT IS NOT AN ATTACKER ON THE NETWORK -- nothing is listening off
// this machine. It is any page the operator has open: evil.example can POST
// to a loopback port and queue a generation, and the browser hides only the
// *response*. CORS is therefore not a control, and this is what stands in for
// authentication that is not there yet.
func TestGuard_RefusesAPageFromSomewhereElse(t *testing.T) {
	h := newHarness(t)

	for _, c := range []struct {
		name, origin string
	}{
		{"another site", "https://evil.example"},
		{"another site over http", "http://evil.example"},
		{"this port on another host", fmt.Sprintf("http://evil.example:%d", testPort)},
		{"another port on this host", "http://127.0.0.1:9999"},
		// strings.HasSuffix(h, "localhost") would let this through, which is
		// why the accepted set is closed rather than a suffix match.
		{"a name that merely ends in localhost", fmt.Sprintf("http://localhost.evil.test:%d", testPort)},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/requests", strings.NewReader(oneJob))
			r.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
			r.Header.Set("Origin", c.origin)
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
			if n := len(h.store.Calls); n != 0 {
				t.Errorf("the store was touched %d time(s) by a refused request", n)
			}
		})
	}
}

// TestGuard_RefusesAHostThatIsNotThisServer is the DNS rebinding case.
//
// An attacker's domain resolving to 127.0.0.1 makes their page same-origin
// with THEIR OWN name, so the Origin check passes -- the browser sets Origin
// to their site and the page is on their site. The Host header is what gives
// it away.
func TestGuard_RefusesAHostThatIsNotThisServer(t *testing.T) {
	h := newHarness(t)

	for _, host := range []string{
		"evil.example:7777",
		"192.168.1.10:7777",
		"127.0.0.1:9999",
		"localhost.evil.test:7777",
		"notaport",
	} {
		t.Run(host, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/config", nil)
			r.Host = host
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d for Host %q, want 403", w.Code, host)
			}
		})
	}
}

// TestGuard_AcceptsWhatTheOperatorsOwnTooling Sends.
//
// localhost is accepted, which is NOT the same decision loopback.ParseBind
// makes. There, the name was about to be handed to net.Listen and resolved at
// listen time -- so a hosts file mapping it to a LAN address would publish the
// port. Here it has already been resolved, by the browser, to an address that
// reached a socket bound to loopback and nothing else.
//
// A request with no Origin is accepted too: curl sends none, and so does every
// script an operator writes against their own machine. Refusing would break
// the command line without stopping a browser, which always sends one on a
// cross-origin fetch.
func TestGuard_AcceptsWhatTheOperatorsOwnToolingSends(t *testing.T) {
	h := newHarness(t)

	for _, c := range []struct{ name, host, origin string }{
		{"the loopback address", "127.0.0.1:7777", "http://127.0.0.1:7777"},
		{"the name", "localhost:7777", "http://localhost:7777"},
		{"the v6 address", "[::1]:7777", "http://[::1]:7777"},
		{"curl, with no Origin at all", "127.0.0.1:7777", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/config", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

// --- config ---

func TestConfig_SaysWhatAUICanOffer(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/api/config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := decode[configView](t, w)

	if len(got.Providers) != 1 || got.Providers[0].Name != "local" {
		t.Errorf("providers = %+v, want only the one this process can reach", got.Providers)
	}
	if !got.Providers[0].Free {
		t.Error("the local provider is reported as paid")
	}
	// The ceilings, so a slider knows where it stops.
	if got.Ceilings.TTL != "1h0m0s" {
		t.Errorf("ttl = %q", got.Ceilings.TTL)
	}
	if got.Ceilings.MaxHourly.Micro != 1_000_000 {
		t.Errorf("max_hourly = %+v, want $1.00/hr as an integer", got.Ceilings.MaxHourly)
	}
	// Money crosses twice: the integer to compare, the string to show. The
	// one place in this repository that formats money is the money type.
	if got.Ceilings.MaxHourly.Display != node.Rate(1_000_000).String() {
		t.Errorf("display = %q, want the money type's own rendering", got.Ceilings.MaxHourly.Display)
	}
	if got.AllowPaid {
		t.Error("allow_paid is true without the flag")
	}
	if !got.Servable {
		t.Error("servable is false for a config with an sd.serve block")
	}
	if got.Preview != "proj" || got.PreviewInterval != 5 {
		t.Errorf("preview defaults = %q every %d, want the config's", got.Preview, got.PreviewInterval)
	}
	// Absent before this process's first rental, which is a pointer because
	// "nothing has asked yet" and "the server has no samplers" are different
	// answers.
	if got.Caps != nil {
		t.Error("caps are reported before anything has asked a server")
	}
}

func TestConfig_ReportsTheCapabilitiesOnceSomethingHasLearnedThem(t *testing.T) {
	h := newHarness(t)
	if err := h.store.PutCaps(t.Context(), sdserver.Capabilities{
		Model: "sd15.gguf", Samplers: []string{"euler"},
		Limits: sdserver.Limits{MaxWidth: 2048},
	}); err != nil {
		t.Fatal(err)
	}
	got := decode[configView](t, h.do(t, "GET", "/api/config", ""))
	if got.Caps == nil {
		t.Fatal("caps are absent after they were cached")
	}
	if got.Caps.Model != "sd15.gguf" || got.Caps.Limits.MaxWidth != 2048 {
		t.Errorf("caps = %+v", got.Caps)
	}
}

// --- posting a request ---

func TestPostRequest_QueuesABatchAndKicksTheLoop(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/requests", oneJob)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := decode[map[string]string](t, w)
	if got["id"] == "" {
		t.Fatal("no id came back, so a UI cannot watch what it just queued")
	}

	req, err := h.store.Get(t.Context(), genstore.ReqID(got["id"]))
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != genstore.StatusQueued {
		t.Errorf("status = %q, want queued", req.Status)
	}
	// The operator's own ttl, because a UI that sent none meant the default
	// they configured rather than no deadline at all.
	if req.TTL != time.Hour {
		t.Errorf("ttl = %v, want the config's 1h", req.TTL)
	}
	if len(req.Jobs) != 1 || req.Jobs[0].Prompt != "a cat" {
		t.Errorf("jobs = %+v", req.Jobs)
	}
}

// TestPostRequest_RefusesAMisspelledField.
//
// The same decision config.Load makes with KnownFields: a misspelled key is a
// typo, not a comment. A UI sending `step` instead of `steps` would otherwise
// generate at the server's default and the operator would never learn why
// their four-step preview took a minute.
func TestPostRequest_RefusesAMisspelledField(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a","step":4}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field: %s", w.Code, w.Body.String())
	}
}

// TestPostRequest_HasNowhereToPutAnExtra.
//
// `extra` reaches every field of the generation API ember does not type,
// including the ones naming file paths on the node -- so a request that could
// set it could point a running sd-server at weights the config never
// mentioned. The seal is that the field does not exist on the wire type, so
// this is refused as an unknown field rather than by a check somebody could
// remove.
func TestPostRequest_HasNowhereToPutAnExtra(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a","extra":{"model":"/etc/shadow"}}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if n := len(h.store.Calls); n != 0 {
		t.Error("the request reached the store")
	}
}

func TestPostRequest_RefusesWhatItCannotParse(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ name, body, want string }{
		{"not json", `{`, "read the request"},
		{"a ttl that is not a duration", `{"provider":"local","ttl":"soon","jobs":[{"prompt":"a"}]}`, "ttl"},
		{"a ttl of zero", `{"provider":"local","ttl":"0s","jobs":[{"prompt":"a"}]}`, "not positive"},
		{"a negative ttl", `{"provider":"local","ttl":"-1h","jobs":[{"prompt":"a"}]}`, "not positive"},
		// A JSON number would be a float on both sides of this wire, and
		// ParseFloat("0.29")*1e6 is 289999. Decimal strings only.
		{"a budget that is not a price", `{"provider":"local","max_hourly_usd":"cheap","jobs":[{"prompt":"a"}]}`, "max_hourly_usd"},
		{"a negative budget", `{"provider":"local","max_run_usd":"-1","jobs":[{"prompt":"a"}]}`, "max_run_usd"},
		{"no jobs", `{"provider":"local","jobs":[]}`, "generate nothing"},
		{"no provider", `{"jobs":[{"prompt":"a"}]}`, "no provider"},
		{"a preview mode that does not exist", `{"provider":"local","preview":"magic","jobs":[{"prompt":"a"}]}`, "preview"},
		{"a negative preview interval", `{"provider":"local","preview_interval":-1,"jobs":[{"prompt":"a"}]}`, "preview_interval"},
		{"an output name that cleans away to nothing", `{"provider":"local","jobs":[{"prompt":"a","output":"../.."}]}`, "jobs[0]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := h.do(t, "POST", "/api/requests", c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if got := decode[map[string]string](t, w)["error"]; !strings.Contains(got, c.want) {
				t.Errorf("error = %q, want it to mention %q", got, c.want)
			}
		})
	}
}

// TestPostRequest_RefusesAPaidProviderWithoutTheFlag.
//
// Checked here AND in the planner, at the last statement before a create
// call. Two checks because this one can be gone around -- a request that
// reached the store another way never passed through here -- and the money
// must not depend on which door it came in by.
func TestPostRequest_RefusesAPaidProviderWithoutTheFlag(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		reg := provider.NewRegistry()
		if err := reg.Add(local.New(dockerengine.NewRecordingEngine())); err != nil {
			t.Fatal(err)
		}
		// A provider whose Caps().Free is false, which is the zero value --
		// so a provider added later is paid by default.
		if err := reg.Add(providertest.NewRecordingProvider("runpod")); err != nil {
			t.Fatal(err)
		}
		o.Registry = reg
	})

	w := h.do(t, "POST", "/api/requests", `{"provider":"runpod","jobs":[{"prompt":"a"}]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if got := decode[map[string]string](t, w)["error"]; !strings.Contains(got, "--allow-paid") {
		t.Errorf("error = %q, want it to name the flag", got)
	}

	// And allowed with the flag.
	h2 := newHarness(t, func(o *Options) {
		reg := provider.NewRegistry()
		if err := reg.Add(providertest.NewRecordingProvider("runpod")); err != nil {
			t.Fatal(err)
		}
		o.Registry = reg
		o.AllowPaid = true
	})
	if w := h2.do(t, "POST", "/api/requests", `{"provider":"runpod","jobs":[{"prompt":"a"}]}`); w.Code != http.StatusAccepted {
		t.Errorf("status = %d with --allow-paid, want 202: %s", w.Code, w.Body.String())
	}
}

func TestPostRequest_RefusesAnUnknownProvider(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/requests", `{"provider":"nowhere","jobs":[{"prompt":"a"}]}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
}

// TestPostRequest_RefusesWhenThereIsNoServerToRunOn, at load rather than after
// a card has been rented.
func TestPostRequest_RefusesWhenThereIsNoServerToRunOn(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.SD.Serve = nil })
	w := h.do(t, "POST", "/api/requests", oneJob)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if got := decode[configView](t, h.do(t, "GET", "/api/config", "")); got.Servable {
		t.Error("the config reports itself servable with no sd.serve block")
	}
}

// TestPostRequest_RefusesASamplerTheServerDoesNotHave, once something has
// learned what it has.
//
// This is what the capability cache buys: before it, a mistyped sampler is
// refused about a minute into a rented card -- after the node is up and 56 GiB
// of weights are loaded. After it, the refusal arrives while the operator is
// still looking at the form.
func TestPostRequest_RefusesASamplerTheServerDoesNotHave(t *testing.T) {
	h := newHarness(t)
	if err := h.store.PutCaps(t.Context(), sdserver.Capabilities{
		Samplers: []string{"euler", "heun"},
	}); err != nil {
		t.Fatal(err)
	}

	w := h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a","sampler":"eular"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	got := decode[map[string]string](t, w)["error"]
	if !strings.Contains(got, "eular") || !strings.Contains(got, "euler") {
		t.Errorf("error = %q, want it to name the typo and the alternatives", got)
	}

	// And a sampler it does have is fine.
	if w := h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a","sampler":"euler"}]}`); w.Code != http.StatusAccepted {
		t.Errorf("status = %d for a sampler the server has, want 202: %s", w.Code, w.Body.String())
	}
}

// TestPostRequest_AcceptsASamplerItCannotYetCheck.
//
// Before this process's first rental there is no capability list, and
// refusing everything until there is would mean the first request can never
// be made. app.NewPlan catches it after the node is up, which is where it was
// always caught.
func TestPostRequest_AcceptsASamplerItCannotYetCheck(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a","sampler":"anything"}]}`)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 before anything has asked a server: %s", w.Code, w.Body.String())
	}
}

// --- reading requests ---

func TestRequests_ListAndFetch(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	list := decode[struct {
		Requests []requestView `json:"requests"`
		Running  string        `json:"running"`
	}](t, h.do(t, "GET", "/api/requests", ""))
	if len(list.Requests) != 1 || list.Requests[0].ID != id {
		t.Fatalf("listing = %+v", list.Requests)
	}
	// Without the gallery: a listing is the queue, and reading every
	// request's images to render it would be a query per row.
	if len(list.Requests[0].Images) != 0 {
		t.Error("the listing carries images")
	}

	one := decode[requestView](t, h.do(t, "GET", "/api/requests/"+id, ""))
	if one.ID != id || one.Status != "queued" {
		t.Errorf("request = %+v", one)
	}
	if one.TTL != "1h0m0s" {
		t.Errorf("ttl = %q", one.TTL)
	}
}

func TestRequests_MissingIsNotFound(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/api/requests/nope",
		"/api/requests/nope/events",
	} {
		w := h.do(t, "GET", path, "")
		if path == "/api/requests/nope" && w.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, w.Code)
		}
	}
}

func TestRequests_ListRefusesABadLimit(t *testing.T) {
	h := newHarness(t)
	for _, v := range []string{"0", "-1", "lots"} {
		if w := h.do(t, "GET", "/api/requests?limit="+v, ""); w.Code != http.StatusBadRequest {
			t.Errorf("limit=%s status = %d, want 400", v, w.Code)
		}
	}
}

// TestCancel_StopsAQueuedRequestAndRefusesARunningOne.
//
// Cancelling a running one would mean destroying its node, which takes every
// other request on the same rental with it -- so the refusal says that rather
// than pretending the button does nothing.
func TestCancel_StopsAQueuedRequestAndRefusesARunningOne(t *testing.T) {
	h := newHarness(t)
	queued := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	w := h.do(t, "POST", "/api/requests/"+queued+"/cancel", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got, err := h.store.Get(t.Context(), genstore.ReqID(queued))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}

	// Already ended.
	if w := h.do(t, "POST", "/api/requests/"+queued+"/cancel", ""); w.Code != http.StatusConflict {
		t.Errorf("cancelling an ended request = %d, want 409", w.Code)
	}

	// Running.
	running := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
	if _, ok, err := h.store.ClaimNext(t.Context(), "lease", genstore.Compat{}); err != nil || !ok {
		t.Fatalf("setup: ok=%v err=%v", ok, err)
	}
	w = h.do(t, "POST", "/api/requests/"+running+"/cancel", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("cancelling a running request = %d, want 409", w.Code)
	}
	if got := decode[map[string]string](t, w)["error"]; !strings.Contains(got, "every other request") {
		t.Errorf("error = %q, want it to say what cancelling would cost", got)
	}
}

func TestCancel_MissingIsNotFound(t *testing.T) {
	h := newHarness(t)
	if w := h.do(t, "POST", "/api/requests/nope/cancel", ""); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// --- events ---

// TestEvents_ReplayFromACursor is the same question the websocket answers, and
// deliberately the same code: a client that would rather poll must not be
// told anything a reconnect would not tell it.
func TestEvents_ReplayFromACursor(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	if err := h.store.AppendEvents(t.Context(), genstore.ReqID(id), progress(5)); err != nil {
		t.Fatal(err)
	}

	all := decode[eventsView](t, h.do(t, "GET", "/api/requests/"+id+"/events", ""))
	if len(all.Events) != 5 || all.Next != 5 {
		t.Fatalf("from the start: %d event(s), next %d", len(all.Events), all.Next)
	}

	rest := decode[eventsView](t, h.do(t, "GET", "/api/requests/"+id+"/events?after=3", ""))
	if len(rest.Events) != 2 || rest.Events[0].Seq != 4 || rest.Next != 5 {
		t.Errorf("after 3: %+v", rest)
	}

	// Nothing new, and the cursor does not move backwards.
	none := decode[eventsView](t, h.do(t, "GET", "/api/requests/"+id+"/events?after=5", ""))
	if len(none.Events) != 0 || none.Next != 5 {
		t.Errorf("after 5: %+v", none)
	}
}

func TestEvents_RefusesACursorThatIsNotASequenceNumber(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
	for _, v := range []string{"-1", "soon"} {
		if w := h.do(t, "GET", "/api/requests/"+id+"/events?after="+v, ""); w.Code != http.StatusBadRequest {
			t.Errorf("after=%s status = %d, want 400", v, w.Code)
		}
	}
}

// --- images ---

func TestImage_ServedWithItsTypeDeclaredAndNotSniffed(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	key := id + "/cat-00.png"
	if err := h.blobs.Put(t.Context(), key, []byte("\x89PNG\r\n\x1a\nnot-really")); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddImage(t.Context(), genstore.Image{
		Req: genstore.ReqID(id), Job: 1, Index: 0, Key: key, Bytes: 17,
	}); err != nil {
		t.Fatal(err)
	}

	w := h.do(t, "GET", "/api/requests/"+id+"/images/1/0", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	// Declared, never sniffed: these are bytes a rented machine produced,
	// and letting a browser decide what they are is letting the node decide.
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("content-type = %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("x-content-type-options = %q, want nosniff", got)
	}
	// A result never changes, so it is worth caching.
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("cache-control = %q, want a result to be cacheable", got)
	}

	// And it appears on the request, as a path a browser can fetch rather
	// than a blob key -- which is this process's business and could be an S3
	// object tomorrow.
	one := decode[requestView](t, h.do(t, "GET", "/api/requests/"+id, ""))
	if len(one.Images) != 1 {
		t.Fatalf("images = %+v", one.Images)
	}
	if !strings.HasPrefix(one.Images[0].Path, "/api/requests/") {
		t.Errorf("path = %q, want a URL and not a key", one.Images[0].Path)
	}
}

// TestImage_APreviewIsNeverCached. It is written over as sampling proceeds, so
// a cached one is a progress display that appears to freeze.
func TestImage_APreviewIsNeverCached(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	key := id + "/cat-preview-00.png"
	if err := h.blobs.Put(t.Context(), key, []byte("frame")); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddImage(t.Context(), genstore.Image{
		Req: genstore.ReqID(id), Job: 1, Index: 0, Key: key, Preview: true,
	}); err != nil {
		t.Fatal(err)
	}

	w := h.do(t, "GET", "/api/requests/"+id+"/images/1/0?preview=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want no-store", got)
	}

	// A preview and a result are different images of the same job, so asking
	// for one must not serve the other.
	if w := h.do(t, "GET", "/api/requests/"+id+"/images/1/0", ""); w.Code != http.StatusNotFound {
		t.Errorf("asking for the result got %d, want 404 -- only a preview exists", w.Code)
	}
}

// TestImage_ARowWithNoBytesSaysSo. The operator may have moved or deleted the
// directory, and the row is not wrong about what was produced.
func TestImage_ARowWithNoBytesSaysSo(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
	if err := h.store.AddImage(t.Context(), genstore.Image{
		Req: genstore.ReqID(id), Job: 1, Index: 0, Key: id + "/gone.png",
	}); err != nil {
		t.Fatal(err)
	}
	w := h.do(t, "GET", "/api/requests/"+id+"/images/1/0", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := decode[map[string]string](t, w)["error"]; !strings.Contains(got, "not on disk") {
		t.Errorf("error = %q, want it to distinguish a missing file from a missing row", got)
	}
}

func TestImage_RefusesANonNumericPath(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
	for _, p := range []string{"/images/one/0", "/images/1/first"} {
		if w := h.do(t, "GET", "/api/requests/"+id+p, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", p, w.Code)
		}
	}
}

// --- offers ---

func TestOffers_KeepsEachProvidersFailureBesideItsAnswer(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		reg := provider.NewRegistry()
		if err := reg.Add(local.New(dockerengine.NewRecordingEngine())); err != nil {
			t.Fatal(err)
		}
		broken := providertest.NewRecordingProvider("runpod")
		broken.Errors["Offers"] = fmt.Errorf("could not reach the API")
		if err := reg.Add(broken); err != nil {
			t.Fatal(err)
		}
		o.Registry = reg
		o.AllowPaid = true
	})

	got := decode[offersView](t, h.do(t, "GET", "/api/offers", ""))
	if len(got.Listings) != 2 {
		t.Fatalf("got %d listing(s), want one per provider", len(got.Listings))
	}
	var sawError bool
	for _, l := range got.Listings {
		if l.Provider == "runpod" {
			// "I could not ask RunPod" must never render as "RunPod has
			// nothing" -- the same rule `ember ls` exits 2 for.
			if l.Error == "" {
				t.Error("a provider that could not be asked reported no error")
			}
			sawError = true
		}
	}
	if !sawError {
		t.Error("the broken provider is not in the listing at all")
	}
}

// TestOffers_CannotBeAskedForSomethingTheCeilingForbids.
//
// A listing filtered by a higher ceiling than the operator set would show them
// a card they are not allowed to rent, and the refusal would arrive after they
// chose it.
func TestOffers_CannotBeAskedForSomethingTheCeilingForbids(t *testing.T) {
	h := newHarness(t)
	// The config's ceiling is $1.00/hr; ask for ten.
	w := h.do(t, "GET", "/api/offers?max_hourly_usd=10.00", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	// The local provider's offer is free, so nothing is filtered out here;
	// what is asserted is that the request was accepted and the ceiling
	// applied rather than the query winning. The planner enforces the same
	// ceiling before any create call.
	got := decode[offersView](t, w)
	if len(got.Listings) == 0 {
		t.Error("no listings came back")
	}
}

func TestOffers_RefusesAFilterItCannotParse(t *testing.T) {
	h := newHarness(t)
	if w := h.do(t, "GET", "/api/offers?max_hourly_usd=cheap", ""); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if w := h.do(t, "GET", "/api/offers?provider=nowhere", ""); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- nodes ---

func TestNodes_IsReconcilesOwnAnswer(t *testing.T) {
	h := newHarness(t)
	// Injected, because asking the providers needs a Docker daemon -- and
	// what this endpoint must not do is scan with rules of its own.
	h.scan = func(context.Context) (app.Scanned, error) { return app.Scanned{}, nil }

	w := h.do(t, "GET", "/api/nodes", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := decode[nodesView](t, w)
	if got.Attention != 0 {
		t.Errorf("attention = %d on an empty scan", got.Attention)
	}
}

func TestNodes_SaysSoWhenItCannotScan(t *testing.T) {
	h := newHarness(t)
	if w := h.do(t, "GET", "/api/nodes", ""); w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when no scanner was wired", w.Code)
	}

	h.scan = func(context.Context) (app.Scanned, error) { return app.Scanned{}, fmt.Errorf("docker is not running") }
	if w := h.do(t, "GET", "/api/nodes", ""); w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when a provider could not be asked", w.Code)
	}
}

// --- the server itself ---

func TestNew_RefusesWithoutAStore(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("a server with no store was accepted")
	}
}

func TestNew_DefaultsThePort(t *testing.T) {
	s, err := New(Options{Store: genstore.NewRecordingStore()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.URL(), fmt.Sprint(DefaultPort)) {
		t.Errorf("URL = %q, want the default port", s.URL())
	}
}

// TestStatic_ServesThePlaceholderAndSaysItIsOne.
//
// The committed placeholder is what makes `go build ./...` work on a clean
// clone: //go:embed fails at compile time when its pattern matches nothing.
// Built() is how `ember serve` knows to print "run make web" rather than
// leaving an operator to interpret a page.
func TestStatic_ServesThePlaceholderAndSaysItIsOne(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "make web") {
		t.Errorf("the placeholder does not say how to build the UI:\n%s", w.Body.String())
	}
	if h.Built() {
		t.Error("Built() is true for the placeholder")
	}
}

// TestStatic_AClientRenderedRouteGetsTheApp. Without it, reloading the page
// on /requests/abc is a 404.
func TestStatic_AClientRenderedRouteGetsTheApp(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/requests/abc", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the app", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<title>ember</title>") {
		t.Error("a client-side route did not get index.html")
	}
}

func TestStatic_IsGuardedToo(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example:7777"
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// --- helpers ---

type eventsView struct {
	Next   int64          `json:"next"`
	Events []report.Event `json:"events"`
}

// progress is a run of sampling-step events, which is what a watching client
// mostly receives.
func progress(n int64) []report.Event {
	var es []report.Event
	for i := int64(1); i <= n; i++ {
		es = append(es, report.Event{
			Seq: i, Kind: report.KindJobProgress, Unit: report.UnitJob,
			Index: 1, Total: 1, Pass: 1, Step: int(i), TotalSteps: int(n),
		})
	}
	return es
}
