package catalogue

import (
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// the4090 is the real shape the committed catalogue fixture has: NONE
// overall, LOW in one site. It is the observation hasCapacity exists for, and
// the reason this package stores sites rather than summaries.
func the4090(t time.Time, site string, level Level) Sample {
	return Sample{
		ObservedAt: t, Provider: "runpod", Kind: KindGPU,
		ID: "NVIDIA GeForce RTX 4090", Name: "RTX 4090", MemoryGiB: 24,
		Datacentre: site, Availability: level, Overall: LevelNone,
		Secure:    Tier{Offered: true, Price: node.RateFromUSD(0.89), MaxCount: 8},
		Community: Tier{Offered: true, Price: node.RateFromUSD(0.34), MaxCount: 8},
	}
}

func TestSummarise_OneRowPerSitePerType(t *testing.T) {
	rows := Summarise([]Sample{
		the4090(at(2, 10), "US-IL-1", LevelLow),
		the4090(at(2, 10), "EU-RO-1", LevelHigh),
		the4090(at(2, 11), "US-IL-1", LevelNone),
		the4090(at(2, 11), "EU-RO-1", LevelHigh),
	})
	if len(rows) != 2 {
		t.Fatalf("got %d row(s), want one per site", len(rows))
	}
	// Sorted by site within a type, so a report an operator compares between
	// runs does not reorder because a page boundary moved.
	if rows[0].Datacentre != "EU-RO-1" || rows[1].Datacentre != "US-IL-1" {
		t.Fatalf("rows = %q, %q", rows[0].Datacentre, rows[1].Datacentre)
	}

	eu, il := rows[0], rows[1]
	if eu.Samples != 2 || eu.High != 2 {
		t.Errorf("EU-RO-1 = %d sample(s), %d high", eu.Samples, eu.High)
	}
	if il.Low != 1 || il.None != 1 {
		t.Errorf("US-IL-1 = %d low, %d none", il.Low, il.None)
	}
	if il.MemoryGiB != 24 || il.Name != "RTX 4090" {
		t.Errorf("the row lost the card's own description: %+v", il)
	}
	if !il.First.Equal(at(2, 10)) || !il.Last.Equal(at(2, 11)) {
		t.Errorf("span = %s..%s", il.First, il.Last)
	}
	// The latest sample is the only thing in the row a live query could also
	// have answered, so it is the one figure that must track the last
	// observation rather than the first.
	if il.Latest != LevelNone {
		t.Errorf("latest = %q, want the most recent level", il.Latest)
	}
}

// TestSummarise_MaskedCountsWhatTheSummaryWouldHaveHidden. This is the whole
// reason Overall is stored: a collector keeping only the summary would have
// recorded these samples as unavailable for a card that was rentable, and
// this column says how often that would have happened.
func TestSummarise_MaskedCountsWhatTheSummaryWouldHaveHidden(t *testing.T) {
	rows := Summarise([]Sample{
		the4090(at(2, 10), "US-IL-1", LevelLow),  // summary NONE, site LOW
		the4090(at(2, 11), "US-IL-1", LevelNone), // summary NONE, site NONE
	})
	if len(rows) != 1 {
		t.Fatalf("got %d row(s)", len(rows))
	}
	if rows[0].Masked != 1 {
		t.Errorf("masked = %d, want 1: one of the two samples was rentable while the summary said NONE",
			rows[0].Masked)
	}
}

// TestSummarise_AgreementIsNotMasking. The column measures a disagreement; a
// summary that says NONE over a site that also says NONE is the two agreeing,
// and counting it would make the figure useless by inflating it on every
// sold-out card.
func TestSummarise_AgreementIsNotMasking(t *testing.T) {
	rows := Summarise([]Sample{the4090(at(2, 10), "US-IL-1", LevelNone)})
	if rows[0].Masked != 0 {
		t.Errorf("masked = %d over a site that agreed with the summary", rows[0].Masked)
	}
}

// TestSummarise_ASummaryRowIsMarkedAndMeasuresTheSummary. RunPod names no
// datacentre for some types -- three of the four in the committed fixture --
// and such a row is kept, because a card that vanishes from the series is
// indistinguishable from one that was never offered.
func TestSummarise_ASummaryRowIsMarkedAndMeasuresTheSummary(t *testing.T) {
	rows := Summarise([]Sample{{
		ObservedAt: at(2, 10), Provider: "runpod", Kind: KindGPU,
		ID: "AMD Instinct MI300X OAM", Overall: LevelNone,
	}})
	if len(rows) != 1 {
		t.Fatalf("got %d row(s)", len(rows))
	}
	r := rows[0]
	if !r.FromSummary {
		t.Error("a row with no site is not marked as a summary row")
	}
	if r.None != 1 || r.Unreported != 0 {
		t.Errorf("the summary row counted %d none and %d unreported; it measured the summary, "+
			"which said NONE", r.None, r.Unreported)
	}
	if r.Masked != 0 {
		t.Error("a summary row counted a disagreement with itself")
	}
}

// TestSummarise_TheSummaryRowSortsLastWithinAType, because the sites are what
// the report is for and a row with no site is a footnote to them.
func TestSummarise_TheSummaryRowSortsLastWithinAType(t *testing.T) {
	noSite := the4090(at(2, 10), "", LevelUnreported)
	rows := Summarise([]Sample{noSite, the4090(at(2, 10), "US-IL-1", LevelLow)})
	if len(rows) != 2 || rows[0].Datacentre != "US-IL-1" || !rows[1].FromSummary {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestRow_ShareDividesByWhatWasReported. A site the provider stopped
// reporting is not a site with no capacity: dividing by every sample would
// read as a card getting scarcer when what happened is that a field went
// absent.
func TestRow_ShareDividesByWhatWasReported(t *testing.T) {
	rows := Summarise([]Sample{
		the4090(at(2, 10), "US-IL-1", LevelLow),
		the4090(at(2, 11), "US-IL-1", LevelUnreported),
	})
	share, ok := rows[0].Share()
	if !ok {
		t.Fatal("a row with one reported level has no share")
	}
	if share != 1 {
		t.Errorf("share = %.2f, want 1: one sample reported, and it was rentable", share)
	}
	if rows[0].Reported() != 1 {
		t.Errorf("reported = %d of %d samples", rows[0].Reported(), rows[0].Samples)
	}
}

// TestRow_ShareSaysNothingRatherThanZero. Zero per cent means the provider
// said NONE every time; nothing reported means ember does not know, and the
// two send an operator to different places -- one to another datacentre, the
// other to find out why the field is empty.
func TestRow_ShareSaysNothingRatherThanZero(t *testing.T) {
	rows := Summarise([]Sample{the4090(at(2, 10), "US-IL-1", LevelUnreported)})
	if _, ok := rows[0].Share(); ok {
		t.Error("a row that reported nothing produced a share")
	}
	if rows[0].Unreported != 1 {
		t.Errorf("unreported = %d", rows[0].Unreported)
	}
}

// TestRow_AnUnknownLevelIsCountedAndNamed. It is the one thing in the file
// nothing in ember could have predicted, so it is kept verbatim rather than
// folded into a level ember does know.
func TestRow_AnUnknownLevelIsCountedAndNamed(t *testing.T) {
	rows := Summarise([]Sample{
		the4090(at(2, 10), "US-IL-1", "PLENTIFUL"),
		the4090(at(2, 11), "US-IL-1", "PLENTIFUL"),
	})
	r := rows[0]
	if r.Other != 2 {
		t.Errorf("other = %d, want 2", r.Other)
	}
	if len(r.Unknown) != 1 || r.Unknown[0] != "PLENTIFUL" {
		t.Errorf("unknown = %v, want the word once", r.Unknown)
	}
	if share, _ := r.Share(); share != 1 {
		t.Errorf("share = %.2f; an unrecognised level counts as capacity", share)
	}
}

// TestRow_PricesAreARangeWhenTheyMove, because whether prices drift is one of
// the questions the series exists to answer and a last-seen figure cannot
// carry it.
func TestRow_PricesAreARangeWhenTheyMove(t *testing.T) {
	cheap := the4090(at(2, 10), "US-IL-1", LevelLow)
	cheap.Secure.Price = node.RateFromUSD(0.79)
	rows := Summarise([]Sample{cheap, the4090(at(2, 11), "US-IL-1", LevelLow)})
	r := rows[0]
	if !r.Secure.Moved() {
		t.Error("a price that changed does not report as moved")
	}
	if r.Secure.Min != node.Rate(790_000) || r.Secure.Max != node.Rate(890_000) {
		t.Errorf("secure = %v..%v, want 0.79..0.89 in exact microdollars", r.Secure.Min, r.Secure.Max)
	}
	if r.Community.Moved() {
		t.Error("a price that did not change reports as moved")
	}

	// And the other direction: a quote that falls has to widen the range
	// downward, which a min that only ever compared upward would miss.
	falling := the4090(at(2, 12), "US-IL-1", LevelLow)
	falling.Secure.Price = node.RateFromUSD(0.59)
	rows = Summarise([]Sample{the4090(at(2, 10), "US-IL-1", LevelLow), falling})
	if rows[0].Secure.Min != node.Rate(590_000) || rows[0].Secure.Max != node.Rate(890_000) {
		t.Errorf("a falling price gave %v..%v, want 0.59..0.89",
			rows[0].Secure.Min, rows[0].Secure.Max)
	}
}

// TestSummarise_EveryLevelLandsInItsOwnColumn, including MEDIUM, which no
// other test here produces -- a column nothing ever increments is a column
// that can be wired to the wrong counter for the life of the feature.
func TestSummarise_EveryLevelLandsInItsOwnColumn(t *testing.T) {
	rows := Summarise([]Sample{
		the4090(at(2, 10), "EU-RO-1", LevelHigh),
		the4090(at(2, 11), "EU-RO-1", LevelMedium),
		the4090(at(2, 12), "EU-RO-1", LevelLow),
		the4090(at(2, 13), "EU-RO-1", LevelNone),
		the4090(at(2, 14), "EU-RO-1", LevelUnreported),
		the4090(at(2, 15), "EU-RO-1", "PLENTIFUL"),
		the4090(at(2, 16), "EU-RO-1", "ABUNDANT"),
	})
	r := rows[0]
	if r.High != 1 || r.Medium != 1 || r.Low != 1 || r.None != 1 || r.Unreported != 1 || r.Other != 2 {
		t.Errorf("counts = high %d, medium %d, low %d, none %d, unreported %d, other %d",
			r.High, r.Medium, r.Low, r.None, r.Unreported, r.Other)
	}
	if r.Samples != 7 {
		t.Errorf("samples = %d, want 7", r.Samples)
	}
	// Two distinct words, each named once. A list that repeated a word per
	// sample would be unreadable after a week of hourly collection.
	if len(r.Unknown) != 2 {
		t.Errorf("unknown = %v, want each word once", r.Unknown)
	}
}

// TestRow_ATierThatIsNotOfferedCarriesNoPrice. The catalogue quotes zero both
// for "free" and for "declined to quote", and only one of those exists here.
func TestRow_ATierThatIsNotOfferedCarriesNoPrice(t *testing.T) {
	s := the4090(at(2, 10), "US-IL-1", LevelLow)
	s.Community = Tier{Offered: false, Price: node.RateFromUSD(0.34)}
	rows := Summarise([]Sample{s})
	if rows[0].Community.Offered {
		t.Error("a tier that does not sell the card reports as offering it")
	}
	if rows[0].Community.Min != 0 || rows[0].Community.Max != 0 {
		t.Errorf("community = %v..%v, want nothing", rows[0].Community.Min, rows[0].Community.Max)
	}
}

func TestSummarise_NothingIsNoRows(t *testing.T) {
	if rows := Summarise(nil); len(rows) != 0 {
		t.Errorf("rows = %+v", rows)
	}
}

func TestSummarise_KindsAreSeparateRows(t *testing.T) {
	cpu := Sample{
		ObservedAt: at(2, 10), Provider: "runpod", Kind: KindCPU, ID: "cpu5c",
		Datacentre: "EU-RO-1", Availability: LevelHigh, Overall: LevelHigh,
		Secure: Tier{Offered: true, Price: node.RateFromUSD(0.03)},
	}
	rows := Summarise([]Sample{the4090(at(2, 10), "EU-RO-1", LevelHigh), cpu})
	if len(rows) != 2 {
		t.Fatalf("got %d row(s)", len(rows))
	}
	// Kind first, so the two tables do not interleave.
	if rows[0].Kind != KindCPU || rows[1].Kind != KindGPU {
		t.Errorf("kinds = %q, %q", rows[0].Kind, rows[1].Kind)
	}
}

// TestRow_MatchUsesTheSameComparisonAsAnOfferFilter: a filter matches a model
// name it is a prefix of, at the capacity boundary only.
func TestRow_MatchUsesTheSameComparisonAsAnOfferFilter(t *testing.T) {
	h100 := Row{Kind: KindGPU, ID: "NVIDIA H100 80GB HBM3"}
	for _, want := range []node.GPUModel{"", "h100", "h100-80g"} {
		if !h100.Match(want) {
			t.Errorf("--gpu %q does not match %q", want, h100.ID)
		}
	}
	for _, want := range []node.GPUModel{"h100-40g", "a100", "h1000"} {
		if h100.Match(want) {
			t.Errorf("--gpu %q matches %q", want, h100.ID)
		}
	}
	a100 := Row{Kind: KindGPU, ID: "NVIDIA A100-SXM4-40GB"}
	if !a100.Match("a100") || a100.Match("a1000") {
		t.Errorf("a100 matching is not at the capacity boundary for %q", a100.ID)
	}
}
