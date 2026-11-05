# Testing discipline

The full record: what each gate is, the defects that motivated it, the
harness traps, and the measurements. `AGENTS.md` carries the summary and
the commands.


- Standard library `testing` only. No testify, no gomock, no testcontainers.
- One narrow interface per external system, with a real implementation and a
  `Recording*` fake carrying `Calls`, `Outputs`, `Errors` and `FailOn` — the
  same field names as go-tun's `linux.RecordingRunner`, so they read without
  being learned.
- `go test ./...` must need no GPU, no Docker, no network and no credentials.
  The labs are separate and gated twice: `//go:build integration` **and**
  `EMBER_INTEGRATION=1`.
- Coverage is gated per package by `cmd/check-coverage`, with `-strict`: a
  package in neither the gated list nor the exclusion list **fails the build**.
  Exclusions are printed on every run so they appear in CI logs and review
  diffs rather than living unread in a Makefile variable.
- **A test that skips itself reports success for something that never ran.** CI
  greps for `--- SKIP` in both the unit and the lab logs and fails on a hit.
  In the Makefile's lab targets that grep was for a long time **the only thing
  that could fail them**, because the `go test` beside it was piped into `tee`
  and a shell pipeline reports the last command's status: four targets --
  `test-image-labs`, `test-preview-lab`, `test-serve-lab` and
  `test-image-labs-publish` -- exited 0 over a red suite, including the one
  that gates a publish, and the guard against the small failure was bolted onto
  a pipeline that discarded the large one. The Makefile sets
  `SHELL := /bin/bash` and `.SHELLFLAGS := -o pipefail -c` once, globally,
  rather than fixing four recipes, so the next pipeline somebody writes cannot
  reintroduce it. `pipefail` and not `-e`: make already stops a target on a
  non-zero recipe line, while `-e` would abort `workload-sd` before the
  `rm -f .model.gguf` that follows its docker build and leave a multi-gigabyte
  checkpoint in the tree. The SKIP grep now shares the recipe line with the
  test and runs from a captured status, because a failing line stops the
  target and "it failed **and** it skipped" is two problems rather than one.
- **Code written, covered and connected to nothing was found eight times by
  accident, and the ninth was found by a detector.** This document counted
  them: `queue.Cost`, the unrecorded preview frame, `sdserver.Client.Cancel`,
  `sortedKeys`, `blob.Delete`, `RateFromFloat`, `wireErrorEnvelope` and
  `node.Offer.Interruptible`. Two were money defects and one was an
  operator-facing error path; **none was found by a test**, because in every
  case the tests passed -- the suite and the code agreed with each other and
  neither was connected to the program. `unused` is enabled and found one of
  the eight. It cannot find the rest: a test caller is a caller to the
  compiler, golangci-lint v2.15.0 exposes none of staticcheck's
  tests-aware settings (`whole-program` and `check-exported` are refused by
  the config schema), and the only lever -- the global `run.tests: false` --
  stops every linter looking at test files *and* still reports only
  unexported identifiers in a non-main package, which five of the eight were
  not. So `cmd/check-coverage`'s `TestNoProductionDeadCode` loads the tree
  through `go/packages` and fails on a function with no production caller. It
  is a test and not a subcommand of the binary it lives in, because the guard
  whose subject is "nobody looked" must not need somebody to invoke it. The
  hard part is types: a method satisfying an interface something uses is used
  even where nothing names it, and the version that could not see that
  reported a hundred and thirty-six fakes -- not a noisy detector but one
  somebody deletes in the first week. `Recording*` fakes are classified by
  file for the same reason, and a reference *from* a fake is a test
  reference, which is how `genstore.JobRun.Sampling` surfaced. The baseline
  it landed on is thirty-two symbols, each named with the reason it was not
  deleted, printed on every run and allowed only to shrink: an entry the
  guard stops reporting fails, so a waiver cannot outlive the function it
  waived -- the `imagelabs` typo reached from the other direction. Fields and
  types are out of scope and that is stated rather than silent, so
  `node.Offer.Interruptible` would still not be caught: a wire field is
  written by reflection, and a field-level version would report every one of
  them. The ninth instance was `Queue.Out` and its accessor -- a writer
  `ember serve` set, read by two tests, carrying nothing for the life of the
  field. The thirty-third is now a build failure rather than a lucky catch.
- **Every provider is held to `providertest.RunConformance`, because the
  guarantees are not in the interface.** `provider.Provider` is six methods
  and says nothing about Destroy being idempotent, about a label surviving
  `Create` → `List` → `ParseLabel`, about `Inspect` on a handle the provider
  does not have, or about `Caps` matching fact rather than intent -- and every
  one of those is a defect this repository has already paid for and was
  re-proving by hand, per provider. The suite is credential-free and
  network-free by construction: it only calls the provider handed to it, so
  runpod runs it against `runpodfake` over `httptest` and local against the
  recording Docker engine, and nothing in the suite knows which. Its own
  checks are `func(ctx, Provider) error` rather than assertions on a `*T`, so
  `conformance_test.go` can run each one against a provider built to be
  correct and then against a provider that breaks exactly that invariant and
  nothing else, and require the matching check to go red: an assertion suite is
  the easiest kind of code to write so that it cannot fail. Both providers
  passed it unchanged on the first run, which on its own proves nothing --
  what proves something is that reinstating sixteen defects in their real code
  turned fifteen of them red.
  **The exposure decision is not reimplemented, it is called**:
  `RunConformance` applies the same `AssertSSHOnly`-or-`AssertNoPublishedPorts`
  choice `provision` makes from the same `Caps` field, because a provider
  checking its own exposure is a provider grading its own homework and a
  second copy of the rule is a second place for it to drift.
  **Nothing in it skips.** A check that does not apply is driven by `Caps` and
  asserts the capability claim instead, which is why the leftover check's
  "this provider has nothing to sweep" branch fails a provider that implements
  `LeftoverAPI` without the flag.
  **Two invariants are deliberately left out, and that is recorded in code
  rather than in silence.** A provider's int port being range-checked rather
  than narrowed is not observable through the interface -- `node.Endpoint.Port`
  is already a `uint16`, so an assertion there is vacuous and the real check
  stays beside `runpod.port16`. And a `Destroy` that re-derives the run id
  from the machine instead of taking the argument only goes wrong once the
  machine is gone, which means staging it behind the provider's back: the only
  route is a hook per fake, which is the escape hatch that would stop the
  suite holding a provider to its interface at all. So that one lives in
  `local`'s own `TestDestroy_RemovesTheNetworkWhenTheContainerIsAlreadyGone`,
  and `TestConformance_CannotCatchARederivedRunID` asserts that the suite
  still cannot see it -- a limitation nobody can read out of the code is a
  limitation that will be forgotten.
