package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openInputs(t *testing.T) *Inputs {
	t.Helper()
	in, err := OpenInputs(filepath.Join(t.TempDir(), "inputs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })
	return in
}

// pngOf is a real PNG of the given size.
//
// ENCODED RATHER THAN A CANNED BLOB, because what is being tested is that
// image.DecodeConfig reads the header -- so the header has to be the one a
// real encoder writes, and the geometry has to be something the test chose.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	im.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInputs_StageIsKeyedByItsOwnContent.
//
// The reference is the digest, so the same bytes staged twice are one object
// and the name carries no trace of where they came from. That is what makes
// an uploaded filename unable to reach a path: there is no filename in the
// call at all.
func TestInputs_StageIsKeyedByItsOwnContent(t *testing.T) {
	in := openInputs(t)
	b := pngOf(t, 8, 4)

	got, err := in.Stage(b)
	if err != nil {
		t.Fatal(err)
	}
	want := Ref(sha256.Sum256(b))
	if got.Ref != want {
		t.Errorf("ref %q, want %q", got.Ref, want)
	}
	if got.Format != "png" || got.Width != 8 || got.Height != 4 || got.Bytes != len(b) {
		t.Errorf("staged %+v, want png 8x4 of %d bytes", got, len(b))
	}

	again, err := in.Stage(b)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ref != got.Ref {
		t.Errorf("the same bytes staged twice got two names: %q and %q", got.Ref, again.Ref)
	}

	back, err := in.Load(got.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, b) {
		t.Error("what came back is not what was staged")
	}
}

// TestInputs_StageTakesAJpegToo, so the allow-list is two formats and not one.
func TestInputs_StageTakesAJpegToo(t *testing.T) {
	in := openInputs(t)
	got, err := in.Stage(jpegOf(t, 16, 16))
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "jpeg" {
		t.Errorf("format %q, want jpeg", got.Format)
	}
}

// TestInputs_RefusesWhatIsNotAnImage.
//
// THE ALLOW-LIST IS THE SET OF REGISTERED DECODERS, so a gif, a tarball and a
// sentence are all refused by the same mechanism. sd-server refuses a
// non-image too -- measured, a 400 with "invalid generation parameters" --
// but that refusal arrives after a card is rented and names the request
// rather than the file.
func TestInputs_RefusesWhatIsNotAnImage(t *testing.T) {
	in := openInputs(t)
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"a sentence", []byte("this is not an image at all")},
		{"a tar header", append([]byte("payload.png"), make([]byte, 512)...)},
		// A GIF is a real image and still refused: the standard library has
		// no decoder registered here, which is the allow-list doing its job
		// rather than a string check that happened to miss it.
		{"a gif", []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00")},
		// A PNG magic number with nothing behind it, which is what a byte
		// sniffer would accept.
		{"a png signature alone", []byte("\x89PNG\r\n\x1a\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := in.Stage(tc.b); err == nil {
				t.Fatal("staged something that is not a png or a jpeg")
			} else if !strings.Contains(err.Error(), "not a png or a jpeg") {
				t.Errorf("the refusal does not say what is accepted: %v", err)
			}
			// AND NOTHING WAS WRITTEN. The order is the cheap-refusal order:
			// a file on disk for a request that was going to be refused is a
			// file nothing removes.
			if n := countStaged(t, in); n != 0 {
				t.Errorf("%d file(s) were written for a refused upload", n)
			}
		})
	}
}

// TestInputs_RefusesAnEmptyUpload, which is what a failed read leaves.
func TestInputs_RefusesAnEmptyUpload(t *testing.T) {
	in := openInputs(t)
	if _, err := in.Stage(nil); err == nil || !strings.Contains(err.Error(), "no bytes") {
		t.Errorf("an empty upload was not named as one: %v", err)
	}
}

