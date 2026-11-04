//go:build integration && imagelab

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/test/integration/harness"
)

// THE TWO LABS #139 SLICE 3 ADDED, and both are about something only a
// browser can answer.
//
// The router is a `pushState`, a `popstate` listener and a `preventDefault`,
// and every one of those is a claim about Chrome: whether a click the page
// intercepted really did not load a document, whether the back button reaches
// the listener, whether a deep link mounts the page the path names. The pure
// half -- which path is which page, and which clicks are the page's -- is
// `route.rs`'s own `cargo test`.
//
// AND THE SECOND ONE IS THE HAZARD RATHER THAN A FEATURE. A page change
// unmounts a component for the first time in this program's life, and the
// progress watcher holds a `set_timeout` the BROWSER owns: it fires up to five
// seconds after the operator left, on an owner Leptos has disposed, in a crate
// built `panic = "abort"`. That is a trap rather than an error value, and the
// only thing in a position to see it is the page's own `error` handler.

// routeSentinel is planted on `window` and is how these labs tell a
// client-side navigation from a document load.
//
// A VALUE ON `window` AND NOT A RESOURCE COUNT, and the reason is that a
// document load is INVISIBLE to anything measured inside the document. A full
// load re-fetches about 1.9 MiB of wasm under `Cache-Control: no-store` and then
// everything WORKS -- the page is the same page, the panel is the right panel,
// and `performance.getEntriesByType('resource')` in the new document reports
// the same single `ember_bg.wasm` the old one did, because the timeline starts
// over with the document. So the failure this is written for has no symptom at
// all except a second of latency, which is exactly why it needs an assertion.
// A value set on `window` is cleared by a new document and by nothing else.
//
// AND THESE LABS CARRY THEIR OWN CONTROL: the deep link at the end of
// `TestServe_UINavigatesWithoutReloadingTheApp` is a real document load, and
// the sentinel is asserted GONE after it. Without that the assertion above it
// would be consistent with a sentinel nothing can clear.
const routeSentinel = "ember-lab-did-not-reload-0123456789"

// plantSentinel marks the document, and counts the wasm fetches it has made.
func plantSentinel(t *testing.T, ctx context.Context) int {
	t.Helper()
	plant := fmt.Sprintf(`(function () {
		window.__emberNavProbe = %q;
		return performance.getEntriesByType("resource")
			.filter(function (e) { return e.name.indexOf("ember_bg.wasm") !== -1; })
			.length;
	})()`, routeSentinel)
	n, err := chromedp.Run(ctx, chromedp.Evaluate[int](plant))
	if err != nil {
		t.Fatalf("planting the sentinel: %v", err)
	}
	// The one the document loaded with. A zero means the probe is looking for
	// the wrong name, which would make the count below meaningless.
	if n != 1 {
		t.Fatalf("the document reports %d fetches of ember_bg.wasm rather than one, so the "+
			"count this lab compares against measures nothing", n)
	}
	return n
}

// sameDocument asserts the sentinel is still there, and that the wasm has been
// fetched once.
func sameDocument(t *testing.T, ctx context.Context, what string, wasm int) {
	t.Helper()
	const read = `(function () {
		return [
			window.__emberNavProbe || "",
			String(performance.getEntriesByType("resource")
				.filter(function (e) { return e.name.indexOf("ember_bg.wasm") !== -1; })
				.length),
		];
	})()`
	got, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](read))
	if err != nil {
		t.Fatalf("reading the sentinel after %s: %v", what, err)
	}
	if got[0] != routeSentinel {
		t.Fatalf("%s loaded a whole document: the sentinel is %q. A link the page did not "+
			"intercept is about 1.9 MiB of wasm re-fetched under no-store with everything still "+
			"working, which is the one failure here with no symptom of its own",
			what, got[0])
	}
	if got[1] != fmt.Sprint(wasm) {
		t.Errorf("%s left the document reporting %s fetches of ember_bg.wasm against %d "+
			"before it", what, got[1], wasm)
	}
}

