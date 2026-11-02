package logx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// handlerFuncs are the slog entry points that decide a format.
//
// slog.New is deliberately absent. `slog.New(slog.DiscardHandler)` is the
// fallback seven library packages use to stay silent when no logger was handed
// to them -- it decides neither a level nor a format, and banning it would
// mean every one of those packages had to import this one to say nothing.
var handlerFuncs = map[string]bool{
	"NewTextHandler": true,
	"NewJSONHandler": true,
}

// TestNoHandlerOutsideLogx states this package's reason to exist as code.
//
// The handler was built in two places with the same literal, and both pinned
// the level to info -- so seventeen debug calls in this tree could not be
// displayed by any invocation of ember that shipped. Changing ember's log
// format meant finding every place that had built a handler, and the second
// place is the one that gets missed: AGENTS.md records the same failure for
// the journal's "nodes" subdirectory, which was spelled correctly in both
// copies until it wasn't.
//
// So the rule is a build failure rather than a line in a document. It matches
// on the qualifier as well as the name, the correction
// TestNoListenOutsideLoopback needed: an earlier version flagged any selector
// called NewTextHandler and therefore flagged logx.New's own call, which is
// the one call it exists to permit.
//
// Test files are exempt. A test that wants to read what was logged has to
// build a handler over a buffer, nine of them do, and none of them is part of
// any binary's dependency surface.
func TestNoHandlerOutsideLogx(t *testing.T) {
	checked := 0
	walkModule(t, func(rel string, fset *token.FileSet, f *ast.File) {
		if rel == filepath.Join("internal", "logx", "logx.go") {
			return
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
					if q, ok := sel.X.(*ast.Ident); ok && q.Name == "slog" && handlerFuncs[sel.Sel.Name] {
						t.Errorf("%s:%d: slog.%s is called outside internal/logx; "+
							"build the logger with logx.New so one place decides ember's level and format",
							rel, fset.Position(v.Pos()).Line, sel.Sel.Name)
					}
				}
			case *ast.CompositeLit:
				// &slog.HandlerOptions{Level: ...} is the other half of the
				// decision, and a file holding one is a file that has an
				// opinion about the level even if it hands the struct
				// somewhere else to be used.
				if sel, ok := v.Type.(*ast.SelectorExpr); ok {
					if q, ok := sel.X.(*ast.Ident); ok && q.Name == "slog" && sel.Sel.Name == "HandlerOptions" {
						t.Errorf("%s:%d: a slog.HandlerOptions literal outside internal/logx; "+
							"the level is logx.Options.Level and is decided in one place",
							rel, fset.Position(v.Pos()).Line)
					}
				}
			}
			return true
		})
	})
	if checked == 0 {
		t.Fatal("inspected no files; the walk is broken, not the module")
	}
}

// logMethods are the calls that can put a value in front of a reader.
//
// Over-broad on purpose: `t.Error` and `http.Error` match too. That costs
// nothing, because the rule below only fires when such a call also unwraps key
// material, and a line doing that is a line to rewrite wherever it is.
var logMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true, "With": true,
}

// TestNoKeyMaterialInALogCall is the seal that had to land with a reachable
// debug level, not after it.
//
// node.Secret exists because "one slog.String("hostkey", string(pem)) at debug
// level writes a private key into the journal permanently, and nothing about
// that line looks wrong in review". Every such line in this tree was harmless
// while no shipped invocation of ember could display a debug record. Adding
// --log-level debug removes that accident, so the discipline stops being a
// property of the handler and becomes a property of the call sites -- and a
// property of call sites is what a walk can check.
//
// THE RULE IS THE AUDIT HANDLE node.Secret ALREADY NAMES. Secret.Bytes() is
// documented as the one visible unwrapping, "so that an audit for places key
// material is handled greps for .Bytes() and finds all of them"; cmd/embernode
// repeats the shape for the PEM it reads out of its environment, because it
// cannot import internal/node. Logging the wrapper itself is safe and stays
// legal -- Secret implements slog.LogValuer and prints a fingerprint -- so what
// is forbidden is precisely the spelling that defeats it.
//
// WHAT IT CANNOT SEE, said plainly because a guard whose limits are not
// written down gets believed past them: an unwrapping laundered through a
// helper that merely BUILDS the attributes and returns them, since the match
// is on a log call's own argument list. cmd/embernode had exactly that for an
// hour -- a keyAttrs() helper, which reads better -- and the reinstatement
// pass found this walk green with `string(...Bytes())` sitting in it. The two
// keys are spelled inline at the log call there now, and cmd/embernode's own
// keys_test.go asserts on the OUTPUT rather than on the shape, which is the
// check that holds whatever the spelling.
//
// It is NOT a substring filter over message text. That is the tool AGENTS.md
// rejects for rejectSecrets, which deliberately does not scan `cmd` or `cmds`
// because a prompt is arbitrary English: "a token of affection" is a
// legitimate prompt. This matches on the shape of the expression instead.
func TestNoKeyMaterialInALogCall(t *testing.T) {
	checked := 0
	walkModule(t, func(rel string, fset *token.FileSet, f *ast.File) {
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !logMethods[sel.Sel.Name] {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(a ast.Node) bool {
					inner, ok := a.(*ast.CallExpr)
					if !ok {
						return true
					}
					if s, ok := inner.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Bytes" && len(inner.Args) == 0 {
						t.Errorf("%s:%d: .Bytes() unwraps key material inside a %s call; "+
							"log the node.Secret itself -- it is a slog.LogValuer and prints a fingerprint",
							rel, fset.Position(inner.Pos()).Line, sel.Sel.Name)
					}
					return true
				})
			}
			return true
		})
	})
	if checked == 0 {
		t.Fatal("inspected no files; the walk is broken, not the module")
	}
}

// walkModule parses every non-test Go file under internal/, cmd/ and test/.
//
// Shared by the two walks above rather than written twice, which is the defect
// this package is about. The skip list is loopback/nolisten_test.go's,
// including its correction: the logo directory is matched by its path and not
// by its name, because skipping any directory *called* assets also skipped
// internal/assets -- a whole package exempt from the one rule it was meant to
// obey, with nothing saying so.
func walkModule(t *testing.T, fn func(rel string, fset *token.FileSet, f *ast.File)) {
	t.Helper()
	root := filepath.Join("..", "..")

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
			case ".git", "bin", "testdata", "web":
				return fs.SkipDir
			}
			if rel == "assets" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, top := range []string{"internal", "cmd", "test"} {
			if rel == top || strings.HasPrefix(rel, top+string(filepath.Separator)) {
				fset := token.NewFileSet()
				f, perr := parser.ParseFile(fset, path, nil, 0)
				if perr != nil {
					return perr
				}
				fn(rel, fset, f)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
}
