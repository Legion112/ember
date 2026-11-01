package report

import (
	"bytes"
	"io"
	"sync"
)

// maxSegment bounds how much is buffered waiting for a line ending.
//
// A remote process that writes a megabyte without a newline would otherwise
// grow this buffer until the run ends, and nothing would reach the UI in the
// meantime -- so the operator watching a wedged workload sees nothing at all,
// which is the one moment the output matters most.
const maxSegment = 64 << 10

// Writer adapts a Sink to an io.Writer, emitting KindLog events.
//
// IT EXISTS so that the packages which only print -- gpucheck, and whatever a
// workload or an sd-server writes to its own stdout -- need no change to reach
// a UI. They keep taking an io.Writer; the web path hands them this one.
//
// IT BUFFERS TO A LINE ENDING, and that is not a nicety. io.Copy from an SSH
// channel hands over whatever arrived, not whatever was written: a naive
// adapter emits "the weights loa" as one event and "ded in 4s\n" as the next,
// and a log pane renders shredded text. Both \n and \r terminate a segment,
// because sd-cli reports progress by overwriting one line with \r, and a
// buffer waiting for a newline that never comes is a progress display that
// never updates.
//
// THE TERMINATOR TRAVELS WITH THE TEXT. Text writes a KindLog event's Text
// verbatim, so bytes -> Writer -> Sink -> Text -> stdout is byte-for-byte
// identity. Stripping the terminator and letting the renderer add "\n" would
// turn a \r progress stream into three hundred lines, and would quietly
// rewrite what a rented machine actually said.
//
// IT HOLDS A MUTEX because two goroutines write to this today: workload.Run
// sets the remote command's Stderr to the writer and copies its stdout to the
// same writer from the calling goroutine, and starting an sd-server does the
// same. A terminal tolerates interleaved writes; a buffer being sliced by two
// goroutines is a data race that -race reports.
//
// Close flushes a trailing partial line. A caller that forgets loses the last
// thing the node said, which is usually the error.
func Writer(s Sink) io.WriteCloser { return &writer{s: s} }

type writer struct {
	mu  sync.Mutex
	buf []byte
	s   Sink
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	w.drain(false)
	return len(p), nil
}

// Close flushes whatever has no terminator yet.
func (w *writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drain(true)
	return nil
}

// drain emits every complete segment. final also emits the incomplete tail.
func (w *writer) drain(final bool) {
	for {
		i := bytes.IndexAny(w.buf, "\n\r")
		if i < 0 {
			break
		}
		end := i + 1
		if w.buf[i] == '\r' {
			// A lone \r at the end of what has arrived so far may be the
			// first half of \r\n. Emitting now would split one line ending
			// into two events, the second of which is an empty line the node
			// never wrote -- so it waits for the next Write, unless there
			// will not be one.
			if end == len(w.buf) && !final {
				break
			}
			if end < len(w.buf) && w.buf[end] == '\n' {
				end++
			}
		}
		w.emit(w.buf[:end])
		// Compacted rather than resliced: w.buf = w.buf[end:] leaves append
		// extending the original array forever, so a long-running server's
		// output never releases any of it.
		w.buf = append(w.buf[:0], w.buf[end:]...)
	}

	if final || len(w.buf) >= maxSegment {
		if len(w.buf) > 0 {
			w.emit(w.buf)
			w.buf = w.buf[:0]
		}
	}
}

func (w *writer) emit(b []byte) {
	// Copied into a string here: b aliases the buffer, which the next Write
	// compacts in place.
	w.s.Emit(Event{Kind: KindLog, Text: string(b)})
}
