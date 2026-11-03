//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/test/integration/harness"
)

// WHAT THESE LABS ARE FOR.
//
// `ember serve` is the first thing in this program a browser opens, and the
// first that answers a request somebody else's page can make. Two of the
// three properties below cannot be checked anywhere but here: whether a real
// socket is reachable from this machine's other addresses, and whether a real
// process that is killed takes its rental with it.
//
// They need no workload image, so the ordinary CI job runs them. The one that
// needs a generation server lives in serve_image_test.go, behind the imagelab
// tag, for the reason the preview lab is: CI builds only ember:lab, and a lab
// that requires an image CI does not build makes every pull request red.

// serveConfig is a configuration `ember serve` will start with: it needs an
// sd: block, because the serve argv names the weights and a request may not.
//
// The model path need not exist for the labs that never generate -- nothing
// starts a server until a request is queued.
const serveConfig = `
sd:
  serve: [sd-server, --model, /opt/models/sd15.gguf]
  jobs:
    - prompt: placeholder
`

// freePort takes one and gives it back.
//
// `ember serve` has to know its own port before it listens -- the Host check
// compares against it -- so binding :0 and reading the number back is not
// available.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// served is a running `ember serve`: where it listens and what it accepts.
//
// THE TOKEN IS HERE BECAUSE A LAB IS A CLIENT. `ember serve` authenticates
// every route, so a lab that only knew the port would be exactly the
// unauthenticated second process the credential exists to refuse -- which is
// what TestServe_RefusesEveryRouteWithoutTheToken asserts on purpose.
type served struct {
	Port int

	// Token is read out of the announcement rather than injected.
	//
	// NOT FROM EMBER_SERVE_TOKEN, deliberately: the minted path is the one
	// every operator gets by default, so it is the one worth exercising. A
	// lab that set the environment variable would never run the code that
	// mints, prints and parses a credential -- which is three of the four
	// things that can be wrong here.
	Token string
}

// startServe runs `ember serve` and waits until it has announced its URL.
func startServe(t *testing.T, l *harness.Lab, extra ...string) (*exec.Cmd, served, func() string) {
	t.Helper()
	port := freePort(t)
	args := append([]string{"serve", "--port", fmt.Sprint(port)}, extra...)

	cmd, wait := l.Start(t.Context(), args...)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// The printed URL is the signal that both halves are up. Waiting on a
	// dial instead would race the listener.
	l.WaitForLine(ctx, "Ctrl-C to stop")

	// And the credential, off the one line that carries it. The announcement
	// is already printed by the time the line above returns, so this does not
	// wait again.
	line := l.WaitForLine(ctx, "http://127.0.0.1:")
	_, tok, ok := strings.Cut(strings.TrimSpace(line), "?t=")
	if !ok || tok == "" {
		t.Fatalf("ember serve announced no token: %q", line)
	}
	return cmd, served{Port: port, Token: tok}, wait
}

// startServeOn runs `ember serve` on a port and with a token the caller chose.
//
// FOR THE ONE LAB THAT RESTARTS THE SERVER, and both halves of the signature
// are that lab's requirement rather than a convenience. The port has to be the
// same one the browser is already on, because a reconnect to a different port
// is not a reconnect. And the token has to survive, because a MINTED one does
// not: `ember serve` mints per run, so a restart leaves the browser holding a
// cookie for a credential that no longer exists and the resumed handshake is
// refused -- which is a real property of the program and the reason the
// announcement says "set serve_token in credentials.yaml for one that does not
// change". A lab about the reconnect must not be a lab about that.
//
// It reads the token from nowhere, because with a configured one there is
// nothing to read: a credential the operator chose is deliberately not
// printed, so `startServe`'s parse of `?t=` would come back with the
// announcement's own `<token>` placeholder.
func startServeOn(
	t *testing.T, l *harness.Lab, port int, token string, extra ...string,
) (*exec.Cmd, served, func() string) {
	t.Helper()
	// Inherited by the subprocess: harness.StartRaw builds its environment
	// from os.Environ(), and t.Setenv puts it back afterwards. The rule this
	// does not break is the one that matters -- no secret in argv -- which is
	// why there is no --token flag to pass instead.
	t.Setenv(config.EnvServeToken, token)

	args := append([]string{"serve", "--port", fmt.Sprint(port)}, extra...)
	cmd, wait := l.Start(t.Context(), args...)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	l.WaitForLine(ctx, "Ctrl-C to stop")
	return cmd, served{Port: port, Token: token}, wait
}

