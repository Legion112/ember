# What a run records

The schema and its migrations, the event stream as the truth, the columns as a
derived index, and the node memory probe.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **A version check that only ever compares is a version number instead of a
  migration.** `genstore.migrate` refused a file from the future and did
  nothing about a file from the past, while the schema is
  `CREATE TABLE IF NOT EXISTS` -- a no-op against a table that already
  exists. So the first column ever added would have been missing from every
  database written before it: `Open` succeeds, the version check passes, and
  the failure arrives as `table jobs has no column named params` the next time
  an operator queues a request, with their whole history in a file ember can
  no longer write. Found by needing the column, not by reading the code.
  There is a migration list now, applied in one transaction, and a lab-free
  test that builds a version-1 file by hand and reads it with today's ember --
  because what has to be proved is that today's binary reads yesterday's file,
  which checking out the old code cannot show.
- **One JSON column, not twenty-three.** Nothing queries the optional
  parameters: no `WHERE`, no `ORDER BY`, no join. They are payload carried to
  the generation call, which is what `events.body` already is -- and a column
  per field would mean a migration every time upstream adds a knob, to a
  table whose only reader selects a request's rows by id.
- **Emitted is not recorded, and recorded is not queryable.** Every figure a
  performance question is about -- per-job cost, the span it took, the steps
  it sampled, the rental's rate and its billable against elapsed time, the
  model load as measured against as reported -- was already on
  `report.Event` and already persisted, as one opaque JSON blob per event.
  The `jobs` table, the row a job *is*, had **no execution columns at all**,
  so "what did an image cost on this card" meant JSON-parsing every row of
  the table that grows per step per job per rental.
  `grep -rn 'SUM(\|GROUP BY' internal/genstore/` returned nothing. They are
  columns now, written through from the same events, on the precedent
  `requests.node` and `requests.cost` set one line at a time: *"written
  through to the row as well, so `ember ls`'s node and this request can be
  joined without reading the event stream"*.
- **The event stream stays the truth and the columns are a derived index.** A
  disagreement between them is resolved in the stream's favour and is a bug
  in the writer, not a fact about the run. That is what makes the duplication
  acceptable rather than two sources of truth.
- **Nothing derived is stored.** Cost per image, steps per second and the
  sampling share of the bill are methods on `genstore.Bucket`, computed on
  the way out. A stored quotient is a number that can be wrong while the
  terms it came from are right -- the same reason money is kept as integer
  microdollars and formatted late.
- **`queue.Cost` is gone, and it had never been called.** It summed one
  request's card time by scanning that request's whole event stream in Go,
  and for the life of the function nothing but its own test invoked it -- the
  coverage floor was all that kept it. `genstore.Spend` answers the same
  question with a `SUM` over columns. Two cost-summing paths would have been
  the "two sources of truth for one number" mistake `Offer.VCPU` exists to
  avoid, so there is one. It is the third thing found in this repository that
  was written, covered and connected to nothing.
- **A query layer with no reader is the same failure as a feature built end to
  end and never connected.** Columns, a migration and an aggregate that
  nothing calls would have agreed with themselves and proved nothing, which
  is the closed loop `capabilities.model` and the unrecorded preview frame
  both sat in. `ember spend` is the reader, deliberately thin: #27's ledger
  builds on it rather than growing a second way to sum the same money.
- **A rental is counted once, on the request that opened it.** Only the opener
  carries `requests.cost`, because the rental's per-rental costs are paid once
  for everything it served. So the period total is a `SUM` over openers, and
  the job-side figures come from a **subquery keyed on the request** rather
  than a plain join -- joining `requests` to `jobs` and summing `requests.cost`
  multiplies each rental's cost by its job count, which is the kind of wrong
  that looks plausible in a figure nobody has a second copy of to check.
- **The spend period is half-open and the day is UTC.** `--since` includes and
  `--until` excludes, so consecutive periods tile without a request landing in
  both; a day boundary in local time would put the same spend in different
  buckets depending on where the operator was sitting. A period typed
  backwards is refused rather than answered with nothing, because an empty
  report reads as "ember spent nothing" -- the one answer a spend report must
  not give wrongly.
