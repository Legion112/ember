package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/webapi"
)

// servedConfigFile writes a config with an sd.serve block.
func servedConfigFile(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), config.FileName)
	body := "provider: local\nimage: ghcr.io/example/sd:v1\nttl: 30m\n" +
		"sd:\n  serve: [sd-server, --model, /m.gguf]\n  jobs:\n    - prompt: a\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestServe_RefusesWhatItCannotRun.
//
// Every one of these happens before a listener is opened, a database is
// created or a provider is asked -- so a mistake in the invocation costs
// nothing. The one that matters most is the missing sd.serve block: without
// it a queued request has nowhere to run, and finding that out after a card
// has been rented is finding it after the billing started.
func TestServe_RefusesWhatItCannotRun(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"a bad flag", []string{"--nonsense"}, "flag provided but not defined"},
		{"an argument", []string{servedArg(t), "extra"}, "takes no arguments"},
		{"port zero", []string{servedArg(t), "--port", "0"}, "outside 1..65535"},
		{"a port above the range", []string{servedArg(t), "--port", "70000"}, "outside 1..65535"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runServe(c.args, &out, labDeps(t))
			if err == nil {
				t.Fatal("accepted an invocation it cannot run")
			}
			if ExitCode(err) != ExitUsage {
				t.Errorf("exit = %d, want %d", ExitCode(err), ExitUsage)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func servedArg(t *testing.T) string {
	t.Helper()
	return "--config=" + servedConfigFile(t, "")
}

// TestServe_RefusesAConfigWithNoGenerationServer, at startup rather than
// after a request has been queued against it.
func TestServe_RefusesAConfigWithNoGenerationServer(t *testing.T) {
	p := filepath.Join(t.TempDir(), config.FileName)
	if err := os.WriteFile(p, []byte("provider: local\nimage: x:v1\nttl: 30m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runServe([]string{"--config", p}, &out, labDeps(t))
	if err == nil {
		t.Fatal("accepted a config with no sd.serve block")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitUsage)
	}
	// It names the example that costs nothing, because the operator's next
	// move is to copy one.
	if !strings.Contains(err.Error(), "sd-server.example.yaml") {
		t.Errorf("err = %v, want it to name a configuration to copy", err)
	}
}

func TestServe_ReportsAMalformedConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), config.FileName)
	if err := os.WriteFile(p, []byte("provider: [not a string\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runServe([]string{"--config", p}, &out, labDeps(t)); err == nil {
		t.Fatal("a malformed config was ignored")
	}
}

func TestServe_HelpSaysThereIsNoAuthentication(t *testing.T) {
	var out bytes.Buffer
	// -h is flag.ErrHelp, which this package reports as a usage error. What
	// is being asserted is the text, as every other help test here does.
	_ = runServe([]string{"-h"}, &out, labDeps(t))
	got := out.String()
	for _, want := range []string{
		// The one thing a person has to decide here is whether to leave this
		// running, so the help says what is not guarded as plainly as what
		// is.
		"NO AUTHENTICATION",
		"--allow-paid",
		"loopback only",
		"narrow the ceilings",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the help does not mention %q:\n%s", want, got)
		}
	}
}

