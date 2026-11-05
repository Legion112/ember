//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/webapi"
	"github.com/Legion112/ember/test/integration/harness"
)

// WHAT THESE LABS ARE FOR, AND WHY THE UNIT TESTS CANNOT BE THEM.
//
// internal/queue's direct_test.go proves the invariant that matters -- a
// submitted run waits for the drain loop, the maximum number of open rentals
// is one, and the run executes on the loop's own goroutine -- against fakes.
// internal/cli's upclient_test.go proves the client talks to a socket, renders
// through report.Text and detaches, against a fake server. Neither of them can
// answer the question #146 is actually about: whether a real `ember up` finds a
// real `ember serve`, whether the run it submits is rented by THAT process, and
// whether Ctrl-C on the client leaves the run going.
//
// A SHORT TEST NAME IS LOAD-BEARING HERE. harness.New uses t.TempDir(), which
// embeds the test's own name, and the socket under it is
// <state>/socket/serve.sock -- which has to fit in the 107 characters bind(2)
// accepts on this kernel. A lab named for everything it asserts would push
// past that, StateSocket would refuse, and the client would report no daemon
// over a daemon that is running: every assertion below would then pass against
// the direct path, which is the "it reads as working" failure this repository
// keeps recording. So the names are short and the reason is here.

// upRunConfig is the sd: block `ember serve` refuses to start without.
const upRunConfig = `
sd:
  serve: [sd-server, --model, /opt/models/sd15.gguf]
  jobs:
    - prompt: placeholder
`

// slowProbe is a rental long enough to be interrupted while it is still
// going.
//
// NOT A TEST HOOK: `gpu_probe` is the config key an operator writes for a card
// that needs time to come up, doing exactly what it says, on a node that has
// no card. What is under test is the handoff and the detach, and a run that
// needed a model would need ember:sd and twenty seconds of sampling to assert
// a property that has nothing to do with either.
const slowProbe = `
gpu_probe: [sh, -c, "sleep 12"]
gpu_probe_timeout: 2m
`

