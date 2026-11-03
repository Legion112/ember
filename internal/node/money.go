package node

import (
	"fmt"
	"math"
	"math/bits"
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

// MaxMicroUSD saturates a cost that cannot be represented.
//
// Reaching it means the inputs were nonsense; the value is deliberately
// enormous so that any budget comparison against it refuses, which is the safe
// direction for a figure ember could not compute.
const MaxMicroUSD = MicroUSD(math.MaxInt64)

// Cost is what r comes to over d, rounded up to the next microdollar.
//
// Rounding up rather than to nearest: an estimate that reads low is an
// estimate that lets a run start which the budget would have refused.
//
// The arithmetic is split because the obvious spelling overflows, and it
// overflowed in production code here for weeks. `int64(r) * int64(d)` with d
// in nanoseconds exceeds int64 for any rate above about $2.56/hour over an
// hour: an H100 at $2.69/hour for one hour computed as *negative* $2.43, and
// an 8-GPU node at $15.92/hour for one hour computed as a cheerful $0.55.
//
// That is not a cosmetic bug. provision.Budget.Allow calls this to decide
// whether a run is within its ceiling, so the budget silently waved through
// exactly the expensive hardware it exists to guard against -- while working
// correctly for every cheap card, which is why nothing noticed.
//
// So: whole hours are multiplied directly, where the product is bounded by the
// rate times a few million hours; the sub-hour remainder goes through a
// 128-bit intermediate, because r times a sub-hour nanosecond count genuinely
// needs more than 64 bits.
func (r Rate) Cost(d time.Duration) MicroUSD {
	if d <= 0 || r <= 0 {
		return 0
	}

	hours := int64(d / time.Hour)
	rem := int64(d % time.Hour)

	whole := int64(r) * hours
	if hours != 0 && whole/hours != int64(r) {
		// The division check is the standard overflow test for a product
		// that has already been computed. Saturating rather than wrapping:
		// a wrapped total is a number a budget check will happily accept.
		return MaxMicroUSD
	}

	// (r * rem) / hour, with the product allowed to exceed 64 bits.
	//
	// bits.Div64 panics when the high word is at least the divisor, and that
	// cannot happen here: r is at most 2^63-1 and rem is below one hour in
	// nanoseconds, which is under 2^42, so the product is under 2^105 and the
	// high word is under 2^41 -- comfortably below time.Hour's 3.6e12. The
	// bound is written out rather than guarded by an unreachable branch,
	// because a branch that can never be taken is a branch nobody can show
	// is right.
	// Both operands are non-negative: r is positive by the guard at the top,
	// and rem is a remainder of a positive duration.
	hi, lo := bits.Mul64(uint64(r), uint64(rem)) //nolint:gosec // r > 0 and rem >= 0, checked above
	q, rr := bits.Div64(hi, lo, uint64(time.Hour))
	if rr != 0 {
		q++
	}

	// q is at most r: it is (r * rem) / hour with rem below one hour, so the
	// quotient cannot exceed r, which is already an int64. The sum is still
	// checked, because that is the one step the bound above does not cover.
	total := whole + int64(q) //nolint:gosec // q <= r <= MaxInt64, see above
	if total < whole {
		return MaxMicroUSD
	}
	return MicroUSD(total)
}

// RateFromUSD converts a provider's float price per hour into an exact rate.
//
// This is the only place a float is allowed to become money, and it exists
// because provider APIs quote prices as JSON numbers. Everything above it is
// integer microdollars, so a budget ceiling is an exact comparison and
// 0.1+0.2 > 0.3 cannot bypass it.
//
// Rounded, not truncated. int64(2.01 * 1e6) is 2009999, because 2.01 has no
// exact binary representation -- so truncation would quietly under-report the
// price and a budget check would pass on a node a hair over the ceiling.
//
// The example used to be 0.44, which does NOT truncate: it comes out exactly
// 440000, as 0.29 comes out exactly 290000. That is the stronger argument
// rather than a weaker one -- the error cancels for some values and not for
// others, so there is no telling by looking which prices survive the trip,
// and rounding is the only rule that does not have to know.
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
// it is wrong for exactly the values that matter: "2.01" becomes
// 2.0099999999999998 and then 2009999 microdollars, so a price that reads
// identical to the provider's web page compares as cheaper than it is.
//
// "0.29" was the example here and it does not truncate -- it is
// 0.28999999999999998 exactly as stated, and times a million it is exactly
// 290000, because the error cancels. Which is why this function exists at all
// rather than a documented list of bad values: you cannot tell by looking.
//
// What it accepts and refuses is enumerated in money_fuzz_test.go's seed
// corpus and checked against a math/big.Rat oracle on every run, because the
// only other feedback this function has is a price that silently compares as
// cheaper than it is. Refused today, each for a reason: an exponent ("1e3"),
// a thousands separator ("1,000"), a sign ("+1", "-1", and a unicode minus),
// a trailing point ("5."), a radix prefix ("0x10"), non-ASCII digits, and any
// space inside the number. None of those is a spelling a provider or a config
// has ever produced, and every one of them has a reading that is wrong by
// orders of magnitude rather than by a rounding error.
func ParseUSD(s string) (MicroUSD, error) {
	// Errors quote the string as typed. Quoting the trimmed copy meant
	// `max_hourly_usd: "$1,000"` reported `malformed price "1,000"` -- a
	// string the operator never wrote, with the one character that explains
	// nothing removed and the one that caused it left in.
	orig := s

	s = strings.TrimSpace(s)
	// Trimmed on both sides of the "$", because trimming only before it
	// accepted " $1.00" and refused "$ 1.00", which is an arbitrary
	// distinction in a field an operator types by hand.
	s = strings.TrimSpace(strings.TrimPrefix(s, "$"))
	if s == "" {
		return 0, fmt.Errorf("empty price")
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("negative price %q", orig)
	}

	// s is non-empty here, so a missing whole part means the string began with
	// "." — ".5" — which is a spelling providers do emit.
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if hasFrac && frac == "" {
		return 0, malformedPrice(orig)
	}

	var cents int64
	for _, c := range whole {
		if c < '0' || c > '9' {
			return 0, malformedPrice(orig)
		}
		cents = cents*10 + int64(c-'0')
		if cents > maxWholeDollars {
			return 0, fmt.Errorf("price %q is out of range: the whole part cannot exceed %d dollars, a tenth of what an int64 of microdollars can hold -- check the figure is in dollars and not cents or microdollars", orig, int64(maxWholeDollars))
		}
	}
	total := cents * micros

	// Six fractional digits is the whole precision of MicroUSD. A seventh
	// digit is truncated rather than rejected, because providers do publish
	// prices like "0.4290000000000001" after a float round-trip of their own
	// and refusing those would make ember unable to see real offers.
	//
	// truncated records that the truncation threw away something, because on
	// one input it throws away the entire value: every digit of "0.0000001"
	// is past the sixth, so the result was zero and the error was nil. Zero
	// is not "a very small price" anywhere in ember -- Budget.Allow guards on
	// MaxRate > 0 and Filter.Match on MaxPrice > 0 -- so a ceiling typed in
	// the wrong unit silently became *no ceiling at all*, on the one field
	// whose whole job is to stop a run that costs too much. The seventh-digit
	// rule is kept for every value it was written for, where it is an error
	// of one part in ten million; it is refused where it is an error of one
	// part in one, because that is not a truncation, it is a different number.
	truncated := false
	if hasFrac {
		scale := int64(micros / 10)
		for i, c := range frac {
			if c < '0' || c > '9' {
				return 0, malformedPrice(orig)
			}
			if i >= 6 {
				truncated = truncated || c != '0'
				continue
			}
			total += int64(c-'0') * scale
			scale /= 10
		}
	}
	if total == 0 && truncated {
		return 0, fmt.Errorf("price %q is positive but below one microdollar, which ember cannot tell from no ceiling at all: write 0 for no limit, or a figure of at least 0.000001", orig)
	}
	return MicroUSD(total), nil
}

// maxWholeDollars is the largest whole part ParseUSD accepts. It is a
// deliberately conservative tenth of what MicroUSD can represent (the exact
// ceiling is 9223372036854 dollars, MaxInt64/micros), and money_fuzz_test.go's
// oracle mirrors this bound rather than the representable one.
//
// Do not "correct" it to math.MaxInt64 / micros. The whole part alone would
// fit, but the fractional digits are added afterwards: ParseUSD("9223372036854.999999")
// then carries the total past MaxInt64 and returns a NEGATIVE amount with a
// nil error -- a negative money ceiling, silently. Measured, not reasoned; the
// tenth is the margin that keeps whole*micros + frac inside int64.
//
// Named so the check and the message that quotes it cannot come apart.
const maxWholeDollars = math.MaxInt64 / micros / 10

// malformedPrice is the one refusal for every spelling ParseUSD declines for
// being the wrong shape. It is one wording rather than one message per rule
// because the rules are enumerated, with the accept/refuse classification, in
// money_fuzz_test.go, and a message per rule is eight strings to keep in step
// with a list that already exists; what the operator needs is the accepted
// form and the reason there is a rule at all.
//
// The key the operator typed it under is not named here. Every caller wraps
// the error with its own key -- max_hourly_usd, max_run_usd, max_day_usd or
// max_month_usd, from the config and, for the first two, from a queued
// request -- so naming one here would be wrong for the other three.
//
// The original string is quoted, as typed: see ParseUSD.
func malformedPrice(orig string) error {
	return fmt.Errorf("malformed price %q: write a plain decimal number of dollars, optionally after a $ -- 1.79, 0.429, $2.49 -- "+
		"with no thousands separator, sign, exponent, trailing point, radix prefix, space inside the number or non-ASCII digit, "+
		"because each of those has a reading that is wrong by orders of magnitude", orig)
}

// There is deliberately no float64 entry point to Rate. `RateFromFloat` was
// one, written for Vast.ai -- #22 part 2, which does not exist -- and deleted
// having never been called. Its premise, that Vast.ai reports prices as JSON
// numbers, was read from documentation rather than from a response, which is
// the bet `allowedCudaVersions` and `capabilities.model` both lost: a schema
// read in place of a reply. Worse, it named a ceiling that was not the live
// one, so a reader of this file could believe MaxRate was enforced here when
// what enforces it is `Offer.Validate`, on every offer from every provider.
// Two ceilings for one number with one of them unreachable is the shape
// `queue.Cost` was deleted for.
//
// The two judgements it carried are the reason this note exists rather than
// nothing, because whoever writes that converter has to make them again:
//
//   - Round to NEAREST, not up. `Rate.Cost` rounds up because an estimate
//     that reads low lets a run start the budget would have refused -- but a
//     float that already *is* the provider's published price has no error to
//     bias away from, and rounding it up makes ember's displayed price
//     disagree with the invoice.
//   - Refuse NaN, either infinity, a negative, and anything above MaxRate.
//     A figure above the ceiling means the field was not dollars per hour --
//     cents, or a total rather than a rate -- and renting on it rents
//     something nobody priced.
//
// Nothing needs to re-implement the ceiling itself: an offer carrying such a
// price is refused by `Offer.Validate` whether or not its provider checked.
