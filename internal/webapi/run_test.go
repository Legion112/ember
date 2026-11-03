//go:build unix

package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
)

// UNIX-GATED FOR socket_test.go's REASON, and the reason is the feature rather
// than the test: ListenSocket is //go:build unix and refuses on every other
// platform, so a privileged route on a channel that cannot exist is not a
// property to assert there. The mux-shape assertion below is portable in
// principle and lives here anyway, because the thing it is about -- which
// channel a route is on -- has only one channel off unix.

// RecordingRuns is the run starter as a handler test can see it.
//
// The house fake shape, and `err` rather than a bool because every refusal on
// this path is a sentence the client prints in full -- which is the half #108
// found the page throwing away. `Asks` is what makes "the route did not run"
// an assertion rather than an inference from a status code: a 200 of the UI's
// own HTML and a 404 are both "not accepted", and neither says whether a
// rental was started.
type RecordingRuns struct {
	mu   sync.Mutex
	Asks []app.RunAsk
	ID   genstore.ReqID
	Err  error
}

func (r *RecordingRuns) Start(_ context.Context, ask app.RunAsk) (genstore.ReqID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Asks = append(r.Asks, ask)
	if r.Err != nil {
		return "", r.Err
	}
	id := r.ID
	if id == "" {
		id = "req_fake"
	}
	return id, nil
}

func (r *RecordingRuns) asks() []app.RunAsk {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]app.RunAsk(nil), r.Asks...)
}

// runHarness is a server whose privileged route is wired to a fake.
func runHarness(t *testing.T) (*harness, *RecordingRuns) {
	t.Helper()
	runs := &RecordingRuns{}
	h := newHarness(t, func(o *Options) { o.Runs = runs })
	return h, runs
}

