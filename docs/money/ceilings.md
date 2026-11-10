# The four spending ceilings

`max_hourly_usd`, `max_run_usd`, `max_day_usd`, `max_month_usd` — and the
sweep that found three of the five renting paths were not gated at all.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

- **The period ceiling on the store fill could not fail either, and sweeping
  the class found two more on the two lines above it.** #155 found the
  `--allow-paid` gate on the queue's own store-filling rental green over its
  own deletion, because the fake served no CPU catalogue and the run stopped
  at a 404 before the gate was reached. (The first wording said that entry was
  "four entries up". It was two at the time and three after a rebase, which is
  why there is no count here: a positional reference in this file is the
  hand-maintained list with no list.) This file's
  sentence covers **both** calls -- the sync "is a rental and is gated like one
  -- `refusePaid` and the period ceilings, the same two calls the GPU rental
  gets" -- and the second half was in the same state for the same reason one
  line later. Measured, with the call replaced by a marker so the assertion is
  on text the patch INTRODUCES: deleting
  `p.refuseOverPeriod(ctx, cfg.TTL, sync.Offer.Price)` from `fillStore` was
  **exit 0** across `go test ./...`, and **exit 1** with the new assertion.
  **Nothing anywhere under `test/` configures `max_day_usd` or
  `max_month_usd`, which is still true** -- so `Ceilings.IsZero()`
  short-circuits in every lab there is and the deletion changes no answer in
  any of them. The lab suite is green over the defect for exactly the reason
  the unit suite was.

  **A UNIT TEST AND NOT A LAB, and the reason is the inverse of #155's step
  8.** That assertion is the money a rental CAME TO, which only the store lab
  can make: every other lab rents from the local provider at zero, so a cost
  assertion elsewhere compares a blank against a blank. This gate fires
  **before anything is created**, on a projection built from the offer's own
  price -- so the only thing it needs is a priced offer, and `storeFakes`
  already serves the provider's committed `cpus.json`, which prices the fill's
  machine at the $0.06/hr this file quotes. A refusal creates no node, mounts
  no store and writes no object, so a lab would cross no join the unit test
  does not and would buy a Docker run for nothing.

  **THREE NUMBERS, CHOSEN SO THAT ONLY THE REAL GATE PASSES THEM**, which is
  the "what would this assertion say if the bug were present?" question asked
  of a money figure. $19.99 spent today, a $20 day ceiling, a $0.06
  projection: a ceiling that **ignored the ledger** compares $0.06 against $20
  and allows; one that **reported rather than projected** compares $19.99
  against $20 and allows -- the "ceiling every period breaches by one rental"
  the entry above refuses to be; the live one adds them, gets $20.05, and
  refuses. Both wrong readings were reinstated in `Ceilings.Allow` and both
  are red. So is a projection of `0` and a projection over a zero TTL. The
  **day** rather than the month because the day is checked first and because
  nothing on a rental path had ever driven it. A second, independent witness
  sits beside the message: `recordSync`'s first call is `Adopt`, so a refused
  fill that had already written a ledger row would mean the ceiling fired
  after the journal had been told a rental was coming -- no row means it fired
  before. And the **acceptance is in the same test**, because a refusal alone
  is consistent with a ceiling that refuses every fill there is, which is what
  a free rental must never get.

  **The sweep is the finding, because the class was two gates wider than the
  issue.** Every money gate in the tree was deleted and judged on an exit
  code. `fillStore`'s `refusePaid` (exit 1, #155's), `Plan`'s own
  `refuseOverPeriod` (exit 1), `DirectRunner`'s `refusePaid` (exit 1),
  `webapi`'s `refusePaid` at the enqueue edge (exit 1) and
  `provision`'s `Budget.Allow`, which is `max_hourly_usd` and `max_run_usd`
  (exit 1) all hold. **Two did not.** Deleting
  `refusePaid(p.AllowPaid, plan.Provider, plan.Offer)` from `Plan` -- the
  `--allow-paid` gate on the **card**, the more expensive of the two rentals
  and the last statement before the create -- was **exit 0**; and replacing
  `plan.Offer.Price` with `0` in the ceiling call beside it was **exit 0** as
  well, as was replacing `cfg.TTL` with `0`. A ceiling that projects nothing
  refuses nothing.

  **Both were invisible for one structural reason: the two gates read
  `plan.Offer`, so they are on the far side of a successful `app.NewPlan`, and
  no fake in `internal/queue` could get one.** `ceiling_test.go` reaches the
  ceiling through the **local** provider, which plans fine and is **free** --
  so `refusePaid` returns nil and the projection is zero, and
  `TestAppPlanner_ChecksThePeriodCeilingBeforeRenting` proves `Plan` arrives at
  the ceiling through the **nil-ledger** branch, which `refuseOverPeriod`
  answers before it ever computes a projection. `storeFakes` served no GPU
  catalogue. Every route to those two lines went through a provider that either
  could not choose an offer or could not be charged for one. So the missing
  piece was one fake: a paid provider that CAN choose an offer, serving the
  provider's own committed `gpus.json` at `/v2/catalog/gpus` and pricing the
  RTX 4090 at $0.89/hr. There is still no `/v2/pods` route, so nothing there
  can create a node even if a gate let it through.

  **A NEW HELPER AND NOT A ROUTE ADDED TO `storeFakes`, AND THE FIRST REASON
  GIVEN FOR THAT WAS FALSE -- which is this file's own recurring defect
  committed by the entry recording it.** Both this entry and
  `moneygate_test.go` said the route could not go there because "three tests
  in `preflight_test.go` assert positively on failing at `list offers from
  runpod`" and would stop asserting the order they exist for. Measured: adding
  a `/v2/catalog/gpus` route to `storeFakes` breaks **nothing**,
  `go test ./internal/queue/` exit 0. There is **one** such assertion in that
  package, at `preflight_test.go:255`, and it survives the route; the three
  are in `internal/cli/preflight_test.go`, a different package with its own
  fake, and one of those three is a **negative**. **The real reason is the
  fixture and it is stronger than the one that was written:** `storeConfig`
  asks for EU-RO-1 and the fake's volume listing reports it -- the region that
  reaches `Filter.Region` is the provider's answer, which `AttachStore` reads
  off `storeFakes`'s own `network-volumes` handler, and a store's datacentre
  *overrides* the configured placement -- while the committed `gpus.json`
  carries capacity only in CA-MTL-3, US-MO-1 and US-IL-1. So `storeFakes` can
  never produce a GPU offer at all. The gates behind `plan.Offer` are out of
  its reach by **fixture** and not by the catalogue route it was missing, and
  reaching them from there would mean moving that region on both sides or
  re-capturing the catalogue, perturbing five existing tests to serve one. A
  comment wrong about *why* a seal holds is how the seal gets removed by
  somebody being helpful -- the `AssertNoStray` cleanup comment that claimed
  the opposite of what the code did -- and this one would have sent the next
  reader to delete a helper they believed was redundant.

  **And the narrowed TTL is red in BOTH arms, which the first wording of this
  entry got wrong in the direction that undersells the test.** The config
  allows two hours and the request asks for one, so the honest projection is
  $0.89 and the config's own TTL would make it $1.78; with $18.95 spent
  against a $20 day the narrowed figure is $19.84 and goes ahead while the
  config's is $20.73 and refuses. This said the acceptance arm caught a
  ceiling reading `p.Config.TTL` "and nowhere else", and that the refusal arm
  "is red against both readings and distinguishes neither". Measured:
  substituting `p.Config.TTL` is red at the refusal's `want` list as well,
  because $0.89 and $20.34 both move, **and** at the acceptance. The
  acceptance arm is a second witness rather than the only one, and it is the
  one that would still fire if the refusal's figures were ever loosened.

  **The ceiling is matched as `ceiling of $20,` and not as `$20`, because the
  short spelling could not fail.** `$20` is a substring of the `$20.05` and
  `$20.34` two fields later in the same message, so misprinting the ceiling
  figure leaves `go test -run AgainstThePeriodCeilings ./internal/queue/` at
  **exit 0** -- the assertion was held up by the total beside it and the
  property by a different test entirely,
  `TestCeilings_RefuseARentalThatWouldCrossTheCeiling`.
  That is the "a mention is a whole token" rule the config-key guard settled
  over `--as`, met again inside a money assertion, and the comma is what the
  message already puts after the figure so the bound costs nothing. With it,
  the same misprint is **exit 1** in both tests.

  **What is deliberately left alone, written down so it is not read as a
  miss.** `DirectRunner.Start` has `refusePaid` and **no** period ceiling,
  which is the recorded design *taken before the rental moved into the
  daemon*: the ceilings bound spending with nobody at the keyboard, and a
  submitted `ember up` is the operator asking -- "the config path travels and
  the ceilings replace", which is why that path takes the file the operator
  named. `ember up` and `ember store sync` in a shell are the same case and
  have no period ceiling either. **That is the right call for this change and
  not a settled one**, because #146 moved where the rental lives without
  moving the ceiling, and a submitted run still *consumes* the queue's
  allowance: `AdoptRun` writes `requests.cost` and `genstore.Spend` sums that
  column with no status filter, so `max_month_usd` reads as a bound on the
  month and is a bound on the queue alone. That is **#165**, not this one: a
  pointer to an issue nobody can find is the shape this file keeps
  correcting.
  **THAT PARAGRAPH IS NOW THE RECORD OF A GAP THAT IS CLOSED, and it was not
  settled after all: all three of those paths carry the ceiling as of #165, and
  the sentence "the ceilings replace" turned out to be about the wrong
  ceilings.** It is left in place rather than rewritten because a prediction
  this file made and the code then met is worth more than a tidy present
  tense -- the arrangement two entries down in **Threat model** uses the same
  wording for the same reason. The entry below has what the reading found and
  the one thing this paragraph got wrong: `--allow-paid` is not the only place
  the socket is not simply the operator's own shell.
  `internal/webapi` and `internal/provision` were **measured and restored**
  and nothing in them was edited. Two of the deletions also turned
  `TestNoProductionDeadCode` red as collateral, because stubbing a gate out
  leaves the symbol it called with no production caller -- a true report about
  the patched tree and not a second finding.

  **Opening and closing controls both exit 0, and two no-op patches were
  reported as not caught.** The pass is eleven reinstatements with the marker
  each one introduces asserted before any suite result is believed, a build
  check ahead of every test run, and one backup per path taken once -- the five
  harness traps this file records, including the rollback that half-patched its
  own backup. **And the measurements review took afterwards found both kinds,
  which is why they are named rather than counted.** Prose only: the
  `storeFakes` route and the un-narrowed TTL, two sentences above that were
  false about code that was right. Code: the misprinted ceiling, which moved a
  `want` list in both tests, and `s3.EndpointFor`'s fallback, which added an
  `S3BaseURL`. The first wording of this sentence said "four measurements,
  three of them prose" -- a wrong tally, three paragraphs under the paragraph
  correcting a wrong positional count, which is the whole argument for not
  keeping one. The expectation it bears out is the one this file already
  states: the first round of a reinstatement pass is reliably incomplete, and
  that holds for the prose and not only for the suite.

