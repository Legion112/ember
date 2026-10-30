package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

func cmdStore(args []string, out io.Writer) error { return runStore(args, out, deps{}) }

// runStore dispatches the store subcommands.
//
// A second level of switch rather than `store-create` and `store-rm` at the
// top: these are the only commands in ember that act on something which
// outlives a run, and grouping them is what makes that visible in `ember
// help` rather than mixed in among the node commands.
func runStore(args []string, out io.Writer, d deps) error {
	if len(args) == 0 {
		storeUsage(out)
		return usageError{errors.New("ember store needs a subcommand")}
	}
	switch args[0] {
	case "create":
		return runStoreCreate(args[1:], out, d)
	case "rm":
		return runStoreRm(args[1:], out, d)
	case "-h", "--help", "help":
		storeUsage(out)
		return nil
	default:
		storeUsage(out)
		return usageError{fmt.Errorf("unknown store subcommand %q", args[0])}
	}
}

func storeUsage(out io.Writer) {
	outf(out, "ember store manages persistent storage that outlives a node.\n\n")
	outf(out, "A store is the one thing ember creates that no deadline, no reaper and\nno Ctrl-C ever ends. It bills by the month whether or not anything is\nattached to it, which is why nothing creates one as a side effect.\n\n")
	outf(out, "  create   create the store named in ember.yaml\n")
	outf(out, "  rm       destroy a store and everything on it\n\n")
	outf(out, "`ember ls` reports every store and what it costs.\n")
}

// runStoreCreate creates the store this configuration names.
//
// The size and region come from the config rather than from flags, so the
// thing that gets created is the thing every later `ember up` will look for.
// A store created with one size by a flag and referred to by a config naming
// another is a mismatch nothing would report.
func runStoreCreate(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("store create", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	fs.Usage = func() {
		outf(out, "ember store create makes the store named in ember.yaml.\n\n")
		outf(out, "It is the only thing in ember that brings a standing monthly charge\ninto existence, and it exists as a command precisely so that renting a\nnode cannot do it by accident.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember store create takes no arguments; it creates the store named in %s", *cfgPath)}
	}

	reg, _, cfg, err := registryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	if !cfg.HasStore() {
		return usageError{fmt.Errorf("no store is named in %s; add a store block with at least a name, "+
			"a size_gib and a region", *cfgPath)}
	}

	sp, p, err := storeProvider(reg, node.ProviderID(cfg.Provider))
	if err != nil {
		return err
	}

	s, err := sp.StoreCreate(context.Background(), cfg.Store.Name, cfg.Store.SizeGiB, cfg.Store.Region)
	if err != nil {
		return err
	}

	outf(out, "created %s\n", s)
	if m := s.Monthly(); m > 0 {
		// Said at the moment of creation, not only by `ember ls` later. This
		// is the one command whose whole cost is in the future, and the
		// operator is never again as well placed to decide it is too much.
		outf(out, "this will cost %s every month until `ember store rm %s` removes it.\n", m, s.Name)
	} else if p.Caps().Free {
		outf(out, "this provider charges nothing for storage.\n")
	}
	return nil
}

