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
	"path/filepath"

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
	outf(out, "  push     upload a local file to the store, without renting anything\n")
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
func attachStore(ctx context.Context, p provider.Provider, cfg config.Config) (Attached, error) {
	if !cfg.HasStore() {
		return Attached{}, nil
	}

	sp, ok := p.(provider.StoreAPI)
	if !ok || !p.Caps().Stores {
		return Attached{}, fmt.Errorf("this configuration names the store %q, but provider %s has no persistent storage; "+
			"remove the store block or choose a provider that does", cfg.Store.Name, p.Name())
	}

	s, found, err := findStore(ctx, sp, cfg.Store.Name)
	if err != nil {
		return Attached{}, err
	}
	if !found {
		return Attached{}, fmt.Errorf("no store named %q at %s. Create it with:\n  ember store create\n"+
			"Nothing here will make one for you: a store bills by the month and nothing destroys it on a deadline",
			cfg.Store.Name, p.Name())
	}

	mount, err := cfg.StoreMount()
	if err != nil {
		return Attached{}, err
	}
	return Attached{Handle: s.Handle, Mount: mount, Region: s.Region}, nil
}

// Attached is the store this run will mount, resolved.
//
// A value rather than three return parameters, because the three travel
// together everywhere and a caller that dropped the region would silently
// lose the placement lock -- the one part of this whose absence looks like
// success right up until the node lands in the wrong datacentre.
type Attached struct {
	Handle node.StoreHandle
	Mount  node.Mount

	// Region is the store's datacentre, empty for a provider that does not
	// pin stores to one.
	Region string
}

// IsZero reports whether this run mounts nothing.
func (a Attached) IsZero() bool { return a.Handle.IsZero() }

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

	reg, _, cfg, err := registryAndStore(*cfgPath, d)
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

	sp, p, err := storeProvider(reg, node.ProviderID(cfg.Provider))
	if err != nil {
		return err
	}
	ctx := context.Background()
	store, found, err := findStore(ctx, sp, cfg.Store.Name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no store named %q at %s. Create it with:\n  ember store create", cfg.Store.Name, p.Name())
	}
	if store.Region == "" {
		return fmt.Errorf("provider %s does not expose its stores over S3, so there is nothing to push to. "+
			"Put the file on the volume directly", p.Name())
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

	creds, err := d.credentials(mustConfigDir(cfg, d))
	if err != nil {
		return err
	}

	endpoint := s3.EndpointFor(store.Region)
	if d.S3BaseURL != "" {
		endpoint = d.S3BaseURL
	}
	c := &s3.Client{
		Endpoint: endpoint,
		Region:   store.Region,
		Creds:    creds.S3,
	}
	if c.Creds.IsZero() {
		return fmt.Errorf("no S3 credentials. RunPod issues these separately from the API key, "+
			"on the Credentials page under \"S3 API keys\", and shows the secret once. "+
			"Put them in credentials.yaml as runpod_s3_access_key and runpod_s3_secret, or set %s and %s",
			awssig.EnvAccessKey, awssig.EnvSecret)
	}

	outf(out, "pushing %s to %s in %s\n", local, store.Name, store.Region)
	if err := c.Upload(ctx, string(store.Handle.ID), name, f, fi.Size(), out); err != nil {
		return err
	}

	outf(out, "\nuploaded. Add it to %s:\n\nassets:\n  - url: <where a node can fetch it>\n    sha256: %s\n    as: %s\n",
		*cfgPath, sum, name)
	return nil
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

// mustConfigDir resolves the credentials directory, falling back to the
// config's own so that a failure here is reported by the loader rather than
// by a nil path somewhere lower down.
func mustConfigDir(cfg config.Config, d deps) string {
	if ps, err := resolvePaths(cfg, d); err == nil {
		return ps.config
	}
	return cfg.ConfigDir
}
