//go:build integration && imagelab

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/test/integration/harness"
)

// THE TWO LABS THAT NEED A GENERATION SERVER.
//
// Behind the imagelab tag, with the preview lab, for its reason: CI builds
// only ember:lab, and the CUDA image takes forty minutes to compile. A lab
// requiring an image CI does not build makes every pull request red, which
// teaches people to ignore red.
//
// Neither is skipped. A missing image is a hard failure naming the command
// that builds it, because a lab that skips itself reports success for
// something that never ran.
//
//	make test-serve-lab
//
// WHY ember:sd AND NOT THE CUDA BUILD. sd-server in the CUDA image needs
// libcuda.so.1 to start at all, so there is no GPU-less runner on which it
// proves anything. The CPU image is built from the same upstream revision --
// TestWorkloadImages_PinTheSameUpstreamRevision keeps that true -- and
// carries sd-server and a baked sd15. So these run on a laptop, for nothing,
// against the same program that goes to RunPod.

// sdImage is the CPU workload build.
const sdImage = "ember:sd"

// sdLabConfig is a servable configuration using the model baked into it.
//
// --model and not --diffusion-model: this is a whole checkpoint in one file,
// and the flag that works for one silently cannot load the other.
const sdLabConfig = `
sd:
  serve: [sd-server, --model, /opt/models/sd15.gguf]
  ready_timeout: 4m
  preview: proj
  preview_interval: 1
  jobs:
    - prompt: placeholder
`

// requireSDImage fails loudly, naming the command that fixes it.
func requireSDImage(t *testing.T) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "docker", "image", "inspect", sdImage)
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s is missing. Build it with:\n  make workload-sd MODEL=/path/to/a.gguf", sdImage)
	}
}

// servableLab is a lab whose config `ember serve` will start with.
func servableLab(t *testing.T) *harness.Lab {
	t.Helper()
	requireSDImage(t)
	l := harness.New(t)
	l.WriteConfig(map[string]string{"image": sdImage, "ttl": "20m"})
	l.AppendConfig(sdLabConfig)
	return l
}

// queueJobs posts a batch and returns its id.
func queueJobs(t *testing.T, srv served, body string) string {
	t.Helper()
	resp := ask(t, "POST", srv, "/api/requests", body)
	if resp.Status != http.StatusAccepted {
		t.Fatalf("queueing failed: %d %s", resp.Status, resp.Body)
	}
	var q struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body, &q); err != nil {
		t.Fatal(err)
	}
	return q.ID
}

type requestState struct {
	Status string `json:"status"`
	Node   string `json:"node"`
	Images []struct {
		Path    string `json:"path"`
		Preview bool   `json:"preview"`
	} `json:"images"`
}

func getRequest(t *testing.T, srv served, id string) requestState {
	t.Helper()
	resp := ask(t, "GET", srv, "/api/requests/"+id, "")
	var r requestState
	if err := json.Unmarshal(resp.Body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func waitForStatus(t *testing.T, srv served, id string, want string, within time.Duration) requestState {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r := getRequest(t, srv, id)
		if r.Status == want {
			return r
		}
		switch r.Status {
		case "failed", "cancelled", "interrupted":
			t.Fatalf("request %s ended as %s, want %s", id, r.Status, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("request %s was %s after %v, want %s", id, r.Status, within, want)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestServe_DrainsTheQueueOntoOneRental is the behaviour the whole feature
// exists for.
//
// A SECOND REQUEST ARRIVING WHILE THE FIRST IS GENERATING IS SERVED BY THE
// SAME NODE. Every per-rental cost -- waiting for the machine, pulling the
// image, reading and verifying the weights, loading them into memory -- is
// paid once instead of twice. On a rented A100 that is about three minutes
// against roughly a hundred seconds of generation, so the difference between
// one rental and two is most of the bill.
//
// It is asserted by counting "node ready" in ember's own output, which is the
// only number that cannot be argued with: two of them is two machines.
func TestServe_DrainsTheQueueOntoOneRental(t *testing.T) {
	l := servableLab(t)
	cmd, srv, wait := startServe(t, l)

	// A long first batch, so there is time to queue a second while it runs.
	first := queueJobs(t, srv, `{"provider":"local","jobs":[
		{"prompt":"a paper boat","width":192,"height":192,"steps":4,"seed":1,"output":"boat"},
		{"prompt":"a lighthouse","width":192,"height":192,"steps":4,"seed":2,"output":"light"},
		{"prompt":"a railway station","width":192,"height":192,"steps":4,"seed":3,"output":"station"}
	]}`)

	// Not "a container exists": the rental has to be generating before the
	// second request can join it, and a container exists before sd-server
	// has even been started.
	waitForStatus(t, srv, first, "running", 4*time.Minute)

	second := queueJobs(t, srv, `{"provider":"local","jobs":[
		{"prompt":"a slow river","width":192,"height":192,"steps":4,"seed":4,"output":"river"}
	]}`)

	firstDone := waitForStatus(t, srv, first, "done", 10*time.Minute)
	secondDone := waitForStatus(t, srv, second, "done", 10*time.Minute)

	// EVERYTHING THAT NEEDS THE SERVER HAPPENS FIRST. Signalling and then
	// fetching an image is a connection refused, which the first version of
	// this lab did -- and which reads as a broken endpoint rather than as a
	// test asking a stopped process for something.
	if n := len(firstDone.Images); n < 3 {
		t.Errorf("the first request produced %d image(s), want 3", n)
	}
	if n := len(secondDone.Images); n < 1 {
		t.Errorf("the second request produced %d image(s), want 1", n)
	}
	for _, r := range []requestState{firstDone, secondDone} {
		for _, im := range r.Images {
			if im.Preview {
				continue
			}
			assertDecodablePNG(t, srv, im.Path)
		}
	}

	// The rental's node and cost go to the request that opened it; the rider
	// gets its own per-job card time. Summing the per-request figures would
	// understate the bill and attributing the rental to each would multiply
	// it, so the two differ on purpose.
	if firstDone.Node == "" {
		t.Error("the request that opened the rental does not name its node")
	}
	if secondDone.Node != "" {
		t.Errorf("the rider names a node (%s); the rental belongs to the one that opened it",
			secondDone.Node)
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	out := wait()

	// THE ASSERTION. One machine, not two.
	if n := strings.Count(out, "node ready"); n != 1 {
		t.Errorf("ember rented %d node(s) for two requests, want 1. Output:\n%s", n, out)
	}

	// Nothing survived. The cleanup asserts it too; saying it here names the
	// lab that leaked rather than leaving it to the harness.
	if nodes := l.Nodes(t.Context()); len(nodes) != 0 {
		t.Errorf("%d node(s) survived", len(nodes))
	}
}

// assertDecodablePNG fetches an image and checks it is one.
//
// Decoded and not merely counted: a zero-byte file, a JSON error page and a
// truncated write all have a length, and only decoding tells them from an
// image. The preview lab makes the same check for the same reason.
func assertDecodablePNG(t *testing.T, srv served, path string) {
	t.Helper()
	resp := ask(t, "GET", srv, path, "")
	if resp.Status != http.StatusOK {
		t.Errorf("%s: status %d", path, resp.Status)
		return
	}
	// Declared, never sniffed: these are bytes a rented machine produced.
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("%s: content-type %q", path, got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s: x-content-type-options %q, want nosniff", path, got)
	}

	b := resp.Body
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Errorf("%s is not an image (%d bytes): %v", path, len(b), err)
		return
	}
	if format != "png" {
		t.Errorf("%s decoded as %s", path, format)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		t.Errorf("%s is %dx%d", path, cfg.Width, cfg.Height)
	}
}

// TestServe_UI drives the real page in a real browser.
//
// WHY THIS EXISTS, and it is not completeness. Three bugs in this feature
// were invisible to every other kind of test and cost real debugging time:
//
//   - A nil slice marshals as `null`, and the client's decoder treated that
//     as a type error rather than an empty list -- so the first envelope of
//     every websocket connection was dropped whole, taking the request state
//     with it.
//   - A keyed list only re-renders when its key changes, and a job's key is
//     its index, so the progress row rendered once and froze. The step
//     advanced in the database and never on screen.
//   - The browser cached a wasm module whose name does not change between
//     builds, so a reload after `make web` served the previous app.
//
// Every one of those passed the Go tests, passed `cargo test`, and was
// visible the moment a browser loaded the page. That is what this lab is
// for: it is the only thing in the repository that can see them.
//
// AND NOW THE CREDENTIAL, which is the fourth thing of that kind. `ember
// serve` authenticates every route, and the carrier a browser uses is a
// SameSite=Strict HttpOnly cookie the server hands back in exchange for the
// token in the printed URL. Every claim in that sentence is a claim about
// Chrome and not about ember: whether a 303 with a Set-Cookie leaves the
// token out of the address bar, whether the cookie reaches an <img src> in
// the gallery, and -- the one with no Go test that could ever stand in for it
// -- whether Chrome attaches a Strict cookie to a websocket handshake, which
// is how the progress row learns anything at all. A unit test asserting that
// ember accepts a cookie it was handed proves ember agrees with itself.
//
// It drives BOTH DIRECTIONS in one lab: the page is loaded without the
// credential first and has to be refused, then the one-time URL is opened and
// the whole flow has to work. The refusal half is here rather than in a
// separate lab because a guard with no test for its refusal is the
// AssertNoStray failure -- an assertion that cannot fail, passing for the
// life of the suite -- and the two halves next to each other are what make it
// impossible to weaken one without the other going red.
//
// It needs a Chrome or Chromium binary, which GitHub runners have. A missing
// one is a hard failure naming what to install, never a skip.
func TestServe_UI(t *testing.T) {
	requireChrome(t)
	l := servableLab(t)
	cmd, srv, wait := startServe(t, l)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)

	// STOPPED WITH SIGINT, not left to the context.
	//
	// exec.CommandContext kills with SIGKILL when its context ends, and a
	// SIGKILLed serve leaves its rental running -- deliberately, because no
	// deferred cleanup can cover that case. Leaving it to the test's own
	// context therefore leaks a node on every run, which is what the harness
	// reported the first time this lab passed its own assertions.
	defer func() {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		if out := wait(); !strings.Contains(out, "every rental has been destroyed") {
			t.Errorf("ember exited without reporting the teardown:\n%s", out)
		}
	}()

	// Headless, and no-sandbox because CI runs as root in a container where
	// Chrome's own sandbox cannot start. This is a browser pointed at a
	// loopback port serving this repository's own page.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
	)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()

	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 12*time.Minute)
	defer cancelTimeout()

	// Selectors are plain strings, which chromedp v0.21 resolves with a DOM
	// search -- CSS and XPath both. They are the page's own structure
	// (.posture, .jobs li, .gallery figure) rather than attributes added for
	// a test: if the markup changes this should change with it.
	//
	// FIRST, WITHOUT THE CREDENTIAL. A browser that has never seen the token
	// -- an operator typing the port from memory, or any other page on this
	// machine navigating here -- gets the refusal and not the app.
	if err := chromedp.Do(ctx, chromedp.Navigate(pageURL)); err != nil {
		t.Fatalf("navigating to %s: %v", pageURL, err)
	}
	refused, rerr := chromedp.Run(ctx, chromedp.Text(`body`))
	if rerr != nil {
		t.Fatalf("reading the refusal: %v", rerr)
	}
	if !strings.Contains(refused, "?t=") {
		t.Errorf("an unauthenticated page load was not refused; the page says:\n%s", refused)
	}
	// Not the app. This is the half that would pass against a server which
	// served the UI anyway, so it is asserted separately: the wasm mounts an
	// <h1>, and the refusal is plain text with no elements in it at all.
	mounted, merr := chromedp.Run(ctx,
		chromedp.Evaluate[bool](`document.querySelector('h1') !== null`))
	if merr != nil {
		t.Fatalf("asking whether the app mounted: %v", merr)
	}
	if mounted {
		t.Error("the app mounted for a browser with no credential")
	}

	// THE FLOW A PERSON FOLLOWS: open the line ember printed, see the offers,
	// type a prompt, generate, watch the steps, get an image.
	//
	// The one-time URL carries the token in a query parameter, which the
	// server exchanges for the cookie and redirects away -- so everything
	// after this navigation is the cookie doing the work, including the
	// websocket and the gallery's <img>.
	tokenURL := fmt.Sprintf("%s?t=%s", pageURL, srv.Token)
	if err := chromedp.Do(ctx, chromedp.Navigate(tokenURL)); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}

	// THE CONSOLE WATCHER STARTS AFTER THE FIRST NAVIGATION, and the order
	// is not a style choice. chromedp.Console subscribes to a target, and a
	// context whose target has not been created yet has nothing to subscribe
	// to -- so starting it first deadlocks the first action, and the symptom
	// is a test that hangs until its timeout with an empty console to show
	// for it. Twelve minutes of that is how this was found.
	//
	// It is worth having anyway: a wasm panic leaves a page that simply
	// stops, which looks exactly like a server that is not answering, and
	// which every assertion below could still pass against.
	console := watchConsole(t, ctx)

	// THE TOKEN IS OUT OF THE ADDRESS BAR, which is what the 303 buys: a
	// fragment would never have reached the server, and a query parameter
	// that stayed would sit in the history entry and in every Referer the
	// page sends. Asserted here because only a browser knows what its own
	// location ended up being.
	where, lerr := chromedp.Run(ctx, chromedp.Location())
	if lerr != nil {
		t.Fatalf("reading the location: %v", lerr)
	}
	if strings.Contains(where, srv.Token) || strings.Contains(where, "?t=") {
		t.Errorf("the browser is still on a URL carrying the token: %s", where)
	}

	if err := chromedp.Do(ctx,
		// The app mounted at all, which is the wasm having started.
		chromedp.WaitVisible(`h1`),
		// The posture line: the one thing a person has to read before
		// deciding whether to leave this running.
		chromedp.WaitVisible(`.posture`),
		chromedp.Click(`//button[contains(text(), "what is available")]`),
		chromedp.WaitVisible(`table.offers tr`),
	); err != nil {
		t.Fatalf("loading the page: %v\n%s", err, console.text())
	}

	offers, oerr := chromedp.Run(ctx, chromedp.Text(`table.offers`))
	if oerr != nil {
		t.Fatalf("reading the offers: %v\n%s", oerr, console.text())
	}
	if !strings.Contains(offers, "$") {
		t.Errorf("the offers table shows no price:\n%s", offers)
	}

	// A small generation: enough steps that the progress row has something
	// to show, small enough that a laptop CPU finishes it. It is also
	// smaller than it used to be, measured: with the width asserted the lab
	// runs in 39s against 86s, because the generation it had been doing all
	// along was the panel's full 512x512.
	setNumber(t, ctx, "width", "192", console)
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(`textarea`, "a paper boat on a slow river"),
		chromedp.Click(`button.primary`),
		chromedp.WaitVisible(`.jobs li`),
	); err != nil {
		t.Fatalf("queueing a generation: %v\n%s", err, console.text())
	}

	// THE WIDTH REACHED THE REQUEST, and for the life of this lab nothing
	// checked that it did.
	//
	// The control was driven and the request was queued and no assertion
	// crossed the gap, so a page that took the value into the element and
	// not into the draft would have gone on passing while generating at the
	// panel's default -- which is the AssertNoStray shape inside the one lab
	// that exists because no unit test can reach this class of bug. It is
	// also precisely the frozen-row bug's sibling: the value is on screen
	// and the state behind it is stale.
	//
	// What is asserted is a claim about Chrome and about Leptos rather than
	// about ember: a keystroke has to reach an `on:input` handler, that
	// handler has to update the draft, and the draft has to be what the
	// generate button posts. Read off the request the SERVER stored, because
	// reading it back out of the page would be the page agreeing with
	// itself.
	//
	// Errorf and not Fatalf: at the default of 512 this generation still
	// finishes well inside the budget, so there is no reason to stop and
	// lose everything the rest of the lab would have said. The picker lab
	// fatals on the same assertion because 512 would time it out there.
	queued := waitForQueuedRequest(t, srv, 30*time.Second)
	if raw := string(getRaw(t, srv, queued)); !strings.Contains(raw, `"width":192`) {
		t.Errorf("the width typed into the page did not reach the request: %s", raw)
	}

	// THE PROGRESS ROW ADVANCES, which is the assertion the frozen-row bug
	// would fail: the row appears either way, and only the step text moves.
	// Waited for rather than asserted once, because the model loads first
	// and on a laptop CPU that is tens of seconds.
	step := waitForText(t, ctx, `.jobs li`, "step ", 10*time.Minute, console)
	if !strings.Contains(step, "/") {
		t.Errorf("the progress row shows %q, want a step out of a total", step)
	}
	// A pass and a step, never a percentage: passes restart for each batch
	// item and each stage, so a bar built from them runs to the end and
	// starts again, which reads as a hang.
	if strings.Contains(step, "%") {
		t.Errorf("the progress row shows a percentage (%q); sd-server's steps describe "+
			"the current pass and not the job", step)
	}

	// THE MONEY ROW SAYS WHICH STATE THE RENTAL IS IN, and the figure it
	// shows is the one its caption names.
	//
	// The row used to be gated on `progress.spent` -- the card time the queue
	// emits per job -- captioned "card time", and render `request.cost`,
	// which is the whole rental and is written once from the DEFERRED
	// teardown, to the request that opened the rental and to no other. So it
	// read `$0` for the whole generation and `$0` for ever on a rider.
	//
	// THE LOAD-BEARING HALF IS THE SENTENCE, not the absence of a figure, and
	// the reason is this provider: the local provider's price is zero, so
	// `spent` is zero on every rental any lab here can start and the old row
	// never rendered at all. That is why nothing caught it. Reinstating the
	// old gate therefore makes the row VANISH rather than print `$0`, and
	// only the wait below goes red -- an assertion that the row carries no
	// "$" would pass against the bug it is written for.
	rental := waitForText(t, ctx, `dl.rental`, "rental", 2*time.Minute, console)
	if !strings.Contains(rental, "not priced yet") {
		t.Errorf("the money row says %q; a rental that is still up has no cost yet, and "+
			"the operator has to be able to tell that from a rental that cost nothing",
			rental)
	}
	if strings.Contains(rental, "$") {
		t.Errorf("the money row shows a figure (%q) for a rental ember has not priced; "+
			"requests.cost is written from run_cost, which arrives from the teardown",
			rental)
	}

	// And the image arrives, and is an image.
	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`.gallery figure:not(.preview) img`),
	); err != nil {
		t.Fatalf("waiting for the image: %v\n%s", err, console.text())
	}
	src, err := chromedp.Run(ctx,
		chromedp.AttributeValue(`.gallery figure:not(.preview) img`, "src"))
	if err != nil {
		t.Fatalf("reading the image source: %v\n%s", err, console.text())
	}
	if !src.Exists || src.Value == "" {
		t.Fatal("the gallery has an image with no source")
	}
	assertDecodablePNG(t, srv, strings.TrimPrefix(src.Value, pageURL[:len(pageURL)-1]))

	// The row says it finished, which is the other half of the frozen-row
	// bug: it froze at "starting" while the image was already on screen.
	done := waitForText(t, ctx, `.jobs li`, "file(s)", 3*time.Minute, console)
	if !strings.Contains(done, "file(s)") {
		t.Errorf("the progress row never reported the files: %q", done)
	}

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// offersProbe reads the offers panel the way a person does: whether there is
// a table, what is in it, and whether either sentence that replaces one is on
// screen.
//
// THREE ANSWERS AND NOT ONE, because the defect had two readings and only the
// milder one is about the rows. A page that refreshed its table and went on
// swallowing a provider's error would pass an assertion about the rows alone,
// which is the AssertNoStray shape: a check that can pass for either reason
// proves neither.
const offersProbe = `(function () {
	function texts(sel, sub) {
		return Array.from(document.querySelectorAll(sel))
			.map(function (e) { return e.textContent.trim(); })
			.filter(function (s) { return s.indexOf(sub) !== -1; })
			.join("; ");
	}
	var t = document.querySelector("table.offers");
	return [
		"table|" + (t ? t.innerText.replace(/\s+/g, " ").trim() : ""),
		"nothing|" + texts("p.hint", "has nothing matching"),
		"failed|" + texts("p.error", "could not be asked"),
	];
})()`

