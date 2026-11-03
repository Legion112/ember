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
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// WHAT THIS LAB IS FOR, AND WHY IT CANNOT BE A UNIT TEST.
//
// internal/webapi's own socket tests drive a real listener and prove the
// handler's decision: a request that arrived on the socket needs no token, a
// request on the port still does, and no header can claim the channel. What
// they cannot answer is whether the `ember serve` an operator starts makes a
// socket at all, where, at what mode, in a directory at what mode, announced
// how, and whether it is still there afterwards -- which are properties of a
// real process and a real filesystem.
//
// THE CLAIM THIS CHANNEL IS WORTH, AND THE PART OF IT NO SINGLE-UID TEST RUN
// CAN REACH. AGENTS.md says a unix socket at 0600 "answers the every-uid case
// with no secret at all". The mechanism has two halves: the socket's mode, and
// the directory's. Both are asserted below against what a real `ember serve`
// created. What is NOT asserted anywhere in this repository is a connect
// attempt from a second account, because a test run is one uid and making a
// second one needs root -- and a lab that needed root would be a lab nobody
// runs. So what stands in its place is measured rather than argued: connect(2)
// on a unix socket requires WRITE permission on the node (a socket chmod-ed to
// 0000 refuses its own owner, measured on this kernel), and the socket is 0600
// inside a directory at 0700 -- so an account that is neither the owner has
// neither the write bit on the socket nor the search bit on the directory it
// is in. The gap is stated rather than papered over: that is a deduction from
// two measured facts and not a third measurement.

// serveSocketPath reads the socket out of the announcement rather than
// rebuilding it.
//
// ONE NAME FOR A PATH, NOT ONE PER CALL SITE. The journal's "nodes"
// subdirectory was a literal in the CLI and a second literal in this harness;
// they drifted, the harness read the wrong directory, and a lab asserting "a
// clean run leaves no records" passed without checking anything. A lab that
// spelled <state>/socket/serve.sock for itself would pass the day
// loopback.StateSocket changed either component -- against a socket nothing
// was listening on.
func serveSocketPath(t *testing.T, l *harness.Lab) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	line := l.WaitForLine(ctx, "A unix socket at ")
	_, rest, ok := strings.Cut(line, "A unix socket at ")
	if !ok {
		t.Fatalf("ember serve announced no socket: %q", line)
	}
	path, _, ok := strings.Cut(rest, " ")
	if !ok || path == "" {
		t.Fatalf("could not read a path out of %q", line)
	}
	return path
}

// socketClient talks to a running ember over its unix socket.
//
// NO Authorization HEADER ANYWHERE IN THIS FILE, which is the whole point:
// every request below carries no credential of any kind, and the ones that
// succeed succeed because connect(2) already answered who was asking.
func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

// askSocket sends one request over the socket and reads what came back.
//
// The Host is spelled by the caller because it is part of what is under test:
// `curl --unix-socket` sends `Host: localhost`, with no port, and the TCP
// guard compares a port.
func askSocket(t *testing.T, c *http.Client, method, host, origin, path, body string) answer {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+host+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s over the socket: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{Status: resp.StatusCode, Body: b, Header: resp.Header}
}

