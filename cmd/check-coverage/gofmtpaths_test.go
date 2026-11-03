package main

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Legion112/ember/internal/repotree"
)

// gofmtPathsVar is the Makefile variable the fmt recipe and the CI step are
// both pointed at. Named here so a rename fails loudly rather than quietly
// matching nothing, which is makeVetTags' rule.
const gofmtPathsVar = "GOFMT_PATHS"

// TestGofmtReachesEveryGoFileInThisCheckout is the derivation the Makefile
// cannot do for itself.
//
// `gofmt -l -w .` walked the whole directory, and an agent worktree is a
// whole second checkout of this repository at `.claude/worktrees/<name>/` --
// inside the one it was cut from. That is #143's defect, and here it is worse
// than at the three Go guards it was filed about: those READ another
// checkout's source and failed this suite over it, while `make fmt` REWRITES
// it. Measured, with one nested checkout holding an unformatted file:
// `gofmt -l .` named that file, and `golangci-lint run ./...` and
// `go vet ./...` named nothing, because those two resolve through the module
// and a nested checkout has a go.mod of its own.
//
// gofmt has no exclusion flag and no notion of a module, so it cannot be
// taught the prune the Go guards use -- it has to be handed paths. A list is
// then exactly the thing LAB_TAGS and PUBLISH_LABS exist not to be unless
// something checks it, and both directions are needed for the reasons those
// two already record:
//
//   - A .go file under none of the listed paths is a file `make fmt` does not
//     format and CI does not check, which is how a whole top-level directory
//     could arrive unformatted and unreported -- the `imagelab` gap, where
//     the tree grew something the gate's list had never heard of.
//   - A listed path holding no .go file is dead configuration, and it is also
//     how a typo passes: `interna` beside `internal` satisfies the first
//     direction for every file while formatting nothing. gofmt EXITS 1 on a
//     path that does not exist, so a misspelling of a directory that is not
//     there is loud already -- the direction this covers is a path that
//     exists and holds no Go source, where gofmt is silent.
//
// Checked by reinstating both: `GOFMT_PATHS := cmd internal` leaves test/
// unformatted-and-unchecked and goes red naming it, and an entry for `docs`
// goes red naming that.
func TestGofmtReachesEveryGoFileInThisCheckout(t *testing.T) {
	root := filepath.Join("..", "..")

	fields, found := makeVarFields(t, root, gofmtPathsVar)
	if !found {
		t.Fatalf("no %s assignment in %s; the fmt recipe's path list is the source this test compares against, and it cannot be found", gofmtPathsVar, makefileName)
	}
	if len(fields) == 0 {
		t.Fatalf("%s assigns %s nothing; `gofmt` would then be handed no path at all", makefileName, gofmtPathsVar)
	}

	listed := map[string]bool{}
	for _, p := range fields {
		listed[p] = true
	}

	covered := map[string]int{}
	files := 0
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
			// gofmt skips a directory whose name begins with a dot or an
			// underscore, and `bin` holds built binaries rather than source.
			// `testdata` is NOT skipped here, unlike in treeBuildTags: gofmt
			// does descend into it, so a .go file there is a file CI checks
			// and `make fmt` must reach.
			case ".git", "bin":
				return fs.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && rel != "." {
				return fs.SkipDir
			}
			if repotree.Nested(root, path) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		top, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		if !listed[top] {
			t.Errorf("%s is Go source and %q is not in the Makefile's %s, so `make fmt` never formats it and the CI gofmt step never checks it: add %q to the list",
				filepath.ToSlash(rel), top, gofmtPathsVar, top)
			return nil
		}
		covered[top]++
		return nil
	})
	if err != nil {
		t.Fatalf("walk the checkout: %v", err)
	}
	// The floor every walk here carries: one that visited nothing reports
	// that the list is complete. There are about four hundred.
	if files < 200 {
		t.Fatalf("found only %d Go files in this checkout; the walk is broken, not the Makefile", files)
	}

	for _, p := range slices.Sorted(maps.Keys(listed)) {
		if covered[p] > 0 {
			continue
		}
		// A path gofmt cannot open is already a failure of the recipe, so the
		// message tells the two apart rather than reporting one as the other.
		if _, statErr := os.Stat(filepath.Join(root, p)); statErr != nil {
			t.Errorf("the Makefile's %s lists %q, which is not in this checkout: `make fmt` exits 1 on it", gofmtPathsVar, p)
			continue
		}
		t.Errorf("the Makefile's %s lists %q and no Go file in this checkout is under it; delete the entry or the directory it was meant to reach is misspelled",
			gofmtPathsVar, p)
	}
}

