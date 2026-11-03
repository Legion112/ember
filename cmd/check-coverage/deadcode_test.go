package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// modulePath is this module, so a loaded package can be told from a dependency.
const modulePath = "github.com/Legion112/ember"

// TestNoProductionDeadCode is the detector for the one defect this repository
// has now found eight times by accident.
//
// AGENTS.md counts them. `queue.Cost` summed a request's card time and only
// its own test ever called it. `wireErrorEnvelope` was typed, fixtured and
// covered, and the only thing that ever unmarshalled into it was the test that
// wrote the fixture -- so every non-2xx reached the operator as a raw body.
// `node.Offer.Interruptible` was a field nothing set, read by a money guard
// that therefore could not fire. `RateFromFloat` was an unreachable copy of a
// live ceiling. Two of the eight were money defects and one was an
// operator-facing error path, and **not one was found by a test**, because in
// every case the tests passed. That is what "covered" means here: the test
// suite and the code agreed with each other and neither was connected to the
// program.
//
// `unused` is enabled in .golangci.yml and found exactly one of the eight
// (`sortedKeys`, #46). It cannot find the rest and it cannot be configured to.
// Two reasons, both checked rather than assumed:
//
//   - A test caller is a caller as far as the compiler is concerned, and
//     golangci-lint v2.15.0's `unused` settings are
//     exported-fields-are-used, parameters-are-used,
//     local-variables-are-used, field-writes-are-uses,
//     post-statements-are-reads and generated-is-used. `whole-program` and
//     `check-exported`, which staticcheck once had, are refused by the config
//     schema. The only lever is the global `run.tests: false`, which stops
//     every linter looking at test files -- and .golangci.yml's exclusions are
//     scoped to `_test.go` precisely because they are looked at.
//   - Even with `run.tests: false`, `unused` reports only unexported
//     identifiers in a non-main package. Five of the eight were exported.
//
// So this is the same machinery the other four guards here use -- walk, find,
// fail -- pointed at a question a syntactic walk cannot answer, because
// telling a dead method from one that exists to satisfy an interface needs
// types. `golang.org/x/tools/go/packages` is already a dependency of this
// command (cmd/check-coverage's main uses x/tools/cover), so it costs no new
// one; it adds `golang.org/x/mod` and `golang.org/x/sync` to go.mod as
// indirect requirements of x/tools, and nothing to go.sum, which is the proof
// they were already in the module cache.
//
// It is a **test** and not a subcommand of this binary, although the binary is
// right here and already runs under a Makefile target. A target has to be
// invoked; `go test ./...` is what everybody already runs and what CI already
// runs twice. The guard whose whole subject is "nobody looked" must not itself
// depend on somebody looking. cmd/check-coverage is still the package, because
// it is excluded from the coverage gate (a guard needs no coverage floor) and
// because TestEveryBuildTagReachesTheGates and
// TestNoWorkflowPublishesAMovingLatestTag are the same shape of
// whole-tree assertion -- as against internal/logx's and internal/loopback's
// guards, which are tests inside the package they seal.
func TestNoProductionDeadCode(t *testing.T) {
	tr := loadTree(t)
	cands := findCandidates(tr)
	if len(cands) < 500 {
		// The premise, asked rather than assumed, for the reason
		// TestNoWorkflowPublishesAMovingLatestTag asks its own: a loader that
		// silently found nothing reports that the tree is clean. There are
		// about nine hundred.
		t.Fatalf("only %d function declarations found in the production tree; this guard cannot have looked at it", len(cands))
	}
	prod, test := countRefs(tr, cands)
	satisfied := interfaceMethods(t, tr, cands)

	// The interface check, asked rather than assumed, and named rather than
	// counted. Both of these methods exist only to satisfy an interface --
	// queue.Recorder is a report.Sink and nothing constructs it by that name,
	// cli.multiFlag is a flag.Value handed to flag.Var -- and both were
	// reported as dead by a version of the walk that was subtly wrong. If this
	// stops suppressing them the guard has started reporting live code, which
	// is the failure that gets a detector deleted, so it fails here with the
	// cause named instead of thirty lines lower with a list of symbols.
	for _, sym := range []string{
		"internal/queue.(*Recorder).Emit",
		"internal/cli.(*multiFlag).Set",
	} {
		ok := false
		for key, c := range cands {
			if c.sym == sym && satisfied[key] {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("%s is no longer recognised as satisfying an interface; the interface walk is broken and every report below it is suspect", sym)
		}
	}

	// Printed on every run, not logged with t.Log -- which `go test` hides
	// unless -v or a failure. AGENTS.md's rule for the coverage gate's
	// exclusions is that they "appear in CI logs and review diffs rather than
	// living unread in a Makefile variable", and an allow-list for dead code
	// is the one that most needs reading.
	fmt.Printf("deadcode: %d baseline exclusions, %d test-support paths\n", len(deadExcluded), len(testSupport))
	for _, e := range deadExcluded {
		fmt.Printf("deadcode: excluded %s -- %s\n", e.sym, e.why)
	}

	excluded := map[string]string{}
	for _, e := range deadExcluded {
		if _, dup := excluded[e.sym]; dup {
			t.Errorf("%s is listed twice in deadExcluded", e.sym)
		}
		if strings.TrimSpace(e.why) == "" {
			t.Errorf("%s is excluded with no reason written down", e.sym)
		}
		excluded[e.sym] = e.why
	}

	// A path that no longer exists is the `imagelabs` failure (#46): a typo or
	// a rename turns an exclusion into a line nobody will ever match, and the
	// list then claims to be waiving something it is not.
	for _, s := range testSupport {
		path := filepath.Join(tr.root, filepath.FromSlash(strings.TrimSuffix(s.path, "/")))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("testSupport names %s, which is not in the tree (%v); "+
				"a stale entry waives nothing and hides the next real one", s.path, err)
		}
	}

	hit := map[string]bool{}
	var found []string
	for key, c := range cands {
		if len(prod[key]) > 0 || satisfied[key] {
			continue
		}
		if _, ok := excluded[c.sym]; ok {
			hit[c.sym] = true
			continue
		}
		where := "and nothing anywhere calls it"
		if n := len(test[key]); n > 0 {
			where = fmt.Sprintf("and only test code calls it (%d site(s))", n)
		}
		found = append(found, fmt.Sprintf("%s\n\t%s is declared in production code %s", c.pos, c.sym, where))
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("%s;\n\tconnect it to the program, delete it, or add it to deadExcluded with the reason. "+
			"AGENTS.md records eight of these, two of which were money defects and none of which a passing test could see.", f)
	}

	// And the mirror, which is the half that rots. An exclusion for a symbol
	// that is now called from production, or that no longer exists, is a hole
	// left open over a wall somebody already rebuilt -- so it fails, the same
	// direction AssertLeftoverCapsMatch checks its claim in.
	for _, e := range deadExcluded {
		if !hit[e.sym] {
			t.Errorf("deadExcluded names %s, which this guard no longer reports; "+
				"it has either gained a production caller or been deleted, so remove the entry", e.sym)
		}
	}
}

