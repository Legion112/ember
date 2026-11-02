package labdaemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// LockWait is how long a suite waits for another one to finish.
//
// Generous on purpose. A full integration run is about 85 seconds, so a wait
// measured in minutes is not a queue anybody has to think about -- and
// `make test-serve-lab` holds the daemon for considerably longer, which is the
// case where failing fast would turn a legitimate queue into a red build and
// teach people to re-run rather than read.
//
// Bounded all the same, and below the 20 minutes `make test-integration` gives
// the whole binary, so the failure is one sentence naming the holder rather
// than Go's timeout panic and a dump of every goroutine in the process.
const LockWait = 10 * time.Minute

// pollInterval is how often a waiter retries the lock. flock has no timed
// wait, so exclusivity is taken non-blocking in a loop; that is also what
// makes it possible to report who is holding it while waiting.
const pollInterval = 250 * time.Millisecond

// Lock is exclusive permission to act on one Docker daemon's ember objects.
type Lock struct {
	f    *os.File
	path string
}

// Path is the lock file, for a message that has to name it.
func (l *Lock) Path() string { return l.path }

// LockPath is where the lock for this daemon lives.
//
// In the system temp directory rather than in the checkout, because the whole
// point is that two worktrees of this repository contend for it: a path under
// the repository root is a different file per worktree, which is a lock that
// serializes nothing.
func LockPath(daemonID string) string {
	sum := sha256.Sum256([]byte(daemonID))
	return filepath.Join(os.TempDir(), "ember-lab-"+hex.EncodeToString(sum[:])[:12]+".lock")
}

// Acquire takes exclusive permission to act on this daemon, waiting up to
// wait for whoever has it. A wait of zero makes one attempt.
//
// onWait, when set, is called while waiting with how long it has been and who
// is holding the lock, so a suite that is queued says so rather than looking
// hung.
//
// The lock is an advisory file lock and that is the whole reason this is not a
// pid file: the kernel drops a flock when the holding process dies, including
// under SIGKILL, a panic or a closed laptop. Two of the labs here SIGKILL
// ember on purpose and one of them is what found this, so a lock that could go
// stale would have had to be stolen -- and stealing a lock is the daemon-wide
// delete this package exists to prevent, one level of indirection away.
func Acquire(daemonID string, wait time.Duration, onWait func(waited time.Duration, holder string)) (*Lock, error) {
	path := LockPath(daemonID)

	// Read-write if it can be, read-only if it cannot. flock needs nothing but
	// a descriptor, so a lock file another user created -- a shared build host,
	// which is one of the ways two suites end up on one daemon -- still
	// serializes correctly; all that is lost is the line naming this process,
	// and reporting the wrong holder is worse than reporting none.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666) //nolint:gosec // a fixed path under TempDir keyed by the daemon's id
	readOnly := false
	if errors.Is(err, os.ErrPermission) {
		f, err = os.Open(path)
		readOnly = true
	}
	if err != nil {
		return nil, fmt.Errorf("open the lab lock %s: %w", path, err)
	}

	deadline := time.Now().Add(wait)
	start := time.Now()
	reported := time.Duration(-1)
	for {
		err := lockExclusive(f)
		if err == nil {
			l := &Lock{f: f, path: path}
			if !readOnly {
				l.stamp()
			}
			return l, nil
		}
		if !errors.Is(err, errLocked) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if waited := time.Since(start); waited >= wait || time.Now().After(deadline) {
			holder := readHolder(f)
			_ = f.Close()
			return nil, fmt.Errorf("another integration run holds this daemon: %s\n"+
				"A lab's cleanup acts on every ember-labelled object on the daemon -- that breadth is what catches the leaked bridge network nobody thought to look for -- so two suites cannot run at once. This one waited %v instead of deleting the other's containers.\n"+
				"The lock is %s and it cannot go stale: the kernel releases it when the process holding it dies, so if no suite is running any more, the process named above is stuck and killing it is the fix",
				holder, waited.Round(time.Second), path)
		}
		// Once every ten seconds, not once per poll -- and never within the
		// first second, because the holder stamps its identity *after* taking
		// the lock and a poll can land in between. That is not theoretical: on
		// the first run of two suites started together, the waiter's first
		// message named a process that had finished three minutes earlier,
		// because it read the line the previous holder left behind. A
		// diagnostic that names the wrong process is worse than one that waits
		// a second to be right.
		if onWait != nil && time.Since(start) >= time.Second {
			if w := time.Since(start).Round(10 * time.Second); w != reported {
				reported = w
				onWait(time.Since(start), readHolder(f))
			}
		}
		time.Sleep(pollInterval)
	}
}

// Release drops the lock.
//
// A suite never calls this: the only correct moment to release is process
// exit, and the kernel does that for free. `make reap-labs` does, because it
// is one short-lived command and a deferred release is how it reads.
func (l *Lock) Release() error {
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return f.Close()
}

// stamp writes who holds the lock, so a waiter can name them.
//
// Best effort and never fatal. The lock's correctness is the kernel's; this is
// only the difference between "another integration run holds this daemon" and
// a message an operator can act on.
func (l *Lock) stamp() {
	wd, err := os.Getwd()
	if err != nil {
		wd = "an unknown directory"
	}
	line := fmt.Sprintf("pid %d in %s since %s", os.Getpid(), wd, time.Now().Format(time.RFC3339))
	if err := l.f.Truncate(0); err != nil {
		return
	}
	_, _ = l.f.WriteAt([]byte(line), 0)
}

// readHolder reads the identity line a holder stamped.
func readHolder(f *os.File) string {
	buf := make([]byte, 512)
	n, err := f.ReadAt(buf, 0)
	if n == 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return "a process that left no identity in the lock file"
	}
	return string(buf[:n])
}
