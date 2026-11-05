package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

// testToken is the credential every request in this package carries.
//
// Minted rather than a literal, and minted ONCE for the test binary rather
// than per harness, so the helpers below can authorise a request without
// threading a value through every call site. Fresh each run, because a
// hard-coded token is one somebody would reach for in production.
var testToken = NewToken()

// authorize makes a request look like it came from something that holds the
// token.
//
// THE HEADER AND NOT THE COOKIE, because that is the carrier a non-browser
// client uses and these tests are not a browser. The cookie half is what the
// chromedp lab proves, since whether Chrome attaches a SameSite=Strict cookie
// to a websocket handshake is a question for Chrome.
func authorize(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+string(testToken.Bytes()))
	return r
}

// harness is a server with no Docker, no provider account and no card.
//
// Everything worth a test here is a decision about a request, which is why
// this package owns none of the renting: internal/queue has the loop and
// internal/app has the provisioning, so these tests are httptest and nothing
// else.
type harness struct {
	*Server
	store  *genstore.RecordingStore
	blobs  *blob.Dir
	inputs *blob.Inputs
	reg    *provider.Registry
}

// RecordingDrain is the drain loop as a handler test can see it.
//
// The house fake shape -- Calls, plus the one answer that has to be scripted.
// Reaching the real *queue.Queue would mean a handler test with a rental in
// it, which is the thing this package owns none of.
//
// `reached` is false by default ON PURPOSE, which is the same decision
// Caps.PublicSSH makes: the uninformative answer is the safe one. A fake that
// said "yes, I stopped it" without being told would make every cancel test
// pass against a handler that never called Cancel at all.
//
// `holding` IS ITS OWN FIELD AND NOT DERIVED FROM `running`, because the
// state this package's two 409s turn on is exactly the one where they differ:
// a rental claimed and provisioning is held and not generating. A fake that
// computed one from the other could not produce it.
type RecordingDrain struct {
	Calls   []string
	running genstore.ReqID
	holding genstore.ReqID
	reached bool
}

func (d *RecordingDrain) Kick() { d.Calls = append(d.Calls, "Kick") }

func (d *RecordingDrain) Running() (genstore.ReqID, bool) {
	d.Calls = append(d.Calls, "Running")
	return d.running, d.running != ""
}

func (d *RecordingDrain) Cancel(id genstore.ReqID) bool {
	d.Calls = append(d.Calls, "Cancel "+string(id))
	return d.reached && id == d.running
}