// offersSay runs the probe once.
func offersSay(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	lines, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](offersProbe))
	if err != nil {
		t.Fatalf("reading the offers panel: %v", err)
	}
	out := make(map[string]string, len(lines))
	for _, l := range lines {
		k, v, _ := strings.Cut(l, "|")
		out[k] = v
	}
	return out
}

// waitForOffers polls until the panel says what it is supposed to, and fails
// with what it said instead.
//
// A POLL AND NOT A WaitVisible, for the reason waitForText is one: what is
// being waited for is an element's DISAPPEARANCE in one direction and its
// text in the other, and chromedp has no wait for either.
func waitForOffers(
	t *testing.T, ctx context.Context, want func(map[string]string) bool, within time.Duration,
	what string,
) map[string]string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := offersSay(t, ctx)
		if want(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the offers panel never %s within %v; it says table=%q nothing=%q failed=%q",
				what, within, got["table"], got["nothing"], got["failed"])
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// setText types into one of the page's text controls, named by the label
// beside it.
//
// `setNumber`'s selector and keystrokes, with enough backspaces for a card
// name: the same one spelling for both labs, which is why that helper exists.
func setText(t *testing.T, ctx context.Context, label, value string) {
	t.Helper()
	sel := fmt.Sprintf(`//label[span[@class="help" and text()=%q]]/input`, label)
	keys := kb.End + strings.Repeat(kb.Backspace, 32) + value
	if err := chromedp.Do(ctx, chromedp.SendKeys(sel, keys)); err != nil {
		t.Fatalf("setting %s to %q: %v", label, value, err)
	}
}

