package app

import (
	"context"
	"errors"
	"io"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/tunnel"
)

// batch is a workload KIND driven over one rental, and it exists because there
// are two of them.
//
// WHY THIS IS THE SEAM AND NOT A WIDER ONE. #190 asks for a workload-kind
// interface where "each kind supplies: how to start its server, how to submit
// a job, how to read progress, what parameters are typed, and what a result
// is" -- five things. Four of those are already somewhere a kind owns them:
// the typed parameters are its own config block and its own job type, starting
// a server is `node.Spec.Serve` plus the kind's own argv builder, submitting
// and reading progress are the kind's package, and what a result is reaches
// everybody as a `report.Event`. What was NOT anywhere was the dispatch: the
// one line in Plan.serve that chose between them, which read
// `if len(p.Spec.Serve) > 0` and was therefore a question about sd-server's
// argv rather than about which kind this run is.
//
// So this interface is one method, and the five things stay where the kind
// already keeps them. A wider interface -- readiness, submit, poll, result --
// would be designed from sd-server's shape and then have to be satisfied by a
// program with no server at all, which is the driver-interface-from-one-example
// mistake AGENTS.md records, committed while appearing to avoid it. The honest
// test of an interface is that a second implementation exists, and the second
// one here is the one that proves the WIDE interface would have been wrong.
//
// A func type was the alternative and is refused for one reason: Describe. A
// refusal that cannot name which kind it is about is a refusal an operator has
// to work out, and `ember up --dry-run` prints the kind before anything is
// rented.
type batch interface {
	// Run drives this kind's whole batch over one rental and emits its own
	// progress into sink.
	//
	// Every parameter is one Plan.serve already has in hand. It takes the
	// seams (a tunnel.Dialer and an sshx.Runner) rather than the SSH client,
	// so an implementation is testable against the recording pair instead of
	// needing a node -- which is the discipline runWorkload and fetchOutputs
	// already follow.
	//
	// runDir is this run's output directory, decided once by the caller: a
	// batch fetches several times and every artifact of one run has to land
	// in one place.
	Run(ctx context.Context, out io.Writer, sink report.Sink, client tunnel.Dialer,
		r sshx.Runner, spec node.Spec, so session.Options, runDir string, rate node.Rate) error

	// Describe names the kind, for a refusal and for `--dry-run`.
	Describe() string
}

// batchOf is the dispatch, and it is one function rather than a chain of ifs
// in Plan.serve.
//
// It returns nil for the two shapes that are not a typed batch -- a `cmds` run
// and an interactive one -- because those are not kinds in this sense: `cmds`
// is opaque argv by design and an interactive session has no jobs at all.
// AGENTS.md's position that "`cmds` stays as the general mechanism" is
// unchanged by any of this; what changed is that it stopped being the only
// answer for anything that is not stable diffusion.
//
// BOTH AT ONCE IS REFUSED AT NewPlan AND NOT HERE, so that a config naming two
// kinds fails before a card is chosen rather than having one of them silently
// win a switch. This function therefore has no "both" arm, and the ordering
// below is not a precedence.
func (p Plan) batchOf() batch {
	if len(p.Spoken.Jobs) > 0 {
		return spokenBatch{run: p.Spoken}
	}
	if len(p.Jobs.Jobs) > 0 || len(p.Spec.Serve) > 0 {
		return servedBatch{jobs: p.Jobs}
	}
	return nil
}

// servedBatch is stable diffusion through a resident sd-server.
//
// A four-line adapter and not a move of runServed's body: that function is
// 300 lines with a server process, a forward, a poll loop and a cancel
// channel in it, and rewriting it to fit an interface in the same change that
// introduces the interface would mean the seam and its first implementation
// were verified by the same tests at once.
type servedBatch struct{ jobs sdserver.Run }

func (b servedBatch) Describe() string { return "sd (a resident sd-server)" }

func (b servedBatch) Run(ctx context.Context, out io.Writer, sink report.Sink, client tunnel.Dialer,
	r sshx.Runner, spec node.Spec, so session.Options, runDir string, rate node.Rate,
) error {
	if len(spec.Serve) == 0 {
		// Reachable from a config that names sd jobs and no serve command,
		// which NewPlan refuses -- so this is the belt to that braces and
		// says which key is missing rather than starting nothing.
		return errors.New("sd.jobs names jobs and sd.serve is empty: there is no server to submit them to")
	}
	return runServed(ctx, out, sink, client, r, spec, b.jobs, so, runDir, rate)
}
