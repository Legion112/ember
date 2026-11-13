package genstore

// Kind names which workload a request is for.
//
// STRINGS AND NOT AN ENUM OF ITS OWN, for `node.Leftover.Kind`'s reason
// turned up one notch: this one IS dispatched on rather than only displayed,
// so the set has to be closed -- and `KindValid` is what closes it. What a
// named `type Kind string` would buy is that a provider name could not be
// assigned to it by mistake; what it costs is a conversion at every one of
// the four seams this crosses (the wire, the store, the queue, the ledger),
// and the column is `TEXT` at the bottom of all of them.
const (
	// KindSD is an image generation through a resident sd-server.
	KindSD = "sd"

	// KindTTS is speech: one piper invocation per utterance.
	KindTTS = "tts"
)

// Kinds is every kind ember runs, in the order a UI should offer them.
//
// DERIVED FROM NOTHING AND THEREFORE THE ONE LIST, which is the shape
// `report.Kinds` already has: a second spelling of this -- in the wire type's
// validation, in a switch, in a UI -- is a place for one of them to be
// forgotten, and the forgotten one is always the kind added last.
func Kinds() []string { return []string{KindSD, KindTTS} }

// KindValid reports whether k is a kind ember runs.
//
// The empty string is valid and means KindSD; see KindOf.
func KindValid(k string) bool {
	if k == "" {
		return true
	}
	for _, v := range Kinds() {
		if v == k {
			return true
		}
	}
	return false
}

// KindOf is the kind to run for a stored value, resolving empty to sd.
//
// ONE PLACE, AND IT IS THE ONLY PLACE THAT KNOWS EMPTY MEANS SD. Every row
// written before schema version 6 has no kind, and every one of them is an
// image generation -- so the alternative was `UPDATE requests SET kind='sd'`,
// which rewrites every row of a table this schema is careful never to rewrite
// and asserts about an operator's history rather than recording the absence.
// The cost of the absence is that two spellings mean one thing, and the way
// that cost is paid once is that nothing else compares against `""`.
func KindOf(k string) string {
	if k == "" {
		return KindSD
	}
	return k
}

// WorkloadOf is the declared workload a stored row names, resolving empty.
//
// ONE PLACE, AND IT IS THE ONLY PLACE THAT KNOWS AN EMPTY WORKLOAD NAMES THE
// IMPLICIT ENTRY. A configuration that declares no `workloads:` describes one
// entry per driver out of its top-level keys, and each of those entries is
// named after its own driver -- so a row with no workload names the entry
// `KindOf(kind)` identifies, which is "sd" for every row written before the
// kind column existed and for every image generation since.
//
// The cost of the two absences is that three spellings mean one thing, and the
// way that cost is paid once is that nothing else compares against `""`. The
// SQL in ClaimNext is the one unavoidable second copy and says so where it is
// written.
func WorkloadOf(workload, kind string) string {
	if workload != "" {
		return workload
	}
	return KindOf(kind)
}