// answer is a finished response: read, closed, and still inspectable.
//
// The body is read here rather than handed back open. A helper that returns a
// live *http.Response and leaves a t.Cleanup to close it is the exact shape
// bodyclose exists to flag -- and the linter is right: a lab that forgets one
// leaks a connection per request, which on the SIGKILL labs is a connection
// per orphaned node.
type answer struct {
	Status int
	Body   []byte
	Header http.Header
}

// ask makes a request that looks like the UI's own: this server's Host, this
// server's Origin, and the credential it announced.
//
// The header carrier and not the cookie, because a Go client is not a
// browser. The cookie half is the chromedp lab's job -- whether Chrome
// attaches a SameSite=Strict cookie to a websocket handshake and to an <img>
// is a question for Chrome.
func ask(t *testing.T, method string, s served, path, body string) answer {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, path)
	req, err := http.NewRequestWithContext(t.Context(), method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", s.Port))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", method, path, err)
	}
	return answer{Status: resp.StatusCode, Body: b, Header: resp.Header}
}

// TestServe_IsReachableFromNowhereElse.
//
// THE GUARANTEE THE WHOLE PROJECT IS WRITTEN AROUND, applied to the one
// feature a browser opens. internal/loopback is what enforces it and
// TestNoListenOutsideLoopback is what stops anybody going around it --
// including through http.ListenAndServe, which takes an address as a string
// and was added to that guard for this package.
//
// A unit test can assert the handler's behaviour; only a real listener can
// answer "is there a socket on this machine's LAN address". The dial below is
// evidence rather than proof -- a firewall that DROPs looks like a closed
// port from here -- and it is still the only thing that can see a published
// port.
//
// It is dialled inline rather than through a type in internal/exposure, and
// that is now the only probe in the tree: exposure.Probe was deleted having
// never had a caller, because a probe needs an address and the ports it was
// written to find have none. See the note beside AssertNoPublishedPorts. This
// one has an address -- the listener is on this machine and the port is the
// one ember printed -- which is exactly the difference.
func TestServe_IsReachableFromNowhereElse(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	_, srv, _ := startServe(t, l)

	// Both loopback families answer. A v4-only listener is invisible to a
	// browser that resolves localhost to ::1 first, and the operator reads
	// connection-refused on a working server as a bug in ember.
	for _, host := range []string{
		fmt.Sprintf("127.0.0.1:%d", srv.Port),
		fmt.Sprintf("[::1]:%d", srv.Port),
	} {
		c, err := net.DialTimeout("tcp", host, 2*time.Second)
		if err != nil {
			t.Errorf("%s did not answer: %v", host, err)
			continue
		}
		_ = c.Close()
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	probed := 0
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		probed++
		target := net.JoinHostPort(ipnet.IP.String(), fmt.Sprint(srv.Port))
		c, derr := net.DialTimeout("tcp", target, 2*time.Second)
		if derr == nil {
			_ = c.Close()
			t.Errorf("%s answered: ember serve is published off this machine", target)
		}
	}
	if probed == 0 {
		// Not a failure, and said out loud: a silent pass here would be a
		// security assertion that ran against nothing.
		t.Log("this machine has no non-loopback address, so there was nothing to probe")
	}
}

