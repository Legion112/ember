package tts

import (
	"errors"
	"fmt"
	"strings"
)

// Voice is one catalogue entry: a model choice an operator published.
//
// WHY THERE IS A CATALOGUE AT ALL, which is the whole of #190's shape and the
// only reason a browser can ask for speech without widening the seal. A
// request names a provider, a card and a batch of jobs and NOTHING else -- no
// image, no store, no mount, no ports, no serve argv, no gpu probe, no assets.
// A voice is a file on the node, so a `voice:` field on the wire would be a
// request naming a path, which is the first of those seals to go.
//
// So the operator publishes a LIST and a request names an **entry id**: one
// opaque string, chosen from what ember published. That is the allow-list rule
// this repository applies to everything dangerous, and it is the same shape as
// `hires.upscaler` being checked against `Capabilities.Upscalers` rather than
// against a pattern -- except that the authority here is the operator's config
// rather than a server's response, because ember is the thing that knows which
// voices its image carries.
//
// WHAT A CATALOGUE ENTRY IS NOT. #190 describes an entry carrying "the image
// reference, the assets with their digests, the probe, the serve command and
// the outputs" -- a whole model stack. This is narrower on purpose and the
// narrowing is the point: a stack catalogue makes a request choose an IMAGE,
// which is a bigger question than speech (every seal in node, exposure and
// loopback sits downstream of `image:`) and one the sd path would have to move
// for too, since `sd.serve` is one server per config today. A voice is the one
// dimension a browser needs and the one that is a path on the node, so it is
// the honest first entry. The wider catalogue is the follow-up and the id
// shape does not change when it arrives.
type Voice struct {
	// ID is what a request names. Opaque, compared with ==, and never joined
	// to a path.
	ID string

	// Name is what a UI puts in its list. Free text, because nothing reads
	// it but a human.
	Name string

	// Voice is the .onnx on the node and EspeakData piper's data directory.
	//
	// THESE ARE THE FIELDS THE SEAL EXISTS FOR. They are set from the
	// operator's config and are unreachable from any request: `Ask` -- the
	// shape a request stores -- has no field for either, so a round trip
	// through the store cannot invent one.
	Voice      string
	EspeakData string
}

// Catalogue is the published list, in the operator's own order.
//
// A SLICE AND NOT A MAP, because the order is what a UI shows and a map's is
// random -- the same reason config.WriteConfig sorts its keys. Lookup is
// linear over a list an operator typed by hand, which is single digits.
type Catalogue []Voice

// Lookup resolves an id to the entry the operator published.
//
// The refusal names what IS available, because the one way to hit it from a
// browser is a stale page: a UI that loaded the list, the operator edited the
// config, and the id no longer exists. "unknown voice" with no list sends them
// to read a file they cannot see from there.
func (c Catalogue) Lookup(id string) (Voice, error) {
	for _, v := range c {
		if v.ID == id {
			return v, nil
		}
	}
	if len(c) == 0 {
		return Voice{}, errors.New("this configuration publishes no voices, so there is " +
			"nothing to speak with: add a tts.voices list to ember.yaml")
	}
	ids := make([]string, 0, len(c))
	for _, v := range c {
		ids = append(ids, v.ID)
	}
	return Voice{}, fmt.Errorf("no voice %q; this configuration publishes %s",
		id, strings.Join(ids, ", "))
}

// Validate refuses a catalogue before anything is published from it.
//
// At config load and not at lookup, because a duplicate id is a mistake whose
// symptom is that one of two voices can never be chosen -- which an operator
// would read as the UI being broken rather than as their own file.
func (c Catalogue) Validate() error {
	seen := map[string]bool{}
	for i, v := range c {
		if err := ValidateVoiceID(v.ID); err != nil {
			return fmt.Errorf("tts.voices[%d]: %w", i, err)
		}
		if seen[v.ID] {
			return fmt.Errorf("tts.voices[%d]: id %q is used twice, so one of the two could "+
				"never be chosen", i, v.ID)
		}
		seen[v.ID] = true
		if strings.TrimSpace(v.Voice) == "" {
			return fmt.Errorf("tts.voices[%d] (%s): voice is empty; it names the .onnx on the node",
				i, v.ID)
		}
		if strings.TrimSpace(v.EspeakData) == "" {
			return fmt.Errorf("tts.voices[%d] (%s): espeak_data is empty; it names piper's "+
				"espeak-ng data directory on the node", i, v.ID)
		}
	}
	return nil
}

// MaxVoiceID bounds an id, which is a token and not a sentence.
const MaxVoiceID = 64

