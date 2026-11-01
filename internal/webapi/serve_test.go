package webapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/genstore"
)

// freePort takes a port, releases it, and hands back the number.
//
// There is a race in principle -- something else could take it between the
// close and the bind -- and in practice nothing on a test machine is hunting
// for a just-released ephemeral port. The alternative is binding port 0 and
// reading the number back, which this server cannot do: the guard compares
// the Host header against its own port, so it has to know it before it
// listens.
func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return uint16(port)
}

// TestServe_AnswersOnBothLoopbackFamilies.
//
// A v4-only listener is invisible to a browser that resolves localhost to ::1
// first, and the operator reads connection-refused on a working server as a
// bug in ember. That rule has been in AGENTS.md since the first port forward;
// this is the one feature an actual browser opens, so it is the case it was
// written for.
func TestServe_AnswersOnBothLoopbackFamilies(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, func(o *Options) { o.Port = port })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()

	for _, host := range []string{
		fmt.Sprintf("127.0.0.1:%d", port),
		fmt.Sprintf("[::1]:%d", port),
	} {
		t.Run(host, func(t *testing.T) {
			var body string
			// The listener may not be up on the first attempt; the server is
			// starting in another goroutine.
			deadline := time.Now().Add(5 * time.Second)
			for {
				resp, err := http.Get("http://" + host + "/api/config") //nolint:noctx // a loopback probe in a test
				if err == nil {
					defer func() { _ = resp.Body.Close() }()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("status = %d", resp.StatusCode)
					}
					body = resp.Status
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s never answered: %v", host, err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if body == "" {
				t.Error("no answer")
			}
		})
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want nil after a cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
}

// TestServe_IsReachableFromNowhereElse.
//
// The guarantee the whole project is written around, applied to the one thing
// here that a browser opens: nothing ember serves is reachable from another
// machine. loopback.Listen is what enforces it and
// TestNoListenOutsideLoopback is what keeps anybody from going around it --
// including through http.ListenAndServe, which takes an address as a string
// and was added to that guard for this package.
func TestServe_IsReachableFromNowhereElse(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, func(o *Options) { o.Port = port })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = h.Serve(ctx) }()

	// Wait for it to be up.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Every non-loopback address this machine has. There should be no
	// listener on any of them.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		checked++
		target := net.JoinHostPort(ipnet.IP.String(), fmt.Sprint(port))
		c, derr := net.DialTimeout("tcp", target, 500*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			t.Errorf("%s answered: ember serve is published off this machine", target)
		}
	}
	if checked == 0 {
		// Not a failure: a machine with no non-loopback address is a machine
		// where there is nothing to check. Said out loud, because a silent
		// pass here would be a security assertion that ran against nothing.
		t.Log("this machine has no non-loopback address, so there was nothing to probe")
	}
}

// TestServe_SaysSoWhenThePortIsTaken, rather than failing with a bare
// address-in-use that reads like a bug in ember.
func TestServe_SaysSoWhenThePortIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	// Hold the v6 form as well, so neither family is available.
	if ln6, err6 := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port)); err6 == nil {
		defer func() { _ = ln6.Close() }()
	}

	h := newHarness(t, func(o *Options) { o.Port = port })
	err = h.Serve(t.Context())
	if err == nil {
		t.Fatal("Serve succeeded with the port already taken")
	}
	if !strings.Contains(err.Error(), "another ember serve") {
		t.Errorf("err = %v, want it to suggest the likely cause", err)
	}
}

// TestStatic_ServesABuiltUIFromDisk, which is the development loop: edit,
// rebuild the wasm, reload -- with no Go rebuild in between.
func TestStatic_ServesABuiltUIFromDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<!doctype html><title>built</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Everything only `make web` produces, which is how Built() tells a real
	// UI from the committed placeholder. BOTH files, because one of each is
	// not a build -- a test caught Built() and the page disagreeing about
	// exactly that.
	for _, name := range builtFiles {
		if name == "index.html" {
			continue // written above, with content this test asserts on
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("\x00asm"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	h := newHarness(t, func(o *Options) { o.Dist = dir })
	if !h.Built() {
		t.Error("Built() is false for a directory with a built UI in it")
	}
	w := h.do(t, "GET", "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "built") {
		t.Errorf("served the embedded copy instead of -dist:\n%s", w.Body.String())
	}
}

// TestStatic_FallsBackWhenDistIsNotThere. A mistyped -dist must not leave an
// operator with a server that 404s every page, which reads as "not running".
func TestStatic_FallsBackWhenDistIsNotThere(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Dist = filepath.Join(t.TempDir(), "nope") })
	w := h.do(t, "GET", "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the embedded copy", w.Code)
	}
	// The embedded copy, whichever it is -- which on a clean clone is the
	// placeholder and after `make web` is the app. Asserting one would make
	// this test depend on whether somebody had built the UI.
	if !strings.Contains(w.Body.String(), "<title>ember</title>") {
		t.Errorf("did not fall back to the built-in page:\n%s", w.Body.String())
	}
}

// TestListRequests_NamesWhatIsGeneratingNow, which is how a UI shows "yours is
// next" rather than nothing.
func TestListRequests_NamesWhatIsGeneratingNow(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	// No queue is wired, so nothing is generating and the field is absent --
	// which is the honest answer rather than a guess.
	got := decode[struct {
		Running string `json:"running"`
	}](t, h.do(t, "GET", "/api/requests", ""))
	if got.Running != "" {
		t.Errorf("running = %q with no queue wired", got.Running)
	}
	if _, err := h.store.Get(t.Context(), genstore.ReqID(id)); err != nil {
		t.Fatal(err)
	}
}
