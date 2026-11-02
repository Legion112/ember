package cli

import (
	"flag"

	"github.com/Legion112/ember/internal/logx"
)

// logFlags are --log-level and --log-format, registered once and read once.
//
// WHY A HELPER AND NOT TWO fs.String CALLS PER SUBCOMMAND. This is the same
// decision internal/logx exists for, one layer up: two copies of a flag's
// name, its default and its help text are two copies that drift, and the one
// that drifts is the one nobody passed. Only the two subcommands that build a
// logger register them -- `ember up` and `ember serve` -- because a flag on
// `ember ls` that could not change anything is a flag that teaches an
// operator the knob does not work.
//
// THE DEFAULTS ARE EMPTY STRINGS, NOT "info" AND "text". logx.Resolve has to
// be able to tell "the operator typed it" from "the operator's shell has
// EMBER_LOG_LEVEL set", and a flag defaulting to info would silently override
// the environment on every invocation that did not pass one -- which is the
// shape of bug where a setting appears to be ignored for no visible reason.
type logFlags struct {
	level  *string
	format *string
}

// addLogFlags registers the pair on fs.
func addLogFlags(fs *flag.FlagSet) logFlags {
	return logFlags{
		level: fs.String("log-level", "",
			"debug, info, warn or error (default info, or "+logx.EnvLevel+")"),
		format: fs.String("log-format", "",
			"text or json (default text, or "+logx.EnvFormat+")"),
	}
}

// resolve turns the flags into the one pair of values, refusing a bad one.
//
// The refusal is a usage error and not a run error: a mistyped level is a
// mistyped command line, so exit 3 rather than exit 1 -- which is the
// contract `ember ls`'s exit 2 is part of, and the reason a bad flag must not
// read as "there are findings".
func (f logFlags) resolve() (logx.Options, error) {
	o, err := logx.Resolve(*f.level, *f.format)
	if err != nil {
		return logx.Options{}, usageError{err}
	}
	return o, nil
}
