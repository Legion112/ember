package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/statestore"
)

func TestParseHandle(t *testing.T) {
	h, err := parseHandle("local/abc123")
	if err != nil {
		t.Fatal(err)
	}
	if h.Provider != "local" || h.ID != "abc123" {
		t.Fatalf("got %+v", h)
	}

	// A handle with no provider would be dispatched to whichever provider
	// happened to be first in the registry, and a destroy aimed at the wrong
	// provider either does nothing or destroys something else.
	for _, bad := range []string{"", "abc123", "/abc123", "local/", "  ", "local/a/b"} {
		if _, err := parseHandle(bad); err == nil {
			t.Errorf("parseHandle(%q) was accepted", bad)
		}
	}
}

func TestParseHandle_TolerantOfSurroundingSpace(t *testing.T) {
	// Copied out of ls output, which is column-aligned.
	h, err := parseHandle("  local/abc123  ")
	if err != nil || h.ID != "abc123" {
		t.Fatalf("got %+v, %v", h, err)
	}
}

func TestDown_RefusesTheWrongNumberOfArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"local/a", "local/b"}} {
		var out bytes.Buffer
		if err := runDown(args, &out, labDeps(t)); ExitCode(err) != ExitUsage {
			t.Errorf("runDown(%v) = %v, want a usage error", args, err)
		}
	}
}

func TestDown_RefusesAnUnknownProvider(t *testing.T) {
	var out bytes.Buffer
	err := runDown([]string{"runpod/abc"}, &out, labDeps(t))
	if ExitCode(err) != ExitUsage {
		t.Fatalf("got %v, want a usage error", err)
	}
}

// TestDown_ANodeThatIsAlreadyGoneIsASuccess. The goal of destroy is that the
// node is gone, and it is. Reporting failure here would teach an operator to
// ignore the one command whose failures matter.
func TestDown_ANodeThatIsAlreadyGoneIsASuccess(t *testing.T) {
	var out bytes.Buffer
	if err := runDown([]string{"local/nosuchcontainer"}, &out, labDeps(t)); err != nil {
		t.Fatalf("destroying an absent node failed: %v", err)
	}
	if !strings.Contains(out.String(), "destroyed") {
		t.Errorf("got %q", out.String())
	}
}