// aRunBody is a well-formed ask, as the client sends one.
func aRunBody(cfg string) string {
	b, err := json.Marshal(RunBody{Config: cfg, GPU: "rtx4090", TTL: "30m"})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestRun_TheRouteDoesNotExistOnTheLoopbackPort.
//
// THE SEAL, AND IT IS A MISSING REGISTRATION RATHER THAN A REFUSAL. This route
// carries a config path, argv, and ceilings that REPLACE the file's -- which
// is everything postRequest is written not to accept, because a browser must
// not name the file that sets its own limits. A runtime check on
// arrivedOnSocket would have been one line and would have left the route
// present on the port, which is a condition a later change can move. So the
// assertion is on the two route tables: the port's mux does not have the
// pattern at all, and the socket's does.
//
// ServeMux.Handler IS WHAT ANSWERS IT, because it returns the pattern that
// matched -- so this reads the thing that decides rather than inferring it
// from a status. Inferring would be weak here in exactly the way this
// repository keeps recording: `/api/runs` on the port falls through to `/`,
// which is the client-rendered app's own route, so a POST to it answers 200
// with the UI's HTML -- a status no assertion should be built on.
func TestRun_TheRouteDoesNotExistOnTheLoopbackPort(t *testing.T) {
	h, _ := runHarness(t)
	r := httptest.NewRequest(http.MethodPost, RunPath, nil)

	_, onPort := h.mux.Handler(r)
	if onPort == "POST "+RunPath {
		t.Errorf("the loopback port's mux matched %q for %s %s: a page, a curl on the port, a "+
			"forwarded `ssh -L` and a second account with the token all reach that pattern, and "+
			"it accepts a config path and raises money ceilings", onPort, r.Method, RunPath)
	}

	_, onSocket := h.sockMux.Handler(r)
	if onSocket != "POST "+RunPath {
		t.Errorf("the socket's mux matched %q, so the privileged route is reachable from nowhere "+
			"and `ember up` over the socket cannot work", onSocket)
	}
}

// TestRun_TheSocketServesEveryOtherRouteThroughTheOnePortTable.
//
// The second mux DELEGATES rather than re-registering, so there is one route
// table and a route added to the API is on both channels without anybody
// remembering. This is what stops the two drifting -- the shape the image
// labs' build-tag lists were corrected into.
func TestRun_TheSocketServesEveryOtherRouteThroughTheOnePortTable(t *testing.T) {
	h, _ := runHarness(t)
	for _, path := range []string{"/api/config", "/api/offers", "/api/requests", "/ws", "/"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		got, pattern := h.sockMux.Handler(r)
		if pattern != "/" {
			t.Errorf("the socket's mux matched %s with its own pattern %q; every route but the "+
				"privileged one is meant to be the port's table, reached through the one "+
				"delegation", path, pattern)
		}
		// HANDLER IDENTITY AND NOT A STATUS. What is being asserted is that
		// there is ONE route table: the socket's `/` is the port's whole mux,
		// so a route added to the API is on both channels without anybody
		// remembering -- which is what stops the two drifting the way the
		// image labs' build-tag lists did.
		if got != http.Handler(h.mux) {
			t.Errorf("the socket's mux does not delegate %s to the port's table, so the two can "+
				"come to serve different route sets", path)
		}
	}
}

// TestRun_TheSocketStartsTheRunAndTheTokenOnThePortDoesNot.
//
// BOTH DIRECTIONS IN ONE TEST, because a refusal on its own is consistent with
// a route that refuses everything -- the zero-Pin lesson -- and the acceptance
// on its own cannot say which channel accepted it. The port's request carries
// the server's own Host, Origin and token, which is to say it passes every
// check in guard.go and auth.go: what stops it is that there is no handler.
func TestRun_TheSocketStartsTheRunAndTheTokenOnThePortDoesNot(t *testing.T) {
	h, runs := runHarness(t)
	runs.ID = "req_abc"
	c, _ := socketServer(t, h)

	got := overSocket(t, c, http.MethodPost, "localhost", "", RunPath, aRunBody("/tmp/ember.yaml"))
	if got.Status != http.StatusAccepted {
		t.Fatalf("over the socket: status %d, body %s", got.Status, got.Body)
	}
	var acc RunAccepted
	if err := json.Unmarshal(got.Body, &acc); err != nil {
		t.Fatal(err)
	}
	if acc.ID != "req_abc" {
		t.Errorf("accepted as %q, want the id the runner gave", acc.ID)
	}
	if n := len(runs.asks()); n != 1 {
		t.Fatalf("the runner was asked %d time(s), want 1", n)
	}
	ask := runs.asks()[0]
	if ask.Config != "/tmp/ember.yaml" || ask.Overrides.GPU != "rtx4090" || ask.Overrides.TTL.Minutes() != 30 {
		t.Errorf("the ask did not survive the wire: %+v", ask)
	}

	// AND ON THE PORT, with everything a request can carry.
	onPort := h.do(t, http.MethodPost, RunPath, aRunBody("/tmp/ember.yaml"))
	if onPort.Code == http.StatusAccepted {
		t.Errorf("the loopback port accepted a run (%d): %s", onPort.Code, onPort.Body.String())
	}
	if n := len(runs.asks()); n != 1 {
		t.Errorf("the runner was asked %d time(s) after a request on the port, want still 1: "+
			"a route that carries a config path and replaces money ceilings must not be "+
			"reachable from a channel a browser can open", n)
	}
}

// TestRun_RefusesABodyNobodyCouldHaveMeant, before a config is read.
//
// Every one of these is free and every one is answered as a 400 with the
// sentence -- which is what the client prints. The unknown field is the one
// worth having: a `max_hourly` that silently did nothing would run at the
// config's ceiling and the operator would never learn which number bounded
// their card, which is config.Load's own KnownFields decision reached over a
// socket.
func TestRun_RefusesABodyNobodyCouldHaveMeant(t *testing.T) {
	h, runs := runHarness(t)
	c, _ := socketServer(t, h)

	cases := []struct{ name, body, want string }{
		{"not json", "{", "read the run"},
		{"an unknown field", `{"config":"/tmp/e.yaml","max_hourly":"1"}`, "max_hourly"},
		{"no config at all", `{}`, "config file"},
		{"a relative config path", `{"config":"ember.yaml"}`, "relative"},
		{"a ttl that is not a duration", `{"config":"/tmp/e.yaml","ttl":"half an hour"}`, "ttl"},
		{"a hold that is not a duration", `{"config":"/tmp/e.yaml","hold":"ages"}`, "hold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := overSocket(t, c, http.MethodPost, "localhost", "", RunPath, tc.body)
			if got.Status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", got.Status, got.Body)
			}
			if !strings.Contains(string(got.Body), tc.want) {
				t.Errorf("the refusal should name %q, got %s", tc.want, got.Body)
			}
		})
	}
	if n := len(runs.asks()); n != 0 {
		t.Errorf("%d malformed ask(s) reached the runner", n)
	}
}

