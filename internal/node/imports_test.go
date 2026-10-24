package node

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/Legion112/ember/"

// TestNodeImportsNothing states this package's defining rule as code.
//
// node is the domain core. The moment it imports a sibling, the sibling's
// dependencies become its dependencies, and a package that cannot be tested
// without Docker or a network is a package whose invariants stop being checked
// on every run. transcription/AGENTS.md records the same rule for
// internal/transcript and the reason it was written down: it is the thing that
// keeps the pure layer pure, and nothing enforces it except a test.
//
// Test files are exempt — they are not part of the package's dependency
// surface and never reach a binary.
//
// Files are parsed one at a time rather than with parser.ParseDir, which is
// deprecated as of Go 1.25 because it ignores build tags when grouping files
// into packages. Here that would matter the day this package grows a
// //go:build file: ParseDir could file it under a package name this loop never
// visits, and the rule would quietly stop being checked.
func TestNodeImportsNothing(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, modulePath) {
				t.Errorf("%s imports %s; internal/node must depend on nothing in this module", name, path)
			}
		}
	}

	// A version of this test that found no files would pass for the wrong
	// reason, which is the failure mode every source-walking test has.
	if checked == 0 {
		t.Fatal("parsed no non-test files; the walk is broken, not the package")
	}
}
