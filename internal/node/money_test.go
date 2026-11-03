package node

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestParseUSD_DoesNotRoundTripThroughFloat is the reason this function is
// hand-written. strconv.ParseFloat("2.01")*1e6 is 2009999.9999999998, which
// truncates to 2009999 and makes a price compare as cheaper than the provider's
// own page says it is.
func TestParseUSD_DoesNotRoundTripThroughFloat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want MicroUSD
	}{
		{"0.29", 290000},
		{"0.34", 340000},
		{"1.11", 1110000},
		{"2.134", 2134000},
		{"0.000001", 1},
		{"0", 0},
		{"1", 1000000},
		{"$1.64", 1640000},
		{" 0.429 ", 429000},
		{".5", 500000},
		{"7.99", 7990000},
		// A seventh fractional digit is truncated, not rejected: providers do
		// publish 0.4290000000000001 after a float round-trip of their own,
		// and refusing those would hide real offers.
		{"0.4290000000000001", 429000},
		// Space on either side of the dollar sign. " $1.00" worked and
		// "$ 1.00" did not, which is a distinction nothing could justify in a
		// field an operator types by hand.
		{"$ 1.50", 1500000},
		{"$\t1.50", 1500000},
		// A fraction of nothing but zeros is zero, which is a figure an
		// operator may legitimately write: zero means no ceiling. It is the
		// boundary of the refusal below, and the reason that refusal asks
		// whether truncation discarded a *non-zero* digit.
		{"0.0000000", 0},
	} {
		got, err := ParseUSD(tc.in)
		if err != nil {
			t.Fatalf("ParseUSD(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseUSD(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseUSD_Refuses(t *testing.T) {
	for _, in := range []string{"", " ", "-1", "-0.5", "abc", "1.2.3", "1,5", "1.", "$", "1e5", "99999999999999999999"} {
		if got, err := ParseUSD(in); err == nil {
			t.Errorf("ParseUSD(%q) = %d, want an error", in, got)
		}
	}
}

// TestParseUSD_RefusesBelowOneMicrodollar. Every digit of "0.0000001" is past
// the sixth, so the seventh-digit truncation threw away the entire value and
// returned zero with a nil error -- and zero is not "a very small price"
// anywhere in ember. Budget.Allow guards on MaxRate > 0 and Filter.Match on
// MaxPrice > 0, so a ceiling typed in the wrong unit silently became no
// ceiling at all, on the one field whose job is to stop a run that costs too
// much.
func TestParseUSD_RefusesBelowOneMicrodollar(t *testing.T) {
	for _, in := range []string{"0.0000001", "0.0000009", "0.000000499", "$0.0000001", "0." + strings.Repeat("0", 40) + "7"} {
		if got, err := ParseUSD(in); err == nil {
			t.Errorf("ParseUSD(%q) = %d, want an error: a positive price that reads as zero reads as no ceiling", in, got)
		}
	}
}

// TestParseUSD_ErrorsQuoteWhatWasTyped. The errors used to quote the trimmed
// copy, so `max_hourly_usd: "$1,000"` reported `malformed price "1,000"` -- a
// string the operator never wrote, with the one character that explains
// nothing removed and the one that caused it left in.
func TestParseUSD_ErrorsQuoteWhatWasTyped(t *testing.T) {
	for _, in := range []string{"$1,000", "$abc", "$-1", "$0.0000001"} {
		_, err := ParseUSD(in)
		if err == nil {
			t.Fatalf("ParseUSD(%q) was accepted", in)
		}
		if !strings.Contains(err.Error(), strconv.Quote(in)) {
			t.Errorf("ParseUSD(%q) error %q does not quote the string as typed", in, err)
		}
	}
}

// TestParseUSD_RefusalsSayWhatToWrite. `malformed price "$1,000"` was the
// whole message for every spelling ParseUSD declines for its shape, while the
// function's own doc comment enumerated the answer, so an operator told their
// string was malformed had to guess which of eight rules they broke. There is
// one input per place the refusal is raised -- a bare trailing point, a bad
// character in the whole part, a bad character in the fraction -- because
// each is its own return statement and a wording that reaches two of three is
// a wording that reaches the one nobody tried.
//
// The assertion is on the substance that makes it a remediation: the accepted
// form, an example the operator can copy, the rule their string most likely
// broke, and the reason there is a rule. A test that checked only that the
// message is longer would pass for any padding.
func TestParseUSD_RefusalsSayWhatToWrite(t *testing.T) {
	shape := []string{
		"plain decimal number of dollars",
		"1.79, 0.429, $2.49",
		"thousands separator",
		"exponent",
		"wrong by orders of magnitude",
	}
	for _, in := range []string{"5.", "$1,000", "1e3", "0x10", "1.5x", "1 000"} {
		_, err := ParseUSD(in)
		if err == nil {
			t.Fatalf("ParseUSD(%q) was accepted", in)
		}
		for _, want := range shape {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseUSD(%q) error %q does not carry %q", in, err, want)
			}
		}
	}

	// The range refusal is about overflow and says so, and it says what
	// overflowed rather than reaching for a policy ceiling ParseUSD
	// deliberately does not have.
	_, err := ParseUSD("99999999999999999999")
	if err == nil {
		t.Fatal("an amount past int64 microdollars was accepted")
	}
	for _, want := range []string{"out of range", "922337203685 dollars", "a tenth of what an int64 of microdollars can hold", "not cents or microdollars"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("out-of-range error %q does not carry %q", err, want)
		}
	}
}