// ---- loading ----

type tree struct {
	pkgs []*packages.Package
	fset *token.FileSet
	root string
}

// loadTree type-checks the production tree, including the test variant of
// every package, which is where the test callers are.
//
// test/ is deliberately not loaded. A lab caller and no caller at all are the
// same verdict here -- neither is production -- so loading the labs would buy
// only a more precise count in a message, and it would cost the two lab build
// tags and every module they import being present in the cache. GOPROXY=off
// is set so that cost cannot be paid silently over the network: AGENTS.md says
// `go test ./...` needs none, and a guard that quietly started requiring one
// would break that rule in the place nobody looks.
func loadTree(t *testing.T) *tree {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Dir:   root,
		Tests: true,
		Env:   append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly"),
	}
	loaded, err := packages.Load(cfg, "./internal/...", "./cmd/...")
	if err != nil {
		t.Fatalf("load the tree: %v", err)
	}
	tr := &tree{root: root}
	var errs []string
	seen := map[*packages.Package]bool{}
	packages.Visit(loaded, nil, func(p *packages.Package) {
		if seen[p] || !inModule(p.PkgPath) {
			return
		}
		seen[p] = true
		for _, e := range p.Errors {
			errs = append(errs, p.PkgPath+": "+e.Error())
		}
		if tr.fset == nil {
			tr.fset = p.Fset
		}
		tr.pkgs = append(tr.pkgs, p)
	})
	// A load error means some package was type-checked without its
	// dependencies, and every reference in it is then invisible -- which this
	// guard would read as dead code. It fails rather than reporting.
	if len(errs) > 0 {
		sort.Strings(errs)
		t.Fatalf("the tree did not type-check, so references cannot be resolved:\n%s", strings.Join(errs, "\n"))
	}
	return tr
}

