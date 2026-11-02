package node

import (
	"math"
	"math/big"
	"strings"
	"testing"
	"unicode"
)

// oracleParseUSD is a second implementation of ParseUSD, written from the
// rules rather than from the code, against which the fuzzer compares it.
//
// The precedent is internal/awssig, which is checked against AWS's own
// published values rather than against a server because its only other
// feedback is a 403 with no detail. ParseUSD's only other feedback is a price
// that silently compares as cheaper than it is: nothing downstream of it can
// tell a correctly parsed $0.69 from a $6.90 that lost a digit, because both
// are a plausible rate for a real card. There are no published vectors for a
// decimal string, so the oracle is the arithmetic done a different way --
// exactly, in math/big, with no int64 anywhere and no digit loop.
//
// The two must agree about the *domain* decisions and not only the
// arithmetic, which is the whole difficulty: an oracle that rounded the
// seventh fractional digit instead of truncating it would report a
// differential on every price a provider has ever published, and an oracle
// that accepted a negative or an exponent would report one on every refusal
// ember makes on purpose. So each rule below names which decision it mirrors.
//
// It returns the value and whether the input is accepted. The accept/refuse
// classification is as much of the contract as the number: a refusal that
// turns into a zero is the bug this file exists to find.
func oracleParseUSD(in string) (*big.Int, bool) {
	// Trimming is strings.TrimSpace in both, deliberately: the oracle is a
	// second derivation of ember's rules, not of unicode.IsSpace. What is
	// independent is everything after it.
	s := strings.TrimSpace(in)
	s = strings.TrimSpace(strings.TrimPrefix(s, "$"))
	if s == "" {
		return nil, false // empty price
	}
	if strings.HasPrefix(s, "-") {
		return nil, false // a negative rate is a field ember has misread
	}

	// At most one point, and ".5" means "0.5" -- a spelling providers emit.
	// A trailing point ("5.") is refused: it is a number somebody stopped
	// typing, and reading it as 5 guesses at what they meant.
	whole, frac := s, ""
	switch n := strings.Count(s, "."); n {
	case 0:
	case 1:
		i := strings.IndexByte(s, '.')
		whole, frac = s[:i], s[i+1:]
		if frac == "" {
			return nil, false
		}
	default:
		return nil, false
	}

	// ASCII digits only, so no exponent, no sign, no separator, no radix
	// prefix, no non-ASCII digit and no internal space survives. Checked
	// here by a predicate over the whole string rather than inside an
	// accumulating loop, which is the point of writing it twice.
	if !asciiDigits(whole) || !asciiDigits(frac) {
		return nil, false
	}

	// The whole part's ceiling. ParseUSD refuses above MaxInt64/micros/10,
	// which is a tenth of what MicroUSD can hold -- conservative, and the
	// oracle mirrors the conservative bound rather than the representable
	// one, because what is being checked is agreement and not taste.
	// Leading zeros are stripped first: "0000001" is a dollar, and the
	// length shortcut exists only so a megabyte of digits from the fuzzer
	// does not become a megabyte-wide big.Int.
	w := strings.TrimLeft(whole, "0")
	const ceiling = math.MaxInt64 / micros / 10
	if len(w) > len("922337203685") {
		return nil, false
	}
	wi := big.NewInt(0)
	if w != "" {
		if _, ok := wi.SetString(w, 10); !ok {
			return nil, false
		}
	}
	if wi.Cmp(big.NewInt(ceiling)) > 0 {
		return nil, false
	}

	// floor(value * 10^6), as a division of exact rationals. That is the
	// seventh-digit truncation derived from arithmetic instead of from
	// skipping characters: the tail past the sixth digit is strictly less
	// than one microdollar, so it cannot change the floor -- which is also
	// why cutting the fraction short here is exact. The cut is far past six
	// to keep that obvious rather than tight.
	const irrelevantPast = 32
	cut := frac
	if len(cut) > irrelevantPast {
		cut = cut[:irrelevantPast]
	}
	num, ok := new(big.Int).SetString("0"+whole+cut, 10)
	if !ok {
		return nil, false
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(cut))), nil)
	value := new(big.Rat).SetFrac(num, den)
	scaled := new(big.Rat).Mul(value, new(big.Rat).SetInt64(micros))
	micro := new(big.Int).Quo(scaled.Num(), scaled.Denom()) // both positive, so Quo is floor

	// A positive amount that truncates to nothing is refused rather than
	// reported as zero, because zero means "no ceiling" to Budget.Allow and
	// to Filter.Match. Asked of the whole fraction, not of the 32 digits
	// above: "0." then thirty zeros then a 1 is still a price somebody wrote.
	if micro.Sign() == 0 && strings.Trim(whole+frac, "0") != "" {
		return nil, false
	}
	return micro, true
}

func asciiDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// FuzzParseUSD compares ParseUSD against the oracle above.
//
// The seed corpus is every price spelling this repository actually contains --
// the ceilings in the example configs and the config tests, the provider
// prices in the fixtures, ember's own per-image figures -- plus every case
// that was enumerated by hand while settling #53 and found to be undocumented:
// an exponent, a thousands separator, a sign, a unicode minus, non-ASCII
// digits, a radix prefix, leading zeros, a space inside the number, a
// sub-microdollar amount, and a fraction long enough to truncate.
//
// It is meaningful without -fuzz: `go test` runs every seed through both
// implementations, so the enumeration is a table test that happens to also be
// a corpus. -fuzz is what adds the spellings nobody thought of.
func FuzzParseUSD(f *testing.F) {
	for _, s := range []string{
		// Real spellings: example configs, config tests, provider fixtures.
		"0.50", "1.00", "2.60", "3.00", "10.00", "20.00", "25.00", "100.00",
		"250.00", "0.25", "8.00", "12.6", "0.29", "2.01", "0.34", "1.11",
		"2.134", "7.99", "$1.64", " 0.429 ", ".5", "1", "0",
		// ember's own per-image figures, which are why cents are not enough.
		"0.4475", "0.17375",
		// A provider's float round-trip, which is why the seventh digit is
		// truncated rather than refused.
		"0.4290000000000001", "0.9999999", "0.000001", "0.0000015",
		// Refused on purpose, each for its own reason.
		"", " ", "$", "-1", "-0.5", "-0", "abc", "1.2.3", "1,5", "1,000",
		"$1,000", "1.", "5.", ".", "..", "1e5", "1e3", "1E3", "1e-3",
		"99999999999999999999", "+1", "+0.5", "−1", "0x10", "0b101",
		"1_000", "NaN", "Inf", "١1", "１", "１.５", "1 000",
		"1 .5", "1. 5", "1$", "$$1",
		// The bug this file was written for: positive, below a microdollar.
		"0.0000001", "0.0000009", "0.000000499", "0.00000000000000000001",
		// And its boundary: an explicit zero, however it is spelled.
		"0.0000000", "0.000000", "00.00", "000",
		// Whitespace around the dollar sign, both ways round.
		" $1.00", "$ 1.00", "$\t1.50",
		// The range ceiling and one past it.
		"922337203685", "922337203686", "0000000000000000000001",
		// Long fractions, with and without anything to truncate.
		"0.429" + strings.Repeat("0", 64), "0." + strings.Repeat("9", 64),
		"0." + strings.Repeat("0", 40) + "7",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, err := ParseUSD(in)
		want, ok := oracleParseUSD(in)

		if err != nil {
			if ok {
				t.Fatalf("ParseUSD(%q) = %v, oracle accepts it as %v micro", in, err, want)
			}
			// Error strings are lowercase and carry the remediation; a
			// message that starts with a capital is one that was written
			// without reading the rest of this package.
			msg := err.Error()
			if msg == "" || unicode.IsUpper([]rune(msg)[0]) {
				t.Fatalf("ParseUSD(%q) error %q is not a lowercase error string", in, msg)
			}
			return
		}
		if !ok {
			t.Fatalf("ParseUSD(%q) = %d micro, oracle refuses it", in, int64(got))
		}
		if got < 0 {
			t.Fatalf("ParseUSD(%q) = %d, a negative price would offset a real one in any sum", in, int64(got))
		}
		if want.Cmp(big.NewInt(int64(got))) != 0 {
			t.Fatalf("ParseUSD(%q) = %d micro, oracle says %v micro", in, int64(got), want)
		}
	})
}
