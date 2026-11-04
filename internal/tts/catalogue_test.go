package tts

import (
	"reflect"
	"strings"
	"testing"
)

func cat() Catalogue {
	return Catalogue{
		{ID: "lessac", Name: "Lessac (en_US)", Voice: "/opt/voices/lessac.onnx", EspeakData: "/e"},
		{ID: "thorsten", Name: "Thorsten (de_DE)", Voice: "/opt/voices/thorsten.onnx", EspeakData: "/e"},
	}
}

func TestCatalogue_LookupResolvesAnIdAndNamesWhatIsThere(t *testing.T) {
	v, err := cat().Lookup("thorsten")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if v.Voice != "/opt/voices/thorsten.onnx" {
		t.Errorf("voice = %q", v.Voice)
	}

	// THE REFUSAL LISTS WHAT IS PUBLISHED, because the one realistic way to
	// reach it is a stale page: the UI loaded the list, the operator edited
	// the config, and the id no longer exists. "unknown voice" with no list
	// sends them to read a file they cannot see from a browser.
	_, err = cat().Lookup("nope")
	if err == nil {
		t.Fatal("an unknown id was resolved")
	}
	for _, want := range []string{"nope", "lessac", "thorsten"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestCatalogue_AnEmptyCatalogueIsItsOwnSentence. An operator who has not
// configured speech asked for something this process cannot do; one who has,
// and typed an id wrong, needs the list. Naming `tts.voices` is the difference
// between a refusal they can act on and one they read as a bug.
func TestCatalogue_AnEmptyCatalogueIsItsOwnSentence(t *testing.T) {
	_, err := Catalogue(nil).Lookup("lessac")
	if err == nil {
		t.Fatal("an empty catalogue resolved an id")
	}
	if !strings.Contains(err.Error(), "tts.voices") {
		t.Errorf("error = %q, want it to name tts.voices", err)
	}
	// And it must NOT read as a typo, which is the other half of the split.
	if strings.Contains(err.Error(), "no voice \"lessac\"") {
		t.Errorf("error = %q, want it to say the catalogue is empty rather than that the id is wrong", err)
	}
}

func TestCatalogue_ValidateAcceptsAGoodListAndRefusesEachWayItCanBeWrong(t *testing.T) {
	if err := cat().Validate(); err != nil {
		t.Fatalf("a good catalogue was refused: %v", err)
	}
	cases := []struct {
		name string
		cat  Catalogue
		want string
	}{
		{
			// The symptom of this one is that one of two voices can never be
			// chosen, which an operator reads as the UI being broken rather
			// than as their own file -- which is why it is refused at load.
			"a duplicate id",
			Catalogue{{ID: "a", Voice: "/v", EspeakData: "/e"}, {ID: "a", Voice: "/w", EspeakData: "/e"}},
			"used twice",
		},
		{"no id", Catalogue{{Voice: "/v", EspeakData: "/e"}}, "id is empty"},
		{"no voice", Catalogue{{ID: "a", EspeakData: "/e"}}, "voice is empty"},
		{"no espeak data", Catalogue{{ID: "a", Voice: "/v"}}, "espeak_data is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cat.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestValidateVoiceID_IsAnAllowListAndNotADenyList.
//
// An id reaches no path TODAY, so a loose charset costs nothing now. What it
// costs is the day somebody widens the catalogue to a whole model stack and
// reaches for the id as a directory name or a label value -- at which point a
// `..` accepted for a year is already in operators' configs and in their
// stored requests.
func TestValidateVoiceID_IsAnAllowListAndNotADenyList(t *testing.T) {
	for _, ok := range []string{"a", "lessac", "en_us-lessac-medium", "v1.2", "a_b-c.d", "0"} {
		if err := ValidateVoiceID(ok); err != nil {
			t.Errorf("id %q was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "..", "a/b", `a\b`, "a b", "A", "Lessac", "a:b", "a*", "a\x00", "é",
		strings.Repeat("a", MaxVoiceID+1),
	} {
		if err := ValidateVoiceID(bad); err == nil {
			t.Errorf("id %q was accepted", bad)
		}
	}
	// Exactly the bound is accepted: "too long" and "the bound" are different
	// answers, which is internal/s3's `do` rule and MaxText's.
	if err := ValidateVoiceID(strings.Repeat("a", MaxVoiceID)); err != nil {
		t.Errorf("an id of exactly the bound was refused: %v", err)
	}
}

// TestAsk_ResolveIsTheOnlyPlaceAPathEnters is the seal's own assertion: the
// ask carries an id, the catalogue carries the files, and the resolved job has
// both.
func TestAsk_ResolveIsTheOnlyPlaceAPathEnters(t *testing.T) {
	a := Ask{Text: "hello", VoiceID: "lessac", Output: "greeting"}
	j, err := a.Resolve(cat())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if j.Voice != "/opt/voices/lessac.onnx" || j.EspeakData != "/e" {
		t.Errorf("resolved to voice=%q espeak=%q", j.Voice, j.EspeakData)
	}
	if j.Text != "hello" || j.Output != "greeting" {
		t.Errorf("text/output = %q/%q", j.Text, j.Output)
	}

	// AND THE ASK HAS NOWHERE A PATH COULD HAVE COME FROM, which is what makes
	// the sentence above a seal rather than a convention. Asserted by
	// reflection over the type, so a field added later that could carry one
	// has to be named here -- a hand-read list of what Ask holds is the thing
	// that goes stale.
	for _, f := range askFields() {
		switch f {
		case "Text", "VoiceID", "Output", "Speaker", "LengthScale", "NoiseScale", "NoiseW", "SentenceSilence":
		default:
			t.Errorf("tts.Ask has gained a field %q: a request's stored shape may name a voice "+
				"ID and never a path, so this needs reading before it is allowed", f)
		}
	}
}

// TestAsk_ResolveAppliesTheSameBoundsTheNodeWill, so a refusal an operator
// sees at enqueue is the one function the node would have applied -- rather
// than two wordings of one rule.
func TestAsk_ResolveAppliesTheSameBoundsTheNodeWill(t *testing.T) {
	bad := Ask{Text: "hello", VoiceID: "lessac", LengthScale: f(-1)}
	if _, err := bad.Resolve(cat()); err == nil {
		t.Fatal("a negative length_scale survived resolution")
	}
	if _, err := (Ask{VoiceID: "lessac"}).Resolve(cat()); err == nil {
		t.Fatal("an empty text survived resolution")
	}
	if _, err := (Ask{Text: "x", VoiceID: "lessac", Output: "../a"}).Resolve(cat()); err == nil {
		t.Fatal("an output that is a path survived resolution")
	}
	// And an id nothing published, which is the OTHER refusal Resolve can
	// make and the one that is not about the numbers: it is Lookup's, reached
	// through here, so a reader following the resolution finds both.
	if _, err := (Ask{Text: "x", VoiceID: "nope"}).Resolve(cat()); err == nil {
		t.Fatal("an unpublished voice id survived resolution")
	}
}

// TestAsk_ValidateRefusesWhatItCanWithoutACatalogue.
//
// At the wire's edge there may be no catalogue to look up against yet -- the
// refusal for an id nothing published is Lookup's -- so this is what an ask
// can be refused for on its own, and it must include the id's shape: an id
// that is not a token is a mistake in the body rather than a stale page.
func TestAsk_ValidateRefusesWhatItCanWithoutACatalogue(t *testing.T) {
	if err := (Ask{Text: "x", VoiceID: "lessac"}).Validate(); err != nil {
		t.Fatalf("a good ask was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		a    Ask
	}{
		{"no voice id", Ask{Text: "x"}},
		{"an id that is a path", Ask{Text: "x", VoiceID: "../etc/passwd"}},
		{"no text", Ask{VoiceID: "lessac"}},
		{"an output that is a path", Ask{Text: "x", VoiceID: "lessac", Output: "a/b"}},
		{"a bad number", Ask{Text: "x", VoiceID: "lessac", NoiseW: f(-1)}},
	} {
		if err := c.a.Validate(); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// askFields is Ask's field names, read off the type.
//
// REFLECTION AND NOT A LIST, because the assertion above is about a field
// somebody ADDS: a hand-read list of what Ask holds is exactly the thing that
// goes stale, and what would be smuggled in is whichever field the list has
// not heard of.
func askFields() []string {
	tp := reflect.TypeOf(Ask{})
	out := make([]string, 0, tp.NumField())
	for i := range tp.NumField() {
		out = append(out, tp.Field(i).Name)
	}
	return out
}
