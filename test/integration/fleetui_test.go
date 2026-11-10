//go:build integration && imagelab

package integration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/test/integration/harness"
)

// THE FLEET IN A BROWSER, AND THE ONE STATE ONLY A REAL NODE CAN PRODUCE.
//
// Everything about the projection is held by `go test ./internal/...` and
// everything about the four states is held by a table in internal/queue. What
// neither can reach is the join between two programs over a wire: a worker
// that is really running, really beating every five seconds, and really stops
// -- while its container stays up, its TCP connection stays open and its card
// keeps billing. That is the middle row of the heartbeat's own table, it is
// the reason the beat exists instead of a port check, and it is produced here
// by freezing a node rather than by setting a timestamp.
//
// AND THE SECOND CLASS, which is this directory's own history: a keyed list
// that re-renders only when its key changes. This repository has found that
// four times in `web/` -- the progress row froze while the step advanced in
// the database, the price cell would not take a cost written after the status
// was terminal. A worker row keyed on its node id, whose heartbeat moves
// underneath it, is exactly that shape a fifth time. The fleet table is
// written with no `<For>` and no key, so it cannot be; this is what proves the
// rendering actually changes rather than that the JSON did.

// fleetLabConfig is one rental's worth of configuration, and the measurement
// that says why it is one.
//
// TWO SIMULTANEOUS sd WORKERS ARE NOT REACHABLE ON ONE LOCAL PROVIDER, which
// is a fact about the provider rather than about this lab. Strict packing means
// a request a live rental can serve rides it, so two rentals need mutually
// incompatible work -- and `genstore.Compat` has four axes, of which three are
// fixed here. `provider` is one; `kind` would give a SPEECH rental, which runs
// piper over `ssh exec`, has no worker and needs a different image; and `gpu`
// does not separate them either, because the local provider advertises exactly
// two offers and `servesClause`'s first two admissions close the gap in both
// directions: a request that names no card rides any rental of its provider,
// and a rental that asked for nothing serves a request naming the card it
// happened to get. Asking for the CPU offer instead is refused one layer down
// -- `node.Spec` will not take a serve command with a CPU-only filter.
//
// MEASURED RATHER THAN ARGUED: the first run of this lab asked for nothing and
// for the card, got ONE node, and generated both requests on it with
// "conditioning cache hit" on the second -- the server's own evidence that it
// was the same process.
//
// So this drives the property two rows share with one, and it is the one the
// browser is actually needed for: a row whose KEY (the node) is stable while
// the things worth reading beside it move. That is the defect this page has
// been repaired for four times.
//
// THE JOBS ARE SLOW ON PURPOSE. A rental with nothing left to do is destroyed
// at once -- no idle timeout, because a node with nothing to do is a node
// billing for nothing -- so a lab that wants to LOOK at a live rental has to
// give it work that outlasts the looking. 40 steps at 256 square on the CPU
// build measured 41 s a job on this machine.
const fleetLabConfig = `
sd:
  serve: [sd-server, --model, /opt/models/sd15.gguf]
  ready_timeout: 4m
  jobs:
    - prompt: placeholder
`

// fleetProbe reads the fleet table, which is its own table beside the sweep's.
//
// THE PAGE'S OWN STRUCTURE, like billingProbe: `table.fleet`, and one line per
// row so a test can assert about the row for a particular node rather than
// about whichever the ordering put first.
const fleetProbe = `(function () {
	var panel = Array.from(document.querySelectorAll("section.panel"))
		.find(function (s) {
			var h = s.querySelector("h2");
			return h && h.textContent.trim() === "what is billing";
		});
	if (!panel) { return ["panel|absent"]; }
	return Array.from(panel.querySelectorAll("table.fleet tbody tr")).map(function (r) {
		return (r.className.indexOf("bad") !== -1 ? "BAD " : "") +
			r.innerText.replace(/\s+/g, " ").trim();
	});
})()`

