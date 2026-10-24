package node

import (
	"math"
	"testing"
	"time"
)

// TestParseUSD_DoesNotRoundTripThroughFloat is the reason this function is
// hand-written. strconv.ParseFloat("0.29")*1e6 is 289999.99999999994, which
// truncates to 289999 and makes a price compare as cheaper than the provider's
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

func TestRateFromFloat_RefusesNonFiniteNegativeAndAbsurd(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.5, 5000} {
		if got, err := RateFromFloat(f); err == nil {
			t.Errorf("RateFromFloat(%v) = %v, want an error", f, got)
		}
	}
}

func TestRateFromFloat_Accepts(t *testing.T) {
	got, err := RateFromFloat(2.134)
	if err != nil {
		t.Fatal(err)
	}
	if want := Rate(2134000); got != want {
		t.Errorf("RateFromFloat(2.134) = %d, want %d", got, want)
	}
	if got, err := RateFromFloat(0); err != nil || got != 0 {
		t.Errorf("RateFromFloat(0) = %d, %v", got, err)
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