// TestRun_CarriesTheRunnersOwnRefusalInFull.
//
// A refusal from Start is a refusal `ember up` would have made itself -- an
// unpinned image, a card with no image, a store missing the weights, a
// provider that is not free -- and those messages carry the remediation. A
// status code instead would throw away the only part an operator can act on,
// which is exactly what the cancel button did for the life of #108.
//
// 400 AND NOT 500, because none of them is ember failing.
func TestRun_CarriesTheRunnersOwnRefusalInFull(t *testing.T) {
	h, runs := runHarness(t)
	runs.Err = errors.New("no image for sm_89, which is what RTX 4090 is; images: has [sm_75]")
	c, _ := socketServer(t, h)

	got := overSocket(t, c, http.MethodPost, "localhost", "", RunPath, aRunBody("/tmp/ember.yaml"))
	if got.Status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", got.Status, got.Body)
	}
	if !strings.Contains(string(got.Body), "images: has [sm_75]") {
		t.Errorf("the runner's sentence did not reach the client in full: %s", got.Body)
	}
}

// TestRun_AServerWithNoRentalLoopSaysSoRatherThanPanicking.
//
// Options.Runs is nil for every handler test in this package and for `ember
// serve` before the loop exists, which is the same tolerance Queue has -- and
// 501 rather than a panic for the reason getNodes answers 501 with no scanner:
// a nil in a handler is a 500 with a stack in the log and nothing an operator
// can read.
func TestRun_AServerWithNoRentalLoopSaysSoRatherThanPanicking(t *testing.T) {
	h := newHarness(t)
	c, _ := socketServer(t, h)

	got := overSocket(t, c, http.MethodPost, "localhost", "", RunPath, aRunBody("/tmp/ember.yaml"))
	if got.Status != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501: %s", got.Status, got.Body)
	}
	if !strings.Contains(string(got.Body), "rental loop") {
		t.Errorf("the refusal should say what is missing, got %s", got.Body)
	}
}

// TestRunBody_RoundTripsEveryOverride, which is the whole of what the channel
// carries.
//
// ONE TYPE FOR BOTH ENDS is what makes this cheap: internal/cli marshals this
// struct and this package decodes it, so the test is the field list rather
// than two declarations somebody has to keep in step. The Go-to-Rust seam in
// this program is the other shape, and it has cost a nil slice read as a
// missing field and a width an i32 could not hold.
func TestRunBody_RoundTripsEveryOverride(t *testing.T) {
	in := RunBody{
		Config: "/home/op/ember.yaml", Provider: "runpod", Image: "x@sha256:abc",
		GPU: "h100-80g", MaxHourlyUSD: "2.49", TTL: "2h", Hold: "15m",
		Ports: []string{"ui:8188"}, Cmd: []string{"sd-cli", "--help"},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out RunBody
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		// DeepEqual over the whole struct rather than field by field, so a
		// field added later is in the comparison by construction -- which is
		// the reason ws.go compares the marshalled view instead of a few
		// chosen fields.
		t.Errorf("the body did not round trip: %+v -> %+v", in, out)
	}
	ask, err := out.toAsk()
	if err != nil {
		t.Fatal(err)
	}
	if ask.Config != in.Config || ask.Hold.Minutes() != 15 || ask.Overrides.TTL.Hours() != 2 {
		t.Errorf("toAsk lost something: %+v", ask)
	}
	if len(ask.Overrides.Ports) != 1 || ask.Overrides.Ports[0] != "ui:8188" {
		t.Errorf("ports = %v", ask.Overrides.Ports)
	}
	if len(ask.Overrides.Cmd) != 2 || ask.Overrides.Cmd[1] != "--help" {
		t.Errorf("cmd = %v", ask.Overrides.Cmd)
	}
	if ask.Overrides.MaxHourlyUSD != "2.49" {
		t.Errorf("max_hourly_usd = %q", ask.Overrides.MaxHourlyUSD)
	}
}

