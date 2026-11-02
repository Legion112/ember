//go:build unix

package labdaemon

import (
	"errors"
	"os"
	"syscall"
)

// errLocked means somebody else holds the lock right now.
var errLocked = errors.New("the lab lock is held")

// lockExclusive takes the whole-file lock without blocking.
//
// Non-blocking because flock offers no timed wait, and a caller that cannot
// report who it is waiting for -- or give up with a message -- is a caller
// that looks hung. The retry loop is in Acquire.
//
// It is an flock and not a lock file whose existence means "held", because
// the kernel releases an flock when the process holding it dies. Two labs in
// this suite SIGKILL ember on purpose, the machine this was written for runs
// several agents that get interrupted, and a lock that survives its holder
// would have to be stealable -- which puts the daemon-wide delete back.
func lockExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return errLocked
	default:
		return err
	}
}
