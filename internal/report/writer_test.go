package report

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// collect is a Sink that keeps what it was given.
type collect struct {
	mu sync.Mutex
	es []Event
}

func (c *collect) Emit(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.es = append(c.es, e)
}

func (c *collect) texts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.es))
	for _, e := range c.es {
		out = append(out, e.Text)
	}
	return out
}

func (c *collect) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.es)
}

// TestWriter_JoinsAChunkedLine is the defect this adapter exists for.
//
// io.Copy from an SSH channel hands over whatever arrived, not whatever was
// written. Without buffering, a log pane renders "the weights loa" and then
// "ded in 4s" as two entries, and an operator reading it reasonably concludes
// the output is corrupted.
func TestWriter_JoinsAChunkedLine(t *testing.T) {
	c := &collect{}
	w := Writer(c)

	for _, chunk := range []string{"the weights ", "loa", "ded in 4s\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if got, want := c.texts(), []string{"the weights loaded in 4s\n"}; !equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriter_EmitsNothingUntilALineEnds keeps the buffering honest: a partial
// line must not reach a UI as if it were complete.
func TestWriter_EmitsNothingUntilALineEnds(t *testing.T) {
	c := &collect{}
	w := Writer(c)

	if _, err := w.Write([]byte("half a line")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if n := c.len(); n != 0 {
		t.Fatalf("emitted %d event(s) for a line that has not ended", n)
	}

	// ...and Close must not lose it. A caller that forgets to close loses the
	// last thing the node said, which is usually the error.
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, want := c.texts(), []string{"half a line"}; !equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriter_TreatsCarriageReturnAsALineEnding is why \r is in the delimiter
// set.
//
// sd-cli reports progress by overwriting one line with \r and never writing a
// newline until it finishes. Splitting on \n alone means a progress display
// that shows nothing for the whole of a generation and then everything at
// once -- which is indistinguishable from a hang, during the one phase an
// operator is watching.
func TestWriter_TreatsCarriageReturnAsALineEnding(t *testing.T) {
	c := &collect{}
	w := Writer(c)

	if _, err := w.Write([]byte("  10%\r  20%\r")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The second \r is the last byte, so it is held: it may be the first half
	// of \r\n, and emitting now would invent an empty line.
	if got, want := c.texts(), []string{"  10%\r"}; !equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	if _, err := w.Write([]byte("  30%\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := c.texts(), []string{"  10%\r", "  20%\r", "  30%\n"}; !equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriter_TreatsCrlfAsOneEnding: \r\n split across two events would put an
// empty line into the stream that the node never wrote.
func TestWriter_TreatsCrlfAsOneEnding(t *testing.T) {
	c := &collect{}
	w := Writer(c)

	// Deliberately split *between* the \r and the \n, which is exactly how a
	// socket read can arrive.
	if _, err := w.Write([]byte("done\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write([]byte("\nnext\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got, want := c.texts(), []string{"done\r\n", "next\n"}; !equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriter_ThroughTextIsIdentity is the property that lets a node's own
// output pass through a UI without being rewritten.
//
// TestStartServer_StreamsTheOutputAndReaps asserts a remote process's bytes
// arrive in ember's writer unchanged. If the terminator did not travel with
// the text -- if the renderer added "\n" instead -- a \r progress stream would
// come out as hundreds of lines and a tail with no newline would gain one.
func TestWriter_ThroughTextIsIdentity(t *testing.T) {
	const in = "loading\nweights\r  10%\r  20%\r\ndone\nno trailing newline"

	var buf bytes.Buffer
	w := Writer(Text(&buf))
	// Written one byte at a time, which is the worst case any chunking can
	// produce.
	for i := 0; i < len(in); i++ {
		if _, err := w.Write([]byte{in[i]}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := buf.String(); got != in {
		t.Errorf("round trip changed the bytes:\n got %q\nwant %q", got, in)
	}
}

// TestWriter_FlushesAnEndlessLine: a remote process that writes megabytes
// without a newline must not buffer until the run ends. The operator watching
// a wedged workload would otherwise see nothing at all, during the one moment
// the output matters most.
func TestWriter_FlushesAnEndlessLine(t *testing.T) {
	c := &collect{}
	w := Writer(c)

	if _, err := w.Write([]byte(strings.Repeat("x", maxSegment+10))); err != nil {
		t.Fatalf("write: %v", err)
	}

	if n := c.len(); n != 1 {
		t.Fatalf("emitted %d event(s), want 1 flush of the oversized segment", n)
	}
	if got := len(c.texts()[0]); got != maxSegment+10 {
		t.Errorf("flushed %d bytes, want %d", got, maxSegment+10)
	}
}

// TestWriter_SurvivesTwoWriters is not hypothetical: workload.Run sets the
// remote command's Stderr to this writer and copies its stdout to the same
// writer from the calling goroutine, and starting an sd-server does the same.
// A terminal tolerates interleaved writes; a buffer sliced by two goroutines
// is a data race.
func TestWriter_SurvivesTwoWriters(t *testing.T) {
	c := &collect{}
	w := Writer(c)

	const n = 200
	var wg sync.WaitGroup
	for _, who := range []string{"out", "err"} {
		wg.Add(1)
		go func(who string) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				// One whole line per Write -- Fprintf formats into a buffer
				// and calls Write once -- so the mutex makes each line atomic
				// even though the two goroutines interleave.
				if _, err := fmt.Fprintf(w, "%s %d\n", who, i); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(who)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := c.len(); got != 2*n {
		t.Errorf("got %d events, want %d; lines were split or lost", got, 2*n)
	}
	for _, s := range c.texts() {
		if !strings.HasSuffix(s, "\n") || strings.Count(s, "\n") != 1 {
			t.Errorf("event %q is not exactly one line", s)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
