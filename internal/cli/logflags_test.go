package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/logx"
)

// TestLogFlags_ABadLevelIsAUsageErrorBeforeAnythingIsRead.
//
// Exit 3 and not exit 1, because `ember ls`'s exit 2 is a documented contract
// with cron and CI: a mistyped flag that read as "there are findings" or as
// "ember itself broke" would send an operator looking for an orphaned node.
// It is checked on `up` and on `serve`, because those are the two subcommands
// that build a logger and therefore the two that register the flags.
func TestLogFlags_ABadLevelIsAUsageErrorBeforeAnythingIsRead(t *testing.T) {
	for _, tc := range []struct{ name, flag, value string }{
		{"level", "--log-level", "verbose"},
		{"format", "--log-format", "yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			// No --config and no image: if the refusal came after the config
			// was read this would fail with a different error, which is the
			// property being asserted as much as the exit code.
			err := runUp([]string{tc.flag, tc.value}, &out, labDeps(t))
			if err == nil {
				t.Fatalf("%s %s was accepted", tc.flag, tc.value)
			}
			if ExitCode(err) != ExitUsage {
				t.Errorf("exit = %d, want usage (%d): %v", ExitCode(err), ExitUsage, err)
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("the refusal does not name the flag that was wrong: %v", err)
			}
		})
	}

	var out bytes.Buffer
	if err := runServe([]string{"--log-level", "verbose"}, &out, labDeps(t)); ExitCode(err) != ExitUsage {
		t.Errorf("serve exit = %d, want usage: %v", ExitCode(err), err)
	}
}

// TestLogFlags_AreOnTheHelpOfBothCommandsThatLog.
//
// A flag an operator cannot discover is a flag they do not use, and the help
// is the only place `--log-level` is announced -- there is no `ember config`
// to print it.
func TestLogFlags_AreOnTheHelpOfBothCommandsThatLog(t *testing.T) {
	for name, run := range map[string]func([]string, *bytes.Buffer) error{
		"up":    func(a []string, b *bytes.Buffer) error { return runUp(a, b, labDeps(t)) },
		"serve": func(a []string, b *bytes.Buffer) error { return runServe(a, b, labDeps(t)) },
	} {
		var out bytes.Buffer
		_ = run([]string{"-h"}, &out)
		for _, want := range []string{"-log-level", "-log-format", logx.EnvLevel, logx.EnvFormat} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("`ember %s -h` does not mention %q:\n%s", name, want, out.String())
			}
		}
	}
}

// TestLogFlags_ResolveTurnsTheFlagsIntoTheOnePairOfValues.
//
// The resolution itself is internal/logx's, tested there against the
// environment; what is checked here is that this package's wrapper hands the
// values through unchanged rather than defaulting them a second time, which
// is the drift the whole of internal/logx exists to prevent.
func TestLogFlags_ResolveTurnsTheFlagsIntoTheOnePairOfValues(t *testing.T) {
	level, format := "debug", "json"
	o, err := logFlags{level: &level, format: &format}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if o.Level != slog.LevelDebug || o.Format != logx.FormatJSON {
		t.Errorf("resolve = %+v, want debug/json", o)
	}
}