func inModule(path string) bool {
	path = strings.TrimSuffix(strings.TrimSuffix(path, ".test"), "_test")
	return path == modulePath || strings.HasPrefix(path, modulePath+"/")
}

// relPath is the module-relative name of a file, or "" for a file outside the
// tree -- which is what `go test`'s generated _testmain.go is.
func (tr *tree) relPath(pos token.Pos) string {
	rel, err := filepath.Rel(tr.root, tr.fset.Position(pos).Filename)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// isTest is the one classification the whole guard rests on, so it is three
// rules and no heuristic.
func isTest(rel string) bool {
	if rel == "" || strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "test/") {
		return true
	}
	for _, s := range testSupport {
		if s.path == rel || (strings.HasSuffix(s.path, "/") && strings.HasPrefix(rel, s.path)) {
			return true
		}
	}
	return false
}

// testSupport are the production files that are test support.
//
// AGENTS.md requires "one narrow interface per external system, with a real
// implementation and a `Recording*` fake", and a fake cannot live in a
// _test.go file because the labs import it as a package. So its constructor
// and its inspection accessors have, by construction, no production caller and
// never will. Classifying the file rather than listing its twenty methods is
// what keeps this list short enough to read -- without it this guard reported
// fifty-five symbols, more than half of them fakes, which is the shape of
// detector that gets turned off.
//
// It cuts both ways on purpose: a reference *from* one of these files is a
// test reference, so a production function whose only caller is a fake is
// still reported. That is how genstore.JobRun.Sampling surfaced -- the fake
// aggregates in Go and the real store aggregates in SQL.
//
// A path here that is not in the tree is a failure, not a no-op.
var testSupport = []struct{ path, why string }{
	{"internal/dockerengine/recording.go", "the dockerengine.API fake"},
	{"internal/genstore/recording.go", "the genstore.Store fake, which aggregates in Go so the SQL has something to disagree with"},
	{"internal/sshx/recording.go", "the sshx.Runner fake"},
	{"internal/tunnel/dialer_fake.go", "the tunnel dialer fake"},
	{"internal/provider/providertest/", "the provider conformance harness, imported by provider tests and by the labs"},
}

