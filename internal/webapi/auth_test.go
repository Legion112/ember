package webapi

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
)

// everyRoute is the whole surface, as a request a browser or a script makes.
//
// NAMED ONCE AND USED BY THE REFUSAL TEST, because the failure this guards
// against is a route added later that forgets the middleware -- which is the
// shape of bug AGENTS.md records for AssertLeftoverCapsMatch: a claim checked
// in one direction only. /ws is here too, since "a guard that covers the JSON
// routes and not the socket guards nothing".
var everyRoute = []struct{ method, path string }{
	{"GET", "/"},
	{"GET", "/requests/abc"},
	{"GET", "/api/config"},
	{"GET", "/api/offers"},
	{"GET", "/api/nodes"},
	{"POST", "/api/requests"},
	{"GET", "/api/requests"},
	{"GET", "/api/requests/abc"},
	{"POST", "/api/requests/abc/cancel"},
	{"GET", "/api/requests/abc/events"},
	{"GET", "/api/requests/abc/images/0/0"},
	{"GET", "/ws?id=abc"},
}

// unauthenticated builds a request that passes the Host and Origin checks and
// carries no credential.
//
// IT LOOKS EXACTLY LIKE THE OPERATOR'S OWN curl, which is the point: a second
// uid on this machine can reach a loopback port, sends no Origin -- so
// checkOrigin waves it through, deliberately -- and addresses the server by
// its real Host. Everything in guard.go passes. The credential is the only
// thing that tells this request from the one h.do makes.
func unauthenticated(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(oneJob))
	r.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	return r
}

// TestAuth_RefusesEveryRouteWithoutTheCredential.
//
// THE REFUSAL IS THE ASSERTION WORTH HAVING. A credential with no test for
// what it refuses is the AssertNoStray failure: an assertion that cannot
// fail, passing for the life of the suite. Each of these was confirmed by
// removing the check from guarded and watching this go red.
func TestAuth_RefusesEveryRouteWithoutTheCredential(t *testing.T) {
	for _, c := range everyRoute {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			h := newHarness(t)
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, unauthenticated(c.method, c.path))

			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403: %s", w.Code, w.Body.String())
			}
			// AND NOTHING WAS READ OR WRITTEN. A 403 over a handler that had
			// already queued the request would be the wrong half of the
			// check: the browser hides the response and the side effect has
			// already happened, which is the whole reason this file exists.
			if n := len(h.store.Calls); n != 0 {
				t.Errorf("the store was touched %d time(s) by a refused request: %v",
					n, h.store.Calls)
			}
		})
	}
}

// TestAuth_AcceptsEitherCarrier.
//
// Two spellings of one secret and no precedence between them: a browser sends
// the cookie, because it cannot set a header on a websocket handshake or on an
// <img>; curl and the labs send the header, because a shell has no cookie jar
// worth the trouble.
func TestAuth_AcceptsEitherCarrier(t *testing.T) {
	tok := string(testToken.Bytes())

	for _, c := range []struct {
		name string
		set  func(*http.Request)
	}{
		{"an Authorization header", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+tok)
		}},
		{"a lowercase scheme, which RFC 7235 says is the same", func(r *http.Request) {
			r.Header.Set("Authorization", "bearer "+tok)
		}},
		{"the cookie the exchange sets", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		}},
		{"a wrong header and the right cookie", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer not-the-token")
			r.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			r := unauthenticated("GET", "/api/config")
			c.set(r)
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestAuth_RefusesEveryWrongCredential.
//
// A TOKEN OF THE WRONG LENGTH IS NOT A SPECIAL CASE, and that is the design
// rather than an omission: tokenMatches hashes both sides before comparing,
// so every comparison is the same thirty-two bytes and a short token, a long
// one and a correct prefix are all simply wrong. The prefix cases are here
// because `==` would answer them in measurably different times, which is what
// subtle.ConstantTimeCompare over fixed-width digests removes.
func TestAuth_RefusesEveryWrongCredential(t *testing.T) {
	tok := string(testToken.Bytes())

	for _, c := range []struct{ name, value string }{
		{"empty", ""},
		{"another token entirely", string(NewToken().Bytes())},
		{"the right token truncated", tok[:len(tok)-1]},
		{"the right token with one byte more", tok + "A"},
		{"one correct character", tok[:1]},
		{"the right length and the wrong bytes", strings.Repeat("A", len(tok))},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)

			for _, carrier := range []string{"header", "cookie"} {
				r := unauthenticated("GET", "/api/config")
				if carrier == "header" {
					r.Header.Set("Authorization", "Bearer "+c.value)
				} else {
					r.AddCookie(&http.Cookie{Name: cookieName, Value: c.value})
				}
				w := httptest.NewRecorder()
				h.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusForbidden {
					t.Errorf("%s: status = %d, want 403", carrier, w.Code)
				}
			}
		})
	}
}

