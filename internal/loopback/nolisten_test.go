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

// configListenMethods are the methods on net.ListenConfig.
var configListenMethods = map[string]bool{"Listen": true, "ListenPacket": true}

// TestNoListenOutsideLoopback states this package's reason to exist as code.
//
// Every refusal in ParseBind is worth nothing if some other package opens its
// own listener because it was two lines shorter than routing through here —
// and that is exactly the shortcut a provider package takes when it wants a
// callback endpoint "just for a moment". The rule is therefore not a
// convention in a document; it is a build failure the first time somebody
// writes net.Listen somewhere else.
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
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "testdata", "assets":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
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

		// Variables declared as net.ListenConfig, so lc.Listen(...) is caught
		// as well as net.Listen(...).
		configs := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			sel, ok := vs.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ListenConfig" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "net" {
				for _, name := range vs.Names {
					configs[name.Name] = true
				}
			}
			return true
		})

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
			bad := (qual.Name == "net" && netListenFuncs[sel.Sel.Name]) ||
				(configs[qual.Name] && configListenMethods[sel.Sel.Name])
			if bad {
				t.Errorf("%s:%d: %s.%s is called outside internal/loopback; route it through loopback.Listen so the address is proved to be loopback first",
					rel, fset.Position(call.Pos()).Line, qual.Name, sel.Sel.Name)
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
