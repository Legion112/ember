//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/webapi"
)

// UNIX-GATED FOR THE FEATURE'S REASON rather than the test's:
// loopback.ListenSocket is //go:build unix and refuses on every other
// platform, because a socket there carries no mode and a channel that
// describes itself as authenticated by the operating system with nothing
// setting the mode is the "reads as working" defect on a security control.
// There is nothing to assert about a client for a channel that cannot exist.

// shortHome is a home directory with a SHORT path, and the length is the
// point.
//
// t.TempDir() embeds the test's own name, and the socket under it is
// <home>/state/socket/serve.sock -- which on this kernel has to fit in the 107
// characters bind(2) accepts. A test named for what it asserts would push past
// that and StateSocket would refuse, so the client would report no daemon over
// a daemon that is running and every assertion here would pass for the wrong
// reason. Measured rather than assumed: loopback.maxSocketPath is 107 because
// 108 answers `bind: invalid argument`.
func shortHome(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	if err := os.MkdirAll(filepath.Join(d, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// fakeServe is an `ember serve` as the client can see it: a unix socket at the
// path loopback decides, answering the three routes `ember up` uses.
//
// IT LISTENS THROUGH loopback.ListenSocket, which is the production call, so
// what this exercises is the real path and the real mode checks rather than a
// socket a test placed by hand. The path is StateSocket's, for the reason the
// socket lab reads it out of the announcement: a client that spelled
// <state>/socket/serve.sock for itself would be the journal's "nodes"
// subdirectory, and it would look in the wrong place the day either component
// changed.
type fakeServe struct {
	mu     sync.Mutex
	bodies []webapi.RunBody

	// id is what the 202 names, status what GET /api/requests/{id} answers,
	// and reason what it puts in `error`.
	id     string
	status string
	reason string

	// refuse, when set, is answered instead of the 202, with this status and
	// this sentence -- which is what the client has to print in full.
	refuseWith int
	refusal    string

	// events is handed out from a cursor, as the real route does.
	events []report.Event
}

func (f *fakeServe) asks() []webapi.RunBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]webapi.RunBody(nil), f.bodies...)
}

func (f *fakeServe) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+webapi.RunPath, func(w http.ResponseWriter, r *http.Request) {
		var b webapi.RunBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, b)
		refuse, refusal := f.refuseWith, f.refusal
		id := f.id
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if refuse != 0 {
			w.WriteHeader(refuse)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": refusal})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(webapi.RunAccepted{ID: id})
	})
	mux.HandleFunc("GET /api/requests/{id}", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		st, reason := f.status, f.reason
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": st, "error": reason, "node": "local/n1",
		})
	})
	mux.HandleFunc("GET /api/requests/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		after := int64(0)
		_, _ = fmt.Sscanf(r.URL.Query().Get("after"), "%d", &after)
		f.mu.Lock()
		all := f.events
		f.mu.Unlock()
		var out []report.Event
		for _, e := range all {
			if e.Seq > after {
				out = append(out, e)
			}
		}
		next := after
		if n := len(out); n > 0 {
			next = out[n-1].Seq
		}
		if out == nil {
			out = []report.Event{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"next": next, "events": out})
	})
	return mux
}