// TestServe_UIShowsTheSecondAnswerAndNotTheFirst is #130.
//
// WHY A BROWSER, AND WHY THIS IS NOT THE cargo TEST BESIDE IT. The decision
// -- which of the three things a listing renders as -- is `Listing::answer`,
// and `views::tests` drives it through the signal three times. What no Rust
// test can see is the `<For>`: a keyed list re-renders a row only when its
// key changes, the key was the provider's NAME, and `leptos::For` hands the
// item to the child by value -- so the second query of one provider was
// discarded by the renderer, with every layer below it correct. That is the
// third time this exact class has shipped in this file, and a browser is what
// found it each time.
//
// THE FILTER AND NOT THE PROVIDER, because changing the provider resets the
// signal to `Offers::default()` and empties the list, so the one case that
// could not refresh is narrowing the card -- which is what the box is for.
//
// THREE PRESSES AND NOT TWO: a table that has gone and come back is the
// ordinary way an operator uses that box, and a fix that cleared the rows
// once would pass against two presses.
//
// NO RENTAL AND NO WORKLOAD IMAGE. `/api/offers` asks the local provider what
// it has, which costs nothing and touches no card, so this lab is seconds
// rather than minutes -- and the state it is about is reachable with no
// seeding at all, unlike the money column one request over.
func TestServe_UIShowsTheSecondAnswerAndNotTheFirst(t *testing.T) {
	requireChrome(t)
	l := harness.New(t)
	l.WriteConfig(nil)
	l.AppendConfig(serveConfig)

	cmd, srv, wait := startServe(t, l)
	defer func() {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		_ = wait()
	}()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelTimeout()

	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(fmt.Sprintf("%s?t=%s", pageURL, srv.Token))); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	console := watchConsole(t, ctx)

	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`h1`),
		chromedp.Click(`//button[contains(text(), "what is available")]`),
	); err != nil {
		t.Fatalf("asking what is available: %v\n%s", err, console.text())
	}
	first := waitForOffers(t, ctx, func(s map[string]string) bool {
		return s["table"] != ""
	}, 30*time.Second, "showed a table at all")

	// Narrowed to a card no provider has. Same provider, so the same key as
	// the press before it -- which is the entire premise of the defect.
	setText(t, ctx, "card", "nosuchcard")
	if err := chromedp.Do(ctx,
		chromedp.Click(`//button[contains(text(), "what is available")]`)); err != nil {
		t.Fatalf("asking again with a narrowed filter: %v\n%s", err, console.text())
	}
	narrowed := waitForOffers(t, ctx, func(s map[string]string) bool {
		return s["table"] == "" && s["nothing"] != ""
	}, 30*time.Second,
		"replaced the first answer with the second (it is still showing a table)")
	if narrowed["table"] != "" {
		t.Errorf("a narrowed filter still shows the first answer: %q", narrowed["table"])
	}

	// And widened again, because that is the other half of using the box.
	setText(t, ctx, "card", "")
	if err := chromedp.Do(ctx,
		chromedp.Click(`//button[contains(text(), "what is available")]`)); err != nil {
		t.Fatalf("asking again with the filter cleared: %v\n%s", err, console.text())
	}
	again := waitForOffers(t, ctx, func(s map[string]string) bool {
		return s["table"] != "" && s["nothing"] == ""
	}, 30*time.Second, "brought the table back")
	if again["table"] != first["table"] {
		t.Errorf("the same query answered differently:\nfirst %q\nthird %q",
			first["table"], again["table"])
	}

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// TestServe_UIShowsACostWrittenAfterTheStatusEnded is #131.
//
// WHY A BROWSER. The table's `<For>` is keyed on `(id, status)` and
// `requests.cost` is written AFTER the status is terminal -- `finish` writes
// done/failed/cancelled and `Recorder` writes the cost through from
// `provision.WithNode`'s deferred teardown -- so the key `(id, "done")` is
// created with a zero and never changes again. Every layer below the renderer
// was right: the two-second poll re-fetched the correct figure hundreds of
// times and the row would not take it. `cargo test` drives `price_cell`
// through the signal, which is the decision; the key itself is invisible to
// Rust, and this is the only thing that can see it.
//
// WHY IT SEEDS INSTEAD OF RENTING, and this is the sharper half. No lab here
// can produce a non-zero `requests.cost`: every lab rents from the local
// provider, whose price is zero, which is the same reason `Progress::rental`'s
// `Accruing` and its non-free `Charged` are unreachable from one. So a lab
// that rented would assert a blank cell against a blank cell and pass against
// the defect.
//
// WHY THE SEED IS MID-RUN AND NOT BEFORE THE PROCESS STARTS, which is where
// the capability lab's `PutCaps` goes. What has to be shown is that the
// figure arrives in a tab that is ALREADY OPEN: a page loaded after the write
// shows it either way, which is exactly the reload the issue's negative
// control used to prove the bytes were reaching the browser. So the row is
// written while the page is on screen and the cost a moment later, in the
// order the queue writes them. Two writers on one SQLite file is fine and is
// not an accident of this lab: `genstore.OpenFile` sets `journal_mode(WAL)`
// and `busy_timeout(5000)` for exactly this, and the caps lab's own comment
// says so where it closes its handle.
//
// NOTHING IS RENTED. The seeded row is `running` for the instant between
// `Adopt` and the `Update` that ends it, and `ClaimNext` only ever takes a
// QUEUED row -- so the drain loop cannot touch it, and `queue.Run`'s
// Interrupt sweep has already run by the time it exists.
func TestServe_UIShowsACostWrittenAfterTheStatusEnded(t *testing.T) {
	requireChrome(t)
	l := harness.New(t)
	l.WriteConfig(nil)
	l.AppendConfig(serveConfig)

	cmd, srv, wait := startServe(t, l)
	defer func() {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		_ = wait()
	}()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 3*time.Minute)
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

	// Out of band, through the store rather than through the code under
	// test, which is the rule the SIGKILL labs and the caps lab follow.
	store, err := genstore.Open(filepath.Join(l.Dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Errorf("closing the seeded store: %v", cerr)
		}
	}()

	id, err := store.Adopt(t.Context(), genstore.Request{
		Provider: "local",
		GPU:      "h100-80g",
		TTL:      20 * time.Minute,
		Jobs:     []genstore.Job{{Prompt: "a paper boat"}},
	})
	if err != nil {
		t.Fatalf("seeding a request: %v", err)
	}
	// Ended, with no cost, which is the state `finish` leaves behind: the
	// teardown has not written the rental's figure yet and the key
	// (id, "done") is now fixed for ever.
	if err := store.Update(t.Context(), id, func(r *genstore.Request) {
		r.Status = genstore.StatusDone
		r.Ended = time.Now()
	}); err != nil {
		t.Fatalf("ending the seeded request: %v", err)
	}

	cell := rowOf(string(id)) + `/td[@class="price"]`
	// The ROW and not the cell: an empty `<td>` has no bounding box, so
	// WaitVisible on the thing being asserted about would be waiting for the
	// state the assertion exists to refuse.
	if err := shortWait(ctx, rowOf(string(id)), 30*time.Second); err != nil {
		t.Fatalf("the seeded row never reached the table: %v\n%s\n%s",
			err, panelText(ctx), console.text())
	}
	if got := priceOf(t, ctx, cell); got != "" {
		t.Fatalf("an unpriced rental shows %q rather than nothing", got)
	}

	// And the teardown's own write, with the status untouched. $1.24 rather
	// than a round dollar, because `node.MicroUSD.String` is "$%.6g" and a
	// figure Go rendered is the only one this page may print.
	if err := store.Update(t.Context(), id, func(r *genstore.Request) {
		r.Cost = node.MicroUSD(1_240_000)
	}); err != nil {
		t.Fatalf("writing the rental's cost: %v", err)
	}
	after := getRequest(t, srv, string(id))
	if after.Status != "done" {
		t.Fatalf("the seeded request is %q, so the key moved and the lab proves nothing",
			after.Status)
	}

	// No reload anywhere in here, which is the whole assertion: the
	// two-second poll is the only thing that brings the figure over.
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for {
		last = priceOf(t, ctx, cell)
		if last == "$1.24" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the price cell still says %q after the cost was written; "+
				"the row is frozen at its pre-teardown zero\n%s\n%s",
				last, panelText(ctx), console.text())
		}
		time.Sleep(300 * time.Millisecond)
	}

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// priceOf reads one cell's text under its own deadline.
func priceOf(t *testing.T, ctx context.Context, sel string) string {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	text, err := chromedp.Run(c, chromedp.Text(sel))
	if err != nil {
		t.Fatalf("reading %s: %v", sel, err)
	}
	return strings.TrimSpace(text)
}

