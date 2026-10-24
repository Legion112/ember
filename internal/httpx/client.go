// Package httpx is ember's HTTP client for provider APIs.
//
// It exists so that the three things that go wrong with a hand-rolled provider
// client go wrong in one place instead of three: a request with no timeout
// that hangs a provision forever, a response body that is never closed and
// eventually exhausts the connection pool mid-poll, and an error that says
// "unexpected status 400" without the body that explains which field was
// wrong.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// DefaultTimeout bounds a single request.
//
// Every call ember makes to a provider is a small control-plane operation --
// create a pod, read its status, destroy it -- so a minute is generous. There
// is no "no timeout" option: http.Client's zero value has none, which is how a
// provision ends up hanging until someone notices.
const DefaultTimeout = 60 * time.Second

// maxErrorBody bounds how much of a failure response is read into an error.
//
// Without a limit, a provider returning an HTML error page under load puts a
// megabyte of markup into a log line, and the one useful sentence is somewhere
// in the middle of it.
const maxErrorBody = 8 << 10

// Client is a provider API client.
//
// BaseURL plus *http.Client is the seam the provider tests point at an
// httptest.Server. A RoundTripper would be smaller but wrong: it bypasses the
// real client's timeout and its body-close behaviour, which are two of the
// three things this package exists to get right.
type Client struct {
	BaseURL string

	// Token is the bearer credential. It is a node.Secret so that a stray
	// slog.Any or %v prints a redaction rather than the key.
	Token node.Secret

	// HTTP defaults to a client with DefaultTimeout.
	HTTP *http.Client

	// UserAgent identifies ember to the provider. Providers rate-limit and
	// support-ticket by user agent, and "Go-http-client/2.0" tells them
	// nothing.
	UserAgent string
}

func (c Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

func (c Client) agent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return "ember"
}

// StatusError is a non-2xx response.
//
// It carries the body, truncated, because a provider's 400 is the only thing
// that will say which field ember got wrong, and an error that drops it turns
// a five-second fix into an afternoon.
type StatusError struct {
	Method string
	URL    string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	// The code is always printed, and the name only when net/http has one.
	// An earlier spelling put StatusText in the message and fell back when
	// the *whole formatted string* was empty -- which it never is -- so an
	// unassigned code such as 499 printed "GET <url>: " with no code at all.
	// The test that should have caught it used && where it needed ||, and so
	// passed on either half being present.
	s := fmt.Sprintf("%s %s: status %d", e.Method, e.URL, e.Code)
	if name := http.StatusText(e.Code); name != "" {
		s += " " + name
	}
	if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

// Retryable reports whether trying again could plausibly succeed.
//
// 429 and 5xx only. A 4xx is ember sending something wrong, and retrying it
// just means being wrong more expensively -- against a billing API, possibly
// much more expensively.
func (e *StatusError) Retryable() bool {
	return e.Code == http.StatusTooManyRequests || (e.Code >= 500 && e.Code <= 599)
}

// Do sends a request with a JSON body and decodes a JSON response.
//
// body may be nil for GET and DELETE. out may be nil when the response is not
// needed, in which case the body is still drained and closed so the connection
// can be reused.
func (c Client) Do(ctx context.Context, method, path string, body, out any) error {
	u, err := c.resolve(path)
	if err != nil {
		return err
	}

	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body for %s %s: %w", method, u, err)
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return fmt.Errorf("build request %s %s: %w", method, u, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent())
	if tok := string(c.Token.Bytes()); tok != "" {
		// The header, never the URL: a query parameter is logged by every
		// proxy on the path and ends up in the provider's access logs.
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		// url.Error embeds the full URL, which for a provider API is harmless,
		// but it also embeds the method and the wrapped cause, so it is kept.
		return fmt.Errorf("%s %s: %w", method, redactURL(u), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &StatusError{
			Method: method, URL: redactURL(u), Code: resp.StatusCode,
			Body: strings.TrimSpace(string(raw)),
		}
	}

	if out == nil {
		// Drained before close so the connection goes back to the pool. A
		// close on an unread body makes the transport discard the connection,
		// and a poll loop then opens a fresh TCP+TLS connection every second.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response from %s %s: %w", method, redactURL(u), err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%s %s returned %d with an empty body, but a result was expected", method, redactURL(u), resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w; body began %q", method, redactURL(u), err, head(raw))
	}
	return nil
}

// resolve joins path onto BaseURL, refusing anything that would send a
// request somewhere other than the configured API.
//
// A path that parses as an absolute URL would silently retarget the request --
// including ember's bearer token -- at whatever host it names. That is worth a
// hard error even though every path in this repository is a literal, because
// the next one might not be.
func (c Client) resolve(path string) (string, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return "", errors.New("httpx: no base URL configured for this provider")
	}
	if strings.Contains(path, "://") {
		return "", fmt.Errorf("httpx: %q is an absolute URL, not a path; a path that can name its own host would send ember's credentials wherever it points", path)
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return "", fmt.Errorf("httpx: base URL %q does not parse: %w", c.BaseURL, err)
	}
	if base.Scheme != "https" && base.Hostname() != "127.0.0.1" && base.Hostname() != "localhost" && base.Hostname() != "::1" {
		// Plain HTTP would put the bearer token on the wire in clear. Loopback
		// is allowed so the provider tests can point at an httptest.Server,
		// which is the whole reason BaseURL is injectable.
		return "", fmt.Errorf("httpx: base URL %q is not https; ember will not send a credential in clear", c.BaseURL)
	}
	return strings.TrimSuffix(base.String(), "/") + "/" + strings.TrimPrefix(path, "/"), nil
}

// Get is Do with no body.
func (c Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

// Post sends body and decodes out.
func (c Client) Post(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, http.MethodPost, path, body, out)
}

// Delete removes a resource, treating 404 as success.
//
// Not-found is success because the only caller is a provider's Destroy, whose
// goal is that the machine is gone -- and it is. Deciding that here keeps
// every provider from reimplementing it and getting it subtly different.
func (c Client) Delete(ctx context.Context, path string) error {
	err := c.Do(ctx, http.MethodDelete, path, nil, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil
	}
	return err
}

// redactURL strips any query string before the URL reaches an error message.
//
// Nothing in ember puts a credential in a query parameter -- resolve and the
// Authorization header see to that -- but errors are the most-copied text in
// any incident, and a redaction here costs nothing.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i] + "?<redacted>"
	}
	return u
}

func head(b []byte) string {
	const n = 200
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
