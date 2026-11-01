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
	})
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
