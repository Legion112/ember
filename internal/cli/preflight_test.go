package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/awssig"
)

// The pre-flight tests all drive `ember up` against a RunPod store whose API
// fake serves the volume listing and NOTHING ELSE -- no catalogue route, so
// provision.PickOffer cannot succeed.
//
// THAT IS THE ASSERTION AND NOT AN ACCIDENT OF THE HARNESS. The whole claim
// of this change is that the question is asked before anything commits to
// hardware, and a fake that cannot answer "what cards do you have" is the
// cheapest way to prove it: a refusal naming the weights means the check ran
// first, and a refusal naming offers means it did not run at all. So every
// test below reads which of the two came back, and the full-store case is the
// negative control -- without it, a check that refused every run would pass
// the missing-asset tests exactly as happily.
const (
	// wantsSync is what a refused run must say, and it names the remedy
	// rather than the problem.
	wantsSync = "ember store sync"

	// reachedOffers is the error from the fake's missing catalogue route:
	// what a run that got PAST the weights fails with, and the only way to
	// tell "the check passed this run" from "the check was not there".
	reachedOffers = "list offers from runpod"
)

func storeWithAssets(t *testing.T, objects map[string]int64) (deps, string) {
	t.Helper()
	f := &fakeStore{objects: objects}
	return f.deps(t, "store:\n"+
		"  name: models\n"+
		"  size_gib: 40\n"+
		"  region: EU-RO-1\n"+
		"  mount: /models\n"+
		"assets:\n"+
		"  - url: https://example.invalid/weights.gguf\n"+
		"    sha256: 09d005300dd8dcbbd489bb75ada6254145c84c2c9c3d7cc1829e3c5dedcb42ce\n"+
		"    as: weights.gguf\n"+
		"verify: always\n")
}

// TestUp_RefusesAStoreMissingTheWeightsBeforeChoosingAnOffer is the defect
// this whole change is about, as a refusal.
//
// Before this existed the run rented the card and then spent it downloading:
// measured on 2026-11-03, 14m47s of an RTX PRO 6000 at $2.49/hr to pull
// 56.16 GiB, 97.9% of the rental, with the GPU at 0% throughout.
func TestUp_RefusesAStoreMissingTheWeightsBeforeChoosingAnOffer(t *testing.T) {
	d, cfg := storeWithAssets(t, map[string]int64{})

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		t.Fatal("a run whose weights are not on the store was planned without a word")
	}
	if !strings.Contains(err.Error(), wantsSync) {
		t.Errorf("the refusal must name the cheap remedy, got %v", err)
	}
	if !strings.Contains(err.Error(), "weights.gguf") {
		t.Errorf("the refusal must name the asset, got %v", err)
	}
	// And it never got as far as asking what cards exist.
	if strings.Contains(err.Error(), reachedOffers) {
		t.Errorf("the check must run before PickOffer, but the run reached it: %v", err)
	}
}

// TestUp_AStoreWithTheWeightsIsNotRefused is the negative control.
//
// Without it every assertion above is consistent with a check that refuses
// every run there is -- the shape AssertNoStray sat in for the life of the
// lab suite. This one must get PAST the weights and fail on the fake's
// missing catalogue instead.
func TestUp_AStoreWithTheWeightsIsNotRefused(t *testing.T) {
	d, cfg := storeWithAssets(t, map[string]int64{"weights.gguf": 60306914804})

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		// The fake has no catalogue, so this cannot be reached; if it ever
		// is, the run got further than the harness can account for.
		t.Fatal("the run planned against a fake with no catalogue route")
	}
	if strings.Contains(err.Error(), wantsSync) {
		t.Fatalf("a store holding every asset was refused for missing them: %v", err)
	}
	// Positively: it reached the catalogue. Asserting only "not the sync
	// refusal" would pass for a run that failed the check some other way,
	// which is how this test read until a reinstatement showed it could not
	// tell the two apart.
	if !strings.Contains(err.Error(), reachedOffers) {
		t.Fatalf("a full store did not get as far as choosing an offer: %v", err)
	}
}

// TestUp_AnEmptyObjectIsNotTheWeights: a PUT that failed after creating the
// key leaves a zero-length object, and under `verify: present` the node finds
// it, calls it there and loads nothing.
func TestUp_AnEmptyObjectIsNotTheWeights(t *testing.T) {
	d, cfg := storeWithAssets(t, map[string]int64{"weights.gguf": 0})

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil || !strings.Contains(err.Error(), wantsSync) {
		t.Fatalf("an asset on the store at 0 bytes is not an asset, got %v", err)
	}
}

// TestUp_WithoutS3CredentialsTheRunIsNotRefused.
//
// RunPod issues S3 keys separately from the API key and an account can have
// one and not the other, so an operator who has never made them runs today
// and has to keep running. "I could not ask" is not "it is not there", and
// here the honest answer is to carry on and say so rather than to refuse --
// refusing would break working setups in order to save money on broken ones.
func TestUp_WithoutS3CredentialsTheRunIsNotRefused(t *testing.T) {
	d, cfg := storeWithAssets(t, map[string]int64{})
	d.Creds.S3 = awssig.Credentials{}

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		t.Fatal("the run planned against a fake with no catalogue route")
	}
	// It must reach the catalogue, which is to say it must get past the
	// store entirely. "Not the sync refusal" is too weak: with the skip
	// removed, the run fails on an unsigned S3 call instead, which is a
	// different sentence and the same stopped run. Found by reinstating it.
	if !strings.Contains(err.Error(), reachedOffers) {
		t.Fatalf("a run was stopped by a check it could not make: %v", err)
	}
}
