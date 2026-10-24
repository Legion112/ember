package node

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// MicroUSD is an amount of money in millionths of a dollar.
//
// Money is an integer in this package and never a float. A budget ceiling is
// compared with ==, < and >, and float arithmetic makes those comparisons
// lie: 0.1+0.2 > 0.3 is true in float64, so a $0.30 ceiling built by adding
// two offers rejects an offer that costs exactly $0.30, and the inverse
// rounding lets a run exceed a ceiling the operator set. At $4/hr for a B200
// those cents are not the point; the point is that a budget which is
// occasionally wrong by a rounding error is a budget nobody trusts.
type MicroUSD int64

// Rate is a MicroUSD per hour.
type Rate MicroUSD

// MiB is a count of mebibytes. Providers report VRAM and disk in several
// units; everything is normalised to this one at the mapping edge.
type MiB int64

// MiBPerGiB converts between the two units providers quote VRAM in.
//
// Named rather than written as 1024 at each call site, because the bug it
// prevents is silent: RunPod reports VRAM as "24" meaning GB, ember's Offer
// carries MiB, and a straight assignment makes a 24 GB card report 24 MiB.
// Nothing errors -- every --min-vram filter simply rejects all real hardware,
// and `ember offers` looks like a provider with no capacity.
const MiBPerGiB = 1024

// GiB converts a provider's whole-GiB VRAM figure to MiB.
func GiB(n int) MiB { return MiB(n) * MiBPerGiB }

const (
	micros = 1_000_000

	// MaxRate is a sanity ceiling, not a policy. The most expensive thing on
	// any provider ember speaks to is a few dollars an hour; a parsed rate
	// above this means the field was not what we thought it was — cents
	// instead of dollars, or a total instead of an hourly — and continuing
	// with it would rent something nobody priced.
	MaxRate = Rate(1000 * micros)
)

func (m MicroUSD) String() string { return fmt.Sprintf("$%.6g", float64(m)/micros) }
func (r Rate) String() string     { return MicroUSD(r).String() + "/hr" }

// Cost is what r comes to over d, rounded up to the next microdollar.
//
// Rounding up rather than to nearest: an estimate that reads low is an
// estimate that lets a run start which the budget would have refused.
func (r Rate) Cost(d time.Duration) MicroUSD {
	if d <= 0 {
		return 0
	}
	num := int64(r) * int64(d)
	den := int64(time.Hour)
	q := num / den
	if num%den != 0 {
		q++
	}
	return MicroUSD(q)
}

// RateFromUSD converts a provider's float price per hour into an exact rate.
//
// This is the only place a float is allowed to become money, and it exists
// because provider APIs quote prices as JSON numbers. Everything above it is
// integer microdollars, so a budget ceiling is an exact comparison and
// 0.1+0.2 > 0.3 cannot bypass it.
//
// Rounded, not truncated. int64(0.44 * 1e6) is 439999 on this hardware,
// because 0.44 has no exact binary representation -- so truncation would
// quietly under-report every price ending in a hundredth of a cent, and a
// budget check would pass on a node a hair over the ceiling.
//
// A negative or non-finite price becomes zero rather than nonsense. A
// provider quoting NaN is a provider ember should not rent from, and the
// caller checks for a zero price separately: "declined to quote" and "free"
// are different, and only the local provider is the second one.
func RateFromUSD(usd float64) Rate {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd <= 0 {
		return 0
	}
	return Rate(math.Round(usd * micros))
}

// ParseUSD reads a decimal dollar amount such as "0.34" or "1" into MicroUSD
// without going through a float.
//
// strconv.ParseFloat then multiplying by 1e6 is the obvious implementation and
// it is wrong for exactly the values that matter: "0.29" becomes
// 0.28999999999999998 and then 289999 microdollars, so a price that reads
// identical to the provider's web page compares as cheaper than it is.
func ParseUSD(s string) (MicroUSD, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "$")
	if s == "" {
		return 0, fmt.Errorf("empty price")
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("negative price %q", s)
	}

	// s is non-empty here, so a missing whole part means the string began with
	// "." — ".5" — which is a spelling providers do emit.
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if hasFrac && frac == "" {
		return 0, fmt.Errorf("malformed price %q", s)
	}

	var cents int64
	for _, c := range whole {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("malformed price %q", s)
		}
		cents = cents*10 + int64(c-'0')
		if cents > math.MaxInt64/micros/10 {
			return 0, fmt.Errorf("price out of range %q", s)
		}
	}
	total := cents * micros

	// Six fractional digits is the whole precision of MicroUSD. A seventh
	// digit is truncated rather than rejected, because providers do publish
	// prices like "0.4290000000000001" after a float round-trip of their own
	// and refusing those would make ember unable to see real offers.
	if hasFrac {
		scale := int64(micros / 10)
		for i, c := range frac {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("malformed price %q", s)
			}
			if i >= 6 {
				continue
			}
			total += int64(c-'0') * scale
			scale /= 10
		}
	}
	return MicroUSD(total), nil
}

// RateFromFloat converts a provider's JSON number to a Rate.
//
// Vast.ai reports prices as JSON numbers, so a float64 is what the decoder
// hands us and there is no decimal string to parse. Rounding is to nearest
// here rather than up, because the float already is the provider's value and
// biasing it would make ember's displayed price disagree with the invoice.
func RateFromFloat(f float64) (Rate, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("price is not a finite number: %v", f)
	}
	if f < 0 {
		return 0, fmt.Errorf("negative price %v", f)
	}
	r := Rate(math.Round(f * micros))
	if r > MaxRate {
		return 0, fmt.Errorf("price %v/hr above the %v sanity ceiling: the field is probably not dollars per hour", f, MaxRate)
	}
	return r, nil
}