// deadExcluded is the baseline this guard landed on top of, and it is a
// backlog rather than a set of permissions.
//
// Thirty-two symbols in the production tree already had no production caller
// when the detector was written. Deleting them is thirty-two judgement calls
// across sixteen packages, several of them in files other branches are
// touching, and AGENTS.md's own position on the hardest of these is that "a
// detector should report it; what to do about a report is a human's call". So
// they are named, each with the reason it is here rather than fixed, and the
// list may only shrink: an entry this guard stops reporting is a failure, so
// nobody can delete a function and leave its waiver behind.
//
// What the list buys is the thirty-third. A new symbol with no production
// caller is a failed build, and adding it to this list is a line in a review
// diff with a sentence attached -- which is exactly what none of the eight had.
//
// ONE STATED LIMIT, WHICH THE THIRTY-FOURTH WAS FOUND BY HAND BECAUSE OF. A
// call from a production file is a production reference whether or not the
// function making it is itself reachable, so an excluded symbol's own callees
// are never reported. `nodemem.Report.Limit` sat behind the entry that used to
// be here for `nodemem.Report.Headroom` exactly that way -- `Limit`'s only
// caller was the dead `Headroom`, so the guard saw a live reference, and the
// waiver justified one dead symbol by naming another ("beside Limit which is
// read"). Both are gone now; the hole is not. Closing it means reachability
// from an entry point rather than a reference count, which is a different walk
// and would report a cascade this list has no measured opinion about -- so it
// is written down rather than guessed at, because a limitation nobody can read
// out of the code is a limitation that will be forgotten. The practical rule
// for a reviewer: an exclusion's reason may not point at another symbol as
// evidence that it is live.
var deadExcluded = []struct{ sym, why string }{
	// Deliberately unreachable from production, and AGENTS.md says so in
	// words. These two are the entries that are not a backlog: they are the
	// answer.
	// The reason this entry gave was wrong in both halves, which matters
	// because for a few hours it was the only thing in the repository that
	// went red over a reinstated TOFU fallback -- a stale-inventory check
	// whose own message says to delete the entry (#111). It said TOFU is
	// "unreachable through WithNode, which mints a key for every node":
	// minting is why the fallback *branch* was unreached, not why it could
	// not be written, and one line in trustFor reinstated it. And it named a
	// "trust-on-first-use lab" that does not exist; the callers are this
	// package's own tests and internal/tunnel/sshdial's. What guards it now
	// is a type -- provision.trustFor and provision.Session.Trust are
	// hostkey.Pin -- so the fallback does not compile on that path, and the
	// mechanism is asserted positively beside it. This entry is bookkeeping
	// and was never the seal.
	{"internal/hostkey.(*Store).TrustOnFirstUse", "no production caller: provision.trustFor and provision.Session.Trust are typed hostkey.Pin, so a trust that is not a pin cannot be named on the path that rents nodes. Kept for a provider that genuinely cannot accept an injected key, of which there is none today; reached by this package's tests and internal/tunnel/sshdial's"},
	{"internal/httpx.(*StatusError).Retryable", "httpx has no retry loop and must not gain one; the classifier sits where somebody would add it"},
	{"internal/hostkey.(*Store).Get", "a pin is read through Trust, never by id; Get exists for the store's own tests and for a reader that does not exist yet"},

	// Reported, and the fix is a decision about behaviour rather than a
	// deletion.
	{"internal/node.CUDAAtLeast", "the comparison min_cuda would be enforced with, and nothing enforces it: RunPod refuses the field outright and no other provider reads the floor, so deleting this would remove the evidence that the key is unenforced"},
	{"internal/exposure.Tolerated", "written so a caller could log the platform ports it decided to live with, and no caller logs them -- 'an exposure that is waved through silently is one nobody re-examines' is its own doc comment"},
	{"internal/report.Writer", "the Sink-as-io.WriteCloser adapter; node byte streams reach the operator another way"},
	{"internal/report.Multi", "fan-out over sinks, superseded by the queue wrapping its own sinks"},
	{"internal/report.Kinds", "the enumeration of event kinds, for a reader that would list them"},
	{"internal/logx.WithRequest", "the request-id key, beside WithRun which production does attach"},
	{"internal/webapi.(*Server).Handler", "the mux, exported for tests; Serve uses s.mux directly"},
	{"internal/workload.Outputs", "a predicate on node.Spec.Outputs that the fetch path answers inline"},
	{"internal/node.ParseState", "the inverse of State.String, for a reader of a state ember wrote down; every provider maps its own strings"},

	// Accessors that are the only way to read a sealed type's own state. The
	// seals are the point -- loopback.Bind's field is unexported with no
	// literal path, exposure.PortRequest's `public` is unexported -- and an
	// accessor with no reader yet is the cheap half of that. Each is one line.
	{"internal/app.(Attached).IsZero", "does this run mount a store; the planner branches on the Handle instead"},
	{"internal/blob.(*Dir).Name", "the rooted directory, for an error message nothing composes"},
	{"internal/catalogue.(*Store).Path", "the day file a sample belongs to; Append computes it internally"},
	{"internal/catalogue.(Level).Known", "whether a level is one of the four the schema enumerates"},
	{"internal/catalogue.(Level).Reported", "whether the provider gave a level at all; the report uses Rentable and Rank"},
	{"internal/catalogue.(Series).Span", "first and last observation in a read series"},
	{"internal/genstore.(JobRun).Sampling", "the job's duration; the real store computes it in SQL and only the Go fake calls this"},
	{"internal/hostkey.(Fingerprint).Algo", "the key type; every message prints the whole fingerprint"},
	{"internal/hostkey.(Pair).Fingerprint", "the minted pair's fingerprint; callers fingerprint the public half directly"},
	{"internal/keys.(Identity).Fingerprint", "the operator key's fingerprint, for a line nothing logs"},
	{"internal/loopback.(Bind).AddrPort", "the sealed bind's address; everything dials through String"},
	{"internal/node.(HostKeySource).Authenticated", "whether a source authenticated the first connection; ls reports the source itself"},
	{"internal/node.(LeftoverHandle).IsZero", "the zero leftover, by symmetry with Handle.IsZero which is read"},
	{"internal/node.(ReadyNode).HostKey", "the key a ready node reported; the pin is built from the minted key and never from this"},
	{"internal/node.(State).Valid", "whether a state is one this package defined; Billable is the guard that matters and defaults to true"},
	{"internal/node.(StorePrice).Hourly", "a store's hourly rate; ls prices stores by the month, which is how they bill"},
	{"internal/report.(*Stream).Seq", "the sequence the stream has reached; the number travels on the event"},
	{"internal/statestore.(*Store).Dir", "the journal directory -- AGENTS.md's 'one name for a path': the harness reads it through this package, and through Open rather than through here"},
}

