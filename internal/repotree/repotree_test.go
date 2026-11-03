package repotree

import (
	"os"
	"path/filepath"
	"testing"
)

// mkdir makes dir under base and returns its path.
func mkdir(t *testing.T, base string, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{base}, parts...)...)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNested_SeesAWorktreeACloneAndNothingElse is the whole of this package's
// behaviour, and the negative half is the half that matters.
//
// A prune that is too wide is worse than the bug it fixes: every guard that
// asks this question stops reading its own tree and reports success for a
// walk that visited nothing, which is the `AssertNoStray` failure. So the
// ordinary directory and the root itself are asserted beside the two shapes
// of nested checkout, and a change that made this function answer true more
// often has to turn one of them red.
func TestNested_SeesAWorktreeACloneAndNothingElse(t *testing.T) {
	root := t.TempDir()

	// What `git worktree add` leaves: a one-line FILE holding a gitdir
	// pointer. This is the shape the agent worktrees have and the shape the
	// defect was found through.
	worktree := mkdir(t, root, ".claude", "worktrees", "agent-1")
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// What `git clone` leaves: a directory. Somewhere a name-based skip would
	// never have been written for, which is the argument for not using one.
	clone := mkdir(t, root, "vendor", "somebodys-fork")
	mkdir(t, clone, ".git")

	ordinary := mkdir(t, root, "internal", "cli")

	for _, c := range []struct {
		name string
		dir  string
		want bool
	}{
		{"a worktree, whose .git is a file", worktree, true},
		{"a clone, whose .git is a directory", clone, true},
		{"an ordinary package directory", ordinary, false},
		{"the parent of a worktree", filepath.Join(root, ".claude", "worktrees"), false},
		{"the root being walked", root, false},
		// The root reached by a different spelling of the same path. WalkDir
		// hands the root back exactly as given, so this is the case a caller
		// could introduce rather than one the walk produces -- and the answer
		// must still be "not nested", because pruning the root prunes
		// everything.
		{"the root, spelled with a trailing separator", root + string(filepath.Separator), false},
	} {
		if got := Nested(root, c.dir); got != c.want {
			t.Errorf("Nested(root, %s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestNested_DoesNotPruneTheRootEvenWhenItIsACheckout is the one case that
// cannot be folded into the table above, because it needs the root to carry a
// .git of its own -- which every real caller's root does.
//
// Reinstated by deleting the root comparison: this goes red and nothing else
// in the repository does, because every guard's walk would then prune its
// first directory and pass having read no files.
func TestNested_DoesNotPruneTheRootEvenWhenItIsACheckout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Nested(root, root) {
		t.Fatal("the root of the walk was reported as a nested checkout; every guard that asks would then prune its own tree and report success for having read nothing")
	}
}

// TestNested_AStatThatFailsForAnotherReasonReadsAsOurOwn pins the direction
// the error path answers in.
//
// A directory ember cannot stat into must read as this tree's: skipping it
// would hide source from whichever guard asked, silently. The provocation is
// a `.git` path that cannot be a directory entry at all -- `.git` exists here
// as a FILE, so `Join(dir, ".git")` under it answers ENOTDIR rather than
// ENOENT, which is an error that is not "does not exist".
func TestNested_AStatThatFailsForAnotherReasonReadsAsOurOwn(t *testing.T) {
	root := t.TempDir()
	// root/notadir is a file, so root/notadir/.git is ENOTDIR.
	if err := os.WriteFile(filepath.Join(root, "notadir"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Nested(root, filepath.Join(root, "notadir")) {
		t.Fatal("a stat that failed for a reason other than absence was read as a nested checkout; the loud direction is to read the files")
	}
}
