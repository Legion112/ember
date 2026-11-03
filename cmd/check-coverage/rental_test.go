package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// WHAT RENTS, AND THE ONE PACKAGE THAT MAY NAME IT.
//
// `ember serve` holds one rental at a time, and the whole of that invariant is
// that there is one drain goroutine and a rental lives inside one iteration of
// its loop -- so two concurrent rentals cannot be expressed rather than being
// refused by a counter. internal/queue's own direct_test.go asserts that
// property where it can be observed: a submitted run waits for the loop, the
// maximum number of open rentals is one, and the run executes on the loop's
// own goroutine stack.
//
// WHAT THAT CANNOT SEE is a SECOND PLACE a rental could start. #146 adds a
// privileged HTTP route that starts runs, and the whole reason it is safe is
// that internal/webapi has no way to name a rental: its seam is one interface
// with one method, and the implementation is beside the drain loop. That is a
// fact about the import graph, and a fact about an import graph is exactly
// what a guard can hold.
//
// provision.WithNode IS THE CALL. Everything that rents goes through it --
// app.Plan.Do, app.Plan.SyncAssets and app.Plan.WithServed are its three
// callers -- and it is the function whose deferred destroy on a context that
// survives cancellation is why no path out of a rental leaves a machine
// running. A package that called it would be a package that could rent, and
// the second one to do so would be the second GPU.
//
// SO THE ALLOW-LIST IS ONE PACKAGE, which is what makes this cheap enough to
// be worth having. This repository refuses a guard whose exception list is the
// whole difficulty -- the lowercase-error check was measured as needing nine
// words and three edits in 164 commits -- and the measurement here is the
// opposite: one entry, true since the function was written, and the thing it
// would catch is a paste into a handler.
//
// A PARSE AND NOT A GREP, for ParseBind's reason one layer up: `grep
// WithNode` over this tree also matches the three comments in internal/queue
// and internal/webapi that name it as the thing their invariant rests on, and
// a guard that fired on a comment arguing for it would be deleted in the first
// week.
func TestNothingRentsOutsideInternalApp(t *testing.T) {
	// The one package that may rent, with the reason, printed on every run so
	// it appears in CI logs and review diffs rather than living unread.
	const allowed = "internal/app"
	t.Logf("rental: provision.WithNode may be called from %s -- "+
		"Plan.Do, Plan.SyncAssets and Plan.WithServed are the three rentals in this program", allowed)

	root := repoRootFor(t)
	found := callsToWithNode(t, root)
	if len(found) == 0 {
		// A guard that reports nothing because it can no longer see anything
		// is the shape this repository keeps finding: the selector may have
		// been renamed, or this test may be walking the wrong tree.
		t.Fatal("found no call to provision.WithNode anywhere, so this guard is looking at " +
			"the wrong thing: app.Plan.Do, Plan.SyncAssets and Plan.WithServed all call it")
	}

	pkgs := map[string][]string{}
	for file := range found {
		pkgs[filepath.ToSlash(filepath.Dir(file))] = append(pkgs[filepath.ToSlash(filepath.Dir(file))], file)
	}
	names := make([]string, 0, len(pkgs))
	for p := range pkgs {
		names = append(names, p)
	}
	sort.Strings(names)

	for _, p := range names {
		if p == allowed {
			continue
		}
		sort.Strings(pkgs[p])
		t.Errorf("%s calls provision.WithNode (%s), and only %s may.\n"+
			"Renting is what the drain loop serialises: a second place a rental can start is a "+
			"second GPU, which is the invariant internal/queue is built on. If this is a new kind "+
			"of run, hand it to queue.Queue.Submit rather than renting beside the loop.",
			p, strings.Join(pkgs[p], ", "), allowed)
	}
}

// callsToWithNode finds every production call of provision.WithNode.
//
// Test files are out of scope on purpose: a test that rents is a lab, and the
// labs rent through `ember` as a subprocess rather than by calling this.
func callsToWithNode(t *testing.T, root string) map[string]int {
	t.Helper()
	found := map[string]int{}

	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			t.Fatalf("stat %s: %v", base, err)
		}
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path) //nolint:gosec // walking this repository's own tree
			if err != nil {
				return err
			}
			// Comments are DISCARDED rather than filtered, which is the whole
			// difference from a grep: three comments in this tree name
			// provision.WithNode as the thing their invariant rests on, and a
			// guard that fired on the argument for a rule is a guard somebody
			// removes.
			f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "WithNode" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "provision" {
					return true
				}
				found[filepath.ToSlash(rel)]++
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return found
}

// repoRootFor is this repository's root, from the test's own location.
func repoRootFor(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected %s to be the module root: %v", root, err)
	}
	return root
}
