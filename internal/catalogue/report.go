package catalogue

import (
	"sort"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// Row is what the series says about one type at one site.
//
// Every field is counted, not derived. The share of samples that were
// rentable, and the span the series covers, are methods -- for the reason
// genstore.Bucket computes its quotients on the way out: a stored quotient is
// a number that can be wrong while the terms it came from are right.
type Row struct {
	Kind Kind            `json:"kind"`
	ID   string          `json:"id"`
	Name string          `json:"name,omitempty"`
	From node.ProviderID `json:"provider"`

	// Datacentre is empty on a row the provider named no site for, and
	// FromSummary says so in a form a consumer can branch on rather than by
	// testing a string for emptiness. Its level counts are the global
	// summary's, because that is the only figure there was.
	Datacentre  string `json:"datacentre"`
	FromSummary bool   `json:"from_summary,omitempty"`

	Samples int `json:"samples"`

	// The level counts. Explicit fields and not a map, so a level that
	// nobody has seen before cannot quietly become a key no reader looks at:
	// it lands in Other, and the report says the word out loud.
	High       int `json:"high"`
	Medium     int `json:"medium"`
	Low        int `json:"low"`
	None       int `json:"none"`
	Unreported int `json:"unreported"`
	Other      int `json:"other,omitempty"`

	// Unknown holds the levels that landed in Other, in the provider's own
	// spelling. Kept because it is the one thing the series records that
	// nothing else in ember could have predicted.
	Unknown []Level `json:"unknown,omitempty"`

	// Masked counts samples where the type's global summary said NONE while
	// this site reported capacity.
	//
	// This is the whole reason Overall is stored. A collector keeping only
	// the summary would have recorded those samples as "unavailable" for a
	// card that was rentable, and this column is how often that would have
	// happened -- measured, rather than argued about.
	Masked int `json:"masked"`

	Secure    Prices `json:"secure"`
	Community Prices `json:"community"`

	// MemoryGiB is VRAM per card, from the samples. Zero on a CPU row.
	MemoryGiB int `json:"memory_gib,omitempty"`

	First time.Time `json:"first"`
	Last  time.Time `json:"last"`

	// Latest is the level of the most recent sample, which is the only thing
	// in the row a single live query could also have told you.
	Latest Level `json:"latest"`
}

// Prices is the range one tier quoted over the period.
//
// A range and not a last-seen figure, because "whether prices drift" is one
// of the four questions the series exists to answer and a single number
// cannot answer it.
type Prices struct {
	Offered bool      `json:"offered"`
	Min     node.Rate `json:"min_micro_usd_per_hour,omitempty"`
	Max     node.Rate `json:"max_micro_usd_per_hour,omitempty"`
}

func (p *Prices) observe(t Tier) {
	if !t.Offered {
		return
	}
	if !p.Offered {
		p.Offered, p.Min, p.Max = true, t.Price, t.Price
		return
	}
	if t.Price < p.Min {
		p.Min = t.Price
	}
	if t.Price > p.Max {
		p.Max = t.Price
	}
}

// Moved reports whether the quote changed during the period.
func (p Prices) Moved() bool { return p.Offered && p.Min != p.Max }

// Reported is how many samples carried a level at all.
//
// The denominator of Share, and the reason it is not Samples: a site the
// provider stopped reporting is not a site with no capacity, and dividing by
// every sample would read as a card getting scarcer when what happened is
// that a field went absent.
func (r Row) Reported() int { return r.Samples - r.Unreported }

// Share is the fraction of reported samples in which something was rentable,
// and false when nothing was reported at all.
//
// The bool exists so a caller cannot print 0% for a row that measured
// nothing. Zero per cent means "never available"; no observation means "ember
// does not know", and the two send an operator to different places.
func (r Row) Share() (float64, bool) {
	n := r.Reported()
	if n == 0 {
		return 0, false
	}
	return float64(r.High+r.Medium+r.Low+r.Other) / float64(n), true
}

// Summarise folds samples into one row per (kind, id, datacentre).
//
// Pure and total: it takes what was read off disk and returns rows, with no
// clock and no filesystem, which is what lets the whole report be tested
// against a series built in a table.
func Summarise(samples []Sample) []Row {
	type key struct {
		kind       Kind
		id         string
		datacentre string
	}
	rows := map[key]*Row{}
	order := []key{}

	for _, smp := range samples {
		k := key{smp.Kind, smp.ID, smp.Datacentre}
		r, ok := rows[k]
		if !ok {
			r = &Row{
				Kind:        smp.Kind,
				ID:          smp.ID,
				Name:        smp.Name,
				From:        smp.Provider,
				Datacentre:  smp.Datacentre,
				FromSummary: smp.Datacentre == "",
				MemoryGiB:   smp.MemoryGiB,
				First:       smp.ObservedAt,
			}
			rows[k] = r
			order = append(order, k)
		}
		r.Samples++
		if smp.ObservedAt.Before(r.First) {
			r.First = smp.ObservedAt
		}
		if !smp.ObservedAt.Before(r.Last) {
			r.Last = smp.ObservedAt
			r.Latest = smp.Level()
		}
		switch l := smp.Level(); l {
		case LevelHigh:
			r.High++
		case LevelMedium:
			r.Medium++
		case LevelLow:
			r.Low++
		case LevelNone:
			r.None++
		case LevelUnreported:
			r.Unreported++
		default:
			r.Other++
			r.Unknown = appendLevel(r.Unknown, l)
		}
		// Only ever counted on a site row. On a summary row the two columns
		// are the same figure, and a count of a figure disagreeing with
		// itself would be noise in the column that exists to measure a real
		// disagreement.
		if !r.FromSummary && smp.Overall == LevelNone && smp.Availability.Rentable() {
			r.Masked++
		}
		r.Secure.observe(smp.Secure)
		r.Community.observe(smp.Community)
	}

	out := make([]Row, 0, len(order))
	for _, k := range order {
		out = append(out, *rows[k])
	}
	// Kind, then id, then site -- with the summary row last within a type,
	// because the sites are what the report is for and a row with no site is
	// a footnote to them.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.FromSummary != b.FromSummary {
			return b.FromSummary
		}
		return a.Datacentre < b.Datacentre
	})
	return out
}

func appendLevel(ls []Level, l Level) []Level {
	for _, have := range ls {
		if have == l {
			return ls
		}
	}
	return append(ls, l)
}

// Match reports whether a row is about the card an operator asked for.
//
// Through node.NormalizeGPU and GPUModel.Accepts rather than a substring
// test, so `--gpu h100` matches "NVIDIA H100 80GB HBM3" at the capacity
// boundary and `--gpu a100` does not match an A1000. That is the same
// comparison node.Filter makes, and the reason it is not string equality is
// written down there: NormalizeGPU keeps the capacity suffix because an A100
// 40G and an A100 80G are not substitutes.
func (r Row) Match(want node.GPUModel) bool {
	if want == "" {
		return true
	}
	return node.NormalizeGPU(r.ID).Accepts(want)
}
