// Command check-coverage fails the build when a package is below its floor.
//
// It is a descendant of jira-mcp's tool of the same name, with one addition
// that matters: -strict. There, the floor applies to a hand-maintained list of
// packages, so a package added later is simply not gated — it escapes
// silently, and nobody finds out until someone reads the list. Here, a package
// that appears in neither -packages nor -exclude fails the run and has to be
// placed deliberately in one or the other.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/cover"
)

func main() {
	var (
		profile  = flag.String("profile", "coverage.out", "go cover profile path")
		module   = flag.String("module", "", "module path prefix, e.g. github.com/Legion112/ember")
		packages = flag.String("packages", "", "comma-separated pkg=min pairs relative to the module, e.g. internal/node=100")
		exclude  = flag.String("exclude", "", "comma-separated packages deliberately not gated")
		overall  = flag.Float64("overall", 0, "minimum coverage percent across the gated packages")
		strict   = flag.Bool("strict", false, "fail if a package is in neither -packages nor -exclude")
	)
	flag.Parse()

	// A stray positional argument means the flags after it were never parsed.
	// That happened once: a wrapped make variable split into two shell words,
	// and the gate reported success while checking four packages out of nine
	// with no overall floor and no -strict.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "check-coverage: unexpected argument %q; every flag after it was ignored\n", flag.Arg(0))
		os.Exit(2)
	}

	if err := run(*profile, *module, *packages, *exclude, *overall, *strict, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "check-coverage: %v\n", err)
		os.Exit(1)
	}
}

type stats struct{ stmts, covered int }

func (s stats) percent() float64 {
	if s.stmts == 0 {
		// A package with no statements is vacuously covered. Reporting 0 here
		// would fail the gate for a package that is nothing but type
		// declarations, which is a real shape in this repo.
		return 100
	}
	return 100 * float64(s.covered) / float64(s.stmts)
}

func run(profilePath, module, packagesSpec, excludeSpec string, overallMin float64, strict bool, w io.Writer) error {
	// A failed write to the report stream means the pipe is gone and the exit
	// status is about to be the only thing anyone sees, so there is nothing
	// useful to do with the error.
	out := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

	if module == "" {
		return fmt.Errorf("-module is required")
	}
	floors, err := parseFloors(packagesSpec)
	if err != nil {
		return err
	}
	excluded := map[string]bool{}
	for _, p := range splitCSV(excludeSpec) {
		excluded[p] = true
	}

	profiles, err := cover.ParseProfiles(profilePath)
	if err != nil {
		return fmt.Errorf("parse profile %s: %w", profilePath, err)
	}

	seen := map[string]*stats{}
	for _, p := range profiles {
		pkg := packageOf(p.FileName)
		rel, ok := strings.CutPrefix(pkg, module+"/")
		if !ok {
			continue
		}
		st := seen[rel]
		if st == nil {
			st = &stats{}
			seen[rel] = st
		}
		for _, b := range p.Blocks {
			st.stmts += b.NumStmt
			if b.Count > 0 {
				st.covered += b.NumStmt
			}
		}
	}

	var fail bool

	if strict {
		var ungated []string
		for rel := range seen {
			if _, ok := floors[rel]; ok {
				continue
			}
			if excluded[rel] {
				continue
			}
			ungated = append(ungated, rel)
		}
		sort.Strings(ungated)
		for _, rel := range ungated {
			fmt.Fprintf(os.Stderr, "FAIL: %s is in neither -packages nor -exclude; gate it or justify excluding it\n", rel)
			fail = true
		}
	}

	var gated stats
	names := make([]string, 0, len(floors))
	for rel := range floors {
		names = append(names, rel)
	}
	sort.Strings(names)

	for _, rel := range names {
		st, ok := seen[rel]
		if !ok {
			// A gated package the profile never mentions is a package that was
			// renamed or deleted without updating the gate. Silently passing
			// it would leave the gate pointing at nothing.
			fmt.Fprintf(os.Stderr, "FAIL: %s is gated but absent from the profile; was it renamed?\n", rel)
			fail = true
			continue
		}
		pct := st.percent()
		out("%-40s %6.1f%%  (min %.0f%%)\n", rel, pct, floors[rel])
		if pct+1e-9 < floors[rel] {
			fmt.Fprintf(os.Stderr, "FAIL: %s coverage %.1f%% < %.0f%%\n", rel, pct, floors[rel])
			fail = true
		}
		gated.stmts += st.stmts
		gated.covered += st.covered
	}

	if len(excluded) > 0 {
		// Printed on every run, so the exclusion list shows up in CI logs and
		// in review diffs rather than living unread in a Makefile variable.
		ex := make([]string, 0, len(excluded))
		for p := range excluded {
			ex = append(ex, p)
		}
		sort.Strings(ex)
		out("not gated: %s\n", strings.Join(ex, " "))
	}

	pct := gated.percent()
	out("%-40s %6.1f%%  (min %.0f%%)\n", "overall(gated)", pct, overallMin)
	if pct+1e-9 < overallMin {
		fmt.Fprintf(os.Stderr, "FAIL: overall coverage %.1f%% < %.0f%%\n", pct, overallMin)
		fail = true
	}

	if fail {
		return fmt.Errorf("coverage gate failed")
	}
	return nil
}

func parseFloors(spec string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, pair := range splitCSV(spec) {
		name, min, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("package %q has no floor; write it as %s=100", pair, pair)
		}
		v, err := strconv.ParseFloat(min, 64)
		if err != nil {
			return nil, fmt.Errorf("floor for %s: %w", name, err)
		}
		if v < 0 || v > 100 {
			return nil, fmt.Errorf("floor for %s is %v, outside 0..100", name, v)
		}
		out[name] = v
	}
	return out, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func packageOf(file string) string {
	i := strings.LastIndex(file, "/")
	if i < 0 {
		return file
	}
	return file[:i]
}
