package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	// publishLabsVar is the Makefile variable the publish gate selects with.
	// Named here so a rename fails this test loudly rather than quietly
	// matching nothing, which is the rule makeVetTags already follows: a
	// guard that cannot find its source of truth must not report that the
	// truth agrees with it.
	publishLabsVar = "PUBLISH_LABS"

	// labDir is the package the gate runs, relative to the module root.
	labDir = "test/integration"

	// labPrefix is the population. It is also what `make test-image-labs`
	// selects with, so a test named outside it is in neither set.
	//
	// `TestWorkloadImages_` -- plural -- is deliberately not in it and is not
	// a hole: that one name belongs to a Dockerfile-pin assertion that no
	// image-lab target selects either, and `make test-integration` runs it on
	// every pull request.
	labPrefix = "TestWorkloadImage_"
)

// publishLabsExcluded names a workload-image lab deliberately outside the
// publish gate, with the reason, which is the cmd/check-coverage idiom: an
// exclusion whose reason is not written down is a waiver nobody can review.
//
// It may only shrink, and a stale entry fails below.
var publishLabsExcluded = []struct{ test, why string }{
	{
		"TestWorkloadImage_SupportsVideoGeneration",
		"needs ember:sd, the CPU build, and a publish of the CUDA image must not require an unrelated image to exist; " +
			"TestWorkloadImages_PinTheSameUpstreamRevision is what keeps the two honest",
	},
}

