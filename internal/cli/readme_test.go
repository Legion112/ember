package cli

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
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

// TestREADME_DocumentsEveryFlag is one of the three things AGENTS.md's last
// line names, and the one of them that could be read out of the tree and was
// not.
//
// "Document CLI flag, config key and behaviour changes in README.md in the
// same commit" names three. Config keys are read out of the tree by the test
// above, and verbs -- which are not one of the three but are the same shape of
// claim -- by the one above that. Flags were left to the sentence, and seven of
// them went missing -- `--max-hourly-usd` on `ember up` and `ember offers`, `--cmd` on
// up, `--as` and `--vcpu` on store, `--only` on archs, and `--from` and
// `--archs` on images. `--max-hourly-usd` is the money ceiling and the one
// AGENTS.md leans on by name: an operator could not learn from README that the
// flag exists, let alone that it REPLACES a configured `max_hourly_usd` rather
// than narrowing it the way a request's value is narrowed.
//
// Which is what the sentence predicts will happen. This is
// TestEveryBuildTagReachesTheGates' argument one axis over -- a list
// maintained beside the thing it describes catches the entry somebody
// remembered and never the entry somebody forgets -- and a rule maintained in
// prose is that list with no list.
//
// BOTH DIRECTIONS, and deliberately not symmetric: each is permissive in the
// direction where being wrong is cheap.
//
//   - a flag in internal/cli and nowhere in README is the drift that
//     happened. Matched anywhere in the document, because README's register
//     puts flags inside larger spans (`ember archs --cards`) and inside
//     fenced examples, and a guard demanding a particular shape would fail
//     flags that are documented properly.
//   - a flag README types at ember that no FlagSet declares is the other
//     half, and without it this is the hand-maintained list with extra steps
//     that TestREADME_NamesEveryVerb's comment already refuses. A RENAMED
//     flag is caught by the first direction the moment the new name appears
//     nowhere; a REMOVED one leaves README describing something nobody can
//     type, and nothing would fire. It found one on its first run --
//     `ember ls --all`, at README:1361 and five more times in AGENTS.md.
//     `ember ls` has one flag, `--config`; it has asked every configured
//     provider since it was written, and `--all` has never existed.
func TestREADME_DocumentsEveryFlag(t *testing.T) {
	b, err := os.ReadFile(readmePath())
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(b)

	declared := declaredFlags(t)
	// A floor, not a count. A parse that quietly matched nothing would make
	// this test pass over anything, which is the failure AssertNoStray sat in
	// for the life of the lab suite: it reported success because there was
	// nothing left to look at.
	if len(declared) < 25 {
		t.Fatalf("found only %d flags in internal/cli; the parse is broken, not the package", len(declared))
	}

	for _, name := range sortedKeys(declared) {
		if mentionsFlag(readme, name) {
			continue
		}
		where := slices.Clone(declared[name])
		sort.Strings(where)
		t.Errorf("--%s is declared on %s and appears nowhere in README.md; AGENTS.md's rule is that a CLI flag is documented in the same commit",
			name, strings.Join(where, " and "))
	}

	for _, m := range readmeEmberFlags(readme) {
		if _, ok := declared[m.name]; ok {
			continue
		}
		t.Errorf("README.md:%d types `--%s` at ember and no flag.FlagSet in internal/cli declares it. Either the flag is gone and the line is stale, or it belongs to another program and wants a backticked span of its own", m.line, m.name)
	}
}

