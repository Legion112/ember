package loopback

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// netListenFuncs are the package-level entry points in net that can publish a
// port.
var netListenFuncs = map[string]bool{
	"Listen":       true,
	"ListenTCP":    true,
	"ListenIP":     true,
	"ListenUDP":    true,
	"ListenPacket": true,
	"ListenUnix":   true,
}

// httpListenFuncs are net/http's two one-line servers.
//
// They take an address as a *string* and hand it straight to net.Listen, so
// they walk around every refusal in ParseBind: `http.ListenAndServe(":7777",
// h)` publishes an ember web UI -- which can rent GPUs on the operator's
// account -- to the whole LAN, and the line looks entirely ordinary. They are
// listed here because `ember serve` is the first thing in this program that
// serves HTTP, and these are exactly the two lines that are shorter than
// doing it properly.
var httpListenFuncs = map[string]bool{
	"ListenAndServe":    true,
	"ListenAndServeTLS": true,
}

// listenMethods are the methods to refuse on a tracked receiver.
//
// net.ListenConfig and http.Server share the shape: a value whose methods can
// bind an address. (*http.Server).Serve is deliberately absent -- it takes a
// net.Listener, so it can only serve something loopback.Listen already
// proved, which is how cmd/emberwl does it and how ember serve must.
var listenMethods = map[string]map[string]bool{
	"net.ListenConfig": {"Listen": true, "ListenPacket": true},
	"http.Server":      httpListenFuncs,
}

// TestNoListenOutsideLoopback states this package's reason to exist as code.
//
// Every refusal in ParseBind is worth nothing if some other package opens its
// own listener because it was two lines shorter — and that is exactly the
// shortcut a provider package takes when it wants a callback endpoint "just
// for a moment". The rule is therefore not a convention in a document; it is a
// build failure the first time somebody writes net.Listen somewhere else.
//
// It matches on the qualifier, not on the method name. An earlier version
// flagged any selector called Listen, which meant it flagged loopback.Listen
// — the one call it exists to permit. A guard test that fires on correct code
// is a guard test people learn to ignore.
//
// Test files are exempt. They are not part of any binary's dependency
// surface, and httptest binds loopback of its own accord.
func TestNoListenOutsideLoopback(t *testing.T) {
	root := filepath.Join("..", "..")
	thisPkg := filepath.Join("internal", "loopback")
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "testdata":
				return fs.SkipDir
			}
			// The repository's logo directory, matched by its path and not by
			// its name. Skipping any directory *called* assets also skipped
			// internal/assets, which is Go code -- so for the life of this
			// test a whole package was exempt from the one rule it exists to
			// enforce, and nothing said so.
			if rel == "assets" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		if strings.HasPrefix(rel, thisPkg+string(filepath.Separator)) {
			return nil
		}
		if !strings.HasPrefix(rel, "internal"+string(filepath.Separator)) &&
			!strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) &&
			!strings.HasPrefix(rel, "test"+string(filepath.Separator)) {
			return nil
		}
		checked++

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}

		// Variables bound to a type whose methods can bind an address, so
		// lc.Listen(...) and srv.ListenAndServe() are caught as well as
		// net.Listen(...) and http.ListenAndServe(...).
		tracked := trackedVars(f)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qual, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}

			name := sel.Sel.Name
			bad := (qual.Name == "net" && netListenFuncs[name]) ||
				(qual.Name == "http" && httpListenFuncs[name]) ||
				listenMethods[tracked[qual.Name]][name]

			if bad {
				t.Errorf("%s:%d: %s.%s is called outside internal/loopback; route it through loopback.Listen so the address is proved to be loopback first",
					rel, fset.Position(call.Pos()).Line, qual.Name, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}

	// A walk that found nothing would pass for the wrong reason.
	if checked == 0 {
		t.Fatal("inspected no files; the walk is broken, not the module")
	}
}

// trackedVars maps a variable name to the qualified type it was bound to, for
// the types in listenMethods.
//
// Both spellings are recognised, because both appear in real code: `var lc
// net.ListenConfig` is a ValueSpec with a type, and `srv := &http.Server{...}`
// -- which is how every Go program writes a server, cmd/emberwl included --
// is an assignment with a composite literal and no type to read. Catching
// only the first would have let the one case this test was extended for walk
// straight past it.
func trackedVars(f *ast.File) map[string]string {
	out := map[string]string{}

	note := func(names []*ast.Ident, qualified string) {
		if _, ok := listenMethods[qualified]; !ok {
			return
		}
		for _, n := range names {
			out[n.Name] = qualified
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			if q := qualifiedType(v.Type); q != "" {
				note(v.Names, q)
			}
		case *ast.AssignStmt:
			// Positional: the i'th name takes the i'th value's type. A
			// multi-value call on the right has no composite literal to read,
			// so qualifiedType returns "" and nothing is tracked.
			for i, rhs := range v.Rhs {
				if i >= len(v.Lhs) {
					break
				}
				id, ok := v.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if q := compositeType(rhs); q != "" {
					note([]*ast.Ident{id}, q)
				}
			}
		}
		return true
	})
	return out
}

// qualifiedType reads "net.ListenConfig" out of a type expression, through a
// pointer if there is one.
func qualifiedType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return qualifiedType(t.X)
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
	}
	return ""
}

// compositeType reads the type out of `http.Server{...}` or
// `&http.Server{...}`.
func compositeType(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return compositeType(v.X)
		}
	case *ast.CompositeLit:
		return qualifiedType(v.Type)
	}
	return ""
}
