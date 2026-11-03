package cli

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestComments_CiteNoFlagThatNoFlagSetDeclares is the third direction of the
// flag guard, and the only one of the three that can see a comment.
//
// An `--all` on an `ember ls` has never existed. `cmd_ls.go` declares one
// flag, `--config`, and `runLs` has asked every configured provider since it
// was written. TestREADME_DocumentsEveryFlag found that invocation in README
// on its first run, #100 swept it out of README and AGENTS.md -- and EIGHTEEN
// Go comments kept citing it, as the authority for decisions that are
// otherwise correct. The sweep covered the two documents because that is
// where the guard looked. Nothing looked at comments.
//
// Which is the failure AGENTS.md records twice at one remove: a comment that
// justifies the entry somebody remembered cannot catch the entry somebody
// forgets, and `node-image.yml` published a moving tag for the life of the
// image UNDERNEATH a comment arguing against one. Here it is the citation
// itself that is fiction, which is the worst combination for a reader -- the
// argument persuades and the command does not exist, so an operator who tries
// it gets a usage error from the tool the comment told them to trust.
//
// THE GUARD WAS NOT EXPECTED TO BE WORTH BUILDING, and the measurement said
// otherwise. #93 tried three rules for harvesting flag tokens out of prose
// and two of them condemned `git describe --tags` and an sd-server
// `--listen-ip` because those sat near the word *ember*; comments are a
// larger and less curated surface than README and legitimately name four
// other programs' flags. So the expected answer was "no guard, and the reason
// written down" -- the call #97 made for a rule it could not express cheaply.
//
// What makes this one cheap is that the rule is NOT NEW. `emberFlags`
// is the window #93 settled on after those three attempts: from the word
// `ember` to the end of the line or the next backtick, whichever comes first,
// and two dashes rather than one.
//
// Run over every comment in the tree against `declaredFlags`, on the tree as
// it stood: **48 flags cited at ember, 30 of them declared, and 18 misses --
// every single miss `--all`.** Not one false positive. The four flags the
// pessimistic case named are reached by nothing: `docker run --memory`,
// `sd-cli --list-devices`, `nvidia-smi --query-gpu` and `git describe --tags`
// all appear in comments here, and none of them has the word `ember` between
// the start of its line and its own backticked span. After the sweep it is
// 30 cited, 30 declared, 0 misses.
//
// WHAT IT GIVES UP is a flag the word ember does not reach on its own line:
// one on a later line of the same comment, and one the window's own backtick
// bound cuts off. That bound is what buys the precision above, so the two are
// the same decision and not two -- and the guard made the point about itself
// before anybody read it. The first draft of this comment claimed a prose
// citation with no backticks would escape, and the test failed on that very
// sentence: with no backtick anywhere on the line the window runs to the end
// of it, so prose is caught and a span is where the cut can happen. Measured,
// not reasoned, is the whole convention here, and this one cost one run.
//
// ONE DIRECTION ONLY, unlike the two guards above it. "A flag no comment
// mentions" is not a defect: a comment is not documentation, nothing obliges
// one to name a flag, and that direction would be a test demanding prose.
// README's guard has the second direction because README is the document
// AGENTS.md's last line names.
//
// WHEN IT FIRES FALSELY the remedy is not house style. The message asks for
// the other program's flag to be given a backticked span of its own, which is
// what README:140 does for `git describe --tags` and what
// `internal/sdserver/sdserver.go` does for `--listen-ip` -- and a reader who
// cannot tell whose flag a comment means is the ambiguity that produced this
// defect. Checked by reinstating one citation and by inventing a flag no
// FlagSet declares.
func TestComments_CiteNoFlagThatNoFlagSetDeclares(t *testing.T) {
	declared := declaredFlags(t)
	// A floor, not a count, for the reason TestREADME_DocumentsEveryFlag has
	// one: a parse that quietly matched nothing would make this pass over
	// anything.
	if len(declared) < 25 {
		t.Fatalf("found only %d flags in internal/cli; the parse is broken, not the package", len(declared))
	}

	cited := 0
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// PARSED, NOT GREPPED, for which lines are comments. A `--flag`
		// inside a string literal is argv ember builds and not a citation --
		// `sdserver.ServeCommand` appends `--listen-ip 127.0.0.1` as code --
		// and a guard that read those would condemn the program for naming
		// its own dependency's flags.
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, b, parser.ParseComments)
		if perr != nil {
			return perr
		}
		comment := map[int]bool{}
		for _, g := range f.Comments {
			for _, c := range g.List {
				at := fset.Position(c.Pos()).Line
				// A /* */ comment spans lines; a // one is a single line and
				// this loop runs once for it.
				for i := range strings.Split(c.Text, "\n") {
					comment[at+i] = true
				}
			}
		}

		// The same extraction README's stale direction uses, over the whole
		// file and filtered to the comment lines: one rule, measured once,
		// rather than a second spelling of it that can drift -- the journal's
		// "nodes" subdirectory, which was a literal in two places until they
		// disagreed and an assertion read the wrong directory.
		for _, m := range emberFlags(string(b)) {
			if !comment[m.line] {
				continue
			}
			cited++
			if _, ok := declared[m.name]; ok {
				continue
			}
			t.Errorf("%s:%d cites `--%s` at ember and no flag.FlagSet in internal/cli declares it. "+
				"Either the flag is gone and the comment is stale, or it belongs to another program "+
				"and wants a backticked span of its own -- README:140 spells `git describe --tags` "+
				"that way, and README:593 names a flag that does not exist without typing it as an "+
				"invocation", path, m.line, m.name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree for comments: %v", err)
	}

	// The other floor, and it is the one that matters: this test's whole
	// failure mode is a walk that visited nothing, which would report success
	// for a rule that never ran -- the `AssertNoStray` shape, where the
	// assertion passed because the cleanup had already emptied what it was
	// about to look at.
	if cited < 20 {
		t.Fatalf("only %d flags were cited at ember across every comment in the tree; "+
			"the walk or the extraction is broken, not the comments", cited)
	}
}