- **A grouping reaches a `GROUP BY`, so the set is closed and checked twice.**
  `GroupBy.Valid` refuses anything else and `groupExpr` switches over the
  constants rather than interpolating the string. It is the one place in
  `genstore` where a caller's value could have become part of a statement.
- **The model is recorded from a response, and that is what makes grouping by
  it possible.** It was not offered for a schema version, because `sd.serve`
  is config rather than request and the only stored copy was a single cached
  `caps` row the next rental overwrites -- so a historical request's model was
  not knowable, and offering the grouping would have been the
  `allowedCudaVersions` mistake again: a value read from the wrong place and
  believed. `requests.model` is that column, written at claim time from
  `capabilities.model.name` -- the same `/capabilities` response `PutCaps`
  caches, and an **object** rather than the string the published field list
  implies. It is written to **every** request on a rental and not only to the
  opener, which is the opposite of how the rental's cost and memory figures
  are attributed: those are quantities, and writing one rental's to each of
  its requests multiplies the bill, while a model is a label every request on
  that node ran against -- recording it on the opener alone would split one
  model's spend between its own name and the dash. Nothing backfills, and here
  there is not even a wrong answer available: no event carries the model, so a
  backfill would have to read the cached caps row and stamp every row in the
  history with whatever is loaded today. A row from before the column is empty
  and both readers print a dash.
- **The empty model means nobody read one, and the comment that said otherwise
  named a case the write cannot reach.** `queue.go` justified writing `''` as
  "a server that reported no model is a fact" -- and that response shape does
  exist, `capabilities_no_model.json` reports `{"name":"","path":"","stem":""}`
  -- but it never gets to the write. `sdserver.WaitModel` retries `empty` and
  ends as a `NotReadyError`, so a server with no weights fails the rental
  before a claim is recorded and `s.Caps()` is only ever read from a server
  that loaded something. What leaves the column empty is an `ember up` whose
  config describes no jobs, or a row from before the column -- which is exactly
  what `groupExpr` and `keyOr` already act on, so the comment contradicted
  both of its readers. This is "'it has no figures' is the fact; 'it ran
  before ember recorded them' was a guess at the cause" one column over, and
  it matters because a reader who believed it had two moves and both are
  refused elsewhere in this file: make the column nullable to tell the two
  states apart, which is a branch nothing can reach, or read a stored `''` as
  evidence that a server answered with no name, when it is evidence that no
  server was asked. A comment that is wrong about why a seal holds is how the
  seal gets removed by somebody being helpful -- the `AssertNoStray` comment
  that claimed the cleanup ran "before AssertNoStray records the failure,
  never instead of it" while the code did the opposite. **There is no test to
  prove this one, and that is stated rather than papered over**: a comment has
  no exit code, and inventing an assertion that the column is empty for a
  jobless run would test a behaviour that was never wrong.
- **What the server reports is the diffusion model, and that bounds what the
  grouping can distinguish.** `resolve_display_model_path` in sd-server's
  `routes_sdcpp.cpp` answers the basename of `--model`, else of
  `--diffusion-model`, else nothing -- so a FLUX.2 configuration naming its
  diffusion model, text encoder and VAE separately records the first and says
  nothing about the other two, and two setups differing only in those read as
  one model. Written down rather than worked around: a name ember composed
  from the serve argv would be a reading of the config, which is the thing
  this column exists not to be. Verified against a live `/capabilities` for
  the single-file case (`{"name":"sd15.gguf","path":"/opt/models/sd15.gguf",
  "stem":"sd15"}`, off `ember:sd` on a laptop) and from upstream's source for
  the split case, because a monolithic checkpoint handed to
  `--diffusion-model` refuses to load -- so **no response has been observed
  for a three-file configuration**, and the limit above is read from the
  server's code rather than from its answer.
- **A total that is missing part of its history says so, every time.** A
  request from before these columns existed contributes its rental cost and no
  per-job figures, so `Bucket.Unrecorded` counts it and `ember spend` prints
  how many it had to leave out -- and names cost-per-image as an upper bound
  when there are any, because the division is over a smaller denominator than
  the money paid for. Found by reading the real output, not by a test. A
  figure wrong in the expensive direction that does not say so is worse than
  one that is missing, which is why `ember ls` exits non-zero on a
  provider it could not ask.