// start binds the socket under home and serves until the test ends.
func (f *fakeServe) start(t *testing.T, home string) loopback.Socket {
	t.Helper()
	if f.id == "" {
		f.id = "req_fake"
	}
	if f.status == "" {
		f.status = "done"
	}
	sock, err := loopback.StateSocket(filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := loopback.ListenSocket(t.Context(), sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return sock
}

// created counts the container creates the Docker fake saw, which is how a
// test tells the submitted path from the direct one.
//
// THE ABSENCE OF A RENTAL IS THE ASSERTION, not the presence of an HTTP call:
// a client that posted the run AND then rented locally would satisfy an
// assertion about the post, and would be two rentals -- which is the whole
// thing this change is careful about.
func created(d deps) int {
	n := 0
	for _, c := range d.Engine.(*dockerengine.RecordingEngine).Calls {
		if strings.HasPrefix(c, "ContainerCreate") {
			n++
		}
	}
	return n
}

func upConfig(t *testing.T, home string) string {
	t.Helper()
	p := filepath.Join(home, "ember.yaml")
	body := "provider: local\nimage: ghcr.io/example/wl:v1\nttl: 10m\ncmd: [echo, hi]\n" +
		"config_dir: " + filepath.Join(home, "config") + "\n" +
		"state_dir: " + filepath.Join(home, "state") + "\n" +
		"output_dir: " + filepath.Join(home, "out") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUp_SubmitsToARunningEmberServeInsteadOfRenting.
//
// The whole of slice 3 in one assertion pair: the run reaches the daemon, and
// THIS process rents nothing. The second half is what makes it a change rather
// than an addition -- a client that posted and then rented would be two GPUs,
// which is the failure the one-rental invariant exists for, reached from the
// client side.
func TestUp_SubmitsToARunningEmberServeInsteadOfRenting(t *testing.T) {
	home := shortHome(t)
	f := &fakeServe{id: "req_abc", status: "done", events: []report.Event{
		{Seq: 1, Kind: report.KindLog, Text: "running: echo hi\n"},
		{Seq: 2, Kind: report.KindLog, Text: "hi\n"},
	}}
	f.start(t, home)

	d := labDeps(t)
	d.HomeDir = home
	cfg := upConfig(t, home)

	var out bytes.Buffer
	if err := runUp([]string{"--config", cfg, "--gpu", "rtx4090", "--ttl", "30m"}, &out, d); err != nil {
		t.Fatalf("runUp over the socket: %v\n%s", err, out.String())
	}

	if n := created(d); n != 0 {
		t.Errorf("the client rented %d container(s) as well as submitting the run; that is the "+
			"second rental this whole shape is built to make unrepresentable", n)
	}
	asks := f.asks()
	if len(asks) != 1 {
		t.Fatalf("the daemon was asked %d time(s), want 1", len(asks))
	}
	// AN ABSOLUTE PATH, because `ember serve` runs from its own working
	// directory: a relative one would name a different file, and that file
	// sets the ceilings, the image and the store.
	if !filepath.IsAbs(asks[0].Config) {
		t.Errorf("the config path travelled as %q, which the daemon would resolve against its "+
			"own cwd", asks[0].Config)
	}
	if asks[0].GPU != "rtx4090" || asks[0].TTL != "30m0s" {
		t.Errorf("the overrides did not reach the daemon: %+v", asks[0])
	}
	// The node's own output is rendered, which is what makes the submitted
	// path's terminal the same terminal.
	if !strings.Contains(out.String(), "running: echo hi\nhi\n") {
		t.Errorf("the run's own output did not reach this terminal:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "submitted to the ember serve on") {
		t.Errorf("the client did not say where it submitted the run:\n%s", out.String())
	}
}

// TestUp_RunsDirectlyWhenNoDaemonIsListening.
//
// NOT A REFUSAL, which is the decision #146's own comment argued the other way
// for -- correctly, for a NEW verb. `ember up` exists, every measurement in
// README was taken with it, and a version that refused unless a server
// happened to be up would break all of them for nothing. The engine's
// ContainerCreate is the evidence the direct path ran rather than the absence
// of an error, because an error can come from anywhere.
func TestUp_RunsDirectlyWhenNoDaemonIsListening(t *testing.T) {
	home := shortHome(t)
	d := labDeps(t)
	d.HomeDir = home
	e := d.Engine.(*dockerengine.RecordingEngine)
	e.AddImage("ghcr.io/example/wl:v1")
	e.FailOn = "ContainerCreate"
	cfg := upConfig(t, home)

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg}, &out, d)
	if err == nil {
		t.Fatal("the create was supposed to fail")
	}
	if n := created(d); n == 0 {
		t.Errorf("no container was created, so `ember up` did not fall back to renting here: "+
			"%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "submitted to the ember serve") {
		t.Errorf("the client claimed to submit a run with no daemon listening:\n%s", out.String())
	}
}

// TestUp_ADryRunNeverAsksADaemon.
//
// It rents nothing and prints what a rental would be, so routing it through a
// daemon would make the cheapest thing in ember depend on a process being up
// -- and it would need the route to answer with a plan rather than a request
// id, which is a second shape for no gain.
func TestUp_ADryRunNeverAsksADaemon(t *testing.T) {
	home := shortHome(t)
	f := &fakeServe{}
	f.start(t, home)

	d := labDeps(t)
	d.HomeDir = home
	d.Engine.(*dockerengine.RecordingEngine).AddImage("ghcr.io/example/wl:v1")
	cfg := upConfig(t, home)

	var out bytes.Buffer
	if err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatalf("dry run: %v\n%s", err, out.String())
	}
	if n := len(f.asks()); n != 0 {
		t.Errorf("a dry run asked the daemon %d time(s)", n)
	}
	if !strings.Contains(out.String(), "dry-run: nothing was created") {
		t.Errorf("the dry run did not print its own answer:\n%s", out.String())
	}
}

// TestUp_CarriesTheDaemonsRefusalInFull.
//
// Every refusal on this path is a refusal `ember up` would have made itself --
// an unpinned image, a card with no image, a store missing the weights, a
// provider that is not free -- and those messages carry the remediation.
// Reporting a status code instead would throw away the only part an operator
// can act on, which is what the cancel button did for the life of #108.
func TestUp_CarriesTheDaemonsRefusalInFull(t *testing.T) {
	home := shortHome(t)
	const sentence = "provider runpod is not free and ember serve was started without --allow-paid"
	f := &fakeServe{refuseWith: http.StatusBadRequest, refusal: sentence}
	f.start(t, home)

	d := labDeps(t)
	d.HomeDir = home
	cfg := upConfig(t, home)

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg}, &out, d)
	if err == nil {
		t.Fatal("a refused submission was reported as success")
	}
	if err.Error() != sentence {
		t.Errorf("the daemon's sentence did not reach the operator in full: %v", err)
	}
	if n := created(d); n != 0 {
		t.Errorf("a refused submission rented %d container(s) anyway", n)
	}
}

