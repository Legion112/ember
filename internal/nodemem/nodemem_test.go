package nodemem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
)

func TestParse_ReadsBothViewsFromTheUnifiedHierarchy(t *testing.T) {
	// What a cgroup v2 container on a large host answers, which is the shape
	// the FLUX.2 nodes would have had: a limit far below the host's memory,
	// and a host figure that made every one of them look like it had hundreds
	// of gigabytes spare.
	got := Parse(strings.Join([]string{
		"cgroup2_max 68719476736",
		"cgroup2_current 13421772800",
		"mem_total_kb 741343232",
		"mem_cached_kb 50331648",
		"assets 60129542144",
	}, "\n"))

	if got.CgroupVersion != 2 {
		t.Errorf("cgroup version = %d, want 2", got.CgroupVersion)
	}
	if got.CgroupMax != 64<<30 {
		t.Errorf("limit = %d, want 64 GiB", got.CgroupMax)
	}
	if got.CgroupCurrent != 12.5*(1<<30) {
		t.Errorf("current = %d", got.CgroupCurrent)
	}
	// Kibibytes in, bytes out: one unit above this package, so nothing
	// downstream has to remember which fields were scaled.
	if got.HostTotal != 741343232*1024 {
		t.Errorf("host total = %d, want MemTotal scaled to bytes", got.HostTotal)
	}
	if got.HostCached != 50331648*1024 {
		t.Errorf("host cached = %d", got.HostCached)
	}
	if got.Assets != 56<<30 {
		t.Errorf("assets = %d, want 56 GiB", got.Assets)
	}

	// The whole hypothesis in one assertion: the ceiling is the cgroup's and
	// not the host's, so the weights do not fit although `free` says they
	// would eleven times over.
	//
	// ASKED OF node.Memory, WHICH IS THE CODE AN OPERATOR'S HEADROOM COLUMN
	// GOES THROUGH. This package used to answer it itself, with four lines
	// nothing called, and this assertion passed against them while the rule
	// the ledger applies went unexercised on exactly this case.
	if c := memory(got).Ceiling(); c != 64<<30 {
		t.Errorf("Ceiling = %d, want the cgroup's limit and not the host's", c)
	}
	h, ok := memory(got).Headroom()
	if !ok {
		t.Fatal("headroom is unknowable with both figures present")
	}
	if h != (64<<30)-(56<<30) {
		t.Errorf("headroom = %d, want 8 GiB", h)
	}
}

// memory is this report's figures as the rule's own type.
//
// Spelled out here rather than added to Report as a method, because a method
// whose only caller is a test is the defect this package just had: nothing on
// the live-node side acts on the derived answers yet, and the verdict that
// would is deliberately deferred until a rental has happened. By field name,
// never positionally -- the order of the two counts IS the rule.
func memory(r Report) node.Memory {
	return node.Memory{CgroupLimit: r.CgroupMax, HostTotal: r.HostTotal, Assets: r.Assets}
}

// The v1 fallback, which #30 asks for by name: the paths are named rather
// than derived, because a guess at where a hierarchy is mounted reports "no
// limit" on the one kernel it is wrong about -- and "no limit" is the answer
// that makes a node look fine.
func TestParse_FallsBackToTheV1Hierarchy(t *testing.T) {
	got := Parse(strings.Join([]string{
		"cgroup1_max 34359738368",
		"cgroup1_current 1073741824",
		"mem_total_kb 65536000",
		"assets 10737418240",
	}, "\n"))

	if got.CgroupVersion != 1 {
		t.Errorf("cgroup version = %d, want 1", got.CgroupVersion)
	}
	if got.CgroupMax != 32<<30 {
		t.Errorf("limit = %d, want 32 GiB from the v1 hierarchy", got.CgroupMax)
	}
	if got.CgroupCurrent != 1<<30 {
		t.Errorf("current = %d, want 1 GiB", got.CgroupCurrent)
	}
}