- **`httptest.NewRecorder` reads the live header map, so a recorder cannot
  assert a declared content type.** `net/http` snapshots the headers when
  `WriteHeader` runs and `ResponseRecorder.Header()` returns the map rather
  than that snapshot, so a recorder reports whatever the handler set whenever
  it set it. `Server.ok` wrote the status line and *then* called `write`,
  which set `Content-Type` -- discarded -- so every 200 `ember serve` has ever
  sent went out as `http.DetectContentType`'s guess,
  `text/plain; charset=utf-8` over a JSON body, while every **error** was
  declared correctly because `fail` happened to set the header first: the
  declaration was exactly inverted from what the code read as, and
  `postRequest`'s 202 and `postInput`'s 201 had the same slip. Nothing in the
  program was in a position to notice -- `web/src/api.rs` reads the body as
  text and parses it unconditionally, with no `resp.json()` and no
  content-type check anywhere in the page, so the seam's data half was right
  and only its declared type was wrong.
  **The gate was the `AssertNoStray` failure in a header.**
  `TestImage_ServedWithItsTypeDeclaredAndNotSniffed` was named for exactly
  this property and passed with it destroyed: with `getImage`'s two `Set`
  calls moved to after `http.ServeContent`, the recorder still reported
  `image/png` and `nosniff` while the wire had lost `X-Content-Type-Options`
  entirely -- Go sniffs `\x89PNG` to the same content type, so only the header
  that cannot be guessed went missing, which is the half that matters.
  Measured on this tree rather than argued: the same defect under the same
  assertions is GREEN through a recorder and RED over a socket, and deleting
  the header outright turns the recorder red, so the green is blindness to
  *ordering* and not a dead assertion. So `internal/webapi/headers_test.go`
  owns a real listener and every assertion about a header `net/http` could
  have discarded -- including the three that used to sit on a recorder in
  `TestStatic_RefusesAPageLoadReadably` -- and the rest of the package stays
  on the recorder, because a decision about a request body, a status code or a
  store call is the same decision either way. The line is "does this assertion
  depend on *when* a header was set".
- **A gate's tag list is derived from the tree, never maintained beside it.**
  `.golangci.yml` named `integration` and not `imagelab` while `go vet` ran
  with both from three hand-written copies, so the image labs -- the ones
  that stop a bad workload image reaching a registry, and the least
  frequently executed code here -- were linted **nowhere** for their whole
  life. The config's comment already made the right argument ("without it the
  tagged tree is linted nowhere") and then named one tag, which is the
  journal-subdirectory failure reached through a config file: a comment that
  justifies the entry somebody remembered cannot catch the entry somebody
  forgets. What had accumulated in there was an unchecked error and a helper
  whose only callers were deleted in the commit that introduced it -- both
  harmless, which is the point, because nothing would have reported a
  dangerous one either. The vet copies are one `LAB_TAGS` in the Makefile
  now, which the workflows *ask* make for rather than spelling, the idiom
  `workload-image.yml` already uses for the image tag; the YAML list has to
  stay a list, because CI lints through `golangci-lint-action` and the action
  reads that file and never runs make. So the two remaining lists are checked
  against the tree instead of against each other:
  `TestEveryBuildTagReachesTheGates` walks it, reads the constraints with
  `go/build/constraint` -- parsed, because `go list` resolves through a build
  context and so cannot see the one tree this is about -- and asks `go/build`
  itself which tags the toolchain already satisfies rather than guessing
  `unix` from its spelling, which is `ParseBind`'s rule one layer up. It
  fails on a tag missing from either list **and** on a list naming a tag no
  file uses, because `imagelabs` in both lists would otherwise satisfy every
  assertion while linting nothing. Checked by reinstating the gap, by
  inventing a third tag the test had never heard of, and by pasting a
  hand-written `-tags` back into CI.