// ---- candidates ----

type candidate struct {
	sym string
	pos string
}

// findCandidates is every function and method declared in a production file.
//
// Fields and types are out of scope, and that is a stated limit rather than an
// oversight: node.Offer.Interruptible -- one of the eight -- was a field, and
// this guard would not have caught it. A field is written by encoding/json and
// gopkg.in/yaml.v3 through reflection, so "no production reader" is true of
// every wire field in the tree and a field-level version of this would report
// hundreds of them. That is the detector that gets turned off.
func findCandidates(tr *tree) map[string]candidate {
	out := map[string]candidate{}
	for _, p := range tr.pkgs {
		for _, f := range p.Syntax {
			rel := tr.relPath(f.Pos())
			if isTest(rel) {
				continue
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				// init and main are called by the runtime, and _ is not a
				// name anything could call.
				switch fd.Name.Name {
				case "init", "main", "_":
					continue
				}
				fn, _ := p.TypesInfo.Defs[fd.Name].(*types.Func)
				if fn == nil {
					continue
				}
				key := tr.key(fn)
				if _, dup := out[key]; dup {
					continue
				}
				out[key] = candidate{
					sym: symbol(fn),
					pos: fmt.Sprintf("%s:%d", rel, tr.fset.Position(fd.Name.Pos()).Line),
				}
			}
		}
	}
	return out
}

