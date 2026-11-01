package webapi

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
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

// builtMarker is a file only `make web` produces.
//
// Its absence is how `ember serve` knows it is serving the placeholder, which
// is better than serving a page that looks broken: an operator who has not
// run `make web` gets a sentence telling them to.
const builtMarker = "ember_bg.wasm"

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
		if _, err := os.Stat(s.opts.Dist + string(os.PathSeparator) + builtMarker); err == nil {
			return true
		}
	}
	_, err := dist.Open("dist/" + builtMarker)
	return err == nil
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
		// A client-rendered app owns its own routes, so anything that is not
		// a file is the app's own path and gets index.html. Without this, a
		// reload on /requests/abc is a 404.
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path, ".") {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}