- **And `PUBLISH_LABS` was exactly that list, cited by three comments here as
  the precedent against being one.** The gate between a built workload image
  and `docker push` selected its labs with a hand-written alternation of five
  test names, checked against nothing -- and `go test -run` over an expression
  that matches fewer tests than intended prints at most a warning and **exits
  0**. Measured: `make test-image-labs-publish
  PUBLISH_LABS=TestWorkloadImage_ThisNameDoesNotExist` exited 0 having run
  none of the five assertions, and the realistic case is worse because it is
  silent -- one ordinary rename ran four of five and `go test` said nothing at
  all. This is **the second time this one gate has been found unable to
  fail**: `pipefail` fixed "a lab failed and make said 0" and cannot touch "a
  lab was never selected and make said 0", which has the same consequence and
  no symptom. The irony is the whole lesson -- two copies of the list became
  one copy, which is the fix the three comments cite, and nobody asked what
  checked the survivor.

  Both halves are needed and neither is the other's belt.
  `TestPublishGateNamesEveryWorkloadImageLab` reads the Makefile variable and
  compares it against the tree in both directions: a name the list has and no
  test declares, which is the rename and the typo, and a `TestWorkloadImage_`
  the list does not name, which is how the image that lacked `embernode fetch`
  shipped in the first place. It cannot see a `PUBLISH_LABS` overridden on the
  command line, which is how this was measured, so the recipe counts what
  `go test -list` selected against `$(words $(PUBLISH_LABS))` -- which is why
  the variable is a list of names and not an alternation, and why the `-run`
  expression is derived from it rather than written beside it. The two also
  cover each other: the Go test cannot see a broken derivation and the count
  cannot see a lab nobody listed. All four lab targets get the count, because
  `TestWorkloadImage_`, `TestPreview_` and `TestServe_` are prefixes that a
  single rename cannot empty and a prefix change can -- there the minimum is
  one, which is the honest bound a prefix allows.

  `ShipsTheServerItAlreadyCompiles` was outside the gate and is in it now, by
  the sentence already written for `LinksNothingItDoesNotShip`: free, needing
  no image, and about the artifact being published. Checked by reinstating all
  four defects and judging exit codes -- a list naming nothing (make exits 2
  before touching Docker), one rename (five selected against six named, exit
  2), a renamed lab against the Go guard (exit 1, both directions at once), a
  stale exclusion (exit 1) -- and, because belt and braces reads exactly like
  a dead gate, by deleting the guard with the rename still in place and
  watching `go test ./cmd/check-coverage/` go green, which is what proves
  nothing else in the tree was reporting it.
- **"Document CLI flag, config key and behaviour changes in `README.md`" names
  three things, one of them was read out of the tree, and the one that could
  have been and was not had drifted by seven.**
  `TestREADME_DocumentsEveryConfigKey` reads `internal/config`'s `yaml:` tags
  and `TestREADME_NamesEveryVerb` reads `commands()` -- a verb is not one of
  the three, but it is the same shape of claim. Flags were left to the
  sentence, so `--max-hourly-usd` -- the money ceiling, on `up` **and**
  `offers`, and the one this file leans on by name for replacing a configured
  value rather than narrowing it -- appeared nowhere in `README.md`, along with
  `--cmd`, `--as`, `--vcpu`, `--only`, `--from` and `--archs`. The
  `imagelabs` argument one axis over: a rule maintained in prose is the
  hand-maintained list with no list.
  `TestREADME_DocumentsEveryFlag` reads every flag out of `internal/cli` with
  `go/parser`, and the receiver is what makes it a parse rather than a grep --
  a call counts only when the identifier it is called on was bound to a
  `*flag.FlagSet` in that function, by `flag.NewFlagSet` or as a parameter,
  because `grep 'fs\.String'` over this package also returns `p.Min.String()`
  and `e.Cost.String()`, and `cmd_images.go` shadows its own `io/fs` import
  with the FlagSet so a guard keyed on the spelling `fs` would be right by
  luck. A method called on a FlagSet and named in neither
  `flagSetDeclarators` nor `flagSetOthers` is a **fatal**, and
  `TestFlagTablesClassifyEveryFlagSetMethod` holds both lists to
  `reflect.TypeOf(&flag.FlagSet{})` rather than to each other: the alternative
  to failing on a method it cannot classify is reading straight past a flag,
  which is one more gate that cannot fail in a repository that has already
  found four.
  **A mention is a whole token and the config-key guard's substring is
  deliberately not copied.** A key is long and distinctive; `--as`, `--by` and
  `--from` are two to four characters, and a substring would count `--as` as
  documented the day somebody writes up a `--assets` -- a guard whose cheapest
  false pass is on its shortest name goes wrong first where nobody looks.
  One dash or two, because `flag` takes both and `README.md` writes `-image`
  in one place and `--image` in another; measured over the document, the
  single-dash spelling adds that one mention and no false pass.
  **The stale direction is bounded by a backtick, and the bound was
  measured.** It reads the flags `README.md` types at ember -- from the word
  `ember` to the end of the line or the next backtick -- because a renamed
  flag is caught by the forward direction the moment the new name appears
  nowhere, while a *removed* one leaves the document describing something
  nobody can type and nothing would fire. Reading to the end of the line
  instead condemns `--tags`, `--always` and `--abbrev` out of the
  `git describe` span sitting beside `ember version` on one line; reading
  every backticked span that merely names ember is worse rather than better
  and picks `--listen-ip` out of an sd-server argv in a fenced block; and one
  dash would condemn the `-c` in `grep -c opentelemetry` and `jq -c`. What it
  gives up is a flag documented only in prose, which the forward direction
  covers. It found one on its first run: a `--all` on an `ember ls`, in
  `README.md` and four times in **this file**, which has never been a flag --
  `ember ls` has had exactly one, `--config`, and has asked every configured
  provider since it was written. Both documents are corrected; a guard whose
  first run finds nothing is a guard nobody should trust.
  **Behaviour, the third thing the line names, is still on the sentence
  alone**, and is not an omission: a behaviour is not a token anything can
  enumerate out of the tree, so a guard for it would either be a marked table
  of claims maintained by hand -- the thing all three of these exist not to be
  -- or a grep whose only honest answer is "something changed somewhere".
