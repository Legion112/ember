# The scheduler

How a claim is held and what takes it back, why capabilities are keyed by
rental shape, and the one relation both sides of fit-or-create are built from.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

`max_nodes` itself, strict packing and the ledger race are in
[rental-lifecycle.md](rental-lifecycle.md); the per-rental overhead and the
reload figures this page spends are in [batching.md](batching.md). This page is
the layer above them: which rental may take which request, and what happens to
a claim whose holder died.

## The claim lease

- **A CLAIM USED TO BE HELD BY NOTHING.** `ClaimNext` wrote `run_id` and
  `started_unix` and there was no expiry and nothing that reclaimed them. The
  only thing that ever moved a `running` row was `SQLite.Interrupt`, which
  blanket-marked **every** running row as interrupted at startup. That is safe
  when one process owns the file and starts from nothing, and it is wrong the
  moment it is not: a second ember, or a restart while a node is still inside
  its TTL, marked rows whose rentals were alive and billing.

- **THE OLD COMMENT'S PREMISE WAS THE DEFECT, AND HALF OF IT SURVIVES.**
  `Interrupt` justified itself with *"its rental is gone"*. That is not
  ember's claim to make from this side of the wire -- what destroys a leaked
  node is `internal/reconcile` reading the provider's label, which is the
  source of truth, and this column is not. The other half of the old argument
  is still exactly right and is why a reclaimed row is **not** re-queued:
  renting a second card for work the operator may no longer want is the kind
  of help nobody asked for. So `Interrupt` became `Reclaim`, the false half
  was replaced with the argument rather than deleted, and the log line no
  longer asserts it either -- it says the lease ran out and sends the operator
  to `ember ls`.

- **THE EXPIRY IS A HEARTBEAT TERM AND NOT A WORK BOUND, AND THE FIGURE THAT
  DECIDES THAT IS 14m47s.** The obvious expiry is "the claim plus the
  request's TTL", on the reasoning that a node cannot outlive its own
  deadline. It is wrong by however long planning takes: the node's deadline is
  derived from when it was **created**, and between the claim and the create
  sit an offer lookup and, when the store is empty, a whole second rental that
  fetches the weights -- measured at **14m47s** in
  [rental-lifecycle.md](rental-lifecycle.md). A lease that expired in there
  would be reclaimed out from under a request whose card was still being got
  ready, which is the one direction this column must never fail in. So
  `genstore.LeaseTerm` is **2 minutes** and `Queue.Run` renews every row it
  holds at the top of each iteration.

- **TWO MINUTES AGAINST A FIFTEEN-SECOND POLL, AND THE RATIO IS THE POINT.**
  The supervisor's longest park is `idlePoll`, 15 s, so eight missed renewals
  is a process that is not running. Shorter would reclaim a live rental's row
  over one slow statement. Longer costs only how stale a dead process's row
  looks, and nothing spends money on that -- `reconcile` and the node's own
  self-destruct timer are what stop the bill.

- **THE LEASE IS WRITTEN IN THE SAME STATEMENT AS THE CLAIM.** A two-statement
  version has a window in which the row reads `running` with a zero lease, and
  zero is expired, so a `Reclaim` landing in that window takes the row back
  from the rental that has just this moment got it. Same argument as
  `ClaimNext`'s own ONE STATEMENT rule, for the same reason.

- **ZERO IS EXPIRED, NEVER "NO EXPIRY".** Every row written before the column
  existed holds zero, so an upgraded file treats its old rows exactly as the
  blanket sweep did. The dangerous default -- a row nothing will ever reclaim
  -- is unrepresentable rather than avoided by remembering to backfill. The
  consequence is that `Adopt` has to lease the run it records (`adopted + ttl`)
  or a live `ember up` would be reclaimed the instant anything looked.

- **RENEWAL IS KEYED ON REQUEST IDS, NOT ON `run_id`.** `Queue.held` already
  answers precisely "which rows read `running` because this process is alive"
  -- it is what `webapi` asks to choose between two 409s -- and it covers the
  submitted-run path, whose row has no claim lease at all. A separate set for
  the renewal would be a second answer to one question, free to fall behind in
  the direction where a live rental's row is reclaimed.

