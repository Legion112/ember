package main

import (
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The gates must know every build tag in this tree, and nothing was checking
// that they did.
//
// `.golangci.yml` named `integration` and not `imagelab`, so for the life of
// the image labs -- the labs that stop a bad workload image reaching a
// registry -- `make lint` and the golangci-lint step in CI compiled none of
// them. They are also the least frequently *executed* code here, because the
// workflow that runs them is dispatch-only and the CUDA image takes forty
// minutes to build, so the one tree nobody reads was the one tree nothing
// linted. Two findings were sitting in it when this was written: an unchecked
// error and a dead helper left behind by a deletion. Neither mattered; that
// is the point -- nothing would have reported a dangerous one either.
//
// The config's own comment already made the right argument ("without it the
// tagged tree is linted nowhere") and then named one tag. A comment that
// justifies the entry somebody remembered cannot catch the entry somebody
// forgets, which is the "one name for a path, not one per call site" failure
// in AGENTS.md reached through a config file: the journal's "nodes"
// subdirectory was also spelled correctly in both places until it wasn't.
//
// So the tree is the source of truth and both gates are checked against it,
// the shape TestNoListenOutsideLoopback uses -- walk, parse, fail -- rather
// than trusting review. Three alternatives were considered and rejected:
//
//   - Editing the YAML and stopping there. That is the fix for this tag and
//     leaves the next one a silent gap in all the same places, which is the
//     actual defect rather than its symptom.
//   - Deriving golangci-lint's tags from the Makefile. CI lints through
//     golangci/golangci-lint-action, which reads `.golangci.yml` and never
//     runs make, so the list has to exist in the YAML. A `make lint` that
//     passed --build-tags would then disagree with the CI step, and the
//     Makefile's own comment says a `make lint` that disagrees with CI is
//     theatre.
//   - A second command beside check-coverage. It would only run where a
//     target invoked it; a test runs in `go test ./...`, which `make test`,
//     `make all` and CI all do unconditionally, and it is where the module's
//     other structural rules already live.
//
// It lives beside check-coverage because that command exists for exactly this
// class of bug one axis over: -strict refuses a package in neither the gated
// list nor the exclusion list, because a hand-maintained list of things to
// check is a list that silently stops covering what arrives later. A build
// tag is the same list with a different spelling.
//
// The vet side is derived rather than checked twice: the tags reached
// `go vet` from three hand-written copies -- the Makefile and two steps in
// ci.yml -- and the workflows now ask make for them, the idiom
// workload-image.yml already uses for the image tag ("asked of make rather
// than spelled here ... two places drift"). What is left to check there is
// that nobody writes a fourth copy, which TestNoWorkflowSpellsItsOwnVetTags
// does.
const (
	lintConfigName = ".golangci.yml"
	makefileName   = "Makefile"

	// labTagsVar is the Makefile variable the vet recipe interpolates. Named
	// here so a rename fails this test loudly instead of quietly matching
	// nothing: a guard that cannot find its source of truth must not report
	// that the truth agrees with it.
	labTagsVar = "LAB_TAGS"
)

func TestEveryBuildTagReachesTheGates(t *testing.T) {
	root := filepath.Join("..", "..")

	used, files := treeBuildTags(t, root)
	// A walk that found nothing would pass for the wrong reason, which is the
	// failure mode every source-walking test has.
	if files == 0 {
		t.Fatal("parsed no Go files; the walk is broken, not the module")
	}
	if len(used) == 0 {
		t.Fatalf("parsed %d files and found no build constraints at all; this module has had them since the first lab", files)
	}

	lint := lintConfigTags(t, root)
	vet := makeVetTags(t, root)

	for _, tag := range slices.Sorted(maps.Keys(used)) {
		// Asked of go/build rather than guessed from the spelling. `unix`,
		// `linux`, `cgo` and `go1.27` are constraints the toolchain satisfies
		// on its own, and a letters-and-dots heuristic for "is this a project
		// tag?" is the mistake ParseBind made when it classified ":8188" as a
		// hostname: guessing ahead of the real parser sends the reader
		// somewhere the bug is not.
		if satisfiedByDefault(t, tag) {
			continue
		}
		where := strings.Join(used[tag], ", ")
		if !lint[tag] {
			t.Errorf("build tag %q gates %s and is not in %s's run.build-tags, so golangci-lint compiles none of it: add it",
				tag, where, lintConfigName)
		}
		if !vet[tag] {
			t.Errorf("build tag %q gates %s and is not in the Makefile's %s, so `go vet` never type-checks it: add it",
				tag, where, labTagsVar)
		}
	}

	// A tag in a gate that no file uses is dead configuration -- the same
	// "written and connected to nothing" shape this repository has now found
	// several times -- and it is also how a typo passes: `imagelabs` in both
	// lists satisfies every assertion above while linting nothing.
	for _, tag := range slices.Sorted(maps.Keys(lint)) {
		if used[tag] == nil {
			t.Errorf("%s lists build tag %q and no file in the tree is behind it; delete the entry or the files it was meant to reach are misspelled",
				lintConfigName, tag)
		}
	}
	for _, tag := range slices.Sorted(maps.Keys(vet)) {
		if used[tag] == nil {
			t.Errorf("the Makefile's %s lists build tag %q and no file in the tree is behind it; delete the entry or the files it was meant to reach are misspelled",
				labTagsVar, tag)
		}
	}
}

// TestNoWorkflowSpellsItsOwnVetTags keeps the vet tag list derived.
//
// The gap this file is about was three copies of one list: `.golangci.yml`,
// the Makefile's vet target, and two steps in ci.yml that spelled
// `-tags='integration imagelab'` by hand. The workflows ask make for them
// now, and nothing stops the next person pasting the old line back -- at
// which point the copy exists again and the test above cannot see it, because
// a stale copy in CI still names every tag on the day it is written.
//
// The match is a prohibition on a spelling rather than an attempt to
// understand a shell script: any `go vet` line in a workflow that also names
// -tags is a copy, whatever it says. Over-matching fails loudly and is a
// one-line fix; the failure this guards against is a paste, not an evasion.
func TestNoWorkflowSpellsItsOwnVetTags(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the workflow directory: %v", err)
	}

	checked, scripts := 0, 0
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
			scripts++
			for i, line := range strings.Split(script, "\n") {
				code, _, _ := strings.Cut(line, "#")
				if strings.Contains(code, "go vet") && strings.Contains(code, "-tags") {
					t.Errorf("%s runs `go vet` with its own -tags (%q, line %d of that step); the list lives in the Makefile's %s -- run `make vet`",
						name, strings.TrimSpace(line), i+1, labTagsVar)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("read no workflow files; the walk is broken, not the workflows")
	}
	// A walk that found no scripts inspected nothing, and this one can get
	// there without an error: a mapping whose keys are not all strings
	// decodes as map[any]any, so a single such key anywhere in a workflow
	// would have silently hidden every `run:` in that file. Both map shapes
	// are handled, and this is the assertion that says so.
	if scripts == 0 {
		t.Fatalf("parsed %d workflow files and found no run: scripts; the walk is broken, not the workflows", checked)
	}
}

// treeBuildTags maps every build tag in the tree to the files behind it, and
// reports how many files were parsed.
//
// Parsed out of the file text with go/build/constraint, which is the only way
// to see a tag the current build excludes: `go list` and parser.ParseDir both
// resolve through a build context, so the imagelab files -- the ones this
// exists for -- would be invisible to a walk that asked the toolchain which
// files are in the package. Test files are *not* exempt, unlike in
// TestNoListenOutsideLoopback: every tag in this module is on a _test.go
// file.
func treeBuildTags(t *testing.T, root string) (map[string][]string, int) {
	t.Helper()

	used := map[string][]string{}
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
			case ".git", "bin", "testdata", "target", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++

		// PackageClauseOnly stops at the package clause, which is where a
		// build constraint has to be anyway -- so the comments returned are
		// exactly the ones that can carry one.
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.PackageClauseOnly|parser.ParseComments)
		if perr != nil {
			return perr
		}
		for _, group := range f.Comments {
			for _, c := range group.List {
				// Both spellings. The old `// +build` form is still honoured
				// by the toolchain for a file that carries no //go:build
				// line, so a guard that read only the new one would have a
				// hole shaped exactly like the one it exists to close.
				if !constraint.IsGoBuild(c.Text) && !constraint.IsPlusBuild(c.Text) {
					continue
				}
				expr, cerr := constraint.Parse(c.Text)
				if cerr != nil {
					t.Errorf("%s: %q does not parse as a build constraint: %v", rel, c.Text, cerr)
					continue
				}
				for _, tag := range tagsIn(expr) {
					if !slices.Contains(used[tag], rel) {
						used[tag] = append(used[tag], rel)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	return used, files
}

// tagsIn collects the tag names a constraint mentions, negated or not.
//
// A negation still gates source: internal/statestore's `!unix` file is
// compiled exactly when the `unix` file is not, so a gate that knew about one
// and not the other would leave half the pair unchecked.
func tagsIn(e constraint.Expr) []string {
	switch v := e.(type) {
	case *constraint.TagExpr:
		return []string{v.Tag}
	case *constraint.NotExpr:
		return tagsIn(v.X)
	case *constraint.AndExpr:
		return append(tagsIn(v.X), tagsIn(v.Y)...)
	case *constraint.OrExpr:
		return append(tagsIn(v.X), tagsIn(v.Y)...)
	}
	return nil
}

// satisfiedByDefault reports whether the toolchain already satisfies a tag in
// an ordinary build, which is go/build's question and not this test's.
//
// BuildTags is cleared first, and that is the whole reason this is a function
// rather than a set of names: run as `go test -tags=imagelab ./...` and a
// context that inherited the invocation's tags would answer "already
// satisfied" for the one tag this file was written about, so the guard would
// pass most loudly in the run that proves nothing.
func satisfiedByDefault(t *testing.T, tag string) bool {
	t.Helper()

	ctx := build.Default
	ctx.BuildTags = nil

	dir := t.TempDir()
	// A name with no GOOS or GOARCH suffix, so the only constraint on the
	// file is the one being asked about.
	name := "tagcheck.go"
	src := "//go:build " + tag + "\n\npackage p\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
		t.Fatalf("write the probe file: %v", err)
	}
	ok, err := ctx.MatchFile(dir, name)
	if err != nil {
		t.Fatalf("ask go/build about %q: %v", tag, err)
	}
	return ok
}

// lintConfigTags reads run.build-tags out of .golangci.yml.
func lintConfigTags(t *testing.T, root string) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, lintConfigName))
	if err != nil {
		t.Fatalf("read %s: %v", lintConfigName, err)
	}
	var cfg struct {
		Run struct {
			BuildTags []string `yaml:"build-tags"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", lintConfigName, err)
	}
	if len(cfg.Run.BuildTags) == 0 {
		t.Fatalf("%s has no run.build-tags; every tagged tree in this module is linted nowhere", lintConfigName)
	}
	out := map[string]bool{}
	for _, tag := range cfg.Run.BuildTags {
		out[strings.TrimSpace(tag)] = true
	}
	return out
}

// makeVetTags reads the Makefile variable the vet recipe interpolates.
//
// The assignment is matched at the start of a line, which is the whole of the
// grammar being relied on. `make -s print-LAB_TAGS` would ask the real parser
// -- the habit this repository prefers, and what the publishing workflow does
// for the image tag -- but it would make `go test ./...` on a clean clone
// depend on make being installed, which is a requirement AGENTS.md is
// explicit about not adding. A missing or renamed variable is a hard failure
// here instead, so the weaker parse cannot fail open.
func makeVetTags(t *testing.T, root string) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, makefileName))
	if err != nil {
		t.Fatalf("read %s: %v", makefileName, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, labTagsVar)
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, " \t")
		for _, op := range []string{":=", "?=", "+=", "="} {
			if after, found := strings.CutPrefix(rest, op); found {
				out := map[string]bool{}
				for _, tag := range strings.Fields(after) {
					out[tag] = true
				}
				if len(out) == 0 {
					t.Fatalf("%s assigns %s nothing; `go vet` would then compile no tagged file at all", makefileName, labTagsVar)
				}
				return out
			}
		}
	}
	t.Fatalf("no %s assignment in %s; the vet recipe's tag list is the source this test compares against, and it cannot be found", labTagsVar, makefileName)
	return nil
}

// runScripts collects every `run:` script in a parsed workflow document.
//
// Walked generically rather than against a typed workflow schema: the shape
// of a GitHub workflow is somebody else's and changes without asking, and
// what is wanted here is every shell script in the file however it is nested.
func runScripts(node any) []string {
	var out []string
	visit := func(key any, val any) []string {
		if k, ok := key.(string); ok && k == "run" {
			if s, ok := val.(string); ok {
				return []string{s}
			}
		}
		return runScripts(val)
	}
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			out = append(out, visit(k, val)...)
		}
	case map[any]any:
		// yaml.v3 decodes a mapping into map[string]any only when every key
		// is a string. `on:` is the one key in a GitHub workflow that a YAML
		// implementation might hand back as a bool, and one such key makes
		// the whole file this shape -- so a walk that knew only the first
		// shape would skip every script in it and report success.
		for k, val := range v {
			out = append(out, visit(k, val)...)
		}
	case []any:
		for _, item := range v {
			out = append(out, runScripts(item)...)
		}
	}
	return out
}
