package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// stampVar is the symbol the build stamp has to name, spelled once.
const stampVar = "Version"

// stampPkg is this package's path below the module root, as an -X symbol
// spells it.
const stampPkg = "internal/cli"

// xSetting matches one `-X importpath.name=value` linker setting.
//
// The value is deliberately not captured: it is `$(VERSION)` in the Makefile
// and a shell expansion in a workflow, so there is nothing here to compare it
// against. What can be checked is the half that fails silently.
var xSetting = regexp.MustCompile(`-X\s+([^\s'"=]+)=`)

// TestVersionStampTargetsThisPackage is the guard for a failure mode with no
// symptoms.
//
// `-ldflags -X` is a NO-OP when the symbol it names does not exist. The linker
// does not warn, the build succeeds, and the binary carries the var's compiled
// default -- so renaming Version, moving this package, or mistyping the module
// path breaks the stamp invisibly and `ember version` goes back to answering
// what it answered for the whole life of the program before this: nothing
// anybody chose.
//
// It reads the Makefile and every workflow rather than trusting review, the
// shape TestNoListenOutsideLoopback uses, and checks three things that can
// drift apart: that something still passes a stamp at all, that what it names
// is this package and this var, and that the var is still a thing -X can
// write.
func TestVersionStampTargetsThisPackage(t *testing.T) {
	root := filepath.Join("..", "..")
	want := modulePath(t, root) + "/" + stampPkg + "." + stampVar

	files := []string{filepath.Join(root, "Makefile")}
	flows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("glob the workflows: %v", err)
	}
	files = append(files, flows...)

	found := 0
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range xSetting.FindAllStringSubmatch(string(b), -1) {
			found++
			if m[1] != want {
				t.Errorf("%s stamps %q, which no symbol in this module is called; -X is silent about that, so the build would simply stop carrying a version. Want %q", path, m[1], want)
			}
		}
	}
	// An empty result is the failure this test exists to catch, not a pass:
	// deleting the -ldflags line is exactly how `ember version` came to print
	// `dev` in every build that has ever existed.
	if found == 0 {
		t.Fatalf("no -X setting in the Makefile or any workflow, so nothing stamps %s; `ember version` will report a revision from build info at best and %q at worst", want, unstamped)
	}
}

// TestCIBuildsThroughMake keeps the stamp from being walked around.
//
// The workflow's build step was a bare `go build -o bin/ember ./cmd/ember`,
// spelled out beside a Makefile that said the same thing -- so a stamp added
// to one of them would have left the other producing an unstamped binary, and
// CI is the half nobody runs locally. One spelling of the build, as LAB_TAGS
// is one spelling of the tag list and for the same reason.
func TestCIBuildsThroughMake(t *testing.T) {
	flows, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("glob the workflows: %v", err)
	}
	if len(flows) == 0 {
		t.Fatal("found no workflows; the glob is broken, not the repository")
	}

	// `./cmd/ember` and not `./cmd/embernode`, which is a different binary
	// with no version surface and is built inside the images on purpose.
	bare := regexp.MustCompile(`go build[^\n]*\./cmd/ember(\s|$)`)
	for _, path := range flows {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if m := bare.FindString(string(b)); m != "" {
			t.Errorf("%s builds ember directly (%q); use `make build`, or the binary carries no version stamp", path, strings.TrimSpace(m))
		}
	}
}

// TestMakeBuildPassesLdflags: the variable existing is not the variable being
// used.
//
// GO_LDFLAGS could be defined, correct and referenced by nothing -- which is
// the same shape as a feature built end to end and never connected, and would
// leave every one of the assertions above passing over an unstamped build.
func TestMakeBuildPassesLdflags(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}

	builds := 0
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.Contains(line, " build ") || !strings.HasSuffix(line, "./cmd/ember") {
			continue
		}
		builds++
		if !strings.Contains(line, "-ldflags") {
			t.Errorf("the Makefile builds ember without -ldflags: %q", line)
		}
	}
	if builds == 0 {
		t.Fatal("no recipe in the Makefile builds ./cmd/ember; this test is reading the wrong file rather than reporting a pass")
	}
}

// modulePath reads the module line out of go.mod, so the expected symbol is
// not a second copy of a path that a rename would have to find twice.
func modulePath(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}

