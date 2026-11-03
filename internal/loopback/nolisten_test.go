package loopback

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// netListenFuncs are the package-level entry points in net that can publish an
// address.
//
// AN ADDRESS AND NOT A PORT, which used to be the wording and became wrong the
// day this package learned to make a unix socket. ListenUnix and
// net.Listen("unix", …) publish a PATH, and a socket in a directory another
// account can write to is a socket another account can unlink and replace with
// their own -- so the CLI client would hand ember's requests, which can rent
// GPUs, to whoever got there first. That is the same failure as a published
// port with a filesystem in place of a network, which is why the ban on these
// two did NOT loosen when loopback.ListenSocket was written: it narrowed to
// "outside this package", exactly as the ban on net.Listen always was, and
// StateSocket is the only thing that can name the path they are given.
//
// The map is keyed on the function name alone, so the network string is not
// read and `net.Listen("unix", p)` is caught by the same entry as
// `net.Listen("tcp", a)`. TestNoListenGuardCatchesEverySpelling is what proves
// each of these is still reported rather than merely listed.
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

		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		found, perr := findListens(src)
		if perr != nil {
			return perr
		}
		for _, f := range found {
			t.Errorf("%s:%d: %s is called outside internal/loopback; route it through loopback.Listen or loopback.ListenSocket so the address is proved to be loopback -- or the socket's directory ember's -- first",
				rel, f.line, f.call)
		}
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

// listen is one reported call: what it was and where.
type listen struct {
	call string
	line int
}

// findListens is the whole of the rule, over one file's source.
//
// EXTRACTED FROM THE WALK SO THE GUARD CAN BE SHOWN TO FAIL. For the life of
// this test the inspection called t.Errorf inline, so the only evidence it
// worked was that the tree was clean -- which is also what a broken matcher
// reports, and this repository has now found four gates that could not fail.
// The walk above reports what this returns; TestNoListenGuardCatchesEverySpelling
// hands it sources that must be caught and sources that must not.
func findListens(src []byte) ([]listen, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return nil, err
	}

	// Variables bound to a type whose methods can bind an address, so
	// lc.Listen(...) and srv.ListenAndServe() are caught as well as
	// net.Listen(...) and http.ListenAndServe(...).
	tracked := trackedVars(f)

	var out []listen
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
			out = append(out, listen{
				call: qual.Name + "." + name,
				line: fset.Position(call.Pos()).Line,
			})
		}
		return true
	})
	return out, nil
}

// TestNoListenGuardCatchesEverySpelling is the exit code the walk above cannot
// produce.
//
// THE POSITIVE HALF IS NOT THE INTERESTING ONE. Every refused spelling below
// is also the spelling somebody reaching for a shortcut writes, and the two
// that matter most are the unix ones: internal/loopback is allowed to make a
// socket now, and the thing that must not have changed is that nothing else
// can. So `net.ListenUnix` and `net.Listen("unix", …)` are pasted here --
// which is what a package wanting its own socket "just for a moment" would
// write -- and both have to come back reported.
//
// AND THE NEGATIVE CONTROL IS WHY THIS IS NOT A TEST THAT CANNOT FAIL. A
// matcher that reported every selector called Listen would satisfy every case
// above it and is exactly what the first version of this guard did: it flagged
// loopback.Listen, the one call it exists to permit. So the correct spellings
// are run through the same function and must come back clean -- a guard that
// fires on correct code is a guard people learn to ignore, which is the same
// failure as one that cannot fire at all.
func TestNoListenGuardCatchesEverySpelling(t *testing.T) {
	caught := []struct {
		name, body, want string
	}{
		{"net.Listen", `net.Listen("tcp", ":8188")`, "net.Listen"},
		{"net.Listen over a unix path", `net.Listen("unix", p)`, "net.Listen"},
		{"net.ListenUnix", `net.ListenUnix("unix", a)`, "net.ListenUnix"},
		{"net.ListenTCP", `net.ListenTCP("tcp", a)`, "net.ListenTCP"},
		{"net.ListenIP", `net.ListenIP("ip", a)`, "net.ListenIP"},
		{"net.ListenUDP", `net.ListenUDP("udp", a)`, "net.ListenUDP"},
		{"net.ListenPacket", `net.ListenPacket("udp", ":0")`, "net.ListenPacket"},
		{"http.ListenAndServe", `http.ListenAndServe(":7777", h)`, "http.ListenAndServe"},
		{"http.ListenAndServeTLS", `http.ListenAndServeTLS(":7777", c, k, h)`, "http.ListenAndServeTLS"},
		{"a tracked ListenConfig", "var lc net.ListenConfig\n\tlc.Listen(ctx, \"unix\", p)", "lc.Listen"},
		{"a tracked http.Server", "srv := &http.Server{}\n\tsrv.ListenAndServe()", "srv.ListenAndServe"},
	}
	for _, c := range caught {
		t.Run(c.name, func(t *testing.T) {
			got, err := findListens([]byte("package p\n\nfunc f() {\n\t" + c.body + "\n}\n"))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) != 1 || got[0].call != c.want {
				t.Fatalf("findListens = %v, want exactly one %s", got, c.want)
			}
		})
	}

	clean := []struct{ name, body string }{
		// The two calls this package exists to be, named on the package that
		// owns them. The first version of this guard flagged the first of
		// these.
		{"loopback.Listen", `loopback.Listen(ctx, b)`},
		{"loopback.ListenSocket", `loopback.ListenSocket(ctx, s)`},
		// Serving a listener somebody else proved is the whole arrangement
		// webapi and cmd/emberwl use.
		{"a server serving a proved listener", "srv := &http.Server{}\n\tsrv.Serve(ln)"},
		// A method called Listen on something that is not a listener type.
		{"an unrelated Listen", `k.Listen(ctx)`},
		// And a dial, which is outbound and not this package's business.
		{"net.Dial", `net.Dial("unix", p)`},
	}
	for _, c := range clean {
		t.Run("not "+c.name, func(t *testing.T) {
			got, err := findListens([]byte("package p\n\nfunc f() {\n\t" + c.body + "\n}\n"))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("findListens reported %v on correct code", got)
			}
		})
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