// TestFollowRun_ARunThatFailedIsThisCommandsExitStatus.
//
// The row's own reason is the sentence the run wrote into requests.err, which
// is what `ember history` prints and what the page shows. A cancelled run is
// an error too, because that is what the direct path does: Plan.Do hands back
// the cancelled context and `ember up` returns it, so a run stopped by its
// deadline exits non-zero on both paths.
func TestFollowRun_ARunThatFailedIsThisCommandsExitStatus(t *testing.T) {
	cases := []struct {
		status, reason string
		wantErr        bool
		want           string
	}{
		{"done", "", false, ""},
		{"failed", "the card aborted on the first kernel launch", true, "the card aborted"},
		{"cancelled", "job 3/8: cannot be stopped", true, "cannot be stopped"},
		{"interrupted", "", true, "interrupted"},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			home := shortHome(t)
			f := &fakeServe{status: c.status, reason: c.reason}
			sock := f.start(t, home)

			var out bytes.Buffer
			err := upOverSocket(context.Background(), &out, sock,
				app.RunAsk{Config: filepath.Join(home, "ember.yaml")})
			switch {
			case c.wantErr && err == nil:
				t.Fatalf("a %s run was reported as success", c.status)
			case !c.wantErr && err != nil:
				t.Fatalf("a %s run was reported as %v", c.status, err)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("the outcome should name %q, got %v", c.want, err)
			}
		})
	}
}

