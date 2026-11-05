# Style, measured

`AGENTS.md` states the style rules. These are the passes that checked
whether the code actually follows them, and what they found.


Comments explain *why*, and name the defect that motivated them — see
`node.NormalizeGPU`, where substring matching once turned `"A100 SXM4 80GB"`
into `a1004-80g` and made a whole provider look like it had no capacity.

Prefer making a bug unrepresentable over testing for it. Fail loudly rather
than skip quietly. Allow-lists for anything dangerous. Parse, don't grep. Pin
external inputs.

Error strings are lowercase, wrapped with `%w`, and carry the remediation.
`main` prints `ember: <err>` to stderr and exits 1, or 2 when there are
findings, or 3 on a usage error.

- **Two of those three claims are now measured, and both hold.** Nobody had
  measured any of them, and three of the last four audits of this file found a
  rule it states that was not true of the code -- `ember ls --all` was cited
  five times here and four more in comments as a design precedent while never
  having existed. So this is the vindication rather than the usual entry, and
  it is written down for the same reason the volume-bandwidth measurement is:
  it is how you learn the conclusion did not depend on the guess.
- **Lowercase: 915 `errors.New`/`fmt.Errorf` sites, 34 begin with an uppercase
  rune, and none of the 34 is a violation.** Thirty-one are
  `providertest/conformance.go` naming the `provider.Provider` method the
  check is about -- `Name`, `Offers`, `Create`, `List`, `Inspect`, `Destroy`,
  `LeftoverList` -- which is the identifier exception, and they reach
  `t.Errorf` rather than `ember: <err>`; two more are `Cancel` in a queue
  test, and the last is `RunPod`. A further 80 begin with a verb, so the first
  rune belongs to the value: an HTTP method, an `EMBER_*` name, a path, a
  provider name. The 13 `Error()` methods were read by hand: eleven open with
  a lowercase literal, and the two that can open with a capital are
  `httpx.StatusError` and `s3.StatusError`, whose first word is `e.Method` --
  an HTTP verb, the identifier exception again. `sdserver.APIError` leads with
  the server's own `code`, which every captured response spells in lowercase
  snake case, so that one is the server's manners rather than ember's rule.
  Run with `go/parser` over **all 164 commits in this
  repository's history**, the same pass finds those nine leading words and no
  others, ever: **the lowercase rule has never once been broken here.**
- **`%w`: eleven sites print an error with another verb, and none of them is a
  `%w` that was meant.** Five are `fmt.Sprintf` building a *string field* --
  `reconcile.Finding.Reason` in all three scans, `transfer`'s skipped-name
  list, the `why` inside `gpucheck.NotReadyError.Error` -- where `%w` is not a
  legal verb, and the chain is kept by `Unwrap` wherever there is one to keep.
  Five are `providertest` assertions quoting the provider's error as evidence,
  one of them asserting that the error does **not** wrap `node.ErrNotFound`,
  where wrapping would make the diagnostic claim the opposite of the check.
  The eleventh is `s3.multipart`'s abort failure beside a primary `%w`, which
  could be a second `%w` and buys nothing today. **None can change an exit
  code, and that is measured rather than reasoned**: `cli.ExitCode` branches on
  `*cli.FindingsError`, `cli.usageError` and `app.ErrUsage`; the first two are
  constructed only in `internal/cli` and the third only in `internal/app`, and
  not one of the eleven sites is in either package. The structural version of
  the same defect is absent too -- every error type here that holds an inner
  error has an `Unwrap`, and `app.usageError`'s returns `[]error{e.err,
  ErrUsage}` so exit 3 survives the wrap.
- **No guard for the first two, and the measurement is why.** `go/ast` finds
  every site and the first rune is one line; the exceptions are the whole
  difficulty. An allow-list of proper nouns and identifiers would need nine
  words today and would have needed editing **three times in 164 commits** --
  once for `RunPod`, once for `Cancel`, and once when
  `providertest/conformance.go` landed six method names at a stroke. Against
  that it has **never had a defect to catch**, because the historical pass
  above finds zero. A check with no true positive available and a false
  positive every fifty commits is exactly what `nolisten_test.go` records
  about its own first version, which flagged `loopback.Listen`. Deriving the
  exception from the tree instead of maintaining it beside the code -- the
  `LAB_TAGS` rule, and the obvious candidate here -- is *worse*: `Read`,
  `Open`, `Write`, `Create` and `Get` are all method names in this module, so
  `fmt.Errorf("Create the directory: %w", err)` would pass a guard that
  accepts any leading word the tree declares, and a capitalised verb is
  precisely the shape a real violation takes. Both scanners were proved able
  to fail before any of this was believed: a stray `"Could not read the bind
  address"` is reported and a lowercase `"docker could not be reached"` is
  not, and a `%w` turned `%v` in `loopback.ParseBind` is reported while the
  same message reworded with its `%w` intact is not.