// TestDown_DestroysARunningNodeInsideItsDeadline is the case reap deliberately
// will not touch. reap is bound by the deadline because it runs unattended; a
// person naming a node is not guessing.
func TestDown_DestroysARunningNodeInsideItsDeadline(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	id, err := e.ContainerCreate(t.Context(), dockerengine.ContainerSpec{
		Name:  "ember-node-live",
		Image: local.DefaultImage,
		Labels: map[string]string{
			dockerengine.LabelRunID: "0123456789abcdef",
			local.LabelKind:         "node",
			// Deadline far in the future: reap would leave this alone.
			local.LabelLabel: "ember-0123456789abcdef-99999999999",
			local.LabelUser:  "root",
		},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ContainerStart(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runDown([]string{"local/" + id}, &out, d); err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(e.Containers()) != 0 {
		t.Error("the node survived ember down")
	}
}

func TestDown_ReportsABadFlag(t *testing.T) {
	var out bytes.Buffer
	if err := runDown([]string{"--nope", "local/a"}, &out, labDeps(t)); ExitCode(err) != ExitUsage {
		t.Fatalf("got %v", err)
	}
}

func TestDown_HelpExplainsHowItDiffersFromReap(t *testing.T) {
	var out bytes.Buffer
	_ = runDown([]string{"-h"}, &out, labDeps(t))
	if !strings.Contains(out.String(), "ignores the") {
		t.Errorf("help should say it ignores the deadline: %q", out.String())
	}
}

func TestOffers_ListsTheLocalOffer(t *testing.T) {
	var out bytes.Buffer
	if err := runOffers(nil, &out, labDeps(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "local") {
		t.Errorf("got %q", out.String())
	}
}

func TestOffers_SaysSoWhenNothingMatches(t *testing.T) {
	var out bytes.Buffer
	if err := runOffers([]string{"--gpu", "h100"}, &out, labDeps(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no offers match") {
		t.Errorf("got %q", out.String())
	}
}

// TestOffers_AProviderThatCouldNotBeAskedExitsTwo. "I could not ask RunPod"
// must not render as "RunPod has nothing".
func TestOffers_AProviderThatCouldNotBeAskedExitsTwo(t *testing.T) {
	d := labDeps(t)
	d.Engine.(*dockerengine.RecordingEngine).FailOn = "Ping"

	var out bytes.Buffer
	err := runOffers(nil, &out, d)
	if ExitCode(err) != ExitFindings {
		t.Fatalf("exit = %d, want %d (%v)", ExitCode(err), ExitFindings, err)
	}
	if !strings.Contains(out.String(), "could not be asked") {
		t.Errorf("got %q", out.String())
	}
}

func TestOffers_ReportsABadFlag(t *testing.T) {
	var out bytes.Buffer
	if err := runOffers([]string{"--nope"}, &out, labDeps(t)); ExitCode(err) != ExitUsage {
		t.Fatalf("got %v", err)
	}
}

func TestOffers_RefusesAnUnknownProvider(t *testing.T) {
	var out bytes.Buffer
	if err := runOffers([]string{"--provider", "runpod"}, &out, labDeps(t)); ExitCode(err) != ExitUsage {
		t.Fatalf("got %v", err)
	}
}

func TestOffers_HelpSaysPricesAreNotACommitment(t *testing.T) {
	var out bytes.Buffer
	_ = runOffers([]string{"-h"}, &out, labDeps(t))
	if !strings.Contains(out.String(), "not a commitment") {
		t.Errorf("got %q", out.String())
	}
}

// TestDown_ClearsTheJournalRecordAfterTheProviderConfirms.
//
// The order matters: the record is forgotten only once the node is gone. A
// record cleared first is a node nothing will ever look for again, and a
// record left behind makes every later `ember ls` report an orphan that is not
// there -- which teaches an operator to ignore the output.
func TestDown_ClearsTheJournalRecordAfterTheProviderConfirms(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	id, err := e.ContainerCreate(t.Context(), dockerengine.ContainerSpec{
		Name:  "ember-node-recorded",
		Image: local.DefaultImage,
		Labels: map[string]string{
			dockerengine.LabelRunID: "0123456789abcdef",
			local.LabelKind:         "node",
			local.LabelLabel:        "ember-0123456789abcdef-99999999999",
			local.LabelUser:         "root",
		},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ContainerStart(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	store, err := openStore(filepath.Join(d.HomeDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	rec := statestore.Record{
		RunID: "0123456789abcdef", Provider: local.Name, NodeID: id,
		Phase: statestore.PhaseReady, Label: "ember-0123456789abcdef-99999999999",
		Created: time.Now(), Deadline: time.Now().Add(time.Hour),
	}
	if err := store.Create(rec); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runDown([]string{"local/" + id}, &out, d); err != nil {
		t.Fatalf("down: %v", err)
	}

	recs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Errorf("the record outlived the node: %+v", recs)
	}

	// And ls agrees there is nothing left to report.
	out.Reset()
	if err := runLs(nil, &out, d); err != nil {
		t.Errorf("ls after down reports findings: %v\n%s", err, out.String())
	}
}

// TestDown_LeavesAnUnrelatedRecordAlone. recordFor matches on the handle, not
// on being the only record: destroying one node must not clear another node's
// journal entry, which would be a leak nothing ever reports again.
func TestDown_LeavesAnUnrelatedRecordAlone(t *testing.T) {
	d := labDeps(t)

	store, err := openStore(filepath.Join(d.HomeDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	other := statestore.Record{
		RunID: "fedcba9876543210", Provider: local.Name, NodeID: "someothernode",
		Phase: statestore.PhaseReady, Label: "ember-fedcba9876543210-99999999999",
		Created: time.Now(), Deadline: time.Now().Add(time.Hour),
	}
	if err := store.Create(other); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runDown([]string{"local/nosuchcontainer"}, &out, d); err != nil {
		t.Fatalf("down: %v", err)
	}

	recs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].RunID != other.RunID {
		t.Errorf("an unrelated record was cleared: %+v", recs)
	}
}