func (d *RecordingDrain) Holding(id genstore.ReqID) bool {
	d.Calls = append(d.Calls, "Holding "+string(id))
	return d.holding != "" && id == d.holding
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

	// A REAL STAGING STORE AND NOT A FAKE, pointed at a temporary directory.
	// blob.Inputs is a directory and a digest check; there is no external
	// system behind it to stand in for, and an interface introduced to make
	// a test easier is an interface the production code did not need.
	inputs, err := blob.OpenInputs(filepath.Join(t.TempDir(), "inputs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inputs.Close() })

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
		Registry: reg, Store: store, Blobs: blobs, Inputs: inputs,
		Port: testPort, Token: testToken,
	}
	for _, m := range mut {
		m(&o)
	}
	// DERIVED FROM THE CONFIG THE MUTATORS LEFT, which is what cmd_serve
	// does: one reading of the file, handed to the server and to the queue.
	// After the mutators so that a mutator which edits the config -- adding a
	// tts.voices list, say -- changes what this server declares, rather than
	// a test having to set two fields that could disagree.
	if o.Workloads == nil {
		ws, werr := o.Config.WorkloadCatalogue()
		if werr != nil {
			t.Fatal(werr)
		}
		o.Workloads = ws
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{Server: s, store: store, blobs: blobs, inputs: o.Inputs, reg: reg}
}

// upload posts raw bytes to the one route that takes them.
//
// NO Content-Type THAT MEANS ANYTHING, deliberately: the server does not
// consult it, because a client's declared type is a claim about the bytes and
// image.DecodeConfig is a check of them.
func (h *harness) upload(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/inputs", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	r.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", testPort))
	authorize(r)

	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	return w
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
	authorize(r)

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
			r := authorize(httptest.NewRequest("POST", "/api/requests", strings.NewReader(oneJob)))
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
			// AUTHORISED, so the 403 below is the Host check's and not the
			// credential's. A refusal test that could pass for either
			// reason proves neither.
			r := authorize(httptest.NewRequest("GET", "/api/config", nil))
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
			r := authorize(httptest.NewRequest("GET", "/api/config", nil))
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
	// ONE SERVABLE WORKLOAD, FROM THIS CONFIG AND NOT FROM genstore.Kinds().
	// The harness declares no `workloads:`, so the entry is the implicit one
	// derived from `image:` and `sd:` -- named after its own driver, which is
	// what makes a row written before the column resolve to it.
	if len(got.Workloads) != 1 {
		t.Fatalf("workloads = %+v, want the one this config describes", got.Workloads)
	}
	wl := got.Workloads[0]
	if wl.ID != config.DriverSD || wl.Driver != config.DriverSD {
		t.Errorf("workload = %+v, want the implicit sd entry", wl)
	}
	if wl.Preview != "proj" || wl.PreviewInterval != 5 {
		t.Errorf("preview defaults = %q every %d, want the config's", wl.Preview, wl.PreviewInterval)
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

// The feature flags reach the page, and a silence stays a silence.
//
// WHY IT MATTERS ON THIS SIDE. The page greys a control the server said no to,
// and the only thing that tells it is this object. A flag flattened to false
// on the way out would grey the control for every server that published
// nothing -- including every `caps` row cached before ember read these maps --
// and the operator would find the advanced section half unusable with no
// explanation anywhere. `omitempty` on a *bool omits nil and keeps a published
// false, which is the whole mechanism.
func TestConfig_ReportsTheFeatureFlagsAndLeavesTheSilencesOut(t *testing.T) {
	h := newHarness(t)
	no, yes := false, true
	if err := h.store.PutCaps(t.Context(), sdserver.Capabilities{
		Model:     "sd15.gguf",
		Supported: sdserver.Supported{ClipSkip: &no, SLG: &yes},
	}); err != nil {
		t.Fatal(err)
	}
	w := h.do(t, "GET", "/api/config", "")
	got := decode[configView](t, w)
	if got.Caps == nil {
		t.Fatal("caps are absent after they were cached")
	}
	if got.Caps.Features.ClipSkip == nil || *got.Caps.Features.ClipSkip {
		t.Errorf("clip_skip = %v, want a published false", got.Caps.Features.ClipSkip)
	}
	if got.Caps.Features.SLG == nil || !*got.Caps.Features.SLG {
		t.Errorf("slg = %v, want a published true", got.Caps.Features.SLG)
	}
	if got.Caps.Features.Hires != nil {
		t.Errorf("hires = %v, want it absent: the server said nothing about it",
			*got.Caps.Features.Hires)
	}
	// Against the bytes, because the decoded struct cannot tell an absent
	// field from a false one after the fact -- and absent is what the page
	// reads as "not gated".
	if body := w.Body.String(); strings.Contains(body, `"hires"`) {
		t.Errorf("an unpublished flag is on the wire: %s", body)
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
		// ParseFloat("2.01")*1e6 truncates to 2009999. Decimal strings only.
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
	// AND THE PAGE IS NEVER OFFERED IT, which is the half that replaced a
	// `servable` flag: an entry that cannot be served is left out of what
	// `/api/config` publishes, so there is no tab for a workload whose next
	// click would be this 409. One method answers both -- see
	// config.Workload.Unservable.
	if got := decode[configView](t, h.do(t, "GET", "/api/config", "")); len(got.Workloads) != 0 {
		t.Errorf("workloads = %+v, want none offered with no sd.serve block", got.Workloads)
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

// TestCancel_StopsAQueuedRequest is the half that always worked.
func TestCancel_StopsAQueuedRequest(t *testing.T) {
	drain := &RecordingDrain{}
	h := newHarness(t, func(o *Options) { o.Queue = drain })
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

	// AND THE LOOP WAS SIGNALLED ANYWAY, although the row said queued.
	// ClaimNext is one statement and it can run between the Get and the
	// Update here, so a request read as queued may already be generating --
	// in which case the row this handler wrote is about to be overwritten by
	// the queue's own finish and the batch would carry on.
	if !containsCall(drain.Calls, "Cancel "+queued) {
		t.Errorf("calls = %v, want the loop signalled too: a request read as queued "+
			"may have been claimed between the read and the write", drain.Calls)
	}

	// Already ended.
	if w := h.do(t, "POST", "/api/requests/"+queued+"/cancel", ""); w.Code != http.StatusConflict {
		t.Errorf("cancelling an ended request = %d, want 409", w.Code)
	}
}

// TestCancel_StopsARunningRequestWithoutEndingTheRental is the correction.
//
// IT USED TO BE A 409, whose stated reason was that cancelling a generating
// request "would mean destroying that node, which would take every other
// request on the same rental with it". True of cancelling a rental, false of
// cancelling a job: sd-server's own /jobs/{id}/cancel ends one generation and
// leaves the process, the node and the resident weights alone. The refusal
// was protecting against a mechanism nobody had connected.
func TestCancel_StopsARunningRequestWithoutEndingTheRental(t *testing.T) {
	drain := &RecordingDrain{reached: true}
	h := newHarness(t, func(o *Options) { o.Queue = drain })

	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
	if _, ok, err := h.store.ClaimNext(t.Context(), "lease", genstore.Compat{}); err != nil || !ok {
		t.Fatalf("setup: ok=%v err=%v", ok, err)
	}
	drain.running = genstore.ReqID(id)

	w := h.do(t, "POST", "/api/requests/"+id+"/cancel", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cancelling a running request = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !containsCall(drain.Calls, "Cancel "+id) {
		t.Errorf("calls = %v, want the drain loop asked to stop it", drain.Calls)
	}

	// THE ROW IS NOT WRITTEN HERE. The request is still generating when this
	// returns: the queue is polling sd-server for the job's own answer, and
	// what it ended as -- how far it got, whether the job in flight could be
	// stopped at all -- is the queue's to record. A second writer racing the
	// one that knows would sometimes overwrite the truth with a guess.
	got, err := h.store.Get(t.Context(), genstore.ReqID(id))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusRunning {
		t.Errorf("status = %q, want it left as running: the queue records how a cancelled "+
			"generation ended, and two writers for one row means the loser overwrites it",
			got.Status)
	}

	// The reply says a stop is on its way rather than claiming it happened.
	body := decode[map[string]any](t, w)
	if body["cancelling"] != true {
		t.Errorf("body = %v, want cancelling true", body)
	}
	if body["status"] != string(genstore.StatusRunning) {
		t.Errorf("status = %v, want the status the row actually holds", body["status"])
	}
}

// "I could not stop it" and "it is stopped" are different answers.
//
// A row that says running with no rental generating it is what a restart
// leaves behind before queue.Run's Interrupt sweep reaches it. Marking it
// cancelled would report a card as stopped that may still be billing -- the
// same refusal as `ember ls` exiting non-zero on a provider it could
// not ask.
func TestCancel_WillNotClaimToHaveStoppedWhatItCouldNotReach(t *testing.T) {
	cases := map[string]Drain{
		// Running in the row, not running in this process.
		"a stale row": &RecordingDrain{reached: false},
		// `ember serve` before the loop exists, and every other handler test.
		"no loop at all": nil,
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(o *Options) { o.Queue = q })
			id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
			if _, ok, err := h.store.ClaimNext(t.Context(), "lease", genstore.Compat{}); err != nil || !ok {
				t.Fatalf("setup: ok=%v err=%v", ok, err)
			}

			w := h.do(t, "POST", "/api/requests/"+id+"/cancel", "")
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409", w.Code)
			}
			if got := decode[map[string]string](t, w)["error"]; !strings.Contains(got, "no rental in this process") {
				t.Errorf("error = %q, want it to say it could not reach the generation", got)
			}
			got, err := h.store.Get(t.Context(), genstore.ReqID(id))
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != genstore.StatusRunning {
				t.Errorf("status = %q; a cancel that could not be delivered must not record one",
					got.Status)
			}
		})
	}
}

// A cancel during provisioning is refused and must NOT name a restart.
//
// THE REMEDIATION WAS WORSE THAN THE PROBLEM. One message covered both ways
// of reaching this 409, and this is the ordinary one: the row reads `running`
// from ClaimNext and the queue registers its stop channel in `generate`, so
// clicking cancel while a node is coming up lands in a window where a rental
// IS in this process and is about to generate. Telling that operator to
// restart `ember serve` destroys the rental they are paying for in order to
// reconcile a request that was seconds from running.
//
// THE ASSERTION THAT THE TWO ANSWERS DIFFER IS WHAT STOPS THEM BEING MERGED
// BACK TOGETHER, and it is here rather than only in the lab because the lab
// can reach one of the two states and not the other: a row left running by a
// process that is gone cannot be produced without writing it behind the
// queue's back, and queue.Run's Interrupt sweep clears it at startup.
func TestCancel_SaysTheRentalIsComingUpRatherThanAskingForARestart(t *testing.T) {
	queued := func(t *testing.T, drain *RecordingDrain) (*harness, string) {
		t.Helper()
		h := newHarness(t, func(o *Options) { o.Queue = drain })
		id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
		if _, ok, err := h.store.ClaimNext(t.Context(), "lease", genstore.Compat{}); err != nil || !ok {
			t.Fatalf("setup: ok=%v err=%v", ok, err)
		}
		drain.holding = genstore.ReqID(id)
		return h, id
	}

	// Claimed, held, not yet generating: `Running` is empty and `Cancel`
	// has nothing to signal.
	drain := &RecordingDrain{}
	h, id := queued(t, drain)
	w := h.do(t, "POST", "/api/requests/"+id+"/cancel", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: nothing was stopped", w.Code)
	}
	provisioning := decode[map[string]string](t, w)["error"]
	if !strings.Contains(provisioning, "coming up") || !strings.Contains(provisioning, "ask again") {
		t.Errorf("error = %q, want it to say the rental is coming up and to ask again",
			provisioning)
	}
	if !strings.Contains(provisioning, "do not restart ember serve") {
		t.Errorf("error = %q; the operator's instinct here is the old message's advice, and "+
			"a restart during provisioning destroys the rental the request is waiting for",
			provisioning)
	}
	if got, err := h.store.Get(t.Context(), genstore.ReqID(id)); err != nil {
		t.Fatal(err)
	} else if got.Status != genstore.StatusRunning {
		t.Errorf("status = %q; a cancel that could not be delivered must not record one",
			got.Status)
	}

	// The state the restart IS true of: nothing in this process holds it.
	stale := &RecordingDrain{}
	h2 := newHarness(t, func(o *Options) { o.Queue = stale })
	id2 := decode[map[string]string](t, h2.do(t, "POST", "/api/requests", oneJob))["id"]
	if _, ok, err := h2.store.ClaimNext(t.Context(), "lease", genstore.Compat{}); err != nil || !ok {
		t.Fatalf("setup: ok=%v err=%v", ok, err)
	}
	w2 := h2.do(t, "POST", "/api/requests/"+id2+"/cancel", "")
	if w2.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w2.Code)
	}
	orphan := decode[map[string]string](t, w2)["error"]
	if !strings.Contains(orphan, "restart ember serve") {
		t.Errorf("error = %q, want the restart advice kept for the state it is true of", orphan)
	}

	// AND THEY ARE DIFFERENT SENTENCES. Both are 409s over a request the row
	// calls running, so a single message satisfies every status assertion
	// above -- which is how the two came to be one in the first place.
	if same(provisioning, id) == same(orphan, id2) {
		t.Errorf("both 409s say the same thing; provisioning: %s / stale row: %s",
			provisioning, orphan)
	}
	if strings.Contains(provisioning, "restart ember serve to have the queue reconcile it") {
		t.Errorf("the provisioning refusal still names the restart: %s", provisioning)
	}
}

