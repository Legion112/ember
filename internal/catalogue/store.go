package catalogue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DirName is the subdirectory of the state directory holding the series.
//
// Named once rather than spelled at each call site, for the reason
// statestore.DirName is: "nodes" was a literal in the CLI and a second
// literal in the lab harness, the two drifted, and an assertion about an
// empty journal passed while reading a directory nobody wrote to.
const DirName = "catalogue"

// ext is the suffix of a day file. JSON Lines, so a day is appended to and
// never rewritten.
const ext = ".jsonl"

// Store is a directory of day files, one line per sample.
//
// APPEND-ONLY, AND THAT IS THE DESIGN. The entire value of this directory is
// the series: a sample is one point of a measurement that cannot be taken
// again, so a format that can rewrite history is one that can lose it to a
// bug in the writer. There is no Update, no Delete and no compaction here.
// Removing old days is `rm`, where the operator can see what they are
// destroying.
type Store struct {
	dir string

	// mu serialises this process's own appends. It does nothing about a
	// second ember -- an hourly timer and a manual run can overlap -- which
	// is why each day's batch goes out in one write call; see Append.
	mu sync.Mutex
}

// Open returns a store rooted at dir. The directory is created on first
// append.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("catalogue directory is empty")
	}
	return &Store{dir: dir}, nil
}

// Dir reports where the series is kept.
func (s *Store) Dir() string { return s.dir }

// Path is the file a sample taken at t belongs to.
func (s *Store) Path(t time.Time) string {
	return filepath.Join(s.dir, t.UTC().Format(dayLayout)+ext)
}

// Append writes samples to their day files and returns the files it touched.
//
// Every sample is validated first and one failure refuses the whole batch.
// Half a snapshot is worse than none: the file is never rewritten, so a row
// that cannot be placed in the series is there for good, and a snapshot
// missing half its rows reads as a provider that lost half its catalogue for
// an hour.
//
// A day's rows go out in ONE write call on a file opened O_APPEND. Two embers
// can append at the same time -- an hourly timer and an operator running it
// by hand is the ordinary way -- and a write per line can interleave, which
// produces a line that is the first half of one sample and the second half of
// another. That line is unparseable, and it is unparseable in the one file in
// this program that cannot be regenerated.
func (s *Store) Append(samples []Sample) ([]string, error) {
	if len(samples) == 0 {
		// Not an error, and not a file either. A provider with an empty
		// catalogue is a thing worth saying out loud by the caller rather
		// than recording as a day with no rows.
		return nil, nil
	}
	byDay := map[string][]Sample{}
	for i, smp := range samples {
		if err := smp.Validate(); err != nil {
			return nil, fmt.Errorf("sample %d of %d is not recordable, so none of them were written: %w", i+1, len(samples), err)
		}
		byDay[smp.Day()] = append(byDay[smp.Day()], smp)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil { //nolint:gosec // 0700 is the tightest usable mode
		return nil, fmt.Errorf("create %s: %w", s.dir, err)
	}

	// Sorted so a batch that straddles midnight writes its files in a
	// deterministic order, which is what makes the command's own output
	// comparable between runs.
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)

	written := make([]string, 0, len(days))
	for _, d := range days {
		var buf bytes.Buffer
		for _, smp := range byDay[d] {
			raw, err := json.Marshal(smp)
			if err != nil {
				// Unreachable with the types above, and still an error
				// rather than a panic: it would mean a field was added that
				// cannot be encoded, and the honest answer is that the
				// snapshot was not recorded.
				return written, fmt.Errorf("encode sample for %q: %w", smp.ID, err)
			}
			buf.Write(raw)
			buf.WriteByte('\n')
		}
		path := filepath.Join(s.dir, d+ext)
		if err := appendOnce(path, buf.Bytes()); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	return written, nil
}

// appendOnce opens, writes once, syncs and closes.
//
// O_APPEND and never O_TRUNC: the open flags are the whole guarantee, and a
// truncating open here would silently replace a day of history with one
// snapshot of it.
//
// The fsync is deliberate and cheap. After this the process exits and the
// machine idles until the next hour, so a sample still in the page cache when
// the power goes is an hour of the series lost -- and the series is the only
// thing in ember that cannot be recollected. One fsync an hour buys that.
func appendOnce(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // ember's own state dir
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return fmt.Errorf("append to %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return f.Close()
}

// Days lists the day files already in the series, oldest first.
//
// A directory listing and not a read: the collector asks only whether
// anything has ever been collected, and reading a year of samples to answer
// that would make every hourly snapshot more expensive as the series grew.
func (s *Store) Days() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := parseDayFile(e.Name()); !ok {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ext))
	}
	sort.Strings(out)
	return out, nil
}

