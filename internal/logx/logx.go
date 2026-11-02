// Package logx is the one place ember decides how it logs.
//
// WHY IT IS A PACKAGE AND NOT TWO LITERALS. The handler was constructed
// twice, independently, with the same literal -- in internal/app's
// BuildOptions and in internal/cli's runServe -- which is AGENTS.md's "one
// name for a path, not one per call site" reached through a log line. The
// consequence was not cosmetic: the level was `slog.LevelInfo` in both
// copies, so the seventeen `.Debug(` calls in this tree could not be
// displayed by any invocation of ember that existed. They were not dormant,
// they were dead -- the same "built end to end and never connected" shape as
// the preview frame that was written to disk and never recorded, one layer
// down. Four of them answer the questions a rented machine is debugged with:
// why a node was running but not reachable, why sd-server exited, how long an
// asset took, what the record said a run cost.
//
// WHAT IT IS NOT. It is not a logging framework and it must not become one.
// There is no exporter here, no egress and no new dependency: slog's own two
// handlers already emit logfmt and JSON, which is what a log store ingests.
// It is also NOT report.Event, and the distinction is load-bearing --
// report.Event is what ember *did* (a node was rented, a job cost this), it
// is numbered, persisted to genstore and read back by the UI; a log line is
// how ember *behaved* (a retry, a fallback, a timing that surprised it) and
// is disposable. They correlate through the keys below and nothing routes one
// into the other, because a generation history that is also a log sink is a
// history nothing can bound.
//
// IT IMPORTS ONLY THE STANDARD LIBRARY, deliberately. cmd/embernode -- the
// program that runs on the rented machine -- imports this package and nothing
// else from the module, so the cost of giving the node ember's log format is
// exactly one intra-module dependency, and that dependency can never grow a
// provider client, a Docker client or an SSH stack behind the node binary's
// back.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
)

// Format is how a record is written.
//
// Two, and no more. TextHandler's output is logfmt, which Loki, Vector and
// `grep` all read; JSONHandler is what everything else reads. A third format
// would be a format nobody asked for in a decision that has to be made once.
type Format string

const (
	// FormatText is slog's TextHandler: `time=... level=INFO msg=...`. It is
	// the default because it is what a person reads in a terminal, and
	// `ember up`'s output is asserted by a lab at this spelling.
	FormatText Format = "text"

	// FormatJSON is slog's JSONHandler, for a machine.
	FormatJSON Format = "json"
)

// Environment variables, so a long-running `ember serve` under a unit file can
// be turned up without editing the unit.
//
// THE LEVEL AND THE FORMAT ONLY. Nothing here reads a secret from the
// environment and nothing here accepts one on a flag: /proc/<pid>/cmdline is
// world-readable, which is why AGENTS.md bans a credential in argv, and a
// logging knob is not a reason to make the first exception.
const (
	EnvLevel  = "EMBER_LOG_LEVEL"
	EnvFormat = "EMBER_LOG_FORMAT"
)

// NodeLogPath is where cmd/embernode writes on the rented machine, and ONE
// NAME FOR IT RATHER THAN ONE PER CALL SITE.
//
// Two programs need it: embernode writes the file and internal/app fetches it
// before the node is destroyed. AGENTS.md records what the second literal
// costs -- the journal's "nodes" subdirectory was a literal in the CLI and a
// second literal in the lab harness, they drifted, the harness read the wrong
// directory, and a lab asserting "a clean run leaves no records" passed
// without checking anything. A log fetched from the wrong path fails the same
// way and is quieter still, because that fetch must never fail a run and so
// can only be swallowed.
//
// It lives in this package rather than in internal/node because it is not
// part of a node's shape -- nothing about a Spec or a CreateRequest mentions
// it -- and because a constant in a package neither program already imported
// would be a third import to justify.
//
// /tmp AND NOT THE OUTPUT DIRECTORY, which is internal/app's fetchMarker
// precedent read the other way round: a file under the outputs would be
// swept into the run's own archive, arrive half-written in the middle of a
// batch, and be indistinguishable from a result.
const NodeLogPath = "/tmp/embernode.log"

// Correlation keys, named once.
//
// WHY THIS IS A CONVENTION WITH CONSTANTS RATHER THAN A HABIT. Of the log
// lines in this tree, seven carried a run or request id and the rest carried
// nothing that could be joined to the rental they were about -- so
// "node running but not yet reachable" named no node, in a program whose
// whole subject is that a node costs money by the hour. A key spelled `run`
// in one package and `runID` in another is the same defect as two literals
// for one path: the filter that finds one misses the other, and the line that
// is missing is the one being looked for.
//
// They are attached ONCE, with WithRun and WithRequest, to a logger that is
// then handed down -- not repeated at every call site, which is the form that
// can be forgotten. provision.WithNode attaches the run id for the whole
// lifetime of a rental, and every package that takes a *slog.Logger from it
// inherits the key without knowing it exists.
const (
	// KeyRun is the node.RunID a line belongs to: one rental.
	KeyRun = "run"

	// KeyRequest is the genstore.ReqID a line belongs to, where there is one.
	// A rental can serve several requests, so this is narrower than the run.
	KeyRequest = "request"
)