// same strips the request id, which is the only part of the two refusals that
// is legitimately different.
func same(msg, id string) string { return strings.ReplaceAll(msg, id, "<id>") }

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
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

// TestImage_AppearsOnTheRequestAsAPathAndNotAKey.
//
// THE HEADERS USED TO BE ASSERTED HERE and are now asserted over a socket --
// TestHeaders_AnImageIsServedWithItsTypeDeclaredAndNotSniffed. This test
// could not fail for the property its old name claimed: with getImage's two
// Set calls moved to after http.ServeContent, the recorder still reported
// image/png and nosniff while the wire had lost the nosniff. What is left is
// what a recorder is sufficient for -- a status, a body, and the shape of the
// path a browser is given.
func TestImage_AppearsOnTheRequestAsAPathAndNotAKey(t *testing.T) {
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

	// It appears on the request, as a path a browser can fetch rather than a
	// blob key -- which is this process's business and could be an S3 object
	// tomorrow.
	one := decode[requestView](t, h.do(t, "GET", "/api/requests/"+id, ""))
	if len(one.Images) != 1 {
		t.Fatalf("images = %+v", one.Images)
	}
	if !strings.HasPrefix(one.Images[0].Path, "/api/requests/") {
		t.Errorf("path = %q, want a URL and not a key", one.Images[0].Path)
	}
}

