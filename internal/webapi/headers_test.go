package webapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/genstore"
)

// live runs the handler on a real listener.
//
// THE HARNESS LIMIT THAT HID #106 FOR THE LIFE OF THIS PACKAGE, and the
// reason this file exists at all. httptest.ResponseRecorder.Header() returns
// the LIVE map, not the snapshot net/http took when the status line went out,
// so a recorder reports whatever the handler set whenever it set it -- and a
// recorder-based assertion about a declared Content-Type therefore passed
// while every 200 on the wire was going out as `text/plain; charset=utf-8`.
// It is the AssertNoStray failure in a header: the assertion reads a value
// after the thing it checks has already been decided elsewhere.
//
// So every test that asserts on a header net/http could have discarded goes
// through here. THE REST OF THE PACKAGE STAYS ON THE RECORDER on purpose: a
// decision about a request body, a status code or a store call is the same
// decision either way, and a listener per case would buy nothing but a port.
// The line is "does this assertion depend on when a header was set", and the
// answer is no for everything but content types.
//
// It also sets s.port, because httptest picks the port and the guard compares
// the Host header against this server's own -- so the server checks its real
// address rather than a constant.
func live(t *testing.T, h *harness) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	_, port, ok := splitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if !ok {
		t.Fatalf("could not read the port out of %q", srv.URL)
	}
	h.port = port
	return srv
}

// answer is what a client saw: the status line, the headers as they reached
// the socket, and the body.
//
// NOT AN *http.Response. Handing one back makes every call site look to
// bodyclose like a leak, because the linter cannot see that `wire` closed it
// -- and these three fields are all an assertion about a declared type wants.
type answer struct {
	Status int
	Header http.Header
	Body   []byte
}

// wire sends a request over srv's socket and returns what came back.
//
// THE BODY AS WELL AS THE HEADERS, because `live` makes h.do unusable in the
// same test: h.do spells a Host of 127.0.0.1:testPort and the guard now
// compares against the port httptest picked, so a recorder request after
// live() is refused. A test that needs both a declared type and a request id
// has to read both off the same answer -- and the first version of this file
// did not, which showed up as a 200 declared text/html for a path whose id
// was the empty string.
func (h *harness) wire(t *testing.T, srv *httptest.Server, method, path, ctype string, body []byte) answer {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	// The Host http.Client sends is srv's own, so only the Origin has to be
	// spelled -- and it is this server's, as the UI's would be.
	r.Header.Set("Origin", srv.URL)
	authorize(r)

	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", method, path, err)
	}
	return answer{Status: resp.StatusCode, Header: resp.Header, Body: got}
}

// wireID posts a request over the socket and reads the id back.
func (h *harness) wireID(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	a := h.wire(t, srv, "POST", "/api/requests", "application/json", []byte(oneJob))
	if a.Status != http.StatusAccepted {
		t.Fatalf("enqueue: status = %d: %s", a.Status, a.Body)
	}
	var out map[string]string
	if err := json.Unmarshal(a.Body, &out); err != nil {
		t.Fatalf("decode %q: %v", a.Body, err)
	}
	if out["id"] == "" {
		t.Fatalf("no id on %q", a.Body)
	}
	return out["id"]
}

// declared is the assertion the recorder could not make: what a client was
// told this body is.
func declared(t *testing.T, a answer, want int, ctype string) {
	t.Helper()
	if a.Status != want {
		t.Errorf("status = %d, want %d", a.Status, want)
	}
	if got := a.Header.Get("Content-Type"); got != ctype {
		t.Errorf("content-type = %q, want %q", got, ctype)
	}
	if got := a.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("x-content-type-options = %q, want nosniff", got)
	}
}

