package sdserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// ref is a well-formed staged reference, which is all this package knows
// about one.
//
// Built here rather than through internal/blob's own Ref, so that the format
// is written down twice and the second copy is a test: the shape is a
// contract between a browser, a staging store and this package, and a change
// to it should break something loudly rather than agree with itself.
func ref(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestBody_SendsTheImageAsBareBase64UnderUpstreamsOwnName.
//
// `init_image` AND NOT `image`, at the top of the body and not inside
// sample_params, as bare base64 and not a data URL. Every one of those is a
// way to send a field the server silently ignores, and a generation that
// ignored the image would succeed -- at the server's own default, as a plain
// text-to-image run. Measured against a live sd-server, which echoes
// `generation_info.strength` only when it took the image.
func TestBody_SendsTheImageAsBareBase64UnderUpstreamsOwnName(t *testing.T) {
	pixels := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	strength := 0.3
	body, err := Job{
		Prompt:         "a paper boat",
		InitImageBytes: pixels,
		Params:         Params{InitImage: ref("a"), Strength: &strength},
	}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := body["init_image"].(string)
	if !ok {
		t.Fatalf("no init_image in %v", body)
	}
	if got != base64.StdEncoding.EncodeToString(pixels) {
		t.Errorf("init_image is %q, want the bare base64 of the bytes", got)
	}
	// Not a data URL. Both are accepted by the server; one spelling is
	// ember's so that a captured request means one thing.
	if strings.HasPrefix(got, "data:") {
		t.Errorf("init_image carries a data URL prefix: %q", got)
	}
	// NOT THE REFERENCE. Sending the digest would be sending a name the
	// server has never heard of, and the failure would be a refused
	// generation after a card was rented.
	if strings.Contains(got, "sha256") {
		t.Errorf("init_image carries the reference rather than the bytes: %q", got)
	}
	if v, ok := body["strength"]; !ok || v != 0.3 {
		t.Errorf("strength = %v (present %v), want 0.3", v, ok)
	}
	// Strength is a top-level field, not a guidance one: the two are
	// different numbers and upstream puts them in different places.
	if sp, ok := body["sample_params"].(map[string]any); ok {
		if _, bad := sp["strength"]; bad {
			t.Errorf("strength landed inside sample_params: %v", sp)
		}
	}
}

// TestBody_AZeroStrengthIsSentAndAnUnsetOneIsNot.
//
// THE REASON Strength IS A POINTER, measured rather than inferred: a live
// sd-server echoes `generation_info.strength: 0.0` for an explicit zero and
// `0.75` when nothing was sent, so zero-is-unset would make "keep the whole
// init image" unrepresentable while looking exactly like a field that works.
// The same shape as clip_skip's -1 and eta's null, reached from a fourth
// default.
func TestBody_AZeroStrengthIsSentAndAnUnsetOneIsNot(t *testing.T) {
	zero := 0.0
	body, err := Job{
		Prompt:         "a paper boat",
		InitImageBytes: []byte{1},
		Params:         Params{InitImage: ref("a"), Strength: &zero},
	}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := body["strength"]; !ok || v != 0.0 {
		t.Errorf("strength = %v (present %v), want 0 present", v, ok)
	}

	body, err = Job{
		Prompt:         "a paper boat",
		InitImageBytes: []byte{1},
		Params:         Params{InitImage: ref("a")},
	}.Body(PreviewNone, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["strength"]; ok {
		t.Errorf("strength sent for a job that did not set it: %v", body)
	}
	// The image still is, so "no strength" is not "no image".
	if _, ok := body["init_image"]; !ok {
		t.Errorf("the image went with the strength: %v", body)
	}
}

// TestBody_RefusesAReferenceNothingResolved.
//
// THE QUIET HALF OF THE DEFECT. Omitting the field would send a plain
// text-to-image generation for a request that asked for an image input, and
// bill for it -- the operator would get a picture, and nothing anywhere would
// say the image they picked was not used. The reference is stored and the
// bytes are not, so this is the state a submitter who forgot to resolve them
// produces.
func TestBody_RefusesAReferenceNothingResolved(t *testing.T) {
	r := ref("a")
	_, err := Job{Prompt: "a paper boat", Params: Params{InitImage: r}}.Body(PreviewNone, 0)
	if err == nil {
		t.Fatal("built a body for a job whose image was never loaded")
	}
	if !strings.Contains(err.Error(), r) {
		t.Errorf("the refusal does not name the reference: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing resolved it") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// TestBody_ExtraCannotShadowTheImage.
//
// `init_image` and `strength` are ember's to set when a job names an image,
// so an `extra` that also names them is a double specification -- refused
// rather than resolved, for mergeExtra's reason: a seed resolved the wrong
// way makes two runs that claim to be the same measurement produce different
// images, and an image resolved the wrong way is the same mistake with a
// bigger payload.
func TestBody_ExtraCannotShadowTheImage(t *testing.T) {
	for _, key := range []string{"init_image", "strength"} {
		t.Run(key, func(t *testing.T) {
			strength := 0.3
			_, err := Job{
				Prompt:         "a paper boat",
				InitImageBytes: []byte{1},
				Params:         Params{InitImage: ref("a"), Strength: &strength},
				Extra:          map[string]any{key: "something else"},
			}.Body(PreviewNone, 0)
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("extra set %s and was not refused: %v", key, err)
			}
		})
	}
}

// TestValidateStatic_RefusesAnImageInputThatIsWrongOnItsFace.
//
// Each of these needs no server and would otherwise be found after a card was
// rented. The reference's shape goes through blob.ParseRef, so a path, a
// traversal or a URL in that field is refused by the same function the
// staging store looks names up with -- one spelling, so the field a job
// carries and the file the store will open cannot be two different sets.
func TestValidateStatic_RefusesAnImageInputThatIsWrongOnItsFace(t *testing.T) {
	good := ref("a")
	s := func(v float64) *float64 { return &v }

	for _, tc := range []struct {
		name string
		p    Params
		want string
	}{
		{"a path for a reference", Params{InitImage: "../../etc/passwd"}, "does not begin with"},
		{"a bare digest", Params{InitImage: strings.TrimPrefix(good, "sha256:")}, "does not begin with"},
		{"a truncated digest", Params{InitImage: good[:40]}, "characters of digest"},
		{"an upper-case digest", Params{InitImage: strings.ToUpper(good)}, "does not begin with"},
		// The measured one: accepted by the server, dropped, reported
		// nowhere.
		{"a strength with no image", Params{Strength: s(0.4)}, "no image input"},
		{"a strength above 1", Params{InitImage: good, Strength: s(1.5)}, "outside 0 to 1"},
		{"a negative strength", Params{InitImage: good, Strength: s(-0.1)}, "outside 0 to 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := Job{Prompt: "a cat", Output: "x", Params: tc.p, InitImageBytes: []byte{1}}
			err := ValidateStatic(j, PreviewNone, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// AND THE BOUNDS THEMSELVES ARE ACCEPTED. 0 and 1 are both legal to the
	// server -- measured -- and a check that refused at the limit would pass
	// every assertion above while making "keep the whole image" impossible.
	for _, v := range []float64{0, 1} {
		j := Job{Prompt: "a cat", Output: "x", InitImageBytes: []byte{1},
			Params: Params{InitImage: good, Strength: s(v)}}
		if err := ValidateStatic(j, PreviewNone, 0); err != nil {
			t.Errorf("strength %g refused: %v", v, err)
		}
	}
	// And a job with no image at all is still a job.
	if err := ValidateStatic(Job{Prompt: "a cat", Output: "x"}, PreviewNone, 0); err != nil {
		t.Errorf("a job with no image was refused: %v", err)
	}
}

// TestValidate_RefusesAnImageTheModelDoesNotTake.
//
// generation_features.init_image, which is the map derived from the loaded
// model -- the one that can say no. `features.init_image` is built per mode
// from a literal where every value starts true, so reading that one would be
// reading the map that cannot refuse.
//
// Both directions and the silence, because a gate that greyed everything
// would pass the denial half alone.
func TestValidate_RefusesAnImageTheModelDoesNotTake(t *testing.T) {
	no, yes := false, true
	caps := Capabilities{Samplers: []string{"euler"}}
	j := Job{Prompt: "a cat", InitImageBytes: []byte{1}, Params: Params{InitImage: ref("a")}}

	caps.Supported = Supported{InitImage: &no}
	err := Validate(j, caps)
	if err == nil {
		t.Fatal("a model that published init_image false accepted an image")
	}
	if !strings.Contains(err.Error(), "generation_features.init_image") {
		t.Errorf("the refusal does not name the flag it read: %v", err)
	}
	// The image and not the strength: the strength is refused earlier, and
	// naming it here would send the operator to the wrong control.
	if strings.Contains(err.Error(), "strength") {
		t.Errorf("the refusal names the strength: %v", err)
	}

	caps.Supported = Supported{InitImage: &yes}
	if err := Validate(j, caps); err != nil {
		t.Errorf("a model that published init_image true refused an image: %v", err)
	}

	// A build that published no such flag gates nothing: nil is "the server
	// said nothing" and false is "the server said no", and collapsing them
	// would make the picker unusable against every build that does not
	// publish the map.
	caps.Supported = Supported{}
	if err := Validate(j, caps); err != nil {
		t.Errorf("a silent server refused an image: %v", err)
	}

	// And a denial refuses nothing that is not being sent.
	caps.Supported = Supported{InitImage: &no}
	if err := Validate(Job{Prompt: "a cat"}, caps); err != nil {
		t.Errorf("a job with no image was refused by the image gate: %v", err)
	}
}

// TestCapabilities_ReadsTheImageFlagFromTheModelsOwnMap.
//
// Against the two captures: a real server with weights answers true for every
// generation feature, and a real server with none answers false for every one
// of them. So both directions come off bytes a server sent rather than off a
// hand-built struct.
func TestCapabilities_ReadsTheImageFlagFromTheModelsOwnMap(t *testing.T) {
	caps := testCaps(t)
	if caps.Supported.InitImage == nil {
		t.Fatal("a capture with a model published no init_image flag")
	}
	if !*caps.Supported.InitImage {
		t.Error("a capture with a model says it takes no image input")
	}

	none := capsFrom(t, "capabilities_no_model.json")
	if none.Supported.InitImage == nil {
		t.Fatal("the weightless capture published no init_image flag")
	}
	if *none.Supported.InitImage {
		t.Error("a server with no weights says it takes an image input")
	}
}