- **That sweep corrected the two documents and left eighteen Go comments
  citing the flag, because the guard looks at documents.** `ember ls --all`
  survived in `internal/app`, `internal/cli`, `internal/genstore`,
  `internal/provision`, `internal/queue`, `internal/reconcile` and
  `internal/webapi`, every one of them as the authority for a decision that
  is otherwise correct -- which is the worst combination for a reader: the
  argument persuades and the command does not exist, so an operator who tries
  it gets a usage error from the tool the comment told them to trust. Same
  shape as `node-image.yml` publishing a moving tag for the life of the image
  *underneath* a comment arguing against one, and as the config comment that
  justified the build tag somebody remembered.
  **The guard was expected not to be worth building, and the measurement said
  otherwise.** The prior was that comments are a worse input than `README.md`:
  a much larger and less curated surface that legitimately names
  `docker run --memory`, `sd-cli --list-devices`, `nvidia-smi --query-gpu` and
  `git describe --tags`, so the honest answer looked like "no guard, and the
  reason written down" -- the call taken for a rule that could not be
  expressed cheaply. What makes it cheap is that the rule is **not new**:
  `emberFlags` is the window already settled on after three attempts, and
  `declaredFlags` is already the authority. Run over every comment in the tree
  as it stood it reports **48 flags cited at ember, 30 of them declared, and
  18 misses -- every miss `--all`**, with not one false positive; after the
  sweep it is 30, 30 and none. It reaches none of those four other programs'
  flags -- none has the word `ember` between the start of its line and its own
  backticked span. So
  it is a high-precision, low-recall guard: what it gives up is a flag the
  word `ember` does not reach on its own line, which is the same backtick
  bound that buys the precision. **One direction only**, unlike the two guards
  above it, because a comment is not documentation and "a flag no comment
  mentions" would be a test demanding prose.
  **The one citation left spelled out is the one in the guard's own file, and
  it was re-worded rather than exempted.** `readme_test.go` records what the
  README guard found on its first run, and both documents already write that
  record as two spans -- "a `--all` on an `ember ls`" -- so the comment now
  matches them. An exemption would have been an escape hatch for a seal, which
  is refused here even for a test.
  **The decision not taken, written down so it is not re-litigated:** nothing
  checks that a comment's *reasoning* is sound, only that the command it cites
  exists. Each of the eighteen was correct about the behaviour -- a provider
  that could not be reached is a finding and `ember ls` exits non-zero over
  one -- and no guard can tell that from an argument that is wrong.
- **The first round of a reinstatement pass is reliably incomplete, and a
  flawless one is a finding about the harness rather than about the change.**
  Measured across one session of eleven changes here: every pass that reported
  a result had uncaught cases on its first attempt, between one and six each,
  and chasing them produced a dead condition in a seal, an unreachable error
  branch, an untested zero-time guard and a security walk that could be
  laundered around. That is a claim about the method and not a tally to keep
  up to date -- what it licenses is the expectation, which is that a pass
  going red on every defect at the first attempt has more likely proved
  something about its own thoroughness than about the suite. The question
  that catches most of it, asked of each assertion before the suite is
  believed: **what would this say if the bug were present?** If the answer is
  "the same thing", it is not an assertion. The two shapes below are both the
  *test* being wrong rather than the code, and both were found more than once.
- **The expected value is what the bug produces.** Three in one session, each
  passing against the defect and against the fix.
  `TestHistoryRm_OlderThanReachesEveryRequestAndNotOnlyAPage` seeded 201
  requests with the newest one a day back, which a 24h horizon correctly
  excludes -- so its expected count was 200, exactly what a prune bounded by
  `genstore.defaultLimit` returns. The usage-alignment assertion in
  `TestCatalogue_IsInTheDispatchTable` named `catalogue`, the nine-character
  verb: that line reads identically at `%-8s` and at the width taken from the
  longest name, and the lines that move are every *other* one, so it asserts
  on `up` now. And `TestGPUSamples_APriceIsRoundedAndNotTruncated` used a
  price out of the captured RunPod catalogue, every one of which survives
  `int64(f * 1e6)` -- so it could not tell `node.RateFromUSD` from a
  multiplication, and uses $2.01, which truncates to 2009999.

  **Two more, in a pass whose other eleven defects all went red, and both were
  found by the reinstatement rather than by review.**
  `TestSubmit_TakesASubmittedRunBeforeAClaim` asserts the ORDER the drain loop
  takes two kinds of work in, and only the submitted run recorded into the
  channel it compares -- so the channel held exactly one value whichever order
  the loop chose, and the assertion passed against a loop that reads the
  handoff only when idle, which is the one defect it exists for. Both sides
  write the sequence now. And
  `TestFollowRun_CtrlCDetachesAndSaysWhatThatCosts` looked for "it destroys
  itself at" over the whole of what the client printed -- which `clientSays`
  also prints the moment the node is ready, sixteen lines earlier -- so taking
  the deadline out of the detach notice was GREEN. It reads the span after
  "detached from" now, which is the "a marker matching a second, unpatched
  line" trap in an assertion instead of in a harness.
- **The hand-built fixture is missing the row the bug reads.**
  `TestOpen_MigratesAVersionFourFile` built a version-4 database statement by
  statement and left out the cached `caps` row that `PutCaps` writes on every
  rental -- so the reinstated backfill-from-cache defect found nothing to
  read, wrote the empty string, and the assertion passed. It seeds the row now
  precisely because a backfill written later must have something to find.
  `make tidy-check` is the same shape from the other side: it runs
  `go mod tidy` and then `git diff --exit-code go.mod go.sum`, so an untidy
  `go.mod` in the **working tree** is repaired by the tidy and leaves an empty
  diff and a passing check. The untidy state has to be in the **index**, which
  is what a CI checkout gives it.