- **Nothing backfills, and that is a decision.** The facts are in the event
  bodies of requests that already ran, so a backfill is possible -- and it
  would have to re-derive the step counts from progress frames whose poll
  interval is unknown for those runs, which is the one number this schema is
  careful not to infer.
- **`ember up` records, and not through the queue.** `ember serve` was the only
  writer to the generation history, so the command every measurement in README
  was taken with printed its cost and kept nothing: `ember history`, `ember
  spend --by gpu|day|provider|model` and every per-job cost, timing and
  node-memory column were blind to it. Enqueueing instead would have been four
  separate wrongs, each of which costs money or changes behaviour: `queue.Run`
  claims whatever is **queued** rather than one named request, so an `up` would
  serve the web UI's backlog and bill a measurement run for it; `Run` begins by
  marking everything it finds running as interrupted, which is right for a
  restarting server and wrong for a second process; `AppPlanner` *narrows* a
  request against the config where `up`'s `applyFlags` *replaces*, so
  `--max-hourly-usd 999` would stop raising the ceiling; and `Enqueue` refuses
  a request with no jobs, which two of `up`'s three shapes are. Writing a
  second recorder was the other horn -- two sources of truth for one set of
  columns, drifting the first time one is added -- so the recorder moved **out**
  of `Queue` into `queue.Recorder` and both callers use it. One rental at a time
  is untouched by any of it: that is structural because a rental lives inside
  one iteration of one drain goroutine, and `up` never enters the loop.

  **`up` DOES enter the loop now, and the four reasons above are why it is a
  separate route rather than why it cannot be a client.** #146 reads as a
  transport change and for `ls`, `history` and `spend` it is; for `up` it is a
  change to what the command means, against a decision written down with
  reasons. So `POST /api/runs` is not `POST /api/requests` with a different
  body: it runs `up`'s own pipeline, `app.NewPlan` then `app.Plan.Do`, inside
  the daemon. Reasons **2** and **4** then stop being problems the moment it is
  a different route -- there is no `Enqueue` to refuse a jobless run, and
  `app.Overrides.Apply` is the function `up` itself uses -- and **1** and **3**
  do not arise because nothing is claimed. What had to be designed rather than
  reused is the fifth, which the issue half-named: a run started beside the
  drain loop is a second rental. `Queue.Submit` is the answer and the entry in
  the Threat model above has it.
  **`genstore.Adopt` is still the write, and still for the reason beside it**:
  `Enqueue` writes `StatusQueued` whatever the caller said, and a queued row is
  exactly what `ClaimNext` takes -- so an `ember serve` in another terminal, or
  this one's own loop, would claim it, rent a SECOND card and generate the batch
  again from the record of the run still happening. The adopt and the recorder
  are one function for both callers now, `queue.AdoptRun`, because two copies
  of the row would drift the first time one gained a column.
  **And `report.Writer` has a production caller at last.** It was in
  `TestNoProductionDeadCode`'s baseline with the reason "node byte streams
  reach the operator another way", which was true of both callers it had: `up`
  prints them to the terminal it is attached to, and the queue to `ember
  serve`'s. A SUBMITTED run has an operator at neither -- they are on the far
  side of a socket reading the event stream -- so the adapter whose own doc
  comment says it exists "so that the packages which only print need no change
  to reach a UI" is the mechanism. Wired up rather than deleted, which is
  `sdserver.Client.Cancel`'s precedent and not `queue.Cost`'s: there is no
  second way for a node's bytes to reach a client that is not this process. The
  waiver is gone, which the guard insisted on -- an entry it stops reporting
  fails. What it costs is written down rather than discovered: a chatty
  workload's output is rows in `events`, one per line, and `ember history rm`
  is what removes them.
  **`report.Text` is silent about three kinds and that silence does not hold
  here.** `KindNodeReady`, `KindRunCost` and `KindDestroyFailed` render as
  nothing, and the comment says why: "the CLI already reports all three through
  its slog handler". The slog handler for a submitted run is the DAEMON's, so
  on this path the three reach a terminal nobody is watching for them and the
  operator who typed `ember up` gets nothing at all between "submitted" and the
  workload's first byte -- which on a real rental is minutes. So the client
  prints those three itself, in `clientSays`, and that is the FIRST spelling on
  this channel rather than a second one: the deadline is the whole of what stops
  the bill after a detach, the cost is the figure the run was for, and
  destroy_failed is the one event an operator must not miss. Everything else
  goes through `report.Text` unchanged, which is what keeps the rendered run
  byte-for-byte what `ember up` prints -- asserted by rendering the same events
  both ways and comparing, and by walking `Kinds()` to require silence on every
  other one. Found by a lab: the first run of the detach assertion reported "no
  node has been reported yet" because nothing had told the client there was
  one.