// TestAuth_SaysTheSameThingWhateverWentWrong.
//
// One message for no credential, a wrong one and a stale cookie. A caller
// learning which of the three it was learns something about the secret, and
// the operator does not need to be told -- the remediation is the same line.
func TestAuth_SaysTheSameThingWhateverWentWrong(t *testing.T) {
	h := newHarness(t)

	var bodies []string
	for _, set := range []func(*http.Request){
		func(*http.Request) {},
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") },
		func(r *http.Request) { r.Header.Set("Authorization", "Basic not-even-bearer") },
		func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: "stale"}) },
	} {
		r := unauthenticated("GET", "/api/config")
		set(r)
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		bodies = append(bodies, w.Body.String())
	}
	for i, b := range bodies {
		if b != bodies[0] {
			t.Errorf("refusal %d differs from the first:\n%s\n%s", i, bodies[0], b)
		}
	}
	// And it names the way out. An error that refuses without saying how to
	// proceed is how an operator concludes the server is broken and goes
	// looking for an older binary.
	for _, want := range []string{"?" + tokenParam + "=", "Authorization: Bearer", "serve_token"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, bodies[0])
		}
	}
}

// TestAuth_TheTokenReachesNoLogLineAndNoErrorBody.
//
// node.Secret's discipline, applied to the one credential that arrives over
// the wire: carry it, never print it. The refusal log records the path and
// which carrier was present and nothing else -- not the presented value and
// not a digest of one, because an operator who pasted a password from another
// service into the box would otherwise have it hashed into the journal
// permanently.
func TestAuth_TheTokenReachesNoLogLineAndNoErrorBody(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	tok := string(testToken.Bytes())
	const other = "somebody-elses-password"

	// A refused request carrying a wrong secret, an accepted one carrying the
	// right secret, a refused page load, and a 404 -- which AGENTS.md names
	// separately because an error that interpolates what it was given is how
	// a secret escapes without anybody writing a log line for it.
	for _, r := range []*http.Request{
		func() *http.Request {
			r := unauthenticated("GET", "/api/config")
			r.Header.Set("Authorization", "Bearer "+other)
			return r
		}(),
		func() *http.Request {
			r := unauthenticated("GET", "/api/config")
			r.Header.Set("Authorization", "Bearer "+tok)
			return r
		}(),
		unauthenticated("GET", "/"),
		func() *http.Request {
			r := unauthenticated("GET", "/api/requests/abc/images/9/9?"+tokenParam+"="+tok)
			r.Header.Set("Authorization", "Bearer "+tok)
			return r
		}(),
	} {
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), tok) {
			t.Errorf("a response body carries the token:\n%s", w.Body.String())
		}
		if strings.Contains(w.Body.String(), other) {
			t.Errorf("a response body echoes the presented credential:\n%s", w.Body.String())
		}
	}

	got := logged.String()
	if strings.Contains(got, tok) {
		t.Errorf("the token reached a log line:\n%s", got)
	}
	if strings.Contains(got, other) {
		t.Errorf("a presented credential reached a log line:\n%s", got)
	}
	// It did log the refusals, though -- a check whose failures are invisible
	// is a check nobody can debug.
	if !strings.Contains(got, "carried no credential") {
		t.Errorf("the refusal was not logged at all:\n%s", got)
	}
}