// TestPublishGateNamesEveryWorkloadImageLab checks the publish gate's test
// list against the tree, in both directions.
//
// `make test-image-labs-publish` is the gate between a built workload image
// and `docker push`, on a laptop and in the publishing workflow alike, and it
// has now been found unable to fail twice. The first time, every lab target
// piped `go test` into `tee` and make read tee's status, so a failing lab
// exited 0 -- measured, with `docker tag` and `docker push` running over a
// deliberately red `TestWorkloadImage_ListensOnNothingButSSH`, and fixed by
// `SHELL`/`.SHELLFLAGS` globally. The second time was this: the gate selected
// its labs with a hand-written alternation of five test names, checked
// against nothing, and `go test -run` over an expression that matches fewer
// tests than intended -- or none -- exits 0. `PUBLISH_LABS=<a name no test
// has>` ran zero of the five assertions and the publish proceeded; one
// ordinary rename silently ran four (#115).
//
// `pipefail` could not touch the second, because nothing failed: the labs
// were never selected. So this is the same answer TestEveryBuildTagReachesTheGates
// gave for `.golangci.yml`'s tag list -- derive the question from the tree
// rather than checking two hand-written lists against each other -- and the
// irony recorded in #115 is that three comments in this repository already
// cited `PUBLISH_LABS` as the *precedent* for not keeping a list beside the
// thing it mirrors.
//
// Both directions, because each has already cost something:
//
//   - A name the list has and the tree does not is the rename and the typo,
//     which is what exits 0 with nothing selected.
//   - A lab the tree has and the list does not is how the image that lacked
//     `embernode fetch` shipped: the lab written to catch exactly that was
//     not in the workflow's own `-run` expression.
//
// What this cannot see is a `PUBLISH_LABS` overridden on the command line,
// which is how #115 was measured. That is the Makefile's `LAB_SELECT`: it
// counts what `go test -list` actually selected against `$(words
// $(PUBLISH_LABS))` before anything expensive runs. The two halves also cover
// each other -- this one cannot see a broken `-run` derivation and that one
// cannot see a lab nobody listed.
func TestPublishGateNamesEveryWorkloadImageLab(t *testing.T) {
	root := filepath.Join("..", "..")

	listed, ok := makeVarFields(t, root, publishLabsVar)
	if !ok {
		t.Fatalf("no %s assignment in %s; the publish gate's test list is the thing this compares against, and it cannot be found",
			publishLabsVar, makefileName)
	}
	if len(listed) == 0 {
		t.Fatalf("%s assigns %s nothing, so the publish gate would select no lab at all", makefileName, publishLabsVar)
	}

	tests, labs := labTests(t, filepath.Join(root, labDir))
	// A walk that found nothing passes for the wrong reason, which is the
	// failure mode every source-walking guard here has.
	if len(tests) == 0 {
		t.Fatalf("parsed no Test functions in %s; the walk is broken, not the tree", labDir)
	}
	if len(labs) == 0 {
		t.Fatalf("parsed %d Test functions in %s and found none named %s*; the walk is broken, not the tree",
			len(tests), labDir, labPrefix)
	}

	// Printed on every run rather than with t.Log, which `go test` hides
	// without -v: the exclusions are the part of this a reviewer has to read,
	// the same reason the coverage gate prints its own.
	fmt.Printf("publish gate: %d lab(s) named, %d in the tree, %d excluded\n", len(listed), len(labs), len(publishLabsExcluded))
	for _, e := range publishLabsExcluded {
		fmt.Printf("publish gate: excluded %s -- %s\n", e.test, e.why)
	}

	inList := map[string]bool{}
	for _, name := range listed {
		if inList[name] {
			t.Errorf("%s names %s twice; LAB_SELECT counts the names and `go test -list` counts the tests, so a duplicate makes the two disagree about a gate that is fine",
				publishLabsVar, name)
		}
		inList[name] = true
		if !tests[name] {
			t.Errorf("%s names %s and no test in %s declares it, so `go test -run` selects nothing for that name and exits 0; "+
				"the gate would publish having run one assertion fewer than it claims", publishLabsVar, name, labDir)
		}
	}

	excluded := map[string]string{}
	for _, e := range publishLabsExcluded {
		if strings.TrimSpace(e.why) == "" {
			t.Errorf("%s is excluded from the publish gate with no reason written down", e.test)
		}
		if _, dup := excluded[e.test]; dup {
			t.Errorf("%s is listed twice in publishLabsExcluded", e.test)
		}
		excluded[e.test] = e.why

		// The mirror, which is the half that rots -- the `imagelabs` failure:
		// a renamed or deleted lab turns an exclusion into a line nobody will
		// ever match, and the list then claims to be waiving something it is
		// not.
		if !tests[e.test] {
			t.Errorf("publishLabsExcluded names %s, which %s does not declare; a stale exclusion waives nothing and hides the next real one",
				e.test, labDir)
		}
		if inList[e.test] {
			t.Errorf("%s is both named in %s and excluded from it; one of the two is a leftover and a reader cannot tell which",
				e.test, publishLabsVar)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(labs)) {
		if inList[name] || excluded[name] != "" {
			continue
		}
		t.Errorf("%s declares %s and %s does not name it, so the gate between a built image and `docker push` would not run it. "+
			"Add it, or add it to publishLabsExcluded with the reason -- the image that shipped without `embernode fetch` "+
			"is what a lab outside this list costs", labs[name], name, publishLabsVar)
	}
}

// labTests reports every Test function in the lab package: all of them, and
// the subset named like a workload-image lab, each mapped to its file.
//
// Parsed with go/parser and not read through the toolchain, for
// TestEveryBuildTagReachesTheGates' reason: `go list` resolves through a build
// context, so the `integration && imagelab` files -- which are most of what
// this is about -- would be invisible to a walk that asked which files are in
// the package. Build constraints are deliberately NOT evaluated here either:
// a lab put behind some third tag would be unselectable by this gate and
// would otherwise vanish from the population silently, so it is demanded in
// the list and its author has to write an exclusion saying why instead.
func labTests(t *testing.T, dir string) (map[string]bool, map[string]string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	tests := map[string]bool{}
	labs := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range f.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			// A method cannot be a test, and a receiver named Test* would
			// otherwise be counted as one.
			if !isFunc || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			tests[fn.Name.Name] = true
			if strings.HasPrefix(fn.Name.Name, labPrefix) {
				labs[fn.Name.Name] = name
			}
		}
	}
	return tests, labs
}