// TestServe_RefusesAPageFromSomewhereElse.
//
// The browser threat, which the credential does not replace: any page the
// operator has open can POST to a loopback port while the browser hides only
// the response. All three checks or nothing, so this one has to keep holding
// on its own -- which is why the requests below CARRY THE TOKEN. A refusal
// test that could pass because the credential was missing proves nothing
// about the Origin and Host checks it is named after, and this is exactly how
// a seal gets quietly weakened: the test stays green while the thing it
// guards stops working.
func TestServe_RefusesAPageFromSomewhereElse(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	_, srv, _ := startServe(t, l)

	body := `{"provider":"local","jobs":[{"prompt":"a cat"}]}`

	for _, c := range []struct {
		name         string
		origin, host string
	}{
		{"another site", "https://evil.example", ""},
		{"a name that resolves here", "", "evil.example"},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := fmt.Sprintf("http://127.0.0.1:%d/api/requests", srv.Port)
			req, err := http.NewRequestWithContext(t.Context(), "POST", url, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+srv.Token)
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.host != "" {
				// The DNS rebinding case: the attacker's page is same-origin
				// with their own name, so Origin passes and Host does not.
				req.Host = fmt.Sprintf("%s:%d", c.host, srv.Port)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}

	// And nothing was queued by any of it.
	resp := ask(t, "GET", srv, "/api/requests", "")
	var list struct {
		Requests []json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Requests) != 0 {
		t.Errorf("%d request(s) were queued by a refused page", len(list.Requests))
	}
}

// TestServe_RefusesAPaidProviderWithoutTheFlag.
//
// --allow-paid off by default is what bounds the damage of there being no
// authentication: a cross-site POST to a default invocation can start a free
// local container and spend nothing.
func TestServe_RefusesAPaidProviderWithoutTheFlag(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	_, srv, _ := startServe(t, l)

	resp := ask(t, "POST", srv, "/api/requests",
		`{"provider":"runpod","jobs":[{"prompt":"a cat"}]}`)
	// 403 either way: without a credential RunPod is not registered at all,
	// and with one it is refused for being paid. Both are the right answer to
	// "rent me something that costs money" from a default serve.
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.Status)
	}
}

// TestServe_DestroysItsRentalOnSigint.
//
// THE MOST EXPENSIVE PROPERTY HERE. A server that exits with a node still
// rented bills by the hour with nobody watching, and the TTL is the only
// thing that eventually stops it.
//
// It is also the one that no unit test can reach: http.Server.Shutdown drains
// requests, and a rental does not live in a request -- it lives in the
// queue's goroutine. `ember serve` waits for both, and this is what proves
// it, with a real container and a real signal.
//
// The lab uses the node image rather than a workload one, so nothing
// generates: a node that comes up and is then signalled is enough to leak.
func TestServe_DestroysItsRentalOnSigint(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	cmd, srv, wait := startServe(t, l)

	resp := ask(t, "POST", srv, "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a cat","steps":1}]}`)
	if resp.Status != http.StatusAccepted {
		t.Fatalf("queueing failed: %d %s", resp.Status, resp.Body)
	}

	// WAIT FOR "node ready" AND NOT FOR A CONTAINER TO EXIST, and the
	// difference is a real window this lab found by getting it wrong.
	//
	// A container wears ember's label from the moment it is created, which is
	// before ContainerStart returns -- so signalling then lands *inside*
	// provision.Create, which has not returned a handle yet and whose
	// deferred destroy therefore does not exist. What is left is exactly the
	// orphan AGENTS.md describes: "the window between the request and reading
	// the response is where an orphan with no local record is born", which is
	// why the intent record is written first and why `ember ls` finds it.
	// TestServe_SigkillLeavesExactlyOneFinding is the lab for that case.
	//
	// The property *this* lab is about is different and stronger: a rental
	// that is up and running is destroyed when the process is asked to stop.
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	l.WaitForLine(ctx, "node ready")

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	out := wait()

	if nodes := l.Nodes(t.Context()); len(nodes) != 0 {
		t.Errorf("%d node(s) survived SIGINT; ember said:\n%s", len(nodes), out)
	}
	// And it said so, which is what tells an operator it is safe to walk
	// away.
	if !strings.Contains(out, "every rental has been destroyed") {
		t.Errorf("ember exited without reporting the teardown:\n%s", out)
	}
	// The cleanup's own stray report covers the network beside the container,
	// which is the thing a leak check forgot once and nobody noticed until
	// Docker had subnetted its whole address pool.
}

// TestServe_SigkillLeavesExactlyOneFinding is the other half, and the honest
// one: SIGKILL is the case no deferred cleanup can cover.
//
// The node survives, which is deliberate, and `ember ls` has to be able to
// find it -- which is what the journal's intent record and the holder exist
// for. The lab reaps it itself afterwards, so the stray check stays
// unconditional for every lab that leaks by mistake.
func TestServe_SigkillLeavesExactlyOneFinding(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	cmd, srv, _ := startServe(t, l)

	resp := ask(t, "POST", srv, "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a cat","steps":1}]}`)
	if resp.Status != http.StatusAccepted {
		t.Fatalf("queueing failed: %d %s", resp.Status, resp.Body)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	l.WaitForLine(ctx, "node ready")

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// ls exits 2 when anything needs attention, which is the contract with
	// cron: "there are orphaned nodes" and "ember itself broke" must not look
	// the same to a timer that only sees a status code.
	res := l.Run(t.Context(), "ls")
	if res.ExitCode != 2 {
		t.Errorf("ls exited %d after a killed serve, want 2. Output:\n%s", res.ExitCode, res.Combined())
	}

	// Deliberate, so it is declared: three labs here exist to leave a node
	// behind -- this one and the two in orphan_test.go -- and reaping it here
	// keeps the stray check unconditional for every lab that leaks by
	// mistake. No assertion is needed after it: the cleanup's snapshot is
	// taken once this body returns and before its own reap, so anything this
	// one failed to remove is reported against this lab.
	l.Reap()
}