// TestExchange_TurnsTheOneTimeURLIntoACookie.
//
// THE ONE PLACE A TOKEN MAY APPEAR IN A URL, and the exchange is what takes
// it back out: the browser's next request is a GET of a path with no token in
// it, so the address bar and the session history do not keep one.
func TestExchange_TurnsTheOneTimeURLIntoACookie(t *testing.T) {
	h := newHarness(t)
	tok := string(testToken.Bytes())

	for _, path := range []string{"/", "/requests/abc", "/requests/abc?watch=1"} {
		t.Run(path, func(t *testing.T) {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, unauthenticated("GET", path+sep+tokenParam+"="+tok))

			if w.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303: %s", w.Code, w.Body.String())
			}
			// Nothing downstream may carry it: not the Location, not a
			// Referer from the page it redirects to.
			loc := w.Header().Get("Location")
			if strings.Contains(loc, tok) || strings.Contains(loc, tokenParam+"=") {
				t.Errorf("the redirect keeps the token: %s", loc)
			}
			if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("referrer-policy = %q, want no-referrer", got)
			}

			cs := w.Result().Cookies() //nolint:bodyclose // a recorder's Result has no body to close
			if len(cs) != 1 {
				t.Fatalf("got %d cookie(s), want 1", len(cs))
			}
			c := cs[0]
			if c.Name != cookieName || c.Value != tok {
				t.Errorf("cookie = %s=%q", c.Name, c.Value)
			}
			// HttpOnly, so a wasm panic or a stray console cannot read the
			// credential back out of the page that holds it.
			if !c.HttpOnly {
				t.Error("the cookie is readable from script")
			}
			// SameSite=Strict is the part that is not convenience: it is what
			// stops a cross-site no-Origin GET -- an <img src> or a top-level
			// navigation, the one browser case guard.go lets through -- from
			// carrying the credential.
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("samesite = %v, want Strict", c.SameSite)
			}
			if c.Path != "/" {
				t.Errorf("path = %q, want / so the API and the socket are covered", c.Path)
			}
		})
	}
}

// TestExchange_AWrongTokenInTheURLIsJustRefused.
//
// It sets no cookie and redirects nowhere: the credential check behind it
// answers with the one message every other failure gets. This is an online
// guessing oracle in principle, which is why the minted token is 128 bits of
// crypto/rand rather than something an operator chose.
func TestExchange_AWrongTokenInTheURLIsJustRefused(t *testing.T) {
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, unauthenticated("GET", "/?"+tokenParam+"=wrong"))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if cs := w.Result().Cookies(); len(cs) != 0 { //nolint:bodyclose // a recorder's Result has no body to close
		t.Errorf("a wrong token set %d cookie(s)", len(cs))
	}
}

// TestExchange_IsHonouredOnTheAppRouteAndNowhereElse.
//
// An allow-list of exactly one path shape. A query parameter the whole server
// honoured would be a token in a URL on every route -- including the image
// route a gallery <img> hits and the socket a watcher holds open, which are
// the two places a secret in a query string would then live for the life of a
// generation.
func TestExchange_IsHonouredOnTheAppRouteAndNowhereElse(t *testing.T) {
	h := newHarness(t)
	q := "?" + tokenParam + "=" + string(testToken.Bytes())

	for _, path := range []string{
		"/api/config" + q,
		"/api/requests" + q,
		"/ws" + q + "&id=abc",
		// A file rather than a client-rendered route: base has a dot, so it
		// is served by the file handler and never exchanges anything.
		"/ember.js" + q,
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, unauthenticated("GET", path))
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403: %s", w.Code, w.Body.String())
			}
			if cs := w.Result().Cookies(); len(cs) != 0 { //nolint:bodyclose // a recorder's Result has no body to close
				t.Errorf("%d cookie(s) were set off the app route", len(cs))
			}
		})
	}
}

