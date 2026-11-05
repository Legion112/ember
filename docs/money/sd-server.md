# The sd-server protocol

Wire types captured from responses rather than guessed, previews, progress,
and cancelling a job without destroying the rental.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

- **A long-running remote process must not be reaped by a bare `wait()` in a
  defer.** `ssh.Session.Wait` does not return while the remote process is
  alive: cancelling the context closes the session, sd-server keeps running,
  and no exit status ever arrives. The first version called `wait()` straight
  from `runServed`'s defer and wedged for twenty-three minutes after SIGTERM
  -- never printing the error it was in the middle of returning, so the run
  looked like a hang with no diagnosis. Found with SIGQUIT and a goroutine
  dump, which is the only thing that could have found it. The exit is now
  collected by a goroutine and the defer waits on a channel with a five-second
  cap.
  **The cap had no test for the life of the fix, and the one fake that could
  have reached it returns on `ctx.Done()`.** Measured: deleting the five-second
  cap outright leaves `go test -race ./internal/app/ ./internal/queue/` at exit
  0, so the sentence above asserted a property with no exit code behind it --
  which is what the trust-on-first-use claim in this file was corrected for.
  `fakeServerRunner`'s `linger` is `<-ctx.Done()`, and that is the **tidier**
  behaviour: it is right for every test that uses it, because ember stops the
  server by cancelling. The bug the cap exists for is a `wait` that does not
  return even then -- "cancelling the context closes the session, sd-server
  keeps running, and no exit status ever arrives" -- so every test that drove
  this defer handed it an exit that was always going to arrive, and the cap was
  never the thing that released it. The **sixth** instance of a fake kinder
  than the thing it stands in for, and #128 called it the fifth: the five this
  file already records are `runpodfake`'s 200 on a deleted pod, the preview
  fake that never repeated a frame, **the queue fake that stamped `Seq`
  itself**, the cancel fake with no error object and `RecordingProvider`
  echoing the injected host key -- the issue listed four and left out the
  `Seq` one, which is the entry that cost a whole generation's event stream.
  Counted here rather than carried forward, because a running total in prose
  is the thing this file has been wrong about before. A **second**
  fake beside the first rather than a flag on it, and no widening of
  `sshx.Runner`: the seam already hands back a `wait func() error`, which is
  all this needs.
  **A WEDGE PRINTS NO `--- FAIL`, which is why the test carries its own
  deadline rather than being left to the package timeout.** CI greps the unit
  log for `--- SKIP` and reads `go test`'s status, and a hang is invisible to
  both until the binary's own `-timeout` prints a goroutine dump that names
  nothing an operator or a reviewer can act on. Same rule as the browser labs:
  race the thing you are waiting for against the thing that means it will never
  happen. 7.0s on the tree as it stands -- two seconds for the port wait plus
  the cap -- and without the cap it does not return at all. Checked by deleting
  the cap, by widening it past the assertion's bound, and -- because belt and
  braces reads exactly like a dead gate -- by deleting the cap with the new
  test skipped, which is green: nothing else in the tree reports it.
  **`ws.go`'s ten-second write bound is the same shape, was equally untested,
  and now has the same test.** It is the lesser case: one watcher goroutine
  against a peer that has stopped reading, rather than a process that will not
  exit. Replacing its `context.WithTimeout` with `context.WithCancel` was
  green too. The parent must have **no deadline of its own**, which is why that
  test spends the whole ten seconds -- `WithCancel` inherits a deadline, so a
  bounded parent would pass against the defect as happily as against the fix,
  and the real parent is the websocket handler's request context, which has
  none. No new seam for it either: `coder/websocket` allows one message writer
  at a time and acquires it through a context-aware channel, so an unclosed
  `Writer` blocks the next `Write` exactly as a filled socket buffer does.
  Narrowing `send`'s `*websocket.Conn` to an interface so a fake could block
  would be a production type shaped by a test's convenience, which is the line
  `sshx.Runner` is not widened across either.
- **A server that died has to be noticed at once, not at `ready_timeout`.**
  Handed `--diffusion-model` for a monolithic checkpoint, sd-server printed
  `get sd version from file failed`, exited after about a second, and ember
  polled the port for the full ten-minute default before reporting that it
  "never accepted a connection". On an A100 at $1.79/hour that is thirty cents
  for a program that was already gone, and the explanation had scrolled past
  ten minutes earlier. `orExited` now races every wait against the process's
  exit. It also has to prefer the operation's own answer when the context is
  already cancelled, because ember stops the server by cancelling -- otherwise
  Ctrl-C reports a server that crashed.