- **THE HEARTBEAT IS ON THE SUPERVISOR'S LOOP AND NOT ON A TICKER OF ITS
  OWN.** A lease renewed by a goroutine that outlives a wedged supervisor is a
  lease that says nothing: it would report the process as healthy for exactly
  as long as the process was useless.

- **`Reclaim` RUNS ON THE LOOP AS WELL AS AT STARTUP**, because "the process
  that held this is gone" becomes true while ember runs -- a second ember,
  SIGKILLed -- and not only before it started.

- **`Renew` CAN ALSO SHORTEN A LEASE, AND THAT IS RIGHT.** Once this process
  is renewing an `Adopt` row, the honest expiry is "this process is alive"
  rather than "the node has not timed out", so a socket `ember up` killed with
  SIGKILL is reclaimed in 2 minutes instead of an hour.

- **THE LEASE IS DELIBERATELY NOT A FIELD ON `genstore.Request`.** `Update`
  takes a callback over a Request and writes back every column it names, so a
  lease reachable from there is one any caller could extend whenever it liked
  -- which is the same as not having one. Three statements write it and
  nothing else can.

## Capabilities keyed by rental shape

- **ONE CACHED ROW PER PROCESS WAS CHECKING REQUESTS AGAINST THE WRONG NODE.**
  `PutCaps` wrote the generation server's capability document to a single
  un-keyed row, justified by *"`sd.serve` is fixed by the config, so every
  rental in this process reports the same capabilities."* A request names a
  **provider and a card**, so two live rentals can differ in both, and an
  sd-server's reported capabilities can legitimately differ with the card.
  Enqueue-time validation -- the whole point of the cache, so that a mistyped
  sampler is refused when it is typed rather than a minute into a rented card
  -- was answering from whichever rental wrote last.

- **THE KEY IS `Shape{Kind, Provider, GPU}`, WHICH IS EXACTLY WHAT A REQUEST
  MAY CHOOSE.** The `caps` table has a primary key over the three, so "two
  shapes, two answers" is a property of the schema rather than of a string
  somebody builds correctly. `NewShape` folds the kind through `KindOf` and
  the card through `node.NormalizeGPU`, because a key built two ways is two
  keys.

- **THE KEY IS THE ASK AND NOT THE SILICON, AND THAT BOUNDS WHAT THIS CAN
  DO.** The only reader is enqueue validation, which runs before any node
  exists, so the ask is all it can look up by. A request that named no card is
  therefore still checked against whatever card the last "any card" rental
  landed on -- the same uncertainty as before, now confined to that one shape,
  and exact for every shape that names its card. Keying on the rented card
  would make every row unreadable by the one caller that needs them.

- **THE MIGRATION DELETES THE UN-KEYED ROW RATHER THAN ADOPTING IT**, for the
  reason schema 2 already deleted an incomplete one: nothing recorded which
  shape produced it, and inventing one would assert that last week's rental
  was this week's card. That is the mistake `requests.model` exists because
  of.

## What the worker advertises: nothing

- **THE PLAN SAID THE WORKER WOULD ADVERTISE ITS IMAGE, VOLUMES, CARD AND
  QUEUE DEPTH. IT ADVERTISES NONE OF THEM, AND THE ARGUMENT IS THE THREAT
  MODEL'S.** Asking a rented machine to name its own silicon would make a
  packing decision -- and therefore a decision about money -- rest on a rented
  machine's self-report, which is the trust edge
  *"a tar stream from a rented machine is untrusted input"* generalises. There
  is no change to `internal/control` or `internal/worker` in this slice at
  all.

- **THE CARD IS EMBER'S OWN FACT.** ember chose the offer, so ember holds the
  card: `Compat.Card` is filled from `plan.Offer.GPU` one statement after
  `Plan` returns and **before anything is rented**.

