package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// verbMarker is the line in README.md that this test reads the verb table out
// of. The comment is in the document rather than only here, so somebody
// editing the table is told what checks it before they are told by a failure.
const verbMarker = "verbs: checked against internal/cli.commands()"

// readmePath is the one spelling of it, used by every test in this file.
func readmePath() string { return filepath.Join("..", "..", "README.md") }

// TestREADME_NamesEveryVerb is the mechanism, not the count.
//
// README's Status section said "Every verb is implemented" and then listed
// six, while commands() had twelve. It had been wrong for most of the
// program's life and got worse with every merge, because the list of verbs was
// maintained by hand in two places and nothing compared them -- the same shape
// as the three hand-maintained copies of the build-tag list, and as the four
// copies of a deadline that money discipline exists to prevent.
//
// So it reads the dispatch table and the document and fails on a difference,
// in BOTH directions and including the summaries:
//
//   - a verb in commands() and not in the table is the drift that happened.
//   - a verb in the table and not in commands() is the drift a guard
//     hard-coding today's thirteen names would permit: a hand-maintained list
//     with extra steps, which would keep passing after a verb was renamed or
//     removed and leave the README describing a command that is gone.
//   - a summary that differs makes the table a copy nobody verified. `ember`
//     with no arguments prints these strings, so the document and the program
//     describe each verb in the same words or one of them is wrong.
func TestREADME_NamesEveryVerb(t *testing.T) {
	rows := verbTable(t)
	cs := commands()

	inREADME := map[string]string{}
	for _, r := range rows {
		if _, dup := inREADME[r.verb]; dup {
			t.Errorf("README lists %q twice", r.verb)
		}
		inREADME[r.verb] = r.summary
	}

	inCode := map[string]string{}
	for _, c := range cs {
		inCode[c.name] = c.summary
	}

	for _, c := range cs {
		summary, ok := inREADME[c.name]
		if !ok {
			t.Errorf("`ember %s` is in the dispatch table and not in README's verb table; add a row `| %s | %s |`", c.name, "`"+c.name+"`", c.summary)
			continue
		}
		if summary != c.summary {
			t.Errorf("README describes `ember %s` as %q and the program describes it as %q; `ember` with no arguments prints the second, so they cannot differ", c.name, summary, c.summary)
		}
	}
	for _, r := range rows {
		if _, ok := inCode[r.verb]; !ok {
			t.Errorf("README's verb table lists %q and commands() has no such verb, so the document describes a command nobody can run", r.verb)
		}
	}

	// Order last, and only when the two name the same set, so a missing verb
	// is reported as a missing verb rather than buried in an off-by-one list.
	if len(rows) == len(cs) && !t.Failed() {
		for i := range rows {
			if rows[i].verb != cs[i].name {
				t.Errorf("README's verb table is in a different order from commands(): row %d is %q and the table's entry %d is %q. One order, so a reader comparing them has nothing to reconcile", i+1, rows[i].verb, i+1, cs[i].name)
				break
			}
		}
	}
}

type verbRow struct{ verb, summary string }

// verbTable parses the marked markdown table out of README.md.
//
// A missing marker or an empty table is a t.Fatal and never a pass. A guard
// that quietly finds nothing to check is the failure mode AssertNoStray sat in
// for the life of the lab suite: it reported success because there was nothing
// left to look at.
func verbTable(t *testing.T) []verbRow {
	t.Helper()

	b, err := os.ReadFile(readmePath())
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	lines := strings.Split(string(b), "\n")

	start := -1
	for i, line := range lines {
		if strings.Contains(line, verbMarker) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("README.md has no line containing %q, so there is no verb table to check and this test would pass over anything", verbMarker)
	}

	var rows []verbRow
	seenTable := false
	for _, line := range lines[start+1:] {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			// The marker is an HTML comment and may be several lines; the
			// table is the first one after it, and the first non-row line
			// after that ends it.
			if seenTable {
				break
			}
			continue
		}
		seenTable = true

		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			t.Fatalf("verb table row %q has %d cells, want 2", line, len(cells))
		}
		name := strings.TrimSpace(cells[0])
		// The header and the |---|---| separator.
		if name == "verb" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, ":") {
			continue
		}
		if !strings.HasPrefix(name, "`") || !strings.HasSuffix(name, "`") {
			t.Fatalf("verb table row %q does not name a verb in backticks", line)
		}
		rows = append(rows, verbRow{
			verb:    strings.Trim(name, "`"),
			summary: strings.TrimSpace(cells[1]),
		})
	}

	if len(rows) == 0 {
		t.Fatal("parsed no rows out of README's verb table; the parse is broken, not the document")
	}
	return rows
}

// TestREADME_DocumentsEveryConfigKey is the same guard one layer down.
//
// Eight keys -- negative_prompt, batch_count, allow_interruptible,
// data_centers, disk_gib, state_dir, output_dir, config_dir -- were
// implemented, wired and documented nowhere, against AGENTS.md's own rule that
// a config key is documented in the commit that adds it. A key an operator
// cannot find is a key they cannot use, and `data_centers` and
// `allow_interruptible` bear directly on where a node is placed and what it
// costs.
//
// It reads the yaml tags out of internal/config's source rather than listing
// the keys here, for the reason the verb table above is read rather than
// listed: a hand-maintained list of what needs documenting drifts exactly like
// the thing it is meant to guard. Matching is on the key appearing in a
// backticked span anywhere in README -- deliberately weak, because the
// alternative is a second structured table of fifty-odd keys and a document
// written to be parsed rather than read. What it catches is the failure that
// happened: a key nobody wrote down at all.
func TestREADME_DocumentsEveryConfigKey(t *testing.T) {
	b, err := os.ReadFile(readmePath())
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(b)

	keys := yamlKeys(t)
	if len(keys) < 20 {
		t.Fatalf("found only %d yaml keys in internal/config; the parse is broken, not the package", len(keys))
	}

	for _, k := range keys {
		if !strings.Contains(readme, "`"+k+"`") && !strings.Contains(readme, k+":") {
			t.Errorf("config key %q is implemented and appears nowhere in README.md; AGENTS.md's rule is that a config key is documented in the same commit", k)
		}
	}
}

// yamlKeys returns every `yaml:"..."` tag in internal/config's own source.
//
// Read with go/parser rather than with a regexp over the file, so a key named
// in a comment or in a string literal cannot be mistaken for a field -- the
// mistake that would make this guard pass on a key that is only ever
// mentioned.
func yamlKeys(t *testing.T) []string {
	t.Helper()

	dir := filepath.Join("..", "config")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var out []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			tag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				return true
			}
			key, ok := reflect.StructTag(tag).Lookup("yaml")
			if !ok {
				return true
			}
			// `yaml:"name,omitempty"` and `yaml:"-"` both occur in Go
			// generally; only the name matters, and a skipped field is not a
			// config key at all.
			key, _, _ = strings.Cut(key, ",")
			if key != "" && key != "-" {
				out = append(out, key)
			}
			return true
		})
	}
	return out
}