- **`--diffusion-model` is for a standalone DiT, `--model` for a whole
  checkpoint.** FLUX.2 names its diffusion model, text encoder and VAE
  separately, so every example in this repository used `--diffusion-model` --
  and sd15 in one file needs `--model`. The flag that works for one silently
  cannot load the other.
- **sd-server was already being compiled and thrown away.**
  `examples/CMakeLists.txt` at the pinned revision is two unconditional lines,
  `add_subdirectory(cli)` and `add_subdirectory(server)`, so `cmake --build`
  has always produced both and the image copied one out. No revision bump and
  no extra build time. The copy is guarded, because `find -exec cp` succeeds
  when it matches nothing -- and the guard is braced, or a cmake failure falls
  into it and blames the copy for a compile error at the end of a
  forty-minute build.
- **A completed result is erased 600 seconds after it completed, so one job is
  in flight at a time.** `async_jobs.h` sets `completed_ttl_seconds = 600`;
  `async_jobs.cpp` measures it from `completed_at` and purges from the worker,
  when it wakes before popping the next job and after each one finishes.
  `api.md` lists `410 Gone` for a job and never says why. Queueing ten jobs up
  front would leave eight finished images, each already paid for, one dropped
  connection away from deletion. Submitting one at a time costs nothing --
  the generation context lock serializes the device anyway -- and makes the
  TTL unreachable, the 64-job pending cap irrelevant and `queue_position`
  moot. Read out of the C++ before any client code existed, which is the
  calibration-first habit the RunPod volume API set.
- **One API, two JSON types for one field name.** Model management answers
  `{"error":"...","code":"..."}`, a string beside a code; a failed job answers
  `{"error":{"code":...,"message":...}}`, an object. A Go struct with
  `Error string` fails to decode the second and one with an object fails the
  first, so whichever was written first works until the day the other endpoint
  refuses something -- and then what ember prints is a JSON unmarshalling
  complaint instead of the reason.
- **ember owns the listen address.** `sdserver.ServeCommand` appends
  `--listen-ip 127.0.0.1` and refuses a serve command that names either listen
  flag. A config passing `0.0.0.0` would publish an unauthenticated image
  generator, with arbitrary file paths in its request body, on a rented
  machine's public interface -- the exposure this threat model is written
  against. Same rule and same reason as `tunnel.RemoteLoopback`, and nothing
  binds locally either: ember dials the node's own loopback through
  `direct-tcpip`, so there is no listener outside `internal/loopback`.
- **A job submission is never retried.** A transport failure on
  `POST /img_gen` is ambiguous: the job may exist with an id ember never
  learned. Retrying generates and bills a second image, and the second one is
  invisible because ember is polling the first id. `httpx` has no retry loop
  and must not gain one; the comment sits where somebody would add it.
- **Previews cost GPU time, not just bandwidth, and ember's default is not
  upstream's.** `preview_interval` defaults to 1 upstream -- a frame every
  step -- and the `tae` and `vae` modes run a decode per frame. ember defaults
  to `proj` every 5. A negative interval is refused rather than clamped,
  because upstream clamps a non-positive value to 1: somebody writing `-1` to
  mean "off" would buy the most expensive setting there is.
- **Progress is a pass and a step within it, never a percentage.** sd-server's
  `step`/`total_steps` describe the current sampling pass, and passes restart
  for each batch item, each high/low-noise stage and each highres pass. A bar
  built from them runs to the end and starts again, which reads as a hang.
  Every poll also returns the current frame whether or not it is new, so the
  poll interval is a bandwidth knob and ember deduplicates on the step.
- **A test whose fake never repeats cannot prove a deduplication.** The first
  version of the preview fake advanced the step on every poll, so each line
  appeared once whether or not anything deduplicated -- the assertion passed
  against the bug. A real server returns the same frame to every poll between
  intervals, so the fake does now. Same shape as a lab whose cleanup runs
  before its assertion.
