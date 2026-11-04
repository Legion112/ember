package webapi

import (
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pageRefs are the asset references web/index.html makes.
//
// A REGEX AND NOT A PARSE, which is the one place in this file that argues
// against the house rule, so the argument is written down. The standard
// library has no HTML parser and golang.org/x/net/html is a dependency
// AGENTS.md's list would have to be amended for -- for a thirty-line page
// this repository writes itself and whose two reference shapes are these two
// alternatives. What a parser would buy is tolerance of markup nobody here
// writes; what it costs is a module.
//
// TWO ALTERNATIVES AND TWO FLOORS BELOW, because they are two different
// mechanisms and a regex that lost one would otherwise pass on the other:
// an element's href/src is resolved by the browser against the document, and
// a module specifier is resolved by the module loader against the same URL.
// Both were relative and both 404'd on a two-segment route.
var pageRefs = regexp.MustCompile(`(?:href|src)="([^"]*)"|\bfrom\s+'([^']*)'`)

// WHAT THESE TESTS DO NOT SEE, named rather than left to be assumed: the
// references the page RENDERS but does not write. The gallery's <img src> and
// <a href> and the preview's <img src> (web/src/views.rs) are built by the
// SERVER -- wire.go's imageRef.Path is
// fmt.Sprintf("/api/requests/%s/images/%d/%d%s", ...), leading slash and all
// -- so they are root-relative by construction and no page this parses
// mentions them. A reference composed in Rust is out of reach of a Go test
// over one HTML file, which is a fact about where the join is and not a hole
// in the parse.
//
// appRoutes are the path shapes a browser navigates to, by segment count.
//
// THE DEFECT WAS ONLY BELOW THE ROOT AND A STATUS CODE COULD NOT SEE IT.
// isAppRoute answers the page on every one of these, so a test that asserted
// the page arrived passed against the bug -- and a browser then resolves
// `style.css` against the document's own DIRECTORY, which is "/" for the
// first two and "/requests/" and "/requests/abc/" for the rest. The page was
// served and could not load itself: a blank body, with no message, because an
// import that 404s throws before the inline catch is installed.
//
// `/requests/abc` is the example static.go's own comment names, and the
// three-segment entry is there because the rule is about depth rather than
// about one path: a fix that special-cased two segments would pass the row
// above it.
var appRoutes = []string{"/", "/queue", "/requests/abc", "/requests/abc/images"}

// TestPage_LoadsItsOwnAssetsFromEveryRouteItIsServedOn.
//
// THE REFERENCES ARE READ OUT OF THE PAGE, never listed here. A list would
// be the hand-maintained inventory AGENTS.md refuses in four other places:
// it passes unchanged the day somebody adds a favicon, a manifest or a font
// and references it relatively, which is the same defect with a different
// filename.
//
// The page is served from a Dist this test writes, so the answer does not
// depend on whether anybody ran `make web` -- the "behaves differently from
// one machine to the next" problem the placeholder exists to avoid. What it
// writes is web/index.html, the committed source `make web` copies, plus a
// stand-in at the path of every reference the page names: an absolute
// reference is a claim that the file server has that file, so the stand-in
// goes where the reference says it is.
func TestPage_LoadsItsOwnAssetsFromEveryRouteItIsServedOn(t *testing.T) {
	dir := distFromCommittedPage(t)
	h := newHarness(t, func(o *Options) { o.Dist = dir })

	for _, route := range appRoutes {
		w := h.do(t, "GET", route, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want the page", route, w.Code)
		}
		base, err := url.Parse(route)
		if err != nil {
			t.Fatal(err)
		}
		refs := refsIn(t, w.Body.String())
		for _, ref := range refs {
			u, err := url.Parse(ref)
			if err != nil {
				t.Fatalf("GET %s: reference %q does not parse: %v", route, ref, err)
			}
			// What a browser asks for: the reference resolved against the
			// document's own URL, by net/url rather than by hand.
			got := base.ResolveReference(u)
			a := h.do(t, "GET", got.Path, "")
			if a.Code != http.StatusOK {
				t.Errorf("the page served on %s references %q, which resolves to %s and answers %d",
					route, ref, got.Path, a.Code)
			}
		}
	}
}

// TestPage_ReferencesEveryAssetAbsolutely is the same property read off the
// committed file instead of off the wire, and it is the one that names the
// fix.
//
// BOTH, because they fail differently. The test above is the behaviour and
// would still pass the day isAppRoute stops answering two-segment paths at
// all; this one is the rule, and it names what to change in the one file that
// has to change. A relative reference here is a 404 on exactly the routes
// slice 3 is about to add, which no status code anywhere will report.
//
// A PREFIX CHECK WAS NOT THE RULE, AND IT PASSED A THIRD-PARTY SCRIPT.
// `strings.HasPrefix(ref, "/")` is true of `//evil.example/analytics.js` --
// protocol-relative, off-origin, and the one thing this page's own comment
// forbids by name: "No CDN and no remote font [...] a script from somewhere
// else would be a third party in a tool whose whole posture is that only sshd
// is reachable". So the rule as written was weaker than the rule the page
// states. This package has already been bitten by exactly that spelling --
// auth.go's redirect target records `//evil.example` surviving a path-only
// check -- which is why the test asks `net/url` instead of asking the string:
// a reference with a Scheme or a Host is not this server's to serve, whatever
// its first character is.
func TestPage_ReferencesEveryAssetAbsolutely(t *testing.T) {
	for _, ref := range refsIn(t, committedPage(t)) {
		u, err := url.Parse(ref)
		if err != nil {
			t.Errorf("web/index.html references %q, which does not parse: %v", ref, err)
			continue
		}
		if u.Scheme != "" || u.Host != "" {
			t.Errorf("web/index.html references %q, which is off this origin "+
				"(scheme %q, host %q); nothing this page loads may come from "+
				"anywhere but `ember serve` itself.", ref, u.Scheme, u.Host)
			continue
		}
		if !strings.HasPrefix(u.Path, "/") {
			t.Errorf("web/index.html references %q relatively; a page served on "+
				"/requests/abc resolves it against /requests/ and gets a 404. "+
				"Write it as %q.", ref, "/"+strings.TrimPrefix(u.Path, "./"))
		}
	}
}

// TestPlaceholder_ReferencesNothingAtAll, because it is served on the same
// routes and has the same browser resolving its URLs.
//
// IT IS SELF-CONTAINED TODAY and that is the only reason it was never part
// of the defect: an inline <style>, no script, nothing to resolve. So the
// assertion is the absence rather than the leading slash -- the two tests
// above cannot read this file, since it names no module and would fatal at
// their floor, and a `<link href="style.css">` added here would be the same
// 404 on /requests/abc with nothing reporting it.
func TestPlaceholder_ReferencesNothingAtAll(t *testing.T) {
	b, err := dist.ReadFile("dist/" + placeholderName)
	if err != nil {
		t.Fatal(err)
	}
	if ms := pageRefs.FindAllString(string(b), -1); len(ms) != 0 {
		t.Errorf("%s references %v; it is served on /requests/abc too, so "+
			"each one has to be root-relative", placeholderName, ms)
	}
}

// refsIn is every asset reference in a page, with a floor per mechanism.
//
// THE FLOORS ARE WHY THIS IS A FUNCTION. A regex that matched nothing -- the
// markup moving to single quotes, an import rewritten as a bare specifier --
// turns both tests above green over an unread page, which is the
// AssertNoStray shape reached through a parse. So a page that names no
// stylesheet, or no module, fails here: `web/index.html` is structurally a
// stylesheet plus a module, and that is the honest bound those two shapes
// allow rather than a census of filenames.
func refsIn(t *testing.T, page string) []string {
	t.Helper()
	var refs []string
	var attrs, modules int
	for _, m := range pageRefs.FindAllStringSubmatch(page, -1) {
		switch {
		case m[1] != "":
			attrs++
			refs = append(refs, m[1])
		case m[2] != "":
			modules++
			refs = append(refs, m[2])
		}
	}
	if attrs == 0 {
		t.Fatalf("parsed no href/src out of the page, so nothing below is "+
			"checking anything:\n%s", page)
	}
	if modules == 0 {
		t.Fatalf("parsed no module import out of the page, so nothing below is "+
			"checking the reference that decides whether the app starts:\n%s", page)
	}
	return refs
}

// committedPage is web/index.html: the source of record, which `make web`
// copies into dist beside the module it generates.
func committedPage(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// distFromCommittedPage builds what `make web` leaves behind, deterministically.
//
// The page is the committed one and every other file is a stand-in, because
// what is under test is which URL a reference resolves to and not what the
// bytes at it are. ember_bg.wasm is written whether or not the page names it:
// wasm-bindgen's glue resolves it against its own module URL rather than the
// document's, so it is not a reference this test can see, and Built() is
// about both files.
func distFromCommittedPage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	page := committedPage(t)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range builtFiles {
		if name == "index.html" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("\x00asm"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range refsIn(t, page) {
		u, err := url.Parse(ref)
		if err != nil {
			t.Fatalf("reference %q does not parse: %v", ref, err)
		}
		// AN OFF-ORIGIN REFERENCE GETS NO STAND-IN, which is what stops the
		// behaviour test being satisfied by one. `//evil.example/analytics.js`
		// has Path "/analytics.js", so writing a file there would have made
		// the loop above ask this server for a path it answers 200 and call a
		// third-party script loadable. Nothing local stands in for a host
		// ember does not serve.
		if u.Scheme != "" || u.Host != "" {
			continue
		}
		// Where the file server would have to hold it. Cleaned against "/" so
		// a reference cannot name a path outside the directory this writes.
		rel := strings.TrimPrefix(path.Clean("/"+u.Path), "/")
		if rel == "" || rel == "." {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("/* stand-in */"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
