// Package transfer brings a run's outputs back from a node before it dies.
//
// The mechanism is deliberately boring: run `tar` on the node, stream the
// archive over the SSH connection, extract it here. No SFTP client, no extra
// dependency, and nothing written on the node.
//
// Everything interesting is in the extraction, because a tar stream from a
// machine ember rented for twenty minutes is untrusted input. A tar member can
// name "../../.ssh/authorized_keys" or be a symlink to "/etc/passwd", and an
// extractor that joins paths and writes is the oldest file-overwrite bug there
// is. This one writes through an os.Root, so an escape is refused by the
// kernel-level check rather than by a string comparison somebody has to get
// right.
package transfer

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
)

// MaxBytes bounds one extracted archive.
//
// A node ember does not control decides how much it sends, so without a limit
// a misbehaving or hostile workload fills the operator's disk. 64 GiB is far
// more than any plausible run of generated media and still a limit.
const MaxBytes int64 = 64 << 30

// MaxFiles bounds how many members one archive may hold.
//
// The companion to MaxBytes: a tar of ten million empty files is small and
// still takes the filesystem down.
const MaxFiles = 500_000

// Result is what one fetch retrieved.
type Result struct {
	Files int
	Bytes int64
}

// Fetch retrieves every output path into dir.
//
// dir is opened as an os.Root and every write goes through it, so no archive
// member can land outside it whatever it claims to be called.
//
// A failure on one output does not abandon the others: a run that produced
// renders and a log should not lose the renders because the log was missing.
// The errors are joined and returned after everything that could be fetched
// has been.
func Fetch(ctx context.Context, r sshx.Runner, outputs []node.OutputPath, dir string) (Result, error) {
	if len(outputs) == 0 {
		return Result{}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create output directory %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Result{}, fmt.Errorf("open output directory %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	var total Result
	var errs []error
	for _, o := range outputs {
		res, err := fetchOne(ctx, r, o, root)
		total.Files += res.Files
		total.Bytes += res.Bytes
		if err != nil {
			errs = append(errs, fmt.Errorf("output %s: %w", o.Remote, err))
		}
	}
	return total, errors.Join(errs...)
}

// TarCommand is the command run on the node for one output path.
//
// -C into the parent and name the base, rather than tarring an absolute path:
// GNU tar strips a leading slash with a warning and BSD tar does not, so the
// member names would differ by platform and the extracted layout with them.
// Naming the base explicitly makes the archive's shape ember's decision.
//
// Compression is on the node, where the CPU is rented and the bottleneck is
// the operator's home upload link.
func TarCommand(remote string) (sshx.Command, error) {
	if !path.IsAbs(remote) || path.Clean(remote) != remote || remote == "/" {
		return sshx.Command{}, fmt.Errorf("output path %q must be absolute, clean and not the root", remote)
	}
	cmd := sshx.Cmd("tar",
		"-czf", "-",
		"-C", path.Dir(remote),
		"--", path.Base(remote),
	)
	if err := cmd.Validate(); err != nil {
		return sshx.Command{}, err
	}
	return cmd, nil
}

func fetchOne(ctx context.Context, r sshx.Runner, o node.OutputPath, root *os.Root) (Result, error) {
	cmd, err := TarCommand(o.Remote)
	if err != nil {
		return Result{}, err
	}

	stdout, wait, err := r.Run(ctx, cmd)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = stdout.Close() }()

	res, extractErr := Extract(ctx, stdout, root, o.Local)

	// The exit status is read after the stream, and reported even when the
	// extraction succeeded. A tar that failed halfway produces a valid prefix
	// of an archive: the extraction is happy, the data is incomplete, and
	// that is exactly how an output fetch loses files quietly.
	if err := wait(); err != nil {
		if extractErr != nil {
			return res, errors.Join(extractErr, err)
		}
		return res, err
	}
	return res, extractErr
}

// Extract writes a gzipped tar stream into root under prefix.
//
// Only regular files and directories are written. A symlink, hard link,
// device node or anything else is skipped with its name reported: ember is
// retrieving generated media from a machine it rented, and there is no
// legitimate reason for a render to be a block device. Refusing the kinds
// outright is shorter than reasoning about where each one could point.
func Extract(ctx context.Context, src io.Reader, root *os.Root, prefix string) (Result, error) {
	if strings.Contains(prefix, "..") || filepath.IsAbs(prefix) {
		return Result{}, fmt.Errorf("local output name %q must be a relative path inside the run directory", prefix)
	}

	gz, err := gzip.NewReader(io.LimitReader(src, MaxBytes))
	if err != nil {
		return Result{}, fmt.Errorf("the node did not send a gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var res Result
	var skipped []string

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, fmt.Errorf("read the archive: %w", err)
		}

		if res.Files >= MaxFiles {
			return res, fmt.Errorf("the archive holds more than %d entries; ember stopped rather than fill the disk", MaxFiles)
		}

		// The member name is attacker-controlled, so it is cleaned and
		// checked before it is used, and then used only through root.
		name, err := safeName(prefix, h.Name)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", h.Name, err))
			continue
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllIn(root, name); err != nil {
				return res, err
			}
		case tar.TypeReg:
			n, err := writeFile(root, name, tr, h)
			res.Bytes += n
			if err != nil {
				return res, err
			}
			res.Files++
		default:
			skipped = append(skipped, fmt.Sprintf("%s (%s)", h.Name, kindName(h.Typeflag)))
		}
	}

	if len(skipped) > 0 {
		// Reported rather than silently dropped: an operator who asked for a
		// directory and got fewer files than they expected needs to know it
		// was ember's decision and which entries it applied to.
		return res, &SkippedError{Names: skipped}
	}
	return res, nil
}