// whereIs is the browser's own address, path and query only.
func whereIs(t *testing.T, ctx context.Context) string {
	t.Helper()
	got, err := chromedp.Run(ctx, chromedp.Evaluate[string](
		`window.location.pathname + window.location.search`))
	if err != nil {
		t.Fatalf("reading the location: %v", err)
	}
	return got
}

// TestServe_UINavigatesWithoutReloadingTheApp.
//
// THE FOUR THINGS A ROUTER IS, each of which is a question about Chrome.
//
//   - A LINK THE PAGE HANDLES does not load a document. `Link` calls
//     `preventDefault` and `pushState`, and a version that forgot either would
//     navigate perfectly: the server answers the app on every route, so the
//     only difference is the 1.9 MiB and the second it takes. The sentinel is
//     the only thing that can see it.
//   - THE BACK BUTTON reaches the `popstate` listener. A `pushState` nobody
//     can come back from has broken the one control every browser has, and a
//     lab that only ever went forwards would not notice.
//   - A DEEP LINK mounts the page the path names, which is the half the server
//     side cannot claim: `TestServe_UIMountsOnATwoSegmentRoute` asserts the
//     app starts at `/requests/{id}` and asserts nothing about WHICH request
//     is on screen, because until this slice the path was not read.
//   - A PATH THIS BUILD DOES NOT OWN says so. `isAppRoute` is every dotless
//     path at any depth, so `ember serve` answers the app for `/spend` and a
//     typo arrives here rather than at a 404. A blank panel there reads as a
//     page that failed to load.
//
// IT RENTS NOTHING. The request it navigates to is seeded through `genstore`,
// out of band, which is the rule the SIGKILL labs and the caps lab follow --
// what is being driven is a router, and a rental would add minutes and a card
// to a question about `pushState`.
func TestServe_UINavigatesWithoutReloadingTheApp(t *testing.T) {
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

	// Seeded before the page is driven, so the queue row is there on the first
	// poll rather than appearing under the click.
	//
	// `Adopt` AND NOT `Enqueue`, for the reason the cost lab gives: a queued
	// row is what `ClaimNext` takes, so enqueueing one here would have this
	// server rent a card for a lab about a router.
	store, err := genstore.Open(filepath.Join(l.Dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Errorf("closing the seeded store: %v", cerr)
		}
	}()
	seeded, err := store.Adopt(t.Context(), genstore.Request{
		Provider: "local",
		GPU:      "h100-80g",
		TTL:      20 * time.Minute,
		Jobs:     []genstore.Job{{Prompt: "a paper boat", Seed: 4242, Sampler: "euler_a"}},
	})
	if err != nil {
		t.Fatalf("seeding a request: %v", err)
	}
	if uerr := store.Update(t.Context(), seeded, func(r *genstore.Request) {
		r.Status = genstore.StatusDone
		r.Ended = time.Now()
	}); uerr != nil {
		t.Fatalf("ending the seeded request: %v", uerr)
	}
	id := string(seeded)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 4*time.Minute)
	defer cancelTimeout()

	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(fmt.Sprintf("%s?t=%s", pageURL, srv.Token))); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	console := watchConsole(t, ctx)
	recordPageErrors(t, ctx)

	if err := shortWait(ctx, composePanel, 30*time.Second); err != nil {
		t.Fatalf("the landing page is not the compose page: %v\n%s", err, console.text())
	}
	if got := whereIs(t, ctx); got != "/" {
		t.Fatalf("the one-time URL left the browser on %q rather than the root, so the "+
			"token exchange's own 303 is what is being measured below", got)
	}
	wasm := plantSentinel(t, ctx)

	// ONE. A link the page handles.
	navTo(t, ctx, "the queue", queuePanel)
	if got := whereIs(t, ctx); got != "/queue" {
		t.Errorf("clicking the queue link left the browser at %q; the signal moved and the "+
			"address bar did not, so a reload goes back", got)
	}
	sameDocument(t, ctx, "the queue link", wasm)
	// AND THE COMPOSE PANEL IS GONE, which is what makes this a page rather
	// than a panel that was scrolled to. Four panels in one `<main>` would
	// pass every assertion above.
	if n := countOf(t, ctx, `section.panel`); n != 1 {
		t.Errorf("the queue page has %d panels on it; one route is one panel, and the grid "+
			"that placed four of them by index is what this slice removed", n)
	}

	// TWO. The row's own link, which is the one the queue exists to offer.
	row := rowOf(id) + `/td/a`
	if err := shortWait(ctx, row, 30*time.Second); err != nil {
		t.Fatalf("the seeded row never reached the table: %v\n%s\n%s",
			err, panelText(ctx), console.text())
	}
	if err := chromedp.Do(ctx, chromedp.Click(row)); err != nil {
		t.Fatalf("clicking the row: %v\n%s", err, console.text())
	}
	if err := shortWait(ctx, watchPanel, 30*time.Second); err != nil {
		t.Fatalf("the row's link did not open the request's page: %v\n%s", err, console.text())
	}
	if got, want := whereIs(t, ctx), "/requests/"+id; got != want {
		t.Errorf("the row's link went to %q, want %q", got, want)
	}
	sameDocument(t, ctx, "the queue row's link", wasm)

	// THREE. The back button, twice, which is the `popstate` listener.
	if err := chromedp.Do(ctx, chromedp.NavigateBack()); err != nil {
		t.Fatalf("going back: %v\n%s", err, console.text())
	}
	if err := shortWait(ctx, queuePanel, 30*time.Second); err != nil {
		t.Fatalf("the back button did not return to the queue: %v\n%s", err, console.text())
	}
	if got := whereIs(t, ctx); got != "/queue" {
		t.Errorf("the back button left the browser at %q rather than the queue", got)
	}
	sameDocument(t, ctx, "the back button", wasm)

	if err := chromedp.Do(ctx, chromedp.NavigateBack()); err != nil {
		t.Fatalf("going back again: %v\n%s", err, console.text())
	}
	if err := shortWait(ctx, composePanel, 30*time.Second); err != nil {
		t.Fatalf("the back button did not return to the compose page: %v\n%s",
			err, console.text())
	}
	if got := whereIs(t, ctx); got != "/" {
		t.Errorf("two back presses left the browser at %q rather than the root", got)
	}
	sameDocument(t, ctx, "the second back press", wasm)

	// THE THROWS, READ BEFORE THE DOCUMENT LOADS BELOW. `recordPageErrors`
	// installs a listener on THIS document's `window`, so the two typed
	// addresses that follow take it with them -- and the first version of this
	// lab read it at the end and reported "the page error watcher is gone",
	// which is the watcher working rather than a finding. Everything above
	// this line is the client-side half, which is the half that can throw:
	// `preventDefault`, `pushState` and the `popstate` listener all run inside
	// the wasm.
	if errs := pageErrors(t, ctx); errs != "" {
		t.Errorf("the page threw while being navigated:\n%s", errs)
	}

	// FOUR. A path this build does not own, reached the way an operator
	// reaches it: a typed address, which is a document load.
	if err := chromedp.Do(ctx, chromedp.Navigate(pageURL+"spend")); err != nil {
		t.Fatalf("navigating to a page ember does not have: %v", err)
	}
	// THE SENTENCE AND NOT THE HEADING, because `innerText` is what is
	// RENDERED: `h2` carries `text-transform: uppercase`, so the heading comes
	// back shouted and a match on its own wording is a match on the
	// stylesheet. Found by the first run of this lab, which printed "THERE IS
	// NO PAGE HERE" at an assertion looking for the lowercase of it.
	none := waitForTextIn(t, ctx, `main`, "has no page at", 30*time.Second)
	if !strings.Contains(none, "/spend") {
		t.Errorf("the page for an address ember does not serve does not name it: %q\n"+
			"the server answers the app for every dotless path, so a typo arrives at this "+
			"page rather than at a 404 and the browser's own error page is not what an "+
			"operator sees", none)
	}

	// AND THE DEEP LINK, LAST, BECAUSE IT IS THE CONTROL. This is a real
	// document load, so the sentinel must be GONE -- which is what proves
	// every `sameDocument` above could have failed. Without it they are all
	// consistent with a sentinel nothing in this lab can clear.
	deep := fmt.Sprintf("%srequests/%s", pageURL, id)
	if err := chromedp.Do(ctx, chromedp.Navigate(deep)); err != nil {
		t.Fatalf("navigating to %s: %v", deep, err)
	}
	if err := shortWait(ctx, watchPanel, 30*time.Second); err != nil {
		t.Fatalf("a deep link to a request did not render its page: %v\n%s",
			err, bodyText(t, ctx))
	}
	// THE REQUEST THE PATH NAMES, and not merely a request page. The id the
	// page prints is the first eight characters, which is what the queue row
	// prints too -- so a page that read its id from anywhere but the path
	// would have to get the same eight characters by accident.
	shown := waitForTextIn(t, ctx, watchPanel, "request ", 30*time.Second)
	if !strings.Contains(shown, id[:8]) {
		t.Errorf("the deep link rendered a request page that does not name %s: %q",
			id[:8], shown)
	}
	gone, gerr := chromedp.Run(ctx, chromedp.Evaluate[string](
		`(window.__emberNavProbe || "")`))
	if gerr != nil {
		t.Fatalf("reading the sentinel after the deep link: %v", gerr)
	}
	if gone != "" {
		t.Errorf("a typed address did not load a document (the sentinel is still %q), so the "+
			"sentinel cannot tell a client-side navigation from a reload and every "+
			"assertion above it passed for the wrong reason", gone)
	}

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// countOf is how many elements match, under its own deadline.
func countOf(t *testing.T, ctx context.Context, sel string) int {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := chromedp.Run(c, chromedp.Evaluate[int](
		fmt.Sprintf(`document.querySelectorAll(%q).length`, sel)))
	if err != nil {
		t.Fatalf("counting %s: %v", sel, err)
	}
	return n
}

