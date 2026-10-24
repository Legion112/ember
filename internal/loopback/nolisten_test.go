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

// listenFuncs are the standard-library entry points that can publish a port.
var listenFuncs = map[string]bool{
	"Listen":       true, // net.Listen, and (*net.ListenConfig).Listen
	"ListenTCP":    true,
	"ListenIP":     true,
	"ListenUDP":    true,
	"ListenPacket": true, // net.ListenPacket and (*net.ListenConfig).ListenPacket
	"ListenUnix":   true,
}

// TestNoListenOutsideLoopback states this package's reason to exist as code.
//
// Every refusal in ParseBind is worth nothing if some other package opens its
// own listener because it was two lines shorter than routing through here —
// and that is exactly the shortcut a provider package takes when it wants a
// callback endpoint "just for a moment". The rule is therefore not a
// convention in a document; it is a build failure the first time somebody
// writes net.Listen somewhere else.
//
// Test files are exempt. They are not part of any binary's dependency surface,
// and httptest binds loopback of its own accord.
func TestNoListenOutsideLoopback(t *testing.T) {
	root := filepath.Join("..", "..")
	thisPkg := filepath.Join("internal", "loopback")

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "testdata", "node_modules":
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

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !listenFuncs[sel.Sel.Name] {
				return true
			}
			// net.Listen(...) and lc.Listen(...) both land here. Both are the
			// thing being forbidden, so neither is unwrapped further.
			t.Errorf("%s:%d: %s is called outside internal/loopback; route it through loopback.Listen so the address is proved to be loopback first",
				rel, fset.Position(call.Pos()).Line, render(sel))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
}

func render(sel *ast.SelectorExpr) string {
	if x, ok := sel.X.(*ast.Ident); ok {
		return x.Name + "." + sel.Sel.Name
	}
	return sel.Sel.Name
}