// TestServe_UIGreysWhatTheServerSaysItCannotDo is the capability gate's only
// honest check.
//
// WHY A BROWSER. The gate is four layers deep -- sd-server's two feature maps,
// `sdserver.Supported`, `capsView.Features`, and a `disabled` attribute
// computed in wasm -- and every layer but the last can be checked in Go or in
// `cargo test`. The last one is what the operator sees, and it is exactly the
// kind of thing this repository has shipped broken while every test passed: a
// nil slice that marshalled as `null` and dropped whole updates, a keyed list
// that never re-rendered, a feature built end to end and connected to nothing.
// A greyed control asserted only in a Rust unit test is a greyed control
// nobody has seen.
//
// WHY IT SEEDS THE CACHE INSTEAD OF RENTING. `generation_features` is all-true
// on every server ember can currently start -- ember hands the weights over as
// argv, and upstream calls that the `@command-line` recipe, which declares
// every feature. The one real response that says no is a server with no model
// at all, and ember never caches that one because `WaitModel` fails first. So
// a rental here would prove the enabled direction and nothing else. The denied
// capabilities are written into the store the way a rental would write them,
// through `genstore.PutCaps`, before the process under test starts: state
// seeded out of band, which is the rule the SIGKILL labs follow.
//
// It asserts BOTH DIRECTIONS from one page load -- three flags denied, three
// allowed, and the one control with no flag at all -- because a lab that only
// checked the denials would pass against a page that greyed everything, which
// is the AssertNoStray failure reached from the opposite side.
func TestServe_UIGreysWhatTheServerSaysItCannotDo(t *testing.T) {
	requireChrome(t)
	l := harness.New(t)
	l.WriteConfig(nil)
	l.AppendConfig(serveConfig)

	// Denied: clip skip, skip-layer guidance, the hires pass. Allowed: flow
	// shift, shifted timestep, vae tiling. `eta` is in neither list because
	// the server publishes no flag for it, and the page must leave it alone.
	no, yes := false, true
	store, err := genstore.Open(filepath.Join(l.Dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutCaps(t.Context(), sdserver.Capabilities{
		Model:      "sd15.gguf",
		Samplers:   []string{"euler", "heun"},
		Schedulers: []string{"discrete"},
		Limits:     sdserver.Limits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048},
		Supported: sdserver.Supported{
			ClipSkip: &no, SLG: &no, Hires: &no, InitImage: &no,
			FlowShift: &yes, ShiftedTimestep: &yes, VAETiling: &yes,
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Closed before ember opens it. SQLite in WAL mode would tolerate two
	// writers, but the lab has nothing more to write and a handle left open
	// across the process under test is a handle nothing accounts for.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cmd, srv, wait := startServe(t, l)
	defer func() {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		_ = wait()
	}()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelTimeout()

	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(fmt.Sprintf("%s?t=%s", pageURL, srv.Token))); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	console := watchConsole(t, ctx)

	// The advanced section is collapsed, which is correct -- five rows are the
	// whole job on almost every run -- so it has to be opened before anything
	// in it is visible. Clicking the summary is what a person does.
	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`h1`),
		chromedp.Click(`//summary[contains(text(), "advanced")]`),
		chromedp.WaitVisible(`details[open] input`),
	); err != nil {
		t.Fatalf("opening the advanced section: %v\n%s", err, console.text())
	}

	// Read off the page's own structure: the label's `denied` class and the
	// control's own `disabled`, keyed by the help span that names it. Both,
	// because they are two different failures -- a disabled input that still
	// looks live reads as a box that has not loaded yet, and a dimmed label
	// around a control that still accepts a value is worse than no gate at
	// all.
	const probe = `Array.from(document.querySelectorAll('details label')).map(function (l) {
		var h = l.querySelector('.help');
		var i = l.querySelector('input, select');
		return [h ? h.textContent.trim() : '',
		        l.classList.contains('denied') ? 'grey' : 'lit',
		        (i && i.disabled) ? 'disabled' : 'enabled'].join('|');
	})`
	lines, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](probe))
	if err != nil {
		t.Fatalf("reading the advanced controls: %v\n%s", err, console.text())
	}
	state := map[string]string{}
	for _, line := range lines {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) == 3 && parts[0] != "" {
			state[parts[0]] = parts[1] + "|" + parts[2]
		}
	}
	t.Logf("advanced controls: %v", state)

	for _, tc := range []struct{ label, want, why string }{
		{"clip skip", "grey|disabled",
			"generation_features.clip_skip is false in the cached capabilities"},
		{"slg scale", "grey|disabled",
			"generation_features.slg is false, and the scale is the block's switch"},
		{"hires pass", "grey|disabled",
			"features.hires is false, so the whole second pass is unavailable"},
		// THE FILE PICKER, which is the one control here whose denial costs
		// an upload as well as a refusal: a greyed picker is a file the
		// operator never sends, and a live one against a model that takes no
		// image is bytes staged for a request ember will refuse at enqueue.
		// The allowed direction is asserted by
		// TestServe_UIStagesAFileFromARealFilePicker, which picks a file and
		// generates from it against a server that says yes.
		{"image", "grey|disabled",
			"generation_features.init_image is false, so the model takes no image input"},
		{"flow shift", "lit|enabled",
			"generation_features.flow_shift is true"},
		{"shifted timestep", "lit|enabled",
			"generation_features.shifted_timestep is true"},
		{"vae tiling", "lit|enabled",
			"generation_features.vae_tiling is true"},
		{"eta", "lit|enabled",
			"neither of the server's feature maps names eta, so ember has no answer about it " +
				"and must not invent one"},
	} {
		got, ok := state[tc.label]
		if !ok {
			t.Errorf("no advanced control labelled %q is on the page; the gate is keyed on "+
				"these labels and a rename makes it silently gate nothing", tc.label)
			continue
		}
		if got != tc.want {
			t.Errorf("%q is %s, want %s: %s", tc.label, got, tc.want, tc.why)
		}
	}

	// AND IT SAYS WHY, in words. A grey box is easy to read as "ember has not
	// implemented this yet", which sends somebody to the issue tracker for a
	// limit that belongs to the model they chose.
	hint, err := chromedp.Run(ctx, chromedp.Text(`details p.hint.denied`))
	if err != nil {
		t.Fatalf("reading the denial hint: %v\n%s", err, console.text())
	}
	for _, name := range []string{"clip skip", "skip-layer guidance", "the hires pass",
		"an image input"} {
		if !strings.Contains(hint, name) {
			t.Errorf("the hint does not name %q: %q", name, hint)
		}
	}
	for _, name := range []string{"flow shift", "vae tiling"} {
		if strings.Contains(hint, name) {
			t.Errorf("the hint names %q, which this server does take: %q", name, hint)
		}
	}
	t.Logf("the page says: %s", hint)

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// TestServe_UINamesWhyARequestFailed is the one assertion that crosses the
// gap this defect sat in.
//
// `requestView.Error` was populated by Go on every request view -- both JSON
// routes and the socket -- decoded by `web/src/model.rs`, and read by NO view.
// Measured the only way that means anything: deleting the field from the Rust
// wire type left the crate compiling and every `cargo test` green, because
// serde drops a field nothing asks for. So the operator-visible state of a
// failed generation was the word `failed` in one table cell and nothing else
// -- while AGENTS.md claimed in prose that "a refused request carries the
// reason, which the page shows".
//
// WHY A BROWSER, and it is the same argument as the greys lab above. Go now
// has a test that the socket pushes the row, and `cargo test` has one that
// `Request::reason` reads it. Both halves agreeing with themselves is exactly
// the state this feature was already in, and the thing neither can see is
// whether any line is drawn: the reason renders inside a `<For>` keyed on
// `(id, status)`, which is the keyed list that once froze a progress row at
// "starting" while the step advanced in the database.
//
// THE TRIGGER IS THE REAL ONE AND NOTHING IS RENTED. A card the local
// provider does not offer is refused by `AppPlanner.Plan` before `Create` is
// reached, so `queue.fail` writes `requests.err`, emits NOTHING -- there is
// no sink for a rental that was never opened -- and no container, no image
// pull and no money are involved. That is also the state the socket could not
// report before `wsCursor`: a row that changed with no event behind it never
// reached an open tab, so this lab loads the page FIRST and posts afterwards.
//
// AND THE NEGATIVE IS THE TRAP #103 NAMES. `Progress::failure`'s view prints
// "THE NODE MAY STILL BE RUNNING AND BILLING -- check ember ls", which is true
// of a destroy that failed and false of a request that never had a node. This
// request never had one, so `p.fatal` must be absent -- and that assertion
// really can fail: routing the reason through `report.KindRunFailed` into that
// same field, which was the smaller change and the obvious one, is what makes
// it appear.
func TestServe_UINamesWhyARequestFailed(t *testing.T) {
	requireChrome(t)
	l := harness.New(t)
	l.WriteConfig(nil)
	l.AppendConfig(serveConfig)

	cmd, srv, wait := startServe(t, l)
	defer func() {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		_ = wait()
	}()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelTimeout()

	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(fmt.Sprintf("%s?t=%s", pageURL, srv.Token))); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	console := watchConsole(t, ctx)
	if err := shortWait(ctx, `h1`, 30*time.Second); err != nil {
		t.Fatalf("the app never mounted: %v\n%s", err, console.text())
	}

	// Posted with the tab already open, so what is asserted below arrived
	// while the page was watching rather than on a reload.
	id := queueJobs(t, srv, `{"provider":"local","gpu":"h100","jobs":[
		{"prompt":"a cat","width":64,"height":64,"steps":1,"seed":1,"output":"cat"}
	]}`)
	failed := waitForFailure(t, srv, id, 2*time.Minute)
	if failed.Error == "" {
		t.Fatalf("the row for %s is failed and carries no reason, so there is nothing for the "+
			"page to show and this lab cannot say anything about the rendering", id)
	}
	t.Logf("the row says: %s", failed.Error)

	// THE ROW NAMES IT. Read off the page's own structure -- the status cell
	// of the row whose id button the page printed -- so markup that changes
	// breaks this, which is the correct direction.
	why := rowOf(id) + `//span[@class="why"]`
	if err := shortWait(ctx, why, 30*time.Second); err != nil {
		t.Fatalf("the failed row never showed a reason: %v\nthe queue panel says: %q\n%s",
			err, panelText(ctx), console.text())
	}
	got, err := chromedp.Run(ctx, chromedp.Text(why))
	if err != nil {
		t.Fatalf("reading the reason off the row: %v\n%s", err, console.text())
	}
	if got != failed.Error {
		t.Errorf("the row says %q and the request carries %q; the page must not paraphrase a "+
			"refusal, because the sentence is the server's own and names the remediation",
			got, failed.Error)
	}

	// AND THE WATCHER NAMES IT, which is the half with no event behind it: a
	// request the planner refused has an empty stream, so a panel folding
	// only events has nothing at all to say about it.
	if err := chromedp.Do(ctx, chromedp.Click(rowOf(id)+`/td[1]/button`)); err != nil {
		t.Fatalf("watching the failed request: %v\n%s", err, console.text())
	}
	if err := shortWait(ctx, `p.why`, 30*time.Second); err != nil {
		t.Fatalf("the watcher never said why the request ended: %v\n%s", err, console.text())
	}
	watched, err := chromedp.Run(ctx, chromedp.Text(`p.why`))
	if err != nil {
		t.Fatalf("reading the watcher's reason: %v\n%s", err, console.text())
	}
	if !strings.Contains(watched, failed.Error) {
		t.Errorf("the watcher says %q, which does not contain the row's reason %q",
			watched, failed.Error)
	}

	// THE SENTENCE THAT MUST NOT BE HERE. Nothing was created, so there is no
	// node to check and no bill accruing; printing the destroy warning over
	// this would send an operator to `ember ls` for a machine that never
	// existed, which is the false-"abandoned" mistake the holder check
	// refuses to make.
	fatal, ferr := chromedp.Run(ctx, chromedp.Evaluate[int](
		`document.querySelectorAll('p.fatal').length`))
	if ferr != nil {
		t.Fatalf("counting the destroy warnings: %v\n%s", ferr, console.text())
	}
	if fatal != 0 {
		t.Errorf("the page prints %d \"the node may still be running and billing\" line(s) over a "+
			"request that never reached a provider", fatal)
	}

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// waitForFailure is waitForStatus' mirror: it waits for the one outcome that
// one treats as fatal.
//
// A SEPARATE HELPER RATHER THAN A PARAMETER, because `waitForStatus` fails the
// lab the moment it sees `failed` -- correctly, for every other lab here,
// where a failure is the thing that went wrong. This one is about the failure,
// and it needs the reason as well as the status, which `requestState` does not
// carry.
func waitForFailure(t *testing.T, srv served, id string, within time.Duration) failedState {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		resp := ask(t, "GET", srv, "/api/requests/"+id, "")
		var r failedState
		if err := json.Unmarshal(resp.Body, &r); err != nil {
			t.Fatal(err)
		}
		if r.Status == "failed" {
			return r
		}
		if r.Status == "done" || r.Status == "cancelled" {
			t.Fatalf("request %s ended as %s; it asked for a card the local provider does not "+
				"offer, so the planner was supposed to refuse it", id, r.Status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("request %s was %s after %v, want failed", id, r.Status, within)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

type failedState struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// TestServe_UICancelSaysWhatTheServerAnswered presses the one button in the
// page whose answer used to go nowhere.
//
// WHY A BROWSER. `POST /api/requests/{id}/cancel` answers carefully in both
// directions -- a 409 whose message names the remediation, and a 200 carrying
// `cancelling` so a page can say a stop is on its way while the status
// honestly still reads `running` -- and the page decoded all of it into a
// `serde_json::Value` and threw the `Result` away with `let _ =`. The Go side
// is well covered and every assertion in it is about the handler; `cargo test`
// covers whether the BUTTON should be drawn and nothing about what the click
// does with the answer. Each half agreed with itself, which is the shape of a
// feature built end to end and connected to nothing, and only a browser
// crosses the gap.
//
// BOTH DIRECTIONS IN ONE LAB, in the order an operator meets them, because a
// lab that asserted only the accepted path would pass against a page that
// swallows every refusal -- which is what it did.
//
// HOW THE REFUSAL IS REACHED, and it is a real state rather than a contrived
// one. `queue.Cancel` returns false unless the request is the one its drain
// loop is generating, and the row says `running` from `ClaimNext` -- which is
// before `generate` registers the stop channel. So the whole of provisioning
// is a window in which the row is cancellable and no rental is generating it.
//
// WHICH IS ONE OF THE TWO 409s AND NOT THE OTHER, and the sleep below is what
// makes sure this lab reaches the one it means. A rental IS in this process
// here, running the probe, about to generate -- so the refusal has to say the
// rental is coming up and to ask again. The sentence it used to print,
// "restart ember serve to have the queue reconcile it", belongs to a row left
// `running` by an ember that is GONE, and following it here destroys the
// rental the operator is paying for. That state cannot be reached from a lab
// without writing a row behind the queue's back, so webapi's own
// TestCancel_SaysTheRentalIsComingUpRatherThanAskingForARestart is what holds
// the two answers apart; this is what proves which one a browser is shown.
//
// AND THE WINDOW IS MADE LONG RATHER THAN RACED. On this provider the window
// is a container create, an sshd and a one-second model load -- a handful of
// seconds against a two-second listing poll, which is a coin flip and cost
// one thirteen-minute run to learn. `gpu_probe` runs on the node before the
// weights and before the server, so a probe that sleeps puts twenty-five
// deterministic seconds inside the window with no test-only code anywhere:
// it is the config key an operator uses for a card that needs time to come
// up, doing what it says, on a lab node that has no card at all.
//
// WHAT PROVES THE ANSWER WAS READ is that the accepted cancel's word differs
// from the one the page puts there when it asks. The button goes away the
// moment the click lands -- a double-clicked button is the ordinary case and
// the second ask is the one that comes back 409 -- so the cell says something
// before the server has answered. If that placeholder read "cancelling" too,
// this assertion would pass against a page that had gone back to ignoring the
// body. It reads "asking"; see `Stopping` in model.rs.
//
// Only the CLICKS are driven here; the batch is posted through the API the way
// the other labs post one. What is under test is the row the page drew and
// what it does with the two answers, and a page-driven batch would make this
// lab's generation time a function of the compose panel's defaults.
func TestServe_UICancelSaysWhatTheServerAnswered(t *testing.T) {
	requireChrome(t)
	l := servableLab(t)
	l.AppendConfig(slowProbeConfig)
	cmd, srv, wait := startServe(t, l)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)

	// SIGINT and not the context's SIGKILL, for TestServe_UI's reason -- and
	// here it is the only thing that ends the rental at all: a cancelled
	// request leaves its rental up on purpose, which is the whole difference
	// between cancelling a job and cancelling a rental.
	defer func() {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		if out := wait(); !strings.Contains(out, "every rental has been destroyed") {
			t.Errorf("ember exited without reporting the teardown:\n%s", out)
		}
	}()

	// Three jobs, so the batch is still generating when the second half of
	// this lab presses cancel on it. A single 192x192 job finishes in a few
	// seconds, and a request that ended is a request with no button.
	batch := queueJobs(t, srv, `{"provider":"local","jobs":[
		{"prompt":"a paper boat","width":192,"height":192,"steps":4,"seed":1,"output":"boat"},
		{"prompt":"a lighthouse","width":192,"height":192,"steps":4,"seed":2,"output":"light"},
		{"prompt":"a railway station","width":192,"height":192,"steps":4,"seed":3,"output":"station"}
	]}`)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 12*time.Minute)
	defer cancelTimeout()

	if err := chromedp.Do(ctx,
		chromedp.Navigate(fmt.Sprintf("%s?t=%s", pageURL, srv.Token))); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	console := watchConsole(t, ctx)
	if err := chromedp.Do(ctx, chromedp.WaitVisible(`h1`)); err != nil {
		t.Fatalf("the app did not mount: %v\n%s", err, console.text())
	}

	// THE REFUSAL. The row reads `running` from the claim while the node is
	// still running the probe, so the queue has nothing to signal.
	waitForText(t, ctx, rowOf(batch)+`//td[@class="status"]`, "running",
		2*time.Minute, console)
	if err := chromedp.Do(ctx, chromedp.Click(stopOf(batch)+`/button`)); err != nil {
		t.Fatalf("pressing cancel: %v\n%s", err, console.text())
	}
	if err := shortWait(ctx, queuePanel+`//p[@class="error"]`, 45*time.Second); err != nil {
		t.Fatalf("the page said nothing about a refused cancel: %v\n"+
			"the queue panel says: %q\nthe request is %q\n%s",
			err, panelText(ctx), getRequest(t, srv, batch).Status, console.text())
	}
	// WHICH 409, and this is the whole of #119. There are two ways to reach
	// this refusal and the sleep above picks one of them deliberately: a
	// rental IS in this process, it is running the probe, and it is about to
	// generate. The message used to be the other one -- "restart ember serve
	// to have the queue reconcile it" -- which is the single action that
	// destroys the rental the operator is paying for, to reconcile a request
	// that was seconds from running. So what is asserted here is the
	// provisioning sentence AND the absence of the restart advice: a lab that
	// checked only for a 409, or only for an error element, would pass
	// against the destructive wording going straight back in.
	refusal := waitForText(t, ctx, queuePanel+`//p[@class="error"]`,
		"coming up", 10*time.Second, console)
	t.Logf("the page says: %s", refusal)
	if !strings.Contains(refusal, "ask again") {
		t.Errorf("the page shows %q; the provisioning window resolves itself, so the "+
			"remediation is to ask again rather than to restart anything", refusal)
	}
	if strings.Contains(refusal, "restart ember serve to have the queue reconcile it") {
		t.Errorf("the page shows %q; that is the refusal for a row left running by an "+
			"ember that is GONE, and following it here destroys a live rental", refusal)
	}
	if !strings.Contains(refusal, "do not restart ember serve") {
		t.Errorf("the page shows %q; the operator's instinct is the advice this message "+
			"replaced, so saying not to is the useful half", refusal)
	}

	// AND THE BUTTON CAME BACK, because nothing was stopped. A refused cancel
	// that left the row un-pressable would be the opposite failure: a request
	// still generating with no way to ask again after the restart the message
	// asks for.
	if err := shortWait(ctx, stopOf(batch)+`/button`, 30*time.Second); err != nil {
		t.Fatalf("the cancel button did not come back after a refusal: %v\n"+
			"the queue panel says: %q\n%s", err, panelText(ctx), console.text())
	}

	// Generating for real now, which is what registers the stop channel. The
	// watcher is opened by clicking the row's own id -- its first button --
	// so the evidence is on screen rather than polled from the API.
	//
	// A JOB ROW EXISTING IS THE SIGNAL, and not a step within it: a row
	// appears when `unit_started` is emitted, which is already inside
	// `generate`, and the step text is gone again as soon as that job's
	// images arrive. Waiting for "step " cost a ten-minute run that read
	// "job 1/3 ... 1 file(s)" and never saw one, because the first job had
	// finished before the socket replayed its frames.
	if err := chromedp.Do(ctx, chromedp.Click(rowOf(batch)+`/td[1]/button`)); err != nil {
		t.Fatalf("watching the request: %v\n%s", err, console.text())
	}
	if err := shortWait(ctx, `.jobs li`, 5*time.Minute); err != nil {
		t.Fatalf("no job of the batch ever started: %v\n%s", err, console.text())
	}

	// THE ACCEPTED ONE, which says so before the next poll: the status the row
	// holds is still `running` and will be for as long as sd-server takes to
	// stop, so saying only that would read as a click that did not register.
	if err := chromedp.Do(ctx, chromedp.Click(stopOf(batch)+`/button`)); err != nil {
		t.Fatalf("pressing cancel on a generating request: %v\n%s", err, console.text())
	}
	if err := shortWait(ctx, stopOf(batch)+`/span[contains(text(), "cancelling")]`,
		30*time.Second); err != nil {
		t.Fatalf("the accepted cancel never said what the server answered: %v\n"+
			"the queue panel says: %q\n"+
			"a page that ignores the body is left saying \"asking\", which is the "+
			"word it puts there before the request is sent\n%s",
			err, panelText(ctx), console.text())
	}

	// AND NO BUTTON IS LEFT on that row, because a double-clicked button is
	// the ordinary case: the second ask is the one that comes back 409, and a
	// refusal printed over a cancel that worked is worse than no message at
	// all.
	left, lerr := chromedp.Run(ctx, chromedp.Evaluate[int](
		`document.querySelectorAll('table.queue td.stop button').length`))
	if lerr != nil {
		t.Fatalf("counting the cancel buttons: %v\n%s", lerr, console.text())
	}
	if left != 0 {
		t.Errorf("%d cancel button(s) are still pressable after an accepted cancel; "+
			"the second ask is the one that comes back refused", left)
	}

	// The earlier refusal is gone, so the operator is not reading a sentence
	// about the ask before this one.
	stale, serr := chromedp.Run(ctx, chromedp.Evaluate[int](
		`document.querySelectorAll('table.queue ~ p.error, table.queue + p.error').length`))
	if serr != nil {
		t.Fatalf("asking whether the refusal is still on screen: %v\n%s", serr, console.text())
	}
	if stale != 0 {
		t.Error("the refused cancel's message is still on screen after an accepted one")
	}

	// AND THE QUEUE RECORDED IT, which is the half the handler deliberately
	// does not write: a generating request is still generating when the
	// handler returns, so how it ended belongs to the drain loop. Read from
	// the store through the API, because reading it back off the page would be
	// the page agreeing with itself.
	waitForStatus(t, srv, batch, "cancelled", 5*time.Minute)

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// slowProbeConfig puts a known number of seconds between the claim and the
// generation.
//
// NOT A TEST HOOK: `gpu_probe` is the config key an operator writes for a card
// that needs time to initialise, `gpucheck.Wait` runs it over SSH before the
// assets and before the server, and this node has no card to check -- so a
// probe that sleeps and exits zero is that key doing exactly what it says.
// What it buys is the only window in which a row is honestly `running` and the
// queue has nothing to signal, long enough to click in rather than race.
const slowProbeConfig = `
gpu_probe: ["sh", "-c", "sleep 25"]
`

// The queue panel's own structure, named once rather than per call site: two
// spellings of one selector is the drift the journal's "nodes" literal cost.
//
// A ROW IS FOUND BY THE ID THE PAGE PRINTS, which is the first eight
// characters of the request id in its own button -- so a lab with more than
// one request in the listing cannot assert about whichever row the ordering
// happened to put first.
const queuePanel = `//section[h2[text()="the queue"]]`

// TestServe_UIResumesAProgressStreamItLost.
//
// THE ONE CLASS OF BUG ONLY A BROWSER CAN SEE, reached from a third
// direction. The progress socket noticed its own close, wrote a signal with
// three writers and NO READER, and never re-opened -- so a stream that ended
// mid-generation left the last frame on screen for ever, under a panel still
// captioned as though the card were sampling. #132 measured it on the real
// wasm: the jobs row and the whole panel byte-for-byte identical six seconds
// after the server closed the socket, with no console output of any kind.
// `Socket::open` has taken an `after` since it was written and its one caller
// passed the literal `0`.
//
// TWO DROPS ON ONE GENERATION, and neither can stand in for the other.
//
// The first is made by the BROWSER, with `ember serve` untouched, because
// that is the only arrangement in which the generation carries on underneath
// -- which is the whole story the defect tells: a frozen watcher over a card
// that is still sampling and still billing. What it proves is that the step
// on screen moves past what it was, and nothing else on this page reads a
// sampling step, so only a resumed stream can have brought it.
//
// The second is a RESTART, which is the case the issue leads with: the
// remediation ember's own cancel 409 prints, and how a config change is
// picked up. It cannot produce new events -- it kills the thing that would --
// so what it proves instead is that the note appears and goes away and that a
// NEW process reading the same rows is handed a cursor at least as high as
// the page had reached. A tab left on the old behaviour is permanently
// half-live after one: the queue panel recovers because it polls, and the
// watcher does not.
//
// WHAT NO ASSERTION ON THE PAGE CAN SEE is which cursor the reconnect asked
// from, because `Progress::apply` is idempotent per sequence number: a client
// that replayed the whole generation renders exactly what one that resumed
// correctly renders. So the cursor is read off `ember serve --log-level
// debug`, which is the only place that knows -- and
// TestWS_RecordsTheCursorItWasAskedToResumeFrom is what keeps that line
// honest, because a lab resting on a log line nothing promises to print is a
// lab resting on a grep.
func TestServe_UIResumesAProgressStreamItLost(t *testing.T) {
	requireChrome(t)
	l := servableLab(t)

	// A PORT AND A TOKEN THE LAB OWNS, for the reasons on startServeOn: a
	// reconnect to a different port is not a reconnect, and a minted token
	// does not survive a restart -- so a lab using one would be measuring
	// that instead.
	port := freePort(t)
	const token = "ember-lab-reconnect-token-0123456789"
	first, srv, firstWait := startServeOn(t, l, port, token, "--log-level", "debug")
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
	)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 12*time.Minute)
	defer cancelTimeout()

	tokenURL := fmt.Sprintf("%s?t=%s", pageURL, srv.Token)
	if err := chromedp.Do(ctx, chromedp.Navigate(tokenURL)); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	// After the first navigation: chromedp.Console subscribes to a target,
	// and a context whose target does not exist yet has nothing to subscribe
	// to -- see TestServe_UI, where starting it first deadlocked the lab.
	console := watchConsole(t, ctx)

	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`h1`),
		chromedp.SendKeys(`textarea`, "a paper boat on a slow river"),
	); err != nil {
		t.Fatalf("loading the page: %v\n%s", err, console.text())
	}
	// BEFORE THE SOCKET EXISTS, which is the one ordering constraint: the page
	// opens it when a request starts being watched, and that is the generate
	// click below.
	recordSockets(t, ctx)
	recordPageErrors(t, ctx)
	setNumber(t, ctx, "width", "192", console)
	if err := chromedp.Do(ctx,
		chromedp.Click(`button.primary`),
		chromedp.WaitVisible(`.jobs li`),
	); err != nil {
		t.Fatalf("queueing a generation: %v\n%s", err, console.text())
	}
	id := waitForQueuedRequest(t, srv, 30*time.Second)

	// THE NODE ROW, which is `node_ready` folded in -- so the watcher has a
	// cursor above zero and something on screen that goes stale the moment
	// the stream ends. Waited for rather than the first sampling step,
	// because the step is on the far side of a model load and this lab is
	// about the socket and not about the generation.
	rental := waitForText(t, ctx, `dl.rental`, "node", 6*time.Minute, console)
	if !strings.Contains(rental, "node") {
		t.Fatalf("the watcher never named the node: %q", rental)
	}
	// AND A SAMPLING STEP, which is the thing that goes stale. The defect's
	// own measurement is this text, byte for byte identical six seconds after
	// the stream was gone.
	step := waitForText(t, ctx, `.jobs li`, "step ", 8*time.Minute, console)

	// ABSENT WHILE THE SOCKET IS OPEN, which is half of what makes the line a
	// signal: one that is always there is read as decoration, and an
	// assertion that only ever looks for its presence passes against a page
	// that prints it unconditionally.
	if present, err := streamNote(ctx); err != nil {
		t.Fatalf("reading the stream note: %v\n%s", err, console.text())
	} else if present != "" {
		t.Errorf("the page reports a stopped stream while the socket is open: %q", present)
	}

	// THE FIRST DROP, FROM THE BROWSER, with `ember serve` untouched -- which
	// is the only arrangement in which the generation carries on underneath,
	// and that is the whole story the issue tells: a frozen watcher over a
	// card that is still sampling and still billing. A restart is the second
	// drop below and cannot do this, because it kills the thing that would
	// produce the new events.
	dropSocket(t, ctx, console)

	// AND THE STEP MOVED ON. Only a stream that re-opened could have brought
	// this: the events behind it were appended after the socket was gone, and
	// nothing else on this page reads them -- the queue table's two-second
	// poll carries a status and a cost and no sampling step at all.
	moved := waitForChange(t, ctx, `.jobs li`, step, 4*time.Minute, console)
	if !strings.Contains(moved, "step ") && !strings.Contains(moved, "file(s)") {
		t.Errorf("the progress row reads %q after the stream came back, which is neither "+
			"a step nor a finished job", moved)
	}

	// THE CURSOR, ON THE WIRE. Everything above is consistent with a page
	// that re-opened from ZERO and replayed the whole generation, because the
	// fold is idempotent per sequence number -- so the page renders the same
	// either way and this is the only half that is not.
	resumed := waitForResume(t, l, 60*time.Second)
	if resumed <= 0 {
		t.Fatalf("the reconnect asked for after=%d, which is the defect: "+
			"Socket::open's one caller passed the literal 0", resumed)
	}
	// AND NOT PAST THE END, which is the other direction and the one that
	// loses events: a cursor above what the server has is a watcher that
	// never learns what it missed, and the row freezes exactly as it used to.
	if end := lastSeq(t, srv, id); resumed > end {
		t.Errorf("the reconnect resumed from %d and the request's last event is %d; "+
			"a cursor past the end is a watcher that silently skips what it missed",
			resumed, end)
	}

	// AND IT STOPS. An unbounded retry is a loop nobody asked for, and a page
	// that retries for ever can never print the sentence telling an operator
	// to reload. Six seconds is longer than the backoff's own cap, so a
	// socket still re-opening on a timer would show up as another handshake.
	opens := len(resumedCursors(t, l))
	time.Sleep(6 * time.Second)
	if again := len(resumedCursors(t, l)); again != opens {
		t.Errorf("the watcher opened %d socket(s) and then %d: a stream that is up is "+
			"still being re-opened on a timer", opens, again)
	}

	// THE SECOND DROP: A RESTART, which is the case the issue leads with --
	// the remediation ember's own cancel 409 prints, and how a config change
	// is picked up. SIGINT and not the test's context, for TestServe_UI's
	// reason: exec.CommandContext kills with SIGKILL, and a SIGKILLed serve
	// leaves its rental running.
	if err := first.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signalling the first ember serve: %v", err)
	}
	if out := firstWait(); !strings.Contains(out, "every rental has been destroyed") {
		t.Fatalf("the first ember serve exited without reporting the teardown:\n%s", out)
	}

	// THE PAGE SAYS SO, which it could not say at all. Asserted on THIS drop
	// and not on the browser-side one above for a reason that is about the
	// fix rather than about the lab: the reconnect after a close with the
	// server up succeeds on the first attempt, 250ms later, so the note is on
	// screen for less time than any poll can see -- which is correct
	// behaviour and not something to assert around. A restart is seconds, and
	// an operator looking at the tab is the reader this line exists for.
	note := waitForStreamNote(t, ctx, "stopped", 60*time.Second, console)
	if !strings.Contains(note, "last one that arrived") {
		t.Errorf("the note does not say the step on screen is stale: %q", note)
	}
	if !strings.Contains(note, "reconnecting") {
		t.Errorf("the note does not say ember is trying again: %q", note)
	}
	// AND NOT THE OTHER SENTENCE, which is this lab's own limitation asserted
	// rather than left to be noticed -- the arrangement
	// TestConformance_CannotCatchARederivedRunID uses.
	//
	// `Stream::Lost` -- the retry bound spent, "it did not come back: reload
	// this page" -- is unreachable from HERE, and the reason is structural
	// rather than a matter of effort: it needs a request the page still
	// believes is live whose server never returns, and that is the one case
	// which cannot be followed by the reconnect this lab exists for. A page
	// that has learned its request ended stops retrying by design, and after a
	// restart `queue.Run`'s sweep has ended it. So that state is
	// `cargo test`'s alone (`the_retry_is_bounded_and_backs_off`), and what
	// this assertion buys is the day somebody shortens the bound past a
	// restart typed by hand: the sentence changes, this goes red, and the
	// reader arrives at RETRY_ATTEMPTS, which is set against exactly that.
	if strings.Contains(note, "did not come back") {
		t.Errorf("the retry bound was spent before the restart finished, so the page is "+
			"showing the sentence for a stream that is gone: %q", note)
	}

	second, _, secondWait := startServeOn(t, l, port, token, "--log-level", "debug")
	defer func() {
		if err := second.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling the second ember serve: %v", err)
			return
		}
		secondWait()
	}()

	// IT CAME BACK, and the note going away is the only thing on the page
	// that says the socket is open again: `Socket::open` returning Ok says
	// nothing, because a WebSocket handshake is asynchronous and the
	// constructor succeeds against a server that is not there. This waits for
	// the browser's own `onopen`.
	waitForNoStreamNote(t, ctx, 90*time.Second, console)

	// AND IT RESUMED FROM WHERE IT WAS, which a restart makes a sharper claim
	// than the first drop did: this is a NEW process reading the same rows,
	// so the cursor cannot have come from anything the old connection was
	// holding. `l.Printed()` is the second server's own output.
	again := waitForResume(t, l, 90*time.Second)
	if again < resumed {
		t.Errorf("the watcher resumed from %d after the restart, having reached %d before "+
			"it: a reconnect that goes backwards replays what it already folded",
			again, resumed)
	}

	// THE DROP PATH, which was a throw nobody saw. Socket::drop closes the
	// connection, the browser calls `onclose`, and the closure it would call
	// is freed with the struct -- `closure invoked recursively or after being
	// dropped`, observed by clicking a queue row twice. It was harmless only
	// BECAUSE nothing read the signal that handler wrote; now that the page
	// says when a stream stopped, the same close is the old socket reporting
	// a stop over the new socket's state.
	row := rowOf(id) + `/td/button`
	for range 2 {
		if err := shortWait(ctx, row, 30*time.Second); err != nil {
			t.Fatalf("the queue row for %s never appeared: %v\n%s", id[:8], err, panelText(ctx))
		}
		if err := chromedp.Do(ctx, chromedp.Click(row)); err != nil {
			t.Fatalf("clicking the queue row: %v\n%s", err, console.text())
		}
		time.Sleep(time.Second)
	}
	// READ FROM THE PAGE AND NOT FROM THE CONSOLE WATCHER, and the difference
	// is a check that could not fail. `console.errors()` was the obvious
	// assertion and reinstating the drop defect under it was GREEN twice over:
	// `chromedp.Console` streams the console API's own messages and an
	// exception nobody caught is a different event, and `errors()` filters to
	// "panicked" and "undecodable" besides. An uncaught throw is exactly what
	// this defect produces, so the thing listening has to be the page's own
	// `error` handler.
	if errs := pageErrors(t, ctx); errs != "" {
		t.Errorf("the page threw while switching the watched request, which is the "+
			"dropped-closure throw Socket::drop clears its handlers to prevent:\n%s", errs)
	}
	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// recordSockets makes the page's websockets reachable from the lab.