// TestImage_AskingForAPreviewNeverServesTheResult.
//
// A preview and a result are different images of the same job, and the
// no-store half -- a cached preview is a progress display that appears to
// freeze -- moved to the socket with the rest of the headers, for the reason
// above.
func TestImage_AskingForAPreviewNeverServesTheResult(t *testing.T) {
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

	// Asking for one must not serve the other.
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
		t.Errorf("status = %d, want 502 when the sweep could not be started", w.Code)
	}
}

// TestNew_CarriesTheScannerFromItsOptions is the join this route did not have.
//
// `GET /api/nodes` answered 501 in every `ember serve` there has ever been:
// `Options` had no Scan field, `New` never assigned the Server's `scan`, and
// cmd_serve could not have set it if it had wanted to. The three tests that
// drove the handler set the unexported field directly, from inside this
// package -- so the suite could not tell "the production value is missing"
// from "this test did not set it", which is precisely how it survived.
//
// THE SCANNER GOES THROUGH THE CONSTRUCTOR, which is what makes the two
// distinguishable. A test that reaches past `New` proves the handler works
// and nothing about whether anything can hand it one; this one proves the
// field a caller fills is the field the handler reads, and the lab
// TestServe_ReportsWhatIsBillingToTheBrowser proves cmd_serve fills it.
// Neither alone is enough: the first cannot see an unwired cmd_serve and the
// second cannot run without Docker.
func TestNew_CarriesTheScannerFromItsOptions(t *testing.T) {
	asked := 0
	h := newHarness(t, func(o *Options) {
		o.Scan = func(context.Context) (app.Scanned, error) {
			asked++
			return app.Scanned{}, nil
		}
	})

	if w := h.do(t, "GET", "/api/nodes", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a server built with a scanner: %s", w.Code, w.Body.String())
	}
	// COUNTED, because 200 alone is consistent with a handler that answered
	// out of nothing. What has to be true is that the function the caller
	// passed is the one that ran.
	if asked != 1 {
		t.Errorf("the scanner was asked %d time(s), want 1", asked)
	}
}

