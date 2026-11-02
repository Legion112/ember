package runpod

import "testing"

// cudaCatalogue decodes the one field gpuType deliberately does not.
//
// Declared here and not in types.go on purpose: nothing in ember reads
// cudaVersions, and adding a production field so a test could assert on it
// would be a wire type shaped by a test's convenience -- the reason
// sshx.Runner never grew stdin support. What is being pinned is a property of
// the captured response, so the struct belongs beside the assertion.
type cudaCatalogue struct {
	Gpus []struct {
		ID           string `json:"id"`
		Availability string `json:"availability"`
		DataCenters  []struct {
			ID           string `json:"id"`
			Availability string `json:"availability"`
		} `json:"dataCenters"`
		CudaVersions []struct {
			Version   string `json:"version"`
			Available bool   `json:"available"`
		} `json:"cudaVersions"`
	} `json:"gpus"`
}

// TestCatalogue_ReportsCudaVersionsAndTheyAreNotAFloorForMinCUDA.
//
// THE CATALOGUE DOES RETURN cudaVersions, and three places in this repository
// said it does not -- runpod.go, config.go's `min_cuda` doc and README. The
// claim was written on 2026-10-31 and the live capture carrying the field has
// been committed since 2026-10-29, four directories away: the schema was
// consulted where the response was already in the tree. That is
// `allowedCudaVersions` a third time with the roles reversed, and it is why
// this is an assertion rather than a corrected sentence -- a comment that
// justifies the entry somebody remembered cannot catch the entry somebody
// forgets.
//
// What the field cannot do is enforce `min_cuda`, which is #91's question, and
// the measurement is the capture itself. `available` tracks the card's
// top-level availability summary across all four captured rows -- LOW gives
// every version true, NONE gives every version false, and the AMD card
// reports none at all -- so it is a capacity signal and not a per-version
// capability. Four rows is a correlation rather than a semantics, and the
// decision does not rest on it: whichever reading is right, both are
// refusals.
//
//   - Built on `available`, a floor refuses the RTX 4090 asserted below --
//     NONE overall, LOW in US-IL-1, rentable, and every one of its eight CUDA
//     versions false -- for EVERY value of min_cuda, including ones the card
//     satisfies. That is the bug `hasCapacity` and `regionOffers` exist to
//     prevent, reached through a different field.
//   - Built on `version` alone it describes the catalogue and not the host the
//     pod lands on, and the field's own description in the live OpenAPI
//     document says those versions are "suitable for gpu.allowedCudaVersions
//     on pod create" -- the property POST /v2/pods answers 422 for. So a
//     believed answer could not be acted on at the only place it matters.
//
// Structurally too: cudaVersions is on GpuType and not on dataCenters[], so
// it cannot be re-read through a store's own site the way regionOffers has to.
//
// If a later capture reports `available: true` for a card with site capacity,
// THIS TEST IS SUPPOSED TO FAIL: the correlation would be broken, and #91's
// floor question is reopened on evidence rather than closed by a comment.
func TestCatalogue_ReportsCudaVersionsAndTheyAreNotAFloorForMinCUDA(t *testing.T) {
	cat := fixture[cudaCatalogue](t, "gpus.json")
	if len(cat.Gpus) == 0 {
		t.Fatal("the captured catalogue decoded no gpus")
	}

	// Half one: the field is there. Without this the two halves below could
	// both pass against a response that carries nothing, which is the shape
	// of assertion this repository keeps finding.
	withVersions := 0
	for _, g := range cat.Gpus {
		if len(g.CudaVersions) > 0 {
			withVersions++
		}
	}
	if withVersions == 0 {
		t.Fatal("no entry in the captured catalogue carries cudaVersions; " +
			"three comments once said the live catalogue does not return it and this " +
			"fixture is what disproved them, so an empty answer here means the fixture " +
			"was replaced and those comments need re-deciding rather than re-wording")
	}

	// Half two: the counterexample. A card ember's own filter calls rentable,
	// every one of whose CUDA versions reads unavailable.
	gs := fixture[listGpuTypesResponse](t, "gpus.json").Gpus
	rentable := map[string]bool{}
	for _, g := range gs {
		rentable[g.ID] = hasCapacity(g)
	}

	found := 0
	for _, g := range cat.Gpus {
		if !rentable[g.ID] || len(g.CudaVersions) == 0 {
			continue
		}
		site := ""
		for _, dc := range g.DataCenters {
			if dc.Availability != "" && dc.Availability != availabilityNone {
				site = dc.ID
			}
		}
		if g.Availability != availabilityNone || site == "" {
			continue
		}
		found++
		for _, cv := range g.CudaVersions {
			if cv.Available {
				t.Errorf("%s reads %s overall, has capacity in %s, and reports CUDA %s as available: "+
					"the correlation between `available` and the availability summary is broken, so #91's "+
					"question -- whether min_cuda can be compared against an observed value -- is reopened",
					g.ID, g.Availability, site, cv.Version)
			}
		}
	}
	if found == 0 {
		t.Fatal("the captured catalogue no longer holds a card that is NONE overall, has site " +
			"capacity and reports cudaVersions; that row is the whole evidence that a min_cuda " +
			"floor built on `available` would refuse a rentable card, so #91 needs re-measuring")
	}
}