//
// A BROWSER-SIDE INSTRUMENT AND NOT A SEAM IN THE PAGE, which is the line
// `sshx.Runner` is not widened across either: a page that exposed its socket
// so a test could close one would be a production interface shaped by a
// test's convenience, and there is no such thing here -- `api::Socket` holds
// the only reference and nothing in `web/` hands it out. What this does
// instead is wrap the browser's own constructor, which is the same category
// as `offersProbe`: script the lab evaluates in a page it is driving.
//
// WHY A DROP THE BROWSER MAKES, when a restart is right there. A restart
// kills the generation, so the events a resumed stream would deliver never
// exist -- and "the step moved on after the stream came back" is the one
// assertion that is about the defect rather than about the fix's own
// plumbing. `ember serve` has no way to close one watcher's socket and
// nothing should give it one; Chrome's own offline emulation would do it and
// needs `cdproto/network`, which is an indirect requirement this would
// promote, for a weaker drop.
//
// The wrapper returns the real socket from a plain function called with
// `new`, which is legal and is what makes it transparent: wasm-bindgen's glue
// resolves the `WebSocket` identifier against the global object at call time,
// so there is no cached reference to go around it.
func recordSockets(t *testing.T, ctx context.Context) {
	t.Helper()
	const wrap = `(function () {
		if (window.__emberSockets) { return "already"; }
		window.__emberSockets = [];
		var Real = window.WebSocket;
		window.WebSocket = function (url, protocols) {
			var s = protocols === undefined ? new Real(url) : new Real(url, protocols);
			window.__emberSockets.push(s);
			return s;
		};
		window.WebSocket.prototype = Real.prototype;
		return "wrapped";
	})()`
	got, err := chromedp.Run(ctx, chromedp.Evaluate[string](wrap))
	if err != nil {
		t.Fatalf("wrapping the browser's WebSocket: %v", err)
	}
	if got != "wrapped" {
		t.Fatalf("the WebSocket wrapper reported %q, so the sockets below are not the "+
			"page's own", got)
	}
}