// TestInputs_RefusesADecompressionBomb.
//
// THE BOUND IS ON THE DECLARED GEOMETRY AND NOT ON THE BYTES, and this is why
// both bounds exist. The PNG below is a few hundred bytes -- it passes every
// size check there is -- and declares an image whose decode would allocate
// gigabytes on the machine ember is renting. ember never decodes the pixels,
// which is exactly why it has to read the header.
func TestInputs_RefusesADecompressionBomb(t *testing.T) {
	in := openInputs(t)
	bomb := pngHeader(MaxInputAxis+1, 4)
	if len(bomb) > 1024 {
		t.Fatalf("the bomb is %d bytes, which is not a small file any more", len(bomb))
	}

	// It really does declare what the test says it declares, read by the same
	// function Stage uses. Without this the assertion below could pass
	// because the bytes were malformed rather than because the bound fired --
	// the shape of a test whose expected value is whatever the bug produces.
	cfg, _, derr := image.DecodeConfig(bytes.NewReader(bomb))
	if derr != nil {
		t.Fatalf("the fixture is not a decodable header: %v", derr)
	}
	if cfg.Width != MaxInputAxis+1 {
		t.Fatalf("the fixture declares %d wide, want %d", cfg.Width, MaxInputAxis+1)
	}

	_, err := in.Stage(bomb)
	if err == nil {
		t.Fatal("staged an image declaring an axis past the bound")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(MaxInputAxis)) {
		t.Errorf("the refusal does not name the bound: %v", err)
	}
	if n := countStaged(t, in); n != 0 {
		t.Errorf("%d file(s) were written for a refused upload", n)
	}

	// The other axis, so the check is not one-sided.
	if _, err := in.Stage(pngHeader(4, MaxInputAxis+1)); err == nil {
		t.Error("staged an image declaring a height past the bound")
	}
	// And the bound itself is accepted: a refusal AT the limit would be a
	// different bug and would pass the assertions above.
	if _, err := in.Stage(pngHeader(MaxInputAxis, 1)); err != nil {
		t.Errorf("refused an image exactly at the bound: %v", err)
	}
}

// TestInputs_RefusesAZeroAxis covers the branch no registered decoder can
// produce, by hand-building the header that declares it.
func TestInputs_RefusesAZeroAxis(t *testing.T) {
	in := openInputs(t)
	// A PNG whose IHDR says zero wide. The decoder's own DecodeConfig
	// rejects it, so this asserts the refusal rather than which refusal --
	// what matters is that nothing with no pixels is staged.
	if _, err := in.Stage(pngHeader(0, 4)); err == nil {
		t.Error("staged an image declaring no width")
	}
}

// TestReadInput_RefusesRatherThanTruncating.
//
// THE DEFECT THIS IS WRITTEN FOR is internal/s3's `do`, which used
// io.LimitReader at the bound: a reply one byte too long came back silently
// short, and a truncated listing page parsed as XML right up to where it did
// not. Here it would be worse than confusing -- a truncated image whose
// header survived still decodes, so ember would stage a file that is not the
// file somebody picked and the operator would find out as a generation that
// used the wrong pixels.
//
// Both directions, because a bound that refused everything would pass the
// first half alone.
func TestReadInput_RefusesRatherThanTruncating(t *testing.T) {
	at := bytes.Repeat([]byte{7}, MaxInputBytes)
	got, err := ReadInput(bytes.NewReader(at))
	if err != nil {
		t.Fatalf("a body exactly at the bound was refused: %v", err)
	}
	if len(got) != MaxInputBytes {
		t.Errorf("read %d bytes of %d", len(got), MaxInputBytes)
	}

	over := bytes.Repeat([]byte{7}, MaxInputBytes+1)
	got, err = ReadInput(bytes.NewReader(over))
	if !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("a body one byte past the bound came back as %d bytes and %v; "+
			"a truncation and a refusal are different answers", len(got), err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(MaxInputBytes)) {
		t.Errorf("the refusal does not name the bound: %v", err)
	}
}

