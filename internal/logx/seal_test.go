package logx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// handlerFuncs are the slog entry points that decide a format.
//
// slog.New is deliberately absent. `slog.New(slog.DiscardHandler)` is the
// fallback seven library packages use to stay silent when no logger was handed
// to them -- it decides neither a level nor a format, and banning it would
// mean every one of those packages had to import this one to say nothing.
var handlerFuncs = map[string]bool{
	"NewTextHandler": true,
	"NewJSONHandler": true,
}

// TestNoHandlerOutsideLogx states this package's reason to exist as code.
//
// The handler was built in two places with the same literal, and both pinned
// the level to info -- so seventeen debug calls in this tree could not be
// displayed by any invocation of ember that shipped. Changing ember's log
// format meant finding every place that had built a handler, and the second
// place is the one that gets missed: AGENTS.md records the same failure for
// the journal's "nodes" subdirectory, which was spelled correctly in both
// copies until it wasn't.
//
// So the rule is a build failure rather than a line in a document. It matches
// on the qualifier as well as the name, the correction
// TestNoListenOutsideLoopback needed: an earlier version flagged any selector
// called NewTextHandler and therefore flagged logx.New's own call, which is
// the one call it exists to permit.
//
// Test files are exempt. A test that wants to read what was logged has to
// build a handler over a buffer, nine of them do, and none of them is part of
// any binary's dependency surface.
func TestNoHandlerOutsideLogx(t *testing.T) {
	checked := 0
	walkModule(t, func(rel string, fset *token.FileSet, f *ast.File) {
		if rel == filepath.Join("internal", "logx", "logx.go") {
			return
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
					if q, ok := sel.X.(*ast.Ident); ok && q.Name == "slog" && handlerFuncs[sel.Sel.Name] {
						t.Errorf("%s:%d: slog.%s is called outside internal/logx; "+
							"build the logger with logx.New so one place decides ember's level and format",
							rel, fset.Position(v.Pos()).Line, sel.Sel.Name)
					}
				}
			case *ast.CompositeLit:
				// &slog.HandlerOptions{Level: ...} is the other half of the
				// decision, and a file holding one is a file that has an
				// opinion about the level even if it hands the struct
				// somewhere else to be used.
				if sel, ok := v.Type.(*ast.SelectorExpr); ok {
					if q, ok := sel.X.(*ast.Ident); ok && q.Name == "slog" && sel.Sel.Name == "HandlerOptions" {
						t.Errorf("%s:%d: a slog.HandlerOptions literal outside internal/logx; "+
							"the level is logx.Options.Level and is decided in one place",
							rel, fset.Position(v.Pos()).Line)
					}
				}
			}
			return true
		})
	})
	if checked == 0 {
		t.Fatal("inspected no files; the walk is broken, not the module")
	}
}

// setProviders are the four calls that turn OpenTelemetry on.
//
// The otel API is linked into every ember binary whether anybody wants it or
// not -- Docker's client wraps its own transport in otelhttp.NewTransport
// unconditionally, so internal/dockerengine/dockerclient pulls in 530
// `opentelemetry` symbols -- and it does nothing, because the API with no
// registered provider is a no-op. These are the calls that end that, and
// SetTextMapPropagator is here with the other three because a propagator is
// what starts writing `traceparent` onto outbound requests.
var setProviders = map[string]bool{
	"SetTracerProvider":    true,
	"SetMeterProvider":     true,
	"SetLoggerProvider":    true,
	"SetTextMapPropagator": true,
}