- **A verification harness that cannot tell a build failure from a pass is not
  a verification harness.** Three of the reinstatements in this change
  reported "nothing failed" when what had actually happened was an anchor that
  did not match or a program that did not compile. Grep for the build error as
  well as for `--- FAIL`, and assert that the edit applied before trusting
  what the suite says about it. A third way has turned up since, and it is the
  subtlest because the harness is working correctly throughout: a patch
  inserted *beside* the live code rather than replacing it -- an
  `if false { … }` next to the real assignment -- so the suite was green
  because the defect was never present at all. The edit applied, the program
  compiled, nothing failed, and every check above passed. It was caught only
  by asking *why* a case had gone green rather than accepting the green. The
  cheap answer to all three is a **negative control**: a no-op patch the
  harness must report as *not caught*, beside the positive control that the
  suite is green before anything is patched. A positive control alone cannot
  distinguish "the guard fired" from "the patch did nothing", which is the one
  thing the whole exercise turns on -- the passes that added a negative
  control caught their own broken patches at once, and the ones that did not
  reported them as clean runs.

  **A fourth shape, and it is the inverse of the "beside the live code" one: a
  patch that ADDS a line cannot be checked by asserting the old text is gone.**
  The harness for the socket run route asserts three things per patch -- the
  marker it introduces is present, the text it replaced is absent, and the tree
  still compiles -- and the second of those is exactly wrong for an additive
  defect, which is what "the privileged route is ALSO registered on the port's
  mux" is. It stopped the pass on its first case, correctly from its own point
  of view, so an additive patch now says so rather than having the check
  quietly relaxed for everything. Three of the twelve patches also reported
  BUILD-ERROR on their first attempt -- a missing import each time -- which is
  the harness refusing to read a non-compiling tree as a failing suite rather
  than three false REDs.
  **And a fifth, in the ROLLBACK rather than in the patch, which is the one
  that makes every later case worthless.** One defect needed two edits to the
  same file, and the harness kept one backup per path and copied on every edit
  -- so the second edit backed up the half-patched file over the pristine one,
  and the restore left a tree that did not compile. Every case after it, and
  the closing control, reported BUILD-ERROR. The closing control is what
  caught it: a pass whose last line is not the same green as its first line has
  not restored the tree, so **a reinstatement pass needs a control at both
  ends** and not only at the start. The backup is taken once per path now.
  **And the OPENING control caught a flaky test of this change's own**, which
  is the other thing a control at both ends buys.
  `TestDirectRunner_StartAdoptsTheRowAndHandsTheRunToTheLoop` ran a drain loop
  and then read the row's status, so a rental that failed fast against the
  recording Docker engine wrote a terminal status before the assertion looked
  -- intermittent, and invisible to any single run of the suite. It runs with
  NO loop now, where Submit parks on the rendezvous and the row stays exactly
  as Adopt wrote it; the parking and the abandoned-submission end are asserted
  with it, and the test went from seconds to seven milliseconds. A flaky
  assertion is worse than none, because it teaches people to re-run rather
  than to read -- the `--- SKIP` problem's other half, which this repository
  has already paid for once in the labs.
  **And one defect was caught by the lab alone, which is worth recording
  because it is the most expensive one in that change.** A submitted run that
  held the CLIENT's context instead of the drain loop's destroys the node when
  the operator presses Ctrl-C -- the opposite of the detach the feature is for
  -- and `go test ./internal/...` is entirely green over it: the unit tests
  drive `Submit` and `Direct.Run` directly, where the context is whatever the
  caller passed, so the defect lives in one line of `DirectRunner.Start` that
  only a real client interrupting a real rental can observe. The deterministic
  version of it is the lab's twelve-second `sleep`. This is PR 151's own
  finding one slice later: the most expensive defect available in a change is
  not reliably the one the unit suite can see.

  **A SEVENTH AND AN EIGHTH, both from #204's pass, and the seventh is the
  Makefile defect this repository already records -- committed by the harness
  written to find defects.** Every command the harness ran ended in a pipe to
  `tail` or `grep`, and a shell pipeline reports the **last** command's status:
  so `tail` exited 0 over every red suite and **24 of 26 cases were reported
  GREEN with their own `--- FAIL` lines printed in the detail column beside
  them**. That is exactly `pipefail` -- four lab targets exiting 0 over a red
  suite, one of them the gate on a `docker push` -- in a Python harness instead
  of a Makefile. What caught it is the rule that has now caught four of these:
  the detail contradicted the verdict, so somebody asked why a case had gone
  the way it did. **The lesson is not "set pipefail"**; it is that a harness
  which prints evidence beside its verdict can be caught out by its own
  output, and one that prints only a verdict cannot.

  **The eighth is a RESTORE THAT REPAIRS THE SOURCE AND NOT THE ARTEFACT, and
  it produced a false RED rather than a false green.** The harness restored
  each file with `shutil.copy2`, which **preserves the backup's mtime** -- so
  the restored source was OLDER than the build artefacts cargo had just
  produced from the patched copy, cargo saw it as up to date, did not rebuild,
  and ran the **previous case's** binary. Measured: the closing control failed
  on a tree `git status` reported clean and `diff` reported byte-identical to
  the backup, because the defect was in `target/`, not in `web/src/`. It is the
  shape #138's pass already records for the wasm -- "a restore that repairs the
  SOURCE and not the ARTEFACT, and it goes both ways" -- met through a
  timestamp instead of a missing rebuild. **Go was never affected**, because
  its build cache is keyed on content rather than on mtime, which is why only
  the two cargo cases showed it and why a harness that drove Go alone would
  have shipped with the bug intact. `shutil.copy` plus `os.utime(path, None)`.
