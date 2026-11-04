package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/s3"
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
	case "push":
		return runStorePush(args[1:], out, d)
	case "sync":
		return runStoreSync(args[1:], out, d)
	case "ls":
		return runStoreLs(args[1:], out, d)
	case "abort":
		return runStoreAbort(args[1:], out, d)
	case "verify":
		return runStoreVerify(args[1:], out, d)
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
	outf(out, "  ls       list what is on a store, and any upload still billing\n")
	outf(out, "  verify   check that every configured asset is there, before renting\n")
	outf(out, "  push     upload a local file to the store, without renting anything\n")
	outf(out, "  sync     rent a cheap CPU machine to fetch the configured assets onto it\n")
	outf(out, "  abort    remove incomplete uploads that are billing for nothing\n")
	outf(out, "  rm       destroy a store and everything on it\n\n")
	outf(out, "`ember ls` reports every store and what it costs. It cannot see an\ninterrupted multipart upload, which is not an object and bills by the\nmonth anyway -- `ember store ls` is what finds those.\n")
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

	reg, _, cfg, err := app.RegistryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	if !cfg.HasStore() {
		return usageError{fmt.Errorf("no store is named in %s; add a store block with at least a name, "+
			"a size_gib and a region", *cfgPath)}
	}

	sp, p, err := app.StoreProvider(reg, node.ProviderID(cfg.Provider))
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

	reg, _, cfg, err := app.RegistryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	want := cfg.Provider
	if *providerName != "" {
		want = *providerName
	}
	sp, _, err := app.StoreProvider(reg, node.ProviderID(want))
	if err != nil {
		return err
	}

	ctx := context.Background()
	s, found, err := app.FindStore(ctx, sp, name)
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