// TestCIGofmtStepAsksMakeForThePaths keeps the list one list.
//
// The tags reached `go vet` from three hand-written copies and .golangci.yml
// held a fourth that had never learned about `imagelab`; the workflows ask
// make for them now, and TestNoWorkflowSpellsItsOwnVetTags is what stops the
// paste coming back. This is the same guard for the same reason one tool
// over: a `gofmt` in a workflow that names its own paths is a second copy of
// GOFMT_PATHS, and a second copy drifts the first time this one grows an
// entry.
//
// The match is a prohibition on a spelling rather than an attempt to
// understand a shell script, which is TestNoWorkflowSpellsItsOwnVetTags'
// argument: any `gofmt` line that is not reading the value out of make is a
// copy, whatever it says. Over-matching fails loudly and is a one-line fix.
func TestCIGofmtStepAsksMakeForThePaths(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the workflow directory: %v", err)
	}

	checked, lines := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		checked++
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, script := range runScripts(doc) {
			for i, line := range strings.Split(script, "\n") {
				code, _, _ := strings.Cut(line, "#")
				if !invokesGofmt(code) {
					continue
				}
				lines++
				if strings.Contains(code, "$"+gofmtPathsVar) || strings.Contains(code, "${"+gofmtPathsVar) {
					continue
				}
				t.Errorf("%s runs gofmt over paths of its own (%q, line %d of that step); the list lives in the Makefile's %s -- read it with `make -s print-%s` and pass that",
					name, strings.TrimSpace(line), i+1, gofmtPathsVar, gofmtPathsVar)
			}
		}
	}
	if checked == 0 {
		t.Fatal("read no workflow files; the walk is broken, not the workflows")
	}
	// The floor that matters here: CI's formatting gate is one step in one
	// file, so a walk that found no gofmt line at all means either the step
	// is gone -- in which case nothing checks formatting -- or this parse
	// stopped seeing it.
	if lines == 0 {
		t.Fatalf("parsed %d workflow files and found no gofmt line; either CI no longer checks formatting or this parse cannot see the step", checked)
	}
}

// invokesGofmt reports whether a shell line runs gofmt, as against merely
// containing the word.
//
// The first version of this test matched the substring, which is the
// prohibition-on-a-spelling TestNoWorkflowSpellsItsOwnVetTags argues for --
// and it reported `echo "::error::not gofmt'd:"`, the line that TELLS the
// developer what went wrong. That is `TestNoListenOutsideLoopback`'s own
// first version flagging `loopback.Listen`: a guard that fires on correct
// code is a guard people learn to ignore, and here it would have fired on
// prose.
//
// So the bound is the one character after the word. A command name is
// followed by whitespace or ends the line; `gofmt'd` is followed by an
// apostrophe and `GOFMT_PATHS` is not this word at all, since the match is
// case-sensitive. What precedes it only has to not be part of a longer word,
// so `$(gofmt` and `| gofmt` both count -- which is `emberFlags`' rule for
// `./bin/ember`, one language over.
func invokesGofmt(code string) bool {
	const name = "gofmt"
	for i := 0; ; {
		k := strings.Index(code[i:], name)
		if k < 0 {
			return false
		}
		at := i + k
		i = at + len(name)
		if at > 0 && isShellWordByte(code[at-1]) {
			continue
		}
		if i < len(code) && !isShellSpaceByte(code[i]) {
			continue
		}
		return true
	}
}

// isShellWordByte is deliberately narrower than a path character: `/` and `.`
// are NOT word bytes, so `/usr/bin/gofmt -l .` is an invocation this sees.
func isShellWordByte(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isShellSpaceByte(b byte) bool {
	return b == ' ' || b == '\t'
}

// TestInvokesGofmtTellsACommandFromProse is the negative control for the
// guard above, and it is the half that matters.
//
// A matcher that fires on everything makes TestCIGofmtStepAsksMakeForThePaths
// a test that cannot pass, which gets the guard deleted; one that fires on
// nothing makes it a test that cannot fail, which this repository has now
// found four of. Both directions, over the lines ci.yml really holds plus
// the spellings somebody could write instead.
func TestInvokesGofmtTellsACommandFromProse(t *testing.T) {
	for _, c := range []struct {
		code string
		want bool
	}{
		{"          gofmt -l -w cmd internal test", true},
		{"          unformatted=$(gofmt -l $GOFMT_PATHS)", true},
		{"            gofmt -d $GOFMT_PATHS", true},
		{"          /usr/bin/gofmt -l .", true},
		{"          ls | gofmt", true},
		{"          gofmt", true},

		// The line that broke the first version of this guard: CI's own
		// error message.
		{`            echo "::error::not gofmt'd:"`, false},
		{"          GOFMT_PATHS=$(make -s print-GOFMT_PATHS)", false},
		{"          gofumpt -l .", false},
		{"          go-gofmt-wrapper -l .", false},
		{"          echo gofmtd", false},
	} {
		if got := invokesGofmt(c.code); got != c.want {
			t.Errorf("invokesGofmt(%q) = %v, want %v", c.code, got, c.want)
		}
	}
}