// fleetRows is the fleet table as the browser renders it, one string per row.
func fleetRows(t *testing.T, ctx context.Context) []string {
	t.Helper()
	rows, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](fleetProbe))
	if err != nil {
		t.Fatalf("reading the fleet table: %v", err)
	}
	return rows
}

// pressBilling asks the providers and the queue again.
func pressBilling(t *testing.T, ctx context.Context, console *consoleLog) {
	t.Helper()
	if err := chromedp.Do(ctx,
		chromedp.Click(`//button[contains(text(), "check what is running")]`)); err != nil {
		t.Fatalf("pressing the button: %v\n%s", err, console.text())
	}
}

// rowFor is the one rendered row naming this request, or "".
func rowFor(rows []string, req string) string {
	for _, r := range rows {
		if strings.Contains(r, req[:8]) {
			return r
		}
	}
	return ""
}

// dockerDo runs one docker command against the node, and fails loudly.
//
// THE DAEMON DIRECTLY, which is the only way to produce the state this lab is
// about: `docker pause` freezes every process in the container through the
// cgroup freezer, so the worker stops beating while its TCP connection stays
// established and its card keeps billing. Nothing ember can be asked to do
// produces that, which is the point -- it is what a node that froze or was
// partitioned looks like, and a reachability probe cannot tell it from a node
// that is working.
func dockerDo(t *testing.T, what, id string) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "docker", what, id).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s %s: %v\n%s", what, id, err, out)
	}
}

