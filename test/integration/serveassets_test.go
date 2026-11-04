//go:build integration && imagelab

package integration

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestServe_UIMountsOnATwoSegmentRoute.
//
// THE DEFECT THIS EXISTS FOR WAS A 200 WITH A BLANK BODY. `ember serve`
// answers the app page on every route the app owns, so a Go test asserting a
// status code -- and `TestStatic_AClientRenderedRouteGetsTheApp` is exactly
// that -- passed against it for the life of the feature. What was broken is
// one step later: web/index.html referenced `style.css` and `./ember.js`
// RELATIVELY, a browser resolves a relative reference against the document's
// own directory, and that directory is `/` for `/` and `/queue` and
// `/requests/` for `/requests/abc`. So the two-segment route -- the example
// static.go's own comment names -- served a page whose every asset 404'd.
//
// AND IT WAS SILENT. A module specifier that 404s throws while the module
// script is being loaded, which is BEFORE the `init().catch` in that page is
// installed, so the one thing written to report a failed start could not run.
// The result is a document that arrived, rendered nothing, and said nothing:
// indistinguishable from a server that is not answering, which is the failure
// mode the whole of that page's error handling exists to avoid.
//
// WHY A BROWSER AND NOT A GO TEST. internal/webapi's own
// `TestPage_LoadsItsOwnAssetsFromEveryRouteItIsServedOn` resolves the page's
// references with net/url and asks the file server for each one, which is the
// cheap half and the one that catches a new asset added relatively. What it
// cannot answer is whether a browser agrees: that the resolution rule really
// is the document's directory, that a root-relative specifier is accepted by
// Chrome's module loader at all, and -- the half with no Go equivalent --
// whether the wasm then starts, since wasm-bindgen's glue asks for
// `new URL('ember_bg.wasm', import.meta.url)` and so follows ember.js rather
// than the page. "The page arrived" and "the app mounted" are different
// claims and only one of them was ever in question.
//
// THE SINGLE-SEGMENT ROUTE IS ASSERTED FIRST, and it is the discriminator
// rather than padding. An app that mounts nowhere -- a broken wasm, a wrong
// `make web`, a page the server never served -- fails the same assertion at
// the same place, so without `/` this lab could not say whether the failure
// was about DEPTH. With it, a pass at `/` and a failure at `/requests/abc` is
// the defect and nothing else.
//
// IT RENTS NOTHING AND GENERATES NOTHING, and since #139 slice 3 the id in
// the path is read: that route is the request page, so this navigation mounts
// a watcher for a request the store does not have. Which is still the right
// shape for this lab and is deliberately NOT what it asserts -- what is in
// question here is whether the page can load ITSELF at that depth, so the
// assertions are the `<h1>`, the posture line and the stylesheet, all of which
// are the header and the document rather than the page under it. Whether a
// deep link renders the request it names is
// TestServe_UINavigatesWithoutReloadingTheApp's, which has a request.
func TestServe_UIMountsOnATwoSegmentRoute(t *testing.T) {
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

	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port)

	// The one-time URL is honoured on every app route, which
	// TestExchange_IsHonouredOnTheAppRouteAndNowhereElse already holds -- so
	// the token on the deep route is not what is in question here, and
	// carrying it on both navigations keeps each one a page load a person
	// could make from the line ember printed.
	//
	// `/requests/<id>` and not some other depth: it is the shape slice 3 is
	// about to route, and the shape every comment about this names. The id is
	// sixteen hex characters because that is what genstore.NewReqID answers,
	// and isAppRoute is keyed on whether the last segment holds a dot -- so a
	// made-up id of another shape would be testing a path ember cannot
	// produce.
	for _, route := range []string{"/", "/requests/3f9a1c20d4e7b865"} {
		url := fmt.Sprintf("%s%s?t=%s", base, route, srv.Token)
		if err := chromedp.Do(ctx, chromedp.Navigate(url)); err != nil {
			t.Fatalf("navigating to %s: %v", route, err)
		}
		console := watchConsole(t, ctx)

		// THE APP MOUNTED, which is the wasm having started -- not the
		// document having arrived. <h1> and .posture are the page's own
		// structure and neither exists in the HTML the server sends: both are
		// rendered by the Leptos app, so either one present means the module
		// loaded, the wasm resolved beside it and the mount ran.
		if err := chromedp.Do(ctx,
			chromedp.WaitVisible(`h1`),
			chromedp.WaitVisible(`.posture`),
		); err != nil {
			t.Fatalf("the app did not mount on %s: %v\nconsole:\n%s\npage:\n%s",
				route, err, console.text(), bodyText(t, ctx))
		}

		// AND THE STYLESHEET RESOLVED, which the mount cannot say. `style.css`
		// was the second relative reference and a fix that made only the
		// module absolute would leave this page unstyled and mounting
		// perfectly -- an assertion that could not tell those apart is the
		// same shape as the status code that could not tell served from
		// loadable.
		//
		// Derived from the document rather than from a filename: every
		// <link rel=stylesheet> the page has must have PARSED, so this stays
		// honest the day somebody adds a second stylesheet and asks nothing
		// the day somebody inlines the only one.
		s := stylesheets(t, ctx)
		if s.Links == 0 {
			t.Errorf("%s: the page has no stylesheet link, so nothing here is "+
				"checking whether one resolves", route)
		}
		if s.Parsed != s.Links {
			t.Errorf("%s: the page links %d stylesheet(s) and %d parsed "+
				"(%d reached document.styleSheets at all); a relative href "+
				"resolves against this route's own directory",
				route, s.Links, s.Parsed, s.Entries)
		}
	}
}

