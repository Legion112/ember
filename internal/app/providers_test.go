package app

import (
	"slices"
	"testing"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/node"
)

// TestProviders_AreTheOnesBuildRegistryCanRegister holds the two lists of
// provider names against each other, in both directions.
//
// TWO LISTS AND NOT ONE, WHICH IS A COST AND NOT A DESIGN -- the shape
// `TestDrivers_AreTheKindsTheStoreKnows` already has, one axis over.
// `local.Name` and `runpod.Name` are the names, and `internal/config` cannot
// read them: `local.New` takes a `dockerengine.API`, so importing that package
// would put a Docker client into the dependency graph of the package every
// command starts by loading a file with, and a provider that reads ember.yaml
// is the worse direction. The registry cannot be read either, because it is
// built per run from whatever credentials were found and an entry's
// `providers:` is validated at LOAD -- validating against the registry would
// mean a file refused on a machine with no RunPod token and accepted on one
// with it.
//
// SO IT IS CHECKED RATHER THAN DERIVED. HERE, because this is the package
// where a provider name becomes a registered provider: `BuildRegistry` is the
// one function that puts them in a registry, so it is the one place that
// already imports both.
//
// BOTH DIRECTIONS, because each is a different defect. A name
// `config.Providers()` has and the registry cannot produce is a
// `providers: [vastai]` an operator writes, is accepted at load, and silently
// matches nothing -- a tab whose every provider is disabled. A provider ember
// can register and `config.Providers()` does not name is worse: naming it in
// `providers:` is a load refusal, so an operator cannot restrict a workload to
// the provider they actually rent from.
//
// THROUGH BuildRegistry AND NOT OFF THE TWO CONSTANTS, because what the
// allow-list has to agree with is the set of providers a run can hold, and
// that is this function's answer rather than the packages'. A credential is
// supplied so that the RunPod half is registered; nothing is dialled, because
// `runpod.New` builds a client and asks it nothing.
func TestProviders_AreTheOnesBuildRegistryCanRegister(t *testing.T) {
	reg, err := BuildRegistry(
		Deps{Engine: dockerengine.NewRecordingEngine()},
		config.Credentials{RunPod: node.NewSecret([]byte("not-a-real-key"))},
		config.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	declarable := config.Providers()
	registered := reg.Names()

	if len(declarable) == 0 || len(registered) == 0 {
		t.Fatalf("one of the lists is empty: declarable = %v, registered = %v",
			declarable, registered)
	}
	for _, p := range declarable {
		if !slices.Contains(registered, p) {
			t.Errorf("config.Providers() names %q and BuildRegistry cannot register it; a "+
				"workload restricted to that provider would match nothing and its tab would "+
				"offer no usable option", p)
		}
	}
	for _, p := range registered {
		if !slices.Contains(declarable, p) {
			t.Errorf("BuildRegistry registers %q and config.Providers() does not name it; an "+
				"operator cannot restrict a workload to a provider they do rent from, because "+
				"naming it in providers: is refused at load", p)
		}
	}
}