// TestServe_RefusesEveryRouteWithoutTheToken.
//
// THE THREAT THE TOKEN IS FOR, reproduced exactly: a client on this machine
// that is not the operator's page. It addresses the server by its real Host,
// sends no Origin -- which is what curl does and what checkOrigin
// deliberately allows -- and so passes every check in guard.go. Before the
// credential it would have queued a generation; on `ember serve --allow-paid`
// it would have rented a GPU on somebody else's card.
//
// A LAB AND NOT ONLY A UNIT TEST, because the thing being asserted is that a
// real process on a real loopback socket refuses a real client. The unit
// tests in internal/webapi assert the same refusal against a handler, which
// cannot tell you whether the route table a listening ember built is the one
// that was tested.
//
// Every route, /ws included: a guard that covers the JSON routes and not the
// socket guards nothing, since the socket is how a watcher reads every event
// of every generation.
func TestServe_RefusesEveryRouteWithoutTheToken(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	_, srv, _ := startServe(t, l)

	body := `{"provider":"local","jobs":[{"prompt":"a cat"}]}`

	for _, c := range []struct {
		method, path, body string
	}{
		{"GET", "/", ""},
		{"GET", "/api/config", ""},
		{"GET", "/api/offers", ""},
		{"GET", "/api/nodes", ""},
		{"GET", "/api/requests", ""},
		// The one route that takes bytes inbound. Its body is JSON here and
		// the handler never reads it, because the credential is checked
		// before anything is read -- which is the property being asserted:
		// an ungated upload is a loopback port on which any other uid can
		// write files into the operator's state directory.
		{"POST", "/api/inputs", body},
		{"POST", "/api/requests", body},
		{"POST", "/api/requests/abc/cancel", ""},
		{"GET", "/api/requests/abc/events", ""},
		{"GET", "/api/requests/abc/images/0/0", ""},
		{"GET", "/ws?id=abc", ""},
	} {
		// Three credentials that must all be refused, and a token of the
		// wrong length is not among them on purpose: the compare is over
		// SHA-256 digests, so a short token, a long one and a correct prefix
		// are all simply wrong rather than three cases.
		for _, cred := range []struct{ name, header string }{
			{"nothing at all", ""},
			{"another token", "Bearer " + strings.Repeat("A", 26)},
			{"the right token truncated", "Bearer " + srv.Token[:len(srv.Token)-1]},
		} {
			t.Run(c.method+" "+c.path+" with "+cred.name, func(t *testing.T) {
				var r io.Reader
				if c.body != "" {
					r = strings.NewReader(c.body)
				}
				url := fmt.Sprintf("http://127.0.0.1:%d%s", srv.Port, c.path)
				req, err := http.NewRequestWithContext(t.Context(), c.method, url, r)
				if err != nil {
					t.Fatal(err)
				}
				// No Origin, exactly as a shell on this machine sends none.
				if c.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				if cred.header != "" {
					req.Header.Set("Authorization", cred.header)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				b, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("status = %d, want 403: %s", resp.StatusCode, b)
				}
				// And the response does not leak the credential it was
				// comparing against, which is what a helpful error message
				// does by accident.
				if strings.Contains(string(b), srv.Token) {
					t.Errorf("the refusal echoes the server's token:\n%s", b)
				}
			})
		}
	}

	// And nothing was queued by any of it. The browser hides the response of
	// a cross-origin fetch and a non-browser client does not even need that
	// hiding, so the side effect is the only thing worth asserting.
	resp := ask(t, "GET", srv, "/api/requests", "")
	var list struct {
		Requests []json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Requests) != 0 {
		t.Errorf("%d request(s) were queued by a request with no credential", len(list.Requests))
	}
}