// waitForTextIn polls one element's text until it contains what is wanted.
//
// `Evaluate` AND NOT `chromedp.Text`, for the reason `waitForParams` is a
// poll: `Text` resolves at least one node and then blocks until the context
// ends, so a selector matching nothing spends the whole lab's budget and
// reports "context deadline exceeded" -- naming neither the element nor what
// was on screen instead.
func waitForTextIn(
	t *testing.T, ctx context.Context, sel, want string, within time.Duration,
) string {
	t.Helper()
	probe := fmt.Sprintf(`(function () {
		var e = document.evaluate(%q, document, null, 9, null).singleNodeValue
			|| document.querySelector(%q);
		return e ? e.innerText : "";
	})()`, sel, sel)
	deadline := time.Now().Add(within)
	var last string
	for {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[string](probe))
		if err != nil {
			t.Fatalf("reading %s: %v", sel, err)
		}
		last = got
		if strings.Contains(got, want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never said %q within %v; it says %q", sel, want, within, last)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestServe_UILeavingAReconnectingWatcherDoesNotTrapTheWasm.
//
// THE HAZARD #139 PREDICTED, AND IT IS THE ONE THING THIS SLICE INTRODUCED
// RATHER THAN MOVED.
//
// `views::request::retry` schedules the next open with
// `set_timeout(move || connect(w, again), wait)` and a backoff to five
// seconds, and that timeout is held by the BROWSER: it is not an arena entry,
// it is not an `on_cleanup`, and nothing Leptos disposes can reach it. While
// the watcher was a panel that was never unmounted it always landed on live
// storage. Under a router it lands on a disposed owner -- every signal and
// `StoredValue` in `Watcher` is an arena handle whose non-`try` read is an
// `expect`, and `web/Cargo.toml` builds this crate `panic = "abort"`, so the
// first read is a `unreachable` trap: a `RuntimeError` out of a timer callback
// and a page that stops.
//
// AND THE EPOCH GUARD WAS NOT A SUBSTITUTE, which is the sharp part: the guard
// WAS the read that would fault. `Watcher::alive` is `try_get_value`, which
// answers `None` for a disposed entry instead of trapping.
//
// THE PAGE'S OWN `error` HANDLER AND NOT `console.errors()`, measured rather
// than assumed and recorded where `recordPageErrors` is: `chromedp.Console`
// streams the console API's own messages, an exception nobody caught is a
// different CDP event, and `errors()` filters to "panicked" and "undecodable"
// besides -- so the reconnect lab's console assertion was GREEN against a real
// `Socket::drop` throw twice over.
//
// THE WINDOW IS MADE DETERMINISTIC RATHER THAN RACED. A drop with `ember serve`
// up is answered on the first attempt 250ms later, so navigating "inside the
// backoff window" would be a coin flip. `breakSockets` points the page's own
// `WebSocket` constructor at a path that answers no upgrade, so every re-open
// fails and the backoff walks to its five-second cap -- and twelve attempts is
// forty-three seconds of them, so there is a pending timeout at the moment of
// the navigation and it fires within five seconds of it.
//
// A RESTART CANNOT PRODUCE THIS, which is why the drop is browser-side: SIGINT
// ends the rental, `endIfUnfinished` writes a terminal status, and `retry`
// returns `None` for a terminal request -- so there is no pending timeout to
// outlive anything. Measured on master while this lab was being written, where
// it is also why `TestServe_UIResumesAProgressStreamItLost` reports an empty
// stream note after its restart (#185).
func TestServe_UILeavingAReconnectingWatcherDoesNotTrapTheWasm(t *testing.T) {
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
	// After the first navigation: chromedp.Console subscribes to a target, and
	// a context whose target does not exist yet has nothing to subscribe to --
	// see TestServe_UI, where starting it first deadlocked the lab.
	console := watchConsole(t, ctx)

	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`h1`),
		chromedp.SendKeys(`textarea`, "a paper boat on a slow river"),
	); err != nil {
		t.Fatalf("loading the page: %v\n%s", err, console.text())
	}
	// BEFORE THE SOCKET EXISTS, which is the one ordering constraint: the page
	// opens it when the generation starts being watched, and that is the
	// generate click below.
	recordSockets(t, ctx)
	recordPageErrors(t, ctx)
	setNumber(t, ctx, "width", "192", console)
	if err := chromedp.Do(ctx, chromedp.Click(`button.primary`)); err != nil {
		t.Fatalf("queueing a generation: %v\n%s", err, console.text())
	}

	// GENERATING NAVIGATES, which is the compose page's one route edit: it set
	// a `watching` signal a panel beside it read, and the watcher is a page
	// now. So this wait is also the assertion that it went there.
	if err := shortWait(ctx, watchPanel, 60*time.Second); err != nil {
		t.Fatalf("queueing a generation did not open the request's page: %v\n%s",
			err, console.text())
	}
	wasm := plantSentinel(t, ctx)

	// THE NODE ROW, which is `node_ready` folded in: the socket is open, the
	// request is `running` and is not going to end for minutes. Waited for
	// rather than a sampling step, because the step is on the far side of a
	// model load and this lab is about the socket.
	rental := waitForText(t, ctx, `dl.rental`, "node", 6*time.Minute, console)
	if !strings.Contains(rental, "node") {
		t.Fatalf("the watcher never named the node: %q", rental)
	}

	// THE RETRY LOOP, MADE TO KEEP FAILING. See the header: this is what turns
	// "navigate inside the backoff window" from a race into a wait.
	breakSockets(t, ctx)
	opened := countedSockets(t, ctx)
	dropSocket(t, ctx, console)

	// THREE FAILED RE-OPENS, which puts the backoff at a second or more and
	// means the page is certainly in the loop rather than possibly past it.
	waitForSockets(t, ctx, opened+3, 60*time.Second)
	if note, err := streamNote(ctx); err != nil {
		t.Fatalf("reading the stream note: %v\n%s", err, console.text())
	} else if !strings.Contains(note, "reconnecting") {
		t.Fatalf("the watcher is not reconnecting (%q), so there is no pending re-open for "+
			"the navigation below to outlive", note)
	}

	// AND THE OPERATOR LEAVES. A client-side navigation, which is what
	// disposes the owner -- a document load would take the timeout with it and
	// prove nothing.
	navTo(t, ctx, "the queue", queuePanel)
	sameDocument(t, ctx, "leaving the watcher", wasm)

	// SIX SECONDS, which is past the backoff's own five-second cap: whatever
	// was pending has fired by now.
	time.Sleep(6 * time.Second)

	if errs := pageErrors(t, ctx); errs != "" {
		t.Errorf("the page threw after the watcher was left with a re-open pending:\n%s\n"+
			"that is a `StoredValue` read on a disposed owner, which `panic = \"abort\"` "+
			"turns into a trap -- see `Watcher::alive`", errs)
	}

	// AND THE PAGE IS STILL RUNNING, which is a second look at the same
	// question from the other side: a trapped wasm leaves the DOM exactly as
	// it was, so an assertion about what is on screen cannot tell a live page
	// from a dead one -- but a click it has to intercept can, because
	// `preventDefault` happens inside the wasm and a navigation that reached
	// the server would clear the sentinel.
	//
	// NOT CLAIMED AS THE DISCRIMINATOR. A wasm trap does not poison the
	// instance, so whether a later click still works is not something this lab
	// decides; `pageErrors` above is the assertion and this is cheap.
	navTo(t, ctx, "generate", composePanel)
	sameDocument(t, ctx, "navigating after the window closed", wasm)

	// AND THE RE-OPENS STOPPED. A watcher nobody is watching must not go on
	// asking: the loop belongs to a page that is gone, and a timeout that
	// keeps rescheduling is the leak the guard returns early from rather than
	// the trap it prevents.
	after := countedSockets(t, ctx)
	time.Sleep(6 * time.Second)
	if again := countedSockets(t, ctx); again != after {
		t.Errorf("the page opened %d socket(s) after the watcher was left and then %d: a "+
			"request nobody is watching is still being re-opened on a timer", after, again)
	}

	if errs := console.errors(); errs != "" {
		t.Errorf("the page logged errors:\n%s", errs)
	}
}

