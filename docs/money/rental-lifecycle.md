# The life of a rental

TTL and the deadline, destroy on a context that survives Ctrl-C, detaching,
the intent record, and recording how a rental ended.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

- **TTL is mandatory and the deadline is derived in one place.** The deadline
  is written into the provider's label, the local state record and the node's
  own self-destruct timer; three copies computed at three call sites are three
  copies that can disagree, and the one that disagrees is the one that does not
  stop the bill.
- **The deadline is not re-armable, and that is one invariant rather than a
  missing flag.** `cmd/embernode`'s `armDeadline` reads `EMBER_TTL_SECONDS`
  **once**, at container start, and nothing on the node listens for a change
  -- so a re-arm is not a flag, it is a change to *how* a node learns its
  deadline (a file it re-reads, or a heartbeat it requires), plus two more
  copies to update atomically. That is the entry above, traded away. So the
  answer is "pick the hours you mean and accept them", written down rather
  than implied: `ttl` is a hard stop and the node goes away at it, whatever it
  was in the middle of. A follow-up that wants it has to design the deadline's
  delivery first and the CLI second.
- **There is no idle-stop, and nothing in ember can see the work that would
  justify one.** A rental is bounded by the jobs it was given: a node rented
  to generate something stops when it has, which is what makes an idle node
  unrepresentable rather than merely discouraged. The one shape where an idle
  figure would mean anything is a server held open with nothing submitted, and
  what "idle" means there is "no tokens generated for N minutes" -- a fact
  inside the server's own payload rather than anything ember's own layers can
  observe. An `--idle-stop` built on a weaker signal destroys a node
  mid-generation, which is the one mistake that would make the feature not
  worth having. Same shape as the false-"abandoned" refusal: it answers "not
  idle" for everything it cannot be sure of.
- **Destroy runs on `context.WithoutCancel`.** Ctrl-C cancels the context the
  run was using, and a destroy call on that context fails instantly — the
  single most likely way to leak a GPU.
- **Ctrl-C on a SUBMITTED `ember up` detaches, so the `ttl` stops being a
  ceiling and becomes the exposure.** The entry above is about the process that
  holds the rental, and on the submitted path that is `ember serve` rather than
  the terminal: the run takes the drain loop's context, so there is nothing for
  the client's cancellation to destroy. That is the resilience #146 opens with
  and it removes the thing that used to guarantee no leak when you walk away.
  What is left is the TTL -- mandatory, derived in one place, written into the
  provider's label, the local record and the node's own self-destruct timer --
  so **the default ttl is now the bill an operator is exposed to**, where
  before Ctrl-C usually beat it. Said at the moment of detaching rather than
  left to an invoice: the client prints the deadline it read off `node_ready`,
  `ember ls` (which needs no daemon), `ember down <node>` and `ember history`.
  Exit **0**, because detaching is what was asked for and a non-zero status
  would say something failed. The daemon's own shutdown is untouched and still
  destroys: `ember serve` waits for `queue.Run`, which does not return until
  the deferred destroy has.
- **No daemon means `up` rents here, and the two code paths are written down
  rather than discovered.** #146's own comment argued for a refusal, correctly,
  for a NEW verb -- "a direct fallback is two code paths for a command that
  does not exist yet". `up` exists, every measurement in `README.md` was taken
  with it, and a version that refused unless a server happened to be up would
  break all of those for nothing. Auto-starting one is the answer that is not
  available at all: a background process that can spend money, started because
  somebody typed `up`, is not a thing ember does. So there are two paths and
  `README.md` carries the table of what differs -- Ctrl-C, `--allow-paid`,
  where the node's bytes go, and that `--dry-run` never asks a daemon because
  it rents nothing and would need the route to answer with a plan.
  **The "no daemon" probe is silent in one case and that is a conclusion
  rather than a shrug.** `loopback.StateSocket` refuses a path longer than
  bind(2) accepts, and `ember serve` resolves the same path through the same
  constructor -- so a state directory too long is one no daemon can have
  started under, and "there is no daemon" is the ordinary case rather than a
  finding. Printing it would put a line about a channel nobody asked for on
  every `ember up` in a deeply nested checkout, and that output is a contract
  this repository greps.
- **The intent record is written before the create call.** The window between
  the request and reading the response is where an orphan with no local record
  is born.