// TestFollowRun_CtrlCDetachesAndSaysWhatThatCosts.
//
// THE RESILIENCE BEING ASKED FOR, AND THE EXPOSURE IT CREATES. On the direct
// path Ctrl-C destroys the node, on a context that survives the cancellation,
// so the guarantee was "no leak when you walk away". Here the run carries on
// in the daemon -- it never held anything this process owns -- and what stops
// it is the TTL, which is mandatory and enforced in three places including
// the node's own timer. So the default ttl IS the bill, and that is a thing to
// be told at the moment of detaching rather than to discover from an invoice.
//
// EXIT 0, because detaching is what the operator asked for: a non-zero status
// would say something failed.
func TestFollowRun_CtrlCDetachesAndSaysWhatThatCosts(t *testing.T) {
	home := shortHome(t)
	deadline := time.Now().Add(42 * time.Minute).UTC().Truncate(time.Second)
	f := &fakeServe{status: "running", events: []report.Event{
		{Seq: 1, Kind: report.KindNodeReady, Node: "local/n1", Deadline: deadline},
	}}
	sock := f.start(t, home)

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- upOverSocket(ctx, &out, sock, app.RunAsk{Config: filepath.Join(home, "ember.yaml")})
	}()
	// Long enough for the first poll to have rendered the node_ready, which
	// is where the deadline comes from. The assertion does not depend on the
	// timing: a detach with no node reported yet prints the other sentence,
	// and that is asserted below by what it must NOT say.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("detaching reported an error, which would mean something failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client did not return after its context was cancelled")
	}

	// THE NOTICE AND NOT THE WHOLE OUTPUT, which is what the first version of
	// this got wrong. clientSays prints "it destroys itself at <deadline>" the
	// moment the node is ready, so asserting that substring over everything
	// the client wrote passed against a detach notice with the deadline taken
	// out of it -- a marker matching a second, unpatched line, which is one of
	// the three false GREENs AGENTS.md records. Everything asserted below is
	// read from the span after "detached from".
	_, notice, ok := strings.Cut(out.String(), "detached from ")
	if !ok {
		t.Fatalf("the client printed no detach notice at all:\n%s", out.String())
	}
	for _, want := range []string{
		"THE RUN IS STILL GOING",
		"it destroys itself at",
		deadline.Format(time.RFC3339),
		"whether or not ember is running",
		"ember ls",
		"ember down local/n1",
		"ember history",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("the detach notice does not say %q:\n%s", want, notice)
		}
	}
}

// TestDetached_NamesTheTtlWhenNoNodeHasBeenReported.
//
// The other half of the notice, and it invents nothing: a client that detached
// before a node_ready reached it has no deadline, and a deadline is the one
// number here that must not be guessed. Asserted because the two sentences are
// a pair -- a lab that interrupted early would read this one while believing
// it had read the other, which is exactly what the first run of the browser
// lab did.
func TestDetached_NamesTheTtlWhenNoNodeHasBeenReported(t *testing.T) {
	var out bytes.Buffer
	detached(&out, "req_abc", "", time.Time{})
	got := out.String()
	if strings.Contains(got, "it destroys itself at") {
		t.Errorf("the notice named a deadline nothing reported:\n%s", got)
	}
	for _, want := range []string{
		"THE RUN IS STILL GOING",
		"no node has been reported yet",
		"ember down <node>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the notice does not say %q:\n%s", want, got)
		}
	}
}

// TestFollowRun_RendersTheRunThroughTheSameRendererAsEmberUp.
//
// BYTE-FOR-BYTE, which is the claim the whole carrier choice rests on. The
// events are the ones the run emitted, report.Text is the one function in the
// program where a progress line is spelled, and a KindLog event's text is
// written verbatim with its terminator -- so bytes from the node reach this
// terminal unchanged through a socket and a database, exactly as they reach
// the direct path's terminal through an io.Writer. A second renderer here
// would be a second place a progress line is spelled, which `ember up`'s
// output being a contract makes expensive.
func TestFollowRun_RendersTheRunThroughTheSameRendererAsEmberUp(t *testing.T) {
	events := []report.Event{
		{Seq: 1, Kind: report.KindServerStarting, Cmd: "sd-server --model /m.gguf"},
		{Seq: 2, Kind: report.KindLog, Text: "loading weights\r"},
		{Seq: 3, Kind: report.KindLog, Text: "loaded\n"},
		{Seq: 4, Kind: report.KindUnitStarted, Unit: report.UnitJob, Index: 1, Total: 1, Desc: "a paper boat"},
		{Seq: 5, Kind: report.KindJobProgress, Unit: report.UnitJob, Index: 1, Total: 1, Pass: 1, Step: 3, TotalSteps: 4},
	}

	var want bytes.Buffer
	text := report.Text(&want)
	for _, e := range events {
		text.Emit(e)
	}

	home := shortHome(t)
	f := &fakeServe{status: "done", events: events}
	sock := f.start(t, home)

	var got bytes.Buffer
	if err := upOverSocket(context.Background(), &got, sock,
		app.RunAsk{Config: filepath.Join(home, "ember.yaml")}); err != nil {
		t.Fatal(err)
	}
	// The one line the client adds is its own, so it is trimmed rather than
	// asserted away: everything after it has to be report.Text's output.
	_, rendered, ok := strings.Cut(got.String(), "\n")
	if !ok {
		t.Fatalf("the client printed nothing but %q", got.String())
	}
	if rendered != want.String() {
		t.Errorf("the client rendered the run differently from `ember up`:\n got %q\nwant %q",
			rendered, want.String())
	}
}