// recordPageErrors makes an uncaught exception reachable from the lab.
//
// `watchConsole` CANNOT SEE ONE, measured rather than assumed: reinstating the
// `Socket::drop` defect under this lab's existing `console.errors()` assertion
// was GREEN, twice over. `chromedp.Console` streams the console API's own
// messages, and an exception nobody caught arrives as a different CDP event --
// and `consoleLog.errors()` filters to "panicked" and "undecodable", neither
// of which a dropped-closure throw says. A check that cannot fail is worse
// than no check, so the listener is the page's own.
//
// `error` AND `unhandledrejection`, because wasm-bindgen's glue is reached
// from both an event handler and a promise: the throw this exists for comes
// out of a WebSocket `onclose` and the other half costs one line.
func recordPageErrors(t *testing.T, ctx context.Context) {
	t.Helper()
	const watch = `(function () {
		if (window.__emberErrors) { return "already"; }
		window.__emberErrors = [];
		window.addEventListener("error", function (e) {
			window.__emberErrors.push("error: " + (e.message || String(e)));
		});
		window.addEventListener("unhandledrejection", function (e) {
			window.__emberErrors.push("rejection: " + String(e.reason));
		});
		return "watching";
	})()`
	got, err := chromedp.Run(ctx, chromedp.Evaluate[string](watch))
	if err != nil {
		t.Fatalf("watching for uncaught exceptions: %v", err)
	}
	if got != "watching" {
		t.Fatalf("the page error watcher reported %q, so nothing below is looking", got)
	}
}

// pageErrors is what the page threw, newest last.
func pageErrors(t *testing.T, ctx context.Context) string {
	t.Helper()
	got, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](
		`(window.__emberErrors || ["the page error watcher is gone"])`))
	if err != nil {
		t.Fatalf("reading what the page threw: %v", err)
	}
	return strings.Join(got, "\n")
}

// dropSocket closes the page's current progress socket.
//
// IT FAILS WHEN THERE IS NOTHING TO CLOSE, rather than returning quietly: a
// drop that did not happen makes every assertion after it an assertion about
// a stream that was never interrupted, which is the shape `AssertNoStray`
// had.
func dropSocket(t *testing.T, ctx context.Context, console *consoleLog) {
	t.Helper()
	const drop = `(function () {
		var all = window.__emberSockets || [];
		if (all.length === 0) { return -1; }
		all[all.length - 1].close();
		return all.length;
	})()`
	n, err := chromedp.Run(ctx, chromedp.Evaluate[int](drop))
	if err != nil {
		t.Fatalf("closing the page's progress socket: %v\n%s", err, console.text())
	}
	if n <= 0 {
		t.Fatalf("the page had opened no websocket to close, so nothing below is "+
			"about a stream that dropped\n%s", console.text())
	}
}

// waitForChange polls an element until its text is something other than was.
//
// "IT ADVANCED" AND NOT "IT CONTAINS A STEP", because the row contains a step
// either way: the frozen-row defect and the working page are the same text
// until one of them moves. `waitForText` cannot express it.
func waitForChange(
	t *testing.T, ctx context.Context, sel, was string, within time.Duration, console *consoleLog,
) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got, err := chromedp.Run(ctx, chromedp.Text(sel))
		if err != nil {
			t.Fatalf("reading %s: %v\n%s", sel, err, console.text())
		}
		if got != was {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still reads %q after %v, so nothing the resumed stream carried "+
				"reached it\n%s", sel, was, within, console.text())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitForStreamNote waits for the watcher to report a stopped stream.
//
// NOT `waitForText`, which cannot see an element that is absent: chromedp.Text
// resolves at least one node and blocks until the context ends, so a note that
// never appears costs the whole lab's budget and reports "context deadline
// exceeded" -- which is how the first run of this lab spent twelve minutes.
func waitForStreamNote(
	t *testing.T, ctx context.Context, sub string, within time.Duration, console *consoleLog,
) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for {
		note, err := streamNote(ctx)
		if err != nil {
			t.Fatalf("reading the stream note: %v\n%s", err, console.text())
		}
		last = note
		if strings.Contains(note, sub) {
			return note
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watcher never reported a stopped stream within %v; it says %q\n%s",
				within, last, console.text())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// streamNote is the watcher's one line about its own stream, or "" when there
// is none.
//
// `shortWait` CANNOT ANSWER "IT IS NOT THERE": chromedp.WaitVisible blocks
// until its context ends, so an absence costs the whole wait. This is one
// evaluation and reports the absence as an answer.
func streamNote(ctx context.Context) (string, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return chromedp.Run(c, chromedp.Evaluate[string](
		`(function () { var p = document.querySelector("p.stream");
		  return p ? p.textContent.trim() : ""; })()`))
}

// waitForNoStreamNote waits for the watcher to stop reporting a stopped
// stream.
func waitForNoStreamNote(t *testing.T, ctx context.Context, within time.Duration, console *consoleLog) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		note, err := streamNote(ctx)
		if err != nil {
			t.Fatalf("reading the stream note: %v\n%s", err, console.text())
		}
		if note == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watcher never reconnected within %v; it still says %q\n%s",
				within, note, console.text())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// resumedCursors reads the cursor each progress socket resumed from out of the