- **Nothing creates the run's output directory on a served path.** The
  fetching paths get it from `transfer.Fetch`'s own `MkdirAll`, and a served
  run never calls Fetch because the images arrive in the HTTP response instead
  of a tar. Every served run would have failed at the first image with "no
  such file or directory", after the generation was paid for. Found by writing
  the test, not by running it on a card.
- **The wire types were a guess until a server corrected them, and it took one
  run.** `capabilities.model` is an object, `{"name","path","stem"}`, not the
  string api.md's field list implies; `models/status` carries no `revision`.
  The doc-derived fixture said `"model": "flux2-dev"` and decoded happily, so
  the mistake surfaced as `cannot unmarshal object into Go struct field` after
  a node was up. Everything else held -- `samplers`, `schedulers`,
  `preview_modes`, `limits` and the whole job lifecycle. `capabilities.json`
  and `model_status_ready.json` are captured now, and
  `internal/sdserver/testdata/README.md` says which still are not.
- **The error envelope was typed, fixtured, covered and read by nothing.**
  sd-server spells `error` two ways in one API and `decodeAPIError` has read
  both since the package was written -- but `wireErrorEnvelope`, the string
  form, was decoded only by the test that unmarshalled its fixture into it.
  Every non-2xx reached the operator as httpx's raw body instead, so a
  generation asked for before the weights were in printed
  `status 503 Service Unavailable: {"code":"model_not_loaded","error":"No
  model is loaded"}` -- the reason, as JSON, inside a sentence about a status
  code. The fourth thing found in this repository written, covered and
  connected to nothing, after `queue.Cost`, the unrecorded preview frame and
  the preview column; and the unit test could not see it, because a test that
  decodes a fixture by hand proves the *shape* is readable and nothing about
  whether anything reads it. `sdserver.refusal` is on all five calls now, and
  a lab asserts it against a live 503.
