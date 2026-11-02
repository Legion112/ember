// Package catalogue keeps a record of what a provider said it had, when.
//
// It exists because the answer cannot be backfilled. RunPod reports
// availability as a bare level -- HIGH, MEDIUM, LOW, NONE -- with no
// timestamp and no window, on a card and on each datacentre; both live
// OpenAPI documents were checked and neither has anything historical. Every
// read is a point sample, nothing on the provider's side remembers the
// previous one, and the decision the series informs is the one that is
// hardest to change: a network volume lives in exactly one site and pins
// every node that mounts it there for as long as the store exists. See
// docs/gpu-availability-history.md.
//
// So the only field in a Sample that the provider does not supply is
// ObservedAt, and it is the entire point of the file.
//
// WHY THE TYPE IS HERE AND NOT IN internal/node. node holds what every
// provider must agree about, and a Sample is shaped by one of them: two cloud
// tiers with separate prices and separate per-pod ceilings is RunPod's
// product and nobody else's. Putting it in node would make a one-provider
// shape look like the domain. provider.CatalogueAPI says the same thing about
// the interface.
package catalogue

import (
	"fmt"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// Kind is what a sample is about.
//
// It also decides what the prices on the row mean, and there is deliberately
// no second field saying so: a GPU row quotes one card for an hour and a CPU
// row quotes one vCPU for an hour, because that is what each endpoint
// returns. A `price_unit` column would be a second source of truth for a fact
// Kind already carries, and the two could disagree -- the mistake
// Offer.VCPU's comment names.
type Kind string

const (
	// KindGPU is a card. Prices are per card per hour.
	KindGPU Kind = "gpu"

	// KindCPU is a CPU machine family. Prices are per vCPU per hour, which
	// is how the catalogue quotes them -- not per machine, as node.Offer
	// does. The conversion needs a core count, and a core count is a
	// property of the run rather than of the catalogue, so it is not applied
	// here.
	KindCPU Kind = "cpu"
)

// Valid reports whether k is a kind this package writes.
func (k Kind) Valid() bool { return k == KindGPU || k == KindCPU }

// Level is an availability level, stored exactly as the provider spelled it.
//
// Verbatim and never normalised. A level ember does not recognise is the one
// observation it can never make again, so rewriting it into a known one --
// or dropping the row -- would lose the only copy. Interpretation happens on
// the way out, in Rank and Rentable.
type Level string

const (
	// LevelUnreported is the empty level, and it is not NONE.
	//
	// NONE means the provider said there is nothing; this means it did not
	// say. Treating the second as the first records a card as sold out on
	// the strength of a field that was absent -- the same distinction
	// node.Node.Exposure draws between "published nothing" and "never
	// reported", here in the column the whole file exists for.
	LevelUnreported Level = ""

	LevelNone   Level = "NONE"
	LevelLow    Level = "LOW"
	LevelMedium Level = "MEDIUM"
	LevelHigh   Level = "HIGH"
)

// Rank orders the levels.
//
// An unrecognised level sorts above NONE and below MEDIUM, which is the
// ordering internal/provider/runpod's own rank uses and delegates here so
// there is one of them: ember should not record a card as unavailable because
// the API grew a word it does not know, nor claim it is plentiful. The live
// v2 schema enumerates exactly NONE, LOW, MEDIUM, HIGH -- an enum is a
// promise about today, and this file is read years later.
func (l Level) Rank() int {
	switch l {
	case LevelHigh:
		return 3
	case LevelMedium:
		return 2
	case LevelLow:
		return 1
	case LevelNone, LevelUnreported:
		return 0
	default:
		return 1
	}
}

// Rentable reports whether this level says something could be had.
//
// False for LevelUnreported, because a level that was never reported is not
// evidence of capacity. That direction matters only in the report, which
// counts unreported samples in their own column rather than folding them into
// NONE: an absent field and a sold-out card are different answers and a share
// that mixed them would be wrong in both directions.
func (l Level) Rentable() bool { return l.Rank() > 0 }

// Reported reports whether the provider gave a level at all.
func (l Level) Reported() bool { return l != LevelUnreported }

// Known reports whether this is one of the four levels the schema enumerates.
//
// Used by the report to say out loud that it saw a word nobody has seen
// before, rather than silently ranking it.
func (l Level) Known() bool {
	switch l {
	case LevelNone, LevelLow, LevelMedium, LevelHigh:
		return true
	default:
		return false
	}
}

// Tier is one cloud tier's quote for a type.
//
// The price and the ceiling are nested inside the tier rather than sitting
// beside it as price_secure and max_secure, so a price cannot be read without
// the tier it belongs to and a tier that is not offered at all cannot carry a
// price that looks rentable.
type Tier struct {
	// Offered is whether this tier sells the type at all. A tier that does
	// not is not a zero price: the catalogue quotes 0 for both cases, and
	// mapOffers drops a zero because "declined to quote" is not "free".
	Offered bool `json:"offered"`

	// Price is the list price, in microdollars per hour, for one unit of
	// whatever Kind says this row is about. Integer, because every money
	// figure in ember is: int64(0.34*1e6) is 339999 on this hardware, so a
	// float that only ever gets printed still prints the wrong price.
	Price node.Rate `json:"price_micro_usd_per_hour,omitempty"`

	// MaxCount is how many of one card a single pod may have on this tier.
	// Meaningless for a CPU row, which is why it is omitted there rather
	// than filled with a vCPU figure that would read as a card count.
	MaxCount int `json:"max_count,omitempty"`
}

// Sample is one observation of one type at one site.
//
// One row per (ObservedAt, ID, Datacentre), per the design note, and never
// one row per type. The global summary reads NONE while an individual site
// still has capacity -- that is a real observation from the live catalogue
// and the reason hasCapacity exists in the RunPod mapper -- so a file holding
// only the summary would record "unavailable" for cards that were rentable
// all along. That is worse than no data: it is data that looks like an
// answer.
type Sample struct {
	// ObservedAt is when the sample was taken, in UTC. The only field no
	// provider supplies, and the reason the file is worth keeping.
	ObservedAt time.Time `json:"observed_at"`

	Provider node.ProviderID `json:"provider"`
	Kind     Kind            `json:"kind"`

	// ID is the provider's own catalogue id, unnormalised -- "NVIDIA GeForce
	// RTX 4090" and not "rtx4090". It is what a create call names, and
	// normalising it here would store ember's reading of the name instead of
	// the name. node.NormalizeGPU is applied by the reader, where a filter
	// needs it.
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	// Datacentre is the site this level is about. Empty means the provider
	// named no site for this type in this sample -- which the live catalogue
	// does, for every type it reports no dataCenters array for. Such a row
	// carries Overall and nothing else, and the reader marks it rather than
	// treating a summary as a site.
	Datacentre string `json:"datacentre"`

	// Availability is this site's level. Unreported on a row with no site:
	// copying the summary in would turn the thing the note forbids into a
	// row that looks like a measurement.
	Availability Level `json:"availability"`

	// Overall is the type's global summary, kept to show how often it
	// disagrees with every site. The disagreement is the defect the mapper
	// guards against, and this column is what lets the report say how often
	// it would have fired.
	Overall Level `json:"overall"`

	Secure    Tier `json:"secure"`
	Community Tier `json:"community"`

	// MemoryGiB is VRAM per card, as the catalogue quotes it. Absent on a
	// CPU row, where RAM is a multiple of the core count the run picks.
	MemoryGiB int `json:"memory_gib,omitempty"`
}

// Level is what this sample measured about its own subject.
//
// The site's level for a site row, and the global summary for a row with no
// site -- because for that row the summary is the only figure the provider
// gave, and a column of zeroes would read as "sold out" for a card nobody
// asked about. The report marks those rows, so the two meanings are never
// added together silently.
func (s Sample) Level() Level {
	if s.Datacentre == "" {
		return s.Overall
	}
	return s.Availability
}

// Day is the UTC date this sample belongs to, which is the file it lands in.
//
// UTC and never local time: a collector on a laptop that travels would
// otherwise split one hour's samples across two files and put two different
// hours in one, and the day a row is in is how every period query finds it.
func (s Sample) Day() string { return s.ObservedAt.UTC().Format(dayLayout) }

const dayLayout = "2006-01-02"

// Validate refuses a sample that cannot take its place in the series.
//
// Checked before anything is written, and the whole batch is refused if one
// row fails: a line with no timestamp is a line no period query can find and
// no later run can repair, in the one file in ember that is never rewritten.
func (s Sample) Validate() error {
	if s.ObservedAt.IsZero() {
		return fmt.Errorf("sample for %q has no observed_at, which is the only field a provider does not supply and the reason the file exists", s.ID)
	}
	if s.Provider == "" {
		return fmt.Errorf("sample for %q at %s names no provider", s.ID, s.ObservedAt.UTC().Format(time.RFC3339))
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("sample for %q has kind %q; ember writes %q and %q", s.ID, s.Kind, KindGPU, KindCPU)
	}
	if s.ID == "" {
		return fmt.Errorf("sample at %s has no catalogue id", s.ObservedAt.UTC().Format(time.RFC3339))
	}
	return nil
}
