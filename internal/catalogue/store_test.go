package catalogue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

func at(day, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC)
}

func sample(t time.Time, id, dc string, level Level) Sample {
	return Sample{
		ObservedAt: t, Provider: "runpod", Kind: KindGPU, ID: id,
		Datacentre: dc, Availability: level, Overall: level,
		Secure: Tier{Offered: true, Price: node.RateFromUSD(0.89), MaxCount: 8},
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), DirName))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpen_RefusesAnEmptyDirectory(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Fatal("an empty directory was accepted")
	}
}

// TestAppend_IsAppendOnly is the property the whole format exists for. A
// second snapshot in the same day must add to the file, never replace it: the
// value is entirely in the series, and an open with O_TRUNC would silently
// swap a day of history for one sample of it.
func TestAppend_IsAppendOnly(t *testing.T) {
	s := open(t)
	if _, err := s.Append([]Sample{sample(at(2, 10), "A", "EU-RO-1", LevelHigh)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append([]Sample{sample(at(2, 11), "A", "EU-RO-1", LevelLow)}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Read(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != 2 {
		t.Fatalf("read %d sample(s) after two appends, want 2", len(got.Samples))
	}
	if got.Samples[0].Availability != LevelHigh || got.Samples[1].Availability != LevelLow {
		t.Errorf("samples came back as %q then %q, want HIGH then LOW in observation order",
			got.Samples[0].Availability, got.Samples[1].Availability)
	}
	if len(got.Days) != 1 {
		t.Errorf("two samples in one day wrote %d day file(s)", len(got.Days))
	}
}

// TestAppend_OneFilePerDay, by the UTC date of the observation and not by
// when the file was opened.
func TestAppend_OneFilePerDay(t *testing.T) {
	s := open(t)
	files, err := s.Append([]Sample{
		sample(at(3, 1), "A", "EU-RO-1", LevelHigh),
		sample(at(2, 23), "A", "EU-RO-1", LevelHigh),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("a batch across midnight wrote %d file(s): %v", len(files), files)
	}
	// Sorted, so the command's own output is comparable between runs.
	if filepath.Base(files[0]) != "2026-11-02.jsonl" || filepath.Base(files[1]) != "2026-11-03.jsonl" {
		t.Errorf("files = %v, want the two day names in order", files)
	}
}

// TestAppend_RefusesTheWholeBatchOverOneBadRow. Half a snapshot is worse than
// none: the file is never rewritten, so a row that cannot be placed in the
// series is there for good, and an hour missing half its rows reads as a
// provider that lost half its catalogue.
func TestAppend_RefusesTheWholeBatchOverOneBadRow(t *testing.T) {
	s := open(t)
	good := sample(at(2, 10), "A", "EU-RO-1", LevelHigh)
	bad := good
	bad.ObservedAt = time.Time{}

	files, err := s.Append([]Sample{good, bad})
	if err == nil {
		t.Fatal("a batch with an unplaceable row was written")
	}
	if len(files) != 0 {
		t.Errorf("files = %v, want none", files)
	}
	if !strings.Contains(err.Error(), "none of them were written") {
		t.Errorf("the error should say the whole batch was refused, got %v", err)
	}
	if _, statErr := os.Stat(s.Path(at(2, 10))); !os.IsNotExist(statErr) {
		t.Error("the day file exists, so the good row was written before the bad one was checked")
	}
}

func TestAppend_NothingIsNotAFile(t *testing.T) {
	s := open(t)
	files, err := s.Append(nil)
	if err != nil || files != nil {
		t.Fatalf("an empty batch returned %v, %v", files, err)
	}
	if _, err := os.Stat(s.Dir()); !os.IsNotExist(err) {
		t.Error("an empty batch created the directory")
	}
}

// TestAppend_WritesOneJSONObjectPerLine. The format is the contract: a line
// is a sample, so a reader that is not ember -- jq, a spreadsheet, a year
// from now -- can read it without this package.
func TestAppend_WritesOneJSONObjectPerLine(t *testing.T) {
	s := open(t)
	in := sample(at(2, 10), "NVIDIA GeForce RTX 4090", "US-IL-1", LevelLow)
	in.Overall = LevelNone
	if _, err := s.Append([]Sample{in}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(s.Path(at(2, 10)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("one sample wrote %d line(s)", len(lines))
	}
	var back Sample
	if err := json.Unmarshal([]byte(lines[0]), &back); err != nil {
		t.Fatalf("the line does not decode: %v", err)
	}
	if !back.ObservedAt.Equal(in.ObservedAt) || back.Availability != LevelLow || back.Overall != LevelNone {
		t.Errorf("round trip lost something: %+v", back)
	}
	// Money is integer microdollars on the wire too. A float here would be
	// 0.8899999999999999 the day somebody computes with it.
	if !strings.Contains(lines[0], `"price_micro_usd_per_hour":890000`) {
		t.Errorf("the price is not exact microdollars in the file: %s", lines[0])
	}
}

func TestRead_PeriodIsHalfOpen(t *testing.T) {
	s := open(t)
	if _, err := s.Append([]Sample{
		sample(at(1, 10), "A", "EU-RO-1", LevelHigh),
		sample(at(2, 10), "A", "EU-RO-1", LevelHigh),
		sample(at(3, 10), "A", "EU-RO-1", LevelHigh),
	}); err != nil {
		t.Fatal(err)
	}

	// --since includes and --until excludes, so consecutive periods tile
	// without a sample landing in both.
	got, err := s.Read(at(2, 0), at(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != 1 {
		t.Fatalf("read %d sample(s) for one day, want 1", len(got.Samples))
	}
	if !got.Samples[0].ObservedAt.Equal(at(2, 10)) {
		t.Errorf("read the sample from %s", got.Samples[0].ObservedAt)
	}

	// And the bound is the instant, not the day file: a sample earlier the
	// same day is outside a period that starts at noon.
	got, err = s.Read(at(2, 12), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != 1 || !got.Samples[0].ObservedAt.Equal(at(3, 10)) {
		t.Errorf("a mid-day bound read %d sample(s): %+v", len(got.Samples), got.Samples)
	}
}

func TestRead_AnEmptyDirectoryIsNotAnError(t *testing.T) {
	s := open(t)
	got, err := s.Read(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("reading before anything was collected failed: %v", err)
	}
	if len(got.Samples) != 0 || got.Malformed != 0 {
		t.Errorf("an empty series is not empty: %+v", got)
	}
}

// TestRead_ATruncatedLineIsCountedAndTheRestIsKept. A crash during an append
// leaves a partial last line. Refusing to read a year of history over it is
// the loss this format exists to prevent -- and losing it silently is worse,
// so it is counted and named.
func TestRead_ATruncatedLineIsCountedAndTheRestIsKept(t *testing.T) {
	s := open(t)
	if _, err := s.Append([]Sample{
		sample(at(2, 10), "A", "EU-RO-1", LevelHigh),
		sample(at(2, 11), "A", "EU-RO-1", LevelLow),
	}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.Path(at(2, 10)), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"observed_at":"2026-11-02T12:00`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := s.Read(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("a truncated line made the whole series unreadable: %v", err)
	}
	if len(got.Samples) != 2 {
		t.Errorf("kept %d of 2 good samples", len(got.Samples))
	}
	if got.Malformed != 1 {
		t.Errorf("malformed = %d, want 1", got.Malformed)
	}
	if !strings.Contains(got.Reason, "2026-11-02.jsonl line 3") {
		t.Errorf("the reason should name the file and line, got %q", got.Reason)
	}
}

// TestRead_ALineThatDecodesAndCannotBePlacedIsAlsoCounted. A hand-edited file
// or a writer that is not this package: it decodes, it has no timestamp, and
// no period query could ever find it.
func TestRead_ALineThatDecodesAndCannotBePlacedIsAlsoCounted(t *testing.T) {
	s := open(t)
	if _, err := s.Append([]Sample{sample(at(2, 10), "A", "EU-RO-1", LevelHigh)}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.Path(at(2, 10)), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"id\":\"A\",\"kind\":\"gpu\",\"provider\":\"runpod\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := s.Read(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != 1 || got.Malformed != 1 {
		t.Errorf("read %d sample(s) and %d malformed, want 1 and 1", len(got.Samples), got.Malformed)
	}
	if !strings.Contains(got.Reason, "observed_at") {
		t.Errorf("the reason should say what is wrong with it, got %q", got.Reason)
	}
}

// TestRead_SomethingThatIsNotADayFileIsNamed. A day somebody gzipped or
// renamed is a shorter history than the report thinks it has, and nothing
// else would ever say the data existed.
func TestRead_SomethingThatIsNotADayFileIsNamed(t *testing.T) {
	s := open(t)
	if _, err := s.Append([]Sample{sample(at(2, 10), "A", "EU-RO-1", LevelHigh)}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"2026-11-01.jsonl.gz", "notes.txt", "2026-13-45.jsonl"} {
		if err := os.WriteFile(filepath.Join(s.Dir(), name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "old"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := s.Read(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != 1 {
		t.Errorf("read %d sample(s)", len(got.Samples))
	}
	if len(got.Ignored) != 4 {
		t.Errorf("ignored = %v, want all four non-day entries named", got.Ignored)
	}
	// "2026-13-45" has the shape of a date and is not one. Parsed through
	// time.Parse rather than matched against a shape, because a reader that
	// accepted it would compare a zero time against every period and include
	// the file in all of them.
	for _, want := range []string{"2026-13-45.jsonl", "old"} {
		if !strings.Contains(strings.Join(got.Ignored, " "), want) {
			t.Errorf("%q is missing from %v", want, got.Ignored)
		}
	}
}

func TestDays_ListsWhatHasBeenCollected(t *testing.T) {
	s := open(t)
	days, err := s.Days()
	if err != nil {
		t.Fatalf("Days on a directory that does not exist failed: %v", err)
	}
	if len(days) != 0 {
		t.Errorf("days = %v before anything was collected", days)
	}
	if _, err := s.Append([]Sample{
		sample(at(3, 10), "A", "EU-RO-1", LevelHigh),
		sample(at(1, 10), "A", "EU-RO-1", LevelHigh),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "README"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	days, err = s.Days()
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0] != "2026-11-01" || days[1] != "2026-11-03" {
		t.Errorf("days = %v, want the two day names oldest first", days)
	}
}

func TestSeries_SpanIsTheFirstAndLastObservation(t *testing.T) {
	s := Series{Samples: []Sample{
		{ObservedAt: at(2, 10)}, {ObservedAt: at(1, 9)}, {ObservedAt: at(3, 23)},
	}}
	first, last := s.Span()
	if !first.Equal(at(1, 9)) || !last.Equal(at(3, 23)) {
		t.Errorf("span = %s..%s", first, last)
	}
	if f, l := (Series{}).Span(); !f.IsZero() || !l.IsZero() {
		t.Errorf("an empty series has a span: %s..%s", f, l)
	}
}

// TestAppendAndRead_ADayFileThatIsNotAFile. Not a contrivance: the state
// directory is on the operator's disk and ember is not the only thing that can
// write there. What matters is that the failure names the path rather than
// arriving as a nil-pointer two layers up, because the sample it was carrying
// is gone either way.
func TestAppendAndRead_ADayFileThatIsNotAFile(t *testing.T) {
	s := open(t)
	if err := os.MkdirAll(s.Path(at(2, 10)), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := s.Append([]Sample{sample(at(2, 10), "A", "EU-RO-1", LevelHigh)})
	if err == nil {
		t.Fatal("appending to a directory succeeded")
	}
	if !strings.Contains(err.Error(), "2026-11-02.jsonl") {
		t.Errorf("the error should name the file, got %v", err)
	}
	// And the reader does not try to read it. A directory is not a day file
	// however it is named, so it is named in Ignored -- which is what makes
	// the report say the series is shorter than the directory looks, rather
	// than failing on it or passing over it in silence.
	got, err := s.Read(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("a directory named like a day file made the series unreadable: %v", err)
	}
	if len(got.Ignored) != 1 || got.Ignored[0] != "2026-11-02.jsonl" {
		t.Errorf("ignored = %v, want the directory named", got.Ignored)
	}
}

func TestRead_AnUntilBoundSkipsLaterDayFiles(t *testing.T) {
	s := open(t)
	if _, err := s.Append([]Sample{
		sample(at(1, 10), "A", "EU-RO-1", LevelHigh),
		sample(at(5, 10), "A", "EU-RO-1", LevelHigh),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(time.Time{}, at(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != 1 || len(got.Days) != 1 {
		t.Errorf("read %d sample(s) from %v, want only the first day", len(got.Samples), got.Days)
	}
}

func TestRead_AnUnreadableDirectoryIsAnError(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(time.Time{}, time.Time{}); err == nil {
		t.Fatal("reading a file as a directory succeeded")
	}
	if _, err := s.Days(); err == nil {
		t.Fatal("listing a file as a directory succeeded")
	}
	if _, err := s.Append([]Sample{sample(at(2, 10), "A", "EU-RO-1", LevelHigh)}); err == nil {
		t.Fatal("appending into a file succeeded")
	}
}
