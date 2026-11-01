package app

import (
	"context"
	"fmt"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// StoreProvider resolves a provider that has stores, or says why not.
func StoreProvider(reg *provider.Registry, name node.ProviderID) (provider.StoreAPI, provider.Provider, error) {
	p, err := reg.Get(name)
	if err != nil {
		return nil, nil, Usage(err)
	}
	sp, ok := p.(provider.StoreAPI)
	if !ok || !p.Caps().Stores {
		return nil, nil, fmt.Errorf("provider %s has no persistent storage, so there is no store to manage there", name)
	}
	return sp, p, nil
}

// FindStore looks a store up by the name ember knows it as.
//
// By listing rather than by constructing a handle from the name. The provider
// decides what a store's id is -- RunPod's is an opaque token, the local
// provider's is the volume name -- so building one here would be this package
// guessing at another's identifiers, and a guess that is right for one
// provider and wrong for the next is how `rm` misses.
func FindStore(ctx context.Context, sp provider.StoreAPI, name string) (node.Store, bool, error) {
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

// AttachStore resolves the configured store and puts it on the spec.
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
func AttachStore(ctx context.Context, p provider.Provider, cfg config.Config) (Attached, error) {
	if !cfg.HasStore() {
		return Attached{}, nil
	}

	sp, ok := p.(provider.StoreAPI)
	if !ok || !p.Caps().Stores {
		return Attached{}, fmt.Errorf("this configuration names the store %q, but provider %s has no persistent storage; "+
			"remove the store block or choose a provider that does", cfg.Store.Name, p.Name())
	}

	s, found, err := FindStore(ctx, sp, cfg.Store.Name)
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