// server's own debug log.
//
// A FATAL ON A LINE IT CANNOT PARSE, never a skip of it: a parser that
// silently ignores the line it is looking for reports "no reconnect" for a
// log format that changed, which is a gate that cannot fail.
func resumedCursors(t *testing.T, l *harness.Lab) []int64 {
	t.Helper()
	var out []int64
	for _, line := range strings.Split(l.Printed(), "\n") {
		if !strings.Contains(line, "progress socket opened") {
			continue
		}
		_, rest, ok := strings.Cut(line, "after=")
		if !ok {
			t.Fatalf("a progress socket line records no cursor: %q", line)
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			t.Fatalf("a progress socket line records an empty cursor: %q", line)
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			t.Fatalf("a progress socket line records %q, which is not a cursor: %q",
				fields[0], line)
		}
		out = append(out, n)
	}
	return out
}

// waitForResume waits for a handshake that carried a cursor and returns the
// largest one.
//
// THE LARGEST AND NOT THE LAST, because a reconnect that lands while the
// server is still starting is refused and retried -- those attempts are
// legitimate and carry the same cursor -- while a page that gave up and
// re-opened from zero would add a 0 after them.
func waitForResume(t *testing.T, l *harness.Lab, within time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var best int64
		for _, c := range resumedCursors(t, l) {
			if c > best {
				best = c
			}
		}
		if best > 0 {
			return best
		}
		if time.Now().After(deadline) {
			t.Fatalf("no progress socket resumed from a cursor within %v. "+
				"What the server printed:\n%s", within, l.Printed())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// lastSeq is the highest sequence number the server has for a request.
//
// Over the same route a client that would rather poll uses, which is the
// point of storing events as rows: `next` there and the cursor a socket
// resumes from are the same number by construction.
func lastSeq(t *testing.T, s served, id string) int64 {
	t.Helper()
	resp := ask(t, "GET", s, "/api/requests/"+id+"/events", "")
	if resp.Status != http.StatusOK {
		t.Fatalf("reading the events of %s: %d %s", id, resp.Status, resp.Body)
	}
	var e struct {
		Next int64 `json:"next"`
	}
	if err := json.Unmarshal(resp.Body, &e); err != nil {
		t.Fatal(err)
	}
	return e.Next
}

func rowOf(id string) string {
	return fmt.Sprintf(`%s//table[@class="queue"]//tr[td/button[text()=%q]]`,
		queuePanel, id[:8])
}

func stopOf(id string) string { return rowOf(id) + `/td[@class="stop"]` }

// shortWait waits for one selector under its own deadline.
//
// `waitForText` CANNOT STAND IN FOR THIS, and the difference cost a
// thirteen-minute run with "context deadline exceeded" for a diagnosis:
// chromedp.Text resolves at least one node and blocks until the context ends,
// so an element that never appears is not a thirty-second failure but the
// whole lab's budget. Every wait here is for something that may legitimately
// not be there, which is the state each assertion is about.
func shortWait(ctx context.Context, sel string, within time.Duration) error {
	c, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	return chromedp.Do(c, chromedp.WaitVisible(sel))
}

// panelText is the queue panel as a person would read it, for a failure
// message. Bounded, and never fatal: a diagnosis that cannot be printed
// without its own failure path is a diagnosis nobody gets.
func panelText(ctx context.Context) string {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	text, err := chromedp.Run(c, chromedp.Text(queuePanel))
	if err != nil {
		return fmt.Sprintf("(the panel could not be read: %v)", err)
	}
	return text
}

// setNumber types a value into one of the page's number controls, named by
// the label beside it.
//
// ONE SPELLING FOR BOTH BROWSER LABS. It was inlined in each, and the two
// disagreed about which mechanism drives a Leptos control -- which is how a
// lab came to set a geometry through a call it had written down as doing
// nothing, and assert nothing about where the value went.
//
// End, then enough backspaces to empty whatever was there, then the digits,
// all as REAL KEY EVENTS. Extra backspaces on an empty field do nothing,
// which is cheaper than asking how many digits the default has.
//
// WHAT chromedp.SetValue ACTUALLY DOES, measured, because what was written
// down about it in this file and in AGENTS.md was wrong. It is
// `SetJavascriptAttribute(sel, "value", v)`, and the pinned v0.21.0's
// embedded `js/setAttribute.js` dispatches a bubbling `input` AND a bubbling
// `change` exactly when the name is `value` -- so it does reach an `on:input`
// handler, and the width TestServe_UI set that way had been arriving in the
// request all along. Driven both ways against the real page: SetValue 192
// queues `"width":192`, SetValue 256 queues `"width":256`. The claim that it
// dispatches nothing was a reading of the call rather than of a run, which is
// the `allowedCudaVersions` and `capabilities.model` mistake one more time,
// in a test helper instead of against an API.
//
// Keystrokes anyway, and NOT because SetValue is broken. A property
// assignment plus a synthesized `Event('input')` exercises one binding; a
// real key event goes through `beforeinput`, the browser's own parsing of
// what a number box will accept, and the element's validity state, none of
// which an assignment touches. So it is the stronger exercise of the path a
// person uses, and it is one mechanism in this file rather than two.
//
// The limit is unchanged and is a limit of TYPING: a decimal point does not
// reach an `<input type=number>` through a synthesized keystroke at all, so a
// fractional value is set with the stepper and no lab here can assert one
// that was typed.
//
// The label's own help span is the selector rather than `input[type=number]`,
// which picks whichever number control the layout happens to put first -- a
// selector that silently follows a reflow is a selector that drives the wrong
// control.
func setNumber(t *testing.T, ctx context.Context, label, value string, console *consoleLog) {
	t.Helper()
	sel := fmt.Sprintf(`//label[span[@class="help" and text()=%q]]/input`, label)
	keys := kb.End + strings.Repeat(kb.Backspace, 8) + value
	if err := chromedp.Do(ctx, chromedp.SendKeys(sel, keys)); err != nil {
		t.Fatalf("setting %s to %s: %v\n%s", label, value, err, console.text())
	}
}

// waitForText polls an element until its text contains sub.
//
// chromedp has no "wait until the text changes", and a generation's first
// step can be minutes away on a CPU while the element is visible from the
// start.
func waitForText(t *testing.T, ctx context.Context, sel, sub string, within time.Duration, console *consoleLog) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for {
		got, err := chromedp.Run(ctx, chromedp.Text(sel))
		if err != nil {
			t.Fatalf("reading %s: %v\n%s", sel, err, console.text())
		}
		last = got
		if strings.Contains(got, sub) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never contained %q within %v; it says %q\n%s",
				sel, sub, within, last, console.text())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// requireChrome fails with what to install rather than skipping.
func requireChrome(t *testing.T) {
	t.Helper()
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if _, err := exec.LookPath(name); err == nil {
			return
		}
	}
	t.Fatal("no Chrome or Chromium on PATH. This lab drives the real UI in a real browser,\n" +
		"which is the only thing that can see a frozen progress row or a wasm that did not\n" +
		"start. Install one, or run the other lab on its own with:\n" +
		"  go test -tags='integration imagelab' -run TestServe_DrainsTheQueue ./test/integration/")
}

// consoleLog collects what the page logged.
type consoleLog struct {
	mu    sync.Mutex
	lines []string
}

// watchConsole drains the browser's console in the background.
func watchConsole(t *testing.T, ctx context.Context) *consoleLog {
	t.Helper()
	c := &consoleLog{}
	go func() {
		for msg, err := range chromedp.Console(ctx) {
			if err != nil {
				return
			}
			c.mu.Lock()
			c.lines = append(c.lines, fmt.Sprintf("[%s] %s", msg.Type, msg.Text))
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *consoleLog) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) == 0 {
		return "(the browser logged nothing)"
	}
	return strings.Join(c.lines, "\n")
}

// errors is the subset that matters. A wasm panic leaves the page
// half-rendered, and an undecodable envelope silently drops a whole update --
// both of which the assertions above can pass against.
func (c *consoleLog) errors() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range c.lines {
		low := strings.ToLower(l)
		if strings.Contains(low, "panicked") || strings.Contains(low, "undecodable") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// --- the inbound image path ---

// uploadImage posts raw bytes to the one route that takes them.
//
// NO FORM AND NO FILENAME, which is the shape of the route rather than a
// convenience here: the server derives the key from the content, so there is
// nothing else to send and nothing a filename could reach.
func uploadImage(t *testing.T, s served, body []byte) answer {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/api/inputs", s.Port)
	req, err := http.NewRequestWithContext(t.Context(), "POST", url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", s.Port))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/inputs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("POST /api/inputs: reading the body: %v", rerr)
	}
	return answer{Status: resp.StatusCode, Body: b, Header: resp.Header}
}

// labPNG is a gradient, encoded here.
//
// A REAL IMAGE AND NOT A CANNED BLOB, and a gradient rather than a flat
// colour: with a low strength the generation keeps most of the input, so an
// input with structure in it is one whose influence is visible in what comes
// back. It is also compressible, which keeps the request body small.
func labPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			im.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func labRef(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestServe_GeneratesADifferentImageFromAnUploadedOne.
//
// THE ASSERTION THE WHOLE INBOUND PATH EXISTS FOR, and it is a pixel
// comparison rather than a plumbing check. `strength`, `img_cfg`,
// `control_strength`, `ip_adapter_strength` and `increase_ref_index` were all
// held back from the web UI on the grounds that a control which accepts a
// value, sends it and changes no pixel reads as working -- so the one of them
// that ships has to be shown changing a pixel, end to end, through the real
// routes.
//
// TWO GENERATIONS WITH THE SAME SEED, the same prompt, the same geometry and
// the same step count, differing only in the init image. A server that
// accepted `init_image` and dropped it -- which is exactly what a live
// sd-server does with `strength` when no image is attached, measured -- would
// produce two identical files and fail here. Both arms run on ONE rental,
// which is also the feature the queue exists for.
//
// It carries the route's refusals too, against a real server rather than an
// httptest one: the bound, the format allow-list and a digest nothing staged.
func TestServe_GeneratesADifferentImageFromAnUploadedOne(t *testing.T) {
	l := servableLab(t)
	cmd, srv, wait := startServe(t, l)

	// ONE SIGNAL, AND THE BODY NEEDS THE OUTPUT. This lab asserts on ember's
	// own "node ready" count, so it has to stop the process itself and read
	// what it printed -- and the deferred stop is still there for every path
	// that returns early. Two unconditional signals is what the first version
	// did, and the second reported "process already finished": a failing
	// assertion about the test's own cleanup, which says nothing about ember.
	stopped := false
	stop := func() string {
		if stopped {
			return ""
		}
		stopped = true
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return ""
		}
		out := wait()
		if !strings.Contains(out, "every rental has been destroyed") {
			t.Errorf("ember exited without reporting the teardown:\n%s", out)
		}
		return out
	}
	defer func() { _ = stop() }()

	pixels := labPNG(t, 192, 192)

	// THE REFUSALS FIRST, because none of them needs a rental and all of them
	// are cheaper to read before four minutes of generation.
	if got := uploadImage(t, srv, []byte("#!/bin/sh\nnot an image\n")); got.Status != 400 {
		t.Errorf("a shell script was accepted as an image: %d %s", got.Status, got.Body)
	}
	over := append(append([]byte(nil), pixels...),
		bytes.Repeat([]byte{0}, blob.MaxInputBytes+1-len(pixels))...)
	if got := uploadImage(t, srv, over); got.Status != 413 {
		t.Errorf("an upload past the bound answered %d, want 413: %s", got.Status, got.Body)
	}
	// And a request naming a digest nothing staged is refused before it is
	// queued, which is the cheapest moment there is.
	unstaged := labRef([]byte("never uploaded"))
	if got := ask(t, "POST", srv, "/api/requests", fmt.Sprintf(
		`{"provider":"local","jobs":[{"prompt":"a cat","init_image":%q}]}`, unstaged,
	)); got.Status != 400 {
		t.Errorf("a request naming an unstaged image answered %d, want 400: %s",
			got.Status, got.Body)
	}

	// THE UPLOAD, and the id is the content. Posted twice, because content
	// addressing is the reason one request cannot be served another's bytes:
	// the same file is one object and the name carries nothing else.
	first := uploadImage(t, srv, pixels)
	if first.Status != 201 {
		t.Fatalf("uploading answered %d: %s", first.Status, first.Body)
	}
	var staged struct {
		ID            string `json:"id"`
		Format        string `json:"format"`
		Width, Height int
		Bytes         int
	}
	if err := json.Unmarshal(first.Body, &staged); err != nil {
		t.Fatal(err)
	}
	if staged.ID != labRef(pixels) {
		t.Errorf("id = %q, want the sha256 of the bytes (%q)", staged.ID, labRef(pixels))
	}
	if staged.Format != "png" || staged.Width != 192 || staged.Height != 192 {
		t.Errorf("staged %+v, want a 192x192 png", staged)
	}
	again := uploadImage(t, srv, pixels)
	var second struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(again.Body, &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != staged.ID {
		t.Errorf("the same file uploaded twice got two names: %q and %q", staged.ID, second.ID)
	}

	// THE TWO ARMS. Same seed, same everything, one with the image.
	const geometry = `"width":192,"height":192,"steps":4,"seed":4242`
	plain := queueJobs(t, srv, fmt.Sprintf(
		`{"provider":"local","jobs":[{"prompt":"a paper boat on a slow river",%s,"output":"plain"}]}`,
		geometry))
	fromImage := queueJobs(t, srv, fmt.Sprintf(
		`{"provider":"local","jobs":[{"prompt":"a paper boat on a slow river",%s,"output":"fromimage",`+
			`"init_image":%q,"strength":0.2}]}`, geometry, staged.ID))

	plainDone := waitForStatus(t, srv, plain, "done", 12*time.Minute)
	imageDone := waitForStatus(t, srv, fromImage, "done", 12*time.Minute)

	// The stored request carries the DIGEST and not the pixels, which is what
	// keeps a request row small and the picture in one place.
	if !strings.Contains(string(getRaw(t, srv, fromImage)), staged.ID) {
		t.Errorf("the stored request does not name the staged image")
	}

	plainBytes := fetchImage(t, srv, plainDone)
	imageBytes := fetchImage(t, srv, imageDone)
	if len(plainBytes) == 0 || len(imageBytes) == 0 {
		t.Fatal("one of the two arms produced no image")
	}
	// THE ASSERTION. Identical files mean the init image reached nothing.
	if bytes.Equal(plainBytes, imageBytes) {
		t.Error("the two arms produced byte-identical images, so the uploaded image " +
			"changed no pixel: sd-server accepts init_image and drops a parameter it was " +
			"not given, which is exactly the failure this path exists to avoid")
	}

	// And one rental served both, which is the saving the queue is for.
	out := stop()
	if n := strings.Count(out, "node ready"); n != 1 {
		t.Errorf("ember rented %d node(s) for two requests, want 1. Output:\n%s", n, out)
	}
	if nodes := l.Nodes(t.Context()); len(nodes) != 0 {
		t.Errorf("%d node(s) survived", len(nodes))
	}
}

// getRaw is one request's JSON, for an assertion about a field no view type
// here models.
func getRaw(t *testing.T, s served, id string) []byte {
	t.Helper()
	return ask(t, "GET", s, "/api/requests/"+id, "").Body
}

// fetchImage is a finished request's one result, as bytes.
func fetchImage(t *testing.T, s served, r requestState) []byte {
	t.Helper()
	for _, im := range r.Images {
		if im.Preview {
			continue
		}
		got := ask(t, "GET", s, im.Path, "")
		if got.Status != http.StatusOK {
			t.Fatalf("%s: status %d", im.Path, got.Status)
		}
		if _, _, err := image.Decode(bytes.NewReader(got.Body)); err != nil {
			t.Fatalf("%s is not an image (%d bytes): %v", im.Path, len(got.Body), err)
		}
		return got.Body
	}
	t.Fatalf("request produced no result image: %+v", r.Images)
	return nil
}

// TestServe_UIStagesAFileFromARealFilePicker.
//
// WHY A BROWSER, AND WHY THIS CONTROL ESPECIALLY. A file picker is the one
// kind of control that has no meaning outside a browser: there is no File
// object, no FileList and no `Blob.array_buffer` in a Rust unit test, and
// `cargo test` can only check that a struct serialises. Everything between
// the click and the digest -- whether the change handler reads the picked
// file at all, whether the bytes survive the trip through a Uint8Array,
// whether the 201 decodes, whether the reference lands in the draft that the
// Add button clones -- is invisible to every other kind of test in this
// repository. The three bugs TestServe_UI was written for were of exactly
// that shape.
//
// THE JOIN IS WHAT IS ASSERTED: a file chosen in a real Chrome becomes the
// sha256 the lab computed itself, on a real queued request, read back through
// the API. A feature built end to end and never connected passes every test
// it has, so the assertion crosses the gap rather than checking either side.
//
// It also asserts the picker's own feedback, which is the only way an
// operator can tell a staged file from a refused one -- the geometry and the
// format the SERVER read out of the header, not what the browser claimed.
func TestServe_UIStagesAFileFromARealFilePicker(t *testing.T) {
	requireChrome(t)
	l := servableLab(t)
	cmd, srv, wait := startServe(t, l)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)

	defer func() {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		if out := wait(); !strings.Contains(out, "every rental has been destroyed") {
			t.Errorf("ember exited without reporting the teardown:\n%s", out)
		}
	}()

	// A file on disk, because that is what a picker picks.
	pixels := labPNG(t, 128, 64)
	path := filepath.Join(t.TempDir(), "a-picture.png")
	if err := os.WriteFile(path, pixels, 0o600); err != nil {
		t.Fatal(err)
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 14*time.Minute)
	defer cancelTimeout()

	if err := chromedp.Do(ctx,
		chromedp.Navigate(fmt.Sprintf("%s?t=%s", pageURL, srv.Token))); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	console := watchConsole(t, ctx)

	// The advanced section is collapsed, correctly, so a person opens it
	// before they can pick a file.
	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`h1`),
		chromedp.Click(`//summary[contains(text(), "advanced")]`),
		chromedp.WaitVisible(`.image-input input[type=file]`),
	); err != nil {
		t.Fatalf("opening the advanced section: %v\n%s", err, console.text())
	}

	// NO STRENGTH CONTROL UNTIL THERE IS AN IMAGE, which is a measurement and
	// not a layout preference: a live sd-server accepts a strength with no
	// image, generates, and reports the strength nowhere -- so ember refuses
	// the combination, and a control that could be set first would be a
	// control whose only outcome is a refusal at enqueue.
	before, err := chromedp.Run(ctx, chromedp.Evaluate[int](
		`document.querySelectorAll('.image-input input[type=number]').length`))
	if err != nil {
		t.Fatalf("counting the strength controls: %v\n%s", err, console.text())
	}
	if before != 0 {
		t.Errorf("the strength control is on screen with no image picked")
	}

	// THE PICKER. DOM.setFileInputFiles, which is what a click on the control
	// and a choice in the file dialog comes to -- the dialog itself is the
	// operating system's and not the page's.
	if err := chromedp.Do(ctx,
		chromedp.SetUploadFiles(`.image-input input[type=file]`, []string{path}),
		// The server's own reading of the header, which is what says the
		// upload was taken rather than refused.
		chromedp.WaitVisible(`.hint.staged`),
	); err != nil {
		t.Fatalf("picking a file: %v\n%s", err, console.text())
	}

	line, err := chromedp.Run(ctx, chromedp.Text(`.hint.staged`))
	if err != nil {
		t.Fatalf("reading the staged line: %v\n%s", err, console.text())
	}
	for _, want := range []string{"png", "128x64"} {
		if !strings.Contains(line, want) {
			t.Errorf("the staged line does not say %q: %q", want, line)
		}
	}
	t.Logf("the picker says: %s", line)

	// And the strength appears, because now there is something for it to
	// apply to.
	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`.image-input input[type=number]`)); err != nil {
		t.Fatalf("the strength control did not appear: %v\n%s", err, console.text())
	}

	// A SMALL GENERATION, set the way a person sets it: select what is there
	// and type over it. setNumber is that, and it is shared with TestServe_UI
	// so the two labs cannot come to disagree about what drives a control --
	// which they did, and the disagreement is written up there.
	for _, f := range []struct{ label, value string }{
		{"width", "192"}, {"height", "192"}, {"steps", "4"}, {"seed", "77"},
	} {
		setNumber(t, ctx, f.label, f.value, console)
	}
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(`textarea`, "a paper boat on a slow river"),
	); err != nil {
		t.Fatalf("queueing a generation: %v\n%s", err, console.text())
	}
	// THE STRENGTH IS SET WITH THE STEPPER, AND THAT IS A MEASUREMENT RATHER
	// THAN A PREFERENCE. A decimal point does not reach an
	// `<input type=number>` through a synthesized key event: typing `0`, `.`,
	// `2` leaves the box holding `0` after the second keystroke -- the `.`
	// appears in neither `value` nor `validity.badInput` -- and the `2` then
	// lands in front, so ember was posted `20` and refused it. Reproduced at
	// the desktop as well as here, and it is the automation rather than the
	// page: the arrow key walks the same box to `0.03` in three presses, and
	// digits typed in sequence arrive in order. So the lab uses the control
	// the way the step attribute is for, and nothing in this repository can
	// assert a fractional value TYPED into a number box.
	//
	// Four presses of a 0.05 step is 0.2, which is a fraction and not a
	// round number, so a request carrying 0 or 1 would not pass this.
	if err := chromedp.Do(ctx,
		chromedp.Click(`.image-input input[type=number]`),
		chromedp.KeyEvent(kb.ArrowUp),
		chromedp.KeyEvent(kb.ArrowUp),
		chromedp.KeyEvent(kb.ArrowUp),
		chromedp.KeyEvent(kb.ArrowUp),
	); err != nil {
		t.Fatalf("setting the strength: %v\n%s", err, console.text())
	}
	if err := chromedp.Do(ctx, chromedp.Click(`button.primary`)); err != nil {
		t.Fatalf("clicking generate: %v\n%s", err, console.text())
	}

	// EITHER A JOB ROW OR THE PAGE'S OWN REASON. Waiting only for the row
	// turns a refused POST into a timeout with nothing to show for it -- the
	// first run of this lab spent fourteen minutes that way over a one-line
	// message that was on screen the whole time. The panel already renders
	// the server's sentence; reading it is what makes the failure name
	// itself.
	if why := waitForQueuedOrRefusal(t, ctx, 90*time.Second); why != "" {
		t.Fatalf("the page did not queue the generation: %s\n%s", why, console.text())
	}

	// THE JOIN. The queued request has to name the digest of the file this
	// test wrote -- not a filename, not a path, not something the browser
	// made up.
	id := waitForQueuedRequest(t, srv, 30*time.Second)
	raw := string(getRaw(t, srv, id))
	if !strings.Contains(raw, labRef(pixels)) {
		t.Fatalf("the request the page queued does not name the picked file's digest (%s): %s",
			labRef(pixels), raw)
	}
	if !strings.Contains(raw, `"strength":0.2`) {
		t.Errorf("the request does not carry the strength that was set: %s", raw)
	}
	// The width the lab typed, which is also the check that the key events
	// above reached the draft at all: at the panel's default of 512 this
	// generation would be slow enough to time out, and asserting it here
	// names the cause rather than leaving a timeout to be guessed at.
	if !strings.Contains(raw, `"width":192`) {
		t.Fatalf("the typed geometry did not reach the request: %s", raw)
	}

	// And it generates: the image arrives and is an image.
	done := waitForStatus(t, srv, id, "done", 12*time.Minute)
	if len(fetchImage(t, srv, done)) == 0 {
		t.Error("the generation produced no image")
	}
	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`.gallery figure:not(.preview) img`)); err != nil {
		t.Fatalf("waiting for the image on the page: %v\n%s", err, console.text())
	}

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// waitForQueuedOrRefusal waits for a job row, and returns why there is not
// one.
//
// One Evaluate rather than two WaitVisibles racing, because the two outcomes
// are mutually exclusive and a race between them would report whichever
// happened to be polled first.
func waitForQueuedOrRefusal(t *testing.T, ctx context.Context, within time.Duration) string {
	t.Helper()
	const probe = `(function () {
		if (document.querySelector('.jobs li')) { return ''; }
		var e = document.querySelector('p.error');
		return e ? 'the page says: ' + e.textContent.trim() : 'waiting';
	})()`
	deadline := time.Now().Add(within)
	for {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[string](probe))
		if err != nil {
			return fmt.Sprintf("asking the page what happened: %v", err)
		}
		if got != "waiting" {
			return got
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("nothing was queued and the page said nothing within %v", within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForQueuedRequest is the id of the request the page just posted.
//
// Polled rather than read out of the page, because the id the UI shows is the
// one the server chose and the assertion is about the row the server stored.
func waitForQueuedRequest(t *testing.T, s served, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var list struct {
			Requests []struct {
				ID string `json:"id"`
			} `json:"requests"`
		}
		if err := json.Unmarshal(ask(t, "GET", s, "/api/requests", "").Body, &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Requests) > 0 {
			return list.Requests[0].ID
		}
		if time.Now().After(deadline) {
			t.Fatalf("the page queued nothing within %v", within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
