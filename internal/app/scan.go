package app

import (
	"context"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/reconcile"
	"github.com/Legion112/ember/internal/statestore"
)

// RegistryAndStore builds everything a scan needs from one config read.
//
// The config comes back with them because the caller needs it too, and a
// second config.Load would be a second chance for the two copies to disagree
// about which store this run is talking about.
func RegistryAndStore(cfgPath string, d Deps) (*provider.Registry, *statestore.Store, config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, cfg, err
	}
	ps, err := ResolvePaths(cfg, d)
	if err != nil {
		return nil, nil, cfg, err
	}
	creds, err := d.Credentials(ps.Config)
	if err != nil {
		return nil, nil, cfg, err
	}
	reg, err := BuildRegistry(d, creds, cfg)
	if err != nil {
		return nil, nil, cfg, err
	}
	store, err := OpenStore(ps.State)
	if err != nil {
		return nil, nil, cfg, err
	}
	return reg, store, cfg, nil
}

// Scanned is what one sweep of every provider found.
//
// Nodes and stores come back separately because they are acted on
// differently: reap destroys expired nodes and must never be handed a store,
// and the two cost shapes print differently. The registry and the journal come
// back too, so a caller that then acts on a finding -- reap does -- does not
// pay for a second round of config loading and credential reading, and cannot
// act against a different registry than the one it scanned.
type Scanned struct {
	Nodes  []reconcile.Finding
	Stores []reconcile.Finding

	Registry *provider.Registry
	Store    *statestore.Store
	Config   config.Config
}

// Scan collects from every provider and classifies the result.
//
// It takes a context rather than handing one back. The earlier spelling
// returned context.Background() for the caller to reuse, which reads as
// though the scan's context mattered to what came next; it does not.
func Scan(ctx context.Context, cfgPath string, d Deps) (Scanned, error) {
	reg, store, cfg, err := RegistryAndStore(cfgPath, d)
	if err != nil {
		return Scanned{}, err
	}

	records, err := store.List()
	if err != nil {
		return Scanned{}, err
	}

	listings, errs := reconcile.Collect(ctx, reg)
	nodes := reconcile.Scan(listings, errs, records, time.Now())

	stores, storeErrs := reconcile.CollectStores(ctx, reg)
	configured := map[node.ProviderID]string{}
	if cfg.HasStore() {
		configured[node.ProviderID(cfg.Provider)] = cfg.Store.Name
	}
	return Scanned{
		Nodes:    nodes,
		Stores:   reconcile.ScanStores(stores, storeErrs, configured),
		Registry: reg,
		Store:    store,
		Config:   cfg,
	}, nil
}

// OfferListing is what one provider answered when asked what it has.
//
// The error travels with the answer rather than being returned separately,
// because "I could not ask RunPod" must never render as "RunPod has nothing":
// one unreachable provider must not hide what the others quoted, and must not
// look like success either.
type OfferListing struct {
	Provider node.ProviderID
	Offers   []node.Offer
	Err      error
}

// Offers asks each provider what it has, cheapest first.
//
// only names one provider; empty asks every configured one. The filter is
// re-applied on our side of the wire by provider.FilterOffers regardless of
// what the API was asked, because a marketplace that answers a max-price
// query with a dearer offer must not have that offer presented as something
// ember would rent.
func Offers(ctx context.Context, reg *provider.Registry, filter node.Filter, only string) ([]OfferListing, error) {
	targets := reg.All()
	if only != "" {
		p, err := reg.Get(node.ProviderID(only))
		if err != nil {
			return nil, Usage(err)
		}
		targets = []provider.Provider{p}
	}

	out := make([]OfferListing, 0, len(targets))
	for _, p := range targets {
		offers, err := p.Offers(ctx, filter)
		if err != nil {
			out = append(out, OfferListing{Provider: p.Name(), Err: err})
			continue
		}
		out = append(out, OfferListing{
			Provider: p.Name(),
			Offers:   provider.FilterOffers(filter, offers),
		})
	}
	return out, nil
}
