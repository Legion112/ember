package webapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Legion112/ember/internal/genstore"
)

// imageRefs decodes the artifacts a request view publishes.
func imageRefs(t *testing.T, body []byte) []struct {
	Path    string `json:"path"`
	Media   string `json:"media"`
	Preview bool   `json:"preview"`
} {
	t.Helper()
	var v struct {
		Images []struct {
			Path    string `json:"path"`
			Media   string `json:"media"`
			Preview bool   `json:"preview"`
		} `json:"images"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode the request view: %v\n%s", err, body)
	}
	return v.Images
}

// TestArtifact_TheViewDeclaresWhatTheRouteWillSend is the join, and the join is
// the whole assertion.
//
// A page cannot read an artifact's extension: `imageRef.Path` has none, by
// design, because the route is `/images/{job}/{index}` and the blob key is this
// process's business. So the page is TOLD, and the thing worth proving is that
// what it is told is what it will be sent -- not that both sides happen to say
// `audio/wav` today. Compared against the header rather than against a literal
// for exactly that reason: a second spelling of the answer in this test is a
// second place for it to drift, and the browser's element choice and the bytes
// it then fetches would disagree with nobody reporting it.
//
// BOTH KINDS IN ONE REQUEST, which no real rental produces -- `kind` is on the
// request so one rental runs one driver -- and which is the point here: it is
// the only arrangement in which a `media` keyed off the REQUEST's kind, the
// obvious alternative to this field, gives a different answer from the route.
// One of the two rows would be wrong, and a test with one row each could not
// see it.
func TestArtifact_TheViewDeclaresWhatTheRouteWillSend(t *testing.T) {
	h := newHarness(t)
	srv := live(t, h)
	id := h.wireID(t, srv)

	png := []byte("\x89PNG\r\n\x1a\n" + "the rest is not read")
	wav := []byte("RIFF$\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00")
	for _, a := range []struct {
		key   string
		bytes []byte
		job   int
	}{
		{id + "/cat-00.png", png, 1},
		{id + "/hello-00.wav", wav, 2},
	} {
		if err := h.blobs.Put(t.Context(), a.key, a.bytes); err != nil {
			t.Fatal(err)
		}
		if err := h.store.AddImage(t.Context(), genstore.Image{
			Req: genstore.ReqID(id), Job: a.job, Index: 0,
			Key: a.key, Bytes: int64(len(a.bytes)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	view := h.wire(t, srv, "GET", "/api/requests/"+id, "", nil)
	if view.Status != http.StatusOK {
		t.Fatalf("GET the request: %d %s", view.Status, view.Body)
	}
	ims := imageRefs(t, view.Body)
	if len(ims) != 2 {
		t.Fatalf("%d artifact(s) on the view, want 2:\n%s", len(ims), view.Body)
	}

	// Not a hardcoded pair: whatever the view said, fetching it has to agree.
	for _, im := range ims {
		if im.Media == "" {
			t.Errorf("%s arrives with no declared type, so a page has nothing to choose "+
				"an element from", im.Path)
			continue
		}
		got := h.wire(t, srv, "GET", im.Path, "", nil)
		if got.Status != http.StatusOK {
			t.Errorf("GET %s: %d %s", im.Path, got.Status, got.Body)
			continue
		}
		if ct := got.Header.Get("Content-Type"); ct != im.Media {
			t.Errorf("the view says %s is %q and the route sends %q; a page choosing an "+
				"element from the first would render it over bytes of the second",
				im.Path, im.Media, ct)
		}
	}

	// AND THE TWO ARE DIFFERENT, which is what stops the loop above passing
	// over a view and a route that both answer one type for everything.
	if ims[0].Media == ims[1].Media {
		t.Errorf("both artifacts are declared %q; a png and a wav are not the same type",
			ims[0].Media)
	}
}

// TestArtifact_AnUntypedOneIsNamedAndNotRenderedAsAnImage.
//
// THE PROJECTION DROPS THE ERROR AND THE FIELD GOES EMPTY, and that is the one
// place this projection may not refuse: a request view that failed because one
// of twenty artifacts has an extension the switch does not know would take the
// other nineteen with it -- the prompts, the cost and the reason it ended.
//
// The rest of the view has to survive, so that is asserted beside the empty
// field rather than inferred from it: the healthy png is still there with its
// own type. This is the `Rows` rule one envelope over -- one bad row is not the
// whole list -- reached through a media type instead of an i32.
func TestArtifact_AnUntypedOneIsNamedAndNotRenderedAsAnImage(t *testing.T) {
	h := newHarness(t)
	srv := live(t, h)
	id := h.wireID(t, srv)

	for _, a := range []struct {
		key string
		job int
	}{
		{id + "/cat-00.png", 1},
		{id + "/mystery.bin", 2},
	} {
		if err := h.blobs.Put(t.Context(), a.key, []byte("bytes")); err != nil {
			t.Fatal(err)
		}
		if err := h.store.AddImage(t.Context(), genstore.Image{
			Req: genstore.ReqID(id), Job: a.job, Index: 0, Key: a.key, Bytes: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}

	view := h.wire(t, srv, "GET", "/api/requests/"+id, "", nil)
	if view.Status != http.StatusOK {
		t.Fatalf("GET the request: %d %s", view.Status, view.Body)
	}
	ims := imageRefs(t, view.Body)
	if len(ims) != 2 {
		t.Fatalf("%d artifact(s), want both:\n%s", len(ims), view.Body)
	}
	if ims[0].Media != "image/png" {
		t.Errorf("the healthy artifact is declared %q; one row ember cannot type must not "+
			"cost the view of the rest", ims[0].Media)
	}
	if ims[1].Media != "" {
		t.Errorf("media = %q for an extension artifactType has no answer for, want empty: "+
			"the route 500s for this file, so a page must not be told to render it",
			ims[1].Media)
	}
}

// TestConfig_AlwaysPublishesAProviderIsThePagesAnsweredDiscriminator is the
// invariant the PAGE's "has /api/config answered yet" test rests on.
//
// IT USED TO BE `kinds` AND THAT IS THE WHOLE POINT OF THIS TEST CHANGING.
// `model::Config` read an empty kind list as "ember has not said yet" and NOT
// as "this process runs nothing", which was only sound while no real response
// could carry an empty list -- `genstore.Kinds()` guaranteed that by being a
// literal in ember's source. The previous version of this test said in its own
// comment that a projection which filtered the kinds by what the config can
// run "would make a configuration with no `sd:` and no `tts:` publish `[]`,
// and the page would sit saying it was still waiting". That projection is
// exactly what #200 asks for and what `workloads` now is, so the
// discriminator had to move before the list could be filtered.
//
// `providers` IS THE ONE FIELD NO REAL ANSWER CAN EMPTY: the registry always
// holds the local provider, which needs no credential -- so an empty
// `providers` means the page has not been answered and an empty `workloads`
// means this server can be asked for nothing, which `Config::unrunnable`
// prints. Asserted over the configuration most likely to tempt somebody into
// filtering: the bare harness, no voices, nothing to serve once `sd.serve` is
// gone.
func TestConfig_AlwaysPublishesAProviderIsThePagesAnsweredDiscriminator(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.SD.Serve = nil })
	srv := live(t, h)

	res := h.wire(t, srv, "GET", "/api/config", "", nil)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Status, res.Body)
	}
	var v struct {
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
		Workloads []struct {
			ID string `json:"id"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal(res.Body, &v); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Body)
	}
	if len(v.Providers) == 0 {
		t.Fatalf("providers is empty, which the page reads as \"/api/config has not answered\" "+
			"-- so it would wait for ever:\n%s", res.Body)
	}
	// AND THE OTHER HALF, because this test is worth nothing if the state it
	// is about cannot be reached: a config with nothing servable publishes no
	// workload, which is the answer the page has to be able to tell from a
	// silence.
	if len(v.Workloads) != 0 {
		t.Errorf("workloads = %+v, want none for a config with nothing to serve", v.Workloads)
	}
}
