package catalogue

import (
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

func TestLevel_RankOrdersTheFourLevels(t *testing.T) {
	if LevelHigh.Rank() <= LevelMedium.Rank() ||
		LevelMedium.Rank() <= LevelLow.Rank() ||
		LevelLow.Rank() <= LevelNone.Rank() {
		t.Errorf("the levels do not order: high %d medium %d low %d none %d",
			LevelHigh.Rank(), LevelMedium.Rank(), LevelLow.Rank(), LevelNone.Rank())
	}
	if LevelNone.Rank() != 0 || LevelUnreported.Rank() != 0 {
		t.Errorf("NONE ranks %d and unreported %d; both are no capacity",
			LevelNone.Rank(), LevelUnreported.Rank())
	}
}

// TestLevel_AWordNobodyHasSeenIsNotSoldOut. The schema enumerates four levels
// today; an enum is a promise about today and this file is read years later.
// A level ember does not know must not be recorded as unavailable, because
// the consequence is a card that reads as never rentable in a report somebody
// moves a store on.
func TestLevel_AWordNobodyHasSeenIsNotSoldOut(t *testing.T) {
	const grew Level = "PLENTIFUL"
	if !grew.Rentable() {
		t.Error("an unrecognised level reads as no capacity")
	}
	if grew.Rank() <= LevelNone.Rank() || grew.Rank() >= LevelMedium.Rank() {
		t.Errorf("an unrecognised level ranks %d, want above NONE and below MEDIUM", grew.Rank())
	}
	if grew.Known() {
		t.Error("an unrecognised level claims to be one of the four")
	}
	if !LevelHigh.Known() || !LevelNone.Known() || LevelUnreported.Known() {
		t.Error("Known does not describe the enumerated four")
	}
}

// TestLevel_UnreportedIsNotNone is the distinction the whole column rests on.
// NONE is the provider saying there is nothing; empty is the provider not
// saying. Folding the second into the first records a card as sold out on the
// strength of a field that was absent.
func TestLevel_UnreportedIsNotNone(t *testing.T) {
	if LevelUnreported == LevelNone {
		t.Fatal("unreported and NONE are the same value")
	}
	if LevelUnreported.Reported() {
		t.Error("an empty level claims to have been reported")
	}
	if !LevelNone.Reported() {
		t.Error("NONE is a report: the provider said there is nothing")
	}
	if LevelUnreported.Rentable() {
		t.Error("an unreported level is not evidence of capacity")
	}
}

func TestSample_LevelPrefersTheSiteAndFallsBackToTheSummary(t *testing.T) {
	site := Sample{Datacentre: "EU-RO-1", Availability: LevelLow, Overall: LevelNone}
	if got := site.Level(); got != LevelLow {
		t.Errorf("a site row measures the site, got %q", got)
	}
	// A row the provider named no site for carries the summary and nothing
	// else, so that is what it measured. Reading the site column there would
	// print a column of zeroes for a card nobody asked about.
	summary := Sample{Overall: LevelHigh}
	if got := summary.Level(); got != LevelHigh {
		t.Errorf("a row with no site measures the summary, got %q", got)
	}
}

// TestSample_DayIsUTC. The day decides the file, and a collector on a laptop
// that travels would otherwise split one hour across two files and put two
// hours in one.
func TestSample_DayIsUTC(t *testing.T) {
	east := time.FixedZone("UTC+13", 13*60*60)
	s := Sample{ObservedAt: time.Date(2026, 11, 3, 11, 30, 0, 0, east)}
	// 2026-11-03T11:30+13:00 is 2026-11-02T22:30Z.
	if got := s.Day(); got != "2026-11-02" {
		t.Errorf("day = %q, want the UTC date 2026-11-02", got)
	}
}

func TestSample_ValidateRefusesWhatCannotBePlaced(t *testing.T) {
	ok := Sample{
		ObservedAt: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
		Provider:   "runpod", Kind: KindGPU, ID: "NVIDIA GeForce RTX 4090",
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a complete sample was refused: %v", err)
	}

	for _, tc := range []struct {
		name   string
		breaks func(*Sample)
		want   string
	}{
		{"no observed_at", func(s *Sample) { s.ObservedAt = time.Time{} }, "observed_at"},
		{"no provider", func(s *Sample) { s.Provider = "" }, "provider"},
		{"no kind", func(s *Sample) { s.Kind = "" }, "kind"},
		{"invented kind", func(s *Sample) { s.Kind = "tpu" }, "kind"},
		{"no id", func(s *Sample) { s.ID = "" }, "catalogue id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ok
			tc.breaks(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %q, got %v", tc.want, err)
			}
		})
	}
}

func TestKind_ValidIsClosed(t *testing.T) {
	if !KindGPU.Valid() || !KindCPU.Valid() {
		t.Error("the two kinds ember writes are not valid")
	}
	if Kind("").Valid() || Kind("GPU").Valid() {
		t.Error("Valid accepts something ember does not write")
	}
}

// TestTier_PriceIsIntegerMicrodollars. The whole file is read back to compare
// prices, and a float figure that only ever gets printed still prints the
// wrong price: int64(0.34*1e6) is 339999 on this hardware.
func TestTier_PriceIsIntegerMicrodollars(t *testing.T) {
	tr := Tier{Offered: true, Price: node.RateFromUSD(0.34)}
	if tr.Price != node.Rate(340_000) {
		t.Errorf("price = %d, want exactly 340000 microdollars", tr.Price)
	}
}