// TestParseUSD_TheRangeNamedIsTheRangeEnforced. The message quotes the ceiling
// and the check enforces it, and a message that names a figure the code does
// not hold to is the comment that justifies the entry somebody remembered.
func TestParseUSD_TheRangeNamedIsTheRangeEnforced(t *testing.T) {
	if _, err := ParseUSD("922337203685"); err != nil {
		t.Errorf("the largest whole part the message names was refused: %v", err)
	}
	if _, err := ParseUSD("922337203686"); err == nil {
		t.Error("one dollar past the figure the message names was accepted")
	}
}

// TestRate_CostRoundsUp: an estimate that reads low is an estimate that lets a
// run start which the budget would have refused.
func TestRate_CostRoundsUp(t *testing.T) {
	r := Rate(1000000) // $1.00/hr
	for _, tc := range []struct {
		d    time.Duration
		want MicroUSD
	}{
		{time.Hour, 1000000},
		{30 * time.Minute, 500000},
		{time.Second, 278}, // 277.77… rounds up
		{0, 0},
		{-time.Hour, 0},
	} {
		if got := r.Cost(tc.d); got != tc.want {
			t.Errorf("Rate(%d).Cost(%v) = %d, want %d", r, tc.d, got, tc.want)
		}
	}
}

func TestMoney_Strings(t *testing.T) {
	if got := MicroUSD(340000).String(); got != "$0.34" {
		t.Errorf("MicroUSD.String() = %q", got)
	}
	if got := Rate(2134000).String(); got != "$2.134/hr" {
		t.Errorf("Rate.String() = %q", got)
	}
}

// TestRateFromUSD_RoundsRatherThanTruncates. This is the only place a float is
// allowed to become money, and int64(2.01 * 1e6) is 2009999 because 2.01 has
// no exact binary representation. Truncating would under-report the price, and
// a budget check would then pass on a node a hair over the ceiling.
//
// 2.01 rather than 0.44, which was the example until it was checked: 0.44
// comes out exactly 440000 and so proves nothing here. The table keeps both,
// because a case that passes either way is still a case the next person does
// not have to wonder about.
func TestRateFromUSD_RoundsRatherThanTruncates(t *testing.T) {
	for in, want := range map[float64]Rate{
		2.01:      2_010_000, // int64(2.01*1e6) is 2009999; rounding is what fixes it
		0.44:      440_000,   // comes out exact, and is kept to show that some do
		0.69:      690_000,
		2.69:      2_690_000,
		0.001:     1_000,
		1:         1_000_000,
		15.92:     15_920_000,
		0.0000005: 1, // half a microdollar rounds up, not away
	} {
		if got := RateFromUSD(in); got != want {
			t.Errorf("RateFromUSD(%v) = %d micro, want %d", in, int64(got), int64(want))
		}
	}
}

// TestRateFromUSD_RefusesNonsense. A provider quoting NaN or a negative price
// is one ember should not rent from, and zero is distinguishable by the caller
// from a real price -- "declined to quote" and "free" are different, and only
// the local provider is the second one.
func TestRateFromUSD_RefusesNonsense(t *testing.T) {
	for _, in := range []float64{
		math.NaN(), math.Inf(1), math.Inf(-1), -1, -0.0001, 0,
	} {
		if got := RateFromUSD(in); got != 0 {
			t.Errorf("RateFromUSD(%v) = %d, want 0", in, int64(got))
		}
	}
}

// TestGiB_ConvertsProviderVRAMFigures. RunPod reports "24" meaning GB while
// Offer carries MiB; a straight assignment makes a 24 GB card report 24 MiB,
// nothing errors, and every --min-vram filter rejects all real hardware.
func TestGiB(t *testing.T) {
	for in, want := range map[int]MiB{
		0:   0,
		24:  24 * 1024,
		80:  81920,
		192: 196608,
	} {
		if got := GiB(in); got != want {
			t.Errorf("GiB(%d) = %v, want %v", in, got, want)
		}
	}
	if MiBPerGiB != 1024 {
		t.Errorf("MiBPerGiB = %d", MiBPerGiB)
	}
}

