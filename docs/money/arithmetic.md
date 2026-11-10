# How money is represented and parsed

`int64` microdollars, the hand-written parser, the money libraries that were
measured and refused, and the fuzz oracle.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

- **`node.State.Billable()` is true for everything except `StateDestroyed`**,
  including `StateInvalid`, `StateFailed` and `StateStopped`. A stopped RunPod
  pod still bills for its volume; "not running" is not "not charging". A state
  added later is billable by default, so the author has to think about it to
  make it free.
- **Money is `int64` microdollars, never a float.** `0.1+0.2 > 0.3` is true in
  float64, so a budget ceiling built by arithmetic occasionally refuses an
  offer at exactly the ceiling and occasionally admits one above it. A budget
  that is sometimes wrong by a rounding error is a budget nobody trusts.
  `ParseUSD` is hand-written for the same reason: `ParseFloat("2.01")*1e6` is
  2009999.9999999998, which truncates to 2009999 -- a price that compares as
  cheaper than the provider's. **`ParseFloat("0.29")*1e6` is exactly 290000**,
  which this document and seven comments claimed was 289999 until a
  reinstatement test refused to go red over it. That is the stronger argument
  rather than a weaker one: the error cancels for 0.29 and does not for 2.01,
  so there is no way to tell by looking which prices survive the trip, and the
  only safe rule is not to take it.
- **`node.RateFromFloat` is gone, and it had never been called. It is the
  sixth thing found in this repository that was written, covered and connected
  to nothing**, and the first that was never connected to anything because its
  caller had not been written yet: it converted a provider's JSON number to a
  `Rate` for Vast.ai, which is #22 part 2 and does not exist. That makes it
  speculative generality rather than abandoned code, so the `queue.Cost`
  precedent does not obviously apply and the `sdserver.Client.Cancel` one --
  wire it up, because deleting the only mechanism deletes the mechanism --
  looks like it might. It does not, for two reasons. Its premise, that Vast.ai
  prices arrive as JSON numbers, was read from documentation and not from a
  response, which is exactly the bet `allowedCudaVersions` and
  `capabilities.model` both lost; and there *is* a second mechanism for the
  half that matters, because `Offer.Validate` enforces `MaxRate` on every
  offer from every provider, so the ceiling `RateFromFloat` named was the
  unreachable copy of a live one. A reader of `money.go` could believe it was
  the enforced one, which is the `queue.Cost` mistake -- two paths for one
  number -- arrived at from the other direction. The two judgements worth
  keeping, rounding to nearest rather than up and refusing a figure above the
  ceiling, are a note where the function was, because whoever writes that
  converter has to make them again; losing them was the only way to get this
  wrong. Nothing was reinstated: a deletion plus a comment has no defect to
  put back.
- **The type stays `int64` and the parser stays hand-written, and the
  criterion that decided it is that a budget check is `>` on two integers.**
  `Budget.Allow` is `o.Price > b.MaxRate`. Every candidate replaces that with
  a method returning a value *and an error*, and the error is a currency
  mismatch -- which cannot happen in a program that knows one currency, so it
  is an unreachable branch in a package `cmd/check-coverage` gates at 100.
  This repository already refuses those twice over: "a branch that can never
  be taken is a branch nobody can show is right", and no escape hatch for a
  seal, not even for a test. Measured against the candidates rather than
  remembered, in a scratch module outside this repo.
  **`github.com/govalues/money` v0.2.4** is the closest fit and the one whose
  own documentation disagrees with itself -- the README says `uint64` and
  pkg.go.dev describes a decimal, and `integer.go` settles it: `type fint
  uint64`, wrapped as `Decimal{neg bool, scale int8, coef fint}` inside
  `Amount{Currency, Decimal}`, 24 bytes and not comparable. Pure Go, one
  transitive dependency, and it does hold `$0.17375` -- so it passes criteria
  2, 4 and 5 and fails 3.
  **`github.com/Rhymond/go-money` v1.0.15** fails criterion 2 as the issue
  predicted, and now with a number: `NewFromFloat(0.4475, "USD")` is 44 minor
  units, `$0.44`. The one check it was owed -- whether a custom currency with
  exponent 6 is definable -- is yes, `AddCurrency` takes a fraction and the
  amount then holds 447500. It is still out, because it buys that with a
  non-ISO currency code in every record, comparisons returning
  `(bool, error)`, and **no decimal-string parser at all**: there is no
  `ParseMoney` and no `NewFromString` in the package, so it cannot answer the
  parser question even if it answered the type question.
  **`github.com/shopspring/decimal` v1.6.0** is exact and has zero transitive
  dependencies, and is a `big.Int` pointer plus an exponent: a budget
  comparison allocates and a `Rate` stops being a value. Criterion 3. The
  standard library has no money type, and `golang.org/x/text/currency`
  formats rather than holds.