- **A guard that walks from the repository root reads every checkout nested
  inside it, and agent worktrees put four of them there.** Claude Code cuts a
  worktree at `.claude/worktrees/<name>/` -- *inside* the checkout it was cut
  from, a whole second copy of the tree -- so
  `TestComments_CiteNoFlagThatNoFlagSetDeclares` reported **457 findings on
  master at 927ef78**, every one a correct complaint about a file in somebody
  else's worktree, because those checkouts predate the sweep that removed a
  `--all` from an `ember ls`. The guard was working; it was reading the wrong
  tree. CI never sees it, because CI checks out fresh. This is `LAB_LOG_DIR`'s defect
  through a tree walk instead of a log path -- an assertion reading the wrong
  file -- and the daemon-wide `Reap`'s root cause: safe while this repository
  was one checkout, unsafe the moment there were four.

  **Measured rather than assumed, with one nested checkout holding a stale
  `--all` citation, an `InsecureIgnoreHostKey` and a tag nothing here uses.**
  Exposed: that guard, `TestNoInsecureIgnoreHostKey` and
  `TestEveryBuildTagReachesTheGates`, each exit 1 naming a path under
  `.claude/`. Immune and left alone: `TestNoListenOutsideLoopback` and
  `internal/logx`'s `walkModule`, because both filter to the `internal/`,
  `cmd/` and `test/` prefixes before reading anything and a nested path has
  none of them; `TestNothingRentsOutsideInternalApp`, the README guards,
  `TestNoWorkflowPublishesAMovingLatestTag` and
  `TestPublishGateNamesEveryWorkloadImageLab`, which are handed one
  directory; and `TestNoProductionDeadCode`, `go vet ./...`,
  `go build ./...` and `golangci-lint run ./...`, which resolve through the
  module and a nested checkout has a `go.mod` of its own. Recorded because
  two of those are immune by a prefix allow-list rather than by the rule
  below, and an allow-list widened later takes the exposure back.

  **`.golangci.yml` is clean and `gofmt` was the fourth site -- the only one
  that WRITES.** `make fmt` was `gofmt -l -w .`, and CI's gate was
  `gofmt -l .`: measured, with one unformatted file in a nested checkout,
  `gofmt -l .` named it and `make fmt` rewrote another developer's working
  tree. `gofmt` has no exclusion flag and no notion of a module, so it cannot
  be taught the prune -- it has to be handed paths, which makes the list
  exactly what `LAB_TAGS` and `PUBLISH_LABS` exist not to be unless something
  checks it. `GOFMT_PATHS` is that list and
  `TestGofmtReachesEveryGoFileInThisCheckout` checks it against the tree in
  both directions: a `.go` file under none of the paths, which is how a whole
  top-level directory arrives unformatted and unreported, and a listed path
  with no Go source, which is how `interna` beside `internal` passes the
  first direction while formatting nothing. `TestCIGofmtStepAsksMakeForThePaths`
  stops the workflow growing a second copy, which is
  `TestNoWorkflowSpellsItsOwnVetTags` one tool over -- and **its first
  version reported `echo "::error::not gofmt'd:"`, CI's own error message**,
  which is `TestNoListenOutsideLoopback`'s first version flagging
  `loopback.Listen` for the third time in this file. The matcher bounds the
  word by the character after it and has a table of its own.

  **The prune is `internal/repotree.Nested` and not the name `.claude`.** A
  nested checkout is recognisable by what it IS -- a directory holding its own
  `.git`, a file for a worktree and a directory for a clone, which `os.Stat`
  does not distinguish -- so a stray clone under `vendor/`, a `cp -a` copy and
  the next tool's own directory are all covered, while a name catches only the
  one somebody remembered. `go list` was the alternative and is refused for
  all three guards by an argument already in the tree:
  `TestEveryBuildTagReachesTheGates`'s own comment says `go list` resolves
  through a build context, so the `integration && imagelab` files -- the ones
  these guards most need to read -- are invisible to it. **A package and not
  three copies of four lines, because the copies have no guard available**:
  "every whole-tree walk prunes" is not derivable from the tree, since telling
  a walk of the module root from `internal/app`'s walk of a temp output
  directory needs to know what the root argument evaluates to, and a check
  keyed on the spelling `filepath.WalkDir` would fire on both. One function
  with one test is the only shape whose correctness anything can assert; it
  is in `testSupport` for `providertest`'s reason, since the callers are in
  three different packages and so it cannot be a `_test.go` file.

  **A prune that is too wide is worse than the bug, so every guard that asks
  carries a floor.** `TestNoInsecureIgnoreHostKey` had none -- a walk that
  visited nothing passed -- and has a file count now. Reinstated by making
  `Nested` answer true unconditionally: `internal/repotree` goes red, and so
  do all four guards, each at its own floor ("parsed only 0 Go files"). The
  root comparison is the same shape and the real callers prove it matters,
  because every walk root here is itself a worktree root with a `.git` file:
  deleting it prunes the first directory of every walk and the same four go
  red. The error direction is pinned too -- a stat that fails for any reason
  other than absence reads as this tree's own, because answering "nested"
  there is a silent dead gate and answering "ours" is a failure that names the
  path. Negative controls, which are the half that matters: a real stale
  `--all` citation, a real `InsecureIgnoreHostKey`, a real new build tag and a
  real `.go` file outside `GOFMT_PATHS` each still turn their guard red with
  the nested checkout present, and a no-op reword of the prune's comment stays
  green. And the gofmt guard was deleted with its defect still in place, which
  leaves `go test ./...` at exit 0 -- nothing else in the tree reports it.

  **One stated blind spot, and the comment that used to justify it was wrong
  about `gofmt` rather than about the skip.** The gofmt guard does not descend
  into a dot-named directory, and the reason written beside it was "gofmt
  skips a directory whose name begins with a dot or an underscore" -- false,
  measured: `gofmt -l ./internal` NAMES `internal/.hid/x.go` and
  `internal/_us/x.go` planted unformatted and does not name
  `internal/cli/.hidden.go`, so gofmt's rule is about a FILE's name and not a
  directory's. The skip is sound for a different reason -- it is "do not claim
  coverage of what gofmt is not handed" -- and it changes nothing *under* a
  listed path, because the component the guard compares is the first one. At
  the TOP level it is a real hole: a `.tools/gen.go` is handed to gofmt by
  nothing and reported by nothing, measured at exit 0 through the guard,
  `go test ./...` and `golangci-lint run` alike, where the old `gofmt -l .`
  would have named it. Accepted rather than closed, because `go build ./...`
  and `go list ./...` cannot see it either -- a top-level dot directory is not
  module source -- and the one that exists here is `.claude`, whose nested
  checkouts must not be formatted. The underscore case has no hole and is not
  skipped: a top-level `_tools/gen.go` turns the guard red. Recorded at this
  length because the next reader will otherwise re-derive the wrong reason the
  same way, and this file's own rule is that a comment wrong about why a seal
  holds is how the seal gets removed by somebody being helpful.

  **`internal/logx`'s `walkModule` is the fifth walk and got no floor, which
  is a measurement rather than a shrug.** It looked like the one place the
  warning above has no exit code behind it. It is not: all three of its
  callers already count the files the callback saw and fail on zero, so a
  narrowed or broken allow-list is loud -- measured, with the prefix list
  pointed at a directory that does not exist, all three go red at "inspected
  no files". A floor inside `walkModule` would be a second mechanism for a
  property its callers already hold, which is `queue.Cost`'s mistake and the
  same reason `repotree.Nested` is not added there. And the *widening*
  direction the warning is actually about does not produce a dead gate at all:
  with `.claude` added to that allow-list and a `slog.NewTextHandler` planted
  in a nested checkout, `TestNoHandlerOutsideLogx` goes red naming the nested
  path -- so what a future widener needs is the prune, which is what the
  warning should be read as asking for.