// key is a declaration's identity, and it is the source position rather than
// the types.Object. That is not a convenience; it is the correctness of the
// whole walk.
//
// With Tests:true, go/packages type-checks a package's production files
// *twice* -- once as `internal/foo` and once as `internal/foo [internal/foo.test]`
// -- and the two runs produce different *types.Func objects for the same
// declaration. Keyed by object, every function in every package that has a
// test appeared twice: once with its real production callers and once with
// none, and the second copy was reported. The first version of this guard
// found 389 symbols, 334 of them that same artefact, and a glance at the list
// would have read as proof that the tree is full of dead code. A position is
// the one identity both runs agree on, and because every loaded package shares
// one FileSet it is comparable across them.
//
// Reference *sites* are deduplicated by position for the same reason: a call
// in a production file is otherwise counted once per variant.
func (tr *tree) key(fn *types.Func) string {
	return tr.fset.Position(fn.Pos()).String()
}

func symbol(fn *types.Func) string {
	pkg := ""
	if fn.Pkg() != nil {
		pkg = strings.TrimPrefix(fn.Pkg().Path(), modulePath+"/") + "."
	}
	if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
		recv := types.TypeString(sig.Recv().Type(), func(*types.Package) string { return "" })
		return pkg + "(" + recv + ")." + fn.Name()
	}
	return pkg + fn.Name()
}

// ---- references ----

func countRefs(tr *tree, cands map[string]candidate) (prod, test map[string]map[string]bool) {
	prod, test = map[string]map[string]bool{}, map[string]map[string]bool{}
	for _, p := range tr.pkgs {
		for _, f := range p.Syntax {
			into := prod
			if isTest(tr.relPath(f.Pos())) {
				into = test
			}
			// The enclosing declaration, so a function that only calls itself
			// is not its own caller. Mutual recursion between two dead
			// functions still hides both, which is a known limit and the one
			// `unused`'s reachability analysis does not share.
			var enclosing []string
			ast.Inspect(f, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.FuncDecl:
					key := ""
					if fn, _ := p.TypesInfo.Defs[v.Name].(*types.Func); fn != nil {
						key = tr.key(fn)
					}
					enclosing = append(enclosing, key)
				case *ast.Ident:
					fn, _ := p.TypesInfo.Uses[v].(*types.Func)
					if fn == nil {
						return true
					}
					key := tr.key(fn)
					if _, ok := cands[key]; !ok {
						return true
					}
					if len(enclosing) > 0 && enclosing[len(enclosing)-1] == key {
						return true
					}
					if into[key] == nil {
						into[key] = map[string]bool{}
					}
					into[key][tr.fset.Position(v.Pos()).String()] = true
				}
				return true
			})
		}
	}
	return prod, test
}

// ---- interface satisfaction ----

// interfaceMethods are the methods that exist to satisfy an interface.
//
// This is the hard part and the reason the guard needs types rather than a
// syntactic walk. A method that satisfies an interface something uses is used
// even where nothing names it: genstore.Store has two implementations,
// provider.Provider, sshx.Runner, dockerengine.API and report.Sink have
// several each, and every Recording* fake is nothing but such methods. A
// version of this that could not see that reported a hundred and thirty-six
// extra symbols -- which is not a noisy detector, it is a detector somebody
// deletes in the first week.
func interfaceMethods(t *testing.T, tr *tree, cands map[string]candidate) map[string]bool {
	t.Helper()
	ifaces := reachableInterfaces(t, tr)
	out := map[string]bool{}
	for _, p := range tr.pkgs {
		for _, f := range p.Syntax {
			if isTest(tr.relPath(f.Pos())) {
				continue
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv == nil {
					continue
				}
				fn, _ := p.TypesInfo.Defs[fd.Name].(*types.Func)
				if fn == nil {
					continue
				}
				key := tr.key(fn)
				if _, ok := cands[key]; !ok || out[key] {
					continue
				}
				if implementsSomething(fn, ifaces) {
					out[key] = true
				}
			}
		}
	}
	return out
}