- **A queued row for a run already in flight is two GPUs.** `Enqueue` writes
  `StatusQueued` whatever the caller said -- it is how a request *enters* a
  queue -- and a queued row is exactly what `ClaimNext` takes. An `ember serve`
  in another terminal, which is the ordinary setup for somebody who has both,
  would claim the `up` row, rent a **second** card and generate the batch again
  from the record of the run still happening. So `genstore.Adopt` is a separate
  method that writes `StatusRunning`, which no claim will take, rather than a
  flag on `Enqueue`.
- **The row is written before the create call, and keyed by a fresh random
  id.** The journal's rule -- the window between asking a provider for a
  machine and reading the answer is where a rental nothing recorded is born --
  so a run whose create call failed, and a run whose node billed without ever
  becoming ready, both have a row. The run id would have been the tidier key
  and is not available: `provision.WithNode` mints it *after* that point, so
  keying on it would have meant writing the row from the first event, which
  drops exactly those two runs. `requests.run_id` therefore stays empty for an
  `up` row and the node's own handle, written through from `node_ready`, is the
  join to `ember ls`; neither reader prints the run id.
- **An `ember up` whose config describes no jobs still has a row.** It
  records the request row and the full event stream and no `jobs` rows,
  because the figures the jobs table exists for -- images, passes, steps per
  second -- are not things that run produced. The honest consequence is
  printed rather than hidden: `ember history` shows such a run with no jobs, a
  dash for images and card time, and a footnote saying which runs those are.
- **`unrecorded` required a job, and that condition could not fire.**
  `Validate` refuses a queued request with no jobs, so `jobs > 0` was true of
  every row any reachable state could produce -- the dead half of a seal, which
  `History` had already removed one of. `Adopt` makes it reachable and the
  answer it then gives is the right one: a rental that paid for a node and
  divided it into no images is precisely what the flag means, so both readers
  print a dash rather than a zero and say how many rows they left out. A column
  of zeros in a money ledger reads as free.
- **The model travels on the event, and the decision not to print it moved to
  the renderer.** `model_resident` was emitted only for a batch of **more than
  one**, because "paid once rather than 1 times" is not a saving -- a
  presentation choice made at the emitter, which meant a single-job run
  recorded no load span and no model name at all, in the two columns added to
  explain exactly that. `ember up` with one job is how most of README's
  measurements were taken. The event is now always emitted and carries
  `capabilities.model.name`; `report.Text` is silent for `Total <= 1`, so the
  output is byte-for-byte what it was. The queue still writes the model from
  `s.Caps()` as well, because a rider never sees the rental's own events: one
  authority, two carriers, and each is the only one available on its path.
- **Steps are summed from each pass's declared total, never counted from the
  frames.** ember deduplicates progress on the step, so at a poll slower than
  the card samples it *observes* fewer frames than there were steps -- a count
  of frames would be a measurement of the poll interval. A pass restarts for
  each batch item, each high/low-noise stage and each highres pass, so
  `jobs.steps` is one pass and an it/s built from it understates every batched
  job; `passes` and `steps_done` are two columns because one would lie. The
  pass count is observed, so a pass shorter than one poll could in principle
  be missed, and that limit is written down rather than hidden.
- **`JobRun` is separate from `Job`, and that is structural.** `Job` is the
  wire type a browser posts, so every field on it is something a request may
  set -- putting `Cost` there would make the figure an operator is billed
  against settable by the thing being billed, which is the refusal that keeps
  `Extra` off `Job`. They share the table because they are the ask and the
  outcome of one job; the write path can touch only the outcome columns, as
  `Update` cannot touch `Jobs`.