// TestExchange_WillNotRedirectOffThisServer.
//
// AN OPEN REDIRECT IN THE ONE ROUTE THAT HANDLES A CREDENTIAL would be the
// worst place in this program to have one, and it was reachable in the first
// version: a path of "//evil.example" survives
// `(&url.URL{Path: ...}).String()` unchanged, so the Location came out
// protocol-relative and a browser would have followed it to somebody else's
// site -- with the cookie already set. gosec's G710 found it.
//
// exchange is called directly here, because http.ServeMux cleans a double
// slash and 301s before the handler sees it. That makes the refusal
// unreachable through Handler() TODAY, which is a claim about today's mux and
// not about this function, and a guard whose test cannot reach it is a guard
// nobody can show works.
func TestExchange_WillNotRedirectOffThisServer(t *testing.T) {
	h := newHarness(t)
	tok := string(testToken.Bytes())

	for _, path := range []string{"//evil.example", "//evil.example/requests"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.URL = &url.URL{Path: path, RawQuery: tokenParam + "=" + tok}
			w := httptest.NewRecorder()

			if !h.exchange(w, r) {
				t.Fatal("exchange did not answer the request at all")
			}
			if loc := w.Header().Get("Location"); loc != "" {
				t.Errorf("it redirected to %q", loc)
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}

	// And an ordinary path still redirects, or the guard above would be
	// refusing everything and this file would still pass.
	r := httptest.NewRequest("GET", "/requests/abc?"+tokenParam+"="+tok, nil)
	w := httptest.NewRecorder()
	if !h.exchange(w, r) {
		t.Fatal("exchange ignored a valid one-time URL")
	}
	if got := w.Header().Get("Location"); got != "/requests/abc" {
		t.Errorf("location = %q, want /requests/abc", got)
	}
}

// TestStatic_RefusesAPageLoadReadably.
//
// This is what an operator sees when they type localhost:7777 from memory, or
// open a bookmark from before a restart changed the minted token. A raw JSON
// object rendered in a browser reads as "ember is broken", which is the
// failure mode that gets a control worked around rather than used.
func TestStatic_RefusesAPageLoadReadably(t *testing.T) {
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, unauthenticated("GET", "/"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", got)
	}
	// nosniff and no-store, as the rest of this handler sets: a browser must
	// not decide these bytes are HTML, and a cached refusal would outlive the
	// cookie that fixes it.
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("x-content-type-options = %q, want nosniff", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want no-store", got)
	}
	body := w.Body.String()
	for _, want := range []string{"ember serve", "?" + tokenParam + "=", "serve_token"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal page does not mention %q:\n%s", want, body)
		}
	}
	// Not the app, which is the half that would make this test pass against a
	// server that served the UI anyway.
	if strings.Contains(body, "<title>") {
		t.Errorf("a page load with no credential got HTML:\n%s", body)
	}
}