// SkippedError lists archive members ember declined to write.
type SkippedError struct{ Names []string }

func (e *SkippedError) Error() string {
	const show = 10
	names := e.Names
	more := 0
	if len(names) > show {
		more = len(names) - show
		names = names[:show]
	}
	s := fmt.Sprintf("%d archive entr%s were not written: %s",
		len(e.Names), plural(len(e.Names)), strings.Join(names, ", "))
	if more > 0 {
		s += fmt.Sprintf(", and %d more", more)
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// safeName turns an archive member name into a path under prefix.
//
// The containment is enforced twice, by two different mechanisms: this check,
// and the os.Root the write goes through. Belt and braces, because this is the
// oldest file-overwrite bug there is and the two failure modes are different --
// a string check misses a spelling, and a Root cannot tell ember that the
// archive was trying something.
func safeName(prefix, name string) (string, error) {
	if name == "" {
		return "", errors.New("empty name")
	}

	// The component-by-component check, and the reason it is not a Clean,
	// live in node.SafeRelPath. They are shared because a tar member from a
	// rented machine and an asset name from a config file are the same
	// problem, and a containment check written twice is a containment check
	// fixed once. An absolute member name is refused there rather than
	// reinterpreted as relative: most extractors strip the leading slash, and
	// that is contained -- os.Root sees to that -- but ember's own TarCommand
	// never produces one, so an absolute name means this archive was not made
	// the way ember asks, which is worth saying rather than quietly fixing.
	rel, err := node.SafeRelPath(name)
	if err != nil {
		return "", err
	}
	return path.Join(prefix, rel), nil
}

func mkdirAllIn(root *os.Root, name string) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		err := root.Mkdir(p, 0o700)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrExist):
			// Already there -- but "already there" has two meanings and only
			// one of them is fine. A directory is fine: two outputs can land
			// in overlapping trees, and a second run into the same place is
			// an ordinary thing to do. A *file* of that name is not: nothing
			// under it can be written, and an earlier version treating both
			// alike reported success for a directory it never created and
			// then failed confusingly on the first file inside it.
			fi, statErr := root.Stat(p)
			if statErr != nil {
				return fmt.Errorf("create %s: %w", p, statErr)
			}
			if !fi.IsDir() {
				return fmt.Errorf("create %s: a file of that name is already there, so nothing under it can be written", p)
			}
		default:
			return fmt.Errorf("create %s: %w", p, err)
		}
	}
	return nil
}

func writeFile(root *os.Root, name string, src io.Reader, h *tar.Header) (int64, error) {
	if dir := path.Dir(name); dir != "." && dir != "/" {
		if err := mkdirAllIn(root, dir); err != nil {
			return 0, err
		}
	}

	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	// Bounded by the header's own size. A member claiming 1 KiB and sending a
	// gigabyte is a decompression bomb, and the limit on the whole stream
	// would not catch it until the disk was already much fuller.
	n, err := io.Copy(f, io.LimitReader(src, h.Size))
	if err != nil {
		return n, fmt.Errorf("write %s: %w", name, err)
	}
	if n != h.Size {
		return n, fmt.Errorf("%s was truncated: the header said %d bytes and the stream held %d", name, h.Size, n)
	}
	return n, nil
}

func kindName(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "device node"
	case tar.TypeFifo:
		return "fifo"
	default:
		return fmt.Sprintf("unsupported type %q", t)
	}
}

// RunDir is where one run's outputs go.
//
// The run id is in the name so two runs cannot overwrite each other, and the
// timestamp is first so a listing sorts chronologically -- which is what an
// operator looking for "the one I did this morning" actually wants.
func RunDir(base string, runID node.RunID, started time.Time) string {
	return filepath.Join(base, fmt.Sprintf("%s-%s", started.UTC().Format("20060102-150405"), runID))
}
