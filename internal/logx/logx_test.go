package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestNew_TheZeroOptionsAreWhatEmberAlreadyPrinted.
//
// The point of this package is that one place decides the level and the
// format, and the thing that must NOT change while that is arranged is the
// output: test/integration/lifecycle_test.go greps `ember up`'s output for the
// slog spelling of "node ready", and AGENTS.md records that printing a
// lifecycle event twice "would change `ember up`'s output, and a lab asserts
// it has not". So the zero value has to be exactly the two literals this
// package replaced -- an info-level TextHandler -- and the comparison is
// against a handler built the old way rather than against a string somebody
// typed, because a string typed here would be the same number the bug
// produces.
func TestNew_TheZeroOptionsAreWhatEmberAlreadyPrinted(t *testing.T) {
	var got, want bytes.Buffer

	New(&got, Options{}).Info("node ready", "handle", "test/n1")
	// The literal this package replaced, kept here as the oracle rather than
	// as a string somebody typed. internal/logx/seal_test.go permits a
	// handler in a _test.go file for exactly this.
	slog.New(slog.NewTextHandler(&want, &slog.HandlerOptions{Level: slog.LevelInfo})).
		Info("node ready", "handle", "test/n1")

	// The timestamp differs by microseconds, so the comparison is on
	// everything after it.
	trim := func(s string) string {
		if i := strings.Index(s, " level="); i >= 0 {
			return s[i:]
		}
		return s
	}
	if trim(got.String()) != trim(want.String()) {
		t.Errorf("the default format changed:\n got %q\nwant %q", got.String(), want.String())
	}
}

// TestNew_DebugIsReachable is the defect this package was written for.
//
// Seventeen debug calls existed in this tree and the level was hard-coded to
// info at both construction sites, so not one of them could be displayed by
// any invocation of ember that shipped. They were not dormant; they were dead.
func TestNew_DebugIsReachable(t *testing.T) {
	var at, below bytes.Buffer
	New(&at, Options{Level: slog.LevelDebug}).Debug("node running but not yet reachable")
	New(&below, Options{}).Debug("node running but not yet reachable")

	if !strings.Contains(at.String(), "node running but not yet reachable") {
		t.Errorf("a debug line is invisible at debug level: %q", at.String())
	}
	if below.Len() != 0 {
		t.Errorf("a debug line printed at the default level: %q", below.String())
	}
}

// TestNew_JSONIsValidJSON, because the format exists for a machine and a
// format a machine cannot parse is worse than no format.
func TestNew_JSONIsValidJSON(t *testing.T) {
	var b bytes.Buffer
	New(&b, Options{Format: FormatJSON}).Info("node ready", KeyRun, "abc123")

	var rec map[string]any
	if err := json.Unmarshal(b.Bytes(), &rec); err != nil {
		t.Fatalf("--log-format json emitted something that is not JSON: %v\n%s", err, b.String())
	}
	if rec["msg"] != "node ready" || rec[KeyRun] != "abc123" {
		t.Errorf("the record lost its fields: %v", rec)
	}
}

// TestWith_AttachesTheCorrelationKeyOnce.
//
// The convention is "attached once to a logger that is handed down", so the
// property under test is that a derived logger carries the key on a line that
// never mentions it.
func TestWith_AttachesTheCorrelationKeyOnce(t *testing.T) {
	var b bytes.Buffer
	l := WithRequest(WithRun(New(&b, Options{}), "run-1"), "req-9")
	l.Info("a line that names neither key")

	out := b.String()
	for _, want := range []string{KeyRun + "=run-1", KeyRequest + "=req-9"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s on the line: %q", want, out)
		}
	}
}

// TestWith_AnEmptyIdIsNoKeyAtAll.
//
// `run=""` is worse than no key: a filter for the lines that carry no run
// matches it, so an operator searching for the unattributed lines would be
// shown the attributed ones. A nil logger stays nil so an optional logger
// needs no branch at the call site.
func TestWith_AnEmptyIdIsNoKeyAtAll(t *testing.T) {
	var b bytes.Buffer
	WithRun(New(&b, Options{}), "").Info("a line about no rental in particular")
	if strings.Contains(b.String(), KeyRun+"=") {
		t.Errorf("an empty id was attached anyway: %q", b.String())
	}
	if WithRun(nil, "run-1") != nil || WithRequest(nil, "req-1") != nil {
		t.Error("a nil logger came back non-nil, so a caller holding one would have to branch")
	}
}

