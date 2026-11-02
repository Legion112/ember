//go:build integration && imagelab

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/Legion112/ember/internal/genstore"
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
	// to show, small enough that a laptop CPU finishes it.
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(`textarea`, "a paper boat on a slow river"),
		// SetValue and not SendKeys: a number input already holds 512, and
		// SendKeys would append to it.
		chromedp.SetValue(`input[type=number]`, "192"),
		chromedp.Click(`button.primary`),
		chromedp.WaitVisible(`.jobs li`),
	); err != nil {
		t.Fatalf("queueing a generation: %v\n%s", err, console.text())
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
			ClipSkip: &no, SLG: &no, Hires: &no,
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
	for _, name := range []string{"clip skip", "skip-layer guidance", "the hires pass"} {
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
