//go:build integration && unix

package integration

import (
	"os"
	"syscall"
)

// interrupt is SIGINT: what Ctrl-C sends. Split into its own file so the
// labs' intent stays readable and the platform detail stays in one place.
func interrupt() os.Signal { return syscall.SIGINT }