- **A MERGE PROBE GREPPED FOR CONFLICT MARKERS AND REPORTED A CLEAN MERGE OVER
  TWELVE CONFLICTED FILES, and the command it asked is one whose exit code
  counts nothing either.** The probe was the three-argument
  `merge-tree <base> <ours> <theirs>` piped into `grep -c '^<<<<<<<'`, run to
  decide whether bringing master into a branch needed authorisation, and it
  answered **0**. Measured on the pair it was run against: that form spells a
  marker `+<<<<<<< .our`, with a leading `+` from its diff-like output, so the
  anchored pattern matches **none of the 60** that are there -- and it exits
  **0** over a merge that conflicts in twelve files, so "judge it on the exit
  code" fails here as well, which is the one qualification this file's own
  favourite rule has collected. **A matcher anchored wrong reports exactly
  what a clean run reports**, which is `TestCIGofmtStepAsksMakeForThePaths`'s
  first version firing on CI's own error message, and
  `AssertNoStray`-could-not-fail reached through a probe rather than through a
  cleanup order.
  **The modern two-argument form is the answer and a marker grep cannot read
  that one either**, which is why the rule is about the question rather than
  about the pattern: two arguments and no base exits **1** and names every
  conflicted path with its kind -- `CONFLICT (add/add)`, `CONFLICT (content)`
  -- and contains **no marker at all**, so the same grep answers 0 against the
  correct invocation too. **Ask a trial merge whether it conflicted; never ask
  its output what it looks like.**
  **No guard, because there is nothing committed to guard.** It was one
  ad-hoc command in a session rather than a gate in the tree, so what is
  recorded is the shape and not a test -- and the shape is the half worth
  keeping, because the conclusion drawn from the false 0 (surface the
  divergence rather than resolve it) happened to be the right one, so nothing
  about the outcome said the evidence was wrong. **A probe that is wrong and
  agrees with you is the hardest kind to notice**, and what found this one was
  re-running it afterwards and asking why a clean answer had been followed by
  twelve staged resolutions.