// TestVersion_StampWins also keeps Version a thing the linker can write.
//
// -X writes a package-level var of string type and silently does nothing to
// anything else, so a `const Version` would read correctly, compile, and
// produce an unstamped binary with no error from the linker. The assignment
// below is what makes that unrepresentable rather than tested: a const or a
// non-string type does not compile.
//
// An earlier version of this file asserted the same thing by parsing
// version.go for the declaration, which turned out to fire on a correct change
// -- moving the var to another file in this package breaks nothing, because -X
// names a package and not a file -- and a guard that fails on correct code is
// one people learn to ignore. TestNoListenOutsideLoopback carries the same
// note for the same reason.
func TestVersion_StampWins(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "v9.9.9-7-gdeadbeefcafe"
	if got := version(); got != Version {
		t.Errorf("version() = %q, want the stamp %q", got, Version)
	}

	var out bytes.Buffer
	if err := cmdVersion(nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != Version {
		t.Errorf("ember version printed %q, want %q", got, Version)
	}
}

// TestVersion_UnstampedFallsBack covers the whole ladder below the stamp.
//
// version() reads this process's own build info, which `go test` leaves
// without VCS settings, so the answer here is whatever the toolchain recorded
// -- and the only thing worth asserting is that it is not empty and not the
// stamp, since the stamp is empty.
func TestVersion_UnstampedFallsBack(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = ""
	if got := version(); got == "" {
		t.Error("version() returned the empty string; `ember version` would print a blank line")
	}
}

func TestFromBuildInfo(t *testing.T) {
	cases := []struct {
		name string
		bi   debug.BuildInfo
		want string
	}{
		{
			// What a plain `go build` in this repository records, measured:
			// the pseudo-version reads like a release at v0.0.0 and the
			// revision beside it is the truth.
			name: "revision beats a pseudo-version",
			bi: debug.BuildInfo{
				Main: debug.Module{Version: "v0.0.0-20261102192343-0f109263a709+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs", Value: "git"},
					{Key: "vcs.revision", Value: "0f109263a7091234567890abcdef1234567890ab"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			want: "0f109263a709-dirty",
		},
		{
			name: "a clean tree carries no suffix",
			bi: debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0f109263a7091234567890abcdef1234567890ab"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			want: "0f109263a709",
		},
		{
			// `go install example.com/x@v1.2.3`: no VCS settings, and the
			// module version is a real tag.
			name: "a resolved module version is an answer",
			bi:   debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}},
			want: "v1.2.3",
		},
		{
			name: "a pseudo-version alone is not an answer",
			bi:   debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261102192343-0f109263a709"}},
			want: "",
		},
		{
			name: "(devel) names nothing",
			bi:   debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want: "",
		},
		{
			name: "nothing at all",
			bi:   debug.BuildInfo{},
			want: "",
		},
		{
			// A revision shorter than the abbreviation is used whole rather
			// than indexed past its end.
			name: "a short revision is not truncated",
			bi: debug.BuildInfo{
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
			},
			want: "abc123",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bi := c.bi
			if got := fromBuildInfo(&bi); got != c.want {
				t.Errorf("fromBuildInfo = %q, want %q", got, c.want)
			}
		})
	}
}

func TestIsPseudoVersion(t *testing.T) {
	yes := []string{
		"v0.0.0-20261102192343-0f109263a709",
		"v0.0.0-20261102192343-0f109263a709+dirty",
		// Built on a tag, which is the form that reads most like a release.
		"v1.2.3-0.20261102192343-0f109263a709",
	}
	no := []string{
		"v1.2.3",
		"v1.2.3-rc1",
		"",
		"0f109263a709",
		// The right shape with the wrong characters in it: a timestamp that
		// is not digits, and a hash that is not hex.
		"v0.0.0-2026110219234x-0f109263a709",
		"v0.0.0-20261102192343-0f109263a70z",
		// The right characters at the wrong lengths.
		"v0.0.0-2026110219234-0f109263a709",
		"v0.0.0-20261102192343-0f109263a70",
	}
	for _, v := range yes {
		if !isPseudoVersion(v) {
			t.Errorf("isPseudoVersion(%q) = false; it would be printed as if somebody had tagged it", v)
		}
	}
	for _, v := range no {
		if isPseudoVersion(v) {
			t.Errorf("isPseudoVersion(%q) = true, so a real version would be thrown away", v)
		}
	}
}