// TestReadInput_ReportsAReadThatFailed, so a broken connection is not an
// empty upload.
func TestReadInput_ReportsAReadThatFailed(t *testing.T) {
	_, err := ReadInput(failingReader{})
	if err == nil || !strings.Contains(err.Error(), "read the uploaded image") {
		t.Errorf("a failed read was not reported as one: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the connection went away") }

// TestInputs_StageRefusesOversizeBytesItself.
//
// ReadInput is where the HTTP body is bounded, and this is the same bound at
// the other end of the package: a caller that assembled the bytes some other
// way must not get past it, or the bound would be a property of one code path
// rather than of the store.
func TestInputs_StageRefusesOversizeBytesItself(t *testing.T) {
	in := openInputs(t)
	if _, err := in.Stage(bytes.Repeat([]byte{7}, MaxInputBytes+1)); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("Stage took %d bytes: %v", MaxInputBytes+1, err)
	}
}

// TestParseRef_RefusesEverythingButADigest.
//
// THE WHOLE OF WHAT A REQUEST MAY NAME. Each case below is a thing somebody
// would try or a thing a buggy client would send, and the point of the
// refusal is that none of them can become a path component: a key is
// sixty-four lowercase hex characters, so there is nothing in it to traverse
// with, point at or clean.
func TestParseRef_RefusesEverythingButADigest(t *testing.T) {
	good := strings.Repeat("ab", sha256.Size)
	for _, tc := range []struct{ name, ref string }{
		{"empty", ""},
		{"no scheme", good},
		{"another scheme", "md5:" + good},
		{"a path", "sha256:../../etc/passwd"},
		{"an absolute path", "sha256:/etc/passwd"},
		{"a traversal that cleans away", "sha256:" + good + "/../" + good},
		{"too short", "sha256:" + good[:62]},
		{"too long", "sha256:" + good + "ab"},
		{"upper case", "sha256:" + strings.ToUpper(good)},
		{"not hex", "sha256:" + strings.Repeat("zz", sha256.Size)},
		{"padded", "sha256: " + good},
		{"a trailing newline", "sha256:" + good + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRef(tc.ref); err == nil {
				t.Errorf("accepted %q as a reference", tc.ref)
			}
		})
	}

	key, err := ParseRef("sha256:" + good)
	if err != nil {
		t.Fatalf("refused a well-formed reference: %v", err)
	}
	if key != good {
		t.Errorf("key %q, want %q", key, good)
	}
}

// TestInputs_LoadRefusesBytesThatAreNotTheOnesNamed.
//
// THE REINSTATEMENT THAT MATTERS MOST. The reference is a claim about
// content; without the re-hash it would be an unverified one, and anything
// able to write into the staging directory could leave different pixels under
// a digest a queued request already names. Then the generation would use them
// and nothing would say so.
//
// It is also what makes "one request naming another request's input"
// harmless: whatever a job names, it gets those bytes or an error, never
// somebody else's.
func TestInputs_LoadRefusesBytesThatAreNotTheOnesNamed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inputs")
	in, err := OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	staged, err := in.Stage(pngOf(t, 4, 4))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseRef(staged.Ref)
	if err != nil {
		t.Fatal(err)
	}

	// Something else writes a different image under the same name. Out of
	// band and through the filesystem rather than through this package,
	// because the whole premise is a writer that is not ember.
	if werr := os.WriteFile(filepath.Join(dir, key), pngOf(t, 8, 8), 0o600); werr != nil {
		t.Fatal(werr)
	}

	_, err = in.Load(staged.Ref)
	if err == nil {
		t.Fatal("loaded bytes that do not hash to the reference naming them")
	}
	for _, want := range []string{"hashes to", "not the bytes it names"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	// NOT REPAIRED. The substituted bytes are still there: overwriting them
	// would destroy the only evidence of what happened, which is the rule a
	// mismatched asset digest on a store already follows.
	if b, rerr := os.ReadFile(filepath.Join(dir, key)); rerr != nil || len(b) == 0 {
		t.Errorf("the mismatched file was removed or emptied: %d bytes, %v", len(b), rerr)
	}
}