- **A provider that could not be reached is a finding, not a silence.**
  `ember ls` exits non-zero when any provider failed to answer: "I could
  not check RunPod" must never render as a clean bill of health.
- **The same rule met a third time, in a CACHE rather than on a status code: a
  failed probe is not an answer to memoise.** `local.DetectGPU` returned the
  same zero `GPU` for "there is no card" and "I could not ask", and
  `gpuNow` memoised it behind a `sync.Once` under a comment that justified the
  cache correctly and about the wrong thing -- *"a card does not appear or
  vanish inside one ember invocation"* is true of a card, and what was cached
  is a probe's **answer**. The premise also expired without anybody noticing:
  it was written when every invocation was one `ember up`, and `ember serve`
  builds **one** registry at startup, so that `Once` lives for the life of the
  process and `getOffers` runs the probe on the **HTTP request's** context.
  One browser reload during the first `/api/offers` -- a Docker ping plus an
  `nvidia-smi`, and the page has no AbortController -- therefore removed the
  operator's own card from the listing they pick hardware from, permanently,
  with `--gpu rtx4090` reading as "sold out everywhere". **And it silenced the
  one message written against exactly that**: the
  `nvidia-container-toolkit` diagnosis is guarded on `!g.IsZero()`, so a
  poisoned cache made the explanation unreachable and left silence with a
  working card in the machine. Paid rentals were never at risk, because every
  other caller builds a fresh registry per rental -- the blast radius is one
  command's listing, for as long as it runs.
  **Three states and not a bool flipped on error**, the `([]Exposed, bool)`
  shape `node.Exposure` already uses for "published nothing" against "never
  reported": a card, no card, and nobody could ask. A probe that **answered**
  is cached, including when the answer is "none", because the cost the cache
  exists to avoid is an `nvidia-smi` per listing on a machine that has never
  had a card; only the third state re-runs. The classification is `ctx.Err()`
  and not the error's own shape, because a cancelled `exec.Cmd` surfaces as a
  kill signal, a `Start` failure or a context error depending on where the
  cancel landed -- three spellings of one question -- and it is asked only on
  the error path, since an `Output` that succeeded is an answer whatever the
  context did next.
  **NOT `context.WithoutCancel`, which was the other candidate and is the
  entry three above.** That precedent is an *action* whose absence costs money,
  so the caller's Ctrl-C must not decide it; this is a **read**, and detaching
  it would need a bound of its own -- a clock nobody here has measured, and an
  unbounded one turns a wedged `nvidia-smi` into a listing that never answers,
  which is `runServed`'s bare `wait()` in another package. It also fixes one
  *cause* rather than the class: a probe killed by anything else, or a binary
  missing for one listing, would still be memoised. Re-running needs no new
  clock.
  **The seam carries the second result all the way up**, through
  `local.Provider.Detect` and `app.Deps.DetectGPU`, because a fake that could
  only say "no card" cannot reproduce the behaviour that cost this -- the
  fake-tidier-than-the-thing rule, applied to a seam before it has a defect of
  its own -- and a seam that collapsed the two at the `app` boundary would put
  the bug one layer up. `TestProvider_DetectsOnceWithTheRealDetectorByDefault`
  could not see any of it: **detecting exactly once is precisely what the
  defect does**, so the new assertion is that the probe count is *two* over a
  cancelled listing and a healthy one, with the acceptance and the cardless
  machine's single probe beside it -- without the count, "never cache
  anything" would pass.