// Options are the two decisions. The zero value is info-level text, which is
// what ember printed before any of this existed.
type Options struct {
	Level  slog.Level
	Format Format
}

// New builds the handler. IT IS THE ONLY PLACE IN THIS MODULE THAT DOES.
//
// internal/logx/seal_test.go walks the tree and fails the build when
// slog.NewTextHandler, slog.NewJSONHandler or a slog.HandlerOptions literal
// appears in any other non-test file -- the shape TestNoListenOutsideLoopback
// uses, and for the same reason: every decision made here is worth nothing if
// a second call site decides it differently because that was two lines
// shorter. `slog.New(slog.DiscardHandler)` is deliberately still allowed
// everywhere, because that is the fallback the seven library packages use to
// stay silent when no logger was handed to them, and it decides neither a
// level nor a format.
//
// No ReplaceAttr and no AddSource: text output at info level has to stay
// byte-for-byte what the two replaced literals produced, because
// test/integration/lifecycle_test.go greps `ember up`'s output for the slog
// spelling of "node ready".
func New(w io.Writer, o Options) *slog.Logger {
	opts := &slog.HandlerOptions{Level: o.Level}
	if o.Format == FormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// WithRun attaches the rental a logger's lines belong to.
//
// A no-op for an empty id rather than a line reading `run=""`, which is worse
// than no key at all: a filter for the runs that have no id would match it,
// and an operator reading one would conclude the key is unreliable. A nil
// logger is returned as nil so a caller holding an optional logger does not
// have to branch before attaching the key.
func WithRun(l *slog.Logger, run string) *slog.Logger {
	if l == nil || run == "" {
		return l
	}
	return l.With(KeyRun, run)
}

// WithRequest attaches the request a logger's lines belong to.
func WithRequest(l *slog.Logger, request string) *slog.Logger {
	if l == nil || request == "" {
		return l
	}
	return l.With(KeyRequest, request)
}

// levels is the allow-list, and it is a list rather than slog's own parser.
//
// (*slog.Level).UnmarshalText also accepts an offset -- "INFO+2", "WARN-1" --
// which this program has no use for and which prints in a record as a level
// name nobody can look up. Its refusal is also `slog: level string "x":
// unknown name`, which names neither the four that work nor the flag that was
// wrong, and an error that refuses without saying how to proceed is how an
// operator concludes the tool is broken.
var levels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// ParseLevel reads a level name, case-insensitively.
func ParseLevel(s string) (slog.Level, error) {
	if l, ok := levels[strings.ToLower(strings.TrimSpace(s))]; ok {
		return l, nil
	}
	return 0, fmt.Errorf("%q is not a log level; use one of %s", s, names())
}

// ParseFormat reads a format name, case-insensitively.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	}
	return "", fmt.Errorf("%q is not a log format; use %q (logfmt, what a terminal reads) or %q",
		s, FormatText, FormatJSON)
}

// names lists the levels for an error message, in severity order.
//
// Sorted by the level's own value and not by name, because "debug, error,
// info, warn" is alphabetical and reads as though error sits between debug
// and info -- an ordering an operator would act on.
func names() string {
	ns := make([]string, 0, len(levels))
	for n := range levels {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool { return levels[ns[i]] < levels[ns[j]] })
	return strings.Join(ns, ", ")
}

// Resolve decides the two values from a flag and the environment.
//
// THE FLAG WINS, and an unset flag is the empty string rather than a default
// baked in here -- because "the operator typed it" and "the operator's shell
// has it set" have to be told apart, and a flag defaulting to "info" would
// silently override EMBER_LOG_LEVEL on every invocation that did not pass one.
//
// A bad value is refused rather than ignored, including from the environment.
// A stale EMBER_LOG_LEVEL=verbose that silently fell back to info would mean
// an operator turning up the logs, seeing no change, and concluding that
// debug lines do not exist -- which is exactly the state this package was
// written to end.
func Resolve(levelFlag, formatFlag string) (Options, error) {
	o := Options{Level: slog.LevelInfo, Format: FormatText}

	level, from := levelFlag, "--log-level"
	if level == "" {
		level, from = strings.TrimSpace(os.Getenv(EnvLevel)), EnvLevel
	}
	if level != "" {
		l, err := ParseLevel(level)
		if err != nil {
			return Options{}, fmt.Errorf("%s: %w", from, err)
		}
		o.Level = l
	}

	format, from := formatFlag, "--log-format"
	if format == "" {
		format, from = strings.TrimSpace(os.Getenv(EnvFormat)), EnvFormat
	}
	if format != "" {
		f, err := ParseFormat(format)
		if err != nil {
			return Options{}, fmt.Errorf("%s: %w", from, err)
		}
		o.Format = f
	}
	return o, nil
}