// TestServeSocket_SaysThereIsNoDaemonRatherThanFailing.
//
// Two ways there is none, and neither is an error: nothing listening, which is
// the ordinary case, and a state directory so long that bind(2) would refuse
// the socket -- which is a state directory no `ember serve` can have started
// under either, because it resolves the same path through the same
// constructor. The second is silent on purpose: printing it would put a line
// about a channel the operator did not ask for on every `ember up` in a
// deeply nested checkout, and `ember up`'s output is a contract.
func TestServeSocket_SaysThereIsNoDaemonRatherThanFailing(t *testing.T) {
	if _, ok := serveSocket(configPaths{State: filepath.Join(shortHome(t), "state")}); ok {
		t.Error("serveSocket found a daemon with nothing listening")
	}
	long := filepath.Join(shortHome(t), strings.Repeat("d", 120), "state")
	if _, ok := serveSocket(configPaths{State: long}); ok {
		t.Error("serveSocket accepted a path bind(2) would refuse")
	}
}

// TestClientSays_PrintsTheThreeEventsReportTextIsSilentAbout.
//
// report.Text renders node_ready, run_cost and destroy_failed as NOTHING, and
// its own comment gives the reason: "the CLI already reports all three through
// its slog handler". That is true of every caller it had and false of this
// one -- a submitted run's slog handler is the DAEMON's, writing to a terminal
// nobody is watching for them. So these three are the client's own lines, and
// each of them is a fact an operator cannot be left without: the deadline is
// the whole of what stops the bill after a detach, the cost is what the run
// was for, and a destroy that failed means a machine that is still running.
func TestClientSays_PrintsTheThreeEventsReportTextIsSilentAbout(t *testing.T) {
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	cases := []struct {
		name  string
		event report.Event
		want  []string
	}{
		{
			name: "node_ready",
			event: report.Event{Kind: report.KindNodeReady, Node: "local/n1",
				Endpoint: "172.17.0.2:22", Deadline: deadline},
			want: []string{"rented local/n1", "172.17.0.2:22", "it destroys itself at",
				deadline.Format(time.RFC3339)},
		},
		{
			// A node with no deadline reported is the honest half: the line
			// says what it knows and invents nothing, because a deadline is
			// the one number here that must not be guessed.
			name:  "node_ready with nothing but a handle",
			event: report.Event{Kind: report.KindNodeReady, Node: "local/n2"},
			want:  []string{"rented local/n2"},
		},
		{
			name: "run_cost",
			event: report.Event{Kind: report.KindRunCost, Node: "local/n1",
				Cost: 11333, Elapsed: 90 * time.Second, Billable: 2 * time.Minute},
			want: []string{"the rental cost", "1m30s", "2m0s"},
		},
		{
			name:  "destroy_failed",
			event: report.Event{Kind: report.KindDestroyFailed, Node: "local/n1", Err: "no such container"},
			want:  []string{"DESTROY FAILED", "local/n1", "no such container", "MAY STILL BE RUNNING AND BILLING"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			clientSays(&out, c.event)
			for _, w := range c.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("the line does not say %q: %q", w, out.String())
				}
			}
		})
	}

	// AND EVERY OTHER KIND IS SILENT HERE, which is what keeps the rendered
	// run byte-for-byte what `ember up` prints: a second spelling of a line
	// report.Text already owns would be two accounts of one fact.
	for _, k := range report.Kinds() {
		switch k {
		case report.KindNodeReady, report.KindRunCost, report.KindDestroyFailed:
			continue
		}
		var out bytes.Buffer
		clientSays(&out, report.Event{Kind: k, Text: "x\n", Node: "local/n1"})
		if out.Len() != 0 {
			t.Errorf("clientSays printed %q for %s, which report.Text already renders", out.String(), k)
		}
	}
}
