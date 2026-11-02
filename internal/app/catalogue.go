package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// OpenCatalogue opens the availability series.
//
// One name for the path, as OpenStore is for the journal: the subdirectory is
// named once in internal/catalogue and joined here, so the collector and the
// reader cannot end up reading different directories -- which is how a lab
// assertion about an empty journal once passed while ember was writing
// records perfectly well.
func OpenCatalogue(dir string) (*catalogue.Store, error) {
	return catalogue.Open(filepath.Join(dir, catalogue.DirName))
}

// CatalogueTaken is one provider's contribution to a snapshot.
type CatalogueTaken struct {
	Provider node.ProviderID
	GPURows  int
	CPURows  int
	Sites    int

	// NoSite counts rows the provider named no datacentre for.
	//
	// Reported rather than folded into the row count, because it is the
	// interesting daily figure and because without it "3 gpu row(s) across 2
	// site(s)" reads as an arithmetic mistake. In the captured catalogue
	// every such type also read NONE overall, so this is roughly "cards
	// RunPod is not offering anywhere today" -- which is the series' own
	// question asked of one sample.
	NoSite int
}

// CatalogueFailure is a provider that could not be sampled.
//
// Reported rather than swallowed. An hour that is missing from the series is
// a gap nothing can fill in later, so "I could not ask RunPod" must not read
// the same as "RunPod had nothing" -- the rule `ember ls --all` exits
// non-zero for, applied to the one dataset that cannot be re-derived.
type CatalogueFailure struct {
	Provider node.ProviderID
	Err      error
}

// CatalogueSnapshot is what one collection run did.
type CatalogueSnapshot struct {
	Dir     string
	Files   []string
	Samples int
	Taken   []CatalogueTaken
	Failed  []CatalogueFailure

	// First is true when the directory held no day file before this run.
	//
	// Carried so the command can say, exactly once, that the value is in the
	// series and that nothing here keeps itself running. A collector that
	// must be scheduled and was not is a collector that will be found
	// stopped, and the only moment an operator is reading this output
	// carefully is the first one.
	First bool

	// At is the instant stamped on every sample, which is the whole point of
	// the row and therefore worth printing back.
	At time.Time
}

// CollectCatalogue samples every provider that keeps a catalogue and appends
// the result to the series.
//
// ONE APPEND FOR EVERY PROVIDER, after all of them have answered. A write per
// provider would leave a half-recorded hour behind when the second one fails,
// and the file is append-only: there is no pass that comes back and tidies it.
func CollectCatalogue(ctx context.Context, cfgPath string, d Deps) (CatalogueSnapshot, error) {
	reg, _, cfg, err := RegistryAndStore(cfgPath, d)
	if err != nil {
		return CatalogueSnapshot{}, err
	}
	ps, err := ResolvePaths(cfg, d)
	if err != nil {
		return CatalogueSnapshot{}, err
	}
	store, err := OpenCatalogue(ps.State)
	if err != nil {
		return CatalogueSnapshot{}, err
	}

	out := CatalogueSnapshot{Dir: store.Dir(), At: d.Clock()}

	days, err := store.Days()
	if err != nil {
		return out, err
	}
	out.First = len(days) == 0

	var all []catalogue.Sample
	capable := 0
	for _, p := range reg.All() {
		if !p.Caps().Catalogue {
			continue
		}
		capable++
		api, ok := p.(provider.CatalogueAPI)
		if !ok {
			// The capability is a claim about code and this is the claim
			// being wrong. Loud, because providertest.AssertCatalogueCapsMatch
			// exists to make it impossible and reaching it means that
			// assertion is not being called.
			return out, fmt.Errorf("provider %q says Caps.Catalogue but does not implement provider.CatalogueAPI; "+
				"its own test should be calling providertest.AssertCatalogueCapsMatch", p.Name())
		}
		samples, err := api.CatalogueSnapshot(ctx, out.At)
		if err != nil {
			out.Failed = append(out.Failed, CatalogueFailure{Provider: p.Name(), Err: err})
		}
		if len(samples) == 0 {
			continue
		}
		all = append(all, samples...)
		out.Taken = append(out.Taken, summariseTaken(p.Name(), samples))
	}

	if capable == 0 {
		// A usage error and not an empty run: there is nothing to fix on the
		// provider's side, and a timer that silently collects nothing every
		// hour is the failure this whole feature is about.
		return out, Usage(fmt.Errorf("no configured provider records a catalogue. RunPod is the only one that does, "+
			"and it is registered only when a credential is found -- put RUNPOD_API_KEY in the environment or "+
			"runpod in %s", filepath.Join(ps.Config, "credentials.yaml")))
	}
	if len(all) == 0 {
		if len(out.Failed) > 0 {
			return out, fmt.Errorf("nothing was recorded: %w", out.Failed[0].Err)
		}
		// Every provider answered and every catalogue was empty. Not a
		// silence either: a catalogue with no types in it is a provider with
		// nothing for rent, which has never been observed and would be worth
		// knowing about.
		return out, fmt.Errorf("every catalogue came back empty, so nothing was recorded; " +
			"a provider with no types at all has not been seen before and is worth checking by hand")
	}

	files, err := store.Append(all)
	out.Files = files
	out.Samples = len(all)
	if err != nil {
		return out, err
	}
	return out, nil
}

func summariseTaken(name node.ProviderID, samples []catalogue.Sample) CatalogueTaken {
	t := CatalogueTaken{Provider: name}
	sites := map[string]bool{}
	for _, s := range samples {
		switch s.Kind {
		case catalogue.KindGPU:
			t.GPURows++
		case catalogue.KindCPU:
			t.CPURows++
		}
		if s.Datacentre == "" {
			t.NoSite++
			continue
		}
		sites[s.Datacentre] = true
	}
	t.Sites = len(sites)
	return t
}

// ReadCatalogue reads the series for a period.
//
// Half-open, as `ember spend` is: --since includes and --until excludes, so
// consecutive periods tile without a sample being counted twice.
func ReadCatalogue(cfgPath string, d Deps, since, until time.Time) (catalogue.Series, string, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return catalogue.Series{}, "", err
	}
	ps, err := ResolvePaths(cfg, d)
	if err != nil {
		return catalogue.Series{}, "", err
	}
	store, err := OpenCatalogue(ps.State)
	if err != nil {
		return catalogue.Series{}, "", err
	}
	s, err := store.Read(since, until)
	return s, store.Dir(), err
}
