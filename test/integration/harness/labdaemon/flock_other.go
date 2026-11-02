//go:build !unix

package labdaemon

import (
	"errors"
	"fmt"
	"os"
)

// errLocked means somebody else holds the lock right now.
var errLocked = errors.New("the lab lock is held")

// lockExclusive cannot be implemented here, so it refuses.
//
// A refusal and not a no-op. The lock is what keeps one suite's cleanup from
// deleting another suite's live containers, so a platform where it cannot be
// taken is a platform where the labs must not run at all -- pretending the
// lock was taken would reinstate the defect silently, which is the shape of
// every cleanup bug this harness has had. These labs need a Docker daemon,
// /bin/sh and a SIGKILL anyway, so nothing is lost that was working.
func lockExclusive(*os.File) error {
	return fmt.Errorf("the lab lock needs flock, which this platform does not have; the labs are Unix-only")
}
