package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

type payload struct {
	Name string `json:"name"`
}

// server returns a client pointed at h. httptest serves over loopback HTTP,
// which resolve allows precisely so this seam exists.
func server(t *testing.T, h http.HandlerFunc) (Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return Client{BaseURL: s.URL, Token: node.NewSecret([]byte("secret-token")), HTTP: s.Client()}, s
}

func TestDo_SendsAndDecodesJSON(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT, gotAccept, gotUA string
	var gotBody payload

	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		_ = decode(r, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"pong"}`))
	})

	var out payload
	if err := c.Post(t.Context(), "/v2/pods", payload{Name: "ping"}, &out); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v2/pods" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
	if gotBody.Name != "ping" {
		t.Errorf("body = %+v", gotBody)
	}
	if out.Name != "pong" {
		t.Errorf("out = %+v", out)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotCT != "application/json" || gotAccept != "application/json" {
		t.Errorf("content-type %q, accept %q", gotCT, gotAccept)
	}
	// Providers rate-limit and support-ticket by user agent, and the Go
	// default tells them nothing.
	if gotUA != "ember" {
		t.Errorf("user-agent = %q", gotUA)
	}
}

// TestDo_SendsTheCredentialInTheHeaderNeverTheURL. A query parameter is logged
// by every proxy on the path and lands in the provider's access logs.
func TestDo_SendsTheCredentialInTheHeaderNeverTheURL(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "secret-token") || strings.Contains(r.URL.Path, "secret-token") {
			t.Errorf("the token reached the URL: %s", r.URL)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	if err := c.Get(t.Context(), "/v2/pods", &payload{}); err != nil {
		t.Fatal(err)
	}
}

func TestDo_OmitsAuthorizationWithoutAToken(t *testing.T) {
	var got string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer s.Close()

	c := Client{BaseURL: s.URL, HTTP: s.Client()}
	if err := c.Get(t.Context(), "/x", &payload{}); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("sent an empty bearer: %q", got)
	}
}

func TestDo_UserAgentIsOverridable(t *testing.T) {
	var got string
	c, s := server(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{}`))
	})
	c.UserAgent = "ember/1.2.3"
	_ = s
	if err := c.Get(t.Context(), "/x", &payload{}); err != nil {
		t.Fatal(err)
	}
	if got != "ember/1.2.3" {
		t.Errorf("got %q", got)
	}
}

// TestStatusError_CarriesTheBody. A provider's 400 is the only thing that
// says which field ember got wrong; an error that drops it turns a
// five-second fix into an afternoon.
func TestStatusError_CarriesTheBody(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"ports: 8188/http is not permitted"}`))
	})

	err := c.Get(t.Context(), "/v2/pods", &payload{})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("got %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Errorf("code = %d", se.Code)
	}
	if !strings.Contains(err.Error(), "8188/http is not permitted") {
		t.Errorf("the body is missing from the error: %v", err)
	}
	if se.Retryable() {
		t.Error("a 400 is ember sending something wrong; retrying is just being wrong more expensively")
	}
}

func TestStatusError_Retryable(t *testing.T) {
	for code, want := range map[int]bool{
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		599:                            true,
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusConflict:            false,
	} {
		if got := (&StatusError{Code: code}).Retryable(); got != want {
			t.Errorf("%d: Retryable() = %v, want %v", code, got, want)
		}
	}
}

func TestStatusError_MessageAlwaysCarriesTheCode(t *testing.T) {
	got := (&StatusError{Method: "GET", URL: "https://api.example/v2/pods", Code: http.StatusNotFound}).Error()
	for _, want := range []string{"404", "Not Found", "/v2/pods", "GET"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q is missing %q", got, want)
		}
	}

	// An unassigned code has no StatusText, and the number is then the only
	// thing an operator can act on. Each part is asserted separately: the
	// earlier version of this test used && across two Contains checks, so it
	// passed on either half alone and hid a message that printed neither the
	// code nor a name.
	odd := (&StatusError{Method: "GET", URL: "https://api.example/x", Code: 499}).Error()
	if !strings.Contains(odd, "499") {
		t.Errorf("message %q does not carry the status code", odd)
	}
	if !strings.Contains(odd, "https://api.example/x") {
		t.Errorf("message %q does not carry the URL", odd)
	}
}

// TestDo_TruncatesAHugeErrorBody. A provider returning an HTML error page
// under load must not put a megabyte of markup into one log line.
func TestDo_TruncatesAHugeErrorBody(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", 200<<10)))
	})

	err := c.Get(t.Context(), "/x", &payload{})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("got %T", err)
	}
	if len(se.Body) > maxErrorBody {
		t.Errorf("body is %d bytes, want at most %d", len(se.Body), maxErrorBody)
	}
}

func TestDo_ReportsUndecodableJSONWithTheBodyItSaw(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json at all</html>`))
	})

	err := c.Get(t.Context(), "/x", &payload{})
	if err == nil {
		t.Fatal("HTML decoded as JSON")
	}
	// Without the body, "invalid character '<'" leaves the operator guessing
	// whether they hit the API or a captive portal.
	if !strings.Contains(err.Error(), "not json at all") {
		t.Errorf("the error does not show what came back: %v", err)
	}
}

