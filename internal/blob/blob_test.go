package blob

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openDir(t *testing.T) *Dir {
	t.Helper()
	d, err := OpenDir(filepath.Join(t.TempDir(), "outputs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestDir_RoundTrips(t *testing.T) {
	d := openDir(t)
	want := []byte("generated-pixels")

	if err := d.Put(t.Context(), "run-1/cat-00.png", want); err != nil {
		t.Fatal(err)
	}
	f, err := d.Open(t.Context(), "run-1/cat-00.png")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDir_OpenSeeks is why the interface says ReadSeekCloser.
//
// http.ServeContent seeks, and without it a browser cannot ask for a byte
// range -- which for a video it is scrubbing through is the whole
// interaction.
func TestDir_OpenSeeks(t *testing.T) {
	d := openDir(t)
	if err := d.Put(t.Context(), "a.png", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	f, err := d.Open(t.Context(), "a.png")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "456789" {
		t.Errorf("after seeking to 4, read %q", got)
	}
}

// TestDir_MissingIsNotFound, so a handler can answer 404 rather than 500. The
// two are different answers: one means the operator asked for the wrong
// thing, the other means ember is broken.
func TestDir_MissingIsNotFound(t *testing.T) {
	d := openDir(t)
	_, err := d.Open(t.Context(), "nothing/here.png")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDir_CreatesParents(t *testing.T) {
	d := openDir(t)
	if err := d.Put(t.Context(), "a/b/c/d.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Open(t.Context(), "a/b/c/d.png"); err != nil {
		t.Errorf("a key several directories deep did not round trip: %v", err)
	}
}

func TestDir_DeleteIsIdempotent(t *testing.T) {
	d := openDir(t)
	if err := d.Put(t.Context(), "a.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(t.Context(), "a.png"); err != nil {
		t.Fatal(err)
	}
	// Already gone is a success, for the reason provider.Destroy treats a
	// missing node as one: the goal is that it is not there, and it is not.
	if err := d.Delete(t.Context(), "a.png"); err != nil {
		t.Errorf("deleting a key that was already gone failed: %v", err)
	}
}

// TestValidateKey_RefusesEscape is the string half of the containment.
//
// Each case below is a name that produced a file somewhere else, or an error
// about the wrong thing, in this repository or in one of its dependencies'
// bug trackers.
func TestValidateKey_RefusesEscape(t *testing.T) {
	for _, c := range []struct {
		key, why string
	}{
		{"", "empty"},
		{"/etc/passwd", "absolute"},
		{"../outside.png", "traversal"},
		{"a/../../outside.png", "traversal in the middle"},
		{"a/./b.png", "a dot component, so one file cannot have two keys"},
		{"a//b.png", "an empty component, so one file cannot have two keys"},
		{`a\b.png`, "a backslash, which is a second separator"},
		{"a\x00b", "a NUL"},
	} {
		if err := ValidateKey(c.key); err == nil {
			t.Errorf("ValidateKey(%q) accepted it; it is refused because it is %s", c.key, c.why)
		}
	}
}

func TestValidateKey_AcceptsOrdinaryNames(t *testing.T) {
	for _, key := range []string{
		"a.png",
		"run-20261101-120000-abc/cat-00.png",
		"a/b/c",
		"..hidden.png", // a name that merely starts with dots is a name
		"a..b.png",
	} {
		if err := ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) refused an ordinary name: %v", key, err)
		}
	}
}

// TestDir_RefusesEscapeOnEveryOperation: the check is on the way in to all
// three, not only to Put. A read of ../../etc/shadow is the interesting half.
func TestDir_RefusesEscapeOnEveryOperation(t *testing.T) {
	d := openDir(t)
	const bad = "../../etc/passwd"

	if err := d.Put(t.Context(), bad, []byte("x")); err == nil {
		t.Error("Put accepted an escaping key")
	}
	if _, err := d.Open(t.Context(), bad); err == nil {
		t.Error("Open accepted an escaping key")
	}
	if err := d.Delete(t.Context(), bad); err == nil {
		t.Error("Delete accepted an escaping key")
	}
}

// TestDir_TheRootIsTheSecondMechanism.
//
// ValidateKey is a string check and a string check misses a spelling, so the
// os.Root has to be able to refuse on its own. Checked by handing the root a
// name ValidateKey would allow but which resolves outside: a symlink planted
// in the store, which is exactly what an untrusted tar stream does.
func TestDir_TheRootIsTheSecondMechanism(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outputs")
	d, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A key with no "..", no leading slash and no empty component -- so
	// ValidateKey is happy -- that nonetheless points out of the store.
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKey("link.txt"); err != nil {
		t.Fatalf("this test needs a key ValidateKey accepts: %v", err)
	}

	f, err := d.Open(t.Context(), "link.txt")
	if err == nil {
		_ = f.Close()
		t.Fatal("the root followed a symlink out of the store")
	}
	if strings.Contains(err.Error(), "blob key") {
		t.Errorf("the string check caught this, so the root was not exercised: %v", err)
	}
}

// TestDir_ReportsWhatIsAlreadyInTheWay.
//
// "Already there" has two meanings, and this package has to tell them apart
// for the reason internal/assets does: a directory where a directory is wanted
// is fine, and a *file* there is not, because nothing under it can be written.
// Treating them alike reports success for a write that never happened.
func TestDir_ReportsWhatIsAlreadyInTheWay(t *testing.T) {
	d := openDir(t)

	// A file where a parent directory has to go.
	if err := d.Put(t.Context(), "a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(t.Context(), "a/b.png", []byte("x")); err == nil {
		t.Error("Put wrote under a key that is a regular file")
	}

	// A directory where the blob itself has to go.
	if err := d.Put(t.Context(), "c/d.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(t.Context(), "c", []byte("x")); err == nil {
		t.Error("Put overwrote a directory with a file")
	}
}

// TestDir_DeleteReportsWhatItCouldNotRemove. A non-empty directory is not
// "already gone", and reporting it as a success would be ember saying it had
// freed storage it had not.
func TestDir_DeleteReportsWhatItCouldNotRemove(t *testing.T) {
	d := openDir(t)
	if err := d.Put(t.Context(), "run/a.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(t.Context(), "run"); err == nil {
		t.Error("Delete reported success for a directory it could not remove")
	}
}

func TestOpenDir_ReportsADirectoryItCannotMake(t *testing.T) {
	// A file where the directory should be.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDir(filepath.Join(f, "outputs")); err == nil {
		t.Error("OpenDir accepted a path under a regular file")
	}
}

func TestDir_NameIsForAnErrorMessage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outputs")
	d, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Name() != dir {
		t.Errorf("Name() = %q, want %q", d.Name(), dir)
	}
}

// TestDir_DeleteTakesTheDirectoryItEmptied.
//
// A local key is "<request>/<stem>-00.png", so `ember history rm` removing
// every image of a request empties that request's directory. Left behind, it
// is one inode per pruned request for good -- in the very tree
// `ember history du` is measuring, which would then report disk no command in
// ember can free. The removal is folded into Delete rather than being a second
// method, because the alternative is a call site that has to remember it.
func TestDir_DeleteTakesTheDirectoryItEmptied(t *testing.T) {
	d := openDir(t)
	base := d.Name()

	if err := d.Put(t.Context(), "req1/a-00.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(t.Context(), "req1/a-01.png", []byte("y")); err != nil {
		t.Fatal(err)
	}

	// The first removal leaves the directory, because it is not empty -- a
	// version that removed it would have had to take the second image too.
	if err := d.Delete(t.Context(), "req1/a-00.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "req1")); err != nil {
		t.Fatalf("the directory went while an image was still in it: %v", err)
	}
	if _, err := d.Open(t.Context(), "req1/a-01.png"); err != nil {
		t.Fatalf("the other image went with its sibling: %v", err)
	}

	if err := d.Delete(t.Context(), "req1/a-01.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "req1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied directory is still there: %v", err)
	}
	// And the store's own directory is not a parent it may climb into. The
	// walk stops at ".", so an output tree that happens to be empty is not
	// removed out from under the next run.
	if _, err := os.Stat(base); err != nil {
		t.Errorf("Delete removed the store's own directory: %v", err)
	}
}

// TestDir_DeleteWalksUpOnlyAsFarAsItIsEmpty: a key several directories deep,
// with a sibling part-way up. The walk takes the levels it emptied and stops
// at the first one it did not.
func TestDir_DeleteWalksUpOnlyAsFarAsItIsEmpty(t *testing.T) {
	d := openDir(t)
	base := d.Name()

	if err := d.Put(t.Context(), "a/b/c/deep.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(t.Context(), "a/kept.png", []byte("y")); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(t.Context(), "a/b/c/deep.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "a", "b")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a/b survived its last file: %v", err)
	}
	if _, err := d.Open(t.Context(), "a/kept.png"); err != nil {
		t.Errorf("the walk climbed past a directory that was not empty: %v", err)
	}
}

// Dir must satisfy Store, or the seam is not a seam.
var _ Store = (*Dir)(nil)
