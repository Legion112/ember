// Package assets puts model weights on a store and proves they are the right
// bytes before anything expensive runs.
//
// Both halves live here on purpose. Ensure runs on the node and does the
// fetching; Command runs on the laptop and builds the argv that invokes it
// over SSH. They have to agree on every flag name, and a disagreement would
// show up as a node that fetched nothing and a workload that found an empty
// directory -- so they are written next to each other and tested against each
// other rather than in two packages that drift.
package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
)

// Policy says what to do about an asset that is already on the store.
type Policy string

const (
	// VerifyAlways re-hashes an asset that is already present, every run.
	//
	// The default, and the reason is the store itself: it is writable by
	// every node that mounts it, which makes it a channel between runs that
	// are otherwise isolated. One workload can leave something behind that
	// the next one loads as its model. RunPod offers no read-only mount, so
	// the write cannot be prevented -- it is detected instead, before any
	// GPU time is spent on it.
	VerifyAlways Policy = "always"

	// VerifyPresent accepts a file that is there, without re-reading it.
	//
	// It trades the check above for the time it costs, which at roughly a
	// gigabyte a second is about forty seconds on a forty-gigabyte model set.
	// Worth having as a choice; not worth being the default, because the
	// run it protects costs dollars and the check costs cents.
	VerifyPresent Policy = "present"
)

// DefaultPolicy is what an unset `verify:` means.
const DefaultPolicy = VerifyAlways

// ParsePolicy reads a policy name, refusing anything else.
//
// Parse, do not grep: a misspelled `verify: alwyas` must not quietly become
// the weaker setting, which is exactly the direction a typo would go
// unnoticed in.
func ParsePolicy(s string) (Policy, error) {
	switch Policy(s) {
	case "":
		return DefaultPolicy, nil
	case VerifyAlways:
		return VerifyAlways, nil
	case VerifyPresent:
		return VerifyPresent, nil
	default:
		return "", fmt.Errorf("unknown verify policy %q: use %q or %q", s, VerifyAlways, VerifyPresent)
	}
}

// MaxBytes bounds a single download.
//
// A server that streams forever would otherwise fill the store, and the store
// is the one thing here that nothing cleans up. The ceiling is far above any
// real checkpoint -- RunPod's own volume maximum is 4 TiB -- so hitting it
// means something is wrong rather than that the model is large.
const MaxBytes int64 = 512 << 30

// Result says what Ensure did, so the caller can say it too.
type Result struct {
	// Fetched is true when the file was downloaded, false when it was
	// already there.
	Fetched bool

	// Bytes is how big the asset is, whether fetched or verified in place.
	Bytes int64

	// Verified is true when the digest was checked against the file's
	// contents during this call.
	Verified bool
}

// Ensure makes one asset present under dir and proves it is the right bytes.
func Ensure(ctx context.Context, c *http.Client, dir string, a node.Asset, p Policy, out io.Writer) (Result, error) {
	a, err := a.Validate()
	if err != nil {
		return Result{}, err
	}
	if p == "" {
		p = DefaultPolicy
	}

	full := filepath.Join(dir, filepath.FromSlash(a.As))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return Result{}, fmt.Errorf("make room for %s: %w", a.As, err)
	}

	switch fi, statErr := os.Stat(full); {
	case statErr == nil && fi.IsDir():
		return Result{}, fmt.Errorf("%s is a directory on the store; ember will not replace it", a.As)
	case statErr == nil:
		if p == VerifyPresent {
			say(out, "%s is present (%d bytes), not re-read\n", a.As, fi.Size())
			return Result{Bytes: fi.Size()}, nil
		}
		sum, n, err := hashFile(full)
		if err != nil {
			return Result{}, err
		}
		if sum != a.SHA256 {
			// Named loudly and never repaired by re-downloading. Something
			// wrote to the store, and whether that was corruption or another
			// run is not knowable from here -- what is knowable is that the
			// bytes are not the ones this run asked for, and that is the
			// moment to stop rather than the moment to guess.
			return Result{}, fmt.Errorf("%s on the store does not match its digest: expected %s, found %s. "+
				"The store is writable by every node that mounts it, so something changed these bytes. "+
				"Remove the file and let ember fetch it again once you know what did", a.As, a.SHA256, sum)
		}
		say(out, "%s is present and verified (%d bytes)\n", a.As, n)
		return Result{Bytes: n, Verified: true}, nil
	case !errors.Is(statErr, os.ErrNotExist):
		return Result{}, fmt.Errorf("look for %s: %w", a.As, statErr)
	}

	n, err := download(ctx, c, full, a, out)
	if err != nil {
		return Result{}, err
	}
	return Result{Fetched: true, Bytes: n, Verified: true}, nil
}