// TestServe_ExchangesTheOneTimeURLForACookie.
//
// The browser's half of the credential, over a real listener, without a
// browser: the token in the query string comes back as a Set-Cookie and a
// redirect to a URL that no longer carries it. What a BROWSER then does with
// that cookie -- attaches it to a websocket handshake, to an <img src>, and
// not to a cross-site request -- is TestServe_UI's job, because those are
// claims about Chrome.
func TestServe_ExchangesTheOneTimeURLForACookie(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	_, srv, _ := startServe(t, l)

	// No redirect following: the 303 itself is what is being asserted.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", srv.Port, srv.Token)
	req, err := http.NewRequestWithContext(t.Context(), "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); strings.Contains(loc, srv.Token) {
		t.Errorf("the redirect keeps the token: %s", loc)
	}
	var jar *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "ember_serve" {
			jar = c
		}
	}
	if jar == nil {
		t.Fatalf("no credential cookie was set: %v", resp.Header.Values("Set-Cookie"))
	}
	if jar.Value != srv.Token {
		t.Errorf("cookie value = %q", jar.Value)
	}
	if !jar.HttpOnly || jar.SameSite != http.SameSiteStrictMode || jar.Path != "/" {
		t.Errorf("cookie attributes are wrong: %v", resp.Header.Values("Set-Cookie"))
	}

	// And the cookie alone is enough afterwards, with no header.
	req, err = http.NewRequestWithContext(t.Context(), "GET",
		fmt.Sprintf("http://127.0.0.1:%d/api/config", srv.Port), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(jar)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status = %d with the cookie alone, want 200", resp2.StatusCode)
	}
}

// TestServe_TheTokenIsNotInArgv.
//
// THE RULE THAT CHOSE WHERE THE CREDENTIAL LIVES, asserted against the thing
// it is a rule about: /proc/<pid>/cmdline is world-readable, so a token
// passed as a flag value is readable by every uid on this box for the life of
// the process -- which is precisely the uid the token exists to keep out. A
// credential in argv would be a control that publishes its own secret.
//
// Only a real process can answer this, which is why it is a lab. It is also
// why `ember serve` has no --token flag at all: the token comes from
// credentials.yaml, the environment, or crypto/rand.
func TestServe_TheTokenIsNotInArgv(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	cmd, srv, _ := startServe(t, l)

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid))
	if err != nil {
		t.Fatalf("reading the command line of the server under test: %v", err)
	}
	argv := strings.ReplaceAll(string(raw), "\x00", " ")
	if strings.Contains(argv, srv.Token) {
		t.Errorf("the token is in argv, where every uid on this machine can read it: %s", argv)
	}
	// The assertion is only worth anything if the token was findable at all,
	// so say what was looked for: a search for an empty string finds nothing
	// and would pass against a server with no credential.
	if srv.Token == "" {
		t.Fatal("no token was parsed out of the announcement, so this proved nothing")
	}
	if !strings.Contains(argv, "serve") {
		t.Errorf("that was not ember serve's command line: %s", argv)
	}
}