// mentionsFlag reports whether README documents --name.
//
// A WHOLE TOKEN, which is where this departs from the config-key guard above.
// That one matches a bare substring and argues the case: a key is long and
// distinctive, and the alternative is a second structured table nobody reads.
// Flag names are not distinctive -- `--as`, `--by` and `--from` are two to
// four characters -- and a substring match would count `--as` as documented
// the day somebody writes up a `--assets`. A guard whose cheapest false pass
// is on its shortest name goes wrong first where nobody is looking.
//
// ONE DASH OR TWO, because the flag package accepts both and README uses
// both: `-image` at README:1066 and `--image` at :69 are the same flag, and a
// guard that insisted on the second would be a test about house style that
// sends somebody to edit prose to satisfy it. Measured over the document, the
// single-dash spelling adds exactly that one mention and no false pass.
func mentionsFlag(readme, name string) bool {
	for i := 0; ; {
		j := strings.Index(readme[i:], name)
		if j < 0 {
			return false
		}
		at := i + j
		i = at + len(name)
		if i < len(readme) && continuesFlagName(readme[i]) {
			continue
		}
		dashes := 0
		for at-dashes-1 >= 0 && readme[at-dashes-1] == '-' {
			dashes++
		}
		if dashes != 1 && dashes != 2 {
			continue
		}
		// `re-read` is not `-read`: the character before the dashes must not
		// be part of a word either.
		if before := at - dashes - 1; before >= 0 && continuesFlagName(readme[before]) {
			continue
		}
		return true
	}
}

func flagNameStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// continuesFlagName reports whether c could be the next character of a flag
// name, which is what makes `--as` inside `--assets` not a mention of `--as`.
func continuesFlagName(c byte) bool {
	return c == '-' || c == '_' || flagNameStart(c) || (c >= 'A' && c <= 'Z')
}

type readmeFlag struct {
	name string
	line int
}

// readmeEmberFlags returns the flags README types at ember, for the stale
// direction.
//
// From each occurrence of the word `ember` to the end of the line or the next
// backtick, whichever comes first. A narrow window on purpose: this direction
// condemns, and a false yes accuses working code of being stale.
//
// THE BACKTICK IS THE WHOLE MECHANISM, and the bound was measured over the
// document rather than reasoned about. README:139 is "`ember version` reports
// what built it: `git describe --tags --always" -- two spans on one line -- so
// reading to the end of the line reports three of git's flags as ember flags
// that no longer exist. Reading each backticked span that merely names ember
// is worse rather than better: it picks `--listen-ip` out of an sd-server argv
// in a fenced block that happens to mention ember. And two dashes and not one,
// although mentionsFlag takes either: `$ go tool nm bin/ember | grep -c
// opentelemetry` and `ember up --log-level debug | jq -c ...` would both
// condemn `-c`, because a single dash is how every other program on those
// lines spells its own short flags.
//
// What it gives up is a flag documented only in prose with no invocation near
// it, which is checked by the other direction and not by this one. Under-
// reporting a stale line costs somebody a read of README; condemning `--host`
// because llama-server's config example sits beside the word ember teaches the
// reader to stop believing the test, which is what `allow_interruptible` cost
// when it read like a safety switch and guarded nothing.
func readmeEmberFlags(readme string) []readmeFlag {
	var out []readmeFlag
	for i, line := range strings.Split(readme, "\n") {
		for j := 0; ; {
			k := strings.Index(line[j:], "ember")
			if k < 0 {
				break
			}
			at := j + k
			j = at + len("ember")
			// `embernode` and `ember.yaml` are not invocations; `./bin/ember`
			// is, so what precedes it only has to not be part of a word.
			if at > 0 && continuesFlagName(line[at-1]) {
				continue
			}
			rest := line[j:]
			if rest != "" && (continuesFlagName(rest[0]) || rest[0] == '.') {
				continue
			}
			if e := strings.IndexByte(rest, '`'); e >= 0 {
				rest = rest[:e]
			}
			for _, name := range flagTokens(rest) {
				out = append(out, readmeFlag{name: name, line: i + 1})
			}
		}
	}
	return out
}

// flagTokens pulls the `--name` tokens out of one command line. A bare `--`
// yields nothing: ember's own tar command writes one to end its flags.
func flagTokens(s string) []string {
	var out []string
	for i := 0; i+2 < len(s); i++ {
		if s[i] != '-' || s[i+1] != '-' {
			continue
		}
		if i > 0 && continuesFlagName(s[i-1]) {
			continue
		}
		if !flagNameStart(s[i+2]) {
			continue
		}
		j := i + 2
		for j < len(s) && continuesFlagName(s[j]) {
			j++
		}
		out = append(out, s[i+2:j])
		i = j
	}
	return out
}