- **The fake aggregates in Go and the conformance table checks the two
  agree.** Normally a second implementation of the same arithmetic is the
  thing to avoid; here the SQL is the half most likely to be subtly wrong, and
  a fake that delegated to it could not disagree. It caught a real divergence
  on the first run, in the unusual direction: the fake returned `ErrNotFound`
  from `JobRuns` for an absent request where the real store returns no rows
  and no error, as `Images` and `Events` do. A fake *stricter* than the thing
  it stands in for is the usual danger inverted and grows callers a branch
  for an error nothing returns.
- **The generation history was write-only from the command line, and a
  database nothing reads is a measurement nobody takes.**
  `grep -rln genstore internal/cli/ cmd/` named one file -- `cmd_serve.go`,
  which only *starts* the queue. So every figure in the store was reachable
  only by running a server that can rent a card and clicking through a
  browser, or by opening SQLite by hand, which is how the sequence-number bug
  was found and is a poor way to answer a recurring question. `ember spend`
  made the money queryable in aggregate; `ember history` is the row-by-row
  ledger, which is what an operator reads the moment the aggregate surprises
  them. Both go through one roll-up, so neither can come to disagree with the
  other about what a run produced, and a conformance test sums the ledger and
  compares it against the aggregate in both stores.
- **A ledger row is not a `Request` with counts beside it, and the reason is
  the wire.** `Request` carries `Jobs []Job` -- the prompts and the geometry
  -- and reading those for a listing is a query per row, which is the N+1 the
  ledger exists to replace. So the embedded version arrived with `Jobs` nil,
  and `"jobs": null` beside `"jobs": 6` is a shape where the honest answer
  ("not asked for") is spelled exactly like a request with no jobs at all --
  which `Validate` refuses, so the listing would have been shipping a request
  that cannot exist. `genstore.Entry` is flat instead, and the duplication is
  bounded by what the table prints: a field added to `Request` that nothing
  shows does not belong there. The nil-slice-is-not-an-empty-list rule, met a
  third time, upstream of the wire rather than on it.
- **A rider's rental cost is zero by design, and a money column of zeros
  reads as free.** The rental is charged to the request that opened it and to
  no other, which is already written down above as the only answer that
  neither understates nor multiplies -- and it means every request that joined
  a rental already up sits in the ledger with real card time and no rental
  cost. A reader who does not know that sums the column and gets a bill
  several times too small. `Entry.Rode` says so per row, and it is decided by
  asking whether *another* request on the same rental carries the cost rather
  than by the zero itself: the local provider's rentals are free, so cost
  alone cannot tell "somebody else paid" from "nobody did". Billable is tested
  beside cost for exactly that case.
- **The same column in the browser read `$0` for a whole generation, because
  the row was gated on one number and displayed another.** The watcher's money
  row appeared when `Progress::spent` -- the card time the queue emits per
  job -- became non-zero, was captioned "card time", and rendered
  `requests.cost`, which is the rental. Those are different quantities written
  at different times to different requests: `run_cost` arrives from
  `provision.WithNode`'s **deferred** teardown and `Queue.lifecycle` routes it
  to the opener's sink alone. So the row read `$0` from the first completed job
  until the node was destroyed and `$0` for ever on a rider -- the entry above,
  reached in a second language, where `Entry.Rode`'s answer had not been
  carried across. `Progress::rental` is that answer for one request, and it
  asks **who was told the rental's facts** rather than reading the zero: a
  `node_ready` in this request's stream is the same routing decision that
  decides which request is charged, so there is one source of truth and not a
  second guess at it. Five states, and the two the old row could not tell
  apart are the two with no figure at all.
  **It was unreachable in every lab, which is why nothing caught it.** The
  local provider's price is zero, so `spent` is zero on every rental a lab
  here can start and the defective row never rendered -- a browser assertion
  about the figure would have passed against the bug, so the lab waits for the
  *sentence* and that is the half that goes red.
  **`Progress::rental_cost` is gone, and it was never the fix.** It held the
  same integer as `requests.cost`, from the same event, arriving at the same
  moment, and zero for ever on a rider for the same reason -- two carriers of
  one figure, which is `queue.Cost`'s mistake, and the one this page may print
  is the other one, because `Money` crosses the wire twice so that the browser
  is never a second formatter of money. What the event adds is that the figure
  **exists**, which is what tells a recorded `$0` from a rental nobody has
  priced, so it is a `charged bool` now. There is no dollar card-time figure
  on the page and that is written down rather than left to be noticed:
  `JobRun.Cost` is kept off the posted `Job` on purpose, so nothing on the
  browser's wire carries one, and `ember history` is the column.