// TestRateCost_DoesNotOverflow is a regression test for the most consequential
// bug in this package's history.
//
// `int64(rate) * int64(duration)` with the duration in nanoseconds exceeds
// int64 for any rate above roughly $2.56/hour over an hour. An H100 at
// $2.69/hour for one hour computed as *negative* $2.43; an 8-GPU node at
// $15.92/hour for one hour computed as a cheerful $0.55.
//
// provision.Budget.Allow calls Cost to decide whether a run is within its
// ceiling, so the budget silently waved through exactly the expensive hardware
// it exists to guard against -- and worked correctly for every cheap card,
// which is why nothing noticed. Each case below is a real provider rate.
func TestRateCost_DoesNotOverflow(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate Rate
		d    time.Duration
		want MicroUSD
	}{
		{"rtx4090 for an hour", 690_000, time.Hour, 690_000},
		{"h100 for an hour", 2_690_000, time.Hour, 2_690_000},
		{"h100 for 90 minutes", 2_690_000, 90 * time.Minute, 4_035_000},
		{"h100 for 24 hours", 2_690_000, 24 * time.Hour, 64_560_000},
		{"8x h100 for 10 minutes", 15_920_000, 10 * time.Minute, 2_653_334},
		{"8x h100 for an hour", 15_920_000, time.Hour, 15_920_000},
		{"8x h100 for a week", 15_920_000, 7 * 24 * time.Hour, 2_674_560_000},
		{"the sanity ceiling for a day", MaxRate, 24 * time.Hour, 24_000_000_000},
	} {
		got := tc.rate.Cost(tc.d)
		if got != tc.want {
			t.Errorf("%s: %v over %v = %v (%d micro), want %v (%d micro)",
				tc.name, tc.rate, tc.d, got, int64(got), tc.want, int64(tc.want))
		}
		if got < 0 {
			t.Errorf("%s: a negative cost would offset a real one in any sum", tc.name)
		}
	}
}

// TestRateCost_IsMonotonic. Whatever the arithmetic does internally, a longer
// run must never cost less than a shorter one -- which is the single property
// that would have caught the overflow without anybody knowing the bit widths.
func TestRateCost_IsMonotonic(t *testing.T) {
	for _, rate := range []Rate{690_000, 2_690_000, 15_920_000, MaxRate} {
		prev := MicroUSD(0)
		for mins := 1; mins <= 60*24; mins = mins * 3 {
			got := rate.Cost(time.Duration(mins) * time.Minute)
			if got < prev {
				t.Fatalf("%v: %d minutes cost %v, less than the shorter run's %v", rate, mins, got, prev)
			}
			prev = got
		}
	}
}

// TestRateCost_SaturatesRatherThanWrapping. A wrapped total is a number a
// budget check will happily accept; an enormous one is refused, which is the
// safe direction for a figure ember could not compute.
func TestRateCost_SaturatesRatherThanWrapping(t *testing.T) {
	for _, tc := range []struct {
		rate Rate
		d    time.Duration
	}{
		{Rate(math.MaxInt64), time.Duration(math.MaxInt64)},
		{Rate(math.MaxInt64 / 2), time.Duration(math.MaxInt64)},
		{Rate(math.MaxInt64), time.Hour},
		// Whole hours that just fit, plus a remainder that pushes the sum
		// over: the final addition has to saturate too, not only the
		// multiplication.
		{Rate(math.MaxInt64 / 2), 2*time.Hour + time.Second},
	} {
		got := tc.rate.Cost(tc.d)
		if got < 0 {
			t.Errorf("%v over %v wrapped to %v", tc.rate, tc.d, got)
		}
		if got != MaxMicroUSD {
			t.Errorf("%v over %v = %v, want saturation at %v", tc.rate, tc.d, got, MaxMicroUSD)
		}
	}
}

func TestRateCost_ZeroAndNegative(t *testing.T) {
	if got := Rate(690_000).Cost(0); got != 0 {
		t.Errorf("got %v", got)
	}
	if got := Rate(690_000).Cost(-time.Hour); got != 0 {
		t.Errorf("a negative duration cost %v", got)
	}
	if got := Rate(0).Cost(time.Hour); got != 0 {
		t.Errorf("a free rate cost %v", got)
	}
}

// TestRateCost_RoundsUpOnTheRemainder, which is where the rounding now lives:
// whole hours are exact and only the sub-hour part can have a fraction.
func TestRateCost_RoundsUpOnTheRemainder(t *testing.T) {
	// One microdollar an hour for one second is a tiny fraction, and must
	// still be charged as a whole microdollar.
	if got := Rate(1).Cost(time.Second); got != 1 {
		t.Errorf("got %d micro, want 1", int64(got))
	}
	// Exactly two hours has no remainder at all.
	if got := Rate(1).Cost(2 * time.Hour); got != 2 {
		t.Errorf("got %d micro, want 2", int64(got))
	}
	// Two hours and a second rounds the second up.
	if got := Rate(1).Cost(2*time.Hour + time.Second); got != 3 {
		t.Errorf("got %d micro, want 3", int64(got))
	}
}
