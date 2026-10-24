package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/statestore"
)

func cmdDown(args []string, out io.Writer) error   { return runDown(args, out, deps{}) }
func cmdOffers(args []string, out io.Writer) error { return runOffers(args, out, deps{}) }

// runDown destroys one node by handle.
//
// It exists for the case `reap` deliberately will not touch: a node inside its
// deadline that the operator has decided is finished with. reap is bound by
// the deadline because it runs unattended; a person naming a specific node is
// not guessing, so this does what they asked.
func runDown(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	fs.Usage = func() {
		outf(out, "ember down destroys one node, named as provider/id.\n\n")
		outf(out, "Use `ember ls` to see the handles. Unlike reap, this ignores the\ndeadline: you named the node, so you are not guessing.\n\n")
		outf(out, "usage: ember down <provider/id> [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	if fs.NArg() != 1 {
		return usageError{fmt.Errorf("ember down takes exactly one handle, as provider/id; got %d argument(s)", fs.NArg())}
	}
	h, err := parseHandle(fs.Arg(0))
	if err != nil {
		return usageError{err}
	}

	reg, store, err := registryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	p, err := reg.Get(h.Provider)
	if err != nil {
		return usageError{err}
	}

	// No not-found special case here. The Provider interface already requires
	// Destroy to treat "no such machine" as success -- the goal of destroy is
	// that the node is gone, and it is -- so a second check would be an
	// unreachable branch guarding against a provider breaking its own
	// contract, and that belongs in the provider's tests, not hidden here.
	if err := p.Destroy(context.Background(), h); err != nil {
		return fmt.Errorf("destroy %s: %w", h, err)
	}
	outf(out, "destroyed %s\n", h)

	// The record is cleared only after the provider confirmed. A record
	// forgotten before that is a node nothing will ever look for again.
	if rec, found := recordFor(store, h); found {
		if err := store.Forget(rec.RunID); err != nil {
			return fmt.Errorf("the node is gone but its record could not be cleared, so ember ls will keep reporting it: %w", err)
		}
	}
	return nil
}

// parseHandle splits provider/id, refusing anything else.
//
// Parse, do not grep: a handle with no provider would be dispatched to
// whichever provider happened to be first in the registry, and "destroy"
// aimed at the wrong provider is a request that either does nothing or
// destroys something else.
func parseHandle(s string) (node.Handle, error) {
	prov, id, found := strings.Cut(strings.TrimSpace(s), "/")
	if !found || prov == "" || id == "" {
		return node.Handle{}, fmt.Errorf("%q is not a handle: ember expects provider/id, as printed by ember ls", s)
	}
	if strings.Contains(id, "/") {
		return node.Handle{}, fmt.Errorf("%q has more than one slash; a handle is provider/id", s)
	}
	return node.Handle{Provider: node.ProviderID(prov), ID: id}, nil
}

func recordFor(store *statestore.Store, h node.Handle) (statestore.Record, bool) {
	recs, err := store.List()
	if err != nil {
		// Not fatal: the node is already destroyed, and a journal that cannot
		// be read is a separate problem that `ember ls` will report.
		return statestore.Record{}, false
	}
	for _, r := range recs {
		if rh, ok := r.Handle(); ok && rh == h {
			return r, true
		}
	}
	return statestore.Record{}, false
}

// runOffers lists what can be rented.
func runOffers(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("offers", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cfgPath      = fs.String("config", config.FileName, "path to ember.yaml")
		providerName = fs.String("provider", "", "which provider to ask; empty asks every configured one")
		gpu          = fs.String("gpu", "", "GPU model to require, for example rtx4090")
		maxHourly    = fs.String("max-hourly-usd", "", "hide any offer dearer than this per hour")
	)
	fs.Usage = func() {
		outf(out, "ember offers lists what can be rented, cheapest first.\n\n")
		outf(out, "Prices are per hour in USD, and are what the provider quoted at the\nmoment of asking. They are not a commitment: an offer can be gone by the\ntime ember tries to rent it, which is why ember up re-filters rather than\ntrusting a listing.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	applyFlags(&cfg, *providerName, "", *gpu, *maxHourly, 0, nil)

	filter, err := cfg.Filter()
	if err != nil {
		return usageError{err}
	}
	ps, err := resolvePaths(cfg, d)
	if err != nil {
		return err
	}
	creds, err := d.credentials(ps.config)
	if err != nil {
		return err
	}
	reg, err := buildRegistry(d, creds, cfg)
	if err != nil {
		return err
	}

	targets := reg.All()
	if *providerName != "" {
		p, err := reg.Get(node.ProviderID(*providerName))
		if err != nil {
			return usageError{err}
		}
		targets = []provider.Provider{p}
	}

	ctx := context.Background()
	shown, failed := 0, 0
	for _, p := range targets {
		offers, err := p.Offers(ctx, filter)
		if err != nil {
			// Reported, not returned: one unreachable provider must not hide
			// what the others quoted, and must not look like success either.
			outf(out, "%-10s could not be asked: %v\n", p.Name(), err)
			failed++
			continue
		}
		// Re-filtered our side regardless of what the API was asked: a
		// provider that ignores a price filter must not have its dearest
		// offer printed as though ember would rent it.
		for _, o := range provider.FilterOffers(filter, offers) {
			outf(out, "%-10s %-24s %-12s %s\n", p.Name(), o.Token, o.GPU, o.Price)
			shown++
		}
	}

	if shown == 0 && failed == 0 {
		outf(out, "no offers match the filter.\n")
	}
	if failed > 0 {
		return &FindingsError{N: failed}
	}
	return nil
}