// TestAnnounce_SaysWhatIsNotGuarded.
//
// This is the text an operator reads before deciding whether to walk away
// from the process, so it has to say that any page in their browser can reach
// the port, and whether this server is allowed to spend money.
func TestAnnounce_SaysWhatIsNotGuarded(t *testing.T) {
	srv, err := webapi.New(webapi.Options{Store: genstore.NewRecordingStore(), Port: 7777})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{TTL: 30 * time.Minute, MaxHourlyUSD: "2.49", MaxRunUSD: "3.00"}

	var refused bytes.Buffer
	announce(&refused, srv, cfg, false, "", "/tmp/out")
	got := refused.String()
	for _, want := range []string{
		"http://127.0.0.1:7777/",
		"Paid providers are refused",
		"--allow-paid",
		"no authentication",
		"/tmp/out",
		"destroyed before this exits",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the announcement does not mention %q:\n%s", want, got)
		}
	}

	// "run make web" appears exactly when there is no UI built in, and the
	// assertion is the RELATIONSHIP rather than either state -- because both
	// are correct and which one holds depends on whether the person running
	// the tests has built the frontend. A test that assumed one passed on CI
	// and failed on a laptop, which is the same "behaves differently from one
	// machine to the next" problem the injected Docker engine exists for.
	if srv.Built() == strings.Contains(got, "make web") {
		t.Errorf("Built() is %v but the announcement %s mention building it:\n%s",
			srv.Built(), map[bool]string{true: "does", false: "does not"}[strings.Contains(got, "make web")], got)
	}

	var allowed bytes.Buffer
	announce(&allowed, srv, cfg, true, "", "/tmp/out")
	got = allowed.String()
	for _, want := range []string{
		"PAID PROVIDERS ARE ENABLED",
		// The ceilings, so what it is allowed to spend is on screen rather
		// than in a file the operator has to go and read.
		"30m0s", "$2.49/hr", "$3",
		"narrow those and never widen",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the announcement does not mention %q:\n%s", want, got)
		}
	}
}

// TestServeUntil_StartsBothHalvesAndStopsBoth.
//
// The property worth the most here: a process that exits with a GPU still
// rented bills by the hour with nobody watching. serveUntil waits for the
// queue's goroutine as well as the HTTP server, because a rental does not
// live in a request -- http.Server.Shutdown drains requests and would say
// nothing about a node.
//
// Nothing is rented in this test, so what is asserted is that both halves
// came up, the announcement was printed, the database was created, and the
// function returned on a cancellation rather than hanging.
func TestServeUntil_StartsBothHalvesAndStopsBoth(t *testing.T) {
	home := t.TempDir()
	d := labDeps(t)
	d.HomeDir = home

	cfg, err := config.Load(servedConfigFile(t, ""))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	// Guarded on this side too: serveUntil writes from its own goroutines
	// while this one polls, and a bytes.Buffer read and written at once is a
	// data race -- which -race reports as a failure in the test rather than
	// where it is.
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- serveUntil(ctx, out, d, cfg, serveOptions{Port: freeServePort(t)})
	}()

	// The announcement is printed before either half is waited on, so its
	// arrival is how a test knows the wiring got that far.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "Ctrl-C to stop") {
		if time.Now().After(deadline) {
			t.Fatalf("serveUntil never announced itself:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntil = %v, want nil after a cancellation", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serveUntil did not return after its context ended")
	}

	// The line that tells an operator it is safe to walk away.
	if !strings.Contains(out.String(), "every rental has been destroyed") {
		t.Errorf("the shutdown said nothing about the rentals:\n%s", out.String())
	}
	// And the database it created, which is where the queue and the history
	// live between runs.
	if _, err := os.Stat(filepath.Join(home, "state", genstore.FileName)); err != nil {
		t.Errorf("no database was created: %v", err)
	}
}

// TestServeUntil_ReportsADirectoryItCannotUse, rather than starting a server
// whose every write will fail.
func TestServeUntil_ReportsADirectoryItCannotUse(t *testing.T) {
	// A regular file where the state directory has to go.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := labDeps(t)
	d.HomeDir = home

	cfg, err := config.Load(servedConfigFile(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	if err := serveUntil(t.Context(), out, d, cfg, serveOptions{Port: freeServePort(t)}); err == nil {
		t.Error("serveUntil started with an unusable state directory")
	}
}

// freeServePort keeps these tests off the default port, so running them does
// not collide with an ember serve the operator left running.
func freeServePort(t *testing.T) uint16 {
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

// syncBuf is a bytes.Buffer safe to read while another goroutine writes.
//
// test/integration/harness has the same thing for the same reason: os/exec
// writes a subprocess's two streams from two goroutines, and here serveUntil
// writes from two of its own.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