- **"It has no figures" is the fact; "it ran before ember recorded them" was a
  guess at the cause.** Two different things contribute a rental cost and no
  per-job figures -- a request from before schema version 3, and one that
  failed or was cancelled before a job completed -- and the arithmetic warning
  is warranted for both, because both divide a rental into no images. Naming
  only the first blamed the schema for a run that simply broke. Found by
  printing `ember history` over a failed request, which is the third time in
  this repository that reading real output found a defect no test had an
  opinion about.
- **"Finished" is the precondition, and leaving it out made the warning fire
  on requests that had not run.** A queued request contributes no cost and no
  images for the same arithmetic reason as a pre-v3 one, so `Bucket.Unrecorded`
  counted it and `ember spend` told the operator a request that had not
  happened was missing its history. Nothing is missing from a request that has
  not happened yet. The test is `ended_unix` rather than a status list,
  because a status list in SQL would be a second spelling of
  `Status.Terminal()` -- and every path that ends a request writes the end:
  the queue on done and failed, the API on cancel, `Interrupt` on a restart.
  The fixtures had to be corrected too, because they wrote "done" rows with no
  end, which is not a shape the queue can produce.
- **The ledger cannot spend, and that is structural rather than promised.**
  No provider is constructed anywhere on the path from `ember history` to the
  store, and the only statement it runs is a `SELECT` -- so there is nothing
  for a refusal to refuse. The assertion is that three listings leave the
  queued request still claimable, because a reader that had claimed it would
  leave the queue drained and a request "running" against a rental nothing
  holds.
- **"All eight nodes reported 620-708 GiB free RAM" was the wrong number, and
  it is withdrawn as evidence.** `/proc/meminfo` inside a container reports the
  HOST's memory. Measured here with `docker run --memory 512m`: a container
  bounded at **512 MiB** reports **125.7 GiB** of `MemTotal`, a factor of 252.
  So the figure that looked like it ruled memory out of the unexplained 81
  seconds per image was reading a number that cannot answer the question --
  what bounds the page cache is the container's own `memory.max`, and nothing
  had ever read it. The guess is unharmed and is now testable on the next
  rental rather than on the next rental somebody remembers to type a command
  during. What is still unknown is what RunPod sets: these captures are
  Docker's defaults on one host, and `internal/nodemem/testdata/README.md` says
  so rather than letting them read as an answer.
- **The probe is ember's code and not the operator's `gpu_probe` string.**
  #30 asked for the existing node-side probe to be extended, and the existing
  one is a shell command in `ember.yaml` -- so extending it would have meant
  every operator editing their config, and the values would have been
  *printed* rather than recorded. A figure printed once into a terminal is not
  evidence, which is the whole complaint the issue opens with.
- **It reads and cannot fail a run.** Four file reads and a `stat` of the
  weights, with every failure logged at warn and swallowed: a node that will
  not answer them is a node whose generation should still happen. This is
  deliberately NOT the shape of `gpucheck`, which destroys a node over a dead
  card -- that one guards money already committed, and this one is a
  diagnostic nothing yet acts on. A probe that could refuse would be a
  diagnostic with the authority of a safety check.
- **No verdict, and that is the issue's own instruction.** What to do about a
  node that cannot hold the weights -- a warning naming the expected re-read
  cost per step, or a corrected note if the guess turns out wrong -- is a
  decision to take once there are measurements. `ember history --memory`
  prints bytes and one subtraction; a column saying "this will be slow" would
  be that decision taken from a guess, which is the `allowedCudaVersions`
  mistake with better manners.
- **Zero is "no limit" and a dash is "nobody read it", and conflating them
  makes a slow node look fine.** cgroup v2 writes the literal `max` and v1 a
  sentinel near the top of int64, so both come through as zero -- and
  `mem_cgroup` is what says whether anything was read at all.
  `node.Memory.Ceiling` then falls back to the host's total rather than
  reading zero as a container that cannot allocate anything, which is the
  opposite claim. Found by a test: cutting the probe's output before trimming
  it meant a file that was readable and EMPTY became indistinguishable from
  one that was not there.
