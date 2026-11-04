package webapi

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// dist is the built UI.
//
// `all:` so a file whose name begins with a dot or an underscore is included
// too -- wasm-bindgen does not produce one today, and the pattern that
// silently drops files is not the one to rely on.
//
// The directory holds a committed placeholder for a reason worth repeating
// here: //go:embed fails at COMPILE time when its pattern matches nothing, so
// an empty dist would break `go build ./...` and every CI step for anyone who
// had not first run a Rust build. The Go toolchain does not depend on the Rust
// one.
//
//go:embed all:dist
var dist embed.FS

// builtFiles are what `make web` produces and the committed tree does not.
//
// BOTH, because they can disagree and a test caught them doing it: Built()
// keyed on the wasm while the page keyed on index.html, so a tree with one and
// not the other reported itself built and then served the placeholder --
// telling the operator nothing was built while claiming it was. The page is
// what a browser loads and the module is what the page loads, so a build is
// only a build when there are both.
var builtFiles = []string{"index.html", "ember_bg.wasm"}

// placeholderName is the committed page, served when nothing has been built.
//
// NOT index.html, and that is the point. `make web` writes a real index.html
// into this directory, so if the placeholder had that name then building the
// UI would leave the repository dirty and every test about the placeholder
// would pass or fail depending on whether somebody had run `make web` --
// which is the "behaves differently from one machine to the next" problem
// AGENTS.md is written against. The built files are gitignored; this one is
// committed.
const placeholderName = "placeholder.html"

// static serves the UI.
//
// From disk when Dist is set, which is the development loop: edit, rebuild the
// wasm, reload -- with no Go rebuild in between. Through an os.Root, so a
// symlink in that directory cannot serve a file from outside it. The operator
// named the directory, so this is not a trust boundary; it is one containment
// discipline in the program rather than one for untrusted paths and a
// different one for paths that are currently trusted.
func (s *Server) static() http.Handler {
	if s.opts.Dist != "" {
		root, err := os.OpenRoot(s.opts.Dist)
		if err != nil {
			s.log().Error("could not open the -dist directory; serving the built-in UI instead",
				"dist", s.opts.Dist, "err", err)
		} else {
			return s.serveFS(root.FS())
		}
	}
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: the embed above guarantees the directory. Reported
		// rather than ignored, because a UI that silently 404s looks like a
		// server that is not running.
		s.log().Error("the embedded UI is unreadable", "err", err)
		return http.NotFoundHandler()
	}
	return s.serveFS(sub)
}

// Built reports whether a real UI was built into this binary, or is on disk
// where -dist points.
//
// `ember serve` prints a different line when it is false, so an operator sees
// "run make web" rather than a page they have to interpret.
func (s *Server) Built() bool {
	if s.opts.Dist != "" {
		if allPresent(func(name string) error {
			_, err := os.Stat(filepath.Join(s.opts.Dist, name))
			return err
		}) {
			return true
		}
		// A -dist that exists but holds no build falls back to the embedded
		// copy, so the answer is about that one.
	}
	return allPresent(func(name string) error {
		f, err := dist.Open("dist/" + name)
		if err != nil {
			return err
		}
		return f.Close()
	})
}

func allPresent(stat func(string) error) bool {
	for _, name := range builtFiles {
		if stat(name) != nil {
			return false
		}
	}
	return true
}