// sheetCount is what the document says about its own stylesheets.
type sheetCount struct {
	// Links is how many <link rel=stylesheet> elements the page has.
	Links int
	// Entries is how many of those reached document.styleSheets.
	Entries int
	// Parsed is how many of those hold at least one rule.
	Parsed int
}

// stylesheets counts a page's stylesheet links, the CSSOM entries they
// produced, and how many of those parsed.
//
// ENTRIES IS NOT THE ANSWER AND THAT IS A MEASUREMENT RATHER THAN A GUESS.
// The first version of this helper compared Links against Entries, on the
// reading that a <link> whose fetch failed produces no document.styleSheets
// entry. Reinstated -- the stylesheet href back to relative, the module left
// absolute, so the app mounts over a 404'd sheet -- that comparison was
// GREEN: Chrome keeps a CSSOM entry for the link whatever the fetch answered,
// so Links == Entries over the exact defect this exists for. An assertion
// that cannot fail is worse than none, which is the whole reason the
// reinstatement runs before the suite is believed.
//
// What does differ is whether anything PARSED. The 404 is net/http's own, a
// text/plain body, so the entry carries no rules -- and `style.css` carries
// about two hundred. Entries is kept and printed because it is the number
// that distinguishes "the link is not in the DOM" from "it is and it did
// nothing", which is a different repair.
//
// cssRules is same-origin here and still read under try/catch: a throw is the
// answer "nothing readable parsed", and letting it escape would report a
// browser-security detail as a lab failure.
func stylesheets(t *testing.T, ctx context.Context) sheetCount {
	t.Helper()
	const count = `(function () {
		const hrefs = [...document.querySelectorAll('link[rel~="stylesheet"]')]
			.map((l) => l.href);
		const mine = [...document.styleSheets]
			.filter((s) => s.href && hrefs.includes(s.href));
		const parsed = mine.filter((s) => {
			try { return s.cssRules.length > 0; } catch (e) { return false; }
		});
		return [hrefs.length, mine.length, parsed.length];
	})()`
	got, err := chromedp.Run(ctx, chromedp.Evaluate[[]int](count))
	if err != nil {
		t.Fatalf("counting the page's stylesheets: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("counting the page's stylesheets answered %v", got)
	}
	return sheetCount{Links: got[0], Entries: got[1], Parsed: got[2]}
}

// bodyText is what a person would see, for a failure message.
//
// The defect this lab is about renders an EMPTY body, so the message has to
// say that rather than leaving a reader to guess whether the page was a
// refusal, a placeholder or nothing at all.
func bodyText(t *testing.T, ctx context.Context) string {
	t.Helper()
	got, err := chromedp.Run(ctx, chromedp.Evaluate[string](
		`(document.body ? document.body.innerText : "(no body)")`))
	if err != nil {
		return fmt.Sprintf("(could not read the body: %v)", err)
	}
	if strings.TrimSpace(got) == "" {
		return "(the body is empty -- which is what a 404'd module import looks like)"
	}
	return got
}
