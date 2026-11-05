package cli

import (
	"bytes"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestServe_WiresTheNodeCeilingIntoTheDrainLoop.
//
// WHY A GUARD AT ALL, and it is the cost of a decision taken deliberately.
// `queue.Queue.MaxNodes` reads anything below 1 as 1, because every
// `queue.Queue{}` literal in the suite predates the field and a zero has to
// keep today's behaviour -- which is what makes `max_nodes: 1` the same code
// path as 2 rather than a branch only one of the two exercises. The price is
// that a `cmd_serve` which FORGOT the field would silently pin an operator
// who wrote `max_nodes: 2` to one node, with nothing anywhere reporting it.
//
// THAT IS THE #105 CLASS EXACTLY. `webapi.Options` had no `Scan` field,
// nothing assigned the Server's `scan`, and `GET /api/nodes` answered 501 in
// every shipped `ember serve` -- for the life of the feature -- because the
// three assignments in the tree were all in test files reaching past the
// constructor. The suite could not tell "the production value is missing" from
// "this test did not set it", and that is true here too: every queue test
// builds its own literal.
//
// MAKING IT UNREPRESENTABLE WAS THE FIRST CHOICE AND IS NOT AVAILABLE. A
// pointer field refused as nil would catch "nobody wired it" and nothing
// else -- and the walkaround that matters is not a nil, it is a wiring that
// hands over a number from somewhere other than the operator's file, which is
// the same hole `TestServe_WiresTheDaemonsOwnConfigIntoTheSubmittedRunGate`
// was walked around four times before it asked the type checker.
//
// SO IT ASKS THE SAME QUESTION THAT ONE DOES, through the same
// `configObjects`: the value must be a selector on an object this function
// bound to a `config.Config` -- a parameter of that type, or a `:=` from
// `config.Load` -- and not one it wrote to. A shadow is a different object by
// construction, and `cfg = config.Config{}` or `cfg.MaxNodes = 0` above the
// literal is a write and is refused with the reason named. Everything that
// guard's six wrong wordings bought is reused here rather than re-derived.
//
// A POSITIONAL LITERAL IS A FATAL, which is
// `TestFlagTablesClassifyEveryFlagSetMethod`'s rule: the alternative to
// failing on a literal this guard cannot read is reading straight past the one
// that matters, which is one more gate that cannot fail in a repository that
// has already found several.
//
// AND THERE IS A FLOOR. A load that resolved nothing, or a `queue.Queue` that
// moved file, would otherwise leave this asserting about no literals at all.
func TestServe_WiresTheNodeCeilingIntoTheDrainLoop(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{
		// Production files only. The subject is what `ember serve` wires, and
		// every test in the tree builds its own Queue -- which is the whole
		// reason this guard exists.
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}, ".")
	if err != nil {
		t.Fatalf("load internal/cli: %v", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("internal/cli did not load cleanly (%d error(s)), so this guard cannot read it", n)
	}

	found := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				c := configObjects(pkg.TypesInfo, fn)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					lit, ok := n.(*ast.CompositeLit)
					if !ok || !isQueueQueue(lit) {
						return true
					}
					found++
					checkQueueMaxNodes(t, pkg.Fset, pkg.TypesInfo, lit, c)
					return true
				})
			}
		}
	}
	if found == 0 {
		t.Fatal("found no production queue.Queue to check, so this guard asserted nothing: " +
			"a zero MaxNodes reads as one node, so a literal that stopped naming it would " +
			"pin an operator who wrote max_nodes: 2 to a single rental in silence")
	}
}

// isQueueQueue reports whether a literal builds a queue.Queue.
//
// ON THE SPELLING, for isDirectRunner's reason: this decides which literals to
// LOOK AT, so a miss lands on the `found == 0` floor and fails, where
// isConfigConfig decides what to ACCEPT and a miss there would pass. An import
// alias is therefore a red at the floor and not a hole.
func isQueueQueue(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Queue" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "queue"
}

// checkQueueMaxNodes is the per-literal half.
func checkQueueMaxNodes(
	t *testing.T,
	fset *token.FileSet,
	info *types.Info,
	lit *ast.CompositeLit,
	c configBindings,
) {
	t.Helper()
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			t.Fatalf("%s: a queue.Queue is built with a positional field this guard cannot "+
				"read, so it cannot say whether max_nodes reaches the drain loop: name the fields",
				fset.Position(el.Pos()))
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "MaxNodes" {
			continue
		}

		sel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "MaxNodes" {
			t.Errorf("%s: MaxNodes is set from %s rather than from a config's own MaxNodes, so "+
				"the number this process holds rentals up to may not be the one in the "+
				"operator's file -- and a wrong one is silent, because anything below 1 reads "+
				"as one node",
				fset.Position(kv.Value.Pos()), exprText(fset, kv.Value))
			return
		}
		val, ok := sel.X.(*ast.Ident)
		if !ok {
			t.Errorf("%s: MaxNodes is read off %s, which this guard cannot resolve to a config "+
				"this function bound", fset.Position(sel.X.Pos()), exprText(fset, sel.X))
			return
		}
		obj := info.Uses[val]
		if obj == nil {
			t.Fatalf("%s: no object for %q, so this guard cannot tell which variable it is",
				fset.Position(val.Pos()), val.Name)
		}
		if how, moved := c.moved[obj]; moved {
			t.Errorf("%s: MaxNodes is read off %q and %s in the same function, so the value "+
				"may not be the one the operator's file holds -- a zero or a one there is a "+
				"queue pinned to a single rental with nothing reporting it",
				fset.Position(val.Pos()), val.Name, how)
			return
		}
		if !c.bound[obj] {
			t.Errorf("%s: MaxNodes is read off %q, which is not an object this function bound "+
				"to a config.Config -- a config.Config parameter, or a := from config.Load. A "+
				"SHADOW of the right name is a different variable and lands here",
				fset.Position(val.Pos()), val.Name)
		}
		return
	}
	t.Errorf("%s: this queue.Queue does not set MaxNodes, so it holds at most ONE rental "+
		"whatever max_nodes says: the field reads anything below 1 as 1, which is what keeps "+
		"max_nodes: 1 the same code path -- and is why forgetting it here is silent (#204, and "+
		"the #105 class)", fset.Position(lit.Pos()))
}

// exprText is an expression as it is written, for a message a reviewer can
// find in the file.
func exprText(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "<unprintable>"
	}
	return strings.TrimSpace(b.String())
}