func (s *Server) serveFS(f fs.FS) http.Handler {
	files := http.FileServerFS(f)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The same guard the API gets. A page on another site cannot read
		// this anyway -- it is a script and a wasm blob -- but there is no
		// reason to answer a request that did not come from here, and one
		// seam is easier to reason about than two.
		if err := guard(r, s.port); err != nil {
			s.fail(w, http.StatusForbidden, err)
			return
		}
		// THE ONE PLACE A TOKEN MAY ARRIVE IN A URL, and only on the app's
		// own route -- an allow-list of exactly one path shape rather than a
		// query parameter the whole server honours.
		//
		// This is also why the page itself is behind the credential rather
		// than served to anyone: without an exchange, the one route a browser
		// navigates to would be the one route with no check on it, and the
		// token would have to reach the page through a fragment and be
		// replayed by script on every call. The server consuming it here
		// keeps the whole surface gated and the page unchanged.
		if isAppRoute(r.URL.Path) && s.exchange(w, r) {
			return
		}
		if err := s.authenticate(r); err != nil {
			s.refuseUI(w, r)
			return
		}
		// NOTHING HERE IS CACHED, and that is a decision rather than
		// laziness. `make web` writes ember.js and ember_bg.wasm under names
		// that do not change between builds, so a browser holding the
		// previous ones serves the previous app -- and the operator reloads,
		// sees no change, and concludes the build did nothing. That happened
		// while this UI was being written, twice, and cost more time than
		// re-fetching a megabyte over loopback ever will.
		//
		// The alternative is hashing the filenames, which means the page has
		// to be generated rather than copied, which means a template, for an
		// asset that travels zero network hops.
		w.Header().Set("Cache-Control", "no-store")

		// A client-rendered app owns its own routes, so anything that is not
		// a file is the app's own path. Without this, a reload on
		// /requests/abc is a 404.
		//
		// AND WITH IT, FOR THE LIFE OF THIS HANDLER, IT WAS A 200 THE PAGE
		// COULD NOT LOAD. One page is answered on every depth, and a browser
		// resolves a relative reference against the document's own directory
		// -- so index.html's `style.css` and `./ember.js` became
		// /requests/style.css and /requests/ember.js, which this file server
		// correctly does not have. Only below the root, so `/` and /queue
		// worked; and silent, because a module specifier that 404s
		// throws before the page's own catch is installed, leaving a blank
		// body with no message. The references are root-relative now and
		// web/index.html's comment carries the argument. Nothing here can
		// hold it: a server cannot know which of a page's URLs were meant to
		// be resolved against the document.
		if isAppRoute(r.URL.Path) {
			s.serveApp(w, r, f)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// isAppRoute is a path the client-rendered app owns rather than a file.
//
// Factored out because the token exchange is scoped to exactly this set: the
// routes a browser navigates to, and not /ember.js or an image.
func isAppRoute(p string) bool {
	return p == "/" || !strings.Contains(path.Base(p), ".")
}

// refuseUI answers a browser that arrived without the credential.
//
// PLAIN TEXT AND NOT THE JSON the API routes answer with, and not HTML
// either. This is the response an operator sees when they type
// localhost:7777 from memory or open a bookmark from before a restart, and a
// one-line JSON object rendered raw in a browser reads as "ember is broken"
// -- which is the failure mode that gets a control turned off rather than
// used. Plain text renders readably in every browser and carries no markup to
// get wrong.
//
// The status is the same 403 every other refusal gives, and the body is
// errNoCredential's own sentence: one message for a missing token, a wrong
// one and a stale cookie alike.
func (s *Server) refuseUI(w http.ResponseWriter, r *http.Request) {
	s.log().Warn("refused a page load that carried no credential this server accepts",
		"path", r.URL.Path)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	if _, err := io.WriteString(w, "ember serve\n\n"+errNoCredential.Error()+"\n"); err != nil {
		s.log().Debug("could not write the refusal", "err", err)
	}
}

// serveApp writes the page: what `make web` built, or the committed
// placeholder when nothing has.
func (s *Server) serveApp(w http.ResponseWriter, r *http.Request, f fs.FS) {
	for _, name := range []string{"index.html", placeholderName} {
		b, err := fs.ReadFile(f, name)
		if err != nil {
			continue
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, werr := w.Write(b); werr != nil {
			s.log().Debug("could not write the page", "err", werr)
		}
		return
	}
	http.NotFound(w, r)
}
