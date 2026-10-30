package cli

import (
	"context"
	"flag"
	"io"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/reconcile"
	"github.com/Legion112/ember/internal/statestore"
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
		outf(out, "ember ls lists every node ember created, across every configured provider.\n\n")
		outf(out, "It asks the providers rather than trusting the local records, so a node\nsurvives a lost state directory. Exit 2 means something needs attention.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	findings, stores, _, err := scan(*cfgPath, out, d)
	if err != nil {
		return err
	}

	if len(findings) == 0 {
		outf(out, "no nodes.\n")
	}
	for _, f := range findings {
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

	all := append(append([]reconcile.Finding(nil), findings...), stores...)
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
		outf(out, "ember reap destroys every node past its deadline.\n\n")
		outf(out, "Nodes with no local record are reported and left alone: one may belong to\nanother machine of yours, and destroying somebody's running work because\nthis one had not heard of it is not a trade to make automatically.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	findings, _, ctx, err := scan(*cfgPath, out, d)
	if err != nil {
		return err
	}

	expired := 0
	for _, f := range findings {
		if f.Kind == reconcile.KindExpired {
			expired++
			outf(out, "%s\n", f)
		}
	}
	if expired == 0 {
		outf(out, "nothing to reap.\n")
		return nil
	}
	if *dryRun {
		outf(out, "dry-run: would destroy %d node(s); nothing was touched\n", expired)
		return nil
	}

	reg, store, _, err := registryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	destroyed, rerr := reconcile.Reap(ctx, reg, store, findings)
	outf(out, "destroyed %d of %d expired node(s)\n", destroyed, expired)
	if rerr != nil {
		return rerr
	}
	if destroyed < expired {
		return &FindingsError{N: expired - destroyed}
	}
	return nil
}

// scan collects from every provider and classifies the result.
//
// Nodes and stores come back separately because they are acted on
// differently: reap destroys expired nodes and must never be handed a store,
// and the two cost shapes print differently. One registry serves both, so a
// caller that needs the stores does not pay for a second round of config
// loading and credential reading.
func scan(cfgPath string, out io.Writer, d deps) ([]reconcile.Finding, []reconcile.Finding, context.Context, error) {
	reg, store, cfg, err := registryAndStore(cfgPath, d)
	if err != nil {
		return nil, nil, nil, err
	}

	records, err := store.List()
	if err != nil {
		return nil, nil, nil, err
	}

	ctx := context.Background()
	listings, errs := reconcile.Collect(ctx, reg)
	nodes := reconcile.Scan(listings, errs, records, time.Now())

	stores, storeErrs := reconcile.CollectStores(ctx, reg)
	configured := map[node.ProviderID]string{}
	if cfg.HasStore() {
		configured[node.ProviderID(cfg.Provider)] = cfg.Store.Name
	}
	return nodes, reconcile.ScanStores(stores, storeErrs, configured), ctx, nil
}

// registryAndStore builds everything a scan needs from one config read.
//
// The config comes back with them because the caller needs it too, and a
// second config.Load would be a second chance for the two copies to disagree
// about which store this run is talking about.
func registryAndStore(cfgPath string, d deps) (*provider.Registry, *statestore.Store, config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, cfg, err
	}
	ps, err := resolvePaths(cfg, d)
	if err != nil {
		return nil, nil, cfg, err
	}
	creds, err := d.credentials(ps.config)
	if err != nil {
		return nil, nil, cfg, err
	}
	reg, err := buildRegistry(d, creds, cfg)
	if err != nil {
		return nil, nil, cfg, err
	}
	store, err := openStore(ps.state)
	if err != nil {
		return nil, nil, cfg, err
	}
	return reg, store, cfg, nil
}