- **The seven committed configurations were 71% comment, and the guard that
  stopped one of them rotting was keyed on a directory (#144).** They were
  1,158 lines of which 329 were configuration -- `ember.code-model.example.yaml`
  was 85% prose -- so one file was doing two jobs: the project's documentation
  of record *and* an alternate config, which is the profile/job conflation #144
  complains about, one level up. The prose is `docs/configurations.md` now and
  the five surviving files hold configuration plus a line or two about what a
  wrong value costs. **Measured rather than claimed: the non-comment lines of
  all five are byte-identical to what was committed**, so nothing an operator
  runs changed, and the five sum to 540 lines against 1,158.
  **The two files that moved OUT of the root are the ones that cannot be
  run**, which is the test of "is this an alternate configuration or a worked
  example": `video`'s asset digest is `REPLACE_WITH_THE_DIGEST_PUSH_PRINTS`
  and `code-model`'s `image:` is a sentence, both deliberately, so both refuse
  before anything is rented. They are ```yaml fences in that document, and
  their settings were diffed as a sorted set against the deleted files rather
  than eyeballed.
  **A fenced block is loaded by nothing, so the move would have cost the
  `KnownFields` check over 85 lines of hand-written YAML -- and the rule the
  guard needed was never a directory.** It is "every ember configuration this
  repository commits", which is derivable: a ```yaml fence under `docs/` is
  one. That is the `LAB_TAGS` rule rather than a second hand-maintained list,
  and it costs the converse -- a yaml fence under `docs/` that is NOT an ember
  config fails `TestExampleConfigs_LoadAndParse`, which is the signal to retag
  it. `docs/long-video-is-a-pipeline.md`'s `cmds` chain is why that is a
  feature: it is a real, small, loadable config and it is now checked for
  free.
  **BOTH FLOORS ARE OVER THE ANSWER AND NOT OVER AN INTERMEDIATE, and the
  first shape of the helper had that wrong.** `Load` answers `Default()` and a
  **nil** error for a file that is not there, so a source contributing nothing
  passes every assertion -- and a floor on a local slice that is then not
  returned is the same hole one line further down: it counted `fenced` and
  could have returned `paths` alone, green. `committedConfigs` counts over the
  list it hands back, telling the two apart by whether the path is one of the
  temp files the extractor wrote, so dropping **either** source is red.
  **AND A GLOB IS A PATTERN, SO A GLOB NARROWS: the first floor could be
  satisfied with the 85 lines it exists for UNREAD.** It asked for one fenced
  configuration, which `docs/long-video-is-a-pipeline.md` answers on its own,
  and `filepath.Glob(dir, "*.md")` is not recursive -- so **measured**, moving
  `docs/configurations.md` to `docs/reference/configurations.md` left the test
  at **exit 0** with 6 of 8 configurations loaded and both demoted blocks
  unread, and narrowing the same pattern to `l*.md` does it too. A file move
  silently undoing the one property the guard was built for. The `*.mdx`
  reinstatement could not see it, and that is the lesson rather than the
  defect: it **zeroed** the count, and a floor no single source can fail to
  satisfy is blind to a NARROWING while catching a BREAKING -- which is the
  `AssertNoStray`-could-not-fail shape reached through a floor instead of a
  cleanup order. It is `filepath.WalkDir` now, which closes the
  sub-directory hole and the pattern together because there is no pattern left
  but the `.md` suffix, and the floor is **at least two DOCUMENTS** rather than
  a census of blocks -- "no one document carries it", read off the answer,
  since the extractor names each file `<document>-line<N>.yaml`. Reinstated
  three ways, each red: the suffix test broken (nothing found), the walk
  narrowed to one document (the document floor), and the document floor
  relaxed back to one with the walk still narrowed (the two-configuration
  floor). And the sub-directory move is now **green**, which is the half that
  says the walk is doing the work.
  **AND THAT FLOOR COUNTS DOCUMENTS AND CAN NEVER NAME THE ONE THAT MATTERS,
  which is stated because "no one document carries it" reads as closed and is
  not.** Found by review attacking it rather than by the pass that built it,
  and measured both ways: on the tree as it stands an extractor skipping
  `configurations.md` by name is red -- but at the **two-configuration** floor
  rather than at this one, and only while `long-video-is-a-pipeline.md` is the
  sole other page with a fence. Plant a second `docs/` page carrying one and
  skip `configurations.md`: **exit 0, seven configurations loaded, zero of
  them from `configurations.md`**, both demoted blocks unread with every floor
  satisfied. Latent rather than present, and latent only by an accident of how
  many pages under `docs/` happen to hold YAML. **Not closed, and the trade is
  the reason**: the only fix is naming `configurations`, one hand-maintained
  filename, which is what the floor above refuses and what the `LAB_TAGS`
  argument calls worse than the gap -- and with `Glob` gone there is no
  pattern left to narrow by accident, so reaching it needs a deliberate skip
  of that one name rather than a plausible edit. So it is the
  `TestConformance_CannotCatchARederivedRunID` arrangement: the suite states
  what it cannot see, and whoever adds the second fenced page reads the
  comment and decides again.
  **THE RULE AS STATED IS WIDER THAN THE RULE AS IMPLEMENTED, and the
  difference is `README.md`.** "Every ember configuration this repository
  commits" is the sentence; "every ```yaml fence under `docs/`" is the code,
  and `README.md` holds **14** fences of its own, **12** of which would pass
  unchanged -- including the starter `# ember.yaml` block that is the first
  thing anybody copies. Written down beside the rule rather than left to be
  found, because an undocumented narrowing is the `imagelabs` shape this
  guard's own comment invokes against itself. Not widened with #144 for a
  stated reason: README's fences are partial by design, a two-line
  `cmd:`/`ports:` illustration is not a configuration, and deciding which of
  the 14 are would be the hand-maintained list this guard exists not to be.
  **`TestExampleConfigs_CanSyncTheirOwnStore` was NOT widened to the two
  blocks, and that narrowing loses nothing measurable**: `ember store sync` on
  the video one fails at `ParseAssets` over the placeholder digest and on the
  code-model one at `ValidateImage` over an untagged sentence, so the
  assertion was already resolving an image for a sync that could never happen.
  Its comment says so rather than keeping a subject it no longer has.
  **Nine reinstatements, each red, two negative controls green, controls at
  both ends, and every case ended in a diff against one backup taken once per
  path**: a typo in each demoted block, the walk's suffix test broken, the
  walk narrowed to one document, the document floor relaxed with the walk
  narrowed, the fenced source dropped from the return, the root source dropped
  from the return, a foreign `kind: Deployment` fence, and an unclosed fence.
  The negative control's first anchor missed and the harness reported
  ANCHOR-MISS rather than a verdict, which is that trap working. And -- because
  belt and braces reads exactly like a dead gate -- the guard was reverted to
  its root-only shape with a typo still in a demoted block: `go test ./...` is
  **exit 0**, so nothing else in the tree reports it.
  **One thing has no exit code and is named rather than papered over: nothing
  checks that a relative link in `README.md` or `docs/` resolves.** The sweep
  found `ember up --config ember.local.yaml` in `README.md` -- a file name that
  has never existed, the `ember ls --all` shape reached through a path instead
  of a flag -- and fixed it by hand. A guard is cheap and was not built here,
  because it is a change to the README guards' own file and this change was
  about the configurations; what stands in its place is that every relative
  link in both documents was checked once, by script, and resolves.
  **The scope call stands and the "cheap" is now a measurement rather than an
  estimate: review wrote that guard in about thirty lines of shell over
  `README.md`, `AGENTS.md` and `docs/`, and its FIRST RUN found the
  `ember.local.yaml` defect.** A guard whose first run finds a real defect is
  this repository's own strongest argument for keeping one -- the same
  sentence `TestComments_CiteNoFlagThatNoFlagSetDeclares` earned when its
  first catch was the commit quoting it. So the thing to notice is not the
  decision but that the script existed and was thrown away twice: it is filed
  rather than re-derived a third time.
- **A page answered on every route could not load itself below the root, and a
  status code is what could not see it (#139 slice 1).** `ember serve` serves
  the app's HTML on any path whose last segment holds no dot -- which is what
  makes a reload on `/requests/{id}` the app rather than a 404 -- and
  `web/index.html` referenced `style.css` and `./ember.js` **relatively**. A
  browser resolves a relative reference against the DOCUMENT's own directory,
  so one page asked for `/style.css` on `/` and `/queue` and for
  `/requests/style.css` on `/requests/abc`: single-segment routes worked and
  everything deeper served a page whose every asset 404'd. The example
  `static.go`'s own comment names is the one that did not work. Measured
  before it was fixed, through the handler, and the fix is two tokens.
  **And it was silent, so the page's own error handling could not report it.**
  A module specifier that 404s throws while the module script is being
  LOADED, before the `init().catch` in that script's body is installed -- so
  the one line written to turn a failed start into a sentence never ran, and
  what arrived was a 200 with an empty body: indistinguishable from a server
  that is not answering, which is the failure that handler exists to prevent.
  **`TestStatic_AClientRenderedRouteGetsTheApp` is the trap and it is still
  correct.** It asserts the page arrives on `/requests/abc`, which it did
  throughout: a test about a status code and a `<title>` cannot see the step
  after. So the Go half resolves the page's own references with `net/url` --
  `ResolveReference`, the browser's rule rather than a hand-rolled join -- and
  asks the file server for each one at four depths, over a `Dist` the test
  writes from the committed `web/index.html` so the answer does not depend on
  whether anybody ran `make web`. **The references are PARSED OUT OF the file
  rather than listed beside it**, which is the `LAB_TAGS` rule one surface
  over: a list passes unchanged the day somebody adds a favicon relatively,
  and a reinstated `<link rel="icon" href="favicon.ico">` is red. Both floors
  are over the answer, because a regex that matched nothing would turn both
  tests green over an unread page -- so a page naming no stylesheet and a page
  naming no module each fail, reinstated separately. A regex and not a parse,
  stated where it is: the standard library has no HTML parser and
  `x/net/html` is a module this list would have to be amended for, over a
  thirty-line page this repository writes itself.
  **The browser half is what the Go half cannot claim**, and it is the mount
  rather than the document. `<h1>` and `.posture` are in none of the HTML the
  server sends -- the wasm renders both -- so either one present means the
  module loaded, `ember_bg.wasm` resolved beside it and the mount ran; and
  that blob is a reference no Go test can see, because wasm-bindgen's glue
  asks for `new URL('ember_bg.wasm', import.meta.url)` and so follows
  `ember.js` rather than the page. `/` is asserted FIRST and is the
  discriminator rather than padding: an app that mounts nowhere fails the same
  assertion in the same place, so without it the lab could not say the failure
  was about DEPTH.
  **THE STYLESHEET ASSERTION'S FIRST MECHANISM COULD NOT FAIL, AND THE
  REINSTATEMENT IS WHAT SAID SO.** It compared the page's
  `<link rel=stylesheet>` count against `document.styleSheets`, on the reading
  that a link whose fetch failed produces no CSSOM entry. Reinstated -- the
  href back to relative with the module left absolute, so the app mounts over
  a 404'd sheet -- that comparison was **GREEN**: measured, 1 link, 1 entry,
  and **0 parsed**, so Chrome keeps an entry for the link whatever the fetch
  answered. It counts `cssRules` now and is red on the same defect, with all
  three numbers in the message because "the link is not in the DOM" and "it is
  and it did nothing" are different repairs. The half-fix is why that
  assertion exists at all: making only the module absolute leaves the page
  unstyled and mounting perfectly, which is this entry's own status-code trap
  one layer down.
  **Thirteen reinstatements over three passes, each judged on an exit code,
  with a control at both ends of every pass, three negative controls green,
  and every case ending in a diff against one backup taken once per path.**
  Eleven red: the stylesheet relative, the module relative, both (which is
  master), a favicon added relatively, each floor broken separately, a
  relative link on the placeholder, and -- through the browser -- the module
  relative, both, and the lab's own no-stylesheet floor. One green that is a
  measurement rather than a gap: the stylesheet-only defect against the CSSOM
  mechanism above. **One pass per mechanism and a fresh backup directory for
  the second**, because `backup()` taking one copy per path is right inside a
  pass and is exactly wrong between two -- the shape that once shipped an
  entry here claiming a deletion the tree did not hold.
