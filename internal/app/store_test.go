package app

import (
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/providertest"
)

func TestAttachStore_RefusesAProviderWithNoStorage(t *testing.T) {
	// A config that names a store against a provider that has none. Caught
	// before anything is rented rather than as a nil type assertion when the
	// run is about to start.
	p := providertest.NewRecordingProvider("plain")
	_, err := AttachStore(t.Context(), p, config.Config{
		Provider: "plain",
		Store:    config.StoreConfig{Name: "models"},
	}, true)
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), "no persistent storage") {
		t.Errorf("err = %q", err)
	}
}

func TestAttached_IsZero(t *testing.T) {
	if !(Attached{}).IsZero() {
		t.Error("the zero Attached is not zero")
	}
	if (Attached{Handle: node.StoreHandle{Provider: "p", ID: "v"}}).IsZero() {
		t.Error("a populated Attached reported zero")
	}
}

// TestAttachStore_MakesAStoreNothingBillsFor.
//
// AN OPERATOR MET THE REFUSAL ON THE LOCAL PROVIDER, where a store is a
// Docker named volume. The sentence they got was "a store bills by the month
// and nothing destroys it on a deadline" -- which is true of a RunPod network
// volume and false of the thing they were being refused, so the reason given
// was about a different provider and what remained was friction protecting
// them from a charge that does not exist.
//
// THE SIZE AND THE SITE ARE STILL THEIRS. Creating one does not invent its
// shape -- the name, the size and the region come from the file -- so this
// makes the store the configuration already described rather than guessing at
// one.
func TestAttachStore_MakesAStoreNothingBillsFor(t *testing.T) {
	p := providertest.WithStores("local")
	p.Capabilities.StoresFree = true

	att, err := AttachStore(t.Context(), p, config.Config{
		Provider: "local",
		Store: config.StoreConfig{
			Name: "models", SizeGiB: 70, Region: "here", Mount: "/models",
		},
	}, true)
	if err != nil {
		t.Fatalf("refused to make a store nothing bills for: %v", err)
	}
	if att.Handle.IsZero() {
		t.Fatal("attached a zero handle, so nothing would be mounted")
	}
	// The fake starts with no stores, so anything here it made.
	made := p.Stores
	if len(made) != 1 {
		t.Fatalf("created %d store(s), want 1: %+v", len(made), made)
	}
	if made[0].Name != "models" || made[0].SizeGiB != 70 || made[0].Region != "here" {
		t.Errorf("created %+v, want the name, size and region from the file", made[0])
	}
}

// TestAttachStore_StillRefusesAStoreThatBills.
//
// THE SEAL SURVIVES WHERE IT MEANS SOMETHING, which is the half that makes the
// change above safe. A store at a paid provider bills by the gibibyte-month
// and no deadline, reaper or Ctrl-C ends it -- it is the one charge in this
// whole system that outlives every mechanism ember has for stopping a charge.
// So a config that merely MENTIONS a name must not bring one into existence,
// and the refusal has to keep naming the command that does.
func TestAttachStore_StillRefusesAStoreThatBills(t *testing.T) {
	p := providertest.WithStores("runpod")
	if p.Capabilities.StoresFree {
		t.Fatal("the fake claims free storage, so this case tests nothing")
	}

	_, err := AttachStore(t.Context(), p, config.Config{
		Provider: "runpod",
		Store:    config.StoreConfig{Name: "flux2", SizeGiB: 70, Mount: "/models"},
	}, true)
	if err == nil {
		t.Fatal("brought a standing monthly charge into existence because a config named one")
	}
	if !strings.Contains(err.Error(), "ember store create") {
		t.Errorf("the refusal does not name the command that makes one: %v", err)
	}
	if len(p.Stores) != 0 {
		t.Errorf("created %+v at a provider that bills for storage", p.Stores)
	}
}

// TestAttachStore_ADryRunMakesNothing.
//
// `ember up --dry-run` promises in its own flag help to "say what would be
// rented and create nothing", and a web request asking what a run would cost
// needs the same answer without changing anything. `Plan`'s own doc says
// `--dry-run` is the half where "no node exists, nothing is billing" -- so a
// `docker volume create` on that path is a side effect in a command that
// promised none, even though the volume is free.
//
// FOUND BY A 90-SECOND TEST. Making AttachStore create unconditionally turned
// TestUp_RefusesWhenTheNamedStoreDoesNotExist from an instant refusal into a
// full fake rental that waited out a readiness timeout -- which is how the
// contract break announced itself, since the dry-run store test pre-creates
// its store and would never have noticed.
func TestAttachStore_ADryRunMakesNothing(t *testing.T) {
	p := providertest.WithStores("local")
	p.Capabilities.StoresFree = true

	_, err := AttachStore(t.Context(), p, config.Config{
		Provider: "local",
		Store:    config.StoreConfig{Name: "models", SizeGiB: 70, Mount: "/models"},
	}, false)
	if err == nil {
		t.Fatal("a plan that may not create anything made a store")
	}
	if len(p.Stores) != 0 {
		t.Errorf("created %+v on a path that promised to change nothing", p.Stores)
	}
	if !strings.Contains(err.Error(), "ember store create") {
		t.Errorf("the refusal does not name the command that makes one: %v", err)
	}
}