// --- the server itself ---

func TestNew_RefusesWithoutAStore(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("a server with no store was accepted")
	}
}

func TestNew_DefaultsThePort(t *testing.T) {
	s, err := New(Options{Store: genstore.NewRecordingStore(), Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.URL(), fmt.Sprint(DefaultPort)) {
		t.Errorf("URL = %q, want the default port", s.URL())
	}
}

// TestStatic_ServesWhateverWasBuiltAndSaysWhichItIs.
//
// IT ASSERTS THE RELATIONSHIP, NOT ONE OF THE TWO STATES, because both are
// correct: a clean clone has only the committed placeholder, and a tree where
// somebody ran `make web` has a real app. A test that asserted one would pass
// or fail depending on whether the person running it had built the UI, which
// is the "behaves differently from one machine to the next" problem AGENTS.md
// is written against.
//
// What must hold either way is that Built() and the page agree: `ember serve`
// prints "run make web" exactly when the page is the placeholder, so an
// operator is never told to build something that is already there, and never
// left to interpret a page that is a stub.
func TestStatic_ServesWhateverWasBuiltAndSaysWhichItIs(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "GET", "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()

	// The placeholder's own sentence, which only it has. Matching on
	// "make web" would be wrong: the built page's HTML comment mentions the
	// command too, and the first version of this test failed on that.
	const notBuilt = "was not built into this binary"

	if h.Built() {
		// A built page loads the module `make web` produced beside it.
		if !strings.Contains(body, "ember.js") {
			t.Errorf("Built() is true but the page does not load the module:\n%s", body)
		}
		if strings.Contains(body, notBuilt) {
			t.Error("a built page still tells the operator to build it")
		}
	} else {
		if !strings.Contains(body, notBuilt) {
			t.Errorf("Built() is false but the page does not say so:\n%s", body)
		}
		if !strings.Contains(body, "make web") {
			t.Errorf("the placeholder does not name the command that builds it:\n%s", body)
		}
	}

	// Never cached either way: the page is a loader for a module whose name
	// does not change between builds, so a cached one is an operator
	// reloading after `make web` and seeing the old app.
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want no-store", got)
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
		t.Error("a client-side route did not get the app")
	}
}