// TestNoTelemetrySDKRegistered is the decision of #33 as a build failure
// rather than a paragraph.
//
// WHAT WAS DECIDED AND WHY IT NEEDS ENFORCING. ember does not register an
// OpenTelemetry SDK, and the reasoning is in AGENTS.md's dependency bullet
// with the numbers it was taken on. The part that cannot live in a document
// is that today's state -- API linked, no provider, therefore no cost and no
// egress -- is not a property anybody chose file by file. It is what happens
// when nobody imports the SDK, so it survives exactly until somebody does,
// and the import that would do it is one line in a package that already has
// the API in scope. AGENTS.md's rule for this is "prefer making a bug
// unrepresentable over testing for it", and the closest reachable thing is
// the shape TestNoListenOutsideLoopback uses for net.Listen: walk the tree
// and refuse.
//
// IT SEALS THE SDK AND THE EXPORTERS, NOT THE API. Banning the API would ban
// Docker's client and therefore the local provider, and the API is the half
// that is free: `go tool nm bin/ember | grep -c 'otel/sdk'` is 0 today and
// that zero is the whole seal. go.opentelemetry.io/contrib is deliberately
// permitted for the same reason -- otelhttp is instrumentation against the
// API, already in the tree, and already producing non-recording spans.
//
// IT IS ALSO THE EGRESS SEAL, which is what #34 and #35 both wait on. An
// exporter is the only thing in this tree that would originate a connection
// carrying operator content: a span of a generation naturally holds the
// prompt, the seed and the output path, and the rule for operator content is
// an allow-list -- rejectSecrets deliberately not scanning `cmd`/`cmds`
// because "a token of affection" is a legitimate prompt is the precedent for
// why a deny-list is the wrong tool. So the terms of a future yes are in
// AGENTS.md and this test is what makes somebody read them.
//
// Test files are exempt, as they are for the two walks above, and the limit
// is worth naming because a seal believed past its reach is worse than none:
// a _test.go file importing the SDK would not fail here. It would still have
// to put a `require` in go.mod, which is what TestOTelStaysIndirect covers.
func TestNoTelemetrySDKRegistered(t *testing.T) {
	checked := 0
	walkModule(t, func(rel string, fset *token.FileSet, f *ast.File) {
		checked++
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(path, "go.opentelemetry.io/") {
				continue
			}
			if strings.Contains(path, "/sdk") || strings.Contains(path, "/exporters/") {
				t.Errorf("%s:%d: imports %s; ember registers no OpenTelemetry SDK -- "+
					"the decision and the measurements it was taken on are in AGENTS.md's "+
					"dependency bullet, and changing it means changing that entry first",
					rel, fset.Position(imp.Pos()).Line, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// Matched on the qualifier as well as the name, the correction
			// TestNoHandlerOutsideLogx above already needed: `SetMeterProvider`
			// on some other package's value is not this.
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !setProviders[sel.Sel.Name] {
				return true
			}
			if q, ok := sel.X.(*ast.Ident); ok && q.Name == "otel" {
				t.Errorf("%s:%d: otel.%s registers a global OpenTelemetry provider; "+
					"ember's otel API is deliberately a no-op and AGENTS.md says on what numbers",
					rel, fset.Position(call.Pos()).Line, sel.Sel.Name)
			}
			return true
		})
	})
	if checked == 0 {
		t.Fatal("inspected no files; the walk is broken, not the module")
	}
}

// TestOTelStaysIndirect is the same decision read off go.mod.
//
// WHY A SECOND CHECK FOR ONE RULE. The walk above sees imports in non-test
// files, which is where an SDK would have to be used -- and go.mod is where
// adopting one becomes visible to anybody auditing ember's dependencies,
// including through a route the walk cannot see (a _test.go import, or a
// `go mod tidy` that promotes a module because some new file referenced it).
// Containment twice by two mechanisms, which is the rule the tar extractor is
// written to and the leftover sweep repeats: a string check misses a
// spelling, and the module file cannot be wrong about what was required.
//
// Every otel module in go.mod is there because Docker's client needs it, and
// otlptracehttp is there for a reason sharper still: `go mod why` names
// `github.com/docker/docker/client.test` -- it is pinned by somebody else's
// test binary and contributes 0 symbols to ember. An entry losing its
// `// indirect` marker means a file in this module imported it, which is the
// decision, not a tidy.
//
// Line-scanned rather than parsed with golang.org/x/mod/modfile, because that
// is a module this repository does not have and would be a new dependency
// added to police a dependency decision.
func TestOTelStaysIndirect(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	seen := 0
	for i, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "go.opentelemetry.io/") {
			continue
		}
		seen++
		if !strings.HasSuffix(trimmed, "// indirect") {
			t.Errorf("go.mod:%d: %q is a direct requirement; ember adopts no OpenTelemetry "+
				"module, and AGENTS.md's dependency bullet records what the measurements said",
				i+1, trimmed)
		}
	}
	// Zero would mean Docker stopped instrumenting its client, which is a
	// thing to notice rather than a reason to pass: a check that holds
	// because it found nothing is the failure mode AGENTS.md names for
	// AssertNoStray, which could not fail for the life of the suite.
	if seen == 0 {
		t.Fatal("go.mod names no go.opentelemetry.io module; either the parse is broken " +
			"or docker/docker no longer pulls the otel API in, and AGENTS.md's entry " +
			"about a linked no-op API is then describing a state that no longer exists")
	}
}

