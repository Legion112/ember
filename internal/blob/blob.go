// Package blob is where generated images live, behind one narrow interface.
//
// WHY AN INTERFACE FOR A DIRECTORY. Today every image a run produces is
// written to a directory under the state dir, which is where an operator has
// always found them and where they should stay. What is coming is somewhere
// else: an S3 bucket, a NAS. Those are a different lifetime and a different
// failure mode from a local file, and the place to absorb that difference is
// one seam rather than every call site that serves a PNG.
//
// The S3 implementation is not speculative work left undone -- internal/s3 and
// internal/awssig already exist, because `ember store push` does SigV4
// uploads. What is missing is only the adapter.
//
// CONTAINMENT IS ENFORCED TWICE, by two different mechanisms, which is the
// rule internal/transfer already follows for tar members: ValidateKey checks
// the name component by component, and every read and write goes through an
// os.Root. A string check misses a spelling; a Root cannot tell the caller
// what the name was trying to do.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Store holds bytes under a key.
//
// Open returns an io.ReadSeekCloser rather than an io.ReadCloser because
// http.ServeContent needs to seek: without it a browser cannot ask for a byte
// range, which for a video it is scrubbing through is the whole interaction.
type Store interface {
	Put(ctx context.Context, key string, b []byte) error
	Open(ctx context.Context, key string) (io.ReadSeekCloser, error)

	// Delete removes key, and with it any directory the removal left empty.
	//
	// THE SECOND HALF IS IN THE CONTRACT RATHER THAN IN A SEPARATE METHOD,
	// because the alternative leaves something nothing cleans up. A local key
	// is "<request>/<stem>-00.png", so `ember history rm` removing every
	// image of a request empties that request's directory and leaves it
	// there -- one inode per pruned request, for good, in the tree
	// `ember history du` is measuring. A backend with no directories, which
	// is what internal/s3 will be, implements this as the removal alone and
	// the sentence still reads true of it.
	//
	// It is not a prefix delete and must not become one: a recursive remove
	// driven by a request id is the hazard the containment rules above exist
	// for, and it would take files the operator put there themselves.
	Delete(ctx context.Context, key string) error
}

// ErrNotFound means no such key. Callers distinguish it from a failure to
// read, because the first is a 404 and the second is a 500.
var ErrNotFound = errors.New("no such blob")

// ValidateKey refuses a key that would not stay where it was put.
//
// Slash-separated, relative, and every component a real name. The refusals
// below are each a thing that produced a file somewhere else, or an error
// about the wrong thing:
//
//   - An absolute key, because joining one onto a root is either refused or
//     silently re-rooted depending on who does the joining.
//   - "..", refused and never cleaned. path.Clean("/../a.png") returns
//     "/a.png" -- it *absorbs* the traversal, because nothing is above root --
//     so a check that cleaned first could never fire. internal/transfer
//     shipped exactly that bug.
//   - A backslash, because a key is not a Windows path and accepting both
//     separators means two spellings of one key.
//   - An empty component, so "a//b" and "a/b" cannot both exist.
func ValidateKey(key string) error {
	if key == "" {
		return errors.New("blob key is empty")
	}
	if strings.ContainsAny(key, "\\\x00") {
		return fmt.Errorf("blob key %q contains a backslash or a NUL; keys are slash-separated names", key)
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("blob key %q is absolute; keys are relative to the store", key)
	}
	for _, part := range strings.Split(key, "/") {
		switch part {
		case "":
			return fmt.Errorf("blob key %q has an empty path component", key)
		case ".", "..":
			return fmt.Errorf("blob key %q contains %q; it is refused rather than cleaned, because "+
				"cleaning absorbs a traversal and the check could then never fire", key, part)
		}
	}
	return nil
}

// Dir is the local store: one directory, reached through an os.Root.
//
// IT IS ROOTED AT THE OUTPUT DIRECTORY and not at one run's subdirectory, so
// a key spans runs -- "<run>/<stem>-00.png" -- and listing what a request
// produced does not mean opening a different store per rental.
//
// Put IS A NO-OP FOR A KEY THAT IS ALREADY THERE, and that asymmetry is
// deliberate rather than an oversight. sdserver.WriteImages writes a job's
// images into the run directory as it collects them, through an os.Root, under
// exactly the names this store serves -- so for the local case the bytes have
// already landed and a second write would be the same bytes twice. A remote
// backend's Put does the real work. Somebody will want to "fix" this into a
// copy; the fix would double every write to disk.
type Dir struct {
	root *os.Root
	name string
}

// OpenDir opens or creates the directory and roots a store at it.
func OpenDir(dir string) (*Dir, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the blob directory %s: %w", dir, err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open the blob directory %s: %w", dir, err)
	}
	return &Dir{root: r, name: dir}, nil
}

// Close releases the root.
func (d *Dir) Close() error { return d.root.Close() }

// Name is the directory this store is rooted at, for an error message.
func (d *Dir) Name() string { return d.name }

// Put writes b under key, creating any parent directories.
//
// 0600, as every other file ember writes: a generated image is the operator's
// and nobody else's business on a shared machine.
func (d *Dir) Put(_ context.Context, key string, b []byte) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if dir := path.Dir(key); dir != "." {
		if err := d.root.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s in %s: %w", dir, d.name, err)
		}
	}
	if err := d.root.WriteFile(key, b, 0o600); err != nil {
		return fmt.Errorf("write %s in %s: %w", key, d.name, err)
	}
	return nil
}

// Open returns the bytes stored under key.
func (d *Dir) Open(_ context.Context, key string) (io.ReadSeekCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	f, err := d.root.Open(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s in %s: %w", key, d.name, err)
	}
	return f, nil
}

// Delete removes key and any directory of it the removal left empty.
//
// A key that is already gone is a success, for the reason provider.Destroy
// treats a missing node as one: the goal is that it is not there, and it is
// not.
func (d *Dir) Delete(_ context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := d.root.Remove(key); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s in %s: %w", key, d.name, err)
	}
	d.pruneDirs(path.Dir(key))
	return nil
}

// pruneDirs removes dir and its parents for as long as they are empty.
//
// ERRORS ARE DISCARDED ON PURPOSE, AND THIS IS THE ONLY PLACE HERE THAT DOES.
// The caller asked for a file to be gone and it is; a directory that would not
// go is either not empty -- which is the ordinary case, because two requests'
// keys can share no parent but the root and a run directory holds more than
// one image until the last one goes -- or is a permissions problem on
// something the operator owns. Reporting either as a failed Delete would make
// `ember history rm` print an error over bytes it successfully freed, and the
// first of them is not even unusual.
//
// Rooted, so the walk cannot climb out: "." is the store's own directory and
// stops the loop, and os.Root refuses a name that would leave it anyway. The
// bound on the loop is the key's own depth, which ValidateKey has already
// refused "." and ".." from.
func (d *Dir) pruneDirs(dir string) {
	for dir != "." && dir != "/" && dir != "" {
		if err := d.root.Remove(dir); err != nil {
			return
		}
		dir = path.Dir(dir)
	}
}