// breakSockets makes every websocket the page opens from now on fail its
// handshake.
//
// A BROWSER-SIDE INSTRUMENT AND NOT A SEAM IN THE PAGE, which is the line
// `sshx.Runner` is not widened across either -- and it is `recordSockets`'
// own wrapper, with one flag, rather than a second wrapper that would have to
// agree with it about which socket is the page's.
//
// A PATH THAT ANSWERS NO UPGRADE, which is the most deterministic refusal
// available with the server left alone: `/api/` is not an app route, so
// `ember serve` answers a 404 rather than the page, the handshake has no 101
// and the browser calls `onclose`. A closed port would do it too and would
// mean finding one that stays closed for the length of the lab.
func breakSockets(t *testing.T, ctx context.Context) {
	t.Helper()
	got, err := chromedp.Run(ctx, chromedp.Evaluate[string](
		`(function () {
			if (!window.__emberSockets) { return "not wrapped"; }
			window.__emberBreak = "/api/no-such-socket";
			return "broken";
		})()`))
	if err != nil {
		t.Fatalf("breaking the page's websockets: %v", err)
	}
	if got != "broken" {
		t.Fatalf("the socket wrapper reported %q, so the re-opens below are not being "+
			"refused and the backoff window is a race", got)
	}
}

// countedSockets is how many websockets the page has constructed.
func countedSockets(t *testing.T, ctx context.Context) int {
	t.Helper()
	n, err := chromedp.Run(ctx, chromedp.Evaluate[int](
		`(window.__emberSockets || []).length`))
	if err != nil {
		t.Fatalf("counting the page's websockets: %v", err)
	}
	return n
}

// waitForSockets waits until the page has constructed at least that many.
func waitForSockets(t *testing.T, ctx context.Context, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := countedSockets(t, ctx)
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the page has opened %d websocket(s) within %v, want %d: the retry loop "+
				"is not running, so nothing below is about a pending re-open",
				got, within, want)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