// TestParseLevel_AndItsRefusal. The refusal has to name the four that work:
// an error that refuses without saying how to proceed is how an operator
// concludes the flag does not exist.
func TestParseLevel_AndItsRefusal(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo,
		" warn ": slog.LevelWarn, "Error": slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}

	// INFO+2 is slog's own spelling and is deliberately refused: it prints in
	// a record as a level name nobody can look up.
	for _, in := range []string{"", "verbose", "INFO+2", "trace"} {
		_, err := ParseLevel(in)
		if err == nil {
			t.Errorf("ParseLevel(%q) was accepted", in)
			continue
		}
		for _, want := range []string{"debug", "info", "warn", "error"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseLevel(%q) refused without naming %q: %v", in, want, err)
			}
		}
	}
	// Severity order, not alphabetical: "debug, error, info, warn" reads as
	// though error sits between debug and info, which is an ordering an
	// operator would act on.
	if got := names(); got != "debug, info, warn, error" {
		t.Errorf("names() = %q, want them in severity order", got)
	}
}

// TestParseFormat_AndItsRefusal.
func TestParseFormat_AndItsRefusal(t *testing.T) {
	for _, in := range []string{"text", "TEXT", " json "} {
		if _, err := ParseFormat(in); err != nil {
			t.Errorf("ParseFormat(%q): %v", in, err)
		}
	}
	for _, in := range []string{"", "logfmt", "yaml"} {
		if _, err := ParseFormat(in); err == nil {
			t.Errorf("ParseFormat(%q) was accepted", in)
		}
	}
}

// TestResolve_TheFlagWinsAndTheEnvironmentIsRefusedWhenWrong.
//
// A stale EMBER_LOG_LEVEL that silently fell back to info would mean an
// operator turning the logs up, seeing no change and concluding debug lines do
// not exist -- which is the state this package was written to end, reached
// through the other door.
func TestResolve_TheFlagWinsAndTheEnvironmentIsRefusedWhenWrong(t *testing.T) {
	t.Setenv(EnvLevel, "warn")
	t.Setenv(EnvFormat, "json")

	// Nothing passed: the environment decides.
	o, err := Resolve("", "")
	if err != nil || o.Level != slog.LevelWarn || o.Format != FormatJSON {
		t.Fatalf("Resolve from the environment = %+v, %v", o, err)
	}

	// A flag passed: it wins, and only for the half it names.
	o, err = Resolve("debug", "")
	if err != nil || o.Level != slog.LevelDebug || o.Format != FormatJSON {
		t.Fatalf("the flag did not win: %+v, %v", o, err)
	}

	// A bad environment value is an error that names the variable, because
	// the operator looking at a refused command needs to know it was not
	// their flag.
	t.Setenv(EnvLevel, "verbose")
	if _, err := Resolve("", ""); err == nil || !strings.Contains(err.Error(), EnvLevel) {
		t.Fatalf("a bad %s was not refused by name: %v", EnvLevel, err)
	}
	// And the flag's own refusal names the flag.
	if _, err := Resolve("verbose", ""); err == nil || !strings.Contains(err.Error(), "--log-level") {
		t.Fatalf("a bad flag was not refused by name: %v", err)
	}
	t.Setenv(EnvLevel, "")
	t.Setenv(EnvFormat, "sideways")
	if _, err := Resolve("", ""); err == nil || !strings.Contains(err.Error(), EnvFormat) {
		t.Fatalf("a bad %s was not refused by name: %v", EnvFormat, err)
	}

	// Nothing set anywhere is the default ember already had.
	t.Setenv(EnvFormat, "")
	o, err = Resolve("", "")
	if err != nil || o.Level != slog.LevelInfo || o.Format != FormatText {
		t.Fatalf("the default changed: %+v, %v", o, err)
	}
}

// TestNodeLogPath_IsAbsoluteAndNotUnderTheOutputs.
//
// internal/transfer refuses a relative output path, so a node log path that
// was not absolute would make the collection in internal/app fail on every
// run -- silently, because that fetch must never fail a run and is therefore
// swallowed. And it must not be under the outputs directory: a file there
// would be swept into the run's own archive, arrive half-written in the
// middle of a batch and be indistinguishable from a result, which is the
// reason internal/app's fetch marker lives in /tmp too.
func TestNodeLogPath_IsAbsoluteAndNotUnderTheOutputs(t *testing.T) {
	if !strings.HasPrefix(NodeLogPath, "/") {
		t.Errorf("NodeLogPath = %q, which internal/transfer refuses", NodeLogPath)
	}
	if strings.HasPrefix(NodeLogPath, "/workspace") {
		t.Errorf("NodeLogPath = %q, which is where outputs are written", NodeLogPath)
	}
}