func TestStatic_IsGuardedToo(t *testing.T) {
	h := newHarness(t)
	r := authorize(httptest.NewRequest("GET", "/", nil))
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

// The optional parameters reach the store, and come back on the request view.
//
// Four layers sit between the body and the generation call -- newJob,
// genstore.Job, a JSON column, sdserver.Job -- and each one is a place a field
// can be dropped by a mapping literal that forgot it. A dropped field is
// silent in the worst way: the request is accepted, the image is generated at
// the server's default, and the page shows the setting the operator typed.
//
// Both directions in one test, because the outbound half reuses the inbound
// type and would otherwise look covered by it.
func TestPostRequest_CarriesTheOptionalParameters(t *testing.T) {
	h := newHarness(t)
	body := `{"provider":"local","jobs":[{
		"prompt":"a cat","clip_skip":2,"eta":0,"flow_shift":3.5,
		"shifted_timestep":4,"output_format":"png","output_compression":90,
		"slg":{"layers":[7,8,9],"layer_start":0.01,"layer_end":0.2,"scale":2.5},
		"hires":{"enabled":true,"upscaler":"Latent","scale":2,"steps":10,
		         "denoising_strength":0.7,"target_width":1024,"target_height":768,
		         "upscale_tile_size":128},
		"vae_tiling":{"enabled":true,"tile_size_w":512,"tile_size_h":256,
		              "target_overlap":0.5,"rel_size_w":0.25,"rel_size_h":0.125}
	}]}`
	w := h.do(t, "POST", "/api/requests", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	id := decode[map[string]string](t, w)["id"]

	// What the store holds is what the generation call will be built from.
	r, err := h.store.Get(t.Context(), genstore.ReqID(id))
	if err != nil {
		t.Fatal(err)
	}
	p := r.Jobs[0].Params
	if p.ClipSkip == nil || *p.ClipSkip != 2 {
		t.Errorf("clip_skip = %v, want 2", p.ClipSkip)
	}
	// Zero through a pointer, which is the case a plain int would lose.
	if p.Eta == nil || *p.Eta != 0 {
		t.Errorf("eta = %v, want a pointer to 0", p.Eta)
	}
	if p.FlowShift == nil || *p.FlowShift != 3.5 {
		t.Errorf("flow_shift = %v, want 3.5", p.FlowShift)
	}
	if p.ShiftedTimestep != 4 || p.OutputFormat != "png" || p.OutputCompression != 90 {
		t.Errorf("scalars = %+v", p)
	}
	if p.SLG.Scale != 2.5 || len(p.SLG.Layers) != 3 || p.SLG.LayerEnd != 0.2 {
		t.Errorf("slg = %+v", p.SLG)
	}
	if !p.Hires.Enabled || p.Hires.Upscaler != "Latent" || p.Hires.Steps != 10 ||
		p.Hires.DenoisingStrength != 0.7 || p.Hires.TargetWidth != 1024 ||
		p.Hires.UpscaleTileSize != 128 {
		t.Errorf("hires = %+v", p.Hires)
	}
	if !p.VAETiling.Enabled || p.VAETiling.TileSizeW != 512 || p.VAETiling.RelSizeH != 0.125 {
		t.Errorf("vae tiling = %+v", p.VAETiling)
	}

	// And back out, so the page that exists to say what was asked for does.
	one := decode[requestView](t, h.do(t, "GET", "/api/requests/"+id, ""))
	j := one.Jobs[0]
	if j.ClipSkip == nil || *j.ClipSkip != 2 {
		t.Errorf("view clip_skip = %v", j.ClipSkip)
	}
	if j.Hires == nil || j.Hires.Upscaler != "Latent" {
		t.Errorf("view hires = %+v", j.Hires)
	}
	if j.SLG == nil || j.SLG.Scale != 2.5 {
		t.Errorf("view slg = %+v", j.SLG)
	}
	if j.VAETiling == nil || j.VAETiling.TileSizeW != 512 {
		t.Errorf("view vae tiling = %+v", j.VAETiling)
	}
}

// A job with none of this set renders without the three empty blocks.
//
// The blocks are pointers so the view of an ordinary job is the view it was
// before they existed. Three `{}`s on every row is not a bug in the data, but
// it is a page telling an operator that something is configured when nothing
// is -- and a reader of the JSON cannot tell "sent empty" from "sent".
func TestRequestView_OmitsTheBlocksAJobDidNotSet(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	raw := h.do(t, "GET", "/api/requests/"+id, "").Body.String()
	for _, key := range []string{`"slg"`, `"hires"`, `"vae_tiling"`, `"clip_skip"`} {
		if strings.Contains(raw, key) {
			t.Errorf("the view carries %s for a job that set nothing: %s", key, raw)
		}
	}
}

// A parameter that is wrong on its face is refused with a 400, before a card.
//
// These go through the same sdserver.ValidateStatic the config path uses, so
// what is refused here and what is refused in ember.yaml cannot drift. The
// upscaler case is the one that matters: it is checked against the server's
// published list, so it only refuses once caps are cached -- which is why the
// harness seeds them.
func TestPostRequest_RefusesAParameterOutsideItsRange(t *testing.T) {
	h := newHarness(t)
	if err := h.store.PutCaps(t.Context(), sdserver.Capabilities{
		Samplers: []string{"euler"}, OutputFormats: []string{"png"},
		Upscalers: []string{"Latent"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, body, want string }{
		{"clip_skip below -1",
			`{"provider":"local","jobs":[{"prompt":"a","clip_skip":-2}]}`, "clip_skip"},
		{"denoising above 1",
			`{"provider":"local","jobs":[{"prompt":"a","hires":{"enabled":true,"denoising_strength":2}}]}`,
			"denoising_strength"},
		{"an empty slg range",
			`{"provider":"local","jobs":[{"prompt":"a","slg":{"scale":1,"layer_start":0.5,"layer_end":0.1}}]}`,
			"layer_end"},
		{"compression above 100",
			`{"provider":"local","jobs":[{"prompt":"a","output_compression":101}]}`, "output_compression"},
		{"a tiling overlap above 1",
			`{"provider":"local","jobs":[{"prompt":"a","vae_tiling":{"enabled":true,"target_overlap":3}}]}`,
			"target_overlap"},
		{"a format the server does not have",
			`{"provider":"local","jobs":[{"prompt":"a","output_format":"tiff"}]}`, "output_format"},
		{"an upscaler the server does not have",
			`{"provider":"local","jobs":[{"prompt":"a","hires":{"enabled":true,"upscaler":"../etc/passwd"}}]}`,
			"upscaler"},
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

// extra is still absent, and the absence is still structural.
//
// The parameters above widen what a request may say; this is the assertion
// that they did not widen HOW it says it. The decoder refuses unknown fields,
// so a body naming `extra` is a 400 and not a silent drop -- which is the
// stronger of the two outcomes and worth pinning, because a later change to
// tolerate unknown fields would turn this refusal into exactly the silent
// acceptance the seal exists to prevent. `extra` reaches every untyped field
// of the generation API, including the ones naming file paths on the node.
func TestPostRequest_ExtraStillCannotBeSet(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a","extra":{"diffusion_model":"/etc/shadow"}}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if got := decode[map[string]string](t, w)["error"]; !strings.Contains(got, "extra") {
		t.Errorf("error = %q, want it to name the field it refused", got)
	}

	// And the same for the nested blocks, which are the new surface: an
	// unknown key inside one must not be accepted either.
	w = h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a","hires":{"enabled":true,"model_path":"/etc/shadow"}}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a nested unknown field was accepted: %d %s", w.Code, w.Body.String())
	}
}

// --- the geometry nothing checked before the first rental ---

// TestPostRequest_RefusesAGeometryNoResponseCouldPermit.
//
// WITH NO CACHED CAPABILITIES, which is the state this is about: `haveCaps`
// is false until something has asked a generation server, so `sdserver.Validate`
// -- the half that reads `limits.max_width` -- is skipped, and a width of
// 3,000,000,000 was answered 202. The row went to the store for good, a card
// was rented for a batch sd-server was always going to refuse, and the queue
// panel could not decode the listing it appeared in.
//
// THE ACCEPTANCE IS ASSERTED BESIDE THE REFUSALS, because a bound that
// refused every geometry would satisfy every refusal here: 8192 is the
// largest figure any captured `limits` publishes and has to be taken.
func TestPostRequest_RefusesAGeometryNoResponseCouldPermit(t *testing.T) {
	cases := map[string]struct {
		job  string
		want string
	}{
		"a width an i32 cannot hold": {
			`{"prompt":"a cat","width":3000000000,"height":512}`,
			"width 3000000000 is above 8192",
		},
		"a height an i32 cannot hold": {
			`{"prompt":"a cat","width":512,"height":3000000000}`,
			"height 3000000000 is above 8192",
		},
		"a hires target past the published upscale maximum": {
			`{"prompt":"a cat","hires":{"enabled":true,"target_width":99999}}`,
			"hires target_width 99999 is above 8192",
		},
		"a negative width": {
			`{"prompt":"a cat","width":-512}`,
			"width -512 is negative",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			body := fmt.Sprintf(`{"provider":"local","jobs":[%s]}`, c.job)
			w := h.do(t, "POST", "/api/requests", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if got := decode[map[string]string](t, w)["error"]; !strings.Contains(got, c.want) {
				t.Errorf("error = %q, want it to name %q", got, c.want)
			}
			if rs, err := h.store.List(t.Context(), genstore.Filter{}); err != nil || len(rs) != 0 {
				t.Errorf("rows = %d (err %v); a refusal before anything is rented must leave "+
					"nothing in the store for the browser to choke on", len(rs), err)
			}
		})
	}

	// The largest geometry any captured response publishes, which this bound
	// is taken from and therefore must accept.
	h := newHarness(t)
	ok := `{"provider":"local","jobs":[{"prompt":"a cat","width":8192,"height":8192,
		"hires":{"enabled":true,"target_width":8192,"target_height":8192}}]}`
	if w := h.do(t, "POST", "/api/requests", ok); w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 for limits.max_upscale_width exactly: %s",
			w.Code, w.Body.String())
	}
}

// listFixture is the listing `web/src/model.rs` decodes in its own test.
//
// WHY A FILE AND NOT TWO HAND-WRITTEN COPIES. Nothing anywhere took Go's
// output and asked whether Rust could read it: `cargo test`'s wire tests feed
// JSON somebody typed, whose numbers are all small, and this package's tests
// assert on Go types, where a width of three billion is fine. The seam's
// agreement was verifiable only by hand, which is how an `int` against an
// `i32` survived -- the same closed loop as `capabilities.model`, in two
// languages instead of against an API.
//
// So the bytes are produced HERE, by the handler, and committed; the Rust
// test reads this exact path with `include_str!`, so a cargo build cannot
// miss it. This test is the half that stops the file drifting from what the
// handler sends.
const listFixture = "testdata/request_list_unrepresentable.json"

// TestRequests_AListingWithARowTheBrowserCannotRead.
//
// THE ROW IS SEEDED THROUGH THE STORE AND NOT THE API, deliberately, and it
// is not a contrived shape: `genstore.Request.Validate` has never looked at a
// geometry, so this is exactly the row an ember from before `sdserver.MaxAxis`
// wrote -- and it is still in the database of anybody who hit this. The
// refusal above stops new ones; it does nothing for the ones already there,
// which is why the browser half of the fix is not belt and braces.
func TestRequests_AListingWithARowTheBrowserCannotRead(t *testing.T) {
	h := newHarness(t)

	// Fixed ids and fixed stamps, because the bytes are committed. Newest
	// first, as both stores order a listing, so the oversized row is second.
	base := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	for _, r := range []genstore.Request{{
		ID: "req-healthy", Provider: "local", GPU: "rtx4090", TTL: time.Hour,
		Created: base.Add(time.Minute), Preview: "proj", PreviewInterval: 5,
		Jobs: []genstore.Job{{Prompt: "a lighthouse", Width: 512, Height: 512, Steps: 4, Seed: 7}},
	}, {
		ID: "req-oversized", Provider: "local", GPU: "rtx4090", TTL: time.Hour,
		Created: base, Preview: "proj", PreviewInterval: 5,
		Jobs: []genstore.Job{{Prompt: "a cat", Width: 3000000000, Height: 512, Steps: 4}},
	}} {
		if _, err := h.store.Enqueue(t.Context(), r); err != nil {
			t.Fatalf("seeding %s: %v", r.ID, err)
		}
	}

	w := h.do(t, "GET", "/api/requests", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := w.Body.String()

	want, err := os.ReadFile(listFixture)
	if err != nil {
		t.Fatalf("reading %s: %v\nthe handler produced:\n%s", listFixture, err, got)
	}
	if got != string(want) {
		t.Errorf("the handler no longer sends what %s holds, and web/src/model.rs decodes "+
			"that file -- update it to:\n%s", listFixture, got)
	}

	// AND GO IS HAPPY WITH IT, which is the whole point of the file: the
	// value is representable here and is not there, so neither language's
	// own tests could have found this.
	var list struct {
		Requests []requestView `json:"requests"`
	}
	if err := json.Unmarshal([]byte(got), &list); err != nil {
		t.Fatalf("Go cannot read its own listing: %v", err)
	}
	if len(list.Requests) != 2 {
		t.Fatalf("requests = %d, want both rows", len(list.Requests))
	}
	if list.Requests[1].Jobs[0].Width != 3000000000 {
		t.Errorf("width = %d; the fixture has to carry the value Rust cannot hold, or the "+
			"test over there passes against a listing with nothing wrong with it",
			list.Requests[1].Jobs[0].Width)
	}
}