// runStoreRm destroys a store.
//
// The name is a required argument and is never taken from the config. `ember
// store rm` with no arguments would otherwise destroy whatever the current
// directory's ember.yaml happened to point at, and what it destroys is
// gigabytes somebody paid to upload -- there is no deadline that would have
// ended it, which means there is no accident that would have been harmless.
func runStoreRm(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("store rm", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	providerName := fs.String("provider", "", "which provider holds it; defaults to the configured one")
	fs.Usage = func() {
		outf(out, "ember store rm destroys a store and everything on it.\n\n")
		outf(out, "The name is required and is never read from the config: what this\nremoves is not recreated by a deadline, and the data on it is not\nrecreated at all.\n\n")
		outf(out, "usage: ember store rm <name> [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 1 {
		// Named rather than counted, because the usual cause is flag order:
		// the standard flag package stops parsing at the first non-flag
		// argument, so `ember store rm models -config x` leaves three.
		return usageError{fmt.Errorf("ember store rm takes exactly one store name and got %d argument(s) (%v); "+
			"flags come before the name, as in `ember store rm -config %s models`", fs.NArg(), fs.Args(), *cfgPath)}
	}
	name := fs.Arg(0)

	reg, _, cfg, err := registryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	want := cfg.Provider
	if *providerName != "" {
		want = *providerName
	}
	sp, _, err := storeProvider(reg, node.ProviderID(want))
	if err != nil {
		return err
	}

	ctx := context.Background()
	s, found, err := findStore(ctx, sp, name)
	if err != nil {
		return err
	}
	if !found {
		// Not an error. The caller asked for the store to be gone and it is,
		// which is the same contract destroy has everywhere else here.
		outf(out, "no store named %s at %s.\n", name, want)
		return nil
	}

	if err := sp.StoreDestroy(ctx, s.Handle); err != nil {
		return err
	}
	outf(out, "destroyed %s\n", s)
	return nil
}

// storeProvider resolves a provider that has stores, or says why not.
func storeProvider(reg *provider.Registry, name node.ProviderID) (provider.StoreAPI, provider.Provider, error) {
	p, err := reg.Get(name)
	if err != nil {
		return nil, nil, usageError{err}
	}
	sp, ok := p.(provider.StoreAPI)
	if !ok || !p.Caps().Stores {
		return nil, nil, fmt.Errorf("provider %s has no persistent storage, so there is no store to manage there", name)
	}
	return sp, p, nil
}

// findStore looks a store up by the name ember knows it as.
//
// By listing rather than by constructing a handle from the name. The provider
// decides what a store's id is -- RunPod's is an opaque token, the local
// provider's is the volume name -- so building one here would be this package
// guessing at another's identifiers, and a guess that is right for one
// provider and wrong for the next is how `rm` misses.
func findStore(ctx context.Context, sp provider.StoreAPI, name string) (node.Store, bool, error) {
	list, err := sp.StoreList(ctx)
	if err != nil {
		return node.Store{}, false, err
	}
	for _, s := range list {
		if s.Name == name {
			return s, true, nil
		}
	}
	return node.Store{}, false, nil
}

// attachStore resolves the configured store and puts it on the spec.
//
// It also narrows the offer filter to the store's datacentre, which is the
// placement constraint a store implies: the volume lives in one site and a
// node that mounts it must be created there. Expressing that as a filter
// rather than as provider state is what lets two runs want different stores
// without rebuilding anything.
//
// A missing store is a refusal naming the command that makes one. Creating it
// here would be ember bringing a standing monthly charge into existence
// because a config mentioned a name, which is the one thing `ember store
// create` exists to prevent.
func attachStore(ctx context.Context, p provider.Provider, cfg config.Config, spec node.Spec) (node.Spec, error) {
	if !cfg.HasStore() {
		return spec, nil
	}

	sp, ok := p.(provider.StoreAPI)
	if !ok || !p.Caps().Stores {
		return spec, fmt.Errorf("this configuration names the store %q, but provider %s has no persistent storage; "+
			"remove the store block or choose a provider that does", cfg.Store.Name, p.Name())
	}

	s, found, err := findStore(ctx, sp, cfg.Store.Name)
	if err != nil {
		return spec, err
	}
	if !found {
		return spec, fmt.Errorf("no store named %q at %s. Create it with:\n  ember store create\n"+
			"Nothing here will make one for you: a store bills by the month and nothing destroys it on a deadline",
			cfg.Store.Name, p.Name())
	}

	mount, err := cfg.StoreMount()
	if err != nil {
		return spec, err
	}

	spec.Store, spec.Mount = s.Handle, mount
	if s.Region != "" {
		// The store's own region wins over whatever the config asked for.
		// They can only disagree by mistake, and the store is the half that
		// cannot move.
		spec.Filter.Region = s.Region
	}
	return spec, nil
}