// TestHeaders_EveryJSONAnswerIsDeclaredJSONOnTheWire.
//
// A SHORT LIST AND NOT A ROUTE INVENTORY, which is the point of the fix
// rather than a gap in the test: s.write is now the one place a JSON response
// gets its header and its status, so a single 200 stands for every s.ok call
// site. What the list has to cover is the call sites that do NOT go through
// ok -- postRequest's 202, postInput's 201 -- and fail, which was the only
// one that was ever right and would be the one to break if somebody moved the
// header back out of the helper.
//
// All three statuses and both outcomes, because the defect was an inversion:
// successes were declared text/plain and errors application/json, so a test
// that only looked at errors would have reported a correct server.
func TestHeaders_EveryJSONAnswerIsDeclaredJSONOnTheWire(t *testing.T) {
	h := newHarness(t)
	srv := live(t, h)

	// postRequest's own WriteHeader, and the id every route below needs.
	id := h.wireID(t, srv)

	for _, c := range []struct {
		name, method, path, ctype string
		body                      []byte
		want                      int
	}{
		// s.ok, with nothing to read and with a body to encode.
		{"200 from ok", "GET", "/api/config", "", nil, http.StatusOK},
		{"200 from ok over a stored row", "GET", "/api/requests/" + id, "", nil, http.StatusOK},
		{"202 from postRequest", "POST", "/api/requests", "application/json", []byte(oneJob), http.StatusAccepted},
		// postInput's own WriteHeader.
		{"201 from postInput", "POST", "/api/inputs", "application/octet-stream", samplePNG(t, 8, 8), http.StatusCreated},
		// fail, which was correct by accident and must stay correct on
		// purpose: it is the one that breaks if the header moves back out of
		// the helper.
		{"404 from fail", "GET", "/api/requests/nope", "", nil, http.StatusNotFound},
		{"400 from fail", "POST", "/api/requests", "application/json", []byte(`{"jobs":[]}`), http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := h.wire(t, srv, c.method, c.path, c.ctype, c.body)
			declared(t, a, c.want, "application/json")
			// The body is JSON as well as declared JSON -- otherwise a
			// handler that declared the type and wrote something else would
			// pass, which is this defect with the halves swapped.
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(a.Body, &obj); err != nil {
				t.Errorf("body is not a JSON object: %v: %s", err, a.Body)
			}
		})
	}
}

// TestHeaders_TheRefusedPageLoadIsDeclaredPlainTextOnTheWire.
//
// The one route that answers something other than JSON, and the one whose
// declared type an operator reads directly: this is what they see when they
// type the port from memory or open a bookmark from before a restart, so a
// body declared text/plain and a browser told not to sniff it is the whole
// reason it is not JSON. Over a real connection because its three headers are
// set in the same breath as the status, and a recorder cannot tell whether
// the breath came in the right order -- the bug this file was written for.
func TestHeaders_TheRefusedPageLoadIsDeclaredPlainTextOnTheWire(t *testing.T) {
	h := newHarness(t)
	srv := live(t, h)

	// No credential, deliberately: this is the refusal path, so the request
	// does not go through h.wire.
	a := h.unauth(t, srv, "/")

	declared(t, a, http.StatusForbidden, "text/plain; charset=utf-8")
	if got := a.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want no-store", got)
	}
	if !strings.Contains(string(a.Body), errNoCredential.Error()) {
		t.Errorf("body = %q, want the one refusal sentence", a.Body)
	}
}

// TestHeaders_AnImageIsServedWithItsTypeDeclaredAndNotSniffed.
//
// Declared, never sniffed: these are bytes a rented machine produced, and
// letting a browser decide what they are is letting the node decide.
//
// WHY THIS ONE MOVED HERE. It is the test the comment above getImage was
// written for, and it passed with the property destroyed: with both Set calls
// moved to after http.ServeContent, the recorder still reported image/png and
// nosniff while the wire had lost the nosniff entirely -- Go sniffs \x89PNG
// to the same answer, so only the header that cannot be guessed went missing,
// which is the half that matters. Over a real connection the same
// reinstatement goes red.
func TestHeaders_AnImageIsServedWithItsTypeDeclaredAndNotSniffed(t *testing.T) {
	h := newHarness(t)
	srv := live(t, h)
	id := h.wireID(t, srv)

	key := id + "/cat-00.png"
	if err := h.blobs.Put(t.Context(), key, []byte("\x89PNG\r\n\x1a\nnot-really")); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddImage(t.Context(), genstore.Image{
		Req: genstore.ReqID(id), Job: 1, Index: 0, Key: key, Bytes: 17,
	}); err != nil {
		t.Fatal(err)
	}

	res := h.wire(t, srv, "GET", "/api/requests/"+id+"/images/1/0", "", nil)
	declared(t, res, http.StatusOK, "image/png")
	// A result never changes, so it is worth caching.
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("cache-control = %q, want a result to be cacheable", got)
	}

	// A preview is written over as sampling proceeds, so a cached one is a
	// progress display that appears to freeze.
	pkey := id + "/cat-preview-00.png"
	if err := h.blobs.Put(t.Context(), pkey, []byte("\x89PNG\r\n\x1a\nframe")); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddImage(t.Context(), genstore.Image{
		Req: genstore.ReqID(id), Job: 2, Index: 0, Key: pkey, Preview: true,
	}); err != nil {
		t.Fatal(err)
	}
	pre := h.wire(t, srv, "GET", "/api/requests/"+id+"/images/2/0?preview=1", "", nil)
	declared(t, pre, http.StatusOK, "image/png")
	if got := pre.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("preview cache-control = %q, want no-store", got)
	}
}

// unauth sends a request carrying nothing, over the socket.
func (h *harness) unauth(t *testing.T, srv *httptest.Server, path string) answer {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), "GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: reading the body: %v", path, err)
	}
	return answer{Status: resp.StatusCode, Header: resp.Header, Body: body}
}
