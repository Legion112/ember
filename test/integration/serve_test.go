//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

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

// startServe runs `ember serve` and waits until it has announced its URL.
func startServe(t *testing.T, l *harness.Lab, extra ...string) (*exec.Cmd, int, func() string) {
	t.Helper()
	port := freePort(t)
	args := append([]string{"serve", "--port", fmt.Sprint(port)}, extra...)

	cmd, wait := l.Start(t.Context(), args...)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// The printed URL is the signal that both halves are up. Waiting on a
	// dial instead would race the listener.
	l.WaitForLine(ctx, "Ctrl-C to stop")
	return cmd, port, wait
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

// ask makes a request that looks like the UI's own: this server's Host, and
// this server's Origin.
func ask(t *testing.T, method string, port int, path, body string) answer {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(t.Context(), method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port))
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
// answer "is there a socket on this machine's LAN address". The probe is
// evidence rather than proof -- a firewall that DROPs looks like a closed
// port from here -- which is the same caveat internal/exposure's probe
// carries, and it is still the only thing that can see a published port.
func TestServe_IsReachableFromNowhereElse(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	_, port, _ := startServe(t, l)

	// Both loopback families answer. A v4-only listener is invisible to a
	// browser that resolves localhost to ::1 first, and the operator reads
	// connection-refused on a working server as a bug in ember.
	for _, host := range []string{
		fmt.Sprintf("127.0.0.1:%d", port),
		fmt.Sprintf("[::1]:%d", port),
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
		target := net.JoinHostPort(ipnet.IP.String(), fmt.Sprint(port))
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
// There is no authentication, so this is what stands in for it. The threat is
// not an attacker on the network -- nothing is listening off this machine --
// it is any page the operator has open, which can POST to a loopback port
// while the browser hides only the response.
func TestServe_RefusesAPageFromSomewhereElse(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	_, port, _ := startServe(t, l)

	body := `{"provider":"local","jobs":[{"prompt":"a cat"}]}`

	for _, c := range []struct {
		name         string
		origin, host string
	}{
		{"another site", "https://evil.example", ""},
		{"a name that resolves here", "", "evil.example"},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := fmt.Sprintf("http://127.0.0.1:%d/api/requests", port)
			req, err := http.NewRequestWithContext(t.Context(), "POST", url, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.host != "" {
				// The DNS rebinding case: the attacker's page is same-origin
				// with their own name, so Origin passes and Host does not.
				req.Host = fmt.Sprintf("%s:%d", c.host, port)
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
	resp := ask(t, "GET", port, "/api/requests", "")
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
	_, port, _ := startServe(t, l)

	resp := ask(t, "POST", port, "/api/requests",
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
	cmd, port, wait := startServe(t, l)

	resp := ask(t, "POST", port, "/api/requests",
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
	// The cleanup's own AssertNoStray covers the network beside the
	// container, which is the thing a leak check forgot once and nobody
	// noticed until Docker had subnetted its whole address pool.
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
	cmd, port, _ := startServe(t, l)

	resp := ask(t, "POST", port, "/api/requests",
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

	// Deliberate, so it is declared: these two labs exist to leave a node
	// behind, and reaping it here keeps the stray check unconditional for
	// every lab that leaks by mistake.
	l.Reap()
}