- **A Ctrl-C'd rental's request carried no reason, because every write that
  records how a request ended travelled on the context the signal had just
  cancelled (#159).** The entry in **Threat model** that filed this -- the
  websocket lab's first version, which spent twelve minutes waiting for a
  reason that was never going to appear -- is the record of what it was; this
  is the repair. `Queue.finish` and `Queue.endIfUnfinished` both wrote through
  `Store.Update` on the drain loop's own context, which is the context
  `ember serve` cancels on SIGINT and therefore the context that produced most
  of the errors they were recording: `database/sql` checks it when a
  connection is taken, so the write answered `begin: context canceled` before
  SQLite was reached, the row kept the `running` `ClaimNext` wrote, and the
  next start's `Interrupt` sweep -- one statement that sets a status and an end
  time and nothing else -- turned it into `interrupted` with `requests.err`
  **empty**. So the whole of #103's argument, that the expensive failures all
  happen after money has been committed and an operator has to be able to see
  why, was unreachable on the one path an operator takes every time they stop
  the server. It is the same defect as "a rental that failed after the node
  existed left the request reading `running`", one layer further out: there the
  write was missing, here it was made and refused.

  **THREE WRITERS AND NOT TWO, which the sweep found rather than the issue.**
  `Queue.fail` is the third, and its own call site made the claim that was
  false of it -- "nothing was rented, so nothing is billing; the request
  carries the reason, which is the whole point of resolving before renting" --
  because `Planner.Plan` takes the same context, so a SIGINT landing during
  the offer listing, the store pre-flight or the period ceiling left that write
  unable to land too. It also wrote a literal `StatusFailed`, so detaching its
  context **without** routing it through `classify` would have traded a row
  with no reason for a row with a confident wrong one: a plan a money ceiling
  refused is a failure and a plan the operator interrupted is not. **What that
  consistency costs is written where the switch is**: `http.Client.Timeout`
  expiring satisfies `errors.Is(err, context.DeadlineExceeded)` and
  `internal/httpx` sets a 60s `DefaultTimeout` on every client, so a provider
  that did not answer now reads `cancelled` on the row where `fail`'s literal
  wrote `failed`. Recorded rather than repaired, because `finish` and
  `endIfUnfinished` have answered that way since 001a0d3 -- so this makes the
  third writer consistent rather than inventing the conflation -- and because
  the discriminator is whether OUR context ended, which is the one input
  `writeCtx`'s no-context-parameter design deliberately removes. That is
  **#171**, cited by number because a pointer to an issue nobody can find is
  the shape this file keeps correcting. There is one
  account of what Ctrl-C means in this package and it now has four callers,
  which is the reason `classify` was extracted in the first place.

  **BOTH HALVES, and the bound is not optional.** Detached for the `provision`
  destroy's reason -- "Ctrl-C cancels the context the run was using, and a
  destroy call on that context fails instantly" -- with a store write in place
  of the destroy call. Bounded because an unbounded write on a detached context
  is the second defect wearing the first one's clothes: `runServed`'s bare
  `wait()` wedged for twenty-three minutes after SIGTERM and never printed the
  error it was in the middle of returning, and `ember serve` waits for
  `queue.Run` before it exits, so a write that cannot complete is a server that
  will not stop -- in the code whose job is to record why the last one did not.
  Neither half is new: `Recorder.set`, `recordJob`, `recordImages`,
  `Queue.set` and `DirectRunner`'s own teardown write were all already
  `WithTimeout(WithoutCancel(...), writeTimeout)`, and the three writers this
  is about were the three that were not. They are one `writeCtx()` now, with
  ten callers where there were five inline spellings and one site that had the
  detachment and no bound: a copy per call site is a place for one of them to
  lose a half, and one of them had -- the "one name for a path, not one per
  call site" rule met in a context. And all three writers take
  **no context parameter at all**, which is "prefer making a bug
  unrepresentable over testing for it": the drain loop's context is not a thing
  a caller can hand them.

  **WHY NOTHING IN THE TREE SAW IT: `genstore.RecordingStore.Update` ignored
  its context, which is the seventh fake here tidier than the thing it stands
  in for.** Measured both ways rather than argued. Teaching the fake to answer
  `begin: context canceled` left `go test ./...` at **exit 0** across the whole
  tree, which is the proof that nothing anywhere had ever handed it a cancelled
  context. And the historical arrangement -- the fake as it was, all three
  writers back on the caller's context, this change's own tests taken out --
  is **exit 0** over `go test ./...` as well. The existing regression test was
  the sharp case: `TestQueue_ARentalEndedByCtrlCIsCancelledAndNotFailed` hands
  `once` a LIVE context and a `rentErr` of `context.Canceled`, so it asserted
  the classification over a write that could always land, and the real case has
  both. The fake's check is on two methods rather than on all of them, and
  that is stated rather than silent: `Update` and `AppendEvents` are the writes
  a teardown reaches, and a check on the rest would be a guard with no defect
  available to catch. The one other call that takes a rental's context is
  `once`'s `PutCaps`, and a caps cache that did not land is logged and harmless
  -- the next request is checked against a card instead of against a cache,
  which that call's own comment already says -- so it is left alone rather than
  counted as a third.
  `TestWrite_ACancelledContextWritesNothing` is in the both-stores table, and
  asserts the row and the event rows are **untouched** as well as the errors,
  because an error alone is consistent with a store that wrote and then
  complained.

  **AppendEvents is in that list because the sweep found two more writes, and
  the event stream is the half this file calls the truth.** `generate` flushed
  what the batch had buffered AFTER `s.Generate` returned, on the same
  cancelled context -- so the last progress an operator watched go by was the
  one the database never got, and the columns are a derived index of exactly
  those rows. A progress frame is not `urgent`, so that flush is the only thing
  that can write it. The other is the urgent flush inside `Recorder.Emit`,
  which ran on a bare `context.Background()`: detached, and **unbounded** --
  on the emitter's goroutine, which is the drain loop, so it is `runServed`'s
  bare `wait()` reached through an event rather than through an ssh session.
  Both take `writeCtx` now, and `sink.flush` takes no context either. The
  second has a test, which the first wording of this entry said it could not;
  see the paragraph on it below.

  **The cancellation arrives while the rental is up, through a hook, and not as
  a cancelled context handed to `once`.** `ClaimNext` takes the same context
  and the real store refuses a claim on a cancelled one, so a test that
  cancelled first would be asserting about a state the program cannot be in.
  `fakeRental.onRented` fires once the node exists, which is where a SIGINT
  lands.

  **The batch-side assertion is the SENTENCE and not the status, because the
  two writers cover each other on that path.** With `finish` writing through
  the cancelled context its write is lost, the row is left non-terminal, and
  `endIfUnfinished` -- now detached -- writes the status itself, so an
  assertion on `cancelled` alone would pass against the defect. What
  `endIfUnfinished` cannot write is the same reason: `drain` wraps the batch's
  error as `request <id>: …` before returning it, so the string on the row says
  which writer landed.

  **AND THERE WERE TWO CLASSIFICATIONS, in one package, each under a comment
  saying there was one.** The issue's own terms are that `classify` must be the
  single account of what Ctrl-C means, which is what sent somebody to read it
  -- and `record.go` held `statusOf`, the same switch returning the same
  answers for every input, used by `Recorder.Finish`, which is how `ember up`
  and a submitted run record the same fact. `git log -S` settles the order and
  it is the embarrassing one: `statusOf` came first, in cbd3f14, saying "one
  spelling for the queue and for `ember up`", and **001a0d3 -- the commit whose
  entire argument is that two copies would be two accounts -- extracted
  `classify` four files away from it**. So the change that wrote the rule
  created the second copy of it, in the same package, and the sentence in this
  file that quotes the rule has been standing over two copies ever since.
  `statusOf` is deleted and `Finish` calls `classify`. **Nothing could have
  reported it and that is stated rather than papered over**: both copies had
  callers, so `unused` and `TestNoProductionDeadCode` are both correct to stay
  quiet, and no assertion can fail over two functions that agree -- reinstated,
  the duplicate is **exit 0** over `go test ./...`. What the duplication risked
  is a divergence, and the two paths are covered either side of the deletion
  (`TestRecorder_FinishClassifiesHowARunEnded` and the queue's own), so one
  function is what makes them unable to disagree rather than a test being what
  notices. The `node.Memory` shape with both halves alive.

  **The bound's test carries its own deadline, because A WEDGE PRINTS NO
  `--- FAIL`.** CI reads `go test`'s status and greps for `--- SKIP`, and a
  hang is invisible to both until the binary's own `-timeout` dumps goroutines
  naming nothing a reviewer can act on -- so the thing being waited for is
  raced against the thing that means it will never happen, which is
  `orExited`'s rule as the browser labs spell it. The store blocks until the
  context it was handed is done, the four writers run as parallel subtests, and
  the pass costs ten seconds of wall clock for the same trade `ws.go`'s
  ten-second write bound already makes. Reinstated, each subtest is red at
  **20.0s** rather than hanging. And -- because belt and braces reads exactly
  like a dead gate -- the bound was deleted with that test renamed out of the
  suite: `go test ./...` is **exit 0**, so nothing else in the tree reports an
  unbounded write.

  **Fourteen cases, judged on exit codes, controls at both ends, one backup per
  path taken once, and every case ending in a diff against the pristine copy
  rather than in an exit code.** Nine red where expected: each of the three
  writers on the caller's context, `fail` with a literal `StatusFailed`, the
  bound removed, `endIfUnfinished` writing unconditionally (which also turns
  the pre-existing control red), the fake ignoring its context again,
  `generate`'s flush of the batch's tail, and the urgent flush left unbounded.
  Five green where expected, each a measurement rather than a gap: the bound
  removed with its own test taken out, the whole historical arrangement, the
  duplicate classification, and two reworded comments. One case reported
  ANCHOR-MISS rather than a result, because a test this change renamed left its
  patch pointing at a name the tree no longer has: the harness refusing to read
  a stale anchor as a verdict, which is the first of the shapes below working
  rather than a sixth.

  **THE URGENT FLUSH'S BOUND IS IN THAT RED LIST BECAUSE A REVIEWER REFUSED AN
  INFERENCE, AND THIS PARAGRAPH IS WHAT IT SAID BEFORE.** It read that nothing
  in the tree *could* see that one -- `context.Background()` never cancels and
  the fake answers at once -- and offered it as the
  `subtle.ConstantTimeCompare` answer to "which of these has no exit code".
  The **measurement** was right and reproduces: reinstate it and the suite is
  exit 0. The **inference** was not, and it is the step this file has already
  had to retract twice, about the trust-on-first-use lab and about
  `runServed`'s own cap: "nothing here does see it" is not "nothing here can
  see it". What sees it is forty lines in the shape the two cases above it
  already used -- a `Recorder` whose store blocks `AppendEvents` until its
  context is done, an `Emit` raced against the test's own deadline -- and it
  costs no wall clock at all, because it shares the ten seconds the other
  subtests were already spending. PASS at 10.01s on the repaired tree; with
  `r.Flush(context.Background())` back, FAIL at 20.01s naming the write that
  held. The thing worth keeping is not the test but the order it was found in
  -- the claim was written, measured, believed, and then disproved by somebody
  writing the test the claim said could not exist.

  **AND THE SAME SHAPE ONE LAYER IN, found by a reviewer taking that test's own
  250ms claim seriously enough to follow it.** The entry above used to say both
  halves of `Emit`'s condition got a subtest "because `due` and `urgent` reach
  one flush by different routes: `flushAfter` is 250ms against a zero `last`,
  so the first event of a run is due whatever its kind". Every clause of that
  is true, and the conclusion is backwards: `due || urgent(e.Kind)`
  **short-circuits**, so an urgent event on a fresh recorder is flushed by
  `due` and the urgent arm is never evaluated -- the urgent-named subtest was
  proving what the one beside it proved. Measured with the bound in place, so
  that reaching the flush reads as 10s and not reaching it as 0s: with the
  urgent arm deleted entirely the urgent subtest still sat at **10.00s**. The
  **property** was covered either way, because both arms reach one call site
  and one subtest bounds it; what was not covered is the **condition**, so
  splitting that line into two call sites later could have left the urgent
  branch unbounded with the suite green. One line fixes it -- `r.last =
  time.Now()` on that recorder, which makes `due` false -- and the
  discriminator then flips in both directions: full condition, urgent
  **10.01s** and due **10.01s**; `if due` alone, urgent **0.00s** and due
  10.01s; `if urgent(…)` alone, urgent 10.01s and due **0.00s**. "What would
  this assertion say if the bug were present?" answered "the same thing the
  subtest beside it says" is not an assertion about its own arm, which is the
  retraction above arrived at from inside the test that fixed it.

  **A SIXTH HARNESS SHAPE, and it is the "cannot tell a build failure from a
  pass" trap inverted into a FALSE RED.** The first run of this pass reported
  A, B and H as red, and two of the three were a test package that did not
  compile: the defect is a restored `ctx` parameter, the bound test calls those
  two writers directly, and the harness build-checked with `go build ./...` --
  which does not compile `_test.go` files at all. So a signature change reads
  as a caught defect, in the one direction a harness cannot be caught out
  safely, since a false red is indistinguishable from the thing being looked
  for. The check is `go vet` on the two packages now, which compiles their
  tests, and the A and B patches move the bound test's own call sites with the
  signature so that what the suite reports is an assertion rather than a
  compiler. It was found by asking why H had gone red when it was written as a
  measurement expected to be green -- the "ask why a case went the way it did
  rather than accepting it" rule, which has now caught a harness defect from
  both directions.