// TestServe_SocketIsAuthenticatedByTheOperatingSystem.
//
// The whole of the channel, against a real process:
//
//   - the socket is where the announcement says, at 0600, in a directory at
//     0700 -- which together are the authentication;
//   - every route answers over it with no credential at all;
//   - the loopback port on the SAME process still refuses one, which is the
//     control that stops this lab passing against a server that gave up
//     asking;
//   - the Origin check still runs on the socket and the Host check does not;
//   - a clean exit leaves no socket behind.
func TestServe_SocketIsAuthenticatedByTheOperatingSystem(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	cmd, srv, wait := startServe(t, l)

	path := serveSocketPath(t, l)

	// IT IS UNDER THE STATE DIRECTORY AND NOT THE OUTPUTS. A socket is not
	// something a run produced, and `ember history prune` has no business
	// near it.
	if want := filepath.Join(l.StateDir(), "socket", "serve.sock"); path != want {
		t.Errorf("the announced socket is %q, want %q", path, want)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("ember announced a socket that is not there: %v", err)
	}
	if fi.Mode().Type()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket: %v", path, fi.Mode())
	}
	// THE ASSERTION THE CHANNEL RESTS ON. bind(2) takes no mode and creates
	// the node at 0777 &^ umask -- 0775 under the umask on this machine --
	// and connect(2) needs write permission, so anything wider than 0600 is
	// an account next door that can queue a rental.
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("the socket is mode %04o, want 0600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("the socket directory is mode %04o, want 0700: another account must not be able "+
			"to traverse to the socket or to replace it", got)
	}

	c := socketClient(path)

	// A read, with no credential and the Host curl --unix-socket sends.
	if a := askSocket(t, c, "GET", "localhost", "", "/api/config", ""); a.Status != http.StatusOK {
		t.Errorf("GET /api/config over the socket = %d, want 200: %s", a.Status, a.Body)
	}
	if a := askSocket(t, c, "GET", "localhost", "", "/api/requests", ""); a.Status != http.StatusOK {
		t.Errorf("GET /api/requests over the socket = %d, want 200: %s", a.Status, a.Body)
	}

	// AND THE TWO ROUTES THAT CHANGE SOMETHING, refused by the handler for
	// what they say rather than by the credential for who said it -- which is
	// the distinction that matters, and a 400 is how it is visible. Nothing
	// here is enqueued on purpose: this lab is about the channel, and a lab
	// that rented would be measuring provisioning.
	for _, c2 := range []struct {
		name, method, path, body string
	}{
		{"a request with no jobs", "POST", "/api/requests", `{"provider":"local","jobs":[]}`},
		{"bytes that are not an image", "POST", "/api/inputs", `not a png`},
	} {
		a := askSocket(t, c, c2.method, "localhost", "", c2.path, c2.body)
		if a.Status == http.StatusForbidden {
			t.Errorf("%s over the socket was refused as unauthenticated: %s", c2.name, a.Body)
		}
		if a.Status != http.StatusBadRequest {
			t.Errorf("%s over the socket = %d, want 400: %s", c2.name, a.Status, a.Body)
		}
	}

	// THE CONTROL, ON THE SAME RUNNING PROCESS. A refusal asserted in another
	// lab could pass against a server that has no socket at all, and the
	// direction that costs money is a port that stopped asking -- so the
	// uncredentialled request that the socket just answered is made again on
	// the port and has to be refused.
	url := fmt.Sprintf("http://127.0.0.1:%d/api/config", srv.Port)
	req, err := http.NewRequestWithContext(t.Context(), "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("the loopback port answered an uncredentialled GET with %d, want 403", resp.StatusCode)
	}
	// And with the token it answers, so the refusal above is about the
	// credential and not about a server that broke.
	if a := ask(t, "GET", srv, "/api/config", ""); a.Status != http.StatusOK {
		t.Fatalf("the credentialled control on the port failed: %d %s", a.Status, a.Body)
	}

	// THE TWO HEADER CHECKS, WHICH ARE A DECISION RATHER THAN A CONSEQUENCE.
	// checkHost is a statement about a TCP port and a socket request has
	// none, so it does not run; checkOrigin is about who composed the
	// request, so it does -- a proxy an operator puts in front of this socket
	// is the one thing that can put a page's request on it.
	if a := askSocket(t, c, "GET", "evil.example", "", "/api/config", ""); a.Status != http.StatusOK {
		t.Errorf("a Host naming no port of this server's was refused over the socket: %d %s", a.Status, a.Body)
	}
	a := askSocket(t, c, "POST", "localhost", "https://evil.example", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a cat","steps":1}]}`)
	if a.Status != http.StatusForbidden {
		t.Errorf("a page's Origin over the socket = %d, want 403: %s", a.Status, a.Body)
	}
	// And it queued nothing, which is the side effect the refusal exists for.
	listing := askSocket(t, c, "GET", "localhost", "", "/api/requests", "")
	var list struct {
		Requests []json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(listing.Body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Requests) != 0 {
		t.Errorf("%d request(s) were queued over the socket by a refused page", len(list.Requests))
	}

	// A CLEAN EXIT LEAVES NO SOCKET, which is what makes a restart work:
	// net unlinks a socket its own Listen created, and http.Server.Shutdown
	// is what closes the listener. Only a process killed outright leaves a
	// path behind, and ListenSocket is what clears that one.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	out := wait()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket survived a clean exit (%v); ember said:\n%s", err, out)
	}
}

