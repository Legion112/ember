package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLabLogDirIsKeyedToTheWholeCheckoutPath checks that two checkouts cannot
// share a lab log, by asking make for the path from two directories of the
// same name.
//
// The log is READ BACK: every lab target tees `go test` into it and then greps
// it for `--- SKIP`, which is the gate that exists because a lab reporting
// success for something that never ran is worse than a red build. `tee`
// truncates when its pipeline starts, which is before `labdaemon.Acquire` has
// taken the daemon lock -- so two runs sharing a path means the first run's
// gate greps a file the second one emptied, and an assertion reading the wrong
// file is how the journal's "nodes" subdirectory went unchecked.
//
// It has been wrong twice. First four fixed paths under /tmp; then
// `$(notdir $(CURDIR))`, which is `ember` for every clone this repository has
// -- measured on three checkouts, all three printing `/tmp/ember-labs-ember`
// (#112). Both times the comment above the line claimed the checkout while the
// code named something weaker, which is why this is a test and not a third
// comment: a sentence asserting a property is worth exactly what its exit code
// is.
//
// It EVALUATES the Makefile rather than reading it, which is the opposite of
// what the other guards in this package do, and the reason is that the defect
// was in an evaluation and not in a spelling. A ban on `$(notdir` would fire
// on a correct use of it -- a readable prefix in front of a digest is fine --
// which is the false positive `TestNoListenOutsideLoopback` records about its
// own first version, where it flagged `loopback.Listen`. What has to hold is
// that two paths differ, and only make can say.
func TestLabLogDirIsKeyedToTheWholeCheckoutPath(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	makefile := filepath.Join(root, makefileName)
	if _, err := os.Stat(makefile); err != nil {
		t.Fatalf("stat %s: %v", makefile, err)
	}

	// The same basename under two parents, which is what `git clone` and
	// `cp -r` produce and what the old derivation could not tell apart.
	base := t.TempDir()
	a := labLogDirFrom(t, makefile, filepath.Join(base, "parentA", "ember"))
	b := labLogDirFrom(t, makefile, filepath.Join(base, "parentB", "ember"))

	if a == b {
		t.Errorf("two checkouts both named ember print the same lab log directory %q.\n"+
			"They then tee into one file and grep it back, so the first run's --- SKIP gate reads "+
			"a log the second run truncated -- which is the assertion-reads-the-wrong-file defect, "+
			"on the gate that exists because a skipped lab reports success for something that never ran.", a)
	}

	// A name with a space in it, because LAB_CHECK's
	// `grep -q -- '--- SKIP' $(1)` is unquoted: a log path containing one
	// makes the gate read two filenames, and neither of them is the log. The
	// directory is a legal name a developer can produce by hand, so the
	// property is that the derivation does not carry it through.
	spaced := labLogDirFrom(t, makefile, filepath.Join(base, "two words", "em ber"))
	if strings.ContainsAny(spaced, " \t") {
		t.Errorf("a checkout at a path with a space in it prints the lab log directory %q, "+
			"and the --- SKIP grep that reads it back is unquoted: it would be handed two filenames "+
			"and report that it could not look", spaced)
	}
}

// labLogDirFrom runs `make print-LAB_LOG_DIR` with dir as the working
// directory, which is what sets $(CURDIR).
//
// `-f` against the real Makefile rather than a copy, so this cannot pass
// against a file that has drifted from the one the lab targets use.
func labLogDirFrom(t *testing.T, makefile, dir string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	// `--no-print-directory` and a scrubbed MAKEFLAGS, because this test is
	// itself run from `make test`: a sub-make turns `--print-directory` on by
	// itself, so MAKELEVEL in the environment put two `Entering directory`
	// lines on STDOUT around the value and the first version of this read the
	// three of them as the path. It passed under a bare `go test` and failed
	// under make, which is the way round that hides. MFLAGS and MAKEFLAGS go
	// too: whatever the outer make was invoked with is not this measurement's
	// input.
	cmd := exec.Command("make", "--no-print-directory", "-f", makefile, "print-LAB_LOG_DIR")
	cmd.Dir = dir
	cmd.Env = []string{}
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "MAKEFLAGS="), strings.HasPrefix(kv, "MAKELEVEL="), strings.HasPrefix(kv, "MFLAGS="):
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Fatal and never a skip, including when make itself is missing: this
	// repository is driven by that Makefile, and a guard that reports success
	// because it could not run is the defect it is written about.
	if err := cmd.Run(); err != nil {
		t.Fatalf("make print-LAB_LOG_DIR in %s: %v\nstdout: %s\nstderr: %s", dir, err, out.String(), errb.String())
	}

	// Exactly one line, or this is reading something that is not the value.
	// An empty value is one path shared by every checkout -- the defect
	// rather than an error -- which is what the Makefile's own $(error)
	// guard is for; asserted here too, because a guard comparing two empty
	// strings would report them as equal and blame the derivation.
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) != 1 {
		t.Fatalf("make printed %d lines for LAB_LOG_DIR in %s, want 1: %q\nstderr: %s",
			len(lines), dir, out.String(), errb.String())
	}
	return lines[0]
}