- **What a money library would have bought here is the one thing none of them
  sells.** The only real arithmetic bug in this package's history is
  `Rate.Cost`'s overflow -- `int64(rate) * int64(duration)` in nanoseconds,
  which read an H100 hour as *negative* $2.43 -- and that is money times a
  `time.Duration` rounded up, which no candidate does. `Cost` would stay
  hand-written with a library's error path bolted to it, so the dependency
  would replace the half that was never wrong and leave the half that was.
  The parser is the same answer from the other side: four of the five
  decisions in `ParseUSD` are about providers and config files rather than
  about money, and `govalues`'s own parser gets two of them backwards in the
  expensive direction -- it refuses `"$1.64"` and `" 0.429 "`, which ember's
  own configs and fixtures contain, and *accepts* `"1e3"` as $1000 and `"-1"`
  as minus a dollar, both of which ember refuses because a config saying
  either has been misread.
  Four and not five: the fifth, `RateFromFloat`'s `MaxRate` ceiling, sits in a
  function with **no non-test caller** -- the live ceiling is
  `Offer.Validate`. Written down rather than counted as evidence, and the
  function left alone, because removing it is the Vast.ai path's decision and
  not this one.
- **`ParseUSD` has no sanity ceiling, and that is not an oversight.**
  `RateFromFloat` refuses a rate above `MaxRate` because a provider's JSON
  number that large is a field in the wrong unit. `ParseUSD` reads
  `max_run_usd`, `max_day_usd` and `max_month_usd` as well as the hourly one,
  and a $2000 monthly ceiling is a figure an operator means. One ceiling
  across all four would refuse the three it has no business bounding.
- **A positive price that reads as zero reads as no ceiling.** Every digit of
  `"0.0000001"` is past the sixth, so the seventh-digit truncation discarded
  the whole value and `ParseUSD` returned zero and a nil error. Zero is not "a
  very small price" anywhere in ember: `Budget.Allow` guards on
  `MaxRate > 0`, `Filter.Match` on `MaxPrice > 0`, and `PeriodCeilings` says
  so in prose. So `max_hourly_usd: "0.0000001"` -- a ceiling written in the
  wrong unit, which is exactly the mistake `MaxRate` exists for -- silently
  removed the hourly ceiling, the run-total ceiling and the price filter
  together, on the one field whose entire job is to stop a run that costs too
  much. It is refused now, and only there: the truncation is kept for every
  value it was written for, where it is an error of one part in ten million,
  and refused where it is an error of one part in one, which is not a
  truncation but a different number. `"0.0000000"` is still zero, because an
  operator writing zeros means zero -- which is why the check asks whether
  truncation discarded a *non-zero* digit rather than whether it discarded
  anything.
- **The parser is fuzzed against a `math/big.Rat` oracle, and the oracle has
  to agree about the domain decisions.** The precedent is `internal/awssig`,
  checked against AWS's own published vectors because its only other feedback
  is a 403 with no detail. `ParseUSD`'s only other feedback is a price that
  silently compares as cheaper than it is, and nothing downstream can tell a
  correct $0.69 from a $6.90 that lost a digit, because both are a plausible
  rate for a real card. There are no published vectors for a decimal string,
  so the oracle is the same conversion done a different way: exact rationals,
  `floor(v * 10^6)` by division rather than by skipping characters, and digit
  validation as a predicate over the string rather than inside an accumulating
  loop. The arithmetic is not the interesting part. An oracle that *rounded*
  the seventh digit would report a differential on every price a provider has
  ever published, and one that accepted an exponent or a negative would report
  one on every refusal ember makes on purpose -- so the accept/refuse
  classification is as much of the assertion as the number, and each rule in
  the oracle names which decision it mirrors. 28 million executions over 90
  seconds found no differential, recorded because that is the outcome nobody
  would otherwise write down.
- **The seed corpus is the enumeration, so it runs without `-fuzz`.** What
  `ParseUSD` does with `"1e3"`, `"$1,000"`, `"+0.5"`, a unicode minus,
  non-ASCII digits, `"0x10"`, leading zeros, a space inside the number and a
  fraction long enough to truncate was written down nowhere. Every one of them
  is a seed now, beside every price spelling in the example configs and the
  provider fixtures, so `go test` replays the whole enumeration through both
  implementations whether or not anybody passes `-fuzz`. A corpus that only
  runs under a flag is a table test nobody runs.
- **A price error quotes the string as typed.** The errors used to quote the
  trimmed copy, so `max_hourly_usd: "$1,000"` reported `malformed price
  "1,000"` -- a string the operator never wrote, with the one character that
  explains nothing removed and the one that caused it still there. For the
  same reason the `$` is trimmed on both sides: `" $1.00"` parsed and
  `"$ 1.00"` did not, which is a distinction nothing could justify in a field
  an operator types by hand.
