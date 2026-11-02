package cli

import (
	"context"
	"flag"
	"io"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/reconcile"
)

func cmdLs(args []string, out io.Writer) error   { return runLs(args, out, deps{}) }
func cmdReap(args []string, out io.Writer) error { return runReap(args, out, deps{}) }

// runLs reports every node ember created, asking the providers rather than
// trusting the local records.
//
// It exits 2 when anything needs attention. That is the contract with cron:
// "there are orphaned nodes" and "ember itself broke" must not look the same
// to a timer that only sees a status code.
func runLs(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	fs.Usage = func() {
		outf(out, "ember ls lists every node ember created, across every configured provider,\nalong with the stores it owns and anything a destroyed node left behind.\n\n")
		outf(out, "It asks the providers rather than trusting the local records, so a node\nsurvives a lost state directory. Exit 2 means something needs attention.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	sc, err := app.Scan(context.Background(), *cfgPath, d)
	if err != nil {
		return err
	}
	findings, stores, leftovers := sc.Nodes, sc.Stores, sc.Leftovers

	if len(findings) == 0 {
		outf(out, "no nodes.\n")
	}
	for _, f := range findings {
		outf(out, "%s\n", f)
	}

	// Printed with the nodes rather than with the stores, because a leftover
	// is the wreckage of a node and not a standing charge. Nothing bills for
	// one, so there is no total to print -- what there is instead is a daemon
	// that stops being able to create networks, which is why these count
	// towards attention and a store does not.
	for _, f := range leftovers {
		outf(out, "%s\n", f)
	}

	// Printed after the nodes and on every run, including the run where
	// there are no nodes at all. A store with nothing attached is the one
	// charge that no deadline, no reaper and no Ctrl-C ever ends, so the
	// clean-bill-of-health case is exactly the one it must not be omitted
	// from.
	for _, f := range stores {
		outf(out, "%s\n", f)
	}
	if total := reconcile.MonthlyTotal(stores); total > 0 {
		outf(out, "storage: %s/month, which no deadline or reaper ends.\n", total)
	}

	all := append(append(append([]reconcile.Finding(nil), findings...), stores...), leftovers...)
	if n := reconcile.Attention(all); n > 0 {
		return &FindingsError{N: n}
	}
	return nil
}

// runReap destroys everything past its deadline.
func runReap(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	dryRun := fs.Bool("dry-run", false, "say what would be destroyed and destroy nothing")
	fs.Usage = func() {
		outf(out, "ember reap destroys every node past its deadline, and removes what a\ndestroyed node left behind.\n\n")
		outf(out, "Nodes with no local record are reported and left alone: one may belong to\nanother machine of yours, and destroying somebody's running work because\nthis one had not heard of it is not a trade to make automatically.\n\n")
		outf(out, "A leftover is an object a run created beside its node that outlived it --\na docker bridge network whose node container is gone. Nothing bills for\none, and nothing but this command will ever remove it; left to accumulate\nthey exhaust docker's address pool and every local run starts failing.\nOne inside its deadline is left alone, because a node that is still\nstarting up looks exactly the same.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	ctx := context.Background()
	sc, err := app.Scan(ctx, *cfgPath, d)
	if err != nil {
		return err
	}
	findings := sc.Nodes

	expired := 0
	for _, f := range findings {
		if f.Kind == reconcile.KindExpired {
			expired++
			outf(out, "%s\n", f)
		}
	}
	// Reported and reaped beside the expired nodes, in one command, because
	// this is the command whose job is cleaning up after runs that ended
	// badly. A separate `ember sweep` nobody runs would leave the leak
	// exactly where it was: invisible, free, and accumulating until Docker
	// has subnetted its whole address pool.
	stale := 0
	for _, f := range sc.Leftovers {
		// Counted in the same loop that prints, and only the leftovers. A
		// failed listing arrives in this slice as KindUnreachable -- "I could
		// not ask the daemon what ember left on it" -- and counting those
		// would have reap announce it was about to remove a provider it could
		// not even reach, then report "removed 0 of 1" and exit 2 with a
		// number about nothing.
		if f.Kind != reconcile.KindLeftover {
			continue
		}
		stale++
		outf(out, "%s\n", f)
	}
	if expired == 0 && stale == 0 {
		outf(out, "nothing to reap.\n")
		return nil
	}
	if *dryRun {
		outf(out, "dry-run: would destroy %d node(s) and remove %d leftover(s); nothing was touched\n", expired, stale)
		return nil
	}

	// Reaped against the registry and the journal the scan used, not a
	// second pair built from a second config read. Two reads are two chances
	// for the two copies to disagree about which provider a finding belongs
	// to, and the one that disagrees is the one that does not destroy the
	// node.
	destroyed, rerr := reconcile.Reap(ctx, sc.Registry, sc.Store, findings)
	outf(out, "destroyed %d of %d expired node(s)\n", destroyed, expired)

	// Attempted even when the node reap failed, and its own error kept
	// separately. The two are independent cleanups: a provider that could not
	// destroy a pod says nothing about whether this machine's stale bridge
	// networks can go, and skipping them because something else failed is how
	// a leak survives every run that had anything else wrong with it.
	removed, lerr := reconcile.ReapLeftovers(ctx, sc.Registry, sc.Leftovers)
	if stale > 0 {
		outf(out, "removed %d of %d leftover(s)\n", removed, stale)
	}

	if rerr != nil {
		return rerr
	}
	if lerr != nil {
		return lerr
	}
	if left := (expired - destroyed) + (stale - removed); left > 0 {
		return &FindingsError{N: left}
	}
	return nil
}