- **The remediation half has no count, and it is not claimed to.** Of 188 leaf
  messages in `cli`, `config`, `app`, `loopback` and `node` -- the ones that
  are not a bare `: %w` pass-through -- about two thirds name a flag, a key, a
  file or a command, and the best are very good: the credentials-file refusal
  prints `chmod 600 <path>`, the missing-S3-credential one names the page
  RunPod issues them on and both keys to write, the architecture refusal names
  `make publish-sd-cuda CUDA_ARCH=<n>`. The residue splits three ways and only
  the third is a gap. Most of it is `node`'s `Validate` family refusing a
  struct ember or a provider built, where there is no operator action to name
  and inventing one would be a fiction. Some is `node/errors.go`'s sentinels,
  deliberately short because the provider wraps them with the remediation --
  `"%w: no CPU machine is available in %s, where the store lives"`. What is
  left is a few terse refusals of input an operator typed, and `ParseUSD`'s
  `malformed price %q` is the sharpest of them: its own doc comment enumerates
  every spelling it refuses and why, the message repeats none of it, and the
  field is the one whose entire job is to stop a run that costs too much.
  Filed rather than reworded here, because a message is read before it is
  grepped and three of them want one wording rather than three.
- **That sharpest gap is closed (#124), with the one wording the entry above
  asked for, and the key is the caller's.** `ParseUSD` is shared by four keys
  -- `max_hourly_usd`, `max_run_usd`, `max_day_usd`, `max_month_usd` -- so it
  cannot name one, and does not need to: every call site already wraps with
  `%w` under its own key. `TestBudgets_NameTheKeyAndWhatToWrite` holds the
  five in `internal/config` to key and wording; the two in
  `internal/webapi/wire.go` are asserted for the key alone
  (`TestPostRequest_RefusesWhatItCannotParse`), so gutting the wording leaves
  the webapi suite green. The parser carries the other half, in one
  `malformedPrice` for all three places a wrong *shape* is refused -- the
  accepted form, copyable examples, the rules by name and the reason there is
  a rule -- rather than a message per rule, because the per-rule list is the
  fuzz corpus's and a second copy of it is how they drift. The original string
  is still quoted as typed. `price out of range` got its own wording,
  naming the bound from the constant the check uses -- a conservative tenth of
  what an int64 of microdollars holds, and the constant's comment records why
  widening it yields a negative amount for `9223372036854.999999` -- and
  explicitly not a policy ceiling. `empty price` and `negative price` are
  unchanged: they are self-describing and the argument for a remediation is
  weakest there. **The classification is untouched and that is the claim worth
  checking**: the diff changes error constructors and one named constant, and
  `FuzzParseUSD`'s oracle agreed for 12.9 million executions after it. Each of
  the three shape sites, the range wording, the example, the named rule and
  the stated reason was reinstated separately and went red; a no-op patch
  stayed green.

Document CLI flag, config key and behaviour changes in `README.md` in the same
commit.