- **THE IMAGE AND THE MOUNTED STORE ARE UNMATCHABLE AS DIMENSIONS, AND THAT IS
  SAID RATHER THAN WORKED AROUND.** Both are `app.ResolveImage` and
  `BuildSpec` over this process's own config and that card. A request cannot
  name either -- that is
  [the request seal](../threat-model/request-seal.md) -- and `ember serve`
  loads its config once, so two requests the compat calls interchangeable
  resolve the same reference *by construction*. Widening the request to make
  the scheduler tidier was the alternative and is refused. What was done
  instead is to make the structure checkable:
  `TestResolveImage_DependsOnTheOfferThroughTheCardAndNothingElse` varies the
  provider, token, GPU count, VRAM, price, vCPU, region, reclaim tier and
  label one at a time and asserts the image does not move.

- **CAPABILITIES ARE THE ONE THING THE NODE KNOWS AND EMBER DOES NOT, AND THEY
  ALREADY TRAVEL.** They come back verbatim on `control.KindPrepared`, which
  is the right frame because they are the generation server's own answer and
  arrive exactly once per rental, when the weights are in. `KindHello` is
  wrong for them: it carries a version the handshake must refuse **before**
  any work is pushed, so it cannot wait on a model load.

## Fit-or-create

- **MATCHING ASK AGAINST ASK UNDER-PACKED IN THE DIRECTION THAT COSTS
  MONEY.** A request that named no card was refused a ride on a rental that
  asked for an rtx4090 and got one; a request that named rtx4090 was refused a
  ride on a rental that asked for nothing and landed on exactly that. Both
  times ember rented a second node for work the first one could serve.
  `Compat.Card` closes both: an ask of `""` is satisfied by any card, and a
  named ask by any card `node.GPUModel.Accepts` admits.

- **THE WINDOW THAT LOOKED LIKE "ONE OFFER LOOKUP" IS THE ORDINARY CASE, HIT
  DETERMINISTICALLY. THIS IS THE MEASUREMENT THAT CHANGED THE DESIGN
  MID-FLIGHT.** The first build used one exact relation on both sides, on the
  reasoning that a rental whose offer had not come back yet was too narrow a
  state to pay for. The packing test went red on its first run: `claim` spawns
  and `continue`s, so the supervisor's **next** create-side decision is always
  taken before the slot goroutine has run `Plan` at all. With the exact
  relation there, two queued requests whose asks differ and whose silicon
  agrees provoke two nodes every time -- precisely the defect `Card` was added
  to close.

- **SO THE EXCLUSION WIDENS WHILE THE CARD IS UNKNOWN AND THE RIDE RELATION
  DOES NOT.** A live rental that is still choosing an offer excludes
  everything of its kind and provider (`servesPendingClause`), which is the
  money-safe reading of "might serve it" on the side that spends money. A
  rental must never take a request for a card it does not have, so the ride
  side stays exact (`servesClause`). `CardKnown` is a second field because the
  uncertain state is real and the two sides answer it differently.

- **WHAT THE WIDE SIDE COSTS, BECAUSE A STALL IS NOT FREE.** A request of the
  same kind and provider waits out that rental's `Plan`: seconds in the
  ordinary case, up to **14m47s** when the store is empty. The long case looks
  alarming and is not, because the weights are the config's and are shared --
  a second rental of that kind would want the same store, `StoreFills` already
  serialises the fetch, and it would wait for it **holding a slot** instead of
  waiting here holding nothing. A request of a different kind is never
  excluded by it, which is what keeps a speech request out of an image
  rental's store fill. `Queue.advertise` kicks the supervisor the moment the
  card is known, so nothing waits out a poll interval on top.

- **ONE RELATION, BUILT ONCE AND READ TWICE.** `servesClause` is the ride side
  and, negated per live rental, the exclusion. Two copies of one condition
  drift in two expensive directions: an exclusion **wider** than the ride
  relation starves a request for the length of a rental, and one **narrower**
  hands it a second card.
  `TestClaimNext_TheRideRelationAndTheExclusionAreOneRelation` asserts the
  complement over a 3x5x7 matrix read off the **store**, so it holds the SQL
  and the fake's Go spelling to each other rather than each to itself.