// Series is what a read of the directory found.
type Series struct {
	Samples []Sample

	// Days are the day files read, in order, so a reader can see a gap --
	// an hour with no collector running does not show up in a total.
	Days []string

	// Malformed counts lines that could not be placed in the series, and
	// Reason names the first one. Counted and reported rather than fatal: a
	// truncated last line is what a crash mid-append leaves, and refusing to
	// read a year of history over one broken line is the loss this format
	// exists to prevent.
	Malformed int
	Reason    string

	// Ignored names files in the directory that are not day files. Reported
	// rather than skipped silently, because a day somebody gzipped is a
	// shorter history than the reader thinks it has.
	Ignored []string
}

// Span is the first and last observation in the series.
func (s Series) Span() (first, last time.Time) {
	for _, smp := range s.Samples {
		if first.IsZero() || smp.ObservedAt.Before(first) {
			first = smp.ObservedAt
		}
		if smp.ObservedAt.After(last) {
			last = smp.ObservedAt
		}
	}
	return first, last
}

// Read returns every sample observed at or after since and before until.
//
// Half-open, as `ember spend`'s period is and for the same reason:
// consecutive periods tile without a sample landing in both. A zero bound is
// no bound.
func (s *Store) Read(since, until time.Time) (Series, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing has been collected yet. An empty series and not an
			// error: the reader's own output says the directory is empty and
			// how to start filling it, which is more use than a stat failure.
			return Series{}, nil
		}
		return Series{}, fmt.Errorf("read %s: %w", s.dir, err)
	}

	var out Series
	for _, e := range entries {
		if e.IsDir() {
			out.Ignored = append(out.Ignored, e.Name())
			continue
		}
		day, ok := parseDayFile(e.Name())
		if !ok {
			out.Ignored = append(out.Ignored, e.Name())
			continue
		}
		// A day file is skipped only when the whole day is outside the
		// period. The day is a UTC date and the bounds are instants, so the
		// per-sample check below is what actually decides -- this only keeps
		// a year of files from being read to answer a question about
		// yesterday.
		if !since.IsZero() && !day.AddDate(0, 0, 1).After(since) {
			continue
		}
		if !until.IsZero() && !day.Before(until) {
			continue
		}
		if err := s.readDay(&out, e.Name(), since, until); err != nil {
			return out, err
		}
	}
	sort.Strings(out.Days)
	// Oldest first, so the report reads in the order the samples were taken
	// and a price range is built in time order.
	sort.SliceStable(out.Samples, func(i, j int) bool {
		return out.Samples[i].ObservedAt.Before(out.Samples[j].ObservedAt)
	})
	return out, nil
}

func (s *Store) readDay(out *Series, name string, since, until time.Time) error {
	path := filepath.Join(s.dir, name)
	raw, err := os.ReadFile(path) //nolint:gosec // ember's own state dir
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	out.Days = append(out.Days, strings.TrimSuffix(name, ext))

	for i, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			// The trailing newline of the last record, every time.
			continue
		}
		var smp Sample
		if err := json.Unmarshal([]byte(line), &smp); err != nil {
			out.note(name, i+1, err.Error())
			continue
		}
		if err := smp.Validate(); err != nil {
			// A line that decodes and still cannot be placed: hand-edited,
			// or written by something that is not this package. Counted with
			// the unparseable ones, because the reader can do no more with
			// it either way.
			out.note(name, i+1, err.Error())
			continue
		}
		if !since.IsZero() && smp.ObservedAt.Before(since) {
			continue
		}
		if !until.IsZero() && !smp.ObservedAt.Before(until) {
			continue
		}
		out.Samples = append(out.Samples, smp)
	}
	return nil
}

func (s *Series) note(file string, line int, why string) {
	s.Malformed++
	if s.Reason == "" {
		s.Reason = fmt.Sprintf("%s line %d: %s", file, line, why)
	}
}

// parseDayFile reads a day out of a file name, or says it is not one.
//
// Parsed through time.Parse rather than matched against a shape: "2026-13-45"
// has the shape of a date and is not one, and a reader that accepted it would
// then compare it against a period as a zero time -- which includes it in
// every query.
func parseDayFile(name string) (time.Time, bool) {
	stem, ok := strings.CutSuffix(name, ext)
	if !ok {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation(dayLayout, stem, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}