// TestWS_RefusesWithoutTheCredential, over a real listener.
//
// A HANDSHAKE AND NOT A RECORDER, because a websocket upgrade hijacks the
// connection and a recorder has none to hijack -- so a refusal asserted
// against a recorder would be asserting about a code path a real client never
// reaches.
func TestWS_RefusesWithoutTheCredential(t *testing.T) {
	h := newHarness(t)
	_, wsURL := wsServer(t, h)

	ctx, cancel := contextWithTimeout(t, 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL+"/ws?id=abc", nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("an unauthenticated handshake succeeded")
	}
	if resp == nil {
		t.Fatalf("dial failed without a response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestNew_RefusesWithoutAToken.
//
// THE SEAL. An unauthenticated `ember serve` is not a configuration this
// program can be in: there is no flag that turns the credential off and no
// zero value that means "open". A loopback port is reachable by every uid on
// the machine, so the open server is the one where the account next door
// queues a paid rental -- and refusing to construct it is what makes that
// unrepresentable rather than merely documented.
func TestNew_RefusesWithoutAToken(t *testing.T) {
	_, err := New(Options{Store: genstore.NewRecordingStore(), Port: testPort})
	if err == nil {
		t.Fatal("a server with no credential was accepted")
	}
	// Naming the remediation, as every error here does.
	for _, want := range []string{"every uid", "NewToken", "serve_token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestNewToken_IsFreshAndCarriesAFingerprint.
//
// crypto/rand.Text is 128 bits in base32, which needs no encoding to sit in a
// URL or a cookie and cannot fail -- so there is no error return standing in
// for a branch nothing reaches. It is a node.Secret, so the ways key material
// usually escapes (a stray %v, an slog.Any) print a fingerprint.
func TestNewToken_IsFreshAndCarriesAFingerprint(t *testing.T) {
	a, b := NewToken(), NewToken()
	if a.IsZero() {
		t.Fatal("NewToken minted nothing")
	}
	if string(a.Bytes()) == string(b.Bytes()) {
		t.Fatal("two tokens are the same")
	}
	// URL-safe and cookie-safe without escaping, which is what lets the
	// exchange put one in a query string and read it back unchanged.
	for _, c := range string(a.Bytes()) {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", c) {
			t.Fatalf("the token carries %q, which a URL or a cookie would have to escape", c)
		}
	}
	if s := fmt.Sprintf("%v", a); strings.Contains(s, string(a.Bytes())) {
		t.Errorf("%%v printed the token: %s", s)
	}
	if !strings.HasPrefix(a.Fingerprint(), "SHA256:") {
		t.Errorf("fingerprint = %q", a.Fingerprint())
	}
}

// TestTokenURL_IsTheOnlyPlaceTheTokenIsPrinted.
//
// `ember serve` has to print the minted credential somewhere or nobody can
// use it; this is that one place, and the bare URL is what goes everywhere
// else.
func TestTokenURL_IsTheOnlyPlaceTheTokenIsPrinted(t *testing.T) {
	tok := NewToken()
	s, err := New(Options{Store: genstore.NewRecordingStore(), Port: testPort, Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.TokenURL(), string(tok.Bytes())) {
		t.Errorf("TokenURL carries no token: %s", s.TokenURL())
	}
	// 127.0.0.1 and not localhost: the cookie the exchange sets is scoped to
	// the host and not the port, so a UI reached by the name shares a cookie
	// namespace with every other local server on it.
	if !strings.HasPrefix(s.TokenURL(), "http://127.0.0.1:") {
		t.Errorf("TokenURL does not name the address: %s", s.TokenURL())
	}
	if strings.Contains(s.URL(), string(tok.Bytes())) {
		t.Errorf("URL carries the token: %s", s.URL())
	}
	if got := s.TokenFingerprint(); got != tok.Fingerprint() || strings.Contains(got, string(tok.Bytes())) {
		t.Errorf("fingerprint = %q", got)
	}
}

// TestBearer_ReadsWhatAClientActuallySends.
func TestBearer_ReadsWhatAClientActuallySends(t *testing.T) {
	for _, c := range []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER abc", "abc", true},
		{"Bearer   abc  ", "abc", true},
		{"", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Basic abc", "", false},
		{"abc", "", false},
	} {
		t.Run(fmt.Sprintf("%q", c.header), func(t *testing.T) {
			got, ok := bearer(c.header)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && string(got) != c.want {
				t.Errorf("token = %q, want %q", got, c.want)
			}
		})
	}
}

// TestTokenMatches_IsNotAPrefixCompare.
//
// Direct, because the HTTP tests above cannot distinguish a constant-time
// compare from `==` -- both refuse. What this asserts is the property a
// timing test cannot assert reliably in CI: that the comparison is over
// fixed-width digests, so neither a length nor a prefix is a special case in
// the code at all.
func TestTokenMatches_IsNotAPrefixCompare(t *testing.T) {
	tok := NewToken()
	s, err := New(Options{Store: genstore.NewRecordingStore(), Port: testPort, Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(tok.Bytes())

	if !s.tokenMatches([]byte(raw)) {
		t.Error("the right token did not match")
	}
	for i := 1; i < len(raw); i++ {
		if s.tokenMatches([]byte(raw[:i])) {
			t.Fatalf("a %d-byte prefix of the token matched", i)
		}
	}
	for _, bad := range []string{"", raw + "x", strings.ToLower(raw)} {
		if s.tokenMatches([]byte(bad)) {
			t.Errorf("%q matched", bad)
		}
	}
	// A server holding a token whose digest collides with nothing: the zero
	// Secret is refused by New, so there is no "empty token matches empty
	// credential" state to reach.
	if _, err := New(Options{Store: genstore.NewRecordingStore(), Token: node.Secret{}}); err == nil {
		t.Error("a zero token was accepted")
	}
}