- **A failed job names a generic code and the reason is in the log.** A real
  failure says `generation_failed` / `generate_image returned no results`
  while the server's own stderr says `VAE tile size must be at least 32 image
  pixels on this axis`. The guessed fixture had invented both halves --
  `invalid_request` is in no part of this binary, and a width it dislikes is
  rounded rather than refused -- so a reader would have expected the API to
  explain a failure it only reports. The mirror of the cancel capture, where
  the log said "sampling failed" about a job the API called `cancelled`:
  neither source is sufficient, and which one answers which question is not
  guessable from a field list.
- **A failure provoked through a hole proves nothing, so the input is one
  every check accepts.** `Validate` refuses a sampler the server does not
  list and a width past `limits.max_width`, which is why a failed job was
  recorded as uncapturable -- reaching around it needs an escape hatch, and
  an escape hatch for a test is an escape hatch. What works instead is vae
  tiling with a one-pixel tile: `ValidateStatic` refuses a negative tile and
  nothing else, because no response publishes a tile bound. It stays
  representable deliberately -- the minimum of 32 is in a log and in no
  response, so refusing below it would be the `allowedCudaVersions` mistake
  with a log in place of the schema, and it would trade the only honest route
  to a real failed job for an invented limit.
- **`state: "error"` is on the far side of a port that never opens.** ember
  hands the weights to sd-server as argv, and a `--model` that is not a model
  exits 1 *before listening*, with `get sd version from file failed` on
  stderr -- which is the premise `orExited` was written on and which nothing
  had checked. So `model_status_error.json` is captured by driving
  `models/load`, a route ember never calls, on a server started with no
  weights. A fixture's capture route is worth writing down when it is not the
  route the program takes: what it proves is the decode, not the reachability.
- **The deferral above was answered by the image it had ruled out.** It was
  recorded as "no GPU-less runner can start sd-server", which is true of the
  CUDA build and was never true of the CPU one: the upstream release zip has
  always carried `sd-server` beside `sd-cli`, and `workload-sd.Dockerfile`
  unzipped both and then put one of them on PATH. One symlink later there is
  a lab that starts a real server, generates, and asserts on what comes back
  -- on a laptop, with no GPU, in twenty-one seconds. The lesson is not about
  previews: a deferral justified by a constraint should name which artifact
  the constraint applies to, because this one quietly generalised from the
  CUDA image to both and bought a year of "we cannot test that".
- **Previews were the justification for the whole package and the least
  verified thing in it.** ember knows what a sampler is because progress
  reporting is impossible for a program that will not read its own payload.
  Yet `job_generating.json` was written from api.md, the fake replayed that
  guess, the unit tests confirmed ember agreed with itself, no lab touched the
  served path, and the live FLUX.2 measurement ran `preview: none` in both
  arms -- so no preview frame had ever been observed by anything. The same
  closed loop `capabilities.model` sat in. `make test-preview-lab` now watches
  a real six-step generation: seventeen frames, every step from 1/6 to 6/6,
  each one a decodable PNG.
- **A `proj` preview frame is the latent, not the image.** A 64x64 generation
  produces 8x8 PNGs of about 250 bytes. Nothing in api.md says so and
  "preview frame" reads like a thumbnail of the output, which is what anyone
  sizing a display for one would assume.
- **The wire guesses held, and that is also a result.** Every field ember
  reads was already right -- `pass`, `step`, `total_steps`, `b64_json`,
  `result.images[].b64_json`. What the doc-derived fixtures missed was the
  surroundings: `cancel_requested`, `result.output_format`, a whole
  `generation_info` object echoing the parameters, and timestamps in
  milliseconds where the guesses used seconds. Recorded because the opposite
  outcome is the one that gets written down, and a convention that only ever
  reports its own vindication stops being read.
- **Polling faster than the server samples is why the deduplication exists,
  and now there is a measurement.** Eleven of sixteen consecutive frame pairs
  were the same frame, at a 50 ms poll against CPU sampling. The assertion is
  written so that a server which stopped repeating frames fails the lab
  rather than silently turning ember's dedupe into dead code.
- **Each of the three was checked by reinstating its bug.** A serve command
  built with `--listen-ip 0.0.0.0` instead of through `ServeCommand` makes the
  loopback lab report `sd-server listens on "0.0.0.0:1234"`; inverting the
  repeat check makes it fail against live data, proving the branch is reached;
  corrupting the captured frame's base64 makes the unit test report `image:
  unknown format` rather than passing on bytes that are not an image.
- **A cancel ends a job, not a rental, and the difference is a channel rather
  than a context.** `ember serve` refused a cancel on a generating request,
  and the 409 said why: cancelling it "would mean destroying that node, which
  would take every other request on the same rental with it". That is a true
  sentence about cancelling a *rental* and a false one about cancelling a
  *job* -- `POST /sdcpp/v1/jobs/{id}/cancel` ends one generation and leaves
  the process, the node and the resident weights alone, which is the entire
  shape of the served path. The refusal was guarding a mechanism nobody had
  connected: `sdserver.Client.Cancel` was written, covered to 99% by its own
  package's tests, and called by nothing outside them. So the fix was the join
  and not a deletion -- the `queue.Cost` precedent does not apply, because
  there is no second cancel mechanism and deleting this one deletes the only
  one. `queue.Cancel` closes a per-request channel; nothing anywhere cancels a
  context, because a cancelled context would unwind through
  `provision.WithNode`'s deferred destroy and reinstate exactly the behaviour
  the 409 existed to prevent -- and would also kill the SSH connection the
  cancel has to travel down.
- **A cancel takes the request's remaining jobs with it, and that is where the
  money is.** Stopping only the job in flight would leave the next one
  starting a second later on the same card: the button would look like it
  worked and the bill would not change, which is the "reads as working" defect
  this file keeps recording. On FLUX.2 at 2048x2048 one job is ~141s, so
  cancelling job 3 of 8 is worth 1/8 of the batch if it stops one image and
  6/8 if it stops the rest. An operator clicking cancel on a request means the
  request, because a request is the batch they asked for as one thing.
- **A server that does not report `cancel_generating` is not a refusal, and
  "fail loudly rather than skip quietly" points the wrong way here.** The
  operator has just asked ember to stop spending. Refusing leaves them one
  lever -- stopping `ember serve` -- which destroys the rental and takes every
  other request on it, so the safety check would cost more than the thing it
  refused. ember instead does the half it owns (drops the remaining jobs),
  does not ask a server that says it cannot (the 404 would surface as a
  transport problem rather than as the capability it is), and is loud about
  the part that keeps billing: *"job 3/8: cannot be stopped, so it will finish
  and be paid for (…); 5 job(s) after it were dropped"*. The loudness belongs
  in what it says, not in a 409. `Capabilities.CancelGenerating` therefore
  defaults to the *expensive* answer, which is the opposite of every other
  field beside it: an absent sampler list refuses a job, because a refusal
  before a card is rented costs nothing, and this one is read while a rented
  card is sampling.
- **A cancelled job carries an error object, and testing it before the status
  destroyed the rental.** A real `GET /jobs/{id}` for a cancelled job answers
  `{"code":"cancelled","message":"job cancelled by client"}` beside
  `"status": "cancelled"`, with `result` null. `runJobs` checked `State.Err`
  first, so every live cancel came back as `sd-server refused it: cancelled:
  job cancelled by client` -- a plain failure, which is what the drain loop
  destroys a rental for. Pressing the new cancel button would have taken down
  the node and every other request draining onto it: the exact outcome the
  feature exists to avoid, reached by using the feature. The fake answered a
  cancelled status with no error at all, so the whole unit suite agreed with
  itself and `make test-preview-lab` found it on the first live cancel.
  `testdata/job_cancelled.json` is the capture, and the fake carries the error
  object now. The same server also writes `[E] Diffusion model sampling
  failed` to its own stderr for a job it reports as `cancelled`, which is a
  second reason the status is read and the log is not.
- **ember reports whether it asked, because "cancelled" has two authors.** A
  job that came back cancelled because ember asked is the operator's stop; one
  that came back cancelled with nobody asking is something ember cannot
  account for, and filing the second under the first would record a request
  nobody stopped as `cancelled`. `awaitJob` returns the flag rather than
  letting its caller infer it, since the signal can be closed between two
  polls.
- **The ask is made once per job and the poll carries on after it.** A closed
  channel is permanently ready, so a loop that re-read it would ask every
  poll -- a request a second against the program ember is waiting for. And the
  server's own answer decides the outcome: it may come back `cancelled` with
  nothing, or `completed` because the last step finished while the ask was in
  flight, so the images are still written and the card time is still reported.
  A return that skipped the cost would make a cancelled batch look cheaper
  than it was, which is the one direction a money figure must never be wrong
  in.
- **`cancel_queued` is read by nothing, on purpose.** sd-server's queue is one
  job deep by construction -- `runJobs` submits one at a time so a finished
  result cannot be purged by `async_jobs`' 600-second TTL -- so there is never
  a queued job on the server for ember to cancel. Reading the flag would be
  reading a capability for a state ember cannot produce.
- **The cancel signal is forgotten as each batch ends, not only when it is
  closed.** The drain loop claims the next request onto the same rental, so
  the gap between one batch returning and the next starting is real, and a
  stale signal landing there would stop a batch the operator said nothing
  about. `Cancel` checks the id and `generate`'s defer clears the channel, so
  neither path has to know whether the other ran -- and a second cancel
  reports false rather than closing a closed channel, because a
  double-clicked button is the ordinary case and the panic would be in the
  drain goroutine, with a node running.
- **`webapi.Drain` is an interface so a handler test does not rent anything.**
  Three methods -- `Kick`, `Running`, `Cancel` -- all of which this package
  calls and none of which it implements, the same reason `scan` is injected.
  `RecordingDrain` answers "I did not reach it" by default, which is
  `Caps.PublicSSH`'s decision: a fake that claimed to have stopped something
  without being told would make every cancel test pass against a handler that
  never called `Cancel`.
- **Two different states reach the cancel 409, and one message told the
  operator to take the destructive action in the state it is wrong for.** The
  refusal itself is right -- "I could not stop it" and "it is stopped" are
  different answers -- and the remediation was *"restart ember serve to have
  the queue reconcile it"*, which is true of a row left `running` by an ember
  that is gone and false of the state this fires in most often. The row reads
  `running` from `ClaimNext` and `generate` registers the stop channel after
  the plan, the create, the gpu probe, the asset verify and the model load, so
  the ordinary way to reach the 409 is clicking cancel while the rental is
  still being **provisioned** -- where a rental is very much in this process,
  it is about to generate, and restarting `ember serve` destroys the card the
  operator is paying for in order to reconcile a request that was seconds from
  running. A destructive remedy for a condition that resolves itself.
  **It was invisible until the answer was rendered**: until #108 the page
  posted the cancel as `api::post::<_, serde_json::Value>` and dropped the
  `Result` with `let _ =`, so the sentence was built, serialized, parsed and
  thrown away -- the fix for one defect exposing the next, which is the
  ordinary way this goes.
  **The queue can tell them apart without a counter**, which is the fact two
  rentals at once being unrepresentable already rests on: one drain goroutine,
  a rental inside one iteration of its loop, so "this process holds a rental
  for that request" is a single value. `Queue.held` is set at each claim and
  cleared when `once` returns -- so it covers the teardown as well, because a
  request whose destroy is still running has not been handed back to anybody
  -- and `Holding` is deliberately NOT a second spelling of `Running`: the two
  differ for exactly as long as a rental takes to come up, and that difference
  is the only thing that can distinguish the two 409s. With no drain loop at
  all `holdingRequest` answers false and the restart advice stands, because
  nothing is coming up and the gentler sentence would be a promise.
  **The assertion that the two answers DIFFER is what stops them being merged
  back together**, and it is a unit test rather than a lab because only one of
  the two states is reachable from a browser: #116's cancel lab puts
  `sleep 25` in `gpu_probe`, which runs over SSH before the assets and before
  the server, so the provisioning window is twenty-five deterministic seconds
  with no test-only code -- while a row left `running` by a dead process cannot
  be produced without writing it behind the queue's back, and `queue.Run`'s
  Interrupt sweep clears it at startup anyway. So the lab asserts which of the
  two a browser is shown, including the **absence** of the restart clause, and
  `TestCancel_SaysTheRentalIsComingUpRatherThanAskingForARestart` holds both
  apart. A lab that checked only for a 409, or only that an error element
  appeared, would pass against the destructive wording going straight back in.
- **A rental that failed after the node existed left the request reading
  `running` for the life of the process, and `rentErr` was the fake that
  could have shown it.** `ClaimNext` writes `running`; the terminal status is
  written by `drain`, inside `WithServed`'s closure; and `once` called `fail`
  on exactly one path, `Planner.Plan` returning an error. So every failure
  between those two points -- the create call, waiting for sshd, the GPU
  probe, the asset verify, the model load -- returned with the row unchanged.
  Nothing corrected it afterwards, because `Interrupt` sweeps only at startup.

  **Every one of those failures happens after money has been committed**,
  which is what makes it the expensive kind of wrong: `requests.err` was never
  written either, so #103's failure reason -- built so an operator can see why
  a request died -- was blind precisely where the diagnosis costs most. A live
  instance sat in this machine's own database for three hours: status
  `running`, cost $0.626623, `err` empty, `ended_unix` 0.

  **The remedy was destructive and the UI pointed away from it.** A phantom
  row clears only on a restart, and restarting `ember serve` destroys any
  other rental in flight. Meanwhile the cancel button answered with the
  provisioning-window 409 from #119 -- "the rental is coming up" -- because
  `Queue.held` is cleared when `once` returns, so the gentler of the two
  messages was selected for the one state where the restart advice was
  correct. The two were exactly swapped by a path neither was written
  against.

  **The fix is conditional, and that is the whole care in it.** `drain` calls
  `finish` for every request it generates, so an unconditional write on the
  way out would replace a real outcome with whatever the rental returned --
  the two-writers rule recorded one entry below, where the loser overwrites
  the one that knew. A teardown that fails after a successful batch is the
  ordinary way to reach it, and a done request must not become a failed one.
  `endIfUnfinished` therefore no-ops on a terminal row, and the check is
  **inside the mutator** rather than a read before it, because `Store.Update`
  runs SELECT, the mutator and UPDATE in one transaction: asking first and
  writing after is the same decision with a window in it. `classify` is one
  function because two copies of "what does Ctrl-C mean" is two answers.

  **`fakeRental.rentErr` already existed and no test had ever set it.** The
  field was declared on the fake, plumbed through `fakePlanner`, reached the
  rental -- and `grep` found no assignment anywhere in the package, so the
  entire span this entry is about could not be produced by the suite although
  the fake was already able to produce it. Not a missing assertion but an
  unused capability, which is the "written, covered, connected to nothing"
  shape reached inside a test fake rather than in production.

  **A lab, because a unit test cannot rent.** `TestServe_ARentalThatFails...`
  drives a real container with a `gpu_probe` that exits 1: the node is
  created, sshd is up, the host key is pinned, and only then does `prepare`
  refuse. `gpu_probe_timeout` is five seconds and that is load-bearing rather
  than a shortcut -- `gpucheck.Wait` RETRIES until the timeout, because a card
  that is not up yet and one that never will be look identical on the first
  attempt, and the probe's own output is buffered and printed only when it
  finally gives up. The first version of the lab waited thirty seconds for a
  line that is not emitted until then.

  Checked by reinstating four defects: the missing call (unit and lab both
  red), an unconditional write (the negative-control test red, the lab green,
  which is the correct asymmetry), Ctrl-C classified as a failure (red), and a
  reworded comment that stayed green.
- **A run submitted by `ember up` reaches the provisioning-window 409 and that
  message is wrong for it, which is asserted rather than left to be noticed.**
  Its row is `running` from `Adopt` and `Queue.Holding` is true, so
  `cancelRequest` picks the gentler of the two 409s -- "ask again once it is
  generating" -- and such a run never becomes `Running`, because there is no
  sd-server job to stop: the only way to end an `ember up` is to destroy the
  node, which is `ember down`. So the promise cannot be kept. It is NOT fixed
  here, and the reason is the shape of the fix rather than its size: telling
  the two apart needs a FOURTH answer from the queue, and a fourth answer is a
  change to `webapi.Drain` that belongs with `ember down` over the socket --
  the slice that makes a cancel of a submitted run mean something. Asserting
  the current answer is the `TestConformance_CannotCatchARederivedRunID`
  arrangement: a limitation nobody can read out of the code is a limitation
  that will be forgotten, so the day somebody fixes it the test goes red and
  they read the comment beside it. The detach notice names `ember down`, which
  is the command that does work.
- **The queue records how a cancelled generation ended; the handler does
  not.** A queued request has no rental and nothing to tell, so the handler
  makes it terminal. A running one is still generating when the handler
  returns -- ember is polling sd-server for the job's own answer -- so the
  status it ends up with belongs to the queue. Two writers for one row means
  the loser overwrites the one that knew.
- **Both of those answers were built, serialized, parsed and thrown away one
  frame later.** The page posted the cancel as
  `api::post::<_, serde_json::Value>` and discarded the `Result` with
  `let _ =`, and the queue panel had no error signal at all -- the only two in
  the page are `Compose`'s and the file picker's -- so cancel was the one
  control in this program whose failure went nowhere. The 409 whose message
  names the remediation reached `api::reason`, which exists to carry a
  server's sentence *in full* and says so in its own comment, and the string
  was dropped: what the operator saw was a row that still said `running` with
  a `cancel` button still on it, which is indistinguishable from a click that
  did not register -- so they clicked again, over a card that may still be
  billing, and the second ask is the one that comes back 409. `cancelling` had
  a writer, a wire name, a comment explaining it and **no reader**, which is
  the preview-frame shape in miniature, and `grep -rn 'cancelling' web/`
  returned nothing.
  **The type is named now, for the reason every other route on this seam has
  one**: a `serde_json::Value` decodes a field nobody reads and nobody finds
  out. `Cancelled::label` keeps the two answers apart -- a stop on its way
  (`cancelling`, beside a status that honestly still reads `running`) and a
  request the handler ended itself (its own final status, which reaches the
  operator before the two-second poll) -- and `Stopping::label` adds the state
  that is neither: an ask this page has sent and had no answer to, which says
  `asking`. **Those three words being three is what makes the fix checkable
  from outside.** The page marks a request the moment it asks, because the
  round trip must not be a window in which the button is still there -- so if
  the placeholder were spelled `cancelling` as well, a browser could not tell
  a page that reads the body from one that has gone back to ignoring it, and
  the only check left would be a Rust unit test of the page agreeing with
  itself. An empty cell there is worse again: it reads as nothing having
  happened, which is the defect.
  **The button goes away rather than going grey**, because what it must not do
  is send a second request: the Go side survives a double click by design
  (`Cancel` checks the id and forgets the channel rather than closing a closed
  one) and the page should not be producing the ask that comes back refused. A
  refusal releases it again -- nothing was stopped, and the 409's remediation
  is a restart after which the retry is the right move.
  **Only a browser crosses this gap, and the lab drives both directions on one
  request.** The refusal is reached in a real state rather than a contrived
  one: the row says `running` from `ClaimNext`, and `generate` registers the
  stop channel after `WithNode`, the asset verify and the model load -- so the
  whole of provisioning is a window in which the row is cancellable and no
  rental is generating it, which is exactly what the 409 is written for. A lab
  asserting only the accepted path would pass against a page that swallows
  every refusal, which is the `AssertNoStray` failure reached from a fourth
  direction.
  **The window is made long rather than raced, and with no test-only code.**
  On the local provider it is a container create, an sshd and a one-second
  model load -- a handful of seconds against a two-second listing poll, which
  is a coin flip and cost one thirteen-minute run to learn. `gpu_probe` runs
  over SSH before the assets and before the server, so a lab config whose
  probe sleeps puts twenty-five deterministic seconds inside the window: the
  key an operator writes for a card that needs time to come up, doing what it
  says, on a node that has no card at all. A `waitForText` is also not a
  bounded wait -- `chromedp.Text` resolves at least one node and blocks until
  the context ends, so an element that never appears spends the whole lab's
  budget and reports "context deadline exceeded", which is how that run was
  spent.