// implementsSomething asks for the whole method set and not just the name.
//
// The receiver has to implement the interface entirely, or any method called
// Close would be waved through by io.Closer whether or not its type is ever
// used as one.
func implementsSomething(fn *types.Func, ifaces []*types.Interface) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	recv := []types.Type{sig.Recv().Type()}
	if _, isPtr := recv[0].(*types.Pointer); !isPtr {
		// A value receiver is in the pointer's method set too, and the
		// pointer is what an interface is usually satisfied by.
		recv = append(recv, types.NewPointer(recv[0]))
	}
	for _, it := range ifaces {
		if !ifaceHas(it, fn.Name()) {
			continue
		}
		for _, r := range recv {
			if types.Implements(r, it) {
				return true
			}
		}
	}
	return false
}

func ifaceHas(it *types.Interface, name string) bool {
	for i := range it.NumMethods() {
		if it.Method(i).Name() == name {
			return true
		}
	}
	return false
}

// reflective are the interfaces a method can satisfy without anything in this
// tree ever naming the interface, because the standard library reaches them
// through reflection or through a type assertion of its own: fmt's %v finds
// String, encoding/json finds MarshalJSON, yaml.v3 finds UnmarshalYAML.
//
// They are looked up from the packages that imported them rather than
// hard-coded as method names, so a signature that merely looks right does not
// count.
var reflective = map[string][]string{
	"fmt":                 {"Stringer", "GoStringer", "Formatter"},
	"encoding/json":       {"Marshaler", "Unmarshaler"},
	"encoding":            {"TextMarshaler", "TextUnmarshaler", "BinaryMarshaler", "BinaryUnmarshaler"},
	"gopkg.in/yaml.v3":    {"Marshaler", "Unmarshaler"},
	"sort":                {"Interface"},
	"net/http":            {"Handler"},
	"io":                  {"Reader", "Writer", "Closer", "ReaderFrom", "WriterTo", "Seeker"},
	"log/slog":            {"Handler", "LogValuer"},
	"database/sql/driver": {"Valuer"},
}