// download fetches the asset to a partial file, verifies it, then renames.
//
// Never written straight to its final name. A download interrupted halfway
// would otherwise leave a short file that the next run finds "present" --
// and under VerifyPresent, loads as a model.
func download(ctx context.Context, c *http.Client, full string, a node.Asset, out io.Writer) (int64, error) {
	if c == nil {
		c = DefaultClient()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", a.As, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch %s: %s returned %s", a.As, a.URL, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".partial-*")
	if err != nil {
		return 0, fmt.Errorf("make a partial file for %s: %w", a.As, err)
	}
	name := tmp.Name()
	// Removed on every path that does not rename it away. A partial left on
	// the store is a file nothing will clean up and the operator pays to
	// keep.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()

	say(out, "fetching %s from %s\n", a.As, a.URL)

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, MaxBytes))
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", a.As, err)
	}
	if n >= MaxBytes {
		return 0, fmt.Errorf("fetch %s: stopped at %d bytes, which is this build's ceiling", a.As, MaxBytes)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("finish %s: %w", a.As, err)
	}

	if sum := hex.EncodeToString(h.Sum(nil)); sum != a.SHA256 {
		return 0, fmt.Errorf("%s does not match its digest: expected %s, got %s from %s. "+
			"Nothing was written to the store", a.As, a.SHA256, sum, a.URL)
	}

	// Renamed only after the digest matched, so a file at the final name is
	// always one that has been verified at least once.
	if err := os.Rename(name, full); err != nil {
		return 0, fmt.Errorf("put %s in place: %w", a.As, err)
	}
	if err := os.Chmod(full, 0o600); err != nil {
		return 0, fmt.Errorf("set permissions on %s: %w", a.As, err)
	}
	say(out, "fetched %s (%d bytes, verified)\n", a.As, n)
	return n, nil
}

func hashFile(p string) (string, int64, error) {
	// The path is built from node.SafeRelPath, which checks the name
	// component by component before any cleaning and refuses "..", a leading
	// separator and a backslash spelling of either.
	//
	// Not an os.Root, unlike internal/transfer, and the difference is the
	// input. There, member names come out of a tar stream produced by a
	// machine ember rented from a stranger, so containment is enforced twice
	// because a string check misses a spelling. Here the name comes from the
	// operator's own ember.yaml. Opening an os.Root would still be the
	// stronger construction; it is not used only because os.CreateTemp has
	// no Root equivalent and hand-rolling one for a non-hostile input buys
	// less than it costs to read.
	f, err := os.Open(p) //nolint:gosec // validated by node.SafeRelPath; see above
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", filepath.Base(p), err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", filepath.Base(p), err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// DefaultClient is the client Ensure uses when given none.
//
// No overall timeout, because a forty-gigabyte checkpoint legitimately takes
// minutes and a client timeout would turn a working download into a failure
// nobody could explain. The bounds that matter are on the parts that can hang
// without transferring anything: connecting, and waiting for the first byte
// of the response. A stalled transfer is ended by the caller's context, which
// is the node's own deadline.
func DefaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// Command builds the argv that runs Ensure on a node, over SSH.
//
// It is the other half of this package, and it is here rather than in the CLI
// so that the flag names it writes and the flag names Ensure's caller reads
// cannot drift apart without a test in this package noticing.
func Command(mount node.Mount, a node.Asset, p Policy) (sshx.Command, error) {
	if mount.IsZero() {
		return sshx.Command{}, errors.New("no mount: an asset needs a store to land on")
	}
	v, err := a.Validate()
	if err != nil {
		return sshx.Command{}, err
	}
	if p == "" {
		p = DefaultPolicy
	}
	cmd := sshx.Cmd(NodeBinary, "fetch",
		"--dir", mount.Path(),
		"--url", v.URL,
		"--sha256", v.SHA256,
		"--as", v.As,
		"--verify", string(p),
	)
	if err := cmd.Validate(); err != nil {
		return sshx.Command{}, err
	}
	return cmd, nil
}

// NodeBinary is what runs Ensure on the node. It is in the image already, as
// the thing that installed the host key and started sshd.
const NodeBinary = "embernode"

func say(out io.Writer, format string, a ...any) {
	if out == nil {
		return
	}
	_, _ = fmt.Fprintf(out, format, a...)
}
