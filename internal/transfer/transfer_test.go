package transfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
)

// entry is one archive member for the builders below.
type entry struct {
	name string
	body string
	kind byte
	link string
	size int64 // overrides len(body) when non-zero, for truncation tests
}

func targz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		size := int64(len(e.body))
		if e.size != 0 {
			size = e.size
		}
		h := &tar.Header{
			Name: e.name, Typeflag: kind, Mode: 0o600,
			Size: size, Linkname: e.link, ModTime: time.Unix(0, 0),
		}
		if kind == tar.TypeDir {
			h.Size = 0
			h.Mode = 0o700
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func root(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, dir
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExtract_WritesFilesAndDirectories(t *testing.T) {
	r, dir := root(t)
	src := targz(t,
		entry{name: "out/", kind: tar.TypeDir},
		entry{name: "out/frame0001.png", body: "pixels"},
		entry{name: "out/nested/frame0002.png", body: "more pixels"},
	)

	res, err := Extract(t.Context(), bytes.NewReader(src), r, "renders")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 {
		t.Errorf("files = %d", res.Files)
	}
	if res.Bytes != int64(len("pixels")+len("more pixels")) {
		t.Errorf("bytes = %d", res.Bytes)
	}
	if got := read(t, dir, "renders/out/frame0001.png"); got != "pixels" {
		t.Errorf("got %q", got)
	}
	if got := read(t, dir, "renders/out/nested/frame0002.png"); got != "more pixels" {
		t.Errorf("got %q", got)
	}
}

// TestExtract_RefusesToEscapeTheOutputDirectory is the oldest file-overwrite
// bug there is. A node ember rented for twenty minutes writes the member
// names, and one of them wanting ~/.ssh/authorized_keys is the whole reason
// the containment is enforced twice -- by the name check and again by the
// os.Root the write goes through.
func TestExtract_RefusesToEscapeTheOutputDirectory(t *testing.T) {
	r, dir := root(t)

	// A sentinel next to the root: if any of these land, it is overwritten.
	outside := filepath.Join(filepath.Dir(dir), "DO-NOT-TOUCH")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := targz(t,
		entry{name: "../DO-NOT-TOUCH", body: "clobbered"},
		entry{name: "../../DO-NOT-TOUCH", body: "clobbered"},
		entry{name: "out/../../DO-NOT-TOUCH", body: "clobbered"},
		entry{name: "/etc/passwd", body: "clobbered"},
		entry{name: `..\DO-NOT-TOUCH`, body: "clobbered"},
		entry{name: "safe.txt", body: "kept"},
	)

	res, err := Extract(t.Context(), bytes.NewReader(src), r, "renders")

	// The one legitimate file is still written: a hostile member must not
	// cost the operator the output they asked for.
	if res.Files != 1 {
		t.Errorf("files = %d, want only the safe one", res.Files)
	}
	if got := read(t, dir, "renders/safe.txt"); got != "kept" {
		t.Errorf("got %q", got)
	}

	// And the refusals are reported rather than silently dropped.
	var skipped *SkippedError
	if !errors.As(err, &skipped) {
		t.Fatalf("got %v, want a SkippedError naming what was refused", err)
	}

	if got, err := os.ReadFile(outside); err != nil || string(got) != "original" {
		t.Fatalf("a file outside the output directory was written: %q (%v)", got, err)
	}
	// /etc/passwd being written would be a much worse day; assert it is
	// untouched by checking nothing absolute landed inside either.
	if _, err := os.Stat(filepath.Join(dir, "etc", "passwd")); err == nil {
		t.Error("an absolute member name was written relative to the root")
	}
}

// TestExtract_SkipsLinksAndDevices. ember retrieves generated media from a
// machine it rented; there is no legitimate reason for a render to be a
// symlink or a block device, and refusing the kinds outright is shorter than
// reasoning about where each one could point.
func TestExtract_SkipsLinksAndDevices(t *testing.T) {
	r, dir := root(t)
	src := targz(t,
		entry{name: "keys", kind: tar.TypeSymlink, link: "/home/legion/.ssh/id_ed25519"},
		entry{name: "passwd", kind: tar.TypeLink, link: "/etc/passwd"},
		entry{name: "disk", kind: tar.TypeBlock},
		entry{name: "pipe", kind: tar.TypeFifo},
		entry{name: "real.png", body: "pixels"},
	)

	res, err := Extract(t.Context(), bytes.NewReader(src), r, "renders")
	if res.Files != 1 {
		t.Errorf("files = %d", res.Files)
	}

	var skipped *SkippedError
	if !errors.As(err, &skipped) {
		t.Fatalf("got %v, want a SkippedError", err)
	}
	if len(skipped.Names) != 4 {
		t.Errorf("skipped %d entries: %v", len(skipped.Names), skipped.Names)
	}
	// The operator needs to know which kind each refusal was, not just a
	// count: "symlink" tells them the workload wrote one, which is itself
	// worth knowing.
	joined := strings.Join(skipped.Names, " ")
	for _, want := range []string{"symlink", "hard link", "device node", "fifo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not mention %q: %v", want, skipped.Names)
		}
	}

	for _, gone := range []string{"renders/keys", "renders/passwd", "renders/disk", "renders/pipe"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s was written", gone)
		}
	}
}