// logMethods are the calls that can put a value in front of a reader.
//
// Over-broad on purpose: `t.Error` and `http.Error` match too. That costs
// nothing, because the rule below only fires when such a call also unwraps key
// material, and a line doing that is a line to rewrite wherever it is.
var logMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true, "With": true,
}

// TestNoKeyMaterialInALogCall is the seal that had to land with a reachable
// debug level, not after it.
//
// node.Secret exists because "one slog.String("hostkey", string(pem)) at debug
// level writes a private key into the journal permanently, and nothing about
// that line looks wrong in review". Every such line in this tree was harmless
// while no shipped invocation of ember could display a debug record. Adding
// --log-level debug removes that accident, so the discipline stops being a
// property of the handler and becomes a property of the call sites -- and a
// property of call sites is what a walk can check.
//
// THE RULE IS THE AUDIT HANDLE node.Secret ALREADY NAMES. Secret.Bytes() is
// documented as the one visible unwrapping, "so that an audit for places key
// material is handled greps for .Bytes() and finds all of them"; cmd/embernode
// repeats the shape for the PEM it reads out of its environment, because it
// cannot import internal/node. Logging the wrapper itself is safe and stays
// legal -- Secret implements slog.LogValuer and prints a fingerprint -- so what
// is forbidden is precisely the spelling that defeats it.
//
// WHAT IT CANNOT SEE, said plainly because a guard whose limits are not
// written down gets believed past them: an unwrapping laundered through a
// helper that merely BUILDS the attributes and returns them, since the match
// is on a log call's own argument list. cmd/embernode had exactly that for an
// hour -- a keyAttrs() helper, which reads better -- and the reinstatement
// pass found this walk green with `string(...Bytes())` sitting in it. The two
// keys are spelled inline at the log call there now, and cmd/embernode's own
// keys_test.go asserts on the OUTPUT rather than on the shape, which is the
// check that holds whatever the spelling.
//
// It is NOT a substring filter over message text. That is the tool AGENTS.md
// rejects for rejectSecrets, which deliberately does not scan `cmd` or `cmds`
// because a prompt is arbitrary English: "a token of affection" is a
// legitimate prompt. This matches on the shape of the expression instead.
func TestNoKeyMaterialInALogCall(t *testing.T) {
	checked := 0
	walkModule(t, func(rel string, fset *token.FileSet, f *ast.File) {
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !logMethods[sel.Sel.Name] {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(a ast.Node) bool {
					inner, ok := a.(*ast.CallExpr)
					if !ok {
						return true
					}
					if s, ok := inner.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Bytes" && len(inner.Args) == 0 {
						t.Errorf("%s:%d: .Bytes() unwraps key material inside a %s call; "+
							"log the node.Secret itself -- it is a slog.LogValuer and prints a fingerprint",
							rel, fset.Position(inner.Pos()).Line, sel.Sel.Name)
					}
					return true
				})
			}
			return true
		})
	})
	if checked == 0 {
		t.Fatal("inspected no files; the walk is broken, not the module")
	}
}

// walkModule parses every non-test Go file under internal/, cmd/ and test/.
//
// Shared by the two walks above rather than written twice, which is the defect
// this package is about. The skip list is loopback/nolisten_test.go's,
// including its correction: the logo directory is matched by its path and not
// by its name, because skipping any directory *called* assets also skipped
// internal/assets -- a whole package exempt from the one rule it was meant to
// obey, with nothing saying so.
func walkModule(t *testing.T, fn func(rel string, fset *token.FileSet, f *ast.File)) {
	t.Helper()
	root := filepath.Join("..", "..")

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
			case ".git", "bin", "testdata", "web":
				return fs.SkipDir
			}
			if rel == "assets" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, top := range []string{"internal", "cmd", "test"} {
			if rel == top || strings.HasPrefix(rel, top+string(filepath.Separator)) {
				fset := token.NewFileSet()
				f, perr := parser.ParseFile(fset, path, nil, 0)
				if perr != nil {
					return perr
				}
				fn(rel, fset, f)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
}
