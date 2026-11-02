package cli

import (
	"flag"
	"io"
	"runtime/debug"
	"strings"
)

// Version is the build stamp, written at link time by
//
//	-ldflags "-X github.com/Legion112/ember/internal/cli.Version=$(VERSION)"
//
// which `make build` and CI both pass; see the Makefile's VERSION.
//
// It used to default to "dev" and carry a comment claiming it was "overwritten
// at build time", and nothing anywhere overwrote it -- there was no -ldflags
// in the Makefile or in any workflow for the whole life of the program, so
// every ember that has ever existed answered `ember version` with `dev`. That
// is not cosmetic here: every measurement in README.md is a claim about what a
// specific build did, and none of them could name the build that produced it.
//
// The default is now empty rather than "dev" so that an unstamped build falls
// through to Go's own build info below instead of asserting a name. -X is a
// SILENT no-op when the symbol it names does not exist -- a renamed var or a
// moved package breaks the stamp with no error from the linker and no change
// in the build -- which is why TestVersionStampTargetsThisPackage parses the
// Makefile and the workflows and fails when what they name is not this var.
var Version = ""

// unstamped is what a build that cannot name itself reports.
//
// Reached when there is no -X stamp and no VCS information: `go build` outside
// a repository, or a tree with no .git. It is deliberately not a plausible
// version number -- a string that looked like a tag nobody created would be
// worse than admitting ignorance, which is the same argument this repository
// makes for refusing `:latest`.
const unstamped = "dev"

// revisionLen is how much of a commit hash a version string carries.
//
// Twelve, matching the --abbrev=12 the Makefile passes to git describe, so the
// two paths into a version string abbreviate a revision identically: a build
// stamped from the Makefile and one that fell through to build info below
// cannot be told apart by their shape, only by their contents.
const revisionLen = 12

// version answers what this build is, in order of how much the answer is
// worth.
//
//  1. The -X stamp, which is `git describe --tags --always --abbrev=12 --dirty`
//     and therefore names a tag when there is one.
//  2. Go's own build information: the revision a plain `go build` inside a
//     repository records, or the module version `go install example.com/x@v1.2.3`
//     resolved. This exists so that forgetting the Makefile degrades to the
//     truth rather than to a lie: the defect being prevented is the one above,
//     an unwired mechanism that nobody noticed for the life of the program.
//  3. unstamped.
func version() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := fromBuildInfo(bi); v != "" {
			return v
		}
	}
	return unstamped
}

// fromBuildInfo reads a version out of Go's build information.
//
// THE VCS SETTINGS ARE READ FIRST AND bi.Main.Version SECOND, which is the
// opposite of the obvious order and the point of this function.
//
// A plain `go build ./cmd/ember` in this checkout reports a main-module version
// of `v0.0.0-20261102192343-0f109263a709+dirty` -- a Go pseudo-version, which
// is a real encoding of a real revision and also reads exactly like a release
// tagged v0.0.0. Preferring it would have made an unstamped build name a tag
// nobody cut, which is the one outcome worse than printing `dev`. Measured by
// building both ways, not by reading the documentation: the earlier version of
// this function special-cased only "(devel)", which is what older toolchains
// recorded and what this one does not.
//
// Pure, and takes the *debug.BuildInfo rather than reading it, because a test
// binary carries build info of its own and `go test` stamps no VCS settings
// into it. Asserting on what this process happens to report would be asserting
// on the toolchain's mood, so the decisions are tested against BuildInfo values
// written by hand instead.
func fromBuildInfo(bi *debug.BuildInfo) string {
	var revision, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		// No VCS stamp: a module resolved at a version, which `go install
		// example.com/x@v1.2.3` records here and which is then the best name
		// this build has. "(devel)" and the pseudo-versions above name nothing
		// an operator could check out, so they are not answers.
		if v := bi.Main.Version; v != "" && v != "(devel)" && !isPseudoVersion(v) {
			return v
		}
		return ""
	}
	if len(revision) > revisionLen {
		revision = revision[:revisionLen]
	}
	// The same "-dirty" suffix git describe writes, so an operator comparing a
	// run's reported version against their working tree reads one spelling
	// rather than two. A build from a modified tree is the one whose
	// measurements cannot be reproduced from anything in the repository, which
	// is the whole reason to say so.
	if modified == "true" {
		revision += "-dirty"
	}
	return revision
}

// isPseudoVersion reports whether v is a version the go command synthesised
// from a revision rather than one somebody tagged.
//
// The shape is `...-<14 digits>-<12 hex>`, a UTC timestamp and an abbreviated
// hash, optionally with Go's own `+dirty` build metadata after it. Matched on
// the shape rather than on the `v0.0.0` prefix, because a repository that has
// ever been tagged gets pseudo-versions built on that tag --
// `v1.2.3-0.20261102192343-0f109263a709` -- and those read even more like a
// release than the untagged form does.
//
// Deliberately not golang.org/x/mod/module.IsPseudoVersion: a dependency for
// one predicate, and the exact classification does not matter here. What
// matters is not printing a tag-shaped string for an untagged build, so a
// false positive costs a fallback to the revision -- which is what this
// function's caller wanted anyway -- and never a wrong name.
func isPseudoVersion(v string) bool {
	v = strings.TrimSuffix(v, "+dirty")
	parts := strings.Split(v, "-")
	if len(parts) < 3 {
		return false
	}
	stamp, hash := parts[len(parts)-2], parts[len(parts)-1]
	// The timestamp carries a `0.` prefix -- or a whole pre-release label and
	// then `.0.` -- when the pseudo-version was built on a tag, so it is the
	// last dot-separated component that has to be the fourteen digits.
	stamp = stamp[strings.LastIndex(stamp, ".")+1:]
	if len(stamp) != 14 || len(hash) != 12 {
		return false
	}
	for _, r := range stamp {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range hash {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func cmdVersion(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	outf(out, "%s\n", strings.TrimSpace(version()))
	return nil
}