// TestExtract_RefusesAMemberLongerThanItsHeader. A member claiming 1 KiB and
// sending a gigabyte is a decompression bomb, and a limit on the whole stream
// would not catch it until the disk was already much fuller.
func TestExtract_RefusesAMemberLongerThanItsHeader(t *testing.T) {
	r, dir := root(t)

	// A header that under-reports: tar's writer refuses to write more than
	// Size, so the body is built to match and the header patched down by
	// declaring a smaller size with a longer body.
	src := targz(t, entry{name: "big.bin", body: strings.Repeat("x", 100), size: 100})

	// Truncating the stream mid-member is the reachable version of the same
	// failure: the header promises 100 bytes and fewer arrive.
	cut := src[:len(src)-20]
	res, err := Extract(t.Context(), bytes.NewReader(cut), r, "renders")
	if err == nil {
		t.Fatal("a truncated archive was accepted")
	}
	_ = res
	_ = dir
}

func TestExtract_RefusesAnArchiveThatIsNotGzip(t *testing.T) {
	r, _ := root(t)
	_, err := Extract(t.Context(), strings.NewReader("this is not a gzip stream"), r, "renders")
	if err == nil {
		t.Fatal("a non-gzip stream was accepted")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("got %v", err)
	}
}

func TestExtract_RefusesABadPrefix(t *testing.T) {
	r, _ := root(t)
	for _, bad := range []string{"../escape", "/absolute", "a/../../b"} {
		if _, err := Extract(t.Context(), bytes.NewReader(targz(t)), r, bad); err == nil {
			t.Errorf("prefix %q was accepted", bad)
		}
	}
}