// reachableInterfaces is every interface the tree's own type information
// mentions, plus the two sets no package exports.
//
// "Mentioned" rather than "declared", and recursively: an interface reaches
// this walk through a parameter, a result, a struct field, a map value, a
// slice element or a named type's underlying type, because that is how
// genstore.Store is used -- as a field on a struct nobody asserts.
// It is a worklist and not a bounded recursion, and that is a bug fix rather
// than a preference. The recursive version capped its depth at eight, and the
// roots it started from are the keys of three maps -- so whether an interface
// was reached within the cap depended on Go's map iteration order, which is
// randomised per run. `(*multiFlag).Set` satisfies `flag.Value`, and the guard
// reported it as dead on one run in several while passing on the others: a
// detector that is right most of the time is worse than no detector, because
// the failure arrives on somebody else's unrelated pull request. Reachability
// over a seen set is a fixpoint and has no order to depend on.
func reachableInterfaces(t *testing.T, tr *tree) []*types.Interface {
	t.Helper()
	var work []types.Type
	for _, p := range tr.pkgs {
		for _, o := range p.TypesInfo.Defs {
			if o != nil {
				work = append(work, o.Type())
			}
		}
		for _, o := range p.TypesInfo.Uses {
			if o != nil {
				work = append(work, o.Type())
			}
		}
		for _, tv := range p.TypesInfo.Types {
			work = append(work, tv.Type)
		}
		for path, imp := range p.Imports {
			names, want := reflective[path]
			if !want || imp.Types == nil {
				continue
			}
			for _, n := range names {
				if tn, ok := imp.Types.Scope().Lookup(n).(*types.TypeName); ok {
					work = append(work, tn.Type())
				}
			}
		}
	}

	out := unnamed()
	// Pointer identity, not the type's printed name. With Tests:true a package
	// that has tests is type-checked twice, and the two runs produce two
	// `report.Sink` interfaces that print identically -- `types.TypeString`
	// qualifies by package *path*, which both variants share -- while being
	// different objects whose method signatures mention different
	// `report.Event` types. types.Implements is exact, so collapsing the two
	// kept whichever the map happened to yield first and dropped the one that
	// matched: `(*Recorder).Emit`, which is a report.Sink and nothing else,
	// was reported as dead on every run after the depth cap came out. Keyed by
	// pointer, both are kept and a method is suppressed by whichever of them
	// its own package's types agree with.
	seen := map[any]bool{}
	// Bounded, and hitting the bound is an error rather than a short answer,
	// for the reason ScanStores' page loop is: an interface set silently
	// missing its tail is a false report of dead code. A generic type can in
	// principle instantiate itself forever; nothing here does, and the tree
	// settles around a hundred thousand types.
	const maxTypes = 2_000_000
	processed := 0
	push := func(ty types.Type) {
		if ty != nil {
			work = append(work, ty)
		}
	}
	for len(work) > 0 {
		ty := work[len(work)-1]
		work = work[:len(work)-1]
		if ty == nil {
			continue
		}
		processed++
		if processed > maxTypes {
			t.Fatalf("the type walk did not settle after %d types; the interface set would be incomplete and this guard would report live methods as dead", maxTypes)
		}
		switch v := ty.(type) {
		case *types.Named:
			if seen[v] {
				continue
			}
			seen[v] = true
			push(v.Underlying())
		case *types.Alias:
			push(types.Unalias(v))
		case *types.Interface:
			// The empty interface matches everything and would waive the
			// whole tree.
			if v.NumMethods() == 0 {
				continue
			}
			if seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		case *types.Pointer:
			push(v.Elem())
		case *types.Slice:
			push(v.Elem())
		case *types.Array:
			push(v.Elem())
		case *types.Map:
			push(v.Key())
			push(v.Elem())
		case *types.Chan:
			push(v.Elem())
		case *types.Tuple:
			for i := range v.Len() {
				push(v.At(i).Type())
			}
		case *types.Struct:
			for i := range v.NumFields() {
				push(v.Field(i).Type())
			}
		case *types.Signature:
			if v.Recv() != nil {
				push(v.Recv().Type())
			}
			push(v.Params())
			push(v.Results())
		}
	}
	return out
}

// unnamed are the error-tree interfaces, which have no name anywhere.
//
// errors.Is, errors.As and errors.Unwrap assert against anonymous interfaces
// declared inside the errors package, so an Unwrap method is reachable from
// every errors.Is call in the program and from no type this walk can find.
// Without these, three error wrappers -- app.usageError, gpucheck.NotReadyError
// and sdserver.refused -- were reported as dead with no caller anywhere, which
// is a false positive on the most ordinary idiom in the language.
func unnamed() []*types.Interface {
	errType := types.Universe.Lookup("error").Type()
	anyType := types.NewInterfaceType(nil, nil)
	mk := func(name string, params, results []types.Type) *types.Interface {
		var ps, rs []*types.Var
		for _, p := range params {
			ps = append(ps, types.NewVar(token.NoPos, nil, "", p))
		}
		for _, r := range results {
			rs = append(rs, types.NewVar(token.NoPos, nil, "", r))
		}
		sig := types.NewSignatureType(nil, nil, nil, types.NewTuple(ps...), types.NewTuple(rs...), false)
		it := types.NewInterfaceType([]*types.Func{types.NewFunc(token.NoPos, nil, name, sig)}, nil)
		it.Complete()
		return it
	}
	return []*types.Interface{
		mk("Unwrap", nil, []types.Type{errType}),
		mk("Unwrap", nil, []types.Type{types.NewSlice(errType)}),
		mk("Is", []types.Type{errType}, []types.Type{types.Typ[types.Bool]}),
		mk("As", []types.Type{anyType}, []types.Type{types.Typ[types.Bool]}),
	}
}