// TestServe_SocketRefusesASecondServerRatherThanTakingIt.
//
// TWO REAL PROCESSES, WHICH IS WHY THIS IS A LAB. A second `ember serve` on a
// different port shares the state directory, so it shares the socket -- and
// unlinking a live one would leave the first server accepting on a path
// nothing can reach, which is a running server an operator cannot talk to and
// a rental they cannot see. The refusal is checked on the second process's
// exit code and on what it said, because an operator reading "address already
// in use" learns nothing about which ember to stop.
func TestServe_SocketRefusesASecondServerRatherThanTakingIt(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	_, srv, _ := startServe(t, l)
	path := serveSocketPath(t, l)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	res := l.Run(ctx, "serve", "--port", fmt.Sprint(freePort(t)))
	if res.ExitCode == 0 {
		t.Fatalf("a second ember serve started beside the first:\n%s", res.Combined())
	}
	if !strings.Contains(res.Combined(), "already running") {
		t.Errorf("the refusal does not say what is wrong:\n%s", res.Combined())
	}

	// AND THE FIRST ONE IS UNHARMED, which is the property rather than the
	// exit code. Without this the lab would pass against a second server that
	// unlinked the socket and then failed for some other reason.
	c := socketClient(path)
	if a := askSocket(t, c, "GET", "localhost", "", "/api/config", ""); a.Status != http.StatusOK {
		t.Errorf("the first server's socket stopped answering: %d %s", a.Status, a.Body)
	}
	if a := ask(t, "GET", srv, "/api/config", ""); a.Status != http.StatusOK {
		t.Errorf("the first server's port stopped answering: %d %s", a.Status, a.Body)
	}
}

// TestServe_SocketPathIsNotInArgv.
//
// NOT A SECRET, AND THAT IS THE POINT OF ASSERTING IT. The path is not a
// credential -- the mode is -- so this is not the rule
// TestServe_TheTokenIsNotInArgv asserts. What it asserts is the seal:
// loopback.StateSocket is the only thing that can name a socket path, so
// there is no flag that names one, and a `--socket` an operator could type
// would be a path outside the directory whose mode ember checked. A flag
// would also be the obvious next change, which is why its absence is an exit
// code rather than a sentence.
func TestServe_SocketPathIsNotInArgv(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(serveConfig)
	cmd, _, _ := startServe(t, l)
	path := serveSocketPath(t, l)

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid))
	if err != nil {
		t.Fatalf("reading the command line of the server under test: %v", err)
	}
	argv := strings.ReplaceAll(string(raw), "\x00", " ")
	if strings.Contains(argv, path) {
		t.Errorf("the socket path is in argv, so something named it rather than loopback: %s", argv)
	}
	// The search is only worth anything if the path was findable at all.
	if path == "" {
		t.Fatal("no socket path was parsed out of the announcement, so this proved nothing")
	}
	if !strings.Contains(argv, "serve") {
		t.Errorf("that was not ember serve's command line: %s", argv)
	}
	// And the flag an operator would reach for does not exist, which is what
	// stops the path becoming an argument.
	res := l.Run(t.Context(), "serve", "--socket", path)
	if res.ExitCode == 0 {
		t.Errorf("ember serve accepted a --socket flag:\n%s", res.Combined())
	}
}