- **Three of the five paths that write `requests.cost` had no period ceiling,
  the issue's framing was wrong twice, and the cause predates the socket it
  blamed (#165).** #165 reads "a run submitted over the socket spends the
  daemon's period allowance without being bounded by it", which is true and
  understates it on both axes.

  **It is not about the socket.** Five paths write `requests.cost` -- the
  column `genstore.Spend` sums with **no status filter** -- all of them through
  `queue.AdoptRun` or the queue's own `Recorder`: the queued request, the
  queue's store fill, `ember up` submitted over the socket, `ember up` in a
  shell, and `ember store sync` in a shell. Only the first two were projected
  against a ceiling. So `ember up` has never been bounded by `max_day_usd`,
  daemon or no daemon. The chronology settles whose defect it was and it is not
  #146's: the ceilings landed in `bc30700`; `cbd3f14` -- "the command every
  measurement was taken with recorded nothing" -- gave the ceiling's own INPUT
  a writer the gate did not cover, which is the moment the bound became
  partial; and `8bb845a`, the socket, merely inherited it.

  **And neither config was read on those paths, so the "whose config" question
  dissolved before it could be argued.** `config.Config.PeriodCeilings` had
  exactly **two** non-test callers: `refuseOverPeriod` in
  `internal/queue/planner.go` and a startup **print** in `cmd_serve.go`. So
  this was never "change which config the ceiling comes from", it was "read the
  key at all" -- and the corollary is sharper than the headline: `max_day_usd:
  "banana"` in a config an `ember up` named was accepted and the run went
  ahead, because `KnownFields(true)` catches a key that does not exist and has
  nothing to say about a value that is not a price.

  **Measured rather than argued, before anything was written**: identical
  config, identical empty ledger, the queued arm REFUSES with the full
  `$19.45 ... ceiling of $20, ... $20.34` message, and the submitted arm
  returns `err=<nil>` with **$500** already spent against a `$20` day. There
  was no gate, not a gate computing the wrong number -- which is why the
  structural check (two callers, one of them a print) is the measurement that
  matters and a differential against a wrong figure would have been the easier
  and weaker one.

  **ALL THREE ARE GATED, and the alternative that was refused is the one that
  looks cheaper.** Socket-only would have closed the issue as written and
  shipped a new money divergence between the two `up` paths, in the one place
  #146 exists to remove divergence -- the identical command bounded through a
  daemon and unbounded in a shell, which is `--allow-paid`'s trade run
  backwards. The cost of all three is that `ember up` and `ember store sync`
  can now be refused with no flag to override, on the command every measurement
  in `README.md` was taken with; that is written in `README.md` where an
  operator reads about the keys rather than left to be discovered from an exit
  code.

  **ONE FUNCTION AND NOT FIVE COPIES.** `queue.RefuseOverPeriod` is the rule,
  reached from three call sites covering all five paths, where there was one
  method on `AppPlanner`; `queue.CeilingsOf` is the parse, and it is the **one
  non-test caller of `config.PeriodCeilings` there is** -- the announcement in
  `cmd_serve` reads the key through it too, so the gate and the print cannot
  come to disagree about what `max_day_usd` means. The price and the TTL are
  **parameters** rather than fields read inside, because #157 measured both
  halves as gates that cannot fail when either is a literal zero, and a copy
  per call site is a place for one of them to pass a zero.

  **WHOSE CONFIG ON THE SOCKET: BOTH, TIGHTEST PER PERIOD.** A ceiling is a
  question about a LEDGER, and the ledger a submitted run is recorded in is
  `DirectRunner.Store` -- the daemon's. So the file that owns that ledger has
  to be consulted or #165 stays open; and the run's own has to be consulted or
  an operator whose `ember.yaml` sets `max_day_usd` and who started the daemon
  with a different `--config` gets a bounded `ember up` in a shell and an
  unbounded one through a daemon. `Ceilings.Tightest` takes the smaller of the
  two per period through `Narrow`'s own `smallerUSD`, so zero means unbounded
  on both sides and **neither ceiling is widened by the other**.
  **It is not `Narrow`'s rule reached again and it does not contradict "the
  ceilings replace"**: that sentence is about `app.Overrides.Apply` replacing
  `max_hourly_usd` from a flag, and the period ceilings have no flag and no
  field on any wire type -- there is nothing here that could replace anything.
  What is composed is two files an operator wrote, and the smaller of two
  numbers an operator wrote cannot be a limit they did not ask for.
  **NO OVERRIDE FLAG, which is the default answer and survived being asked.**
  A day or month flag beside `--max-hourly-usd` would be consistent with `up`'s
  flags-replace rule and is wrong for a reason the hourly one is not: the
  hourly ceiling bounds the rental in front of you and these bound a period
  every other rental shares, so a flag that raised one would spend another
  command's allowance. Editing the file, or `--config`, is the escape hatch and
  it is already there.

  **RECORDING IS BEST-EFFORT AND BOUNDING IS NOT, which is the one asymmetry
  and the shell paths are where it is reachable.** `recordRental`'s rule --
  "it cannot fail the run", because refusing to rent over a database that will
  not open would be ember declining to do the thing it is for -- stands
  unchanged. A **configured** ceiling with no readable ledger is a refusal, on
  the rule that makes `ember ls` exit non-zero over a provider it could not
  ask. Two uses of one store and only one of them may be skipped, so they are
  one function (`cli.boundAndRecord`) with one open and one order; the
  alternative was two opens of one SQLite file to ask two questions about the
  same rows, which somebody later makes one anyway, in whichever order they
  happen to write. On `ember serve` the branch is unreachable -- the store is
  always wired -- so without the shell paths it would have been a branch
  nothing could take.

  **NOT ON A DRY RUN**, which both shell callers return before. `--dry-run`
  rents nothing, so a ceiling that refused it would be refusing a command that
  cannot spend -- the free-rental rule with a different zero in it.

  **A FREE RENTAL IS STILL NEVER REFUSED**, answered where it always was, on
  `Ceilings.Allow`'s `projected <= 0`. This was the direction the change could
  most easily have got wrong, because `ember up` against the local provider is
  how most of `README.md`'s measurements were taken, and it has a test on the
  CLI path as well as the arithmetic one.

  **THE PRODUCTION WIRING HAS A GUARD, AND THE REASON IS #105's.** Every test
  of these gates sets `DirectRunner.Config` itself, which is exactly the
  arrangement that let `GET /api/nodes` answer 501 in every shipped
  `ember serve`: the three assignments to `webapi`'s `scan` were all in test
  files reaching past the constructor, so the suite could not tell "the
  production value is missing" from "this test did not set it". Making it
  unrepresentable was the first choice and is **not available**: a zero
  `config.Config` is a legitimate value -- an operator who set no ceiling has
  one -- so no type and no runtime check can tell "nobody wired it" from
  "nothing is configured". So `TestServe_WiresTheDaemonsOwnConfigIntoTheSubmittedRunGate`
  parses `internal/cli`'s production files and fails on a `queue.DirectRunner`
  literal that does not name `Config`, with a floor (a walk that found none
  fails) and a **fatal** on a positional literal it cannot read --
  `TestFlagTablesClassifyEveryFlagSetMethod`'s rule, because the alternative to
  failing on a literal this guard cannot classify is reading past the one that
  matters.

  **THIS PARAGRAPH CARRIES NO TOTAL, AND THAT IS THE THIRD CORRECTION RATHER
  THAN A FOURTH COUNT.** It said SIXTEEN over a list of seventeen; corrected to
  seventeen; then THIRTY over two passes, where the gate pass was right at
  twenty-three and the walkaround pass's enumeration was missing one of its own
  reds -- so the total was right and the list was short, which is the same
  defect in the other direction and no more defensible. Three consecutive
  wrong statements about its own arithmetic, in the file whose rule is that a
  running total in prose is the thing it has been wrong about before. So the
  lists below are the record and nothing sums them: every case is named, every
  case has a verdict, and a reader who wants a number can count the names.
  **AND DROPPING THE TOTAL WAS NECESSARY AND NOT SUFFICIENT, which the fourth
  instance proved by not being a total at all.** With no number left to be
  wrong, the list itself was: one red filed as evidence for a refusal its
  message does not name, and the green half of the same case recorded only in
  the prose below it, so neither side of the list reconciled with the pass that
  produced it -- while the paragraph above said the lists were the record.

  **THE REASON NO AMOUNT OF ARITHMETIC COULD HAVE CAUGHT IT: A RED IS TWO
  CLAIMS, AND ONLY ONE OF THEM WAS EVER BEING CHECKED.** A red says the case
  failed *and* why it failed, and every check this paragraph has carried --
  the total, then the per-family counts, then the reconciliation between list
  and count -- sees the verdict alone. Both misfiled entries had a CORRECT
  verdict and a wrong reason: the index case really is red, the parenthesised
  dereference really is red, and both were filed as evidence for a mechanism
  their own messages do not name. An exit code cannot tell those apart from a
  correctly filed red, so no sum over exit codes can either. What makes a list
  self-checking is that each entry's evidence is **the tool's own output read
  back** -- then the classification stops being something the author is trusted
  to have done and becomes something a reader re-derives, which is what the
  three families below are grouped by and how the two misfilings at the end
  were found.

  **AND THE FORCE OF THAT IS THAT THE REVIEWER MADE THE IDENTICAL ERROR.** The
  independent pass that checked this work captured
  *"Config is set from `cfg` and its address is taken in the same function"*
  for the parenthesised-dereference case -- the message was in its own output
  -- and filed it as evidence for the `ParenExpr` and `StarExpr` arms anyway,
  which is precisely the misreading it had just caught one case over in the
  index entry. Two independent readers, the same mistake, one case apart, with
  the disproving message sitting in both sets of output. That is the strongest
  evidence in this whole sequence that the verdict-only check was the problem
  rather than either reader's care, and it is why the remedy is structural --
  group by the message -- rather than a resolution to read more carefully. The
  `checkDirectRunnerConfig` refusal-site count below is the same lesson a third
  time: both readers counted families, which is a reading, instead of sites,
  which are lines in a function -- and both got a different wrong answer.
  **Every case RED where expected, every control GREEN, controls at both ends
  of each pass, and every case ended in a `git diff` against HEAD rather than
  in an exit code** -- HEAD being the pristine copy, taken once, which is the
  fifth harness trap removed rather than guarded.
  Red: the
  submitted gate deleted; its projection over the DAEMON's ttl; the same
  taking the LONGER of the two ttls; the daemon's ceiling alone; the run's
  alone; a zero projection; both parses naming the same file; the shell gate
  deleted; a zero shell ttl; a zero shell price; a configured ceiling with an
  unopenable ledger reading as satisfied; an unopenable ledger refusing with no
  ceiling configured; the shell gate moved ABOVE the dry-run return; a free
  rental losing its exemption; the row adopted before the ceiling is checked;
  `cmd_serve` not wiring its own config; and a gate that refuses every rental
  there is. Green: two reworded comments.
  **Six more after review, each red**: the shell no-ledger arm asking `IsZero`
  again (the free-rental finding itself); `RefuseOverPeriod` asking `IsZero`
  again, so its nil-ledger arm refuses a free rental; `Binds` forgetting the
  price half; `Binds` forgetting the ceiling half, which refuses where nothing
  is configured; the submitted run not re-checked on the loop; and
  `UnreadableLedger` printing its cause with `%v` instead of wrapping it.
  **One more green, which is a measurement rather than a gap**: `Allow`'s
  deleted `c.IsZero()` disjunct put back is exit 0, because it was redundant
  with the loop below it and not merely unreachable -- see `Allow`, where the
  measurement is written beside the code.
  **The walkaround pass is what proves the wiring guard**, and two of its
  clauses have no defect available on the pristine tree at all: the `moved`
  refusal and the positional-field fatal fire only on code somebody would have
  to write, which is what the walkaround pass writes. **The list is the record
  and nothing sums it**, and each entry sits under the refusal whose message it
  drew, because an entry filed under the wrong one is the mistake corrected at
  the end of this paragraph. **`checkDirectRunnerConfig` refuses at six sites,
  of which these entries draw four, and the figure is counted rather than
  carried**: `Config is set from a %T` five times, `which is not an object this
  function bound` six, `Config is set from %q and %s in the same function`
  twelve, and `this queue.DirectRunner does not set Config` once, which is the
  twenty-four reds. Two are never drawn and both are defensive -- the
  positional-field fatal, and the fatal for an identifier the type checker has
  no object for, which cannot happen in a package that type-checked.
  **That is a different pair from the two named at the top of this paragraph,
  and the overlap is one member**: "no defect available on the pristine tree"
  asks which sites need code somebody would have to write, and the `moved`
  refusal is one of those AND is drawn twelve times by code this pass writes;
  "never drawn" asks which sites no entry reached at all. The positional fatal
  is in both pairs, which is why the two sentences read as one claim and are
  not.
  **Both of the two readers who have counted this got it wrong, in different
  directions, and neither was counting refusals**: this paragraph first said
  three drawn and one defensive, because it counted its own FAMILIES, and the
  not-an-object family draws two sites rather than one -- by AST kind where
  there is no object to name, by name where there is. Review then said six
  sites and accounted for five, keeping the family count and correcting the
  defensive one. A family is a reading of the list; a site is a line in the
  function, and only the second is countable. The written-to family's twelve
  entries draw ONE site, whose `%s` carries the clause -- assigned to, address
  taken, something inside it assigned, the same by a range clause, incremented
  -- which is why counting messages and counting sites are different sums as
  well.

  Red because the field is not set: the `Config` key dropped altogether.

  Red because the value is not an object the function bound -- by AST kind
  where there is no object to name: a zero composite literal; a non-zero one; a
  conversion; a struct selector; and **an indexed value, `Config: cfgs[0]`**.
  By name where there is: a `var` of the right type; a copy through a second
  `:=`; a closure with no config parameter; **a shadow in an inner block**; a
  shadow through a range clause; and a shadow through an if-statement
  initialiser.

  Red because the object was written to: **a reassignment of the parameter one
  line above the wiring**; the parameter's address taken; a write through a
  parenthesised dereference; **a field write that zeroes exactly the two
  ceilings**; a nested field write; a compound-assign to a field; a field write
  inside a closure with the construction outside it; an inc/dec of a field;
  **a range clause assigning into a ceiling field**; a range clause assigning
  the whole variable; a range clause assigning into the KEY position; and an
  assignment inside a `select` clause.

  Green, each one a shape that must stay accepted: a closure that takes the
  config; an identifier from `config.Load`; the fields reordered; **two
  legitimate `:=` range loops beside the wiring**; **the same index write with
  `Config: cfg`**; and the one documented bound -- a config handed across a
  call.
  **ONE ENTRY WAS FILED UNDER THE WRONG REFUSAL, WHICH IS "ASK WHY A CASE WENT
  THE WAY IT DID" APPLIED TO A RED -- the half nobody checks.** "A write
  through an index" was offered as evidence for write-recording and is nothing
  of the kind: its message is "set from a `*ast.IndexExpr`", because the
  literal reads `Config: cfgs[0]`, so it belongs beside the struct selector and
  the conversion and duplicates what they already cover. Measured by splitting
  it, and **both halves are in the lists above** rather than the correction
  living only here -- which is the defect this paragraph had in its own first
  wording, where the disclaimer sat under an unchanged list and no count
  reconciled with either. The split half, `Config: cfg` with the same index
  write, is **green and correctly so**, since the write was to `cfgs` and `cfg`
  was never written to.
  **Two more entries evidence a clause one step earlier than their names
  suggest, which is the same reading error caught before it is made.** "The
  parameter's address taken" and "a write through a parenthesised dereference"
  both draw *"its address is taken in the same function"* -- so neither reaches
  `rootIdent` at all, and the parenthesised one is not evidence for the
  `ParenExpr` and `StarExpr` arms it is named after. What does evidence the
  root walk is the five that draw *"something inside it is assigned to"* -- the
  field write, the nested one, the compound-assign, the one inside a closure
  and the one in a `select` -- plus the two range clauses that say the same of a
  range and the inc/dec.
  And -- worse for the record -- `rootIdent`'s `IndexExpr` and `IndexListExpr`
  arms are unreachable for this guard's subject **in principle**: a tracked
  object is a `config.Config` value, a struct is not indexable, so
  `cfg[…] = x` is not legal Go. Those two arms are defensive, which their own
  comment now says, and no measurement here has ever taken them.
  **The `ParenExpr` and `StarExpr` arms are defensive for a DIFFERENT reason,
  and "nothing evidences them" was the weaker thing to say.** They can fire:
  on a `*config.Config` held as a parameter or a return value, `(*p).MaxDayUSD
  = ""` peels to `p`. What they cannot do is fire USEFULLY here, because a
  pointer that aliases a tracked object can only be obtained by `&cfg`, and the
  address clause records that first -- which is exactly why the parenthesised
  case above draws the address message and not these arms. Unreachable in
  principle and reachable but always pre-empted are two different kinds of
  defensive, and a reader who finds only the index arms' reason will assume
  these share it.
  **THE POINTER-RECEIVER FINDING IS NOT ONE OF THOSE GREENS AND USED TO BE
  LISTED AS ONE.** It was never a walkaround case: the pass prints its header
  and then says in place of a verdict that it is vacuous on this tree, because
  `internal/config` declares no such method -- it was measured by adding one,
  not by patching the construction site. So it belongs to the entry above and
  not to this pass, and counting it here is how the green side read as one more
  than the list held.
  **And the per-arm verdict is the proof the free-rental pair discriminates**:
  with `Binds` reverted to `IsZero`, the PAID arm and the no-ceiling arm stay
  **PASS** and only the FREE arm goes **FAIL**, so the free arm is doing the
  work rather than the three being satisfied by a refusal of everything --
  which is what case K then shows from the other side, going red on the free
  arm AND the no-ceiling arm when the condition is `true`.
  **Two cases reported ANCHOR-MISS rather than a verdict on the first run of
  the extended pass**, because the free-rental fix replaced the very line those
  two were anchored on. Trap one working: a stale anchor reads exactly like a
  clean run, and the harness refused to call it one.

  **Three cases reported BUILD-ERROR rather than a verdict on the first
  attempt, which is the harness refusing to read a non-compiling tree** -- two
  `declared and not used` from dropping one side of the `Tightest` call, and a
  `ctx` that is not in scope where the dry-run patch inserted its call. The
  check is `go vet` on the two packages and not `go build ./...`, because
  `go build` does not compile `_test.go` files and a signature change would
  therefore read as a caught defect: a FALSE RED, which is the one direction a
  harness cannot be caught out safely.

  **And one case was reported wrongly by the harness rather than by the tree,
  which is trap six working.** The "refuses every rental" case printed eight
  failing tests and the socket path's acceptance arm was not among them -- so
  the question was whether that arm could fail at all. It can: the list was
  `head -8` and had exactly eight entries, and re-running the case unfiltered
  names `TestDirectRunner_ProjectsASubmittedRunAgainstTheDaemonsPeriodCeilings`
  and three more. A truncated report and a dead assertion look identical, and
  the only thing that told them apart was asking why a case went the way it
  did.

  **The belt-and-braces measurement, because belt and braces reads exactly
  like a dead gate.** With all three new gates removed -- the submitted run's,
  its re-check on the loop, and the shell's -- this change's own tests removed
  (`internal/cli/moneygate_test.go` deleted and
  `internal/queue/moneygate_test.go` reverted to master), and the two symbols
  that then had no caller removed with them, `go test ./...` is **exit 0**: so
  nothing that existed before this change reported the gap. The two
  intermediate steps are worth recording rather than skipping, because each is
  a *true* report about the patched tree and not a second witness for the gate:
  `TestNoProductionDeadCode` names `DirectRunner.refuseOverPeriod` and then
  `Ceilings.Tightest`, one symbol at a time, which is the collateral #157
  already recorded for the same reason.
  **The free-rental defect is the one exception to that sentence and it points
  the wrong way**, which is why it is stated separately: a pre-existing test
  did have an opinion about it, and the opinion was that the defect was
  correct. Master's `TestAppPlanner_ChecksThePeriodCeilingBeforeRenting` is
  GREEN over the defect and goes RED over the fix -- which is how the fix found
  it, and is the sharpest form of "a test that asserts the bug" this file
  records.

  **AND REVIEW FOUND TWO OF THESE GATES WRONG AFTER ALL, WHICH IS WHAT THE
  PARAGRAPH ABOVE SHOULD HAVE EXPECTED.** Seventeen reinstatements going red
  on a first attempt is the finding about the harness this file predicts, not a
  verdict on the change; an independent pass of twenty found one green against
  expectation and one guard it could walk around. Both are below, with what
  each measured.

  **A FREE RENTAL WAS REFUSED BY A MONEY CEILING, AND THIS FILE'S OWN RULE WAS
  QUOTED FOUR LINES ABOVE THE BRANCH THAT BROKE IT.** The no-ledger refusal
  asked `!cs.IsZero()` and nothing about the price, so `ember up --provider
  local` -- projection zero, adding nothing to any period -- with
  `max_month_usd` set and a `generations.db` that would not open was refused
  outright, where master printed `not recording this run ...` and ran.
  `Ceilings.Allow`'s own `projected <= 0` sits PAST that refusal and never gets
  asked. **The defect is older than #165 and was unreachable**: the same arm in
  `AppPlanner.refuseOverPeriod` has had it since `bc30700`, and `cmd_serve`
  always wires the Ledger, so nothing an operator could type reached it --
  until #165 put the same arm on the shell path, where `genstore.Open` really
  can fail.
  **It was asserted. Twice.** `TestUp_ALedgerThatWillNotOpen...`'s refusal arm
  was configured `provider: local`, so the test asserted the defect and went
  red when the missing condition was added -- an inverted assertion and not a
  missing one. And `TestAppPlanner_ChecksThePeriodCeilingBeforeRenting`
  contradicted itself inside one body: it asserted the same FREE rental refused
  with no ledger and planned with one, where a ledger cannot change a free
  rental's answer, under a comment that states the rule it was breaking. Both
  are inverted, and both now carry a PAID arm beside the free one, because a
  refusal arm on a free provider cannot tell "a ceiling with no ledger refuses"
  from "a ceiling with no ledger refuses everything".
  **The fix is one predicate and not a condition repeated**, because the
  condition was already repeated and both copies were missing the same half.
  `Ceilings.Binds(ttl, price)` is the one place that answers "could this rental
  be refused at all", hoisted ABOVE the ledger question at both sites, so the
  consistency half the review asked for costs nothing and adds no sub-branch to
  an unreachable arm. `Ceilings.UnreadableLedger(why)` is the sentence, which
  existed twice with different wording -- two places for one money refusal to
  drift, which is the other half of that nit.

  **AND THE WIRING GUARD CHECKED THE FIELD'S NAME AND NEVER ITS VALUE, so it
  could be walked around while reading as protection.** `Config:
  config.Config{}` substituted for `Config: cfg` in `cmd_serve` left
  `go build`, `go vet`, `golangci-lint` and `go test ./...` all at exit 0 --
  the guard among them -- with the daemon's ceilings reaching the
  submitted-run gate nowhere and `Tightest` composing a zero against the run's
  config, which is #165's own defect under a test whose prose claims to refuse
  it in advance. That is `TestTrustHasOneImplementationAndItIsPin`'s
  generic-receiver hole one guard over: a guard that can be walked around is
  worse than none, and this one claimed its own completeness in this file
  before anybody had tried.
  **AND THE SECOND VERSION OVER-CLAIMED IN ITS PROSE TOO, which is this same
  failure committed by the paragraph correcting it.** v2 required the value to
  be an identifier the enclosing function "bound to a config.Config" and
  implemented that as a set of NAMES -- a name-plus-type check wearing a value
  check's prose. Review measured three more greens: `cfg = config.Config{}` one
  line above the wiring, because the parameter's own name still covered it; an
  inner block shadowing it with `cfg := config.Config{}`; and an
  immediately-invoked `func(cfg config.Config){...}(config.Config{})`. Only the
  last is the bound v2's prose claimed. The other two write the zero INSIDE the
  function the guard analyses, one line from the construction site -- and the
  shadow is the one with teeth, because `standing` reads the OUTER `cfg`, so
  the startup banner would print the real ceilings while the gate got a zero.
  **So v3 asks the type checker and the shadow stops being a heuristic.** The
  value's `types.Object` must BE the object a `config.Config` parameter bound,
  or the one a `:= config.Load` defined, so a shadow is a different object by
  construction -- and the guard never has to know what a block, a range clause,
  an if-statement initialiser or a nested closure is, which is the whack-a-mole
  a name-keyed fix would have chased one syntax form at a time.
  `golang.org/x/tools/go/packages` is already a direct requirement and
  `TestNoProductionDeadCode` already loads the tree through it.
  **A WRITE is the one thing identity cannot answer and it is REFUSED rather
  than analysed**: `cfg = config.Config{}` and `cfg.MaxDayUSD = ""` both leave
  the object alone and change what is in it, which is dataflow -- so **an
  object the function wrote to BY ANY STATEMENT is no longer accepted**, and
  the message says which way. That is the allow-list direction, a refusal
  rather than an analysis, and it costs no false positive today: no production
  function here that builds a `DirectRunner` writes to its config by any of
  them.
  **THAT SENTENCE NOW HAS NO QUALIFYING CLAUSE, AND IT TOOK FIVE REVISIONS AND
  FOUR WRONG ONES -- which is the entry's own lesson rather than a note about a
  test.** v2's prose said "bound to a config.Config" over a set of NAMES. Its
  fix said "assigned to ... anywhere in the function" while the code looked
  only for a bare identifier on the left, so `cfg.MaxDayUSD = ""` stayed
  accepted: two lines that read as normalisation and zero exactly the two
  ceiling fields, green across `go build`, `go vet`, `golangci-lint` and
  `go test ./...` while `Tightest` composed a zero -- the most plausible
  walkaround of the lot, with no shadow, no closure and no reassignment of the
  variable. And *that* fix still missed the two write forms which are not an
  `*ast.AssignStmt` at all: a range clause with `token.ASSIGN`, where
  `for _, cfg.MaxDayUSD = range []string{""} {}` zeroed a ceiling with the
  guard green, and `*ast.IncDecStmt`, which cannot zero either ceiling because
  both are strings.
  **The last step was a choice between two `case` arms and a sixth attempt at
  wording a bound, and the code is the cheaper and more durable answer.** Go
  has exactly three statement nodes that write to an existing variable --
  `AssignStmt`, `RangeStmt` with `token.ASSIGN`, `IncDecStmt` -- plus `&x`, and
  all four go through one `rootIdent`, whose six forms are *exhaustive* over
  Go's assignable expressions rather than merely safe. A `token.DEFINE` range
  binds a new object and stays acceptable, which the bound check already
  handles. So the inc/dec arm exists for the RULE and not for an exploit: a
  sentence nobody has to be trusted to have stated correctly is worth two
  `case` arms, and this one was mis-stated five times out of six.
  **AND "NO QUALIFYING CLAUSE" WAS THE FIFTH WRONG WORDING, WHICH IS THE
  BOAST OVER-REACHING RATHER THAN THE CODE.** A write can reach the object
  through an EXPRESSION instead of a statement: a pointer-receiver method takes
  the address of its receiver implicitly, so `cfg.ZeroCeilings()` mutates the
  config with no `&` and no assignment anywhere in the AST. Measured -- a
  `func (c *Config) ZeroCeilings()` added to `internal/config` and called above
  the wiring leaves the guard GREEN and the whole tree with it, both ceilings
  zeroed. The claim was *vacuous* rather than wrong, because that package
  declares no such method; what was wrong was asserting more than the code
  does, for the fifth time in one sentence.
  **So the clause is "by any statement" AND the property it rests on is an
  exit code now.** `TestConfig_DeclaresNoPointerReceiverMethodOnConfig` walks
  `internal/config` through the type checker and fails on a pointer receiver on
  `Config`, with a floor -- `Config` has value-receiver methods, so finding
  none means the load resolved nothing -- and a message naming which guard goes
  quiet and the two ways out. It is in `internal/cli` and not in the config
  package, because the rule is this GUARD's bound and not a fact about that
  package's design: a failure there would read as "you may not add a
  pointer-receiver method", which is not the rule. Made to fail before it was
  believed, which is the half that matters: with the probe method present it is
  **exit 1** naming `ZeroCeilings`, and with the method AND the call present
  the wiring guard is **exit 0** while this one is **exit 1** -- which is the
  whole arrangement, since the thing being guarded is a guard going silent. An
  unenforced dependency that is cheap to break and reports nothing is what
  `AssertNoStray` was; this is ten lines and a floor.
  **IT ASKS ONE QUESTION OF THE TYPE AND THAT IS THE REASON FOR ITS SHAPE**:
  `sig.Recv().Type().(*types.Pointer)`, so genericity cannot hide a receiver.
  Measured by type-checking a synthetic `type Config[T any]` with both forms --
  `*p.Config[T]` asserts to `*types.Pointer` **true**, `p.Config[T]`
  **false** -- so a generic pointer receiver is caught and a generic value
  receiver is still correctly ignored.
  `TestTrustHasOneImplementationAndItIsPin` is the cautionary one -- it
  switched on receiver SYNTAX, skipped the shape it could not name, and a
  generic receiver walked past it with the whole tree green while ember dialled
  every node it rented with no host key verification. Whoever writes the next
  guard here should know which of the two approaches survived: one question of
  the type, never a switch over the spellings somebody thought of.
  **AND ITS OWN BOUND IS NARROWER THAN THE SENTENCE IT DEFENDS, which is the
  thing this PR exists to stop happening a seventh time.** `NumMethods()` is
  methods DECLARED on `Config` -- not promoted ones. Two measured greens:
  `Config` embedding a type that has a pointer-receiver method, and a
  pointer-receiver method on a FIELD's type called as `cfg.SD.ZeroField()`.
  Neither is a counterexample to "wrote to by any statement", because a method
  call is not a statement; both are this guard's bound. **And neither can reach
  this issue's subject**: `MaxDayUSD` and `MaxMonthUSD` are DIRECT fields of
  `Config`, so a promoted method mutates the embedded struct and a field-type
  method mutates `cfg.SD`, and `Config` embeds nothing today -- all three
  measured. So the residue now rests on **the ceilings being direct fields of
  `Config`**, which is a far more local and stable property than the one it
  replaced, and that narrowing is the real thing this guard bought.
  **The third option was taken and rejected rather than overlooked**: recording
  a write when a pointer-receiver method is called on a tracked root is about
  the same size and uses type info the guard already has -- and it makes the
  NEW code the thing that needs walking around, where this makes an existing
  claim checkable. The bound stays stated either way, so the cheaper half to
  trust is the one that is a property of another package rather than another
  branch in the walk.
  **The bound is now one thing instead of three, and the inline form was
  closable and deliberately left**: any function or closure that TAKES a
  `config.Config` is accepted whatever its callers pass, so a config handed
  across a call is green -- through a named helper, which needs dataflow, and
  through an immediately-invoked closure, whose arguments are right there to
  read in about fifteen lines. Reading them would make the guard refuse
  `func(c config.Config){…}(zero)` and accept `helper(zero)` where `helper` is
  identical, which is one rule answering two ways and the same reason a
  closure's parameters are accepted at all. "The function" is a bound somebody
  can hold in their head; "the function, except when the call happens to be
  visible" is not.
  **A pointer field refused as nil in `Start` was weighed and rejected**: it is
  this package's own shape for a dependency that must be wired, and it catches
  strictly less -- a nil says "nobody wired it" and says nothing about any of
  these, every one of which hands over a zero rather than nothing.

  **AND THE SUBMITTED RUN'S GATE READ A TOTAL THAT COULD GO STALE IN THE WINDOW
  IT WAS WAITING THROUGH, which is now checked twice.** `Start`'s gate runs on
  the CLIENT's goroutine and `Submit` then parks on the rendezvous until the
  drain loop is between iterations; `requests.cost` is written from
  `provision.WithNode`'s **deferred** teardown, so a queued rental in flight
  when the gate read contributed ZERO, finished and recorded while the
  submission was parked, and the submitted run rented with nothing re-read.
  Worked: $20 day, $19 recorded, a $2 rental in flight, a $0.89 projection --
  $19.89 admits and the day ends at $21.89. Bounded by one rental, and **one
  rental is the magnitude this feature exists to refuse** -- "a ceiling every
  period breaches by one rental, and one rental is the $54 this exists for" --
  so the bound is not a defence. Before #165 both gates lived on the drain
  loop, where "the completed cost of finished rentals" and "everything
  recorded" were the same set; moving one onto a client's goroutine is what
  made the design sentence insufficient, and the sentence was carried over
  unexamined.
  So `Direct.Run` re-checks, on the loop, where no other rental can be in
  flight. The row already exists by then, so a refusal ENDS it rather than
  leaving a phantom reading `running`: `finish` routes through `classify`, the
  reason lands on `requests.err`, and `runOutcome` is what prints it to a
  client still following the stream.
  **THE CLIENT HALF IS TWO TESTS MEETING AT A COLUMN, which is said rather
  than implied.** The queue's test asserts the row the refusal writes, and
  `TestFollowRun_ARunThatFailedIsThisCommandsExitStatus` asserts what the
  client renders from such a row against a `fakeServe`. Nothing takes a real
  daemon's re-check refusal to a real client, so the join is `requests.err`
  rather than a driven path -- ordinary seam discipline here, and the reason to
  write it down is that "the client prints the reason" reads like one test
  having proved it end to end.
  **ITS TEST WAS A FLAKE ON THE FIRST ATTEMPT, AND `make cover-check` IS WHAT
  CAUGHT IT.** That version started the drain loop AFTER the row was adopted --
  which is the only way to make `Submit` park without a rental in flight -- so
  `Run`'s startup `Interrupt` sweep raced the adopt and sometimes won, marking
  the submitted row `interrupted`; the wait then returned that state, because
  `interrupted` IS terminal, and the assertion read a row with no reason on it.
  `go test ./...` was green; `make cover` runs the suite slower and turned it
  red. That is the frequency this file already records as the worst a flake can
  have, found because the full command set is run rather than the one command
  the change was about.
  **"PRODUCTION NEVER HAS THAT ORDER" WAS THE NEXT OVER-CLAIM, AND IT IS
  CORRECTED RATHER THAN ENFORCED.** `serveUntil` starts `srv.Serve`,
  `srv.ServeSocket` and `q.Run` as three goroutines over listeners that are
  ALREADY BOUND, and `Interrupt` is `Run`'s first statement -- so a submission
  accepted inside that scheduling gap is adopted as `running` and then swept to
  `interrupted`, which is a terminal row over a live rental: the phantom class
  inverted. Unreachable in practice and **not by ordering**, because the client
  has to get through a config load, an `app.NewPlan` with a catalogue call and
  possibly an S3 HEAD per asset, and two SQLite reads, all inside one gap.
  **Reordering the goroutines does not close it**, which is why it was not
  taken: starting `q.Run` first schedules a goroutine, it does not run
  `Interrupt`, so the window narrows and the sentence would still be false --
  a fresh over-claim of exactly this class. Sweeping synchronously before the
  listeners accept only moves the window to between the two sweeps, unless
  `Run` stops sweeping, which every other caller needs. The honest close is
  refusing to sweep an ADOPTED row, and that is the per-row holder and liveness
  check `genstore.Adopt`'s own comment already declines, with the reason that
  this direction is transient and self-correcting. So it is left as it is and
  said as what it is -- and the test no longer rests on it either way.
  **So the test is the real scenario now rather than a simulated one**: the
  loop starts FIRST, over an empty history, and is made busy with a queued
  request whose `fakeRental` blocks -- so `Submit` parks because the one
  goroutine that can rent is inside a rental, which is the actual shape of the
  defect. Ten runs and five under `-race` are green, and the one thing the test
  hands itself is said out loud: `fakeRental` emits no `run_cost`, so the
  in-flight rental's cost is written at the moment releasing it would have
  written it, which is the figure landing between the two reads that the whole
  defect is about. The order is settled by which call the test makes next and
  never by a sleep.
  **AND ITS ONE UNBOUNDED WAIT IS BOUNDED, prompted by a red nobody could
  name.** A soak of this package reported a failure at **632.7s** under
  `-race -timeout 1800s -count=20 -cpu=1,2,8` whose name was lost to a
  `tail -3` -- the truncated-report trap again -- and it is unreproduced in
  roughly 265 later iterations across both tips, so it is not attributable and
  is recorded against #158 rather than claimed here. What it did expose by
  being unreadable is that this test had one wait with no deadline: `Start`
  parks in `Submit` on `context.Background()` until the drain loop receives, so
  a loop that had gone away would hang it until the binary's own `-timeout`
  printed a goroutine dump naming nothing -- which is the shape of the report
  that could not be read. Every other wait here already had one. It is a
  `select` with a deadline and a named `t.Fatal` now. That does not make the
  632s red this test's, and it does take this test out of the candidate set for
  the next one.

  **What is NOT done, said rather than left to be found.** The gate counts
  rentals and not store standing charges, which is the gap the original entry
  records and this change does not touch. `ember up` in a shell has no
  discriminator between `plan.Spec.TTL` and `cfg.TTL` -- the overrides have
  already replaced by then, so the two are equal and no test there can tell
  them apart; the socket path HAS one, because the daemon's config is a third
  TTL in scope, and that is where the assertion lives. And the one clock
  dependence is named where it is paid: `runUp` reads `time.Now`, so the shell
  test that seeds a month's spend says in its own failure message that a UTC
  month turning over mid-test puts the seeded rental in the previous period.
  Injecting a clock would mean a production signature shaped by a test's
  convenience, and the other two arms of that test read no clock at all. The
  exposure is the wall-clock length of one subtest against a UTC month, which
  is why the MONTH ceiling is the one that arm uses: a day boundary is thirty
  times likelier for the same test. Weighed again after review raised it and
  left as it is, because the two clock-free arms carry the join and this one
  carries only the ledger read.

  **AND THE STALE-TOTAL RACE IS CLOSED ONLY WHERE THERE IS A LOOP TO CLOSE IT
  ON.** `ember up` and `ember store sync` in a shell read the ledger and then
  rent, and a daemon's rental in flight contributes 0 to that read exactly as
  it did on the socket path -- so the same one-rental overshoot reaches them,
  and there is no goroutine of this process's on which the total would be
  complete. Closing it would mean a lock on the ledger held across a rental, or
  a ledger read the renting process does not own, neither of which exists here.
  Named rather than papered over, because the socket path's re-check makes the
  shell's absence look like an oversight.

  **AND THE RULE THIS ENTRY ARGUES FOR IS NOT APPLIED IN A SHELL, which is
  review's finding and is a deliberate stop rather than a miss.** The whole
  case for `DirectRunner.Config` is that a ceiling is a question about a ledger
  -- so `ember store sync --config scratch.yaml`, with no ceiling in
  `scratch.yaml`, writes into the ledger at the DEFAULT `state_dir` (which
  `app.ResolvePaths` resolves when a config names none, `d.HomeDir` being a
  test-only override) while bounded only by the file it named. Symmetric with
  the socket case and not fixed, for a reason the socket case does not have:
  there, both files are in scope BY CONSTRUCTION -- one arrived in the request
  and the daemon already held the other. In a shell only one is, and the only
  way to find a second would be to read the default `ember.yaml` as well, which
  is ember choosing a file the operator did not name, breaks `--config` as "run
  this configuration instead", and is `ember reap`'s refusal of a second pair
  reached from the other side. The honest statement is that `--config` names
  the ceiling as well as the run.
