package sshexec

import (
	"bytes"
	"errors"
	"sync"

	"golang.org/x/crypto/ssh"
)

// maxStderr bounds the buffered stderr.
//
// A remote command that writes megabytes to stderr -- tar listing every file
// it skipped, for instance -- must not put all of it into one error message,
// and must not grow unboundedly in memory while it does.
const maxStderr = 8 << 10

type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := maxStderr - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	// The full length is reported written so the remote side is never
	// throttled by ember's own buffer being full.
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func asExitError(err error, out **ssh.ExitError) bool {
	return errors.As(err, out)
}
