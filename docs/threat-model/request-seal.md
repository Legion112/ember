# What a request may name

A request names a provider, a card, a kind and a batch of jobs — and nothing
else. Two rentals at once are unrepresentable rather than refused.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **A request may narrow the config's ceilings and never widen them.** `ember
  up`'s `applyFlags` *replaces*: `--max-hourly-usd 999` raises the ceiling,
  because the person typing it is the person who set it. A request is not that
  person, so `queue.Narrow` takes the smaller of the two and `internal/app`
  receives a `provision.Budget` it has no way to widen. The config path comes
  from the `serve` flag and is never accepted over HTTP, or the browser would
  be naming which file sets its own limits.

  **The config path IS accepted on the unix socket, and that sentence is
  amended rather than left to be contradicted by the code.** `POST /api/runs`
  carries one, and the overrides on it REPLACE rather than narrow, because it
  is `ember up`'s command line. What makes that sound is not a check: the route
  is registered on `s.sockMux` and the loopback port's `s.mux` never learns the
  pattern, so a page, a `curl` on 7777 and anything reaching an `ssh -L`
  forward -- token or no token -- reach no handler at all. The two muxes differ
  by one registration, as the two `http.Server`s differ by one `ConnContext`,
  and `sockMux` delegates `/` to `s.mux` so there is ONE route table plus one
  route rather than two tables that can drift. A runtime check on
  `arrivedOnSocket` was the one-line alternative and is the wrong shape: it
  makes the route exist, and a route that exists is one somebody can make
  reachable by moving a condition. On an OS-authenticated socket the caller is
  the operator, with the same authority as typing the command in their own
  shell -- which is the only reason the reversal is available at all, and it is
  written at `app.RunAsk` where it happens.
  **The two rules are one function each now**, which is the half that was a
  hazard before any of this: "a flag replaces" was `cli.applyFlags`,
  unexported, so the socket route would have grown a second copy -- a second
  copy of a money rule, with the opposite rule four files away. It is
  `app.Overrides.Apply`, and both callers call it.
  **One thing on that channel is NOT the operator's own shell, and it is
  `--allow-paid`.** An `ember serve` left running without it is a process the
  operator has said must not spend, so a door that spent anyway would make the
  flag mean "unless you came in the other way" -- which is the thing
  `refusePaid` is checked twice to prevent. So `queue.DirectRunner` calls the
  same `refusePaid` the drain loop does, and the honest cost is written in
  `README.md`: the identical command spends with no daemon and is refused
  through one, because the flag is a property of the long-running process and
  not of the request.
  **THERE ARE TWO SUCH THINGS NOW AND THE SECOND IS `max_day_usd` AND
  `max_month_usd` (#165).** The sentence above says "one thing", and the reason
  it was one was that the period ceilings reached this channel not at all: the
  run's config travelled and nothing read either key from it or from the
  daemon's. A ceiling is a question about a LEDGER and the ledger a submitted
  run is recorded in is the daemon's, so `DirectRunner.Config` is the daemon's
  own file and `Ceilings.Tightest` composes it with the run's -- both bind,
  neither widens the other. Unlike `--allow-paid` this is not the daemon's
  answer *instead of* the operator's: it is both answers, because the only
  reading under which one of them could be dropped is one where an operator's
  written ceiling stops applying. The whole argument is the entry at the end of
  **Money**.
- **A request names a provider, a card and a batch of jobs, and nothing else.**
  No image, no store, no mount, no ports, no serve argv, no gpu probe, no
  assets, no `extra`. Those stay in `ember.yaml`, which is what keeps every
  seal above upstream of anything a request can influence -- a request body
  cannot reach `node.MountAt`, `exposure.SSHPort` or `sdserver.ServeCommand`
  because it is never consulted about them. `extra` is refused structurally:
  the field does not exist on the wire type, and `genstore.Job` has none
  either, so a round trip through the store cannot invent one.
- **Two rentals at once are unrepresentable rather than refused.** There is one
  drain goroutine and a rental lives inside one iteration of its loop. The
  alternative -- a handler that starts a rental per request with a counter
  saying no to the second -- is a counter that can be wrong, and being wrong
  costs two GPUs. A double-clicked button cannot rent twice because there is
  nothing for the second click to start. It is also the shutdown story:
  `queue.Run` does not return until `provision.WithNode`'s deferred destroy
  has, so waiting for one goroutine is waiting for the node to be gone.
  `http.Server.Shutdown` drains requests, and a rental does not live in a
  request -- `ember serve` waits for both.

  **There are two kinds of work on that goroutine now and still one place it
  runs.** `ember up` submitted over the socket is not a queued request -- the
  four reasons it cannot be one are the entry below -- so it does not arrive
  through `ClaimNext`. It arrives through `Queue.Submit`, which is a
  **rendezvous on an unbuffered channel only the drain loop receives from**: a
  send succeeds exactly when the loop is between iterations, so `Submit`
  returning means the loop owns the run, and the loop then runs it inline where
  a claimed request's rental lives. A submission offered while a rental is in
  flight **waits**, which is the honest third answer to "rent now" from a
  process that is already renting -- refusing would be a message where a wait
  is the fact, queueing it is not what `up` means, and renting is the second
  GPU. The buffer is what would have made it a counter: "accepted, and it will
  start some time" needs somebody to know how many are already waiting.
  **The context split is the detach story and it is structural.** `Submit`
  takes the CLIENT's context and bounds only the wait; `Direct.Run` takes the
  LOOP's and bounds the rental. So a Ctrl-C after the 202 cannot reach the
  node, because the rental never held anything the client owned -- rather than
  because something remembered not to pass it along. A Ctrl-C *before* the 202
  cancels a submission, where nothing has been rented, and the row Adopt
  already wrote is ended with the reason rather than left reading `running`.
  **A SHUTDOWN HAS TWO OUTCOMES AND `Submit`'s COMMENT CLAIMED ONE**, which is
  this file's own rule turned on the paragraph that states it: it read "the
  drain loop returns when `ember serve` is stopping and nothing will ever
  receive", and that is true only of a submitter still PARKED. `Run`'s idle
  select has `<-ctx.Done()` and `<-q.handoff()` as two cases of ONE select, so
  a shutdown landing while a submission is parked makes both ready and Go
  picks uniformly -- and if the handoff wins, the loop TAKES the run, reaches
  the top-of-loop `ctx.Err()` and returns having started nothing, with
  `Submit` already answered nil. Something did receive it. **Measured, because
  the obvious shape reports it as unreachable**: with the send and the cancel
  released from one closed channel, 3000 attempts gave 21 accepted
  submissions, 2 of which ran and **19 taken and dropped**; the control --
  park the send, then cancel, where `ctx.Done()` is not yet ready when the
  handoff is -- gave 400 accepted of 400 and **400 ran, 0 dropped**. The
  OUTCOME was right the whole time and only the account was wrong: nothing
  rents but `Direct.Run`, so there is no money and no leak either way. What
  was missing is that the drop was **silent**, so `Run` names the request in a
  warn before that return and "swept to `interrupted` by the next start" is
  something a reader can confirm rather than take on trust. A `done` channel
  is still refused, and more firmly: it would make the taken case a refusal,
  which is the same outcome with more state. The committed test is over the
  LINE and not over the race, because a test that raced for 19-in-3000 is the
  flake this change was already corrected for once.
  **What a test can see of "structural", because this file has been wrong about
  mechanisms before.** Three assertions, each ruling out a different wrong
  answer: `Submit` does not return while a queued rental is open, and the error
  when it gives up is the caller's own DEADLINE rather than a refusal (so a 409
  implementation is red); the fake counts open rentals across both kinds and
  the maximum is one (so a goroutine beside the loop is red); and the run's own
  goroutine stack contains `(*Queue).Run` (so a goroutine with correct locking
  -- a counter with better manners, which is the shape somebody would reach
  for -- is red even though the first two would pass it).
  **That last clause is MEASURED against the answer it is for, which is the
  only reason the third assertion is worth having.** The obvious
  reinstatement -- a goroutine with no locking at all -- is caught by the first
  two, so it proves nothing about a check. The one that does is a correctly
  locked check: a context-aware semaphore shared with the drain loop, and the
  run on a goroutine of its own. Reinstated, `Submit` still waits with the
  caller's own deadline and the maximum number of open rentals is still one --
  **both of the first two assertions pass** -- and
  `TestSubmit_TheRunExecutesOnTheDrainLoopsOwnGoroutine` is what goes red.
  The honest limit is
  that a stack is evidence about one goroutine: `cmd/check-coverage`'s
  `TestNothingRentsOutsideInternalApp` bounds the other half by asking which
  packages may name `provision.WithNode`, which is one.
  **That allow-list was measured and the first answer was wrong.** This entry
  read "one, and has been one since that function was written" -- and over all
  178 commits here, counting the CALL and not the name, `internal/cli` is in
  **135** of them: `cmd_up.go` and `cmd_store.go` both called it directly until
  346f10f, 86 commits ago. So the list would have needed exactly one edit, and
  that edit was the commit that moved the rental into `internal/app` -- a
  deliberate consolidation rather than the routine churn that makes an
  exception list worse than no guard, which is still the argument for having
  it. One more instance of the thing this file keeps recording about itself,
  found by counting rather than by review, in a sentence nobody would have
  doubted. The `-l` form of the same grep is why the guard is a parse: on
  today's tree it reports `internal/cli`, `internal/queue` and `internal/logx`,
  because three comments there name `provision.WithNode` as the thing their
  invariant rests on -- a guard keyed on the spelling would fire on the
  argument for the rule, which is `TestNoListenOutsideLoopback`'s own first
  version flagging `loopback.Listen`.

- **`max_hourly_usd` bounds a card, `max_run_usd` bounds a rental, and
  nothing bounded a sequence of them.** `ember serve` is the first thing here
  that can produce a sequence without the operator being at the keyboard for
  each: thirty queued requests at $1.79/hour on a one-hour TTL are inside
  every ceiling ember had and are $54. `max_day_usd` and `max_month_usd` bound
  a UTC calendar period, checked after the offer is chosen and before anything
  is created -- the same moment and the same arithmetic `max_run_usd` uses, so
  the figure a period is charged and the figure one rental is bounded by
  cannot come apart. This is NOT the mid-run meter refused above: that one
  needed a clock, a price and a running total kept correct *while* a rental is
  in flight, and this needs the completed cost of finished rentals, which is
  already recorded, numbered and durable. One more `SUM` over rows that exist.
- **A request is not asked about the period ceiling, which is stronger than
  narrowing it.** Every other ceiling here follows "a request may narrow the
  config's and never widen them", through `queue.Narrow`'s `smaller`. These
  have no field on the wire type at all, so the limit that stops a browser is
  one the browser cannot name -- the `extra` refusal applied to money rather
  than to parameters.
- **It projects rather than reports, and that is the difference between a
  ceiling and a report.** A check that refuses only once a period has already
  been exceeded is a ceiling every period breaches by one rental, and one
  rental is the $54 this exists for. The projection is the offer's price over
  the *narrowed* TTL, because that is the deadline the node will actually get.
- **A calendar period, and the hole it leaves is written down.** An invoice is
  what the operator is defending against and an invoice is calendar, so a
  rolling window would be a second time concept for a worse match. The cost is
  that a day ceiling resets at midnight UTC and a queue can spend one day's
  allowance twice in two hours across the boundary; what bounds that is the
  month ceiling, which is why both exist and why the month is the one to set
  if only one is set. UTC for the reason `ember spend`'s day bucket is UTC: a
  ceiling that moves with a laptop cannot be reasoned about.
- **It refuses rather than quietly renting something cheaper.** Narrowing
  `max_run_usd` to the remaining headroom was the first design and would have
  reused the existing mechanism exactly -- and it would have meant an operator
  who asked for an H100 getting a 4090 because the month was nearly spent.
  ember exists to measure what a card costs, and a run that silently happened
  on different hardware is a measurement nobody can use.
- **A free rental is never refused by a money ceiling.** The local provider's
  price is zero, so its projection is zero and it adds nothing to any period.
  Refusing one because a paid month is spent would stop the one kind of run
  that costs nothing, which is the opposite of what the ceiling is for.
- **"I could not read what has been spent" is not "it is allowed", and a
  configured ceiling with no ledger is a refusal.** Both are the rule that
  makes `ember ls` exit non-zero over a provider it could not ask: an
  operator who wrote `max_month_usd` has said unattended spending must stop
  somewhere, and "probably fine" is the one answer a spend ceiling must never
  give. The second cannot happen through `ember serve`, which always wires the
  store; it is a refusal because a ceiling that silently does nothing is worse
  than one that was never configured.
- **It counts rentals and not store standing charges, which is a known gap.**
  A store bills by the gigabyte-month whether or not anything runs and
  `reconcile.MonthlyTotal` already prices one, so a ceiling that ignores a
  $40/month store is wrong by a known amount. Counting it would mean a
  provider scan inside the drain loop, and a provider that could not be
  reached would leave two choices: block the queue, which is worse than the
  gap, or carry on without that store's charge -- which is the gap with extra
  steps and a false sense that it was covered.
- **It bites at selection and not at enqueue, although enqueue would be
  kinder.** #31 suggested both: a warning when the request is queued and a
  refusal when it is selected. Refusing at enqueue is a FALSE refusal across a
  period boundary -- work queued on the 30th for next month would be rejected
  although the queue would have run it after the turnover -- and a warning
  field in the enqueue response that the page does not render is the "built
  end to end and never connected" failure this repository has paid for three
  times. What is built instead costs nothing and is not dead: `ember serve`
  prints where both periods stand at startup, so an operator knows their
  headroom before they queue thirty requests, and a refused request carries
  the reason, which the page shows and `ember history` prints.
- **The ceiling's own input has no gap, unlike `ember spend`'s per-job
  figures.** `requests.cost` was a column before the execution columns
  existed, so every rental ember has ever recorded contributes to a period
  total. Worth stating because the command beside it has to warn that its
  other columns are incomplete, and a reader could reasonably assume the same
  caveat applies here.
- **Two parts of #27 were refused, and here is why.** It asked for
  `ember spend` to default to the current calendar month: a default that
  silently excludes everything older is the "a total missing part of its
  history says so" rule inverted, and "what has ember ever cost" would have
  had no way to be asked, so the period stays unbounded and one flag away. It
  also asked for store standing charges as a line on the spend report, which
  contradicts its own done-when -- pricing a store means asking a provider
  what stores exist, and "no provider contacted" is the property that makes
  these two commands safe to run from a timer. The standing charge stays in
  `ember ls`, where the scan already happens.