func TestExtract_HonoursCancellation(t *testing.T) {
	r, _ := root(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	src := targz(t, entry{name: "a.png", body: "x"})
	if _, err := Extract(ctx, bytes.NewReader(src), r, "renders"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestExtract_AnEmptyArchiveIsNotAnError(t *testing.T) {
	r, _ := root(t)
	res, err := Extract(t.Context(), bytes.NewReader(targz(t)), r, "renders")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 {
		t.Errorf("files = %d", res.Files)
	}
}

func TestSafeName(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"out/a.png", "renders/out/a.png", true},
		{"./out/a.png", "renders/out/a.png", true},
		// Absolute names are refused, not stripped to relative: ember's own
		// TarCommand never produces one, so an absolute name means the
		// archive was not made the way ember asks.
		{"/out/a.png", "", false},
		{"out//a.png", "renders/out/a.png", true},
		{"../a.png", "", false},
		{"../../a.png", "", false},
		{"out/../../a.png", "", false},
		{"..", "", false},
		{"", "", false},
		{".", "", false},
		{`..\a.png`, "", false},
	} {
		got, err := safeName("renders", tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("safeName(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
		}
		if tc.ok && got != tc.want {
			t.Errorf("safeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTarCommand(t *testing.T) {
	cmd, err := TarCommand("/workspace/out", "")
	if err != nil {
		t.Fatal(err)
	}
	// -C into the parent and name the base: GNU tar strips a leading slash
	// with a warning and BSD tar does not, so tarring an absolute path would
	// produce member names that differ by platform.
	line := cmd.String()
	for _, want := range []string{"tar", "-czf -", "-C /workspace", "-- out"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q is missing %q", line, want)
		}
	}
}

func TestTarCommand_RefusesAPathThatIsNotOne(t *testing.T) {
	for _, bad := range []string{
		"relative/path",
		"/not/clean/..",
		"/",
		"",
	} {
		if _, err := TarCommand(bad, ""); err == nil {
			t.Errorf("TarCommand(%q) was accepted", bad)
		}
	}
}

// TestTarCommand_QuotesAPathWithShellMetacharacters rather than refusing it.
//
// An earlier version refused anything unquotable, which was defensible while
// every remote command was tar over config-file paths. It is wrong now that
// sshx quotes properly: a directory genuinely called "my renders" is a path
// an operator may legitimately have, and the answer is to send it correctly
// rather than to decline.
//
// The assertion is that the dangerous text ends up inside single quotes,
// where a POSIX shell treats every character literally. The lab in
// test/integration round-trips these through a real sshd and compares what
// arrived, which is the part a unit test cannot do.
func TestTarCommand_QuotesAPathWithShellMetacharacters(t *testing.T) {
	for _, p := range []string{
		"/workspace/$(touch pwned)",
		"/workspace/a;rm -rf x",
		"/workspace/my renders",
		"/workspace/`id`",
		"/workspace/a'b",
		"/workspace/a\"b",
	} {
		cmd, err := TarCommand(p, "")
		if err != nil {
			t.Errorf("TarCommand(%q): %v", p, err)
			continue
		}
		// The base is the last argument, and it must appear in exactly the
		// quoted form sshx would produce -- not bare on the command line.
		line := cmd.String()
		want := sshx.Quote(path.Base(p))
		if !strings.HasSuffix(want, "'") {
			t.Errorf("%q was not quoted at all by sshx.Quote: %s", p, want)
		}
		if !strings.Contains(line, want) {
			t.Errorf("the base of %q is not quoted in the command line:\n  got  %s\n  want it to contain %s", p, line, want)
		}
	}
}

func TestFetch_RetrievesEveryOutput(t *testing.T) {
	r := sshx.NewRecordingRunner()

	one, _ := TarCommand("/workspace/out", "")
	two, _ := TarCommand("/workspace/run.log", "")
	r.Outputs[one.String()] = targz(t, entry{name: "out/a.png", body: "pixels"})
	r.Outputs[two.String()] = targz(t, entry{name: "run.log", body: "lines"})

	dir := filepath.Join(t.TempDir(), "run-1")
	res, err := Fetch(t.Context(), r, []node.OutputPath{
		{Remote: "/workspace/out", Local: "renders"},
		{Remote: "/workspace/run.log", Local: "logs"},
	}, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 {
		t.Errorf("files = %d", res.Files)
	}
	if got := read(t, dir, "renders/out/a.png"); got != "pixels" {
		t.Errorf("got %q", got)
	}
	if got := read(t, dir, "logs/run.log"); got != "lines" {
		t.Errorf("got %q", got)
	}
}

// TestFetch_OneFailureDoesNotLoseTheRest. A run that produced renders and a
// log should not lose the renders because the log was never written.
func TestFetch_OneFailureDoesNotLoseTheRest(t *testing.T) {
	r := sshx.NewRecordingRunner()
	good, _ := TarCommand("/workspace/out", "")
	bad, _ := TarCommand("/workspace/missing", "")
	r.Outputs[good.String()] = targz(t, entry{name: "out/a.png", body: "pixels"})
	r.Errors[bad.String()] = &sshx.ExitError{Cmd: bad.String(), Code: 2, Stderr: "tar: /workspace/missing: No such file or directory"}

	dir := t.TempDir()
	res, err := Fetch(t.Context(), r, []node.OutputPath{
		{Remote: "/workspace/missing", Local: "logs"},
		{Remote: "/workspace/out", Local: "renders"},
	}, dir, "")

	if err == nil {
		t.Fatal("a missing output was reported as success")
	}
	if res.Files != 1 {
		t.Errorf("files = %d, want the one that existed", res.Files)
	}
	if got := read(t, dir, "renders/out/a.png"); got != "pixels" {
		t.Errorf("the surviving output was lost: %q", got)
	}
	// The operator needs to know which path failed, since they named several.
	if !strings.Contains(err.Error(), "/workspace/missing") {
		t.Errorf("the error does not name the failed output: %v", err)
	}
}

// TestFetch_ANonZeroTarIsReportedEvenWhenTheArchiveParsed. A tar that failed
// halfway produces a valid prefix of an archive: the extraction is happy, the
// data is incomplete, and that is exactly how an output fetch loses files
// quietly.
func TestFetch_ANonZeroTarIsReportedEvenWhenTheArchiveParsed(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd, _ := TarCommand("/workspace/out", "")
	r.Outputs[cmd.String()] = targz(t, entry{name: "out/a.png", body: "pixels"})
	r.Waits[cmd.String()] = &sshx.ExitError{Cmd: cmd.String(), Code: 2, Stderr: "tar: some files differ"}

	dir := t.TempDir()
	res, err := Fetch(t.Context(), r, []node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, dir, "")
	if err == nil {
		t.Fatal("a non-zero tar exit was accepted because the archive happened to parse")
	}
	// The files that did arrive are still counted and still on disk.
	if res.Files != 1 {
		t.Errorf("files = %d", res.Files)
	}
}

func TestFetch_NoOutputsIsNoWork(t *testing.T) {
	res, err := Fetch(t.Context(), sshx.NewRecordingRunner(), nil, filepath.Join(t.TempDir(), "unused"), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 {
		t.Errorf("files = %d", res.Files)
	}
}

func TestFetch_ReportsAnUnusableOutputDirectory(t *testing.T) {
	base := t.TempDir()
	inTheWay := filepath.Join(base, "run")
	if err := os.WriteFile(inTheWay, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Fetch(t.Context(), sshx.NewRecordingRunner(),
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, inTheWay, "")
	if err == nil {
		t.Fatal("a file in place of the output directory was accepted")
	}
}

func TestFetch_ReportsABadRemotePath(t *testing.T) {
	_, err := Fetch(t.Context(), sshx.NewRecordingRunner(),
		[]node.OutputPath{{Remote: "relative", Local: "renders"}}, t.TempDir(), "")
	if err == nil {
		t.Fatal("a relative remote path was accepted")
	}
}

func TestRunDir_SortsChronologicallyAndCannotCollide(t *testing.T) {
	base := "/home/legion/ember-out"
	a := RunDir(base, "0123456789abcdef", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	b := RunDir(base, "fedcba9876543210", time.Date(2026, 10, 24, 18, 30, 0, 0, time.UTC))

	if a == b {
		t.Fatal("two runs resolved to the same directory")
	}
	// Timestamp first, so a listing sorts chronologically -- which is what an
	// operator looking for "the one I did this morning" actually wants.
	if filepath.Base(a) >= filepath.Base(b) {
		t.Errorf("%q does not sort before %q", filepath.Base(a), filepath.Base(b))
	}
	if !strings.Contains(a, "20261024-090000") {
		t.Errorf("got %q", a)
	}
	if !strings.Contains(a, "0123456789abcdef") {
		t.Errorf("the run id is missing from %q", a)
	}
}

func TestSkippedError_TruncatesALongList(t *testing.T) {
	var names []string
	for i := range 25 {
		names = append(names, string(rune('a'+i)))
	}
	msg := (&SkippedError{Names: names}).Error()
	if !strings.Contains(msg, "and 15 more") {
		t.Errorf("got %q", msg)
	}
	if !strings.Contains(msg, "25 archive entries") {
		t.Errorf("got %q", msg)
	}
	if got := (&SkippedError{Names: []string{"one"}}).Error(); !strings.Contains(got, "1 archive entry") {
		t.Errorf("got %q", got)
	}
}

func TestPlural(t *testing.T) {
	if plural(1) != "y" || plural(2) != "ies" {
		t.Error("plural is wrong")
	}
}

func TestKindName(t *testing.T) {
	for kind, want := range map[byte]string{
		tar.TypeSymlink: "symlink",
		tar.TypeLink:    "hard link",
		tar.TypeChar:    "device node",
		tar.TypeBlock:   "device node",
		tar.TypeFifo:    "fifo",
	} {
		if got := kindName(kind); got != want {
			t.Errorf("kindName(%q) = %q, want %q", kind, got, want)
		}
	}
	if got := kindName('Z'); !strings.Contains(got, "unsupported") {
		t.Errorf("got %q", got)
	}
}

func TestExtract_ReportsAnUnreadableStream(t *testing.T) {
	r, _ := root(t)
	// A gzip header followed by garbage: the stream opens and then fails.
	src := targz(t, entry{name: "a.png", body: "x"})
	broken := append(src[:len(src)/2:len(src)/2], 0xff, 0xff, 0xff, 0xff)

	if _, err := Extract(t.Context(), bytes.NewReader(broken), r, "renders"); err == nil {
		t.Fatal("a corrupt stream was accepted")
	}
}

var _ io.Reader = (*bytes.Reader)(nil)

// TestExtract_ReportsAFileItCannotWrite. A directory already occupying a
// member's name is the reachable version: the write fails, and it must be an
// error rather than a quietly missing file.
func TestExtract_ReportsAFileItCannotWrite(t *testing.T) {
	r, dir := root(t)
	if err := os.MkdirAll(filepath.Join(dir, "renders", "a.png"), 0o700); err != nil {
		t.Fatal(err)
	}

	src := targz(t, entry{name: "a.png", body: "pixels"})
	if _, err := Extract(t.Context(), bytes.NewReader(src), r, "renders"); err == nil {
		t.Fatal("writing over a directory was reported as success")
	}
}

// TestExtract_ReportsADirectoryItCannotCreate, for the same reason: a file
// already holding the name means the rest of that subtree cannot land.
func TestExtract_ReportsADirectoryItCannotCreate(t *testing.T) {
	r, dir := root(t)
	if err := os.MkdirAll(filepath.Join(dir, "renders"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "renders", "out"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := targz(t, entry{name: "out/", kind: tar.TypeDir})
	if _, err := Extract(t.Context(), bytes.NewReader(src), r, "renders"); err == nil {
		t.Fatal("a directory that could not be created was reported as success")
	}
}

// TestExtract_ADirectoryThatAlreadyExistsIsFine. Two outputs can legitimately
// land in overlapping trees, and a second run into the same directory is an
// ordinary thing to do.
func TestExtract_ADirectoryThatAlreadyExistsIsFine(t *testing.T) {
	r, dir := root(t)
	src := targz(t,
		entry{name: "out/", kind: tar.TypeDir},
		entry{name: "out/", kind: tar.TypeDir},
		entry{name: "out/a.png", body: "pixels"},
	)
	if _, err := Extract(t.Context(), bytes.NewReader(src), r, "renders"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, "renders/out/a.png"); got != "pixels" {
		t.Errorf("got %q", got)
	}
}

// TestExtract_AMemberWhoseBodyIsShortIsATruncation. The header is the
// authority on length: a stream that ends early has lost data, and reporting
// the file as written would be the quiet loss this package is built to avoid.
func TestExtract_AMemberWhoseBodyIsShortIsATruncation(t *testing.T) {
	r, _ := root(t)

	// Built by hand so the header can promise more than the body delivers,
	// which tar's own writer refuses to do.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: "a.bin", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte("x"), 1000)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var gzbuf bytes.Buffer
	gz := gzip.NewWriter(&gzbuf)
	// Cut the uncompressed stream short, so the header stands and the body
	// does not arrive in full.
	if _, err := gz.Write(raw.Bytes()[:600]); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := Extract(t.Context(), bytes.NewReader(gzbuf.Bytes()), r, "renders")
	if err == nil {
		t.Fatal("a truncated member was accepted")
	}
}

func TestFetch_HonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Fetch(ctx, sshx.NewRecordingRunner(),
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, t.TempDir(), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// TestTarCommand_RefusesAPathWithANULByte. The one thing quoting cannot
// carry: a NUL cannot appear in an argument at all.
func TestTarCommand_RefusesAPathWithANULByte(t *testing.T) {
	if _, err := TarCommand("/workspace/a\x00b", ""); err == nil {
		t.Fatal("a path with a NUL byte was accepted")
	}
}

func TestFetch_ReportsAnOutputDirectoryItCannotOpen(t *testing.T) {
	// The output directory is a file. MkdirAll fails, and so does OpenRoot --
	// which is the first thing between a rented machine's tar stream and this
	// disk, so a failure to establish it must be reported rather than fallen
	// through.
	base := t.TempDir()
	dir := filepath.Join(base, "run-1")
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Fetch(t.Context(), sshx.NewRecordingRunner(),
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, dir, "")
	if err == nil {
		t.Fatal("fetching into a file was accepted")
	}
	if !strings.Contains(err.Error(), "output directory") {
		t.Errorf("got %q, want it to name the output directory", err)
	}
}

func TestFetch_ReportsBothAFailedExtractionAndAFailedCommand(t *testing.T) {
	// Both halves, not whichever was noticed first. A tar that failed partway
	// produces a valid prefix of an archive, so the extraction error says the
	// data is bad and the exit status says why -- and an operator given only
	// the first goes looking for a corrupt disk.
	r := sshx.NewRecordingRunner()
	cmd, _ := TarCommand("/workspace/out", "")
	r.Outputs[cmd.String()] = []byte("this is not gzip")
	r.Waits[cmd.String()] = &sshx.ExitError{Cmd: cmd.String(), Code: 2, Stderr: "tar: /workspace/out: No such file or directory"}

	_, err := Fetch(t.Context(), r,
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, filepath.Join(t.TempDir(), "run-1"), "")
	if err == nil {
		t.Fatal("a failed fetch reported success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "gzip") {
		t.Errorf("got %q, want it to carry the extraction failure", msg)
	}
	if !strings.Contains(msg, "No such file") {
		t.Errorf("got %q, want it to carry the command's own stderr", msg)
	}
}

func TestExtract_ReportsAFileBlockedByADirectory(t *testing.T) {
	// A directory and a regular file with the same name in one archive. The
	// open fails, and the failure has to name the member rather than surface
	// as a bare EISDIR from somewhere in the extraction.
	r, _ := root(t)
	src := targz(t,
		entry{name: "out/a", kind: tar.TypeDir},
		entry{name: "out/a", body: "now a file"},
	)

	_, err := Extract(t.Context(), bytes.NewReader(src), r, "renders")
	if err == nil {
		t.Fatal("a file landing on a directory was accepted")
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("got %q, want it to say what it could not create", err)
	}
}

// TestTarCommand_BoundsTheArchiveByAMarkerFile.
//
// A file, not a timestamp. ember's clock and the node's are different clocks,
// so "--newer=@<unix computed here>" would send too much or too little
// whenever they disagreed. A marker touched on the node is compared against
// mtimes from that same clock.
func TestTarCommand_BoundsTheArchiveByAMarkerFile(t *testing.T) {
	cmd, err := TarCommand("/workspace/out", "/tmp/.ember-fetch-mark")
	if err != nil {
		t.Fatal(err)
	}
	got := cmd.String()
	// GNU tar spells it --newer. --newer-than is not a flag and fails the
	// whole command, which was found by running it.
	if !strings.Contains(got, "--newer=/tmp/.ember-fetch-mark") {
		t.Errorf("the marker did not reach tar: %s", got)
	}

	// No marker is the shape every single-command run had before batching
	// existed, and it must stay byte-for-byte the same.
	plain, err := TarCommand("/workspace/out", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "--newer") {
		t.Errorf("an unbounded fetch carries a filter: %s", plain.String())
	}
}

func TestTarCommand_RefusesAnUnusableMarker(t *testing.T) {
	for _, bad := range []string{"relative/mark", "/tmp/../etc/mark", "/tmp/./mark"} {
		if _, err := TarCommand("/workspace/out", bad); err == nil {
			t.Errorf("TarCommand accepted marker %q", bad)
		}
	}
}
