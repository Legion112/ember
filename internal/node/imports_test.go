package node

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

const modulePath = "github.com/Legion112/ember/"

// TestNodeImportsNothing states this package's defining rule as code.
//
// node is the domain core. The moment it imports a sibling, the sibling's
// dependencies become its dependencies, and a package that cannot be tested
// without Docker or a network is a package whose invariants stop being
// checked on every run. transcription/AGENTS.md records the same rule for
// internal/transcript and the reason it was written down: it is the thing that
// keeps the pure layer pure, and nothing enforces it except a test.
//
// Test files are exempt — they are not part of the package's dependency
// surface and never reach a binary.
func TestNodeImportsNothing(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse this package: %v", err)
	}

	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if strings.HasPrefix(path, modulePath) {
					t.Errorf("%s imports %s; internal/node must depend on nothing in this module", name, path)
				}
			}
		}
	}
}