// v2 wins where both answered: a system on the unified hierarchy may still
// mount a v1 compatibility tree whose numbers bound nothing.
func TestParse_PrefersTheUnifiedHierarchyOverTheCompatibilityOne(t *testing.T) {
	got := Parse(strings.Join([]string{
		"cgroup2_max 68719476736",
		"cgroup2_current 2147483648",
		"cgroup1_max 9223372036854771712",
		"cgroup1_current 999",
		"mem_total_kb 65536000",
	}, "\n"))

	if got.CgroupVersion != 2 || got.CgroupMax != 64<<30 {
		t.Errorf("read v%d limit %d, want v2's 64 GiB", got.CgroupVersion, got.CgroupMax)
	}
	if got.CgroupCurrent != 2<<30 {
		t.Errorf("current = %d, want v2's", got.CgroupCurrent)
	}
}

// "No limit" has three spellings and they all mean the same thing.
func TestParse_ReadsEverySpellingOfNoLimit(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		version int
	}{
		// cgroup v2 writes the literal word, which #30 asks be covered.
		{"v2 says max", "cgroup2_max max", 2},
		// v1 has no "max": an unlimited controller reports PAGE_COUNTER_MAX,
		// which is int64's top with the low page bits cleared.
		{"v1 writes a sentinel", "cgroup1_max 9223372036854771712", 1},
		// And some kernels write -1 there instead, which parses as a NUMBER
		// rather than failing -- so the refusal of a negative count is what
		// stops it becoming a limit of minus one gibibyte, and every
		// comparison against it being wrong in the direction that says a
		// node is fine.
		{"v1 writes -1", "cgroup1_max -1", 1},
		// And a file that exists but is empty.
		{"an empty file", "cgroup2_max ", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(c.line + "\nmem_total_kb 65536000\nassets 1073741824\n")
			if got.CgroupMax != 0 {
				t.Errorf("limit = %d, want zero for an unlimited cgroup", got.CgroupMax)
			}
			// Zero means "no limit" and not "a limit of nothing", so the
			// ceiling falls back to the host's total rather than reading as a
			// container that cannot allocate anything. This is the join
			// between the three spellings and the rule: the parser's answer
			// is handed to the code the ledger uses.
			if c := memory(got).Ceiling(); c != 65536000*1024 {
				t.Errorf("Ceiling = %d, want the host's total", c)
			}
			if _, ok := memory(got).Headroom(); !ok {
				t.Error("headroom is unknowable although the host's total is known")
			}
		})
	}
}

// An empty value for cgroup2_max still says the hierarchy was there, which is
// what tells "read and unlimited" from "not read at all".
func TestParse_AnEmptyValueStillNamesTheHierarchy(t *testing.T) {
	if got := Parse("cgroup2_max \n").CgroupVersion; got != 2 {
		t.Errorf("cgroup version = %d, want 2 from a file that was readable and empty", got)
	}
	if got := Parse("mem_total_kb 100\n").CgroupVersion; got != 0 {
		t.Errorf("cgroup version = %d, want 0 where neither hierarchy answered", got)
	}
}

// A node that answered nothing is distinguishable from one with no limit, and
// that distinction is what stops a row of zeros reading as "this node was
// fine".
func TestReport_ZeroIsNotAnAnswer(t *testing.T) {
	var r Report
	if !r.IsZero() {
		t.Error("a zero Report does not report itself as empty")
	}
	// IsZero is what this package owes its caller -- app.reportMemory records
	// nothing over an empty report, because writing zeros would put a row in
	// the history claiming facts nobody read. What those zeros then MEAN to a
	// ceiling is node.Memory's, and its own table covers every quadrant,
	// including the one this test used to be the only cover for: a limit with
	// no weights beside it answers nothing.
	if c := memory(r).Ceiling(); c != 0 {
		t.Errorf("Ceiling = %d over nothing read, want zero", c)
	}
	if _, ok := memory(r).Headroom(); ok {
		t.Error("headroom is knowable over nothing read")
	}
}