// TestInputs_LoadSaysWhenNothingWasStaged, so a handler can tell a missing
// reference from a failure to read one.
func TestInputs_LoadSaysWhenNothingWasStaged(t *testing.T) {
	in := openInputs(t)
	_, err := in.Load("sha256:" + strings.Repeat("cd", sha256.Size))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("an unstaged reference came back as %v, want ErrNotFound", err)
	}
	if err := in.verify("sha256:" + strings.Repeat("cd", sha256.Size)); !errors.Is(err, ErrNotFound) {
		t.Errorf("Verify disagreed with Load: %v", err)
	}
}

// TestInputs_VerifyAgreesWithLoad on the path that works, so the two moments
// a reference is checked cannot come to different answers.
func TestInputs_VerifyAgreesWithLoad(t *testing.T) {
	in := openInputs(t)
	staged, err := in.Stage(pngOf(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.verify(staged.Ref); err != nil {
		t.Errorf("Verify refused what Stage wrote: %v", err)
	}
	if _, err := in.Load("not-a-reference"); err == nil {
		t.Error("Load accepted a malformed reference")
	}
}

// TestInputs_TheRootIsTheSecondMechanism.
//
// CONTAINMENT TWICE, BY TWO DIFFERENT MECHANISMS, which is the tar
// extractor's rule applied here. ParseRef is the string check; this is the
// one that holds when a string check misses a spelling. A symlink planted in
// the staging directory pointing outside it is the case a key check cannot
// see at all, because the key is perfectly well-formed.
func TestInputs_TheRootIsTheSecondMechanism(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "inputs")
	in, err := OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	secret := filepath.Join(base, "secret")
	if err := os.WriteFile(secret, pngOf(t, 2, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ef", sha256.Size)
	// A hard failure and not a skip: a test that skips itself reports
	// success for something that never ran, and this is the assertion that
	// the second containment mechanism exists at all.
	if err := os.Symlink(secret, filepath.Join(dir, key)); err != nil {
		t.Fatalf("this test needs a symlink: %v", err)
	}
	// The key is one ParseRef accepts, so the string check cannot be what
	// refuses below -- without this the assertion could pass for the wrong
	// reason.
	if _, perr := ParseRef("sha256:" + key); perr != nil {
		t.Fatalf("this test needs a reference ParseRef accepts: %v", perr)
	}

	_, err = in.Load("sha256:" + key)
	if err == nil {
		t.Error("read through a symlink out of the staging directory")
	} else if strings.Contains(err.Error(), "image reference") {
		t.Errorf("the string check caught this, so the root was not exercised: %v", err)
	}
}

// TestInputs_SweepKeepsWhatIsStillWantedAndWhatIsStillNew.
//
// TWO BOUNDS, AND THE TEST DRIVES BOTH. `keep` is what an unfinished request
// names and must survive whatever its age; `before` is the age bound, and
// without it the sweep would delete the file an operator picked a second ago
// -- which belongs to no request yet, because they are still composing.
func TestInputs_SweepKeepsWhatIsStillWantedAndWhatIsStillNew(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inputs")
	in, err := OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	old := func(b []byte) Input {
		t.Helper()
		got, serr := in.Stage(b)
		if serr != nil {
			t.Fatal(serr)
		}
		key, _ := ParseRef(got.Ref)
		was := time.Now().Add(-48 * time.Hour)
		if cerr := os.Chtimes(filepath.Join(dir, key), was, was); cerr != nil {
			t.Fatal(cerr)
		}
		return got
	}

	wanted := old(pngOf(t, 2, 2))
	abandoned := old(pngOf(t, 3, 3))
	fresh, err := in.Stage(pngOf(t, 4, 4))
	if err != nil {
		t.Fatal(err)
	}

	swept, err := in.Sweep(time.Now().Add(-24*time.Hour), []string{wanted.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if swept.Files != 1 {
		t.Errorf("swept %d file(s), want 1", swept.Files)
	}
	if swept.Bytes != int64(abandoned.Bytes) {
		t.Errorf("swept %d bytes, want %d", swept.Bytes, abandoned.Bytes)
	}
	if err := in.verify(wanted.Ref); err != nil {
		t.Errorf("the sweep took an image a queued request names: %v", err)
	}
	if err := in.verify(fresh.Ref); err != nil {
		t.Errorf("the sweep took an image that had just been uploaded: %v", err)
	}
	if err := in.verify(abandoned.Ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("the abandoned image is still there: %v", err)
	}
}

// TestInputs_SweepIgnoresWhatItDidNotWrite.
//
// A sweep that removed anything in the directory would be a recursive delete
// driven by a directory name, which is the hazard Store.Delete's own contract
// says it must not become -- and the files it would take are an operator's
// own.
func TestInputs_SweepIgnoresWhatItDidNotWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inputs")
	in, err := OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	mine := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A name that looks almost right: the digest length with a suffix.
	almost := filepath.Join(dir, strings.Repeat("ab", sha256.Size)+".png")
	if err := os.WriteFile(almost, []byte("almost"), 0o600); err != nil {
		t.Fatal(err)
	}

	swept, err := in.Sweep(time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if swept.Files != 0 {
		t.Errorf("swept %d file(s) it did not write", swept.Files)
	}
	for _, p := range []string{mine, almost} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

// TestInputs_SweepIgnoresAMalformedKeepEntry.
//
// A reference in a stored job that is not a reference keeps nothing, rather
// than being trimmed into a key that keeps the wrong file. Parsed and not
// trimmed, which is the same decision ParseRef makes.
func TestInputs_SweepIgnoresAMalformedKeepEntry(t *testing.T) {
	in := openInputs(t)
	if _, err := in.Stage(pngOf(t, 2, 2)); err != nil {
		t.Fatal(err)
	}
	swept, err := in.Sweep(time.Now().Add(time.Hour), []string{"", "nonsense", "sha256:zz"})
	if err != nil {
		t.Fatal(err)
	}
	if swept.Files != 1 {
		t.Errorf("swept %d file(s), want the one nothing named", swept.Files)
	}
}

// TestInputs_SweepReportsADirectoryItCannotRead.
//
// Unlike Dir.pruneDirs, where a directory that will not go is the ordinary
// case, a staging area ember cannot read is bytes accumulating unseen -- so
// it is an error and the caller says so.
func TestInputs_SweepReportsADirectoryItCannotRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inputs")
	in, err := OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Sweep(time.Now(), nil); err == nil {
		t.Error("a sweep against a closed root reported success")
	}
}

// TestOpenInputs_ReportsADirectoryItCannotMake, which is the one failure that
// stops `ember serve` from starting with a staging area.
func TestOpenInputs_ReportsADirectoryItCannotMake(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenInputs(filepath.Join(blocked, "inputs")); err == nil {
		t.Error("opened a staging directory under a file")
	}
}

// TestInputs_NameIsForAnErrorMessage, so a mismatch names the directory to
// look in.
func TestInputs_NameIsForAnErrorMessage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inputs")
	in, err := OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	if in.Name() != dir {
		t.Errorf("Name is %q, want %q", in.Name(), dir)
	}
}

// TestInputs_StageReportsAWriteItCouldNotMake.
func TestInputs_StageReportsAWriteItCouldNotMake(t *testing.T) {
	in := openInputs(t)
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Stage(pngOf(t, 2, 2)); err == nil {
		t.Error("staged an image against a closed root")
	}
}

// TestInputs_LoadReportsAnOpenItCouldNotMake covers the open failure that is
// not a missing file: a directory where a key should be.
func TestInputs_LoadReportsAnOpenItCouldNotMake(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inputs")
	in, err := OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	key := strings.Repeat("ab", sha256.Size)
	if err := os.Mkdir(filepath.Join(dir, key), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = in.Load("sha256:" + key)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("a directory under a key read as %v", err)
	}
}

// countStaged counts the files Stage could have written.
func countStaged(t *testing.T, in *Inputs) int {
	t.Helper()
	names, err := in.list()
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

// pngHeader builds a PNG that declares a size and carries no usable pixels.
//
// BY HAND, because png.Encode will not produce one: the point is a tiny file
// whose header claims an enormous image, which is the decompression bomb the
// axis bound exists for and which no encoder would write.
func pngHeader(w, h int) []byte {
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")

	ihdr := make([]byte, 0, 13)
	ihdr = append(ihdr, byte(w>>24), byte(w>>16), byte(w>>8), byte(w))
	ihdr = append(ihdr, byte(h>>24), byte(h>>16), byte(h>>8), byte(h))
	// 8-bit truecolour, no interlace: the combination DecodeConfig accepts.
	ihdr = append(ihdr, 8, 2, 0, 0, 0)
	writeChunk(&out, "IHDR", ihdr)
	writeChunk(&out, "IEND", nil)
	return out.Bytes()
}

func writeChunk(w io.Writer, kind string, data []byte) {
	n := len(data)
	_, _ = w.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	_, _ = w.Write([]byte(kind))
	_, _ = w.Write(data)
	_, _ = w.Write(crc32Of(append([]byte(kind), data...)))
}

func crc32Of(b []byte) []byte {
	sum := crc32.ChecksumIEEE(b)
	return []byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)}
}

// TestRef_RendersWhatParseRefAccepts, so the two halves of the reference
// format cannot drift apart.
func TestRef_RendersWhatParseRefAccepts(t *testing.T) {
	sum := sha256.Sum256([]byte("pixels"))
	ref := Ref(sum)
	key, err := ParseRef(ref)
	if err != nil {
		t.Fatalf("ParseRef refused what Ref rendered: %v", err)
	}
	if key != hex.EncodeToString(sum[:]) {
		t.Errorf("key %q does not match the digest", key)
	}
}

// TestInputs_SweepReportsWhatItCouldNotRemove.
//
// Unlike Dir.pruneDirs, where a directory that will not go is the ordinary
// case, a staged input ember cannot remove is bytes the operator is keeping
// without knowing it -- which is the leak the sweep exists to end, so it is
// reported rather than counted as freed.
func TestInputs_SweepReportsWhatItCouldNotRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inputs")
	in, err := OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	if _, serr := in.Stage(pngOf(t, 2, 2)); serr != nil {
		t.Fatal(serr)
	}

	// Removing an entry needs write permission on the directory holding it.
	if cerr := os.Chmod(dir, 0o500); cerr != nil {
		t.Fatal(cerr)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	swept, err := in.Sweep(time.Now().Add(time.Hour), nil)
	if err == nil {
		t.Fatal("a sweep that could remove nothing reported success")
	}
	if !strings.Contains(err.Error(), "remove the staged image") {
		t.Errorf("the error does not name what it could not remove: %v", err)
	}
	if swept.Files != 0 {
		t.Errorf("counted %d file(s) as freed that are still there", swept.Files)
	}
}

// verify is Load with the bytes thrown away, for an assertion about presence.
//
// A TEST HELPER AND NOT A METHOD. It was a method once, and
// TestNoProductionDeadCode caught it: production code that only tests call
// is the "written, covered and connected to nothing" defect this repository
// has now found nine times. Both real callers need the bytes -- the enqueue
// handler and jobsFromRequest each build the request body, which carries
// them -- so there was never a second caller for it to have.
func (i *Inputs) verify(ref string) error {
	_, err := i.Load(ref)
	return err
}