// ValidateVoiceID refuses an id that is not an opaque token.
//
// AN ALLOW-LIST AND NOT A DENY-LIST, which is this repository's rule for
// anything dangerous and is worth stating for a field that is dangerous in a
// way it does not look: an id is compared with == and reaches no path today,
// so a loose charset costs nothing NOW. What it costs is the day somebody
// widens the catalogue to #190's full stack and reaches for the id as a
// directory name, a cache key or a label value -- at which point a `..` or a
// `/` that has been accepted for a year is already in operators' configs and
// in their stored requests. `[a-z0-9._-]` is what a UI can round-trip, what a
// label can hold and what a path join cannot be surprised by.
//
// Lowercase only, because the comparison is exact: `Lessac` and `lessac`
// being two entries is a UI showing one choice twice, and case-folding the
// comparison instead would make the id's identity depend on a locale.
func ValidateVoiceID(id string) error {
	if id == "" {
		return errors.New("id is empty; it is the opaque string a request names")
	}
	if len(id) > MaxVoiceID {
		return fmt.Errorf("id is %d bytes, above the bound of %d: an id is a token a UI "+
			"round-trips, not a description -- `name` is where prose goes", len(id), MaxVoiceID)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("id %q contains %q; an id is lowercase letters, digits, "+
				"dot, underscore and dash", id, string(r))
		}
	}
	// AND AN ID THAT IS NOTHING BUT DOTS, which the charset above lets
	// through and which this function's own argument is about.
	//
	// FOUND BY THE TEST WRITTEN FOR THE ARGUMENT AND NOT BY REVIEW: `.` and
	// `..` are spelled entirely out of characters the allow-list permits, so
	// the list alone accepted the two names every path join in existence
	// treats specially -- the hole being in the check whose whole case is
	// "the day somebody uses this as a directory name". It is refused rather
	// than cleaned, which is internal/transfer's rule: path.Clean("/../a")
	// returns "/a", so a check that cleaned first could never fire.
	if strings.Trim(id, ".") == "" {
		return fmt.Errorf("id %q is nothing but dots; every path join there is treats that "+
			"as a directory rather than as a name", id)
	}
	return nil
}

// Ask is one utterance as a REQUEST names it, and the difference from Job is
// the whole seal.
//
// Job carries the resolved paths and is what reaches the node. Ask carries a
// voice ID and has no field for a path, no field for a binary and no field for
// argv -- so a request body, and a row read back out of the store, cannot
// express one. `genstore.Job.TTS` is this type and never Job, which is the
// same asymmetry AGENTS.md records for the init image: the bytes are on the
// thing serialised nowhere and the reference is on the thing stored as JSON.
//
// EVERY OTHER FIELD IS OPERATOR CONTENT OR A NUMBER PIPER PUBLISHES. Text is
// a prompt and travels like one. The five optional numbers are piper's own
// flags, each bounded by Job.Validate. There is no `extra`, which is not a
// refusal here but an absence: piper publishes six options in total, so there
// is nothing an untyped map could carry that a field does not.
type Ask struct {
	// Text is what to say: the operator's content, bounded by MaxText.
	Text string `json:"text"`

	// VoiceID names a catalogue entry and is the ONLY way a request chooses a
	// model.
	VoiceID string `json:"voice_id"`

	// Output is the filename stem, without an extension, refused by
	// ValidateOutput unless it is a plain name.
	Output string `json:"output,omitempty"`

	// The five piper publishes. Pointers for the reason Job's are: piper's
	// defaults are 0, 1.0, 0.667, 0.8 and 0.2, so zero is a value somebody
	// might mean and a zero sentinel would make it unrequestable.
	Speaker         *int     `json:"speaker,omitempty"`
	LengthScale     *float64 `json:"length_scale,omitempty"`
	NoiseScale      *float64 `json:"noise_scale,omitempty"`
	NoiseW          *float64 `json:"noise_w,omitempty"`
	SentenceSilence *float64 `json:"sentence_silence,omitempty"`
}

// Resolve turns an ask into the job that reaches the node.
//
// THIS IS THE ONE PLACE A PATH ENTERS, and it enters from the catalogue rather
// than from the ask. A reader looking for how a request could name a file ends
// up here and finds the lookup.
func (a Ask) Resolve(c Catalogue) (Job, error) {
	v, err := c.Lookup(a.VoiceID)
	if err != nil {
		return Job{}, err
	}
	j := Job{
		Text:            a.Text,
		Output:          a.Output,
		Voice:           v.Voice,
		EspeakData:      v.EspeakData,
		Speaker:         a.Speaker,
		LengthScale:     a.LengthScale,
		NoiseScale:      a.NoiseScale,
		NoiseW:          a.NoiseW,
		SentenceSilence: a.SentenceSilence,
	}
	// Validated here and not only at enqueue, because this is the last point
	// before a rented machine is asked to do it -- the rule EnsureAssets
	// follows for a digest, applied to a number.
	if err := j.Validate(); err != nil {
		return Job{}, err
	}
	return j, nil
}

// Validate refuses an ask before anything is queued.
//
// Everything Job.Validate checks except the resolved paths, which this shape
// does not have -- so the refusals an operator sees at enqueue are the same
// ones, minus the two that cannot be their fault.
func (a Ask) Validate() error {
	if err := ValidateVoiceID(a.VoiceID); err != nil {
		return err
	}
	return Job{
		Text: a.Text, Output: a.Output,
		Speaker: a.Speaker, LengthScale: a.LengthScale, NoiseScale: a.NoiseScale,
		NoiseW: a.NoiseW, SentenceSilence: a.SentenceSilence,
		// The two the ask cannot carry, filled so that Job.Validate's own
		// checks on them do not fire: an ask is not wrong for lacking a path
		// it is not allowed to have.
		Voice: "resolved later", EspeakData: "resolved later",
	}.Validate()
}