- **That rule had two implementations, its tests were split across them, and
  the one this file used to name was the one nothing called.** It said
  `Limit()`, which was `nodemem.Report`'s: four lines whose only caller was
  the dead `Headroom` beside it, on the far side of a SQLite round trip from
  `genstore.Entry.MemCeiling`/`MemHeadroom` -- which is what `ember history
  --memory` actually prints. Both pairs were written in the same commit and
  they agreed for the life of the feature because one of them never ran, which
  is `dataCenter` against `dataCenterId` reached through a document: somebody
  editing the function named here would have changed nothing an operator sees.
  What makes it more than tidiness is **measured** rather than argued --
  deleting `|| e.AssetBytes <= 0` from the live method leaves `go test ./...`
  entirely green, because the only assertion anywhere that a ceiling with no
  asset figure answers *nothing* belonged to the dead copy. Half of the rule
  an operator reads was covered by a test of code that never ran, and that
  half is not hypothetical: a baked image and a local container have no asset
  set, so a headroom printed there would be the whole ceiling, as though the
  weights took no room. So there is one `node.Memory`, in the one package both
  `nodemem` and `genstore` already import -- `genstore` importing `nodemem`
  instead would put `sshx`, `workload` and `x/crypto/ssh` in the dependency
  graph of a SQLite store, which is the `internal/node` seal's own argument
  pointed the other way. Its fields are **named** rather than two positional
  `int64`s, because `Ceiling(total, limit)` compiles and is this rule exactly
  inverted, in the direction that makes a bounded node look unbounded; the
  conformance table runs every quadrant through both stores. `Entry.MemCeiling`
  went with the duplication, and `TestNoProductionDeadCode` is what said so --
  the moment the subtraction became a delegation the ceiling had no production
  reader at all, because the ledger prints the two raw columns and the headroom
  and never the step between them. The waiver that used to sit in that guard's
  list justified the dead `Headroom` by pointing at `Limit`, which was dead
  too; an exclusion's reason may not name another symbol as evidence that it is
  live, because a call from a production file counts whether or not the
  function making it is reachable.
- **The v1 paths are named rather than derived from `/proc/self/cgroup`.** A
  guess at where a hierarchy is mounted reports "no limit" on the one kernel it
  is wrong about, and "no limit" is the answer that makes a node look fine.
  Same direction as `Caps.PublicSSH` defaulting to the stricter check.
- **The asset paths are arguments to `sh -c`, never script text.** A remote
  command is argv and every word is quoted, so a path interpolated into the
  script body would be shell text assembled from a config file -- the
  injection `sshx.Quote` exists to prevent, reached from inside a script
  instead of from a command line. As positional parameters they cannot be
  anything but paths. The asset figure is a `stat` of the weights THIS run
  names rather than a `du` of the mount, because a store holds other runs'
  models and the question is whether this one fits.
- **`Probe`'s first version had a branch nothing could reach**, and coverage
  found it: it tested `err != nil` and then `res.ExitCode != 0`, but
  `workload.Run` returns an error for a non-zero exit as well as for a
  transport failure -- so the diagnostic carrying the node's own complaint
  could never print. The two are told apart by the code, as `gpucheck.Wait`
  does it.
- **`human` is signed now, because one caller's figure is legitimately
  negative.** A node that could not hold the weights has negative headroom,
  and that is the interesting row -- it rendered as `-25769803776 B`,
  unreadable in exactly the line an operator is looking for. Found by printing
  it. The magnitude is taken in `uint64` so `math.MinInt64` cannot recurse
  through a `"-" + human(-n)` that would not terminate.
- **The headroom is a column and not a line of prose per row.** The first
  version printed one sentence per request under the table, which over a
  default page of two hundred is two hundred lines after it. Found by printing
  it, as the `ember store ls` plural was.
- **What #30 asks for and this change cannot do.** Its last done-when is
  "`AGENTS.md:530-553` is amended with what the next rental actually showed".
  No rental has happened, so what is amended above is what a *container*
  showed -- which settles the mechanism and withdraws the misleading figure,
  and does not settle what RunPod sets. That line stays open until somebody
  rents a card, and it is now one command rather than a procedure.