// flagSetDeclarators are the methods on *flag.FlagSet that register a flag,
// against the position of the flag's name among their arguments.
// flagSetOthers are the rest.
//
// Both lists exist so that a method called on a FlagSet and named in NEITHER
// is a hard failure rather than a flag this guard quietly stops seeing --
// which is the only way to be sure that a declarator added to the standard
// library after this was written, or a receiver this walk has misidentified,
// cannot pass through unread. TestFlagTablesClassifyEveryFlagSetMethod holds
// them to the type rather than to each other, for the reason the build-tag
// gate is checked against the tree: two lists agreeing with one another prove
// nothing about the thing they describe.
var flagSetDeclarators = map[string]int{
	"Bool": 0, "BoolFunc": 0, "Duration": 0, "Float64": 0, "Func": 0,
	"Int": 0, "Int64": 0, "String": 0, "Uint": 0, "Uint64": 0,
	"BoolVar": 1, "DurationVar": 1, "Float64Var": 1, "Int64Var": 1,
	"IntVar": 1, "StringVar": 1, "TextVar": 1, "Uint64Var": 1,
	"UintVar": 1, "Var": 1,
}

var flagSetOthers = []string{
	"Arg", "Args", "ErrorHandling", "Init", "Lookup", "NArg", "NFlag",
	"Name", "Output", "Parse", "Parsed", "PrintDefaults", "Set",
	"SetOutput", "Visit", "VisitAll",
}

// TestFlagTablesClassifyEveryFlagSetMethod holds the two tables above to the
// standard library's own type, in both directions.
//
// Without it, `flag` gaining one more declarator -- or a typo in a table
// entry -- would mean declaredFlags reading straight past a flag and the
// guard above reporting it as documented. That is a gate that cannot fail,
// which this repository has now found four of, one of them deciding whether a
// Docker image is published. The reflect call is the derivation; the tables
// are the part an AST walk can act on, and an entry naming no method is
// refused as well, because `Uint64Varr` in both tables would satisfy every
// assertion while reading nothing.
func TestFlagTablesClassifyEveryFlagSetMethod(t *testing.T) {
	classified := map[string]bool{}
	for m := range flagSetDeclarators {
		classified[m] = true
	}
	for _, m := range flagSetOthers {
		if classified[m] {
			t.Errorf("%q is in both flagSetDeclarators and flagSetOthers", m)
		}
		classified[m] = true
	}

	typ := reflect.TypeOf(&flag.FlagSet{})
	onType := map[string]bool{}
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		onType[name] = true
		if !classified[name] {
			t.Errorf("*flag.FlagSet has a method %q in neither flagSetDeclarators nor flagSetOthers; put it in one. declaredFlags fails on a method it cannot classify, and the alternative to failing is reading past a flag", name)
		}
	}
	for _, m := range sortedKeys(classified) {
		if !onType[m] {
			t.Errorf("%q is in one of the flag tables and *flag.FlagSet has no such method, so the entry describes nothing", m)
		}
	}
}