- **THE CARD TEST IS AN `IN`-LIST COMPUTED IN GO, NOT `LIKE gpu || '-%'`.**
  `_` is a LIKE wildcard, and `a100_80g` would match patterns it has no
  business matching; building a pattern out of stored text is grep where this
  repo wants parse. The prefixes come from `Accepts` itself, so one function
  decides what admits what.

### Queue depth is not a dimension

Four reasons, in the order that decides it:

1. **It is not a column of `requests`,** so there is nothing in `ClaimNext`'s
   `WHERE` to compare it against. Every other `Compat` field compares a
   request column to a rental fact; depth is purely a rental fact, so a
   predicate over it is a constant within the statement -- a **gate**, not a
   match, and a gate belongs in the caller where a reader can see it.
2. **ember already knows it exactly.** `pushBatch` pushes a request's whole
   batch and does not return until `ended == total`, and `drain` claims the
   next request only after that returns -- so a worker's queue holds one
   request's jobs and never two requests'. The depth is `len(req.Jobs)` of the
   request the rental is serving, a number ember wrote itself. Advertising it
   would be asking a rented machine for a number ember derives from its own
   bookkeeping, and if the two disagreed ember would have to believe the node.
3. **Using it to exclude on the create side is the scale-out that strict
   packing refuses.** "This rental has six jobs queued, so rent another card"
   buys latency at ~27 s of per-rental overhead plus 80-96 s per image of
   weight reload. See [rental-lifecycle.md](rental-lifecycle.md).
4. **It cannot make a claim more permissive either.** A rental that can serve
   a request can serve it at any depth; depth only changes *when*.

## One defect found on the way

- **`requests.gpu` stored whatever a browser sent.** Three things read that
  column -- the compat matches it against a live rental's card, the caps cache
  is keyed on it, and `ember spend --by gpu` groups by it -- so
  `NVIDIA GeForce RTX 4090` and `rtx4090` were two cards to the scheduler and
  two buckets in the ledger. `genstore.Filter`'s own comment had claimed since
  the column existed that `NormalizeGPU` ran on the way in; it now does.
  Idempotent, and it never rejects a name it has not heard of, so nothing an
  operator can type is refused there.

## Measured or considered, and deliberately not fixed

- **An unnormalised stored ask only gets two of the three card admissions.**
  Rows written before the normalisation hold whatever a browser sent, so
  `cardAdmits`' spelling does not reach them. That is exactly the packing they
  had before `Card` existed, and the alternative is rewriting rows -- the
  thing this schema never does.
- **`ResolveImage` depends on the card through `GPU` *and*
  `Offer.MeasuredCap`.** `ComputeCap()` prefers a capability a provider read
  off the silicon over ember's own table, and only the local provider fills it
  in. A disagreement between `nvidia-smi` and `internal/node/compute.go` would
  make two offers of one GPU name resolve to different images while the compat
  calls them interchangeable. Not guarded, because the compat has no access to
  `MeasuredCap` and the local provider is one machine.
- **A rider's own `max_hourly_usd` is not checked against the rate of the
  rental it joins.** It spends nothing of its own: the rental exists, its cost
  is the opener's, and the period ceilings were projected against the opener's
  whole TTL before the node was created. Refusing the ride would mean renting
  a second card, which is the outcome that *does* cost money. Pre-existing for
  equal asks; the empty-ask admission widens the set it applies to, which is
  why the argument is written down rather than left to be found.
- **`advertise`'s kick is not guarded by a test.** Without it the conservative
  window widens from one offer lookup to one `idlePoll`, which is latency and
  not correctness, and the packing test's 20 s deadline cannot distinguish it
  from the 15 s poll. The deadline was not shortened to manufacture a guard.
- **The added write-lock contention of renewing and reclaiming once per
  supervisor iteration is not measured.** The kick is buffered by one so kicks
  coalesce, which bounds iterations to claims plus polls.
- **Submitted runs (`ember up` over the socket) still never populate the caps
  cache**, so an operator who only uses `up` gets no enqueue-time validation.
  Pre-existing and unchanged.