// TestServe_UIShowsAWorkerOnTheJobAndThenOneThatStopsAnswering.
//
// THE TWO THINGS ONLY A BROWSER CAN SAY, in one rental because that is what a
// local provider can give (see fleetLabConfig), and both of them are the SAME
// defect class this page has been repaired for four times: a row that
// re-renders only when its key changes.
//
//  1. The rental serves two requests in turn. The node is the same machine
//     throughout -- the obvious key -- and the request on the card changes
//     underneath it. A table keyed on the node would show the first request
//     for ever.
//  2. The node is then FROZEN with `docker pause`. The container stays up, the
//     TCP connection stays established, the card keeps billing, and the worker
//     stops beating. That is the middle row of the heartbeat's own table, it
//     is the reason there is a beat instead of a port check, and no unit test
//     can produce it: it needs a real worker that was really beating.
func TestServe_UIShowsAWorkerOnTheJobAndThenOneThatStopsAnswering(t *testing.T) {
	requireChrome(t)
	requireSDImage(t)

	l := harness.New(t)
	l.WriteConfig(map[string]string{"image": sdImage, "ttl": "20m", "max_nodes": "2"})
	l.AppendConfig(fleetLabConfig)

	cmd, srv, wait := startServe(t, l)
	stopped := false
	stopServe := func() {
		if stopped {
			return
		}
		stopped = true
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		_ = wait()
	}
	defer stopServe()

	// TWO REQUESTS FOR ONE RENTAL: the first opens it and the second RIDES it,
	// which is the arrangement the money rule is about -- the rider must get
	// the node without getting the cost. The rider carries two jobs so that
	// the window in which it is the one on the card outlasts the twenty
	// seconds the heartbeat takes to go stale.
	const job = `{"prompt":"a paper boat","width":256,"height":256,"steps":40}`
	opener := queueJobs(t, srv, fmt.Sprintf(`{"provider":"local","jobs":[%s]}`, job))
	rider := queueJobs(t, srv, fmt.Sprintf(`{"provider":"local","jobs":[%s,%s]}`, job, job))

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 15*time.Minute)
	defer cancelTimeout()

	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(fmt.Sprintf("%s?t=%s", pageURL, srv.Token))); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	console := watchConsole(t, ctx)
	if err := chromedp.Do(ctx, chromedp.WaitVisible(`h1`)); err != nil {
		t.Fatalf("the app did not mount: %v\n%s", err, console.text())
	}
	recordPageErrors(t, ctx)
	navTo(t, ctx, "what is billing", billingPanel)

	// THE EMPTY-BEFORE-THE-PRESS CHECK IS GONE, AND ITS JOB IS NOT.
	//
	// It asserted that the fleet table was absent until somebody pressed, as
	// the negative control for everything below -- an assertion that merely
	// found a row would otherwise pass against a table that is always there.
	// The panel now answers when it is opened, so that check would be timing
	// and not behaviour: the two requests above are queued BEFORE the browser
	// navigates, a rental is enlisted in the fleet from the moment it is
	// planned, and whether a row exists at this instant depends on how fast
	// the create call is. A flaky control is worse than none, because a
	// control is the thing you trust when the cases around it disagree.
	//
	// What carries its job instead is the sequence below, which is a stronger
	// control than an empty start ever was: the same row has to name the
	// opener, then the RIDER on the same node, then go stale with that node id
	// intact. A table that was always there, or always the same, fails all
	// three -- and unlike an absence, a transition cannot be satisfied by
	// nothing having happened yet.

	// THE OPENER ON THE CARD.
	first := pressUntil(t, ctx, console, l, 10*time.Minute,
		"a fleet row for the request that opened the rental",
		func(rows []string) bool { return rowFor(rows, opener) != "" })
	openerRow := rowFor(first, opener)
	if len(first) != 1 {
		t.Errorf("the fleet shows %d row(s) for one rental: %q", len(first), first)
	}
	node := nodeIn(openerRow)
	if node == "" {
		t.Fatalf("the fleet row names no machine: %q", openerRow)
	}
	if !strings.Contains(openerRow, "live") {
		t.Errorf("a worker that is beating does not read as live: %q", openerRow)
	}
	if !strings.Contains(openerRow, "beat ") {
		t.Errorf("a live row says nothing about when its worker last spoke: %q", openerRow)
	}
	if strings.HasPrefix(openerRow, "BAD ") {
		t.Errorf("a working rental is painted as needing a person: %q", openerRow)
	}
	if !strings.Contains(openerRow, sdImage) {
		t.Errorf("the row does not say what this rental is: %q", openerRow)
	}

	// THE RIDER TAKES THE CARD, ON THE SAME MACHINE. This is the frozen-row
	// defect's own shape: the node is unchanged -- it is what an author would
	// key the row on -- and the request beside it is not.
	second := pressUntil(t, ctx, console, l, 10*time.Minute,
		"the fleet row to name the request that RODE the rental",
		func(rows []string) bool { return rowFor(rows, rider) != "" })
	riderRow := rowFor(second, rider)
	if got := nodeIn(riderRow); got != node {
		t.Errorf("the rider is reported on %q and the opener was on %q; this is one rental",
			got, node)
	}
	if strings.Contains(riderRow, opener[:8]) {
		t.Errorf("the row names both requests at once: %q", riderRow)
	}

	// AND THE MONEY RULE, END TO END. The rider has the node and not the
	// cost: `requests.node` is written through from an event that reaches the
	// OPENER's stream alone, so a rider's node column was empty until the
	// drain loop wrote it -- and routing the event instead would have put one
	// rental's bill on both rows.
	if got := getRequest(t, srv, rider).Node; got != node {
		t.Errorf("the rider's own row says it ran on %q and the fleet says %q", got, node)
	}

	// NOW FREEZE THE NODE. `docker pause` stops every process in the container
	// through the cgroup freezer: the worker cannot beat, nothing closes, and
	// the card is still rented.
	nodes := l.Nodes(t.Context())
	if len(nodes) != 1 {
		t.Fatalf("the daemon holds %d ember container(s), want 1", len(nodes))
	}
	frozen := nodes[0].ID
	dockerDo(t, "pause", frozen)
	unpaused := false
	unpause := func() {
		if unpaused {
			return
		}
		unpaused = true
		dockerDo(t, "unpause", frozen)
	}
	// Registered before the server is signalled so it unwinds after it: a
	// destroy must not be asked to stop a container whose processes cannot
	// run.
	defer unpause()

	// Longer than the threshold, because what is being waited for is the
	// ABSENCE of frames.
	time.Sleep(control.HeartbeatStale + 5*time.Second)

	after := pressUntil(t, ctx, console, l, 90*time.Second,
		"the frozen node's row to stop reading as live",
		func(rows []string) bool {
			return strings.Contains(strings.Join(rows, " // "), "stale")
		})

	staleRow := rowFor(after, rider)
	if staleRow == "" {
		// The rider may have been dropped by then; any row for this node will
		// do, because there is one rental.
		staleRow = strings.Join(after, " // ")
	}
	if !strings.Contains(staleRow, "stale") {
		t.Fatalf("no row went stale: %q", after)
	}
	// THE ROW CHANGED AND DID NOT MOVE: same machine, different state. A
	// table that had re-rendered the state and dropped the identity, or kept
	// the identity and frozen the state, fails one of these two.
	if !strings.Contains(staleRow, node) {
		t.Errorf("the row lost its machine when it went stale: %q (was %q)", staleRow, node)
	}
	if !strings.HasPrefix(staleRow, "BAD ") {
		t.Errorf("a rental that is not answering and is still billing is not painted as "+
			"needing a person: %q", staleRow)
	}
	if !strings.Contains(staleRow, "not answering") {
		t.Errorf("a stale row says only the word: %q.\n`stale` alone reads as a cache that "+
			"needs refreshing, which is the opposite of what it means here", staleRow)
	}
	// AND THE PANEL SAYS SO ABOVE THE TABLE, because a colour is not a
	// sentence and this is the number an operator acts on.
	says := billingSays(t, ctx)
	if !strings.Contains(says["verdict"], "not answering and are still billing") {
		t.Errorf("the panel's verdict does not mention the wedged rental: %q", says["verdict"])
	}
	// AND EVERY COLUMN OF THIS TABLE IS NAMED. Three of the seven are a bare
	// duration or a single word out of queue.WorkerState -- `starting`, `2s`,
	// `30m left` -- so this table needs its header more than the findings
	// table beside it, not less. `heartbeat` covers both things that cell
	// holds: the age of the last beat, and the sentence that replaces it when
	// the row is wrong.
	const want = "state // node // card · image // generating // heartbeat // deadline // cost"
	if got := says["fleetheader"]; got != want {
		t.Errorf("the fleet table's columns are named %q, want %q", got, want)
	}

	unpause()
	stopServe()
	if errs := console.errors(); len(errs) > 0 {
		t.Errorf("the page logged %v", errs)
	}
}

