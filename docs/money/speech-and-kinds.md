# A second workload kind

piper, the one-method seam a second kind needed, the artifact type, and the
voice catalogue that keeps the request seal intact.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

- **ember has a second typed workload, the seam it needed is ONE method, and
  every number below was measured before any of the design (#190, #36).** #36
  asks whether a second typed integration is worth its cost and says the
  crossover "should come before the design". Both are answered here for
  speech.

  **THE BINARY IS piper AND THE PIN IS NOT MERELY AGAINST DRIFT.**
  `2023.11.14-2` is the last release that ships a self-contained C++ binary
  with onnxruntime beside it; the repository has since been rewritten into a
  Python package (`piper1-gpl`), so a later tag would carry an interpreter and
  break this project's own rule -- "a runtime that compiles or interprets on
  arrival is buying a compiler with GPU time". The image lab asserts there is
  no `python3` on it, which is the only thing that would catch a bump.

  **THE CROSSOVER, which is #36's decision 1.** en_US-lessac-medium, eight
  utterances of about 4.6 s of audio each, one container on this machine:

  | | eight utterances | each |
  |---|---|---|
  | one piper process per utterance | 4740 ms | 592 ms |
  | one process for all eight | 1822 ms | 185 ms load + ~205 ms |

  A fresh process is ~400 ms of fixed overhead -- 185 ms of it the voice load
  -- against 170-220 ms of inference, so the resident shape is worth about
  **2.9x per utterance and crosses over at TWO**, not the ~5 the sd15 run
  predicted: piper's fixed cost is a sixth of sd15's and its per-job work a
  tenth. #36 guessed "probably the bigger win proportionally" and was right.

  **IT IS ONE-SHOT ANYWAY, AND THE REASON IS A SEAM RATHER THAN THE NUMBER.**
  piper has **no server mode**: the pinned release's own `--help` lists
  `--output_file`, `--output_dir`, `--output_raw` and `--json-input`, and every
  one of them reads its text from **stdin**. So resident means holding a
  process's stdin open, and `sshx.Runner` hands back stdout, stderr and a wait
  and has none -- the interface this file records as not widened for a test's
  convenience, which would here be widened for one driver. And 400 ms per
  utterance is noise against the **~27 s of per-rental overhead** already
  measured. The honest order is to take the one-shot measurement on a real
  batch first, which is why the figures live in `internal/tts` rather than in
  a commit message.

  **PROGRESS IS THE JOB AND THERE IS NONE INSIDE ONE, which is #190's open
  question 3 answered by measurement.** piper emits nothing at all between the
  start of an utterance and its completion, then one line:
  `Real-time factor: 0.0367 (infer=0.173 sec, audio=4.713 sec)`. No step, no
  pass, no percentage -- a TTS job is atomic. So the unit is `report.UnitJob`,
  which already exists, and the two figures travel on the artifact event
  because they are the artifact's own duration and what it cost to make: the
  closest thing speech has to a step count, and derivable from nothing else
  ember sees. **Those figures are CUMULATIVE within a process and per-job
  across one**, measured on the eight-utterance run -- `audio=` walked 4.71,
  9.18, 13.68 ... 36.58 and `infer=` 0.173, 0.311, 0.507 ... 1.530. One
  process per job is therefore the shape in which that line means what it
  reads as, and a future resident driver has to subtract; `spokenReport` takes
  the LAST match for that reason rather than the first, which is cheaper to be
  right about now than to find later.
  **It is a parse of a LOG and that is the one place this driver is weaker
  than the sd one**, stated rather than hidden: sd-server answers a JSON
  object with a status in it. The status here is the exit code, which is what
  decides whether the job worked -- the rule this file records about reading a
  status and not a log -- so a line upstream rewords costs the two figures and
  nothing else.

  **THE SEAM IS ONE METHOD, AND A WIDER ONE WOULD HAVE BEEN THE MISTAKE IT
  LOOKS LIKE IT AVOIDS.** #190 asks for a kind to supply five things: how to
  start its server, how to submit a job, how to read progress, what parameters
  are typed, and what a result is. **Four are already where a kind owns
  them** -- the typed parameters are its own config block and its own job
  type, starting a server is `node.Spec.Serve` plus the kind's argv builder,
  submit and poll are the kind's package, and a result reaches every reader as
  a `report.Event`. What was nowhere was the **dispatch**: one line in
  `Plan.serve` reading `if len(p.Spec.Serve) > 0`, which is a question about
  sd-server's argv standing in for "which kind is this run" -- correct while
  there was one kind and no answer at all for a second. So `app.batch` is
  `Run` plus `Describe`, `servedBatch` is a four-line adapter over the
  existing `runServed`, and `spokenBatch` is the second implementation that
  makes the interface honest rather than a guess. A readiness/submit/poll
  interface would have been designed from sd-server's shape and then had to be
  satisfied by a program with no server, which is the
  driver-interface-from-one-example mistake committed while appearing to avoid
  it. `Describe` is why it is an interface and not a func type: a refusal that
  cannot name its kind is one an operator has to work out.
  **`runServed`'s body did NOT move**, deliberately: it is 300 lines with a
  server process, a poll loop and a cancel channel in it, and rewriting it to
  fit an interface in the commit that introduces the interface would mean the
  seam and its first implementation were verified by the same tests at once.

  **`node.Spec` GAINS NO FIELD, AND THAT IS WHY THE ONE-KIND REFUSAL MOVED UP
  A LAYER.** The typed batch travels beside the spec, as `Plan.Jobs` already
  does, so a request can never be consulted about a voice -- the seal this
  file states as "a request names a provider, a card and a batch of jobs, and
  nothing else" holds with nothing added to it. The consequence is that
  `node.NewSpec` **cannot see** this kind, and without a refusal in
  `app.NewPlan` a config naming both would have one of the two silently
  dropped -- an operator who wrote both with a rental that generated the wrong
  thing and no line anywhere saying why. `node.Spec.Workload` is what lets
  that message name the other key.

  **THE ARTIFACT PATH WAS A LITERAL, which is #36's fifth decision.**
  `getImage` declared `image/png` unconditionally -- true of everything ember
  produced until a second kind made a wav, and with `nosniff` beside it a
  browser will not play that file and **cannot be talked into** playing it,
  which is the failure #36 predicts in words. It is derived from the key's
  extension now, through a **closed switch and not `mime.TypeByExtension`**:
  that function reads `/etc/mime.types`, so what ember declares about bytes a
  rented machine produced would depend on which packages are installed on the
  operator's laptop, and it answers for every extension there is where the set
  ember writes is two. An extension no writer here produces is a **500** and
  not an octet-stream, because the row is ember's own: a writer and that
  switch having come apart is a defect, and a download prompt for an image
  reads as ember being broken in a way that points nowhere.
  **`internal/blob` needed nothing**, which is worth saying because #36 lists
  it: it is byte-generic already and the key carries the extension, so there
  is no migration, no stored type and nothing derived -- the `images` table's
  name is left alone and recorded as a name rather than renamed, since it is a
  key already in operators' databases.
  **And the measurement is why the wav test can fail where the PNG one could
  not.** `http.DetectContentType` answers **`audio/wave`** for a RIFF/WAVE
  header -- a *different* spelling from the `audio/wav` ember declares -- so a
  declaration that went missing shows up as the wrong spelling rather than as
  the right answer. For a PNG it sniffs to `image/png`, which is exactly why
  `TestImage_ServedWithItsTypeDeclaredAndNotSniffed` passed with its property
  destroyed and why that assertion lives over a real listener.
  **One defect is NAMED AND NOT FIXED**: `sdserver.WriteImages` names every
  file it writes `.png` whatever `output_format` asked for, so a jpg is
  already served as a PNG today. That is this same defect one driver over, it
  predates this change, and the name is a key already stored -- so
  `artifactType`'s comment says so where the next reader will be.

  **WHAT IS NOT BUILT, said rather than left to be found.** There is **no
  catalogue**: #190's entry-id shape is the right answer for a browser, this
  change is the server side, and `internal/catalogue` is already the GPU
  availability history, so the name is taken as well. The request wire type is
  untouched -- a request names no speech at all yet, so it still names no
  image, no argv and no assets. Nothing in `web/src/` is touched. `ember
  serve` **cannot queue an utterance**: `queue` builds an `sdserver.Run` from a
  stored request, and widening that -- plus `genstore.Job`'s columns, the
  compose panel and a per-kind parameter schema -- is the browser half, which
  is #190's questions 2 and 4 and #139's page split. The resident driver is
  the entry above, with its number.

  **TWO LABS FOUND TWO THINGS ON THEIR FIRST RUN, and one of them was the
  lab.** The image lab asserted piper's flags through `harness.ExecOut`, which
  returns **stdout alone** -- and piper writes its usage to **stderr**, so the
  call came back EMPTY and every assertion reported a missing flag against a
  binary that publishes all of them. The failure of a check reading the wrong
  stream reads exactly like the failure it was written for, which is
  `LAB_LOG_DIR`'s defect through a file descriptor instead of a path.
  And the speed assertion's first version grepped `ember up`'s output for
  `--length_scale 1.1` and **went red against correct code**, because `sayOne`
  prints the utterance and not the argv: an assertion about a mechanism nobody
  had run, which is the `chromedp.SetValue` entry's shape one layer out. It is
  the image lab's own rule now -- same text, same voice, two arms differing
  only in `length_scale`, and the second file has to be LONGER -- so a flag
  ember dropped makes two wavs of one length, which every other assertion in
  that lab passes over.
  **The bound is measured and the first one was a flake.** The curve for this
  voice and this sentence, in sample frames off the wav header: 1.0 -> 65652,
  1.2 -> 76404, 1.4 -> 82036, 1.6 -> 89716, 2.0 -> 103284 -- so `length_scale`
  is **not** a linear multiplier on the duration, 1.4 buys 1.25x and 2.0 buys
  1.57x. 1.4 against a 1.15 bound measured **1.16**: a one-percent margin over
  a figure that moves by three, because `noise_scale` is generator noise and
  nothing here seeds it. 2.0 against 1.3 measures **1.67**, which moves the
  signal rather than narrowing the bound -- the right direction when a
  measurement is tight, and the opposite of what relaxing the bound would have
  been.

  **TWELVE REINSTATEMENTS, CONTROLS AT BOTH ENDS, ONE BACKUP PER PATH TAKEN
  ONCE, AND EVERY CASE ENDING ON A DIFF AGAINST HEAD RATHER THAN ON AN EXIT
  CODE -- AND THE ONE THAT CAME BACK GREEN IS THE FINDING.** Ten red where
  expected and two negative controls green: `artifactType` answering
  `image/png` for a wav, the two-kinds refusal deleted, `Command` sending a
  default ember invented, the text interpolated into the script body,
  `spokenReport` taking the first line, `SpokenStem` not numbering a named
  job, `ParseTTS` dropping `length_scale`, the artifact event not emitted, and
  the fetch skipped.
  **The eleventh -- the dispatch reverted to `len(p.Spec.Serve) > 0`, which is
  the defect this whole seam exists to make impossible -- is `go test ./...`
  exit 0.** Every `runSpoken` test drives the driver or its adapter directly,
  where the dispatch is not consulted at all, so the defect lives in one line
  of `Plan.serve` that only a real rental reaches: a speech run generates
  nothing and says nothing. The lab is what reports it, at **exit 2** on "the
  output does not mention `speaking:`" and "no greeting-01.wav among []". That
  is this file's own "the most expensive defect available in a change is not
  reliably the one the unit suite can see", met again -- and the reason the
  lab exists rather than being coverage.

  **THE VOICE IS BAKED IN, WHICH IS THE OPPOSITE OF THE RULE FOR A
  CHECKPOINT.** This file's measurement is that a `COPY`'d model gets a layer
  digest that is ours alone and can never hit a shared cache -- true, and it
  was 51% of ember-sd at 2.2 GB. This voice is **60 MiB in a 286 MB image**,
  and what baking buys is that a local run needs no store, no mount and no
  sync, which is the whole point of the local provider. The digest is still
  checked, one layer earlier, by `ADD --checksum`, and the image lab
  re-verifies it with `sha256sum` **over SSH** rather than trusting the
  Dockerfile -- because an image's `ENV` does not reach anything sshd starts
  and neither does a build-time check. **The .onnx digest is confirmed
  twice** -- Hugging Face publishes it as the LFS object id and a `sha256sum`
  of the download agrees -- which is the calibration habit
  `internal/provider/runpod/testdata/README.md` set. **The `.json` has ONE
  confirmation**, because it is under the LFS threshold and Hugging Face
  publishes no sha256 for it; said in the Dockerfile rather than left to read
  as equal evidence.
  **There is no CUDA variant and that is a measurement too**: piper's released
  binary links onnxruntime's CPU execution provider only, so there is no
  kernel for a card to be wrong about, nothing for `images:` to key on, and
  the wrong-architecture abort this file records cannot happen to it.
- **A request can name a KIND now, and the catalogue is what keeps the seal
  intact rather than widened (#190).** The entry above ends with "`ember
  serve` cannot queue an utterance", and this is that. The wall was three
  layers deep and every one of them was the same sentence written when sd was
  the only kind.

  **THE SEAL, STATED AS WHAT A REQUEST CAN AND CANNOT NAME.** A speech request
  names a provider, a card, a **voice id** and text. It has no field for a
  voice path, espeak's data directory, a binary, argv, an image, a store, a
  mount, a port, a probe, assets or `extra` -- so the refusal is structural
  rather than checked. `tts.Ask` is the shape a request stores and the shape a
  row reads back as; `tts.Job`, which carries the two filenames, is reachable
  only through `Ask.Resolve`, and the only thing that calls it holds the
  catalogue the process was started with. A test reads `Ask`'s fields through
  reflection and fails on one nobody has read, because a hand-read list of
  what it holds is the thing that goes stale and what would be smuggled in is
  whichever field the list has not heard of.

  **A VOICE CATALOGUE AND NOT #190's WHOLE MODEL STACK, and the narrowing is
  the point.** That issue describes an entry carrying "the image reference,
  the assets with their digests, the probe, the serve command and the
  outputs". A stack catalogue makes a request choose an **image**, which is a
  bigger question than speech -- every seal in `node`, `exposure` and
  `loopback` sits downstream of `image:` -- and one the sd path would have to
  move for too, since `sd.serve` is one server per config. A voice is the one
  dimension a browser needs and the one that is a path on the node, so it is
  the honest first entry, and the id shape does not change when the wider one
  arrives. `internal/catalogue` is also already the GPU availability history,
  so the NAME is taken as well.

  **AN EMPTY CATALOGUE IS A REFUSAL AND NOT A DEFAULT, which is the decision
  a reader is most likely to think is an omission.** The obvious shape is one
  implicit entry derived from `tts.voice` -- and that key's own default points
  at ember's own speech image, so an operator running `image: ember:sd` with
  no `tts:` block would be shown a voice that cannot work, with the refusal
  arriving after a card was rented. So `tts.voices` is the catalogue, it is
  empty unless the operator wrote it, and `POST /api/requests` refuses speech
  against an empty one naming the key. `ember up` is untouched: a config file
  names one voice because it names one run, and the asymmetry is written where
  `ParseTTS` resolves it.

  **THE ID IS AN ALLOW-LIST ON A FIELD THAT IS NOT DANGEROUS YET, and the test
  written for that argument found the hole in it.** `[a-z0-9._-]`, lowercase,
  bounded at 64. It reaches no path today, so a loose charset costs nothing
  NOW -- what it costs is the day somebody widens the catalogue and reaches
  for the id as a directory name, at which point a `..` accepted for a year is
  already in operators' configs and in their stored rows. And `..` **passed**
  the charset, because it is spelled entirely out of permitted characters: the
  hole was in the check whose whole case is "the day somebody uses this as a
  directory name", found by the test written for the case and not by review.
  An all-dots id is refused rather than cleaned, which is
  `internal/transfer`'s rule -- `path.Clean("/../a")` returns `/a`, so a check
  that cleaned first could never fire.

  **KIND IS ON THE REQUEST AND NOT ON A JOB, which makes a mixed batch
  UNREPRESENTABLE rather than refused.** One rental runs one driver, so an
  image job and an utterance in one request would need two of them on one
  node. It is also what `Compat` reads: without it the provider and the GPU
  would match, the claim would succeed, and the drain loop would hand an
  utterance to a resident sd-server that has no route for it -- after a card
  had been paid for. **The VOICE is deliberately not in `Compat`**, which is
  the measurement rather than an omission: a one-shot piper names its own
  `--model` per invocation and nothing is held open, so a rider wanting a
  different voice is free, and a resident driver is the only shape that would
  change it. An assertion says so, so the day one is built it goes red.

  **EMPTY MEANS `sd` AND NOTHING ELSE COMPARES AGAINST `""`.** Every row
  written before schema version 6 is an image generation, so the alternative
  was `UPDATE requests SET kind='sd'` -- which rewrites every row of a table
  this schema is careful never to rewrite, and asserts about an operator's
  history rather than recording the absence. `genstore.KindOf` is the one
  place that reading lives, and it had to reach into SQL: the claim's
  predicate is `CASE WHEN kind = '' THEN 'sd' ELSE kind END`, because a plain
  `AND kind = ?` would refuse every pre-v6 row to an sd rental and leave an
  upgraded queue permanently unclaimable. Both stores spell the same rule, for
  the reason the conformance table exists.

  **TWO COLUMNS AND NOT A REWRITE: `requests.kind` and `jobs.tts`.** The kind
  is a `WHERE` -- it is in `Compat`, so it decides which rental a queued row
  may ride -- and the utterance is payload, one JSON blob in one column, which
  is what `jobs.params` already is for the other driver. **A second key in
  `params` was the alternative and is worse**: that column is
  `sdserver.Params`, a typed struct of one driver's twenty-three fields, so
  folding a second kind's payload in would make it mean "whichever kind's
  parameters these are" and the first reader to unmarshal the wrong one gets
  zeros rather than an error. "One JSON column, not twenty-three" refuses a
  column per FIELD, which this is not.

  **WHAT A SPEECH JOB WRITES INTO THE PROGRESS COLUMNS, which is the question
  the shape of this schema makes hard.** `images`, `bytes`, `cost`, `started`
  and `ended` are real. `passes` and `steps_done` stay **zero**, because an
  utterance has neither -- a figure there would be a number with the authority
  of a measurement and nothing behind it, which is `load_elapsed_seconds`
  twice. The two figures piper does print, the audio's duration and what
  inference took, travel on the artifact event and are in `events` -- and are
  **not columns**, because `ember history` has no reader for them and a column
  nothing reads is "a query layer with no reader", which this file calls the
  same failure as a feature built and never connected. The honest follow-up is
  `audio_ns`/`infer_ns` at the moment a reader exists.

  **`requests.model` IS EMPTY FOR SPEECH, AND THAT IS A REFUSAL.** That column
  is documented as what the generation server reported, and this file's reason
  for it is that "the model is recorded from a RESPONSE, and that is what
  makes grouping by it possible". piper publishes nothing. Filling it from the
  config would be "a value read from the wrong place and believed", which is
  the mistake the column exists to refuse; the voice is not lost by it, since
  each job's stored `Ask` carries the id. Both readers already print a dash
  and the ledger's footnote already says it means nobody read one, which is
  exactly true.

  **THE SEAM IS ONE MORE METHOD AND THE DISPATCH IS STILL ONE PLACE.**
  `queue.runner` is `Generate` plus `Model`, with `sdRunner` holding the
  conversion `Queue.generate` used to do inline and `ttsRunner` holding the
  catalogue. `Rental` gains `WithSpoken` beside `WithServed` -- two methods
  and not one taking a kind, because the two closures are handed different
  things and one returning an `any` would put a type assertion in the drain
  loop, which is the dispatch this work exists to remove rather than relocate.
  **One rental at a time is untouched and still structural**: both arms of the
  branch call `q.drain`, which is called from nowhere else, the rental lives
  inside one iteration of `Run`'s loop, and neither arm names
  `provision.WithNode` -- which `TestNothingRentsOutsideInternalApp` bounds.
  The fakes go through one `rent` for the same reason: two counters could each
  be at one while two nodes were up.

  **THREE WALLS, AND ONLY A LAB COULD SEE TWO OF THEM.** `ember serve`
  refused to start without `sd.serve`; `POST /api/requests` answered 409 to
  everything for the same reason; and both are invisible to `go test ./...`,
  because every unit test of this path builds a `webapi.Options` or a
  `queue.Queue` directly and never asks whether the configuration behind it
  could serve anything. The suite was **entirely green over a configuration
  `ember serve` refuses to start from**. What reported them was the first run
  of the lab that posts a request to a real server -- one after the other,
  each fix exposing the next, which is the ordinary way this goes. Both
  refusals are per kind now and name both keys, and the enqueue one is a
  **409** rather than a 400 because the body is well-formed and what is
  missing is a block in the operator's own file: a 400 would tell a page to
  change what it sent.

  **AND THE LAB'S OWN FIRST GREEN RUN REPORTED A LEAK.** A request that
  finished does not destroy the node -- the drain loop destroys it when
  nothing COMPATIBLE is left -- so the test exiting killed `ember serve`
  before the teardown ran and left a container and a bridge network behind.
  Nothing bills for the network, which is why this file records that class
  going unnoticed until Docker had subnetted its whole address pool. It
  SIGINTs and waits now, which is `ember serve`'s documented shutdown.

  **THE PROJECTION PUBLISHES IDS AND NAMES AND NOT PATHS**, which is
  `wire.go`'s own rule -- a hand-written view means adding a field somewhere
  cannot publish it by default -- and `GET /api/config` carries `kinds` from
  the one list so a kind added later reaches a UI without anybody remembering
  the projection. The catalogue is **one derivation**: `runServe` builds it,
  `serveUntil` takes it, and both `Queue.Voices` and `Options.Voices` are
  handed that one value. The first version had `viewConfig` call
  `cfg.TTSCatalogue()` itself and a test caught it -- two readings of one
  file, four lines from the comment arguing against exactly that, which would
  have become a browser offering ids the drain loop could not resolve the day
  somebody added a `--config` reload.

  **WHAT A BROWSER NEEDS FROM THIS AND WHAT IS STILL MISSING.** It needs
  `GET /api/config`'s `kinds` and `voices`, and a `POST /api/requests` body
  with `kind: "tts"` and a per-job `tts` object. What is NOT here: nothing in
  `web/src/` is touched, there is no compose panel and no per-kind parameter
  schema, and `requestView`'s jobs still project the sd shape -- so a page
  can queue an utterance and read its artifact, and cannot yet render the
  utterance back. That is #139's page split and #190's question 2.

- **TWO CORRECTIONS TO THE ENTRY ABOVE, both found by an independent reading
  and both about an exit code rather than about the code.**

  **THE WAV HEADER ASSERTION COULD NOT FAIL, AND THE REASON IS NOT A DEAD
  ASSERTION.** The entry above says `http.DetectContentType` answers
  `audio/wave` for a RIFF header so a lost declaration shows as the wrong
  spelling. The measurement is right and the inference about WHICH fallback
  was wrong: `http.ServeContent`'s **first** fallback is
  `mime.TypeByExtension`, not sniffing -- and the route passed
  `path.Base(key)`, so the name carried `.wav`, and on a box with
  `/etc/mime.types` that answers `audio/wav`, the identical spelling ember
  declares. Deleting the `Set` left every header assertion on that route
  GREEN. **Worse than dead, because it was environment-dependent**: `.wav` is
  not in Go's builtin mime table, so the same defect was red on a slim
  container and green on a laptop -- a verdict depending on installed
  packages, which is precisely the dependency `artifactType`'s closed switch
  exists to refuse, reintroduced through the test's fallback path. One token
  fixes it: the name is `""`, which costs nothing while a declaration is
  always present (the switch is closed and its unknown path 500s), and the
  assertion is then RED at `content-type = "audio/wave"`. **And the PNG arm
  still cannot fail that way**, measured and stated rather than assumed
  symmetric: Go sniffs `\x89PNG` to `image/png`, so that test's live subject
  remains the ORDERING and the `nosniff` it was moved here for.
  **Reinstated three ways rather than one, which is what separated the two
  halves**: the declaration alone (red), the name alone with the declaration
  present (green, so the token costs nothing), and BOTH -- the historical
  pair -- which is **green** and is the state the fix was worth making for.
  The first attempt at the first of those stubbed `artifactType` as well and
  turned a second, unrelated assertion red, which is the "ask why a case went
  the way it did" rule catching a patch that was about the wrong thing.

  **AND THE ELEVENTH REINSTATEMENT IS EXIT 1 RATHER THAN EXIT 0, with the
  substance unchanged.** The entry above says reverting the dispatch leaves
  `go test ./...` at exit 0. It leaves it at **exit 1**, because
  `TestNoProductionDeadCode` fires on `internal/app.(Plan).batchOf` having no
  production caller once the branch stops calling it. That is the
  `TrustOnFirstUse` shape this file already records and already judges: a
  stale-inventory check whose failure text tells you to delete a function, and
  which says nothing about speech runs being dispatched to an sd-server.
  Excluding that one guard the tree is exit 0 across forty packages, so the
  claim -- that no assertion about the behaviour reports it and only the lab
  does -- holds in full and needed the clause.
- **A GUARD THAT CATCHES ITS DEFECT BY ACCIDENT IS ONE A TIDY-UP DISARMS, and
  the migration rule had no assertion whose subject was the rule.** The entry
  above argues that `CASE WHEN kind = '' THEN 'sd'` is the difference between a
  working upgrade and a permanently unclaimable queue, and reports a
  reinstatement going red. It did -- at
  `TestClaimNext_OnlyJoinsACompatibleRental`, and **only because the shared
  `req()` fixture happened not to set `Kind`**, so its rows stored `''` while
  the compat held `sd`.
  **Measured, and the measurement is the finding**: add `Kind: KindSD` to that
  fixture -- which is an entirely reasonable tidy-up now the column is
  first-class and every other field there is set -- and with the wedge still in
  place `go test ./internal/genstore/` is **ok** and `go test ./...` is **exit
  0, no FAIL anywhere**. `grep` found no pre-schema-6 claim test in that
  package at all. So the rule rested on a literal in a fixture rather than on
  an assertion, and the next person to tidy that struct would have silently
  re-opened it for every operator upgrading from schema 5.
  **BOTH HALVES, because either alone leaves the dependency.** The fixture is
  tidied AND `TestClaimNext_AMigratedRowHasNoKindAndIsStillClaimable` is
  written: it builds the pre-v6 shape itself, asserts in its own body that the
  kind really is empty, and claims it with an `sd` compat -- which cannot be
  switched off by anybody editing a fixture, because the absence is the
  subject. Tidying alone would have removed the only catch there was; writing
  alone would have left two assertions where one is accidental and a reader
  cannot tell which. Reinstated: the SQL defect is now red at the new test and
  **not** at `OnlyJoinsACompatibleRental`, which is the proof the accident is
  gone rather than merely covered.
  **The refusal is asserted before the acceptance**, because an acceptance
  alone is consistent with a claim that takes everything -- which is exactly
  what a compat ignoring the kind is.
  This is `TestOpen_MigratesAVersionFourFile`'s lesson from the other side:
  that one was a hand-built fixture missing the ROW the bug reads, and this was
  a hand-built fixture missing the VALUE.

- **The output stem was the second request-supplied string reaching a filename
  and the only one with no length bound.** The entry above frames the voice id
  as "the one place a path enters", and `Ask.Output` is also a wire field,
  resolved into the node-side path by `Resolve`. `MaxVoiceID` is 64 and
  `MaxText` is 8000, both with written reasons; the stem had none -- so a
  **100,000-character** stem passed `ValidateOutput`, `Job.Validate` and
  `Resolve`, and failed at `piper --output_file` **on the node, after a card
  was paid for**, which is the one thing that function's own doc says it exists
  to prevent. `tts.go`'s own sentence claimed every bound was piper's "except
  `MaxText` and the stem -- both of which are ember's and say so", and the stem
  had no bound to say: a claim in a comment worth exactly what the code does.
  **The bound is DERIVED and the arithmetic is the assertion.** `NameMax` is
  255 -- NAME_MAX on ext4, xfs, btrfs and overlayfs -- and `suffixReserve` is
  what `SpokenStem` composes around a stem, so `MaxOutput` is the subtraction.
  A round number there would be ember's taste standing in for a filesystem's
  limit, and the thing being prevented is exactly the filesystem's refusal; 240
  bytes is longer than any name anybody types, so the bound is not a judgement
  about names at all.
  **AND THE FIRST RESERVE WAS ONE BYTE WRONG, FOUND BY THE ARITHMETIC TEST
  WRITTEN FOR IT AND NOT BY REVIEW.** It reserved EIGHT, derived from a
  two-digit index padded into `-100.wav` -- and `-1000.wav` is nine, because
  `%02d` is a MINIMUM width and nothing bounds how many utterances a batch
  holds, so the index's width is the batch's size. The reserve is ten digits
  now: over-reserving is the safe direction and costs nine bytes of a name
  nobody types. A bound that was correct until somebody queued a hundred
  utterances is the `imagelabs` shape reached through arithmetic.
  **The two halves are in different packages, and that is a boundary rather
  than a choice**: `suffixReserve` is the claim and `SpokenStem` is what
  appends it, and `internal/tts` cannot call `internal/app`. So the
  subtraction is asserted there and the composition here, in
  `TestSpokenStem_FitsInsideTheFilenameBoundItWasGiven` -- which is the test
  that went red over the eight.
  **IT STAYS A DENY-LIST WHERE THE ID GOT AN ALLOW-LIST, and the asymmetry is
  argued rather than left to look like an oversight.** An id is a TOKEN:
  compared with `==`, round-tripped by a UI, and nothing legitimate needs a
  space or a capital in one, so the narrow set costs nothing. A stem is a
  FILENAME an operator chose -- `blob.ValidateKey` and `sdserver`'s own stem
  rule are both deny-lists for that reason, because an allow-list would refuse
  a space, an accent or a CJK character, all of which are names somebody means.
  What the two share is that neither CLEANS: a hostile name is refused, because
  `path.Clean` absorbs a traversal and a check that cleaned first could never
  fire.
  **Six reinstatements, each judged on an exit code AND on which test
  reported it**, which is the half the #165 entry records as the one nobody
  checks: a correct verdict filed under the wrong mechanism is what two
  independent readers got wrong there. Red and correctly attributed: the SQL
  predicate raw, the recording store's comparison raw, the length check gone,
  the reserve back to eight, and `MaxOutput` as a round literal. One negative
  control green. Controls at both ends, every case ending on a `git diff`
  against HEAD -- and one case reported ANCHOR-MISS rather than a verdict,
  because a `gofmt` rewrap had moved the line it was anchored on, which is
  trap one working.
