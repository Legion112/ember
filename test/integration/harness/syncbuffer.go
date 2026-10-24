package harness

import (
	"bytes"
	"sync"
)

// syncBuffer collects a subprocess's output from both streams.
//
// cmd.Stdout and cmd.Stderr are set to the same value so the labs can assert
// on ordinary interleaved output. os/exec then writes to it from two
// goroutines, so a bare bytes.Buffer is a data race that -race reports as a
// failure in the lab rather than in the harness.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