func TestParse_IgnoresWhatItDoesNotKnow(t *testing.T) {
	// A node image with a wrapper that prints a banner must not turn a read
	// into a failure, and a kernel printing something unexpected in a file
	// ember only reads for information must not either.
	got := Parse(strings.Join([]string{
		"Welcome to the node",
		"cgroup2_max not-a-number",
		"cgroup2_current 1048576",
		"something_else 5",
		"",
		"mem_total_kb 1024",
	}, "\n"))
	if got.CgroupVersion != 2 {
		t.Errorf("cgroup version = %d, want 2", got.CgroupVersion)
	}
	if got.CgroupMax != 0 {
		t.Errorf("limit = %d, want zero for a value that is not a number", got.CgroupMax)
	}
	// A negative count is refused rather than carried, which is the case
	// strconv does NOT reject for us: ParseInt("-1") succeeds.
	if neg := Parse("cgroup2_current -1\n").CgroupCurrent; neg != 0 {
		t.Errorf("current = %d from a negative value, want zero", neg)
	}
	if got.CgroupCurrent != 1<<20 {
		t.Errorf("current = %d, want the line that did parse", got.CgroupCurrent)
	}
	if got.HostTotal != 1024*1024 {
		t.Errorf("host total = %d", got.HostTotal)
	}
}

// THE ASSET PATHS ARE ARGUMENTS AND NOT SCRIPT TEXT. A remote command is argv
// and every word is quoted, so a path that reached the script body would be
// shell text assembled from a config file.
func TestCommand_PassesTheAssetPathsAsArguments(t *testing.T) {
	cmd := Command([]string{"/models/a.gguf", "/models/b with space.gguf"})
	if len(cmd) != 6 {
		t.Fatalf("Command = %q, want sh -c <script> $0 and two paths", cmd)
	}
	if cmd[0] != "sh" || cmd[1] != "-c" {
		t.Errorf("Command starts %q %q, want sh -c", cmd[0], cmd[1])
	}
	if cmd[2] != Script {
		t.Error("the script is not the one this package declares")
	}
	for _, p := range []string{"/models/a.gguf", "/models/b with space.gguf"} {
		if !slicesContains(cmd[4:], p) {
			t.Errorf("Command does not pass %q as its own argument: %q", p, cmd)
		}
		if strings.Contains(Script, p) {
			t.Errorf("the path %q is in the script body, where it would be shell text", p)
		}
	}

	// And the whole thing survives being turned into a remote command, which
	// is what quotes each word.
	c := sshx.Cmd(cmd[0], cmd[1:]...)
	if err := c.Validate(); err != nil {
		t.Fatalf("the probe is not a sendable remote command: %v", err)
	}
	if !strings.Contains(c.String(), "'/models/b with space.gguf'") {
		t.Errorf("a path with a space is not quoted as one word: %s", c.String())
	}
}

func slicesContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestAssetPaths_JoinsTheMountAndTheAssetName(t *testing.T) {
	mount, err := node.MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	got, err := AssetPaths(mount, []node.Asset{
		{As: "flux2.gguf", URL: "https://example.test/f", SHA256: strings.Repeat("a", 64)},
		{As: "text/encoder.gguf", URL: "https://example.test/t", SHA256: strings.Repeat("b", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/models/flux2.gguf", "/models/text/encoder.gguf"}
	if len(got) != len(want) {
		t.Fatalf("AssetPaths = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A run with no assets is not an error: a baked image or a local container has
// nothing on a store, and the memory facts are still worth having.
func TestAssetPaths_AreEmptyWithoutAStoreOrAssets(t *testing.T) {
	mount, err := node.MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := AssetPaths(mount, nil); err != nil || got != nil {
		t.Errorf("AssetPaths with no assets = %q, %v", got, err)
	}
	if got, err := AssetPaths(node.Mount{}, []node.Asset{{As: "a"}}); err != nil || got != nil {
		t.Errorf("AssetPaths with no mount = %q, %v", got, err)
	}
}

func TestAssetPaths_RefusesAnAssetItCannotName(t *testing.T) {
	mount, err := node.MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AssetPaths(mount, []node.Asset{{As: "../etc/passwd"}}); err == nil {
		t.Error("an asset escaping the mount was turned into a path")
	}
}

// fakeRunner answers the probe without a node.
type fakeRunner struct {
	out  string
	code int
	err  error
}

func (f fakeRunner) Run(_ context.Context, _ sshx.Command) (io.ReadCloser, func() error, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return nopCloser{strings.NewReader(f.out)}, func() error {
		if f.code != 0 {
			// The real type, so the exit code reaches workload.Run's own
			// classification rather than being invented here.
			return &sshx.ExitError{Cmd: "sh -c ...", Code: f.code}
		}
		return nil
	}, nil
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func TestProbe_ReadsWhatTheNodeAnswered(t *testing.T) {
	r := fakeRunner{out: "cgroup2_max 1073741824\nmem_total_kb 1024\nassets 2147483648\n"}
	got, err := Probe(t.Context(), r, []string{"/models/a.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	if got.CgroupMax != 1<<30 || got.Assets != 2<<30 {
		t.Errorf("Probe = %+v", got)
	}
	// The interesting case: the weights are twice the limit, so this node
	// re-read the model every step.
	h, ok := memory(got).Headroom()
	if !ok || h != -(1<<30) {
		t.Errorf("headroom = %d, %v; want -1 GiB over a node that cannot hold the model", h, ok)
	}
}

func TestProbe_ReportsATransportFailureRatherThanGuessing(t *testing.T) {
	_, err := Probe(t.Context(), fakeRunner{err: errors.New("connection reset")}, nil)
	if err == nil {
		t.Fatal("a probe that could not run reported facts anyway")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("err = %v, want the transport's own reason", err)
	}
}

func TestProbe_ReportsANodeThatRefusedTheRead(t *testing.T) {
	r := fakeRunner{out: "sh: stat: not found\n", code: 127}
	_, err := Probe(t.Context(), r, []string{"/models/a.gguf"})
	if err == nil {
		t.Fatal("a probe that exited non-zero reported facts anyway")
	}
	// The node's own complaint, because a probe that failed with "exit 127"
	// and nothing else is a diagnostic nobody can act on.
	if !strings.Contains(err.Error(), "stat: not found") {
		t.Errorf("err = %v, want the node's own output", err)
	}
}

// The output bound, which exists for the reason gpucheck's does: a node that
// prints megabytes is misconfigured, and holding it in memory to maybe record
// it later would be the bug.
//
// Asserted through the FAILURE path, because that is where the kept bytes are
// observable: a non-zero exit hands them back in the error. The first version
// of this test only checked that the parse still worked, which raising
// maxOutput did not break -- so the bound was not covered by anything, which
// the reinstatement pass found.
func TestProbe_BoundsWhatItKeeps(t *testing.T) {
	// More than maxOutput of noise, with the real answer at the front so the
	// parse still has something to read.
	noise := "cgroup2_max 1073741824\n" + strings.Repeat("x", 4*maxOutput)

	got, err := Probe(t.Context(), fakeRunner{out: noise}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.CgroupMax != 1<<30 {
		t.Errorf("limit = %d, want the line that arrived before the bound", got.CgroupMax)
	}

	_, err = Probe(t.Context(), fakeRunner{out: noise, code: 1}, nil)
	if err == nil {
		t.Fatal("a probe that exited non-zero reported facts anyway")
	}
	// Generously bounded rather than exactly: what matters is that a node
	// printing four megabytes does not put four megabytes in an error.
	if len(err.Error()) > 2*maxOutput {
		t.Errorf("the error carries %d bytes of the node's output, want at most about %d",
			len(err.Error()), maxOutput)
	}
}

func TestLimited_StopsAtTheBound(t *testing.T) {
	var b strings.Builder
	l := &limited{w: &b, n: 4}

	// A write that fits, one that is cut, and one after the bound is spent.
	// Each reports the full length written, because a short count from a
	// writer makes io.Copy report a failure the caller would have to
	// distinguish from a real one.
	for _, s := range []string{"ab", "cdef", "gh"} {
		n, err := l.Write([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		if n != len(s) {
			t.Errorf("Write(%q) = %d, want %d", s, n, len(s))
		}
	}
	if got := b.String(); got != "abcd" {
		t.Errorf("kept %q, want the first 4 bytes", got)
	}
}

// TestParse_ReadsWhatARealContainerAnswered.
//
// Against output captured from `docker run`, not written by hand. The wire
// guesses in this repository have been wrong twice -- capabilities.model is an
// object and not a string, RunPod's allowedCudaVersions is in a schema and not
// in a response -- both times because a schema was read in place of a
// response. See testdata/README.md for how these were taken and what they do
// and do not settle.
func TestParse_ReadsWhatARealContainerAnswered(t *testing.T) {
	// The host's MemTotal, the same in all three because it IS the host's.
	const hostTotal = 131817620 * 1024

	cases := []struct {
		file    string
		version int
		limit   int64
		ceiling int64
	}{
		// A container bounded at 512 MiB reporting 125.7 GiB of host memory:
		// a factor of 252, and the mechanism behind "all eight nodes reported
		// 620-708 GiB free RAM" telling nobody anything about the limit the
		// page cache lived under.
		{"container_limited.txt", 2, 512 << 20, 512 << 20},
		// "No limit" is the literal string `max`, which a parser reading it
		// as a number would have reported as zero -- and the ceiling would
		// then have had to read as a container that cannot allocate anything.
		{"container_unlimited.txt", 2, 0, hostTotal},
		// The root cgroup has no memory.max at all, so nothing was read and
		// the host's total is the only ceiling there is.
		{"host_root_cgroup.txt", 0, 0, hostTotal},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", c.file))
			if err != nil {
				t.Fatal(err)
			}
			got := Parse(string(b))
			if got.CgroupVersion != c.version {
				t.Errorf("cgroup version = %d, want %d", got.CgroupVersion, c.version)
			}
			if got.CgroupMax != c.limit {
				t.Errorf("limit = %d, want %d", got.CgroupMax, c.limit)
			}
			if got.HostTotal != hostTotal {
				t.Errorf("host total = %d, want the host's %d", got.HostTotal, hostTotal)
			}
			// The captures are the only real container output in this
			// repository, so this is where the rule meets observed data
			// rather than a hand-written line -- and it is asked of the one
			// implementation, which is what the ledger's headroom column
			// goes through.
			if ceiling := memory(got).Ceiling(); ceiling != c.ceiling {
				t.Errorf("Ceiling = %d, want %d", ceiling, c.ceiling)
			}
		})
	}
}

// The gap between the two views, stated as a number, because it is the whole
// reason both are recorded.
func TestParse_TheTwoViewsDisagreeByAFactorOf252(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "container_limited.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := Parse(string(b))
	ratio := got.HostTotal / got.CgroupMax
	if ratio < 100 {
		t.Errorf("the host's view is only %dx the cgroup's; this capture was meant to be "+
			"the one where they disagree enormously", ratio)
	}
	// And a 56 GiB model would not have fitted, which the host's view says
	// nothing about.
	got.Assets = 56 << 30
	h, ok := memory(got).Headroom()
	if !ok || h >= 0 {
		t.Errorf("headroom = %d, %v; want a negative answer under a 512 MiB limit", h, ok)
	}
}