// declaredFlags returns every flag internal/cli registers, against the
// subcommand(s) that register it.
//
// Read with go/parser for the reason yamlKeys is, and here the false positive
// is not hypothetical: a grep for `fs\.String` over this package also returns
// `p.Min.String()` and `e.Cost.String()`. So a call counts only when its
// receiver is an identifier this function saw bound to a *flag.FlagSet -- by a
// flag.NewFlagSet in the same function, or as a parameter of that type, which
// is how addLogFlags registers --log-level and --log-format for two
// subcommands instead of four fs.String calls that could drift.
//
// NOT a convention about the name `fs`. cmd_images.go imports io/fs and
// shadows it with the FlagSet in the one function that has one, so a guard
// keyed on the spelling would be reading two different things under one name
// and would be right by luck.
//
// The FlagSet's own name is what an error message reports, because
// `flag.NewFlagSet("store push", ...)` already spells the subcommand an
// operator would have to type -- and a flag named on two subcommands, which
// --max-hourly-usd is, has to say both or the reader goes looking in one file.
//
// Scope is internal/cli, because these are the flags an operator types at
// `ember`. check-coverage, embernode and emberwl have flags of their own and
// are not ember; README documents the program, and a guard reaching into the
// node agent's would be demanding prose about a binary no operator runs.
func declaredFlags(t *testing.T) map[string][]string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}

	out := map[string][]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			sets, verb := flagSetIdents(fn)
			if len(sets) == 0 {
				continue
			}
			where := "ember " + verb
			if verb == "" {
				where = name + "'s " + fn.Name.Name
			}
			for _, flagName := range declaredIn(t, fn.Body, sets, name) {
				out[flagName] = appendOnce(out[flagName], where)
			}
		}
	}
	return out
}

// flagSetIdents returns the identifiers in fn that hold a *flag.FlagSet, and
// the name its FlagSet was created under -- "store push", which is the
// subcommand a failure has to be able to name.
func flagSetIdents(fn *ast.FuncDecl) (map[string]bool, string) {
	sets := map[string]bool{}
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			if !isFlagSetPtr(field.Type) {
				continue
			}
			for _, n := range field.Names {
				sets[n.Name] = true
			}
		}
	}

	verb := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				lit, ok := newFlagSetName(rhs)
				if !ok || i >= len(s.Lhs) {
					continue
				}
				if id, ok := s.Lhs[i].(*ast.Ident); ok {
					sets[id.Name] = true
					if verb == "" {
						verb = lit
					}
				}
			}
		case *ast.ValueSpec:
			if !isFlagSetPtr(s.Type) {
				return true
			}
			for _, id := range s.Names {
				sets[id.Name] = true
			}
		}
		return true
	})
	return sets, verb
}

func isFlagSetPtr(e ast.Expr) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "FlagSet" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "flag"
}

// newFlagSetName reports the first argument of a flag.NewFlagSet call.
func newFlagSetName(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewFlagSet" {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "flag" {
		return "", false
	}
	return stringLit(call.Args[0])
}

// declaredIn walks one function body for the flags registered on sets.
func declaredIn(t *testing.T, body *ast.BlockStmt, sets map[string]bool, file string) []string {
	t.Helper()

	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || !sets[recv.Name] {
			return true
		}

		method := sel.Sel.Name
		at, isDecl := flagSetDeclarators[method]
		if !isDecl {
			if slices.Contains(flagSetOthers, method) {
				return true
			}
			// Unreachable through a real *flag.FlagSet, which
			// TestFlagTablesClassifyEveryFlagSetMethod is what keeps true --
			// so arriving here means the receiver is not one and this walk is
			// reading some other type's method. Loud, because the quiet way
			// to be wrong about a receiver is a flag nobody ever sees.
			t.Fatalf("%s: %s.%s is not a *flag.FlagSet method, so %q is not the FlagSet this guard took it for", file, recv.Name, method, recv.Name)
		}
		if at >= len(call.Args) {
			t.Fatalf("%s: %s.%s has %d argument(s) and the flag's name should be number %d", file, recv.Name, method, len(call.Args), at+1)
		}
		name, ok := stringLit(call.Args[at])
		if !ok {
			// A name assembled at runtime is a name this guard cannot read,
			// and reading past it reports the flag as documented.
			t.Fatalf("%s: %s.%s is given a flag name that is not a string literal, and this guard reads the source", file, recv.Name, method)
		}
		out = append(out, name)
		return true
	})
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func appendOnce(xs []string, s string) []string {
	if slices.Contains(xs, s) {
		return xs
	}
	return append(xs, s)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