func TestDo_ReportsAnEmptyBodyWhenAResultWasExpected(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	err := c.Get(t.Context(), "/x", &payload{})
	if err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("got %v", err)
	}
}

// TestDo_WithNoOutDrainsTheBody, so the connection returns to the pool. A
// close on an unread body makes the transport discard it, and a poll loop then
// opens a fresh TCP and TLS connection every second.
func TestDo_WithNoOutDrainsTheBody(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ignored":true}`))
	})
	if err := c.Do(t.Context(), http.MethodDelete, "/x", nil, nil); err != nil {
		t.Fatal(err)
	}
}

// TestDelete_NotFoundIsSuccess. The goal of destroy is that the machine is
// gone, and it is. Deciding this once here keeps every provider from
// reimplementing it slightly differently.
func TestDelete_NotFoundIsSuccess(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"pod not found"}`))
	})
	if err := c.Delete(t.Context(), "/v2/pods/gone"); err != nil {
		t.Fatalf("a pod that is already gone reported failure: %v", err)
	}
}

func TestDelete_OtherFailuresAreStillFailures(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if err := c.Delete(t.Context(), "/v2/pods/x"); err == nil {
		t.Fatal("a 500 from delete was reported as success")
	}
}

// TestResolve_RefusesAnAbsolutePath. A path that can name its own host would
// send ember's bearer token wherever it points.
func TestResolve_RefusesAnAbsolutePath(t *testing.T) {
	c := Client{BaseURL: "https://api.example"}
	for _, p := range []string{
		"https://evil.example/steal",
		"http://evil.example/steal",
		"gopher://evil.example/",
	} {
		if _, err := c.resolve(p); err == nil {
			t.Errorf("resolve(%q) was accepted", p)
		}
	}
}

// TestResolve_RefusesPlainHTTP, which would put the bearer token on the wire
// in clear. Loopback is exempt because that is the test seam.
func TestResolve_RefusesPlainHTTP(t *testing.T) {
	if _, err := (Client{BaseURL: "http://api.example"}).resolve("/x"); err == nil {
		t.Error("a plain-HTTP base URL was accepted")
	}
	for _, ok := range []string{
		"http://127.0.0.1:8080", "http://localhost:8080", "https://api.example",
	} {
		if _, err := (Client{BaseURL: ok}).resolve("/x"); err != nil {
			t.Errorf("resolve against %q: %v", ok, err)
		}
	}
}

func TestResolve_JoinsWithoutDoublingSlashes(t *testing.T) {
	for _, tc := range []struct{ base, path, want string }{
		{"https://api.example", "/v2/pods", "https://api.example/v2/pods"},
		{"https://api.example/", "/v2/pods", "https://api.example/v2/pods"},
		{"https://api.example/", "v2/pods", "https://api.example/v2/pods"},
		{"https://api.example", "v2/pods", "https://api.example/v2/pods"},
	} {
		got, err := (Client{BaseURL: tc.base}).resolve(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("resolve(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestResolve_RefusesAnEmptyOrUnparseableBaseURL(t *testing.T) {
	if _, err := (Client{}).resolve("/x"); err == nil {
		t.Error("an empty base URL was accepted")
	}
	if _, err := (Client{BaseURL: "https://exa mple"}).resolve("/x"); err == nil {
		t.Error("an unparseable base URL was accepted")
	}
}

func TestDo_ReportsAnUnencodableBody(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	// A channel cannot be marshalled, and the error must name the call rather
	// than surfacing as a bare json error with no context.
	err := c.Post(t.Context(), "/x", map[string]any{"ch": make(chan int)}, &payload{})
	if err == nil || !strings.Contains(err.Error(), "encode request body") {
		t.Fatalf("got %v", err)
	}
}

func TestDo_ReportsATransportFailure(t *testing.T) {
	// A server that is closed before the call: connection refused.
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := Client{BaseURL: s.URL, HTTP: s.Client()}
	s.Close()

	if err := c.Get(t.Context(), "/x", &payload{}); err == nil {
		t.Fatal("a refused connection reported success")
	}
}

func TestDo_HonoursContextCancellation(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := c.Get(ctx, "/x", &payload{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a deadline error", err)
	}
}

func TestDo_RefusesABadMethod(t *testing.T) {
	c := Client{BaseURL: "https://api.example"}
	if err := c.Do(t.Context(), "bad method", "/x", nil, nil); err == nil {
		t.Fatal("an invalid method was accepted")
	}
}

func TestClient_DefaultsATimeout(t *testing.T) {
	// There is deliberately no "no timeout" option: http.Client's zero value
	// has none, which is how a provision hangs until somebody notices.
	if got := (Client{}).client().Timeout; got != DefaultTimeout {
		t.Errorf("default timeout = %v, want %v", got, DefaultTimeout)
	}
	custom := &http.Client{Timeout: time.Second}
	if got := (Client{HTTP: custom}).client(); got != custom {
		t.Error("an injected client was replaced")
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("https://api.example/v2/pods?token=abc"); strings.Contains(got, "abc") {
		t.Errorf("got %q", got)
	}
	if got := redactURL("https://api.example/v2/pods"); got != "https://api.example/v2/pods" {
		t.Errorf("got %q", got)
	}
}

func TestHead(t *testing.T) {
	if got := head([]byte("short")); got != "short" {
		t.Errorf("got %q", got)
	}
	long := head([]byte(strings.Repeat("y", 500)))
	if !strings.HasSuffix(long, "...") || len(long) > 210 {
		t.Errorf("got %d bytes: %q", len(long), long)
	}
}

// decode reads a request body as JSON, for the handlers above.
func decode(r *http.Request, out any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(out)
}

// TestDo_ReportsABodyThatFailsMidRead. A provider connection dropped halfway
// through a response must not surface as a JSON decode error about truncated
// input: the operator needs to know the transfer failed, not that the API
// speaks malformed JSON.
func TestDo_ReportsABodyThatFailsMidRead(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
		// Promise more than is written, then stop. The client reads to the
		// declared length and gets an unexpected EOF.
		w.Header().Set("Content-Length", "4096")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"trunc`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if h, ok := w.(http.Hijacker); ok {
			conn, _, err := h.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	})

	err := c.Get(t.Context(), "/x", &payload{})
	if err == nil {
		t.Fatal("a truncated response decoded cleanly")
	}
	if !strings.Contains(err.Error(), "read response") {
		t.Errorf("the error blames decoding rather than the transfer: %v", err)
	}
}