// upConfigFile writes the config the CLIENT names, and naming one is the
// point.
//
// TWO CONFIGS, AND THE SECOND IS THE REVERSAL BEING EXERCISED. `ember serve`
// needs an sd: block or it refuses to start, and node.NewSpec refuses a spec
// that has both `cmd` and `sd.serve` -- so a submitted `cmd` run cannot use
// the daemon's own file. It uses this one, which is exactly what app.RunAsk
// documents: the config path travels, the daemon reads the file the operator
// named, and the ceilings in it are the ones that bound the run. On the
// loopback port there is no field for it and never will be.
//
// The directories are the lab's own, so the socket this client looks for is
// the socket the daemon bound and the history both write is one file.
func upConfigFile(t *testing.T, l *harness.Lab) string {
	t.Helper()
	p := filepath.Join(l.Dir, "up.yaml")
	body := strings.Join([]string{
		"provider: local",
		"image: " + harness.LabImage,
		"ttl: 10m",
		"config_dir: " + filepath.Join(l.Dir, "config"),
		"state_dir: " + l.StateDir(),
		"output_dir: " + l.OutputDir(),
		slowProbe,
	}, "\n")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// submittedID reads the request id out of the client's own first line.
//
// Off the printed line rather than out of the database, because the line is
// what an operator has: a client that submitted a run and could not say which
// one would leave them nothing to pass to `ember history` or `ember down`.
func submittedID(t *testing.T, l *harness.Lab) genstore.ReqID {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	line := l.WaitForLine(ctx, "submitted to the ember serve on ")
	_, rest, ok := strings.Cut(strings.TrimSpace(line), " as ")
	if !ok || rest == "" {
		t.Fatalf("the client did not name the request it submitted: %q", line)
	}
	return genstore.ReqID(rest)
}

// socketEvents reads a request's event stream over the socket, with no
// credential, the way the client does.
func socketEvents(t *testing.T, path string, id genstore.ReqID) []report.Event {
	t.Helper()
	got := askSocket(t, socketClient(path), http.MethodGet, "localhost", "",
		fmt.Sprintf("/api/requests/%s/events?after=0", id), "")
	if got.Status != http.StatusOK {
		t.Fatalf("reading events over the socket: %d %s", got.Status, got.Body)
	}
	var page struct {
		Events []report.Event `json:"events"`
	}
	if err := json.Unmarshal(got.Body, &page); err != nil {
		t.Fatalf("the event page is not JSON: %v\n%s", err, got.Body)
	}
	return page.Events
}

// TestServe_UpOverTheSocket is the whole of slices 2 and 3 against real
// processes.
//
//   - `ember up` finds the running `ember serve` and submits the run to it;
//   - Ctrl-C on the client DETACHES -- it exits 0, says the run is still
//     billing and names the deadline -- and the daemon finishes the run;
//   - exactly ONE run happened, which is what rules out a client that
//     submitted and then rented here as well;
//   - the node's own output reached the event stream, which is how a detached
//     operator can still read what their rental said;
//   - the privileged route is not on the loopback port of the same process,
//     with that process's own token.
func TestServe_UpOverTheSocket(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(upRunConfig)
	_, srv, _ := startServe(t, l)
	sockPath := serveSocketPath(t, l)

	// THE CLIENT NAMES ITS OWN CONFIG, which is the reversal: the daemon reads
	// the file the operator pointed at rather than the one it was started
	// with. runRaw because Lab.Start inserts the lab's own --config, and the
	// whole point here is a different one.
	client, clientOut := l.StartRaw(t.Context(), "up",
		"--config", upConfigFile(t, l))
	id := submittedID(t, l)

	// INTERRUPTED WHILE THE RUN IS STILL GOING, which is the property. The
	// wait is for the CLIENT to have seen the node -- not for a container to
	// exist -- because the two are a second apart and the assertion below is
	// about what the client can say: a client killed before the node_ready
	// event reached it correctly prints "no node has been reported yet", and
	// a lab that interrupted there would be asserting the weaker of the two
	// sentences while believing it asserted the other.
	ready, cancelReady := context.WithTimeout(t.Context(), 120*time.Second)
	l.WaitForLine(ready, "it destroys itself at")
	cancelReady()
	if err := client.Process.Signal(interrupt()); err != nil {
		t.Fatalf("signal the client: %v", err)
	}
	out := clientOut()

	if !strings.Contains(out, "THE RUN IS STILL GOING") {
		t.Errorf("the client detached without saying the node is still billing:\n%s", out)
	}
	if !strings.Contains(out, "it destroys itself at") {
		t.Errorf("the client did not name the deadline, which is now the whole of what stops "+
			"the bill:\n%s", out)
	}
	if !strings.Contains(out, "ember ls") {
		t.Errorf("the detach notice does not point at the command that needs no daemon:\n%s", out)
	}
	if code := client.ProcessState.ExitCode(); code != 0 {
		t.Errorf("the client exited %d; detaching is what the operator asked for, so a non-zero "+
			"status would say something failed:\n%s", code, out)
	}

	// THE RUN CARRIED ON IN THE DAEMON. The sleep is twelve seconds and the
	// client went away during it, so a run tied to the client's context would
	// end here as cancelled with no output.
	var done genstore.Entry
	deadline := time.Now().Add(4 * time.Minute)
	for {
		rows := history(t, l)
		if len(rows) > 1 {
			t.Fatalf("%d runs were recorded for one `ember up`, so the client rented as well as "+
				"submitting: %+v", len(rows), rows)
		}
		if len(rows) == 1 && rows[0].Status.Terminal() {
			done = rows[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the submitted run never finished in the daemon: %+v", rows)
		}
		time.Sleep(time.Second)
	}
	if done.ID != id {
		t.Errorf("the daemon recorded %s and the client was told %s", done.ID, id)
	}
	if done.Status != genstore.StatusDone {
		t.Errorf("the detached run ended as %q, want done: a run whose lifetime was the "+
			"client's would be cancelled here", done.Status)
	}
	if done.Node == "" {
		t.Error("the finished run names no node, so nothing joins it to what `ember ls` reported")
	}

	// AND THE NODE'S OWN BYTES REACHED THE STREAM, which is the carrier a
	// detached operator has left. On the direct path they go to the terminal
	// the client owns; here that terminal is gone by now, so report.Writer is
	// what puts them somewhere durable.
	var logged string
	for _, e := range socketEvents(t, sockPath, id) {
		if e.Kind == report.KindLog {
			logged += e.Text
		}
	}
	if !strings.Contains(logged, "the-run-finished") {
		t.Errorf("the workload's own output is in no event, so a detached run's log is lost:\n%q",
			logged)
	}

	// THE PRIVILEGED ROUTE IS NOT ON THE PORT, asked with this process's own
	// Host, Origin and token -- which is to say it passes every check in
	// guard.go and auth.go, and what stops it is that no handler is
	// registered. A page, a forwarded `ssh -L` and a second account holding
	// the token all reach the same place.
	body := fmt.Sprintf(`{"config":%q,"cmd":["echo","nope"]}`, l.ConfigPath)
	onPort := ask(t, http.MethodPost, srv, webapi.RunPath, body)
	if onPort.Status == http.StatusAccepted {
		t.Errorf("the loopback port accepted a run: %d %s", onPort.Status, onPort.Body)
	}
	if rows := history(t, l); len(rows) != 1 {
		t.Errorf("a POST to the loopback port left %d row(s) in the history, want the one "+
			"submitted over the socket", len(rows))
	}

	l.WaitNoNodes(t.Context(), 60*time.Second, func() string { return out })
}

// TestServe_UpWithNoDaemon is the fallback, as an exit code rather than as a
// sentence in a document.
//
// REFUSING WOULD HAVE BEEN THE OTHER ANSWER and #146's own comment argued for
// it -- correctly, for a new verb. `ember up` exists and every measurement in
// README was taken with it, so a version that refused unless a server happened
// to be up would break all of those for nothing. The evidence is a node this
// process created: an exit code alone cannot tell "it ran here" from "it ran
// somewhere".
func TestServe_UpWithNoDaemon(t *testing.T) {
	l := harness.New(t)

	// A PROBE THAT FAILS ONCE AND THEN SUCCEEDS, because the assertion below
	// is about a line whose content came from the node. gpucheck buffers a
	// probe's own output and prints it only if the card never comes up, so a
	// probe that passes first time reaches the terminal with nothing; one
	// that needed a second attempt makes ember say so, on the io.Writer, with
	// a figure it could only have got by running something there.
	l.AppendConfig("gpu_probe: [sh, -c, \"test -f /tmp/ember-probed || " +
		"{ touch /tmp/ember-probed; exit 1; }\"]\ngpu_probe_timeout: 2m\n")

	res := l.Run(t.Context(), "up")
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}
	if strings.Contains(res.Combined(), "submitted to the ember serve") {
		t.Errorf("the client claimed to submit a run with no daemon listening:\n%s", res.Combined())
	}
	// The node's facts on this path go straight to the terminal, through an
	// io.Writer and not through an event -- which is the difference the two
	// paths have and the reason report.Writer is only on the other one.
	if !strings.Contains(res.Combined(), "the GPU became usable after") {
		t.Errorf("nothing the node answered reached the terminal:\n%s", res.Combined())
	}
	if n := len(l.Nodes(t.Context())); n != 0 {
		t.Errorf("%d node(s) survived a clean direct run", n)
	}
	if rows := history(t, l); len(rows) != 1 {
		t.Errorf("a direct run recorded %d row(s), want 1: the recording is the same on both "+
			"paths and goes through one queue.AdoptRun", len(rows))
	}
}