// runStorePush uploads a local file to the store over the S3 endpoint.
//
// The point of doing it this way is that no compute is running: the bytes go
// straight to the volume, and the only charge is the storage that was going
// to exist anyway. Fetching the same file from a node would bill the download
// at GPU rates -- about ten minutes of an H100 for a 40 GB model set, every
// time a store is filled.
//
// It prints the digest at the end because that is the value `assets:` needs,
// and reading it from the same pass that uploaded the file is how the two
// cannot disagree.
func runStorePush(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("store push", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	as := fs.String("as", "", "what to call it on the store; defaults to the file's own name")
	fs.Usage = func() {
		outf(out, "ember store push uploads a local file to the store.\n\n")
		outf(out, "Nothing is rented. The file goes straight to the volume over RunPod's\nS3 endpoint, so the only charge is the storage itself -- fetching the\nsame file from a node would bill the download at GPU rates.\n\n")
		outf(out, "usage: ember store push <file> [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 1 {
		return usageError{fmt.Errorf("ember store push takes exactly one file and got %d argument(s) (%v); "+
			"flags come before the file, as in `ember store push -config %s ./sd15.gguf`", fs.NArg(), fs.Args(), *cfgPath)}
	}
	local := fs.Arg(0)

	reg, _, cfg, err := app.RegistryAndStore(*cfgPath, d)
	if err != nil {
		return err
	}
	if !cfg.HasStore() {
		return usageError{fmt.Errorf("no store is named in %s; push needs somewhere to put the file", *cfgPath)}
	}

	name := *as
	if name == "" {
		name = filepath.Base(local)
	}
	// The same check the node applies to an asset name, so a file pushed
	// under a name the node would refuse fails here instead of after the
	// upload.
	name, err = node.SafeRelPath(name)
	if err != nil {
		return usageError{fmt.Errorf("name %q %w", name, err)}
	}

	// The local, free checks first. A typo in a filename should fail without
	// a round trip, and an unreadable file should fail before anything asks
	// a provider about a store.
	//
	// The operator named this file on the command line, which is the whole
	// purpose of the argument; there is no path to constrain it to, because
	// push uploads a checkpoint from wherever it was downloaded.
	f, err := os.Open(local) //nolint:gosec // G304: the file to upload is the argument
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory; push takes one file", local)
	}

	ctx := context.Background()
	c, store, err := storeOverS3(ctx, reg, cfg, cfg.Store.Name, "push to", d)
	if err != nil {
		return err
	}

	// Hashed last, because it reads the whole file: a missing credential or
	// a store in the wrong datacentre should not cost a pass over forty
	// gigabytes first. It has to be known to print at the end, and a digest
	// taken from the same bytes that went up is the only one worth putting
	// in a config.
	sum, err := digestOf(f)
	if err != nil {
		return err
	}

	outf(out, "pushing %s to %s in %s\n", local, store.Name, store.Region)
	if err := c.Upload(ctx, string(store.Handle.ID), name, f, fi.Size(), out); err != nil {
		return err
	}

	outf(out, "\nuploaded. Add it to %s:\n\nassets:\n  - url: <where a node can fetch it>\n    sha256: %s\n    as: %s\n",
		*cfgPath, sum, name)
	return nil
}

// storeOverS3 resolves one store and returns a client for its endpoint.
//
// Factored out of push once three more commands needed the same six steps.
// The ordering is the point and is why it is one function rather than four
// copies: resolve the provider, find the store, refuse a provider with no S3
// endpoint, then check the credentials -- every refusal before anything is
// read, uploaded or billed. A copy that got the order wrong would hash forty
// gigabytes before discovering there is nowhere to put them.
//
// verb goes into the refusals so each one reads as the command the operator
// typed: "nothing to push to" and "nothing to list" are the same fact and
// different sentences.
func storeOverS3(
	ctx context.Context,
	reg *provider.Registry,
	cfg config.Config,
	name, verb string,
	d deps,
) (*s3.Client, node.Store, error) {
	sp, p, err := app.StoreProvider(reg, node.ProviderID(cfg.Provider))
	if err != nil {
		return nil, node.Store{}, err
	}
	store, found, err := app.FindStore(ctx, sp, name)
	if err != nil {
		return nil, node.Store{}, err
	}
	if !found {
		return nil, node.Store{}, fmt.Errorf("no store named %q at %s. Create it with:\n  ember store create", name, p.Name())
	}
	if store.Region == "" {
		// The local provider reports no region for its volumes, because a
		// Docker volume is not in a datacentre. Reported rather than
		// attempted against a hostname that does not exist.
		return nil, node.Store{}, fmt.Errorf("provider %s does not expose its stores over S3, so there is nothing to %s. "+
			"The volume is on this machine's docker daemon", p.Name(), verb)
	}

	creds, err := d.Credentials(app.ConfigDir(cfg, d))
	if err != nil {
		return nil, node.Store{}, err
	}

	endpoint := s3.EndpointFor(store.Region)
	if d.S3BaseURL != "" {
		endpoint = d.S3BaseURL
	}
	c := &s3.Client{Endpoint: endpoint, Region: store.Region, Creds: creds.S3}
	if c.Creds.IsZero() {
		return nil, node.Store{}, fmt.Errorf("no S3 credentials. RunPod issues these separately from the API key, "+
			"on the Credentials page under \"S3 API keys\", and shows the secret once. "+
			"Put them in credentials.yaml as runpod_s3_access_key and runpod_s3_secret, or set %s and %s",
			awssig.EnvAccessKey, awssig.EnvSecret)
	}
	return c, store, nil
}

// digestOf hashes the whole file and rewinds it for the upload.
func digestOf(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", f.Name(), err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runStoreSync fills the store from a rented CPU machine.
//
// The alternative to `push`, and which one is right depends entirely on
// where the bytes are. Push sends them from here, over a home link, for
// nothing: measured at 67 Mbps that is four minutes for a 2 GB checkpoint
// and eighty-one for a 40 GB model set. Sync rents the cheapest machine
// RunPod has -- $0.06/hour against $0.89 for a 4090 -- and has it download
// the same files at datacentre bandwidth, which turns those eighty-one
// minutes into a few and costs about a cent.
//
// So: push what is already on this disk, sync what is on the internet.
//
// It is the same `embernode fetch` the workload path runs, on the same
// store, verified against the same digests. The only difference is that no
// workload follows, and the machine has no GPU to waste while it downloads.
func runStoreSync(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("store sync", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", config.FileName, "path to ember.yaml")
	vcpu := fs.Int("vcpu", 0, "cores to rent; the minimum for the family when unset")
	dryRun := fs.Bool("dry-run", false, "say what would be rented and fetched, and do neither")
	fs.Usage = func() {
		outf(out, "ember store sync fetches the configured assets onto the store.\n\n")
		outf(out, "It rents the cheapest machine with no GPU and has that download them,\nwhich is what you want when the files are on the internet rather than\non this disk. `ember store push` is the other direction.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember store sync takes no arguments; it fetches what %s lists", *cfgPath)}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Assets) == 0 {
		return usageError{fmt.Errorf("no assets are listed in %s, so there is nothing to fetch", *cfgPath)}
	}

	// The same pipeline `ember up` runs, asked for a machine with no card.
	// It used to be a second copy of that sequence in this file -- load,
	// resolve, credentials, registry, store, filter, options, offer, image,
	// spec -- and two copies of an ordering whose whole purpose is that every
	// refusal happens before the create call is one copy too many.
	plan, err := app.NewPlan(context.Background(), app.Request{
		Config: cfg, Deps: d, CPUOnly: true, VCPU: *vcpu,
	}, out)
	if err != nil {
		return err
	}

	if *dryRun {
		return dryRunUp(out, plan)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// BOUNDED AND RECORDED, THROUGH `ember up`'s OWN HELPER. This is a rental
	// -- the cheapest machine with no card, for as long as the download takes
	// -- and it was in no ledger (#148) and inside no period ceiling (#165)
	// either, which is the same pair of defects reached from the command line
	// instead of from the queue. `ember serve`'s own store fill has had both
	// gates since #142; this is the command an operator types for the same
	// work, and $0.06/hour for eighty-one minutes is still money that lands on
	// the invoice `max_month_usd` is defending.
	//
	// boundAndRecord opens the store in this plan's own state directory,
	// refuses the rental if a configured ceiling would not take it, writes the
	// row before the create call and hands back the sink the rental reports
	// to. Recording still cannot fail the run; the ceiling can, which is the
	// one asymmetry and is argued where the function is. `report.Text` is
	// silent about every event a sync emits, so this command's output is
	// byte-for-byte what it was.
	sink, finish, err := boundAndRecord(ctx, out, cfg, plan)
	if err != nil {
		return err
	}
	syncErr := plan.SyncAssets(ctx, out, sink)
	finish(syncErr)
	if syncErr != nil {
		return syncErr
	}
	outf(out, "the store now holds every asset %s lists.\n", *cfgPath)
	return nil
}
