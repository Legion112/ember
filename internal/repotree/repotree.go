// Package repotree answers one question about a walk of this repository's own
// source: does this directory belong to the checkout being walked, or to a
// different one nested inside it?
//
// Several of ember's structural guards walk from the module root and read
// every Go file they find -- TestComments_CiteNoFlagThatNoFlagSetDeclares,
// TestNoInsecureIgnoreHostKey, TestEveryBuildTagReachesTheGates. That was
// sound while this repository was one checkout. Agent worktrees put several on
// one machine, each a whole second copy of the tree, and Claude Code cuts them
// at `.claude/worktrees/<name>/` -- INSIDE the checkout it cut them from. So a
// guard walking from the root reads somebody else's source as this tree's: on
// master at 927ef78 the comment-citation guard reported 457 findings, every
// one of them a correct complaint about a file in another worktree, and
// `go test ./...` failed for every developer who had ever delegated a task.
// It is invisible in CI, which checks out fresh.
//
// This is the `LAB_LOG_DIR` defect one axis over. That one was a lab log
// `tee`d to a fixed /tmp path, so a second worktree's run truncated the file
// the first run's SKIP gate was about to grep -- an assertion reading the
// wrong file. Here the wrong file is reached through a tree walk instead of a
// path, and the root cause is the same one the daemon-wide `Reap` had: safe
// while there was one checkout, unsafe the moment there were four.
//
// WHY A `.git` ENTRY AND NOT THE NAME `.claude`. The thing to skip is not
// where Claude Code happens to put a worktree; it is anything that is its own
// checkout. A stray clone under `vendor/`, a scratch copy somebody made with
// `cp -a`, the next tool's own directory -- all the same defect with a
// different name, and a guard keyed on one spelling catches only the one
// somebody remembered. AGENTS.md's rule is that a gate's input is derived from
// the tree rather than maintained beside it, and the tree says what a checkout
// is: it has its own `.git`.
//
// It cannot be a `go.mod` test instead, although a nested checkout of THIS
// repository has one. go.mod answers "is this a different module", which is
// true of a Go module vendored on purpose and false of a checkout of
// something that is not Go -- and the files these guards read are source,
// which belongs to whoever checked it out.
//
// It is a package and not three copies of four lines because there is no
// guard available for the copies. "Every whole-tree walk prunes" is not
// derivable: telling a walk of the module root from `internal/app`'s walk of
// a temp output directory needs to know what the root argument evaluates to,
// and a check keyed on the spelling `filepath.WalkDir` would fire on both --
// which is TestNoListenOutsideLoopback's own first version flagging
// `loopback.Listen`. One function with one test is the only shape here whose
// correctness anything can assert.
package repotree

import (
	"os"
	"path/filepath"
)

// Nested reports whether dir is the root of a checkout of its own, and so
// holds source that does not belong to the tree rooted at root.
//
// dir is a directory as filepath.WalkDir yields it for a walk rooted at root,
// which is what makes the root comparison a string comparison: WalkDir passes
// the root back exactly as it was given. A caller walking something else wants
// a different function, not a looser one here.
//
// A `.git` of EITHER kind counts. `git worktree add` writes a one-line file
// holding a gitdir pointer, which is what the agent worktrees this exists for
// have; `git clone` writes a directory. os.Stat does not care, and a check
// that looked for only one of them would have a hole shaped exactly like the
// case it was written for.
//
// AN ERROR THAT IS NOT "DOES NOT EXIST" ANSWERS false, which is the loud
// direction. Answering true on a stat ember could not make would skip a
// directory of this tree's own source and turn whichever guard asked into one
// that cannot fail -- a dead gate, which this repository has now found four of
// and records each. Answering false reads the files, and if one of them really
// is another checkout's the failure names the path.
func Nested(root, dir string) bool {
	// The root being walked is a checkout too -- the one whose source every
	// caller is asking about -- so it is never the answer. Without this the
	// first directory of every walk is pruned and every guard goes silent.
	if filepath.Clean(dir) == filepath.Clean(root) {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