// TestRun_ASubmittedRunsCancelGetsTheProvisioningAnswer, which is a
// LIMITATION asserted rather than left to be noticed.
//
// A run submitted by `ember up` has a row that reads `running` from the moment
// Adopt writes it, and the drain loop holds it -- so cancelRequest picks the
// GENTLER of its two 409s, the one that says "ask again once it is
// generating". That run never becomes `Running`: there is no sd-server job to
// stop, because the only way to end an `ember up` is to destroy the node,
// which is `ember down`. So the message promises something it cannot keep.
//
// IT IS NOT FIXED HERE AND THE REASON IS THE SHAPE OF THE FIX. Telling the two
// apart needs a FOURTH answer from the queue, and a fourth answer is a change
// to webapi.Drain that belongs with `ember down` over the socket -- the slice
// that makes a cancel of a submitted run mean something. Asserting the current
// answer is TestConformance_CannotCatchARederivedRunID's arrangement: a
// limitation nobody can read out of the code is a limitation that will be
// forgotten, so the day somebody fixes this the test goes red and they read
// this comment. The other 409 -- "restart ember serve" -- would be worse here
// and is asserted absent for that reason: restarting destroys the rental the
// operator is paying for.
func TestRun_ASubmittedRunsCancelGetsTheProvisioningAnswer(t *testing.T) {
	drain := &RecordingDrain{}
	h := newHarness(t, func(o *Options) { o.Queue = drain })

	id, err := h.store.Adopt(t.Context(), genstore.Request{Provider: "local", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// What queue.direct does while a submitted run is in flight.
	drain.holding = id

	got := h.do(t, http.MethodPost, "/api/requests/"+string(id)+"/cancel", "")
	if got.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", got.Code, got.Body.String())
	}
	body := got.Body.String()
	if !strings.Contains(body, "has a rental coming up") {
		t.Errorf("the refusal is not the provisioning-window one: %s", body)
	}
	// The DESTRUCTIVE answer's own clause, and the phrase is the whole of it:
	// the gentle message also contains the word "restart", in "do NOT restart
	// ember serve", so a substring of one word would pass against the other
	// message -- which is the "a marker matching a second line" trap in an
	// assertion.
	if strings.Contains(body, "reconcile it") {
		t.Errorf("the refusal tells the operator to restart ember serve, which destroys the "+
			"rental they are paying for: %s", body)
	}
	if !strings.Contains(body, "do not restart") {
		t.Errorf("the refusal does not warn against the destructive remedy: %s", body)
	}
	// THE LIMITATION ITSELF. If this stops being the answer, the comment above
	// is what to read: a fourth answer from the queue is the fix, and it
	// arrives with `ember down` over the socket.
	if !strings.Contains(body, "ask again once it is generating") {
		t.Errorf("the answer for a submitted run has changed; see the comment above this test "+
			"-- the promise it could not keep may now be kept, in which case delete this "+
			"assertion and the AGENTS.md entry beside it: %s", body)
	}
}
