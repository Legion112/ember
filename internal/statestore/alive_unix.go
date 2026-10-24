//go:build unix

package statestore

import (
	"errors"
	"os"
	"syscall"
)

// alive reports whether a process with this pid exists on this machine.
//
// Signal 0 performs the permission and existence checks without delivering
// anything, which is the standard way to ask. EPERM counts as alive: the
// process exists and belongs to another user, and treating "I may not signal
// it" as "it is gone" would report a node as abandoned while its owner is
// still using it.
//
// Pid reuse makes a false "alive" possible, and that is the safe direction:
// ember would simply not report a finding it could have. A false "dead" would
// have it tell the operator their running session is abandoned.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	switch {
	case err == nil:
		return true
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EPERM):
		return true
	default:
		return false
	}
}