// pressUntil presses the button until the fleet table says what is wanted.
//
// A PRESS PER POLL AND NOT A TIMER, which is the panel's own rule: the route
// makes three provider sweeps on the request's own context, so it answers when
// somebody asks and never on a schedule. The sleep between presses is what
// keeps this lab from being the poll the design refuses.
func pressUntil(
	t *testing.T, ctx context.Context, console *consoleLog, l *harness.Lab,
	within time.Duration, what string, want func([]string) bool,
) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		pressBilling(t, ctx, console)
		// Long enough for a sweep of one local daemon to come back.
		time.Sleep(2 * time.Second)
		rows := fleetRows(t, ctx)
		if want(rows) {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fleet never showed %s within %v.\nrows: %q\n%s\n"+
				"--- what ember printed ---\n%s",
				what, within, rows, console.text(), tailOf(l.Printed(), 3000))
		}
		time.Sleep(time.Second)
	}
}

// nodeIn is the `provider/id` a rendered row names, or "".
//
// A PARSE AND NOT A CONTAINS, because what the two reads below compare is
// whether the row is about the SAME machine -- and `strings.Contains` of a
// handle inside a row that happens to mention another one would answer yes to
// the wrong question.
func nodeIn(row string) string {
	for _, f := range strings.Fields(row) {
		if strings.HasPrefix(f, "local/") {
			return f
		}
	}
	return ""
}

// tailOf is the last n bytes, because sd-server's progress bars are long.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
