# ember — conventions

This project is **Go-first**, like `go-tun`, `transcription` and `sms-gateway`.

- Go **1.27** for all application code and tooling. `go.mod` pins it; the
  Makefile prefers `~/sdk/go1.27.0` so the compiler cannot drift from the
  declaration just because a system package updated.
- Prefer the standard library and the dependencies already here over new ones.
  The list is `golang.org/x/crypto`, `github.com/docker/docker`,
  `gopkg.in/yaml.v3`, `golang.org/x/tools` (the coverage gate) and
  `modernc.org/sqlite`. **Adding one needs a reason written down**, which is
  what the rest of this bullet is.

  **This list and `go.mod`'s direct requirements are checked against each
  other, by `make tidy-check` and the CI step that calls it.** They had
  drifted: `go build` and `go get` add a requirement without correcting its
  `// indirect` marker, so `yaml.v3`, `coder/websocket`, `modernc.org/sqlite`
  and `chromedp` were each recorded as something nobody asked for, while three
  of the four are argued for at length below. What made that worth a gate
  rather than a tidy is that `TestOTelStaysIndirect` reads the marker as
  evidence -- a seal whose one signal was known to be stale four lines away in
  the same file. `chromedp` is direct through `TestServe_UI` alone, behind
  `integration && imagelab`: `go mod tidy` considers every build
  configuration, so a tag-gated test import is a direct requirement, and
  promoting it changed no package in either the plain or the tagged build
  graph.

  `modernc.org/sqlite` is the queue and the generation history. ember had no
  state that outlives a process: `internal/statestore` is a journal of *live*
  nodes, deliberately empty after a clean run, which is the right shape for
  "what is billing right now" and the wrong one for "what was asked for, what
  came back, and what did it cost". A queue outlives the process that filled
  it, a request has a history worth keeping after its node is gone, and a
  reconnecting client has to be told what it missed from a cursor -- which a
  ring buffer cannot answer once it has wrapped.

  Rust lives in `web/` and nothing in `internal/` imports anything from it.
  `go build ./...` and `go test ./...` work on a clean clone with no Rust
  installed, because `internal/webapi/dist` carries a committed placeholder --
  `//go:embed` fails at *compile* time when its pattern matches nothing, so an
  empty directory there would make the Go toolchain depend on the Rust one.
  `Cargo.lock` is committed and `wasm-bindgen` is pinned to exactly the
  installed CLI, because a mismatch between the generated glue and the binary
  that processes it is a hard error; `make web` checks the two agree and names
  which to change, the shape `require-arch` already uses. The built `ember.js`
  and `ember_bg.wasm` are **not** committed: a wasm blob changes on every
  build and nobody can review one.

  `github.com/coder/websocket` is the web UI's progress stream. It has **zero
  dependencies of its own**, which is most of the argument, and it refuses a
  cross-origin handshake by default -- so the one thing an attacker's page
  could do with the socket is the one thing it already cannot. The alternative
  was long-polling with the standard library, which works and was the first
  plan; a socket is one connection per watched request instead of four
  requests a second, and the latency floor stops being a poll interval.

  It is the **pure-Go** driver and not `mattn/go-sqlite3`, because cgo would
  mean `CGO_ENABLED=0` no longer builds ember and `go test ./...` would need a
  C toolchain, which is the kind of requirement that turns "it just works on a
  clean clone" into a paragraph of instructions. `internal/genstore` puts it
  behind a narrow interface with a `RecordingStore` fake, as every other
  external system here is, so the engine is swappable and the layers above it
  are testable with no file on disk. Flat JSON files -- what `statestore`
  does -- were the alternative and would have meant hand-rolling the one
  operation the queue cannot do without: claim the next request atomically,
  in a single statement.

  **OpenTelemetry is already linked, stays a no-op, and the SDK is not
  adopted.** Docker's client wraps its own transport in
  `otelhttp.NewTransport` unconditionally, so
  `internal/dockerengine/dockerclient` drags the otel API into every ember
  binary: **530** `opentelemetry` symbols in `bin/ember`, of which 155 are
  `otel/trace`, 70 `auto/sdk`, 49 `otelhttp` and 25 `otel/metric` -- and
  **0** from `otel/sdk`, 0 from `google.golang.org/grpc` and 0 from
  `go.opentelemetry.io/proto`. That zero is the whole decision: the API with
  no registered provider does nothing, so the conversation was never "add a
  dependency". `internal/logx`'s `TestNoTelemetrySDKRegistered` and
  `TestOTelStaysIndirect` keep it there, because a state that holds only
  while nobody writes one import is not a state anybody chose.

  Three builds of ember's own `main`, go1.27, same machine, differing only in
  what they register -- against *ember's* dependency set and not a trivial
  program's, because what is already linked is what makes the delta smaller
  than it looks:

  | build | binary | delta | new modules | gRPC symbols |
  |---|---|---|---|---|
  | ember as it ships | 22,834,226 | — | — | 0 |
  | + SDK, hand-written JSON-lines file exporter | 23,396,778 | **+0.54 MB, +2.5%** | **1** | 0 |
  | + SDK + `otlptracehttp` | 33,638,911 | **+10.3 MB, +47.3%** | **11** | 1,922 |

  The **local sink is nearly free and the OTLP one costs half the binary**,
  which is the split #33 predicted -- but *both* of its numbers came from a
  trivial program and are overstated, and its module claim is wrong in the
  direction that matters. "Promoting them adds no new module and no new
  `go.sum` entry" is true of `otel`, `otel/trace` and `otel/metric`, and false
  of `otlptracehttp`: `go mod why` lands on
  `github.com/docker/docker/client.test`, so it is pinned by somebody else's
  **test** binary, contributes 0 symbols to ember, and its own requirements
  were never in a build list -- `grpc`, `protobuf`, `proto/otlp`,
  `grpc-gateway/v2`, `genproto` twice, `x/net`, `x/text`, `cenkalti/backoff`
  and `otlptrace` all arrive with it. A module pinned by a dependency's test
  binary is not a module ember has already paid for. **A schema read in place
  of a response, one more time** -- the `allowedCudaVersions` and
  `capabilities.model` mistake, reached through a dependency graph instead of
  an API. (New-module counts are against `go mod tidy` on this tree rather
  than against the committed `go.mod`, which is not tidy; `go.sum` is left out
  of the table because every line either exporter adds is a test-only hash and
  the figure measures nothing.)

  **"Docker API spans for free" is true and worth about 0.2% of the question
  it is offered against.** `grep -rn 'ImagePull'` returns nothing: ember never
  pulls an image through the Docker API, so no span would cover the pull. The
  whole wrapped surface is 15 control-plane methods, measured over this
  daemon's unix socket at 200 calls each -- `/_ping` 68 µs mean,
  `/v1.45/info` 4.42 ms, `/v1.45/containers/json?all=1` 5.45 ms. One of
  ember's five outbound HTTP surfaces is instrumented; RunPod's API, S3, the
  asset fetch and sd-server all go through `internal/httpx`, which sets no
  `Transport` and is wrapped by nothing. So the free spans are tens of
  milliseconds of control plane out of the ~27 s of per-rental overhead they
  were offered to explain, and none of the expensive half.

  **Metrics, no, and that one is not close.** `ember spend` and
  `ember history` answer it out of SQLite over integer-microdollar columns, and
  the rule above already says the event stream is the truth and those columns
  a derived index. A metrics pipeline would be a *third* copy of numbers that
  have a source of truth, aggregated and sampled by design, in float64 where
  this program keeps money as `int64` -- which is `queue.Cost` at a larger
  scale, and `queue.Cost` had never been called by anything but its own test.

  **Traces, not yet, and the measurement that decides it is #35's.** What was
  wanted is where the per-rental overhead goes -- create, wait-for-ready,
  pull, store read, asset verify, model load -- and a span tree is genuinely
  the better *shape* for it. But a shape is only worth something to a viewer,
  and the sink that costs +2.5% has no viewer: a JSON-lines file of spans is
  read with `jq`, which is not better than reading `took=` off logfmt. The
  thing with a viewer is the +47% one. Meanwhile `--log-level debug` and the
  `run` key now exist, so a `took=` on a debug line at each of those stages
  *is* the measurement, filterable to one rental, for no bytes, no new module
  and no egress. The honest order is: take that measurement first, and come
  back for spans when a question survives it.

  **The egress rule, settled once for #33, #34 and #35: a telemetry
  destination is an allow-list, and the allow-list is loopback.** ember's
  posture sentence is about *inbound* -- `dockerclient` already records that
  "outbound is a different question", and ember already originates outbound to
  a provider's API, an asset URL, S3 and a node's sshd. What is new about an
  exporter is therefore not the connection but the **payload**: a span of a
  generation naturally carries the prompt, the seed and the output path, which
  is operator content, and the rule for operator content is an allow-list and
  never a deny-list -- `rejectSecrets` deliberately not scanning `cmd`/`cmds`
  because "a token of affection" is a legitimate prompt is the precedent for
  why. A collector on 127.0.0.1 is not a posture change at all and is where
  #34 independently arrived for the browser half. Anything else is a third
  party receiving operator content and needs its own written yes. So the terms
  of a future adoption, recorded here so they are not re-litigated: the local
  sink first, the destination loopback-only, span attributes by allow-list,
  costs on the wire as integers, and `Shutdown` on `context.WithoutCancel`
  with a **bounded** wait -- the `provision` destroy precedent for the first
  half and `runServed`'s bare `wait()`, which wedged for twenty-three minutes
  after SIGTERM, for why the bound is not optional.
- **Do not add** Bash, Python or another scripting language for project
  tooling. Non-trivial logic is a Go subcommand. `make setup` calls
  `ember setup`, never a shell script — `transcription` already took this
  direction and `go-tun`'s sixty lines of inlined `ssh` in a Makefile recipe is
  what the alternative looks like a year later.
- External binaries at runtime (`docker`, `ssh` on the node) are fine when
  invoked from Go through an interface that has an in-process fake.

## Layout

`cmd/<binary>` holds a ten-line `main` that calls `internal/cli`. Subcommand
dispatch is a hand-written switch over `flag.NewFlagSet`; logic lives in
`internal/`. There is no `pkg/`.

`internal/node` **imports nothing else from this module**, and
`TestNodeImportsNothing` fails the build if that changes. It is the layer that
can be tested with no Docker, no network and no provider account, and the
moment it imports a sibling, the sibling's dependencies become its own.

## Testing

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

## Threat model

ember's posture is one sentence: **the only thing on the internet that can
reach an ember node is sshd, authenticated by a key that never leaves this
machine, verified against a host key ember minted itself.** Everything below is
that sentence made unrepresentable rather than merely tested.

- **Every listener is loopback, and `loopback.Bind` is the only way to name
  one.** `Bind`'s field is unexported with no literal path, so the only Binds
  in the program came from `Port`, `Port6` or `ParseBind`. The bug: a `bind:`
  string in a config reaching `net.Listen` as `":8188"` publishes ComfyUI —
  which has arbitrary file read and arbitrary code execution through custom
  nodes — to the whole coffee-shop LAN.
- **`net.Listen` appears in exactly one package**, and
  `TestNoListenOutsideLoopback` walks the tree and fails otherwise. Every guard
  above it is worth nothing if a provider package opens its own listener
  because it was two lines shorter.
- **`"localhost"` is not a loopback address and `ParseBind` refuses it.** It is
  a name resolved through `/etc/hosts`, NSS and possibly DNS; a host that maps
  it to a LAN address publishes the port, and nothing in the config looks
  wrong. A name is never proof of loopback.
- **`ParseBind` lets `netip` decide before choosing an error message.** The
  first version guessed "is this a hostname?" with a letters-and-dots
  heuristic and classified `":8188"` and `"[::1%eth0]:8188"` as hostnames,
  giving both the wrong advice. Guessing ahead of the real parser sends the
  reader somewhere the bug is not.
- **Both loopback families are bound.** A v4-only listener is invisible to a
  browser that resolves `localhost` to `::1` first, and the operator reads
  connection-refused on a working tunnel as a bug in ember.
- **Create requests can only ask for 22/tcp.** `exposure.PortRequest`'s
  `public` field is unexported and only `SSHPort()` sets it. The bug: one
  `8188/http` port on RunPod serves the workload at
  `{id}-8188.proxy.runpod.net` with **no authentication**, scanned within
  minutes.
- **The post-create exposure check destroys the node on failure.** A provider
  changing a default reintroduces public exposure that the request shape cannot
  see. A warning is useless here: the node is already wrong.
- **There is no outside-in probe: ember reads the mappings and never dials
  from outside, and the posture is therefore one mechanism rather than two.**
  This bullet used to describe `exposure.Probe` -- a DialFunc seam, a per-port
  timeout, `Open`, `AssertClosed`, 144 lines of tests, this package gated at
  100% -- and nothing outside that test file ever called either method, while
  the bullet sat in a list whose other entries are live seals. Read here it
  claimed a capability, which is why it is a deletion and not a shrug: the
  file's own doc comment named the case it was for, "the provider's API says
  one thing and its network does another", and that case was caught by
  nothing. The live control, `AssertSSHOnly` on what the provider reports back
  plus the destroy, does run and does go red when reinstated.
  **Deleting it was NOT the `sdserver.Client.Cancel` call**, which was written,
  covered, uncalled and wired up because deleting it would have deleted the
  only cancel mechanism. The difference is that the join here is not available:
  **a probe needs an address, and ember has one only for the ports it is
  already deciding about.** An unreported public port has no address by
  construction -- nothing names the host, nothing names the public port -- so a
  probe aimed at the SSH endpoint's host and the node-side port number is
  aimed at an address no provider here serves and answers "closed" always,
  which is `AssertNoStray`-could-not-fail with a dialler in it. A reported one
  is already decided and the node is already destroyed. The one real gap is a
  tolerated `Caps.PlatformPorts` entry, where the concession rests on a
  property of ember's node image rather than a promise from the provider --
  and `node.Exposed` carries the node-side port plus a URL "kept for the error
  message", which on RunPod is `{id}-{port}.proxy.runpod.net` for an http port
  while `mapExposure` writes that field as `ip:publicPort`, and
  `Probe.Open(ctx, host, []uint16)` cannot express either. So the address
  would have to be built from field combinations nobody has captured, which is
  how `allowedCudaVersions` and `capabilities.model` both went wrong.
  **The two judgements are kept as a note where the code was**, the
  `node.RateFromFloat` shape, because whoever writes the next one makes them
  again: a port that answers is conclusive and a port that does not is only
  evidence, since a firewall that DROPs looks exactly like a closed port from
  here; and each port gets its own timeout rather than a share of one budget,
  because a run of dropped ports would otherwise eat the budget between them
  and every later port would report "closed" because the clock ran out --
  a false pass on the only answer that matters. The one dial left in the tree
  is `TestServe_IsReachableFromNowhereElse`, which probes this machine's own
  LAN addresses for the `ember serve` listener; it has an address, and that is
  the whole difference.
- **"Published nothing" and "never reported" are different answers.**
  `node.Exposure()` returns `([]Exposed, bool)` and an unreported node fails
  every assertion. They are the same empty slice, and treating the second as
  the first means a provider whose mapping code forgets to populate exposure
  gets waved through — the check still runs, still passes, and verifies
  nothing. `WithExposed` stores an empty report as an empty slice and never as
  nil for the same reason: `append([]Exposed(nil), es...)` returns nil for
  empty input, which turned the safest possible answer into a failed assertion.
- **`Caps.PublicSSH` defaults to the stricter check.** False — the zero value —
  means the provider publishes nothing at all. A provider author who forgets
  the field gets `AssertNoPublishedPorts`, and the failure is a destroyed node
  with a loud message rather than a public port nobody looks at.
- **Exposure is derived from what the daemon or API reports back, never from
  what the create call asked for.** `dockerengine.Binding` carries the host IP
  and not just the port, because Docker's own default is to publish on
  `0.0.0.0` and the difference between that and `127.0.0.1` is the difference
  between a port only this machine can reach and one the whole LAN can. A
  check that reads ember's own request cannot catch a provider doing something
  else, which is the entire thing it is for.
- **The exposure assertion lives above the providers, not inside them.** A
  provider checking its own exposure is a provider grading its own homework.
- **The label is the source of truth; the journal is a cache.** A record is
  joined to a live node by provider node id *or* by the run id in the node's
  label. Matching on node id alone meant a run killed between `Create`
  returning and the id being written produced two findings -- an orphaned
  record that could not name its node, and an untracked node no record
  mentioned -- and `reap` could act on neither, although both carried the same
  run id the whole time. That defeats the entire reason the record is written
  before the create call.
- **Each record names the process holding its node.** Without it, a run killed
  with SIGKILL leaves a node that is running, recorded and inside its deadline
  -- indistinguishable from a healthy long-running session -- and `ls` prints
  `ok` over a GPU billing with nobody watching. The TTL still bounds the loss;
  the holder makes it visible while it is happening. It answers "not
  abandoned" for everything it cannot be sure of: no holder recorded, a record
  from another machine, a platform where liveness cannot be checked. A false
  "abandoned" would tell an operator their running session is dead, which is
  the one mistake that would make the check not worth having.
- **One name for a path, not one per call site.** The journal's "nodes"
  subdirectory was a literal in the CLI and a second literal in the lab
  harness. They drifted, the harness read the wrong directory and found
  nothing, and a lab asserting "a clean run leaves no records" passed without
  checking anything. The harness now reads through `statestore` itself.
- **A provider API's int port is range-checked, never narrowed.**
  `uint16(70000)` is 4464. On the endpoint path ember would dial a port the
  node is not listening on and blame the node for the timeout; on the exposure
  path a public port would be reported as a *different* port, making an
  assertion about what is reachable an assertion about the wrong number.
- **A filter matches a model name it is a prefix of, at the capacity boundary
  only.** `NormalizeGPU` deliberately keeps the capacity suffix, because an
  A100 40G and an A100 80G are not substitutes -- so `--gpu h100` has to
  accept `h100-80g`, and string equality meant it accepted neither. `a100`
  accepts `a100-40g` and `a100-80g` but not `a1000`, because the next
  character must be the separator. Same failure as the substring bug
  `noiseWords` exists to prevent, reached from the other direction: the card
  was rentable, the filter matched nothing, and the output was identical to a
  provider with no capacity.
- **Memory type is not memory capacity.** `HBM3` and `GDDR6X` are noise words;
  `80GB` is not. RunPod spells a card "NVIDIA H100 80GB HBM3", and leaving the
  memory type in welded it onto the capacity suffix as `h100-80ghbm3`.
- **No escape hatch for a seal, not even for a test.** The RunPod mapper's
  refusal of a public non-SSH port is unreachable, because only
  `exposure.SSHPort` can mint a public request. The first attempt to cover it
  exported a helper from `exposure` that built one -- a hole in the wall the
  seal exists to be. The branch stays uncovered; an escape hatch for tests is
  an escape hatch.
- **A tar stream from a rented machine is untrusted input.** Containment is
  enforced twice, by two different mechanisms: the member name is checked
  component by component, and every write goes through an `os.Root`. A string
  check misses a spelling; a Root cannot tell ember the archive was trying
  something.
- **A hostile member name is refused, not cleaned.** `path.Clean("/../a.png")`
  returns `/a.png`: it *absorbs* the `..` because nothing is above root. An
  earlier version cleaned first and then looked for a leading `..`, so the
  check could never fire -- every hostile name was silently rewritten into a
  valid one, nothing escaped because the Root held, and ember reported success
  for a file called something else. An absolute member name is refused for the
  same reason: ember's own tar command never produces one.
- **Only regular files and directories are extracted.** A symlink, hard link,
  device node or fifo is skipped and *named* in an error. ember is retrieving
  generated media from a machine it rented, and refusing the kinds outright is
  shorter than reasoning about where each one could point.
- **A remote command is argv, and every word is quoted.** An SSH exec request
  is handed to the remote shell, so a command assembled by concatenation is a
  shell injection with the operator's own config as the input. ember used to
  refuse anything unquotable, which was right while every remote command was
  tar over config-file paths; a workload cannot work that way, because a
  prompt is arbitrary text. So `sshx.Quote` wraps in single quotes, where
  every character except the single quote itself is literal, and escapes that
  one as `'\''`. A unit test checks the string ember builds; a lab
  round-trips the same hostile inputs through a real sshd and a real `/bin/sh`
  and compares what arrived, because whether the shell agrees is a question
  for a shell. Both were checked by breaking the quoting and watching them
  fail.
- **A workload runs over the SSH connection, not as the entrypoint.** A node
  ember cannot reach is a node ember cannot destroy, so there is no value in
  a workload outliving the connection — and running it here means the output
  streams live and the exit status is known when it happens rather than
  polled for through a file.
- **A node rented to run something stops when it has run it.** Holding
  afterwards bills for a machine with nothing left to do.
- **The exit status is read after the stream, and always.** A `tar` that failed
  halfway produces a valid prefix of an archive: the extraction is happy, the
  data is incomplete, and that is exactly how an output fetch loses files
  quietly. GNU tar over a missing path exits non-zero and writes a valid
  *empty* archive, which without this would read as a successful fetch of
  nothing.
- **Outputs are fetched on a context that survives Ctrl-C.** The node is about
  to be destroyed either way, whatever it produced is about to stop existing,
  and a run that failed halfway is often the one whose partial output and logs
  are worth having.
- **"Already there" has two meanings.** `mkdir` returning EEXIST over a
  *directory* is fine -- two outputs can land in overlapping trees. Over a
  *file* it is not: nothing under it can be written, and treating them alike
  reported success for a directory that was never created.
- **A fake must reproduce the behaviours that cost money to find, including
  the unhelpful ones.** `runpodfake` reports no host key, leaves `ssh.direct`
  null for a while after the status reads RUNNING, nulls `ssh` and `runtime`
  on every list entry, and publishes a web terminal port nobody asked for --
  because each of those is a real RunPod behaviour that broke ember once. A
  fake that is tidier than the service it stands in for tests a service that
  does not exist. The previous RunPod tests used one-off handlers returning a
  canned blob, which cannot express the thing that actually breaks: time --
  and the conformance suite now points the real provider at this fake instead,
  which is what that comment was asking for.

  **The fake one layer down was tidier, and it said in words that it was
  not.** `providertest.RecordingProvider` echoes the injected host key back,
  which is what a provider supporting injection does and is the right default
  -- and its comment read "leave it zero to exercise the trust-on-first-use
  fallback", while zero *is* the echo. So the one RunPod behaviour ember's
  quietest bug was about, reporting nothing, was unreachable through it, and
  no assertion the `provision` unit tests carried could have caught the
  reinstated defect: not a dead assertion but a case the fake could not
  produce. `ReportsNoHostKey` is that behaviour, a named flag rather than a
  sentinel, because the zero value already means something else. Measured: the
  defect is green against every other case and red against this one.

  It was tidier in one place, and the conformance suite is what found it: a
  `DELETE` of a pod already deleted answered **200**, because the handler
  checked whether the pod was in its map and not whether it was gone. The
  live API 404s, `httpx.Delete` turns that into success, and
  destroy-is-idempotent therefore passed against this fake *without the
  not-found path ever being taken* -- the exact shape of
  `AssertNoStray`-could-not-fail, in a fake rather than in a cleanup order.

  **A route it does not serve is a path no lab can reach.** There was no
  `/v2/catalog/cpus` handler, so a request for it 404'd and `cpuOffers` --
  what `ember store sync` rents its cheap downloaders through -- had never run
  against the fake at all. Two money invariants live behind that endpoint:
  `Filter.CPUOnly`'s exclusivity, and the container-disk clamp, which was
  exercised only against canned bodies. The clamp is the sharper case, because
  a clamp means nothing unless the service would have said no: with the fake
  accepting any disk size, **deleting the clamp passed everything**. It now
  answers the live `400 "Container Disk must be less than or equal to 20"` for
  a CPU pod and nothing for a GPU one, refuses `include=AVAILABILITY` without
  `product`, and reports no availability levels when `include` does not ask
  for them -- a catalogue read that forgot the parameter is a priced catalogue
  with no capacity in it, which presents as "that card is sold out
  everywhere". It also stopped echoing a `gpu` object and a 4090's hourly rate
  onto every pod, because an assertion about whether a CPU-only run got a card
  passed against either answer while it did.

  The catalogue bodies are the provider's own committed fixtures, served
  verbatim rather than copied here, and that is the opposite of the choice the
  rest of this fake makes. What a fixture cannot express is time, and a
  catalogue read has none; what is left is a field shape, and two copies of a
  field shape is how `dataCenter` against `dataCenterId` stayed invisible. The
  query refusals above are behaviour a file cannot hold, so they are code over
  that one body rather than a second body. `internal/provider/runpod/testdata/README.md`
  says which of them were observed and which rest on the endpoint's own
  description.
- **A UI has a class of bug no unit test can reach, so one lab drives a
  browser.** Three in this feature were invisible to the Go tests and to
  `cargo test` and obvious the moment a page loaded: a nil slice marshalled as
  `null` dropped whole websocket updates, a keyed list never re-rendered so the
  progress row froze while the step advanced in the database, and a cached wasm
  meant a reload after `make web` served the previous app. `TestServe_UI` is
  chromedp against the real page, and it exists for exactly those -- not for
  coverage. Its selectors are the page's own structure rather than attributes
  added for it, so markup that changes breaks the test, which is the correct
  direction.
- **A regression test that has never failed proves nothing.** Each of these
  was checked by reinstating the bug and watching the lab go red. **The one
  claim in this list that was itself untrue said so about the most important
  guard here**: "the trust-on-first-use lab fails with the exact warning the
  live run printed" -- and it does not, because the warning is gone. The
  fallback is a refusal now, which is right, and what went with it was the
  only thing either regression test was looking at. Both
  `TestRunPod_PinsTheMintedKeyRatherThanTrustingOnFirstUse` and
  `TestWithNode_PinsTheKeyEmberMintedNotTheOneTheProviderReports` ended on
  `!strings.Contains(log, "first use")`, a string that appears in no log call
  in any package at any level, so reinstating the original RunPod defect kept
  a hundred labs and the whole unit suite green. The only red light anywhere
  was `TestNoProductionDeadCode` noticing `TrustOnFirstUse` had gained a
  caller -- a stale-inventory check whose own failure text tells you to delete
  the entry. The threat-model entry below is the fix; what this entry is for
  is that a sentence in this file asserting a test goes red is worth exactly
  as much as the exit code nobody checked.
- **Clean up what you did not think to look for.** The lab harness removed
  containers and not the networks beside them. Nothing bills for a bridge
  network and no assertion mentioned one, so a leak stayed invisible until
  Docker had subnetted its whole address pool and every run failed with "all
  predefined address pools have been fully subnetted". `AssertNoStray` now
  covers networks, and a lab asserts `ember up` leaves none. The check still
  matches on the *presence* of ember's label and never on its value, because a
  sweep that removes only what the harness remembered creating is this leak
  again -- but the set it is taken over is **this suite run's**, not the
  daemon's. That distinction is the entry two below, and it cost two suite
  runs to learn: the breadth is the property and the daemon was never the right
  scope for it.
- **`AssertNoStray` could not fail, and nobody noticed for the life of the
  suite.** The lab's cleanup ran `Reap()` and then `AssertNoStray()` in that
  order, so the reap removed every container and network and the assertion
  then looked at an empty daemon. Its own comment claimed it ran "before
  AssertNoStray records the failure, never instead of it"; the code did the
  opposite. Every lab's final assertion was passing because there was nothing
  left to see — the exact failure mode the `--- SKIP` gate exists to prevent,
  reached without skipping anything. The cleanup now snapshots the daemon,
  then reaps, then reports the snapshot, so both properties hold: one failing
  lab still cannot cascade, and a leak is still named. Found by reinstating a
  deliberate leak and watching the suite stay green. What the snapshot is taken
  *over* was wrong for longer and in the other direction -- it was the whole
  daemon, which is the entry below -- so the two fixes are one rule: the
  cleanup sees everything this suite run produced, at a moment when reaping
  cannot have emptied it, and nothing else. The same shape was then found one
  layer *out* of the suite, in the Makefile: every lab target piped `go test`
  into `tee`, so `make` read tee's status and a failing lab exited 0 -- the
  gate that decides whether a workload image is published among them.
  Measured rather than argued: with the old recipe a deliberately failing
  `TestWorkloadImage_ListensOnNothingButSSH` left `make publish-sd-cuda`
  running `docker tag` and `docker push`, and the only thing that stopped the
  image shipping was that the registry in that experiment was a closed port.
  Both halves of this entry are the one rule -- a gate nobody has watched fail
  is a gate nobody should trust -- and both were found by reinstating the bug.
- **A cleanup that deleted another run's live nodes.** `Lab.Nodes` filtered
  containers on the mere presence of `ember.run-id`, with no value, and `Reap`
  force-removed everything it returned plus every labelled network and volume
  -- so a lab's cleanup was a whole-daemon delete. That was safe while this
  repository was one checkout. Agent worktrees put several on one machine, each
  able to run the labs against the one Docker daemon, and the two runs then
  destroyed each other's nodes mid-test: `No such container` and `connection
  refused` for a container that existed a moment earlier, 170s of wall clock
  against the usual 85, and **every failing test passing when run alone**. A
  lab that fails for reasons unrelated to the code is the other half of the
  `--- SKIP` problem -- it teaches people to re-run rather than to read, and a
  re-run that passes is indistinguishable from a flake that was hiding
  something. The fix is an advisory lock on the daemon, taken once per test
  binary (`labdaemon.Acquire`), plus a snapshot of what was already labelled
  when the lock was taken, which every listing subtracts. **A per-run label
  value was the obvious answer and is not sufficient**: it can scope what the
  *harness* looks at, and ember's own leftover sweep is daemon-wide by design
  -- `TestLeftover_ReapRemovesANetworkNothingElseWould` asserts `removed 1 of 1
  leftover`, and a second suite's deliberately-expired network is swept by this
  one's `ember reap` whatever the harness filters on. Only exclusivity makes
  those assertions true. It is an flock and not a pid file because the kernel
  releases it when the holder dies, and two labs here SIGKILL ember on purpose;
  a lock that could go stale would have to be stealable, which is the
  daemon-wide delete again with extra steps.
- **"Another run is in progress" is a wait. "Something ember will report is on
  the daemon" is a refusal.** Failing a suite because the daemon is busy would
  be the `--- SKIP` mistake in red, so a queued suite waits, says who it is
  waiting for, and fails with one sentence only if the holder never finishes
  (`LockWait`, ten minutes, under the binary's own 20). A stale object is the
  other question, and the split between tolerating and refusing one is
  **measured** rather than chosen: with one `ember.kind=node` container left
  over on the daemon, `ember ls` reports it `untracked` -- correctly, that is
  what the command is for -- and
  `TestLeftover_ReapRemovesANetworkNothingElseWould` fails ninety seconds in
  with "ls still reports findings after reap", which names nothing an operator
  can act on. ember's own listings are daemon-wide by design, so the harness
  cannot filter that away; deleting it was the defect; so `Objects.Reported` is
  refused up front, naming each object and `make reap-labs`. Everything else --
  a container wearing only `ember.run-id`, which `local.List` never looks at --
  is left strictly alone: not counted, not reported, not removed, named once in
  the log. Both halves are live code, which is the test of the split: refuse
  everything and the subtraction would be dead, subtract everything and the
  refusal would be.
- **`make reap-labs` is the escape hatch, and it is the one place the refusal
  points at a tool rather than a rule.** It is honest about being a developer
  tool: nothing in CI runs it, and it will remove objects no test created.
  Refusing while a suite holds the lock is right there and waiting would be
  wrong -- a tool whose whole job is a daemon-wide delete must not queue behind
  a suite and then remove what it has just finished producing.
- **A lab log is read back, so two checkouts cannot share its path.** The four
  lab targets `tee`d into fixed `/tmp` names and then grepped them for `---
  SKIP`. A second worktree's run truncates that file as it starts -- before
  `labdaemon.Acquire` has taken the daemon lock, so the lock cannot serialise
  it -- and the first run's gate then greps a log belonging to somebody else,
  over a hole where its own output was: an assertion reading the wrong file,
  which is how the journal's "nodes" subdirectory went unchecked, applied to
  the gate that exists because a skipped lab reports success for something that
  never ran.

  **The first fix for it keyed `LAB_LOG_DIR` to `$(notdir $(CURDIR))`, which is
  `ember` for every clone there is.** Measured: `~/github/ember` and two copies
  under different parents all printed `/tmp/ember-labs-ember`, so the collision
  survived in full for the one naming `git clone` gives by default -- while the
  comment above the line said "keyed to this checkout". Nothing had bitten only
  because the agent worktrees that motivated the fix are named `wt-…` and
  `agent-…` and happen to differ. It is a sha256 of the whole path now, and
  **not** `$(subst /,-,$(CURDIR))`: that substitution is not injective over a
  `-`, and `/w/ember-wt/ember` and `/w/ember/wt/ember` both become
  `-w-ember-wt-ember`, which is this defect again one character further down. A
  digest also cannot contain a space, which matters because `LAB_CHECK`'s
  `grep -q -- '--- SKIP' $(1)` is unquoted and would be handed two filenames.
  `TestLabLogDirIsKeyedToTheWholeCheckoutPath` is the exit code the comment was
  standing in for: it asks make for the value from two directories of the same
  name and from one whose name holds a space. It **evaluates** the Makefile
  rather than parsing it, which is the opposite of the other guards in
  `cmd/check-coverage`, because the defect was in an evaluation -- banning
  `$(notdir` by name would fire on a correct use of it, which is
  `TestNoListenOutsideLoopback`'s own first version flagging `loopback.Listen`.
  Both halves were reinstated: the basename, and an empty digest, which is what
  a missing `sha256sum` leaves behind under pipefail and which the Makefile now
  refuses with `$(error)` rather than quietly sharing one path again. And the
  guard was deleted with the basename still in place, because `go test ./...`
  is entirely green over that defect -- nothing else in the tree reports it.
- **A test that asserts on a port number is asserting about the whole
  machine.** `TestForwarder_CancelReleasesEveryPort` checks the real property
  -- after `Run` returns, the same ports must be bindable again -- and
  `freePort` gets those ports by asking the kernel for an ephemeral one and
  giving it straight back. Between the release and the re-bind, anything else
  on the machine can take it; CI runs every package's test binary at once,
  each handing out ephemeral ports, and it did:
  `bind: address already in use` on a pull request that had not touched that
  package, and never once in 300 local runs. A single attempt turns that into
  a red build on an unrelated change, which is how a working check gets
  ignored rather than obeyed. The assertion retries to a bound now, which
  keeps the property -- a forwarder that really held its listener still fails,
  a second or two later -- and loses only the false failure. `EADDRINUSE`
  cannot say who holds the port, so the message says that rather than
  accusing the forwarder.
- **And that test could not have failed anyway, which is the more useful
  finding.** On the cancellation path the leak is unrepresentable: closing the
  listener is what unblocks a goroutine parked in `Accept`, so `wg.Wait`
  cannot return until every listener is closed and `Run` returning *implies*
  the ports are free. Measured rather than reasoned -- deleting `closeAll`
  from the exit path entirely leaves the test green, because
  `context.AfterFunc` has already done it. What it guards is therefore the
  shape: the day somebody unblocks `Accept` another way, `closeAll` becomes
  the only thing releasing the ports. The leak that IS reachable is the
  early-return path, and reinstating it there -- a `Run` that refuses after
  opening a listener and forgets to close it -- is what proves the retry did
  not turn either assertion into a no-op.
- **A lab helper owns what it creates.** `CreateVolume` registers its own
  cleanup rather than leaving each lab to remember, because cleanups run
  last-registered-first: the seeded volume goes before the stray check looks,
  while a volume ember itself left behind is still reported. The first lab
  that forgot would otherwise have turned a working check into a failing
  suite, which is how a working check gets weakened instead of obeyed.
- **A lab that leaks on purpose says so.** The two SIGKILL labs exist to leave
  a node behind, and the first run of the repaired stray check reported both.
  They call `Reap()` themselves once they have finished asserting, which
  declares the orphan as deliberate and keeps the check unconditional for
  every lab that leaks by mistake. Relaxing the check instead would have put
  it straight back where it was. Their orphan is still reaped under the scoping
  above, and must be: it was created during this suite run, which is the only
  thing that distinguishes it from the stale container of a run that died
  yesterday -- and that one is left alone because nothing here made it.
- **The run id travels into `Destroy`; it is never re-derived from the thing
  being destroyed.** `local.Destroy` used to read it off the container's own
  label in order to find the bridge network beside it -- and a container
  removed by anything but ember (`docker rm`, a prune, a crashed lab's
  cleanup) made that inspect return not-found, which Destroy treats as
  success, so the run id stayed empty and `Destroy` returned `nil` over a
  network that is still there. Nothing bills for a bridge network, which is
  why it was invisible; what it costs is every local run failing at once,
  weeks later, with "all predefined address pools have been fully subnetted",
  an error naming Docker and not ember. Every caller already had the run id
  -- provision is destroying a run it created, reap reads it off the finding,
  `ember down` off the record -- so the parameter asks for nothing that was
  not known. The container's own label still wins where there is one: the
  label cannot be wrong about which run a container belongs to and the
  argument can, and believing a wrong argument would remove a *live* run's
  network.
- **A leftover is swept by label, because the label is what survives.**
  Passing the run id closes the case going forward and can do nothing for a
  network that leaked before, or for `ember down provider/id` on a node with
  no record whose container somebody already removed -- there, nothing
  anywhere knows which run it was. So `provider.LeftoverAPI` lists networks
  whose run has no node container and `ember reap` removes them, reported by
  `ember ls` rather than discovered by an exhausted address pool.
  Containment twice, by two mechanisms, as the tar extractor is: the argument
  cannot help what is already on the daemon, and the sweep cannot act until a
  deadline has passed.
- **A leftover needs attention and a store does not, because ember can tell.**
  A store existing is the expected state and ember cannot know whether the
  weights on it are still wanted, so flagging one would teach the operator to
  ignore exit 2. A network whose node is gone is unambiguously garbage.
- **The sweep is bounded by the deadline, and that is the create window.**
  `local.Create` makes the network before the container, so for a moment every
  healthy run has a labelled network with nothing attached -- identical to
  wreckage. A deadline in the future cannot be a run that has finished, so a
  leftover inside its deadline is not reported *at all* rather than reported
  and not reaped: a finding that is sometimes false is worse than one that
  arrives up to a TTL late, and the deadline is already reap's authority over
  nodes. A zero deadline -- an ember label too old to parse -- counts as past,
  because the alternative is that it leaks for good.
- **`AssertLeftoverCapsMatch` checks the claim in both directions.** A
  provider that implements `LeftoverAPI` and forgets `Caps.Leftovers` is never
  swept, which is the direction that leaks; the mirror of
  `AssertStoreCapsMatch`, and the reason both are assertions rather than
  comments.
- **`node.Leftover` carries a `Kind string` and that is deliberate.** There is
  one kind today. A typed enum would be a closed set invented from a single
  example, which is the mistake a driver interface designed from one server
  would be; the field is read from the label the provider wrote and is only
  ever displayed.
- **A lab seeds state out-of-band, through Docker, not through the code under
  test.** Widening `sshx.Runner` with stdin support so a test could write a
  file would be a production interface shaped by a test's convenience.
- **Tests inject credentials rather than resolving them.** Left to read the
  environment, every CLI test picks up the developer's own `RUNPOD_API_KEY`,
  registers the real provider and makes live API calls -- behaving differently
  from one machine to the next. `deps.CloudBaseURL` exists for the same
  reason: a test that registers a cloud provider must not be able to reach it.
- **No ember function takes an `ssh.HostKeyCallback`.** `hostkey.Trust` has one
  unexported method and that package holds no permissive implementation, so
  `ssh.InsecureIgnoreHostKey()` cannot be typed into the program — not behind a
  flag, not in a test. It is the most common way an SSH client ships with no
  server authentication at all, and "we grep for it in review" has a 100%
  failure rate eventually.
- **The pin is built from the key ember minted, never from the one the
  provider reports.** ember generated it and injected it, so it already knows
  which key the node must present and never has to ask. Reading the
  provider's answer first was a real bug: the local provider echoes the key
  back on a container label so it worked there, while RunPod reports nothing,
  and ember's first real cloud provision fell through to trust-on-first-use
  against a brand-new IP — the one case TOFU cannot defend, and the exact
  thing minting the key exists to avoid. It logged a warning, and a warning
  is not a security control.

  **Neither was the test.** For the life of both regression tests the final
  assertion was that the log did *not* contain `"first use"` -- and nothing in
  ember has ever logged that string, so the assertion could not fire. The
  positive half above it could not tell the two mechanisms apart either, and
  the reason is an ordering nobody would look twice at: it offered the
  *injected* key first and required acceptance, then a foreign key and
  required refusal, and a trust-on-first-use trust accepts the first key it is
  shown and refuses every other -- so it satisfied both. The lab's dial was no
  better, because the node really does present the key ember injected: a dial
  proves the key matched and cannot say which mechanism decided it.
  Reinstating the one-line fallback -- TOFU when the provider reports no key,
  every other guard in `trustFor` intact -- was green everywhere.

  **So the mechanism is a type now, and the assertion is positive.**
  `provision.trustFor` and `provision.Session.Trust` are `hostkey.Pin`, a
  concrete type whose only constructor is `hostkey.Pinned`, so
  `return o.Pins.TrustOnFirstUse(id), nil` on that path is a **compile error**
  rather than a thing a test has to notice -- the shape `exposure.PortRequest`
  and `loopback.Bind` already use. `hostkey.Trust` stays an interface, because
  its own seal is what keeps `ssh.InsecureIgnoreHostKey` untypeable and the
  dial legitimately takes either; `TrustOnFirstUse` stays, for a provider that
  genuinely cannot accept an injected key, and is no longer reachable from the
  path that rents machines. The honest limit: this seals `trustFor`, not every
  conceivable dial, so both tests now assert the mechanism rather than a
  message -- **a key ember did not mint, offered before anything else has
  touched the trust, must be refused**. TOFU cannot refuse its first key; that
  is what TOFU is. The acceptance is asserted beside the refusal, because a
  refusal alone is consistent with a trust that refuses everything, which is
  what a zero `Pin` is. A zero `Pin` refusing everything is itself the
  decision: `Pinned`'s error used to hand back a nil `Trust`, which panics in
  `Callback`, so a dropped error is now the strictest answer rather than a
  crash. Checked by reinstating the original defect with the type widened back
  -- both tests go red, the lab twice, because the foreign key it refuses
  first is the key TOFU would have pinned.
- **A provider reporting a key that differs from the injected one is a
  refusal.** It has substituted something, and continuing would pin the
  substitution.
- **A node with no minted key is refused, not downgraded.** Reaching that
  branch would mean ember had stopped injecting, which is worth a refusal
  rather than a quiet fallback. This used to say TOFU "is unreachable through
  `WithNode`, which mints for every node", and that was an argument about a
  *branch* rather than about a seal: minting is why nothing took the fallback,
  not why nobody could write one, and one line in `trustFor` reinstated it.
  What makes it unreachable is the return type, two entries up.
- **A provider may name public ports it adds unconditionally, and only
  those.** RunPod maps its web terminal port (19123/http) to every pod with no
  way to disable it — found when ember's first real provision refused the node
  and destroyed it. `Caps.PlatformPorts` names it; everything else still
  destroys the node. The concession is only safe while nothing in the node
  image listens there, so that is a lab assertion about the image rather than
  a promise from the provider, and a workload asking for that port is refused
  outright.
- **Host keys are minted here and injected, not trusted on first use.** TOFU
  against an IP that has never had a key is a free MITM for anyone on the path.
  Injecting through the provider's API concedes the key to the provider, which
  already has root on the node and therefore learns nothing; it closes the
  window against everyone else.
- **Pins are keyed by node id, not host:port, and die with the node.** A pin
  keyed by IP matches whoever gets that IP next, and the resulting churn is
  exactly what talks people into disabling host-key checking.
- **A host key mismatch is a hard error with no `--force`.** A prompt is a
  bypass with extra steps; the correct response for an ephemeral node is
  always to destroy it.
- **`node.Secret` carries key material and prints a fingerprint.** One
  `slog.String("hostkey", string(pem))` at debug level writes a private key
  into the journal permanently, and nothing about that line looks wrong in
  review. `String()` is redacted too, because a stray `%s` in an error is the
  other way it escapes.
- **No secret ever reaches argv.** `/proc/<pid>/cmdline` is world-readable, so
  a key or token passed as a flag is readable by every user on the box for the
  life of the process. Secrets live in `~/.config/ember/credentials.yaml`
  (0600) or the environment, and reach providers in the API's JSON body.
- **`ember serve`'s `Origin` and `Host` checks bound a browser and nothing
  else.** Nothing it serves is reachable off the machine, so the threat is
  not an attacker on the network -- it is any page the operator has open, which
  can `fetch` a loopback port and queue a generation. The browser hides the
  *response* cross-origin and the side effect has already happened, so CORS is
  not a control and a server relying on it would be relying on a thing that
  does not work. `--allow-paid` is off by default, so a cross-site POST to a
  default invocation can start a free local container and spend nothing. The
  `Origin` and `Host` checks on every route are not authentication; the second
  is the DNS rebinding case, where an attacker's domain resolving to 127.0.0.1
  makes their page same-origin with its own name -- passing the first check and
  failing the second.
- **A loopback port is reachable by every uid, and a request with no `Origin`
  is allowed on purpose, so those two checks closed nothing against a client
  that is not a browser.** `curl` sends no `Origin` and neither does any script
  an operator writes, so refusing a request without one would break the command
  line without stopping a page -- and that allowance is indistinguishable from
  the account next door. On `ember serve --allow-paid` a second uid was one
  `POST /api/requests` from renting an H100 on somebody else's card, and `ssh -L
  7777:localhost:7777` -- the deployment ember is otherwise exactly the tool for
  -- puts the same port on a laptop's loopback with the same property. So there
  is a credential, it is **mandatory**, and `webapi.New` refuses a zero token:
  an unauthenticated `ember serve` is not a configuration this program can be
  in rather than one the documentation warns about, because the operator who
  did not read the documentation is the one with another account on the box.
  It is **additive** -- the credential runs after `checkHost` and
  `checkOrigin`, all three or nothing -- and `--allow-paid` is untouched and
  still off by default, because authentication answers who is asking and the
  flag is a ceiling on what the answer permits.
- **`internal/loopback` was NOT widened, and that was the alternative worth
  taking seriously.** A unix socket at 0600 answers the every-uid case with no
  secret at all and would be the stronger mechanism -- and it needs
  `nolisten_test.go` to stop banning `ListenUnix` by name, which is the one
  seal every other guard here rests on. It also answers nothing about the
  forward: the client end of `ssh -L` is a TCP port on loopback again, so the
  second uid reappears one hop away, and a browser cannot speak to a unix
  socket without a forwarder in front of it that would have the same problem.
  `SO_PEERCRED` is a unix-socket mechanism and its TCP equivalent is matching
  a source port against `/proc/net/tcp` -- racy, Linux-only, and wrong through
  a forward, where the connection is made by sshd as the logged-in user and
  would therefore pass. One token answers both threats and widens no seal.
- **Replacing `subtle.ConstantTimeCompare` with `==` is caught by nothing, and
  that is written down rather than papered over.** Thirteen of the fourteen
  defects reinstated for this change went red; that one did not, and no test
  in this repository could. A timing oracle is a statistical property of a
  machine under load, and a test that asserted one would either be flaky in CI
  or be a test of the CI runner. What stands in its place is that the
  comparison is four lines in one function with the reason written above it,
  and that `s.tokenDigest` is the only thing the comparison can reach -- the
  raw token is on `opts` for the printed URL and nothing else. This is the
  honest answer to "which of your guards has no test", and the kind of guard
  that gets lost in a refactor nobody reviews closely.
- **The token is compared as a digest, never as a string.**
  `subtle.ConstantTimeCompare` is constant time in the length of its arguments
  and returns immediately when they differ, so comparing raw values leaks the
  token's length; `==` leaks the matching prefix one byte at a time. Hashing
  both sides first makes every comparison the same thirty-two bytes, which is
  also why "a token of the wrong length" is not a case in the code -- it cannot
  be reached as one.
- **The browser's carrier is a cookie, and that is what let the frontend stay
  unchanged.** A header is the obvious answer and it fails in three places at
  once: `coder/websocket` cannot set a request header from a browser, so the
  socket would need the token in its query string; the gallery's `<img src>`
  cannot set one either, so the image route would need it in a URL too; and
  `sessionStorage` is per-tab, so a second tab is unauthenticated. An
  `HttpOnly; SameSite=Strict; Path=/` cookie is carried on `fetch`, on the
  handshake and on an `<img>` alike, cannot be read back out by a wasm panic or
  a stray console, and `Strict` closes the one browser hole `checkOrigin`
  deliberately leaves open -- a no-`Origin` GET, which is what a top-level
  navigation and an `<img src>` are. Not one line of `web/` changed.
- **`?t=` is honoured on the app's own route and nowhere else.** The token has
  to reach the browser somehow, and a URL fragment -- the candidate the issue
  described -- never reaches the server, which is its one advantage and is paid
  for by having to serve the page unauthenticated so a script can read the
  fragment: the one route a browser navigates to would be the one route with no
  check on it. So the server consumes a query parameter instead, on the app
  route only, answers with the `Set-Cookie` and a **303 to the same path
  without it**, and the whole surface stays gated. The address bar, the session
  history and every `Referer` the page sends carry nothing afterwards. It is an
  online guessing oracle in principle, which is why a minted token is
  `crypto/rand.Text` and not something an operator chose.
- **No `--token` flag, and a lab reads `/proc/<pid>/cmdline` to prove it.** The
  rule is already written down for provider tokens; this is the first
  credential where the uid that could read argv is the same uid the credential
  exists to exclude, so the assertion is against the running server rather than
  against the source. `EMBER_SERVE_TOKEN`, `serve_token` in
  `credentials.yaml` (0600, `node.Secret`), or `crypto/rand`.
- **A minted token is printed and a configured one is not.** A credential the
  operator did not choose has to be printed or it is unusable; one they put in
  a file does not, and a second copy on a stdout that a systemd unit may be
  writing to a world-readable journal is a copy they did not ask for. The
  fingerprint goes there instead. Nothing else prints either: the refusal log
  records the path and which carrier was present, and **not a digest of what
  was presented** -- an operator who pasted the wrong password would otherwise
  have it hashed into the journal permanently, which is the thing `node.Secret`
  exists to prevent rather than an application of it.
- **One refusal message for no credential, a wrong one and a stale cookie.** A
  caller learning which of the three it was learns something about the secret.
  The page load's refusal is **plain text** rather than the API's JSON, because
  that response is what an operator sees when they type the port from memory or
  open a bookmark from before a restart -- and a raw JSON object rendered in a
  browser reads as "ember is broken", which is the failure mode that gets a
  control worked around instead of used.
- **The browser lab drives both directions, because every claim about the
  cookie is a claim about Chrome.** `TestServe_UI` loads the page with no
  credential and asserts the app did *not* mount, then opens the one-time URL
  and asserts the token left the address bar, the offers load, the progress row
  advances and the image decodes. The third of those is the one no Go test can
  stand in for: whether Chrome attaches a `SameSite=Strict` cookie to a
  websocket handshake. A unit test asserting that ember accepts a cookie it was
  handed proves ember agrees with itself, which is the closed loop
  `capabilities.model` and the unrecorded preview frame both sat in. The
  refusal half lives in the same lab rather than a second one so that
  weakening either is impossible without the other going red.
- **The cross-origin labs now carry the token.** Three of them asserted a 403
  and would have gone on passing with the `Origin` and `Host` checks removed,
  because a request with no credential is refused anyway. A refusal test that
  can pass for either reason proves neither, and that is how a working seal
  gets quietly weakened while the suite stays green -- the `AssertNoStray`
  failure, reached from a third direction.
- **A request may narrow the config's ceilings and never widen them.** `ember
  up`'s `applyFlags` *replaces*: `--max-hourly-usd 999` raises the ceiling,
  because the person typing it is the person who set it. A request is not that
  person, so `queue.Narrow` takes the smaller of the two and `internal/app`
  receives a `provision.Budget` it has no way to widen. The config path comes
  from the `serve` flag and is never accepted over HTTP, or the browser would
  be naming which file sets its own limits.
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
- **Every event is numbered, and nothing was numbering them.** `report.Stream`
  stamps the sequence, emitters do not, and the queue's sinks were not wrapped
  in one -- so every event arrived as `Seq` 0 and `genstore`'s
  `INSERT OR REPLACE` on `(request, seq)` meant each overwrote the last. A
  generation that emitted a node_ready, a server start, six job starts, six
  images and six costs left **one row**. The unit test could not see it because
  its fake set `Seq` itself, which is the same mistake as a preview fake that
  never repeats a frame: a fake tidier than the thing it stands in for tests a
  program that does not exist. Found by running `ember serve` and reading the
  database.
- **Nothing the UI serves is cached, and that is a decision.** `make web`
  writes `ember.js` and `ember_bg.wasm` under names that do not change between
  builds, so a browser holding the previous ones serves the previous app --
  and the operator reloads, sees no change and concludes the build did
  nothing. That happened twice while this was being written. Hashing the
  filenames would mean generating the page rather than copying it, for an
  asset that travels zero network hops. A *result* image is still cached
  forever, because it never changes; a preview frame is `no-store`, because it
  is written over as sampling proceeds.
- **A declared content type is said once, in the one place a JSON response is
  written.** `nosniff` was on the image route alone, argued for in a comment
  above it, and was the header that silently vanished when that route's
  ordering broke -- so it is set beside the `Content-Type` in `Server.write`
  now, where every JSON answer and every refusal goes through. Three call
  sites that each have to remember an ordering is the journal's "nodes"
  subdirectory rule reached through a header: one name for a path, not one per
  call site. A browser allowed to second-guess the type is a browser deciding
  what bytes a rented machine produced are, which is the argument the image
  route already carried and the JSON routes never had.
- **A nil slice is `null` on the wire, and `null` is not an empty list.** The
  websocket's first envelope of every connection carries no events by design,
  so it arrived as `{"events":null}` -- and serde's `default` covers a field
  that is MISSING rather than one that is present and null, so the whole
  envelope failed to decode and took the request state with it. The UI showed
  a job frozen at "starting" while the finished image was already on disk.
  This is the `WithExposed` note above, reached from the other direction: an
  empty answer and a nil answer are the same slice in Go and different values
  in JSON. The server sends `[]`, and the client tolerates `null` anyway,
  because the rule has to hold for a field somebody adds later.
- **A Go `int` and a Rust `i32` are the same literal in JSON and different
  types on either side, which is that rule one axis over.** Ten numeric
  fields cross this seam that way, and the server could always say something
  the client cannot hear: `POST /api/requests` answered **202** to
  `width: 3000000000`, because `validateJobs` calls `sdserver.Validate` --
  the half that reads `limits.max_width` -- only `if haveCaps`, and there are
  no caps until this process's first rental. The row was then in the store for
  good, `serde_json::from_str::<RequestList>` failed on it, and because a
  `Vec<Request>` decodes element by element into **one** result it took every
  healthy request in the envelope with it. `views::Queue`'s `if let Ok(l)`
  kept the signal's last value, so the table read "nothing queued" for the
  life of the tab while a two-second poll re-fetched the same undecodable
  bytes. No error anywhere, in either language, and no test in the tree took
  Go's output and asked whether Rust could read it -- `cargo test`'s wire
  tests are hand-written JSON whose numbers are all small, the Go suite
  asserts on Go types where the value is fine, and the browser lab cannot
  enter it because the control is an `<input type=number>` read into that same
  `i32`. The closed loop `capabilities.model` sat in, with a compiler on the
  far side instead of an API.

  **Both sides, and the reason is that neither reaches what the other does.**
  `sdserver.MaxAxis` is the Go half: a static bound of 8192 per axis, which
  is `limits.max_upscale_width` and `max_upscale_height` in the captured
  `/capabilities` -- the largest geometry any response here publishes, against
  generation maxima of 4096 -- so it cannot refuse a size an observed server
  accepts, and `Validate` still applies the tighter published figure once the
  caps are cached. Read from a response and not from a schema, which is the
  `allowedCudaVersions` rule; the limit it accepts in exchange is that a
  future build publishing a larger upscale maximum would be refused before the
  caps permitting it were read, and that is written where the constant is.
  It reaches **four** fields: `steps`, `batch_count`, `hires.steps`,
  `shifted_timestep`, `slg.layers[]`, the two vae tile sizes and
  `upscale_tile_size` have no published maximum anywhere, and a ceiling
  invented for `steps` would be the guess the geometry bound is careful not to
  be.

  So `model.rs`'s `Rows` is the client half, and it is **tolerance rather than
  widening**. Widening the ten fields to `i64` makes today's numbers exact and
  says nothing about the eleventh -- a string where a number was expected, a
  field somebody adds as a `u64` -- while what cost the operator their queue
  was not the width but that one row's badness is the whole list's. A row that
  will not decode is dropped and **counted**, and the count is rendered,
  because a row that vanished silently is the operator's own request
  disappearing. "nothing queued" is not printed over dropped rows either: the
  state that used to print it is exactly the state where every row failed.
  Containment twice, by two mechanisms, as the tar extractor is.

  **The test is a file one language writes and the other reads.**
  `internal/webapi/testdata/request_list_unrepresentable.json` is produced by
  the handler -- `TestRequests_AListingWithARowTheBrowserCannotRead` fails if
  the handler stops sending what is in it -- and `model.rs` reads that exact
  path with `include_str!`, so a cargo build cannot miss it. The row is seeded
  through the store and not the API on purpose: `genstore.Request.Validate`
  has never looked at a geometry, so it is the row an ember from before the
  bound wrote, and it is still in the database of anybody who hit this. Its
  **negative control** is in the Rust test beside the assertion: the
  oversized element is decoded on its own first and required to fail, because
  the thing that used to go was the envelope, so "the healthy row survived"
  proves nothing against a fixture with nothing wrong with it.

  **What is covered by nothing is the render, and that is recorded rather than
  implied.** `Rows::note`, `Rows::nothing_queued` and the per-row decode have
  unit tests; the three lines in `views::Queue` that put them on screen do
  not, because driving a browser into this state means a row in the database
  that `POST /api/requests` will no longer write -- seedable, as the browser
  lab already hand-seeds a `caps` row for the feature-deny path, and not
  seeded here. So this is the `Job::drop_denied` gap one panel over: the
  decisions are tested where `cargo test` reaches them and the effect is not,
  and the thing to notice is a queue panel that goes blank again rather than
  short.
- **`omitempty` does nothing for a `time.Time`, and the tag read as though it
  worked.** `encoding/json` has no empty value for a struct, so
  `report.Event.Deadline` was on **every** event ember emits as
  `"deadline":"0001-01-01T00:00:00Z"` -- on the websocket, in
  `GET /api/requests/{id}/events`, and in every row of `events.body`, the
  table that grows per step per job per rental: a FLUX.2 batch of eight at 28
  steps is 224 progress rows each asserting a deadline of year one, in the
  stream the rule above makes the source of truth and the columns a derived
  index. A value that looks like a fact, which is the defect this file keeps
  recording. `Rate` on the next line carries the same tag, is an `int64`, and
  is correctly absent; `At` sixteen lines up carries none on purpose, because
  `At` is always meaningful -- so the author knew the distinction, and two
  tags on consecutive lines behaving differently is why nobody saw it.
  **`omitzero`, and not a pointer or a `MarshalJSON`.** `*time.Time` is this
  repository's answer where zero is a legitimate value -- `clip_skip`, `eta`,
  `flow_shift` -- and the zero time is not a deadline anybody can mean, so a
  pointer would buy nilness no reader wants and spend it on a struct carried
  per progress frame. A hand-written `MarshalJSON` over thirty-odd fields is
  worse: it is the comment that cannot catch the entry somebody forgets, with
  a marshaller in the comment's place. `omitzero` calls `time.Time`'s own
  `IsZero` and falls back to comparing the zero value for a type without one,
  so it cannot silently never start the way `omitempty` silently never did.
  **Nothing could tell the two tags apart, which is why the assertion is the
  tags themselves.** `internal/report`'s tests assert on `Text`'s output,
  which never reads `Deadline`; `web/` has no `deadline` field and serde
  ignores unknown keys; `genstore` stores the body opaquely -- so no test in
  either language had an opinion about the marshalled shape of this struct.
  `TestEvent_AnOptionalFieldIsAbsentWhenNobodySetIt` reads Event's own tags
  with `reflect` and walks `Kinds()`, derived from the tree on both axes, so
  it catches the next field somebody adds with a tag that does not fire. It is
  not sufficient alone: it proves a zero `Deadline` is absent from the wire
  and says nothing about whether the field is zero on the kinds that do not
  mean it, which is the gap `capabilities.model` and the unrecorded preview
  frame both sat in -- so `TestLifecycle_OnlyTheEventThatMeansADeadlineCarriesOne`
  marshals the events `internal/app` really constructs. No decode impact and
  no README change: the page never read the key.
- **A feature built end to end and never connected passes every test it
  has.** The preview frame was written to the run directory, the images table
  had a `preview` column, the handler answered `?preview=1` with
  `Cache-Control: no-store`, the wire type carried the flag and the page
  looked for it -- and nothing ever inserted a row with it set. Every one of
  those was dead code, the UI showed a step counter advancing beside no
  picture, and the Go suite, `cargo test` and the chromedp lab were all green,
  because each half agreed with itself and no test crossed the gap. The same
  closed loop as `capabilities.model` and the preview fixtures written from
  api.md, one layer up: what was missing was not a check on a value but a
  check that two components were joined at all. The frame's filename now
  travels on the progress event that reports its step -- one event, so a UI
  cannot learn there is a step 4 before it learns there is a picture of it --
  and the queue records it as an image row keyed `(request, job, index,
  preview)`, which is overwritten rather than accumulated because the file on
  disk is. Both halves were checked by reinstating the bug.
- **A typed subset is the right answer until a browser is the caller.** The sd
  job typed eleven fields and sent everything else through `extra`, which was
  correct while the only caller was a YAML file an operator edits by hand -- a
  map in a config costs nothing to write. `ember serve` changed the premise
  without changing the code: `extra` is structurally refused over HTTP, so an
  untyped field is not inconvenient from the web UI but **unreachable**, and
  the UI shipped able to send **eleven of the fifty-one** parameters the
  server reports with nothing anywhere saying which forty it could not. The
  line moved to "path-free and affects a text-to-image run"; what is still
  out is out for one of two stated reasons, not by omission.
- **The parameters that need an image input were absent on purpose, and one of
  the five now ships.** `strength`, `img_cfg`, `control_strength`,
  `ip_adapter_strength` and `increase_ref_index` are path-free numbers that
  would have been three lines each, and were held back because they do nothing
  without an image and ember had no inbound path -- so a control for them
  would sit on screen, accept a value, send it, and change no pixel. Same
  defect as the preview caption that was true of one mode: it reads as
  working. There is a path now, and `strength` goes with it.
- **Four of the five stayed out, and what changed is that the reason is a
  measurement.** Each was sent to a live sd-server before any client code was
  written, which is the calibration habit the RunPod volume API set. All four
  were accepted; none changed anything observable. `control_strength` and
  `ip_adapter_strength` were echoed nowhere and want `control_image` /
  `ip_adapter_image` **plus** a ControlNet or an IP-Adapter in the serve argv,
  which nothing here has run. `increase_ref_index` came back as
  `ref_image_args: "ref_index_mode=increase"` and wants `ref_images`, an edit
  model's input. `img_cfg` is the sharpest of them: accepted at `0`, at `-1`
  and at `99`, echoed at **none** of them, on a build whose
  `generation_features.img_cfg` says true -- so there is no value ember could
  send and no flag it could read that would tell an operator whether it did
  anything. Typing a parameter on that evidence is `allowedCudaVersions` with
  better manners. `strength` is the one with the opposite evidence: the server
  echoes `generation_info.strength` including an explicit `0`, and two arms of
  one measurement with and without an init image produce different images.
- **A strength with no image is accepted, dropped and reported nowhere, so
  ember refuses the combination.** Measured: 202, a completed generation, and
  no `strength` anywhere in `generation_info`. That is the whole "reads as
  working" defect in one field, reachable from the UI the moment the number
  exists without the picker -- so `staticParams` refuses it rather than
  leaving the operator to notice that the slider does nothing. The server's
  own bound is the other half: `1.5` and `-1` are both 400s and `0` and `1`
  are both accepted, so ember's range is the server's and not a guess.
- **`strength` is a pointer, and that is the fourth default this rule has been
  reached from.** The server's own is **0.75** and `strength: 0` is a real
  value it echoes back as `0.0` -- so zero-is-unset would make "keep the whole
  init image" unrepresentable while looking exactly like a field that works.
  Same as `clip_skip`'s -1 and `eta`'s and `flow_shift`'s null, and the UI
  needs the same spelling: an empty box is None and a box holding 0 is
  Some(0).
- **An uploaded image is keyed by its own content, and three questions are
  answered by that one decision.** The uploaded filename never reaches a path
  because it is **discarded** rather than sanitised -- `internal/transfer`
  shipped the other version, where `path.Clean` absorbed a traversal and the
  check could never fire. "Can one request name another request's input" is
  not an access question, because there is no such thing as whose input it is:
  the name IS the content, so naming an object requires holding its sha256,
  which requires holding the bytes. And the rule that every asset is verified
  by digest before any GPU time is spent on it applies unchanged to bytes
  arriving by a different door -- `Load` re-hashes what it read and refuses a
  mismatch, so the reference is a claim about content that is checked rather
  than trusted. Reported and never repaired: ember does not know what wrote
  them, and overwriting destroys the evidence.
- **Staged inputs are their own store, under the state directory and not the
  outputs.** `blob.Dir` is rooted at the output directory and keys everything
  `<request>/<stem>-NN.png`, which is what lets `ember history rm` remove a
  request's bytes and `ember history du` measure them. An upload has no
  request: it exists *before* one, so it cannot be keyed by a request that
  does not exist yet -- and writing it under one that does would make an
  operator's own photograph as prunable as a generated image. An input nobody
  generated with is bytes ember is storing for nobody; a result is bytes
  somebody was billed for.
- **The upload bound refuses and never truncates.** `blob.ReadInput` reads one
  byte past 8 MiB so that "too large" and "exactly the bound" are different
  answers, which is `internal/s3`'s `do` after it was taught the same thing --
  a truncated listing page parsed as XML right up to where it did not. Here it
  would be worse than confusing: a truncated image whose header survived still
  decodes, so ember would stage a file that is not the file somebody picked
  and the operator would find out as a generation that used the wrong pixels.
- **The axis bound is not a size bound, and it is load-bearing precisely
  because ember never decodes the pixels.** A two-kilobyte PNG can declare
  100000x100000 and pass every byte count there is; the thing that would then
  allocate four bytes per pixel is sd-server, on a rented machine, after a
  card has been paid for. `image.DecodeConfig` reads the header and nothing
  else, so the declared geometry is free and the bound costs one comparison.
  8192 is twice the largest generation dimension the captured `limits`
  publish.
- **The format allow-list is two imports, not a list of strings.** A format
  whose decoder this file does not register cannot be accepted by accident --
  `image.DecodeConfig` answers `ErrFormat` for anything else, and there is no
  way to widen the set without adding a line. A string check against the
  returned `format` would be a second spelling of one rule and the two could
  disagree. webp is deliberately out although a browser's picker offers it:
  the standard library has no decoder, so taking one would mean a new
  dependency or relaying bytes ember never looked at.
- **ember validates although it is only a relay, and the reason is where the
  refusal lands.** sd-server does refuse a payload that is not an image --
  measured, a 400 with `{"error":"invalid generation parameters"}`, the third
  spelling of `error` -- but that refusal arrives after a card is rented and
  names the request rather than the file. Here it costs one header read while
  the operator is still looking at the picker.
- **The reference is checked at three moments and the bytes are loaded at
  one.** At enqueue, where a reference nothing staged is refused before the
  request is even queued; again in `NewPlan`, before the create call; and
  again in `served.Generate`, which is the only layer that sees the jobs that
  will actually be submitted -- because the drain loop builds its `Run` from
  the **stored** request for every batch it claims, including the first, so
  the resolution `NewPlan` did cannot travel with it. A plan that carried the
  pixels would hold megabytes for the life of a rental by nothing that reads
  them.
- **The bytes are on `sdserver.Job` and the reference is on `Params`, and the
  asymmetry is structural.** `Params` is stored as JSON in genstore's `params`
  column and handed back to the browser by `viewRequest`; `Job` is serialised
  nowhere. Putting the bytes on `Params` with a `json:"-"` would work today
  and silently empty itself on a round trip through the store, which is a
  footgun aimed at whoever next reads a request back out of the database. And
  `Body` **refuses** a reference with no bytes rather than omitting the field:
  omitting would send a plain text-to-image generation for a request that
  asked for an image and bill for it, which is the quiet half of the defect.
- **`ember up` has no staging directory, and that is the seal rather than an
  omission.** `config.SDJob` has no image field, so a config cannot name one
  -- and `runServed` passes an empty directory, so the resolution path is
  unreachable from that shape by construction. A job that somehow names an
  image there is refused rather than having the field dropped.
- **The staging area is swept by the thing that fills it.** Not by `ember
  history prune`, which removes a request's rows and the images it was billed
  for -- a staged input is neither, and putting it in that scheme would mean
  deciding an operator's uploaded photograph is as removable as a generated
  image. So every upload removes staged files that no **unfinished** request
  names and that are older than a day. Both bounds are needed: without the
  keep-set a queued batch loses the image it was queued with, and without the
  age bound the sweep deletes the file an operator picked a second ago,
  because they are still composing. That second one is `ember store abort
  --older-than`'s rule -- an upload in progress and an abandoned one are the
  same thing to the thing storing them.
- **A keep-set built from a truncated listing sweeps the wrong files, so it
  sweeps none.** `genstore.List` is bounded by construction, and a page ember
  cannot be sure is the whole answer is not an answer -- the rule the
  paginated S3 listings follow. Hitting the bound is said out loud rather than
  shortening the set, because the rows it would be missing are exactly the
  unfinished requests whose images must not go.
- **Only a browser can check a file picker, and this one is the clearest case
  of that rule in the repository.** There is no `File`, no `FileList` and no
  `Blob.array_buffer` outside a browser, so `cargo test` can check that a
  struct serialises and nothing else. Everything between the click and the
  digest -- whether the change handler reads the picked file, whether the
  bytes survive a Uint8Array, whether the 201 decodes, whether the reference
  reaches the draft the Add button clones -- is invisible to every other kind
  of test here. The lab picks a real file in a real Chrome and asserts that
  the queued request names the sha256 **the lab computed itself**, which is
  the join rather than either half agreeing with itself.
- **`chromedp.SetValue` DOES dispatch an event, the entry that said otherwise
  was a reading of the call rather than of a run, and what it explained was
  real anyway: the lab set a geometry and asserted nothing about where it
  went.** The claim here was that the DOM property changes, `on:input` never
  runs and the draft keeps its default. It is
  `SetJavascriptAttribute(sel, "value", v)`, and the pinned v0.21.0's embedded
  `js/setAttribute.js` dispatches a bubbling `input` **and** a bubbling
  `change` exactly when the name is `value` -- so the width `TestServe_UI`
  set that way had been reaching the request all along. Measured both ways
  against the real page: `SetValue` 192 queues `"width":192` and 256 queues
  `"width":256`, and the second is the negative control, because an assertion
  that passes for one value and cannot fail for another is the thing being
  fixed. Believing the signature over a run is the `allowedCudaVersions` and
  `capabilities.model` mistake, reached through a dependency's source instead
  of an API. **The defect is one bullet down from the mechanism**: a control
  the lab drove and a request the lab queued were two halves with no
  assertion crossing the gap, so a page that stopped driving the draft -- the
  frozen-row bug's sibling, where the value is on screen and the state behind
  it is stale -- would have gone on passing while generating at the default.
  That is `AssertNoStray` inside the one lab this file defends on the grounds
  that no unit test can reach the class of bug it covers. Both browser labs
  now assert the geometry on the request the **server stored**, and both go
  through one `setNumber`, because two inlined spellings are how the two labs
  came to disagree about what drives a control. Real key events in both, not
  because `SetValue` is broken but because an assignment plus a synthesized
  `Event('input')` exercises one binding while a keystroke goes through
  `beforeinput`, the browser's own parsing of what a number box accepts and
  the element's validity state. **No guard, and that is a decision.** The rule
  somebody would want -- no setting a control whose value is later asserted
  through a mechanism that does not reach the signal -- is not derivable from
  the tree: which mechanisms reach a Leptos signal is a question about Chrome,
  answerable only by running one, and the path from a control to the assertion
  crosses a browser, a websocket, SQLite and an HTTP route. The rule that *is*
  cheaply expressible, banning `SetValue` by name, would fire on a call the
  measurement says is correct -- which is the defect
  `TestNoListenOutsideLoopback` records about its own first version, where it
  flagged `loopback.Listen`. What stands in its place is the assertion itself,
  which re-measures the mechanism against a real browser on every lab run.
- **A decimal point does not reach an `<input type=number>` through a
  synthesized key event, and the stepper does.** Typing `0`, `.`, `2` into the
  strength box leaves it holding `0` after the second keystroke -- the `.`
  appears in neither `value` nor `validity.badInput` -- and the `2` then lands
  in front of it, so ember was posted `20` and refused it with a message
  naming a number nobody typed. Measured twice, through chromedp and through
  desktop automation, on `eta` as well, which shipped long before any of this.
  It is the automation and not the page: four presses of ArrowUp walk the same
  box to `0.2`, and digits typed in sequence arrive in order. So the lab uses
  the stepper, and what is written down is the limit -- **nothing in this
  repository can assert a fractional value TYPED into a number box.** A fix
  for the typing path was written first and reverted: `validity.badInput` is
  false in every observation here, so the branch was unreachable and the fix
  was a guess about a defect nothing had reproduced, which is the shape this
  repository refuses.
- **A browser lab that waits only for the outcome it wants reports a timeout
  for a refusal that is on screen.** The first run of the picker lab spent
  fourteen minutes waiting for a job row that was never going to appear,
  because the POST had been refused and the panel was already showing the
  server's sentence. It waits for either now, and the same bug was named in
  1.35 seconds the next run. The rule is `orExited`'s one layer out: race the
  thing you are waiting for against the thing that means it will never
  happen.
- **The image lab's assertion is a pixel comparison and not a plumbing
  check.** Same prompt, same seed, same geometry, same step count, two arms on
  one rental differing only in the init image -- and the two result files have
  to **differ**. A server that accepted `init_image` and dropped it would
  produce two identical images, which is exactly what it does with `strength`
  when no image is attached.
- **A value set nobody has observed is not typed.** `cache_mode`,
  `cache_option`, `scm_mask`, `scm_policy_dynamic`, `qwen_image_layers` and
  both `custom_sigmas` stay in `extra`. api.md names them and does not
  enumerate what they accept, and a dropdown built from a guess is the
  `allowedCudaVersions` mistake with better manners -- a schema read in place
  of a response, which this repository has now made twice.
- **A name that reaches a path join is checked against a list the server
  published, not against a pattern.** `hires.upscaler` resolves as
  `--hires-upscalers-dir / (name + ext)`, and `lora[].path` is a relative id
  under the model root's `loras/` -- so neither is a free filesystem path, and
  the published documentation does not say what either does with `..`.
  `Capabilities.Upscalers` turns the first into an allow-list, and the second
  stayed unreachable from a request until somebody had read that code or sent
  a deliberately hostile one. "ember does not know which of these are safe"
  was the honest statement of the seal; "they read arbitrary files" would have
  been a guess in the other direction. Somebody has now read it, and the next
  entry is the answer.
- **`lora[].path` is confined, and it is confined because it is not a path.**
  It never reaches a path join at all. At the pinned revision, 77b72a2,
  `examples/server/runtime.cpp:480`'s `get_lora_full_path` is an equality
  match against a cache the server built by scanning its own LoRA directories
  -- `std::find_if(... entry.path == path)` -- and a miss returns `""`, which
  `examples/common/common.cpp:2093`'s `parse_lora_json_field` turns into a
  refusal of the whole request. Containment is then enforced a second time by
  a different mechanism, which is the rule the tar extractor follows: the scan
  canonicalises every entry and drops anything `model_path_inside` says is not
  under the directory (`runtime.cpp:444`), and the resolver canonicalises and
  re-checks the entry it matched before handing it back (`runtime.cpp:486`).
  So `..` is not cleaned, absorbed or normalised -- it is simply not an id the
  server published.

  **Measured, not reasoned.** `TestPreview_ALoraPathIsAnIdFromAPublishedListAndNotAPath`
  seeds one LoRA-shaped file in a `--lora-model-dir`, an identical one a
  directory above it, and a symlink inside pointing at the second. The
  published id is accepted (202); `../ember-lab-outside/real.safetensors` --
  a real, readable, supported-extension file -- is refused 400, as are
  `../../etc/passwd`, `/etc/passwd`, `./real.safetensors`,
  `loras/real.safetensors` and `real.safetensors/../real.safetensors`. The
  symlink never appears in `capabilities.loras[]` at all; the server logs
  `skipping inaccessible LoRA or path outside LoRA directory`. The acceptance
  is asserted beside the refusals on purpose: without it every refusal would
  be consistent with a server that refuses every LoRA there is.

  **So a `lora` field is possible, and its shape is the upscaler's.**
  `capabilities.loras[]` publishes `{name, path, display_path}` and the `path`
  is the id `get_lora_full_path` matches, so `Capabilities.Loras []string`
  plus a `slices.Contains` in `validateParams` is the whole seal -- the same
  three lines `Upscalers` already has, refusing before a card is rented.
  `testdata/capabilities_loras.json` is a capture with a non-empty list,
  taken before any client code exists, which is the calibration habit
  `internal/provider/runpod/testdata/README.md` set. It is not built here:
  this change's deliverable was the answer.

  **One thing the measurement turned up that nothing asked for.** A published
  id whose file is not loadable is a *warning*, not a refusal: the server says
  `cannot register LoRA source` and generates the image without the LoRA. So a
  future `lora` field buys a refusal for a name the server does not have and
  nothing at all for a name it has and cannot use -- which is a thing to write
  down rather than discover from an image that looks untouched. **"Nothing at
  all" was the generous reading and it is now measured as worse than
  nothing**: the completed job's `generation_info.loras[]` and the PNG's own
  `parameters` chunk both state the LoRA that was never applied, and the only
  account of the failure is two stderr lines with no job id on them. The
  entry beside the asset-verification ones in **Money** has the measurement
  and why option 1 cannot be built on it.
- **A control for a feature the server denies is unavailable, and the flag it
  is read from is the model's and not the mode's.** sd-server publishes two
  feature maps and they answer different questions. `features` is built per
  mode from a literal where every img_gen value starts true, and only eleven
  keys are ever overwritten; `generation_features` is derived from the loaded
  model, starting from a table where `clip_skip`, `flow_shift`,
  `shifted_timestep`, `slg` and `img_cfg` all default to **false**. So the
  second is the one that can say no, and it is the one five of the six gated
  controls read. `hires` is read from `features` only because
  `generation_features` has no such key.

  **Every flag is a pointer, and the capture is why.** A server with no model
  loaded reports `supported_modes: []`, so the per-mode map is never built and
  `features` collapses to the two cancel flags: `capabilities_no_model.json`
  holds a published false for every generation feature *and* a genuine silence
  for `hires`, in one response. A bool would have read the silence as a no and
  greyed the control against every build that does not publish the maps --
  which is the failure `capsView.Caps` is a pointer to avoid one layer up, and
  the shape of the bug where a `caps` row cached before these columns existed
  decoded to empty lists and refused a correct `output_format`. A nil gates
  nothing; a published false refuses.

  **It does not fire on any server ember can currently start, and that is
  written down rather than implied.** ember hands the weights to sd-server as
  argv, upstream calls that the `@command-line` recipe, and that recipe
  declares all twenty-seven features -- so `generation_features` is entirely
  true on every rental. A catalog recipe, which is where a real denial comes
  from, is reachable only through `POST /models/load`, which ember does not
  call. The one response that says no is a server with no weights, and ember
  never caches that because `WaitModel` fails first. What the gate is for is
  that ember now honours the published answer instead of assuming it, and that
  the UI can distinguish "ember has no control for this" from "the server will
  not take it" -- the distinction this whole surface exists to stop being made
  by omission. The deny path is driven by a real response in the preview lab
  and by a hand-seeded `caps` row in the browser lab, because nothing else can
  reach it.

  **`eta` is ungated and says so.** Neither map names it. A flag invented for
  it would be the `allowedCudaVersions` mistake, and a section where five
  controls are gated and the sixth is silently not would read as all-or-nothing
  -- so the page names what is denied in words rather than leaving it to be
  inferred from which boxes are grey, and `eta`'s own tooltip says ember has no
  answer about it.
- **Greying a control is not the same as not sending one.** `/api/config` is
  fetched after the page mounts, so there is a real window in which the
  advanced section is open, typeable and ungated -- and a job added to the
  batch in that window keeps what was typed. The control greys when the answer
  arrives and the value stays in the draft, so the request would carry a
  parameter whose control is visibly unavailable: the same disagreement between
  what is on screen and what is on the wire that each block's switch exists to
  prevent. `Job::drop_denied` clears the draft *and* the batch, and what it
  clears is exactly what `Body` would have sent -- a zero `shifted_timestep`
  and an slg block with no scale are already absent from the request, and for
  skip-layer guidance the scale is the switch.

  **The ten lines that call it are covered by nothing, and that is recorded
  rather than implied.** `drop_denied`'s decision has a unit test;
  the effect in `Compose` that applies it to the draft and the batch does not,
  because the state it exists for -- a value typed in the window before
  `/api/config` answers -- is not one chromedp can be driven into. Removing
  the effect outright was reinstated and **no test went red**: the only one of
  seventeen that did not. So this is the same closed loop as the preview frame
  nothing inserted, caught while it is ten lines rather than a feature, and
  the thing to notice if a value a greyed control cannot hold ever reaches a
  request.
- **Zero is a value wherever the server's default is not zero.** Every typed
  field used zero for "leave it to the server", which is right when the
  server's own default is zero -- and wrong for `clip_skip`, whose default is
  **-1**, and for `eta` and `flow_shift`, whose defaults are **null**. A
  sentinel of 0 there makes `clip_skip: 0` and `eta: 0` unrepresentable while
  looking exactly like a field that works, so those three are pointers
  through all four layers and `Option` in the page. The UI needs its own
  spelling of the same rule: an empty box is None and a box holding 0 is
  Some(0), or the first keystroke in any other field posts a zero nobody
  typed.
- **A block with a switch sends nothing when the switch is off.** Upstream's
  defaults are `hires.enabled` false and `slg.scale` 0, so a block sent with
  the switch off is settings nothing applies. For skip-layer guidance it is
  worse than redundant: layers with no scale look configured and change
  nothing, which presents as "the model ignores slg" rather than as a value
  that was never sent. `slg.scale` is therefore the switch itself rather than
  a checkbox beside it, because two ways to say "off" can disagree.
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
  that loaded something. What leaves the column empty is an `ember up` with
  `cmds` and no server, or a row from before the column -- which is exactly
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
  `cmds` run would test a behaviour that was never wrong.
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
- **A `cmds` step is not a job row, and a plain `ember up` has no jobs at
  all.** Both record the request row and the full event stream and no `jobs`
  rows: a step's own cost and span are on its `unit_cost` event, and the
  figures the jobs table exists for -- images, passes, steps per second -- are
  not things an `ffmpeg` invocation has. A `steps` table was the alternative
  and is a new table, a migration, a `Store` method, a fake method and
  conformance rows for a shape nothing aggregates; argv on `genstore.Job` was
  the other, and that is the wire type a browser posts, which is what `Extra`
  is kept off it for. The honest consequence is printed rather than hidden:
  `ember history` shows such a run with no jobs, a dash for images and card
  time, and a footnote saying which runs those are.
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
- **A caption that is true of one mode is wrong for the others.** The preview
  figure said "the latent, not the output" unconditionally. That is right for
  `proj`, which projects the latent and previews a 512x512 generation as a
  64x64 smear, and wrong for `tae` and `vae`, which decode a real image every
  frame -- in the direction that makes a working preview look broken.
- **A keyed list only re-renders when the key changes.** The progress rows
  were a Leptos `<For>` keyed on the job's index -- which is exactly what a
  key should be, stable -- so a row rendered once when the job appeared and
  then froze: the step never advanced on screen while it advanced in the
  database. Re-rendering a handful of rows on each event costs nothing; a
  frozen progress display costs the operator their belief that ember is
  working. Only a browser could find this, which is the argument for the
  chromedp lab.
- **The remote end of a forward is built by `tunnel.RemoteLoopback(port)` and
  never parsed from input.** `direct-tcpip` connects anywhere the node can
  reach, including the provider's metadata service; a free-form remote host
  makes every ember node an open relay billed to the operator.
- **A non-loopback bind in `cmd` is reported, and the report is about a belief
  rather than a hole.** `sdserver.ServeCommand` can refuse a listen flag
  outright because it knows it is building sd-server's argv and appends the
  address itself. A `cmd`-based server has no such seal -- the operator writes
  the argv -- and `llama-server --host 0.0.0.0` on a rented machine *looks*
  like the ComfyUI exposure this threat model is written against. It is not:
  `exposure.PortRequest`'s `public` field is unexported, only `SSHPort()` sets
  it, a workload port goes through `exposure.Internal`, and the post-create
  check destroys a node whose provider published anything else. So what
  `--host 0.0.0.0` reveals is that somebody concluded the bind address is what
  makes their endpoint reachable, and `workload.NonLoopbackBinds` says so
  before anything is rented. **A warning and not a refusal**, because `cmd` is
  opaque argv on purpose: refusing would be ember claiming to know what
  `--host` means to a program it has never heard of, and the first config
  whose `--host` names the server to *talk to* would be a config ember will
  not run. "A warning is not a security control" still holds and is exactly
  why this is not one -- the control is the unexported field. The one place
  the belief is partly true is the local provider, where the container sits on
  a Docker bridge and the bind is reachable from this machine at the
  container's address, which is not an internet exposure and is not nothing.
- **`netip` decides, so this is not `rejectSecrets`' mistake reached from the
  other side.** That function deliberately does not scan `cmd`, because a
  prompt is arbitrary English and "a token of affection" is a legitimate
  prompt. Nothing here pattern-matches a string: a value is reported only
  after its flag name matched a short allow-list of listen spellings *and*
  `netip` parsed it as an address that is not loopback. A prompt is one argv
  element, so it can never be mistaken for a flag name, and single-letter
  flags are excluded outright -- `-b` is a bind address to one program and a
  batch size to llama.cpp, and a finding about a batch size is the false
  positive that teaches an operator to stop reading these. The limit is
  written down rather than hidden: `--host localhost` is not reported,
  although a name is never proof of loopback, because it is the node's
  resolver that would answer and not this machine's.

## Money

A leaked node bills by the hour in silence. These are not style preferences.

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
- **TTL is mandatory and the deadline is derived in one place.** The deadline
  is written into the provider's label, the local state record and the node's
  own self-destruct timer; three copies computed at three call sites are three
  copies that can disagree, and the one that disagrees is the one that does not
  stop the bill.
- **The deadline is not re-armable, and an interactive session restarts
  instead.** Asked for by #37, and refused: `cmd` plus `ports` already serves
  a code model to an editor over one forward, so the thing that does not
  survive a multi-hour session is the TTL. `cmd/embernode`'s `armDeadline`
  reads `EMBER_TTL_SECONDS` **once**, at container start, and nothing on the
  node listens for a change -- so a re-arm is not a flag, it is a change to
  *how* a node learns its deadline (a file it re-reads, or a heartbeat it
  requires), plus two more copies to update atomically. That is the one
  invariant above, traded for not having to restart a session. So the answer
  is "pick the hours you mean and accept them", written down rather than
  implied: `ttl` is a hard stop and not a session length, the endpoint goes
  away mid-sentence if that is where you are, and the remedy is a new node, a
  new read of the weights and a new forward. A follow-up that wants it has to
  design the deadline's delivery first and the CLI second.
- **There is no idle-stop, and nothing in ember can see the traffic that
  would justify one.** The other half of #37, refused for a different reason:
  a code model held open with nobody typing is the lowest-duty-cycle workload
  here, billed by the hour of wall clock, which is exactly the failure "a node
  rented to run something stops when it has run it" exists to prevent. But
  `tunnel.Forwarder.pipe` joins the two ends with `io.Copy` and discards both
  byte counts, and nothing else observes a forward at all -- so the only
  signal available today is "a connection exists", which an editor holding a
  keep-alive open satisfies forever. Bytes would not be enough either: a
  heartbeat and a streaming completion are both bytes. What "idle" means for
  an interactive session is "no tokens generated for N minutes", which lives
  in the server's own payload -- the thing ember reads only for `sd:`, and
  only with a written reason. An `--idle-stop` built on the wrong signal
  destroys a node mid-thought, which is the one mistake that would make the
  feature not worth having. Same shape as the false-"abandoned" refusal: it
  answers "not idle" for everything it cannot be sure of, and here that is
  everything.
- **Destroy runs on `context.WithoutCancel`.** Ctrl-C cancels the context the
  run was using, and a destroy call on that context fails instantly — the
  single most likely way to leak a GPU.
- **The intent record is written before the create call.** The window between
  the request and reading the response is where an orphan with no local record
  is born.
- **A provider that could not be reached is a finding, not a silence.**
  `ember ls` exits non-zero when any provider failed to answer: "I could
  not check RunPod" must never render as a clean bill of health.
- **Images are pinned to a tag or a digest; `:latest` is refused.** The point
  of ember is to measure what a model costs and how long it takes, and two runs
  a week apart that both say `:latest` may not have run the same software.
- **A placeholder in a committed config is a sentence whose refusal quotes
  it.** `ember.video.example.yaml` carries `REPLACE_WITH_THE_DIGEST_PUSH_PRINTS`
  where a digest goes, which is #18: it refuses, correctly, but it reads as a
  configuration with a typo in it rather than as a template. So
  `ember.code-model.example.yaml`'s one missing value -- there is no ember
  workload image carrying an LLM server, because an image has to carry
  `embernode` and sshd and this repository builds none -- is written as
  `EMBER-HAS-NO-LLM-WORKLOAD-IMAGE-SEE-THIS-FILES-HEADER`, with no tag, so the
  existing pin rule refuses it and the error *is* the instruction. Everything
  else in that file is real, and the split is stated in its header: `store
  create`, `store push`, `store ls` and `store verify` need no node and no
  image, so the half that can be run today is runnable and the half that
  cannot refuses by name. The weights' digest came from Hugging Face's own
  `lfs.oid` and the `X-Linked-ETag` on a HEAD of the resolve URL, which agree
  -- two confirmations for two requests and no bytes, which is the calibration
  habit the RunPod volume API set rather than a number somebody invented.
- **The refusal covers the node image too, because there is only one image.**
  `node-image.yml`'s comment said `node.Spec` refuses a `:latest` *workload*
  image, which reads as though the node image arrives by some other path and
  is therefore unguarded -- and the node image is the more dangerous of the
  two, since it is what installs the host key and starts sshd. It does not:
  ember
  runs one container that is both, so the node image *is* `image:`, it is the
  reference `node.NewSpec` validates, and `:latest` is refused there. Written
  down because working it out took reading four files, and because
  `node-image.yml` published a `:latest` tag for the life of the image --
  underneath a comment arguing against it -- that ember would have rejected
  on sight. Nothing named it and every example config names a digest.
- **A moving tag is not recognisable from a reference, so one is not
  published.** `validateImage` knows the *name* `latest`; it cannot know that
  `:main` moves, and it accepts it. So renaming the node image's moving tag
  would have converted a reference ember rejects into one it runs silently,
  which is the refusal defeated by deleting the one word that fires it. The
  tag is gone instead, the sha tag is what an external consumer pulls, and
  `cmd/check-coverage`'s `TestNoWorkflowPublishesAMovingLatestTag` stops the
  paste coming back -- the `TestEveryBuildTagReachesTheGates` argument one axis
  over: a workflow edit fixes the tag and leaves the next one a silent gap.
  The guard asserts the production rule still refuses `:latest` before
  believing its own message. The workload images' `<tag>-sm<arch>` is a moving
  tag on purpose and is not caught, because the reference they publish carries
  the digest as well and a guard claiming more than that would be false.
- **The volume is faster than documented, and it barely matters.** Measured
  ~1.0 GB/s against a published 200-400 MB/s. For a 40 GB model set the pull
  dominates either way, so the break-even moves from about six runs a month to
  five. A measurement that agrees with the conclusion is still worth having --
  it is how you learn the conclusion did not depend on the guess.
- **Attaching a network volume removes `/workspace` entirely.** The API
  refuses a pod asking for both a persistent disk and a volume, because
  upstream drops the disk whenever a volume is attached -- and what that means
  in practice is that the directory is not there at all, not that it is empty.
  Found on the first live store run: a workload writing to `/workspace/out`
  failed at the tar with "Cannot open: No such file or directory". The node
  image now creates it, as the workload images already did.
- **A store outlives every node that mounts it, and that is what makes it
  dangerous.** It bills by the gigabyte-month whether or not anything is
  attached, so it is the first resource here that can leak while every node is
  correctly destroyed and `ember ls` prints a clean bill of health. ember
  learned to *see* a store before it learned to *make* one, and nothing that
  rents a node may create one as a side effect: a standing monthly charge
  needs the operator to have typed `ember store create`.
- **It is called a store, not a volume.** `node.Spec.Volume` already means
  RunPod's container disk, which dies with the pod. Two things called "volume"
  with opposite lifetimes is how somebody deletes the wrong one.
- **A store cannot be mounted over `/root`, `/etc` or `/workspace`.**
  `node.Mount` is sealed and `MountAt` holds the refusal list. The first two
  shadow the host key and the authorized_keys embernode writes at startup, so
  sshd comes up with neither and the handshake failure reads exactly like a
  host-key attack. `/workspace` is RunPod's default mount path *and* where
  ember writes outputs, so a store there bills for every render for as long as
  the store exists — and the operator finds out on the invoice a month later.
- **Every asset is verified by digest before any GPU time is spent on it.**
  A store is writable by every node that mounts it, which makes it a channel
  between runs that are otherwise isolated: one workload can leave something
  behind that the next one loads as its model. RunPod offers no read-only
  mount, so the write cannot be prevented and is detected instead.
- **The digest is why plain HTTP is allowed for an asset.** TLS authenticates
  the server; the digest authenticates the bytes, which is the stronger claim
  and the one that matters for forty gigabytes of somebody else's weights. An
  attacker on the path can deny service — the fetch fails closed and nothing
  is written — but cannot substitute content.
- **A resident model is worth 3% or 36%, and ember cannot tell which it bought.**
  FLUX.2 [dev] Q8_0 at 2048x2048, 28 steps, on a rented RTX PRO 6000 at
  $2.49/hr with the weights on a network volume: eight runs, four batch sizes,
  both paths, same prompts and seeds, all eight images byte-identical across
  the two arms. `sd.jobs` is flat -- every job after the first took
  140.80-140.88s, on every node. `cmds` split in two: on a node that kept the
  56 GiB in page cache each step took 145.3s, and on one that did not, 226.6s,
  because every step re-read the whole model off the store at a measured
  868 MiB/s. That is 81s and $0.056 per image of variance between two runs of
  the same configuration against the same card in the same datacentre, and
  what causes it is **still not known** -- the caching run was the last of the
  three rather than the first, so warm-up does not explain it either. What has
  changed is that the "all eight nodes reported 620-708 GiB free RAM" half of
  that sentence has been **withdrawn as evidence**: it never said anything.
  Measured here, with `docker run --memory 512m` against the probe ember now
  carries, a container bounded at **512 MiB** reports **125.7 GiB** of
  `MemTotal` -- a factor of 252 -- because `/proc/meminfo` inside a container
  is the host's. So the figure that looked like it ruled memory out was reading
  the wrong number, and the leading guess is unharmed: what bounds the page
  cache is the pod's own `memory.max`, and nothing had ever read it. `ember
  history --memory` records it on every rental now, so the next FLUX.2 run
  settles it without anybody remembering to type a command at the right
  moment. See `internal/nodemem/testdata/README.md` for the captures and for
  what they do **not** settle -- what RunPod actually sets is still unknown and
  needs a rental. So the honest case for the resident model is
  not the mean saving but that it is the only one of the two whose per-image
  price is the same on every node. Crossover is between one and two images,
  not the ~5 the sd15 run predicted, because one image here is 140s of
  sampling rather than half a second.
- **`load_elapsed_seconds` is not the cost of becoming resident.** sd-server
  reported **551ms** for a model whose weights are 56 GiB, so ember printed
  "551ms of loading is paid once rather than 8 times" while the `cmds` arm of
  the same measurement was spending 80-96s per image re-reading those weights
  -- understating the feature by two orders of magnitude, in the one line an
  operator would size a batch from. The field was believed because api.md
  lists it, which is the same mistake as `capabilities.model` and as RunPod's
  `allowedCudaVersions`: a **schema** read in place of a **response**. ember
  now times the span itself, from starting the process to `models/status`
  reporting ready -- which is sound because sd-server does not open its port
  until the load has finished, and is exactly what a fresh `sd-cli` pays on
  every invocation. The honest line that did exist in `WaitModel` could never
  print, because it is guarded on `polls > 1` and the port opening late means
  the first poll always finds the model ready.
- **A capability checked on one image must be true of the other.** The CPU and
  CUDA workload images are built from the same stable-diffusion.cpp revision
  on purpose, so a result from one is comparable with the other -- and so the
  video-support check, which can only run on the CPU build (the CUDA binary
  needs libcuda.so.1 to print its own help, and the lab machine has no GPU),
  says something about the image that actually goes to RunPod. The lab asserts
  the two pins agree before it asserts anything else.
- **The workload image carries no model, and the measurement is why.** The
  checkpoint was 51% of ember-sd and 92% of what a one-shot run cost, and a
  `COPY`'d model gets a layer digest that is ours alone -- so it can never hit
  a shared cache on any host, while the identical bytes sit on the same
  machines under somebody else's digest. Removing it took the image from 5.08
  GB to 2.87 GB. Above about 10 GB baking stops being possible at all: the
  registry refuses a larger layer and Wan 2.2's experts are 14 GB each.
- **A lab about a published image belongs where the image is built.** The
  workload-image assertions failed CI on their first run, because CI builds
  only `ember:lab` and the CUDA image takes forty minutes to compile.
  Requiring it on every pull request makes every pull request slow; failing
  without it makes every pull request red, which is worse -- it teaches people
  to ignore red. They sit behind an `imagelab` build tag and the publishing
  workflow runs them against the image it just built, *before* pushing, so a
  bad image never ships rather than being noticed afterwards. Not a skip: the
  file is not compiled without the tag, and the one assertion that cannot run
  there -- video support needs `sd-cli`, and the CUDA binary wants
  libcuda.so.1 on a runner with no GPU -- runs against the CPU build, whose
  revision the ordinary suite checks matches. That sentence was **false from
  the commit that wrote it**, and not because of the labs: the same commit
  added the target that runs them, which piped `go test` into `tee` and so
  reported tee's success -- the Testing entry above. A gate stated as a design
  property is worth exactly what its exit code is.
- **The listener assertion runs against the published image too.** ember-sd
  cannot be built FROM the node image -- that one is debian-slim and CUDA
  needs an NVIDIA base -- so it repeats the sshd configuration by hand, on a
  different base, with a different package manager. The platform-port
  concession was justified by a property of the image that never leaves the
  laptop while a *different* image was the one RunPod published a port to.
  The lab now takes an image name, and a missing image is a hard failure
  naming the command that builds it: it must not pull (that would be testing
  GitHub) and must not skip (that reports success for something that never
  ran).
- **A precaution nobody can point at a failure for just makes every build
  slower.** The workload workflow deleted .NET, Android and GHC before
  building, for space. The log says it started with 86 GB free and spent two
  and a half minutes reaching 111 GB. Removed.
- **Push what is on this disk, sync what is on the internet.** Uploading from
  here is free and bounded by the home link -- measured at 67 Mbps, which is
  four minutes for a 2 GB checkpoint and eighty-one for a 40 GB model set.
  Renting the cheapest machine with no GPU and having it download instead
  costs $0.06/hour against $0.89 for a 4090, and fetching sd15 that way took
  2m27s end to end for a quarter of a cent. Neither is the right answer
  always, which is why both exist.
- **Filter.CPUOnly is exclusive in both directions.** A run that wanted a card
  and silently got a CPU box fails at the workload; one that wanted a cheap
  downloader and got a 4090 costs fifteen times what it meant to. Neither may
  happen by default, so a CPU offer matches only a CPU filter and a GPU offer
  only a GPU one.
- **An offer that did not say whether it can be reclaimed is treated as
  reclaimable.** `node.Offer` carried an `Interruptible bool` that no provider
  ever assigned, `provider.Caps` carried a *different* field of the same name
  that was assigned correctly and read by nothing, and `Filter.Match`'s two
  guards therefore compared against a constant false -- so
  `allow_interruptible`, whose default is the strict answer, refused nothing
  for the life of the code while `runpod_cloud: COMMUNITY` rented hardware
  somebody else can take back. The fix is not "populate the field": a provider
  can forget again, and that is the same bug one author at a time. So
  `node.Reclaim` has three states, `ReclaimUnknown` is the zero value, and
  only `ReclaimNever` reads as safe -- the direction `Caps.PublicSSH` and
  `State.Billable` already default in, where a value added later is unsafe
  until its author says otherwise. The same mistake as `Filter.CPUOnly` above,
  reached from the other side: there the two kinds had to be told apart, here
  the two *layers* did, and a name shared across them is what made it
  invisible.
- **The reclaim answer is per offer and the capability only implies it.**
  `Caps.InterruptibleTier` is a whole-catalogue claim, which is how RunPod
  models it: the operator picks the tier in config and everything in it is
  somebody else's machine. Moving the refusal there would have been shorter
  and wrong for Vast.ai, where interruptible bidding is per listing and is
  most of the reason to use the provider -- a `Caps`-only check refuses its
  on-demand offers along with its bids. So the gate is `Offer.Reclaim`, the
  capability is a claim `providertest.RunConformance` holds the offers to in
  both directions, and RunPod derives both from one `reclaimOf(cloud)` so the
  two cannot disagree.
- **A listing emptied by the reclaim guard says which key emptied it.** On the
  community tier every offer is refused, and "no offers matched" reads as "that
  card is sold out everywhere" -- the wrong story, same as the one the store's
  datacentre refusal exists not to tell. It lives in the provider and not in
  `provision.PickOffer`, because every provider runs `FilterOffers` over its
  own output before returning: by the time `PickOffer` sees an empty list the
  refused offers are gone, and the diagnosis there would have been a branch
  that cannot fire. Which is this entry's own defect, so it was worth noticing
  before shipping it.
- **Offer.VCPU tells the two kinds apart, and is the number the create call
  needs.** A separate boolean would be a second source of truth for one fact,
  and the two could disagree.
- **A CPU pod refuses a container disk above 20 GiB.** GPU pods have no such
  ceiling, and the schema publishes no maximum -- the API answers "Container
  Disk must be less than or equal to 20" with a 400. Clamped rather than
  refused, because the disk is incidental to what these are rented for: the
  download lands on the mounted store, and the container's own disk only has
  to hold the image.
- **An incomplete multipart upload keeps every part that arrived, and bills
  for them.** It is not an object, so it does not appear in a listing -- the
  same shape as a leaked pod: nothing shows it and it charges by the month.
  The abort runs from a defer on `context.WithoutCancel`, for the reason
  `provision`'s destroy does: Ctrl-C is the most likely way to end up there,
  and an abort on the cancelled context fails instantly. When the abort
  itself fails, the error says so, because the operator now has parts they do
  not know about.
- **S3 answers 200 and then reports the failure in the body.** The connection
  is held open while the object is assembled, so a client that trusted the
  status reports success for an upload that did not complete.
- **A store that can be written to and not read back hides the one leak
  nothing else can see.** `internal/s3` had `Upload`, `put`, `multipart`,
  `create`, `complete` and `abort` -- no `ListObjectsV2`, no `HeadObject`, no
  `ListMultipartUploads` -- and `ember store` had four verbs, none of which
  read. So the abort above could not run when it was most needed (SIGKILL, a
  panic, a closed laptop, an OOM kill all skip a defer) and *nothing in ember
  could find what it left*: an incomplete upload is not an object, so it is in
  no object listing, and `ember ls` prices stores rather than their contents.
  `ember store ls` and `ember store abort` close it, both over GETs and a
  DELETE that cannot spend money.
- **An orphan is priced from bytes, never from whole gibibytes.**
  `StorePrice.Monthly` takes an `int` of GiB, so a 384 MiB abandoned upload
  prices at exactly nothing through it -- and a leak that reports as free is a
  leak nobody removes, which is the entire failure the listing exists to end.
  `MonthlyBytes` rounds up for the reason `Hourly` does, and the rounding is
  what guarantees the property that matters: non-zero bytes at a non-zero
  price never price as zero.
- **An upload in progress and an abandoned one are the same thing to S3.**
  Both are "started, not completed", so anything acting on them is bounded by
  age: `ember store abort` touches nothing newer than `--older-than` (a day),
  and `ember store ls` lists every one but only exits 2 over the older ones.
  Without the bound, `abort` kills the push running in another terminal and
  `ls` exits 2 over a healthy one -- the second being the same false-finding
  mistake the leftover-network sweep avoids by waiting for a node's deadline.
  A negative bound is refused rather than clamped, because it reads as "no
  bound" and would abort an upload that is still running.
- **A paginated S3 listing is followed to the end, and both halves checked.**
  The rule `ScanStores` already follows against RunPod's own pagination,
  applied to `ListObjectsV2`, `ListMultipartUploads` and `ListParts`:
  `IsTruncated` with no cursor is a page ember cannot ask for, so the tail
  would be dropped while the result read as complete; a cursor on a page that
  says it is final is *not* an error and is simply not followed, because real
  implementations send one and following it re-reads that page forever. The
  loop is bounded and hitting the bound is an error naming what would be
  missing. A listing silently short is exactly the leak these calls exist to
  find.
- **The size on a HEAD is the one Go already parsed, and a missing one is an
  error.** `resp.ContentLength` is -1 when the header was absent, and the
  transport validates the header before a caller sees it -- so re-parsing it
  out of `Header` would be a branch no server could reach, and an httptest
  server will not even send an invalid value. Zero reported as fact reads as a
  file that uploaded as nothing, which is one of the states `store verify`
  exists to detect, so it must not also be what a missing header looks like.
- **"I could not ask" and "it is not there" are different answers, on a
  404.** `Head` returns found=false only for a 404; every other status is an
  error. A 403 means the credentials cannot see the store, and reporting that
  as absence sends an operator to re-upload forty gigabytes over a permissions
  problem. `store verify` returns the error rather than counting a finding for
  the same reason `ember ls` exits non-zero on a provider it could not
  reach.
- **`store verify` checks presence and says plainly that it does not check
  digests.** A digest means reading the object back: forty gigabytes through
  this process, over a home link, to re-derive a number the node derives
  anyway at datacentre bandwidth before the workload starts. `do` reads whole
  bodies into a 1 MiB bound, so a streaming GET would be a different code
  shape for a check that already exists one layer down. What it does add is
  the zero-byte case, which is what a PUT that failed after creating the key
  leaves and what `verify: present` would load as a model.
- **`do` refuses a reply larger than it will read.** It used to
  `io.LimitReader(body, maxBody)`, which truncates silently -- fine while
  every reply was a few hundred bytes of XML, and wrong the moment a listing
  page became one: a cut-off page parses as XML right up to where it does
  not, surfacing as a malformed-document error naming neither the size nor the
  cause. It reads one byte past the bound so the two can be told apart.
- **A signature's only other feedback is a 403 with no detail**, which is why
  every step of SigV4 is a pure function checked against AWS's own published
  values rather than against a server. `SignatureDoesNotMatch` and
  `AccessDenied` are both 403: the reason is in the body, and a client that
  reported only the code would hand back the one detail that cannot be
  guessed. The path encoder is hand-written because `url.QueryEscape` writes
  a space as `+` -- every key without a space signs correctly and the first
  key with one fails.
- **`Sign` does not set `X-Amz-Content-Sha256`.** S3 requires it and the S3
  client sets it, but it is not part of SigV4, and signing it in the signer
  would mean this package could never be checked against AWS's published test
  vectors -- which do not carry that header.
- **Push checks what is local and free before what is remote and slow.** A
  typo in a filename fails without a round trip, and the file is hashed last,
  so a missing credential does not cost a pass over forty gigabytes first.
- **Assets are fetched and verified before the workload, and a failure there
  means it never starts.** The cheapest moment to discover that the weights
  are wrong is before the GPU has been asked to do anything with them. It runs
  over the same SSH connection the workload uses, so the output streams live
  and the exit status is known when it happens.
- **A mismatched digest is reported, never repaired by re-downloading.**
  Something wrote to the store and what did is not knowable from the node;
  what is knowable is that the bytes are not the ones this run asked for, and
  that is the moment to stop rather than the moment to guess. Overwriting
  would also destroy the evidence.
- **A download is renamed into place only after its digest matches.** Written
  straight to its final name, a fetch interrupted halfway leaves a short file
  that the next run finds "present" -- and under `verify: present`, loads as
  a model. Every path out of the download removes the partial, because a
  partial on a store is a file nothing cleans up and the operator pays by the
  month to keep.
- **The same argument one layer later, and the answer is that it cannot be
  made there: a LoRA sd-server cannot load is a warning, the image is
  generated and billed without it, and the response says the LoRA was
  applied.** The three entries above are one rule -- the weights are checked
  before GPU time is spent, a mismatch stops the run, and a short file is
  never mistaken for a model. A LoRA is weights by the same definition and the
  rule has no reach there, because the file is read inside sd-server after the
  job has been accepted. Measured rather than reasoned, by
  `TestPreview_ALoraThatDidNotLoadIsNowhereInTheResponse` against a real CPU
  server: a published id whose contents are the line `not a lora` completes
  with `"status": "completed"`, `"error": null` and one image, and the decoded
  pixels are those of the same prompt and seed with no `lora` key at all -- 0
  of 4096 differ, which is #79's "indistinguishable from a LoRA that did
  nothing" turned into a number.

  **The response does not merely omit it, it contradicts it.** The one mention
  of a LoRA in the whole envelope is
  `result.images[].generation_info.loras[]`, and `generation_info.cpp:121`
  builds that array from the request's own params *before* sampling -- so it
  echoes `{"path":"unloadable.safetensors","multiplier":1}` for a LoRA that
  was never registered. The PNG carries the same claim further: the encoder
  writes a `tEXt` chunk under `parameters` reading
  `a paper boat <lora:unloadable:1.0>`, in the spelling every gallery reads,
  so the artifact outlives the rental asserting a setting it was not generated
  with. The two PNGs differ by 105 bytes and every pixel is the same, which
  means a with-against-without comparison done on bytes reports that the LoRA
  did something. This is the `allowedCudaVersions` failure with the roles
  swapped: not a schema believed in place of a response, but a *response*
  believed when it is a transcript of the request.

  **So #79's option 1 -- fail the job -- is unavailable, and it is unavailable
  twice over.** The server's own account is two `[W]` lines on stderr
  (`unknown format`, then `cannot register LoRA source '<path>'`), which ember
  passes to the operator's terminal as `KindLog` and stores nowhere. Reading
  them would be the coupling this file already refuses by name: the status is
  read and the log is not, because the same server writes
  `[E] Diffusion model sampling failed` for a job it reports as `cancelled`.
  It is worse than that precedent, because sd-server's stderr is one stream
  for the whole rental and carries no job id -- so a batch of eight would get
  a warning that cannot be attributed to the image it belongs to. And there is
  no stricter mode to opt into: `ModelManager::LoraSpec` has a `required` flag
  that `model_manager.cpp:120` honours by failing the call, nothing on the
  wire sets it (`parse_lora_json_field` reads `path`, `multiplier` and
  `is_high_noise`, and `apply_loras` leaves `required` false), and only a
  generation extension ember cannot reach turns it on.

  **It is reachable today, which #79 assumed it was not.** The issue closes
  with "until a request can name a LoRA, this is unreachable from ember", and
  that is true of the wire type and false of the config: `extra` merges into
  the `img_gen` body, so `extra: {lora: [{path: ..., multiplier: ...}]}` in
  `ember.yaml` reaches this path on the next rental. What is bounded is the
  web UI, by the same refusal that keeps `extra` off the wire.

  **What the lab is therefore for is the absence.** It enumerates every
  mention of a LoRA in a completed job by JSON path and asserts the set is the
  request's echo and nothing else, so a sd-server that grows a `loras_loaded`,
  a `warnings` array or a per-entry status fails it -- and option 1 becomes
  buildable at the moment it does. Same shape as
  `TestConformance_CannotCatchARederivedRunID`: a limitation nobody can read
  out of the code is a limitation that will be forgotten. Checked by
  reinstating seven worlds in which the claim is false -- a server that
  refuses, a response that gains a field, an echo that lists only what loaded,
  pixels that differ, metadata that stops claiming it, an `error` object that
  appears, and a missing PNG chunk read as silence -- and all seven went red.

  **Options 2 and 3 are not built, and the reasons are different.** Recording
  it against the request (option 2) needs the fact first, and the only carrier
  is the log: a column written from a stderr grep would be a *stored* number
  with the authority of a measurement and the provenance of a guess, which is
  the `load_elapsed_seconds` mistake with worse evidence. Verifying before the
  workload (option 3) is the one worth coming back for, and it is not cheap
  the way `EnsureAssets` is: `capabilities.loras[]` publishes which ids exist
  and says nothing about whether they load, and the only way to find out is to
  ask the server to load one -- which is a generation, on the card, at full
  price. The honest intermediate, if a `lora` field is ever built, is that the
  `slices.Contains` against the published list buys a refusal for a name the
  server does not have and **nothing** for a name it has and cannot use, and
  that the README says so where an operator reads about the field rather than
  leaving it to be discovered from an image that looks untouched.
- **The store is resolved before the spec is built, not after.** Only the
  provider knows what a store's id is, so resolution needs the registry --
  but a spec built first would carry assets with no store and have to be
  patched afterwards, and an invariant that holds only if everybody remembers
  to patch is not an invariant. Doing it first is what lets `NewSpec` refuse
  assets that have nowhere to land.
- **What gets mounted is on the spec, not on the provider.** A store mount is
  a property of the run and not of the account: two runs against the same
  provider can want different stores, and a provider carrying the answer has
  to be rebuilt between them. It travels in `CreateRequest` with everything
  else the run needs, and the placement constraint it implies is expressed by
  narrowing `Filter.Region` rather than by configuring the provider.
- **Half a mount is refused by NewSpec.** A store with no path would be
  attached and unreachable; a path with no store would be a directory the
  workload reads and finds empty. Both look like a run that succeeded and
  produced nothing.
- **The local provider reports no region for its stores, although one was
  asked for and is recorded on the label.** A Docker volume is not in a
  datacentre; it is wherever the daemon is. Reporting the string back invents
  a placement constraint, and the caller acts on it -- a store with a region
  narrows the offer filter, the local provider's only offer is "localhost",
  and every local run then refuses with "no capacity". Found by running it.
- **A store pins every node that mounts it to one datacentre, and the offer
  filter has to be re-read through that site.** `mapOffers` reports
  `bestRegion(g)` — the single site with the most capacity — so filtering its
  output by the store's region would have rejected a card that is rentable
  exactly where the weights are. A live RTX 4090 reads `NONE` overall and
  `LOW` in US-IL-1; with a store in US-IL-1, the filter-afterwards version
  answers "no capacity" for a card you could have rented. `regionOffers`
  rewrites each entry to its own site's availability instead. Same shape of
  bug as the one `hasCapacity` exists to prevent, one layer up.
- **A store's datacentre overrides the configured placement list, it does not
  intersect with it.** An intersection can be empty while the store's own site
  has capacity, and a placement list excluding the store's site is a request
  the provider can only refuse.
- **An empty offer list with a store attached is a refusal, not an empty
  list.** "No offers matched" reads as "that GPU is sold out everywhere" and
  sends the operator to try another card; the real answer is that their
  weights are in the wrong site, and the fix is a datacentre.
- **A paginated listing is followed to the end, and both halves are checked.**
  `hasNextPage` without a cursor is a page ember cannot ask for; following a
  cursor while `hasNextPage` is false re-reads the last page forever. The loop
  is bounded, and hitting the bound is an error saying the listing is
  incomplete — a listing silently missing its tail is exactly the leak
  `ScanStores` exists to prevent.
- **A created store whose label does not come back is a refusal.** A store
  ember cannot recognise is one `ember ls` will not report and
  `ember store rm` will not remove: a leak created by the command meant to be
  the careful one.
- **A provider may have two APIs that disagree about field names.** RunPod
  serves network volumes at `api.runpod.io/v2/network-volumes` *and*
  `rest.runpod.io/v1/networkvolumes`, and the same field is `dataCenter` on
  one and `dataCenterId` on the other; the published documentation describes
  only the second. The minimum size is 10 and not the documented 0. All of it
  came from one free `GET` and one deliberately-invalid `POST` made before any
  client code existed — which is the cheapest calibration in this repository
  and the model for the rest. See `internal/provider/runpod/testdata/README.md`.
  It happened a second time, with `allowedCudaVersions`: `rest.runpod.io/v1/pods`
  accepts it, `api.runpod.io/v2/pods` — where ember creates — declares eighteen
  properties with `unevaluatedProperties: false` and answers `422` to anything
  else. `min_cuda` was built from the published schema and made every create
  fail. The lesson did not stick the first time because the field was read from
  a *schema* rather than from a *response*.
- **And it did not stick the second time either, in the other direction:
  `cudaVersions` IS in the captured catalogue, and three comments said it was
  not.** `runpod.go`, `config.go`'s `min_cuda` doc and `README.md` all read
  "the live catalogue does not return it", all written on 2026-10-31, while
  `internal/provider/runpod/testdata/gpus.json` -- a live capture committed on
  2026-10-29, four directories away -- carries the field on three of its four
  entries. The schema was consulted where the response was already in the
  tree. **`min_cuda` still cannot be enforced, and now for a measured reason
  rather than a wrong one.** `available` tracks the card's top-level
  availability summary on every captured row, so the RTX 4090 that reads
  `NONE` overall and `LOW` in US-IL-1 -- the row this repository already
  records as rentable while its summary says otherwise -- reports **all eight**
  of its CUDA versions unavailable: a floor built on the flag refuses a card
  you could have rented, for *every* value of `min_cuda`, including values the
  card satisfies, which is the bug `hasCapacity` and `regionOffers` exist to
  prevent reached through a different field. Read without the flag the versions
  describe the catalogue and not the host the pod lands on, and upstream
  describes them as suitable for `gpu.allowedCudaVersions`, the one create
  property that answers 422; the field is on `GpuType` rather than on
  `dataCenters[]`, so it cannot be re-read through a store's site either. So
  the finding is an **assertion over the capture** and not a reworded sentence
  -- a comment that justifies the entry somebody remembered cannot catch the
  entry somebody forgets -- and it fails if a later capture breaks the
  correlation, which reopens the question on evidence. `node.CUDAAtLeast`, the
  config key and RunPod's refusal are all untouched: removing a documented key
  is operator-visible and belongs to whoever reads this, not to the change that
  found the comment was wrong. **What is still unobserved** is what `available`
  means, and whether any host anywhere reports a CUDA *runtime* version ember
  could compare against: `nvidia-smi --query-gpu=compute_cap` is a compute
  capability, which is `node.ComputeCap` and a different number, and
  `pod_running.json`'s `"cudaVersion": "12.4"` is schema-derived and not
  decoded.
- **Nothing is compiled on a rented machine.** The program that runs on a node
  was compiled before the rental began. `CUDA_ARCHS` is gone and
  `CMAKE_CUDA_ARCHITECTURES` is `NN-real`, so the binary carries machine code
  for exactly one architecture and no PTX: PTX JITs forward only, so it can
  never help an older card, and on a newer one it means the driver compiling
  150 kernel modules at first launch, on a cold cache, on every pod, billed by
  the second and reported by nothing. Same rule and same reason as the ban on
  Python and other scripting languages — a runtime that compiles or interprets
  on arrival is buying a compiler with GPU time. If a toolchain cannot emit
  code for a card ahead of time, the answer is a newer toolchain.
- **A wrong-architecture image aborts, loudly and too late.** Measured on two
  rented cards and once locally: ggml raises `no kernel image is available for
  execution on the device` and sd-cli dies with SIGABRT about a second into the
  first generation. It does *not* silently run on the CPU — seven comments in
  this repository used to say it did, and they were wrong. Three failure modes
  were being treated as one, and they are not — measured on the same card with
  the same model:

  | what is wrong | what happens | exit |
  |---|---|---|
  | no GPU passed to the container | succeeds, silently, on the CPU | 0 |
  | GPU present, matching architecture | works | 0 |
  | GPU present, wrong architecture | aborts on the first kernel launch | 134 |

  Only the first is the silent-CPU story, and it is the one `internal/gpucheck`
  and `dockerengine.RecordingEngine` are written about — correctly. The third
  aborts, but it aborts after a node exists, the image is pulled and the
  weights are fetched, all of it billed. So `images:` refuses a card it has no
  entry for before renting, and never falls back to `image:`.
- **The image is not built for the machine that builds it.** ggml defaults
  `GGML_NATIVE` to on, which is `-march=native`. From the same Dockerfile and
  the same source: 0 AVX-512 instructions built on a Zen 3 desktop, 23,458
  built on a GitHub runner. A rented RTX A4000 answered `Illegal instruction
  (core dumped)` after CUDA had already initialised, while the same tag had
  generated an image on another host twenty minutes earlier. The identical
  digest doing different things on different machines is worse than the
  `:latest` problem pinning exists to prevent, because nothing in the reference
  can warn you. `GGML_NATIVE=OFF`, and the build refuses a single `zmm`.
- **An entry in the card table is a claim about hardware, so rent the card.**
  The old lab table grouped `"NVIDIA B200 / B300 SXM6"` at `sm_100`. A B300
  reports compute capability **10.3**. Twenty-four cents of a $8.99/hour card
  settled it, against every future rental of one aborting on a missing kernel.
  The two RTX PRO 6000 MIG partitions went the other way: excluded because
  nobody had tried `sd-cli` on a slice, and one rendered at 9.73 it/s. Both
  answers cost two minutes.
- **The image's `ENV` does not reach anything sshd starts.** ember runs the
  probe and the workload as `ssh host command`, and sshd builds a fresh
  environment per session: `EMBER_CUDA_ARCH=89` is in the image, visible to
  `docker run`, and empty over SSH. This Dockerfile already knew it about
  `LD_LIBRARY_PATH` three lines away. So the architecture is written to
  `/etc/ember-cuda-arch` as well, and the image lab asserts the *file* through
  `ExecOut` — the question is what a workload can see, not what the image
  declares. A probe comparing a card against an empty string refuses every run,
  including the correct ones.
- **`gpu_probe` as documented does not catch a wrong architecture.**
  `sd-cli --list-devices | grep -qi cuda` exits zero on an image built for
  another card, because listing devices asks the driver what hardware exists
  and launches no kernel. Measured: the probe passed, the workload started, and
  it aborted after a 1.6 GB weight fetch. A probe that compares
  `/etc/ember-cuda-arch` to the card's own `compute_cap` refuses it — verified
  in both directions, rejecting sm_75-on-sm_89 and admitting sm_120-on-sm_120.
- **The overhead is per rental, not per result.** Ten 1024x1024 FLUX.2 images
  generated locally spent 4143 s wall against 3872 s of sampling: 27 s of
  overhead per run, 7%. Tolerable here, because nothing is pulled and 56 GiB of
  weights stay in the page cache so a reload costs 0.05 s. On a rented A100 the
  same shape is about three minutes per rental -- provisioning, image pull,
  store read and verify, model load -- against roughly 100 seconds of
  generation, so ten images rented ten times spend more than half the bill
  re-doing setup. `cmds` runs them on one node and pays it once. The cost was
  worst exactly where ember is supposed to help.
- **A batch brings each result home as it is produced.** Fetching once at the
  end means an expired TTL, a dropped connection or a closed laptop loses
  everything rather than everything so far -- and an operator cannot look at
  the third image and decide to stop paying for the other seven. A ten-step
  batch is over an hour of a rented card, which is a long time to be holding
  every result on a machine that is about to be destroyed.
- **The fetch bound is a file on the node, never a timestamp from here.**
  ember's clock and the node's are different clocks, so `--newer=@<unix
  computed here>` sends too much or too little whenever they disagree -- and
  "too little" is silent data loss. A marker touched on the node is compared
  against mtimes from that same clock, so skew cannot enter. GNU tar spells it
  `--newer`; `--newer-than` is not a flag and fails the whole command, found by
  running it against the node image rather than by reading the man page.
- **A failed step stops the batch, and that step's output is still fetched.**
  A batch where step 3 failed and 4 through 10 ran anyway is a batch nobody can
  trust, and the remaining steps bill for work the operator no longer wants.
  The output of the step that failed is often the one that explains why, so it
  is collected before the stop -- on a context that survives Ctrl-C, for the
  reason every other fetch is. **A marker that cannot be touched is wasteful,
  not fatal:** the next fetch sends everything again, which costs bandwidth and
  loses nothing.
- **A step reports card time, not its share of the bill.** The rental also
  paid for provisioning, the image pull and the asset verify, and a batch pays
  those once for all of it rather than once per step -- which is the entire
  saving. Folding them into a per-step figure would overstate it, so the
  wording says "of card time". It is timed across the fetch as well as the
  workload, and not from `workload.Run`'s own duration: the card stays rented
  while the output comes over the wire, so a figure that stopped at the
  workload would understate every step that moved anything large -- which is
  the direction a money figure must never be wrong in.
- **`max_run_usd` cannot name the step it bit on, because it bites before any
  step exists.** The issue asked for that message; the premise was wrong.
  `Budget.Allow` is checked against the offer's price times the TTL at
  selection time, so a batch does not make it fire later -- it is a ceiling on
  the rental, not a running meter. A batch that needs longer needs a larger
  `ttl`. Written down because the request looks reasonable and implementing it
  would have meant inventing a mid-run meter nothing else needs.
- **`rejectSecrets` does not scan `cmd` or `cmds`, and should not.** The
  markers are substring matches on lowercase -- `secret`, `token`, `password`
  -- and a workload's argv carries prompts, which are arbitrary English. "A
  token of affection" is a legitimate prompt and refusing it would be a false
  positive on the one field an operator edits most. The argv rule is enforced
  where it belongs instead: ember passes no *credential* as a flag, and
  `/proc/<pid>/cmdline` being world-readable is why.

- **One cost `cmds` cannot remove, and the one place ember stops being
  workload-agnostic.** Each entry in `cmds` is a fresh `sd-cli` that reads the
  weights into VRAM again; `sd.serve` holds one `sd-server` and they stay
  resident. Measured on an RTX 2080 Ti with sd15 Q8_0 at 256x256 and four
  steps, same prompts and seeds both ways:

  | jobs | `cmds` | `sd.jobs` |
  |---|---|---|
  | 1 | 3.68 s | 9.72 s |
  | 3 | 7.37 s | 10.60 s |
  | 8 | 16.40 s | 12.78 s |
  | 16 | 30.82 s | 16.23 s |

  Marginal cost per job: **1.81 s against 0.43 s**, consistent across 1→8 and
  1→16, against a fixed **~6 s** to start the server. So the crossover is at
  about **five jobs**, and above it the saving grows -- 22% at eight, 47% at
  sixteen, approaching 4.2x per job. All sixteen images are byte-identical
  between the two paths, which is what makes the comparison a comparison.
  **The ~8% this was planned around was arithmetic from the issue's A100
  table, and it was wrong in both directions**: the per-job saving is far
  larger than 8%, and there is a fixed cost that makes a short batch slower.
  The cost of getting it is that ember now knows what a sampler is: progress
  streaming is impossible for a program that refuses to read its own payload,
  and a `curl` in a config is the same knowledge maintained by hand. `cmds`
  stays as the general mechanism, because a typed sd path cannot run `ffmpeg`.
- **`cmds` does chain, through the node's filesystem, and chaining video with
  it is still wrong.** #20 was filed on the premise that `cmds` entries have
  "no way to name a file the step before produced" -- which was a reasonable
  reading of "opaque argv, fetched home between steps" and is false. The fetch
  is a *read*: `TarCommand` builds `tar -czf - -C dir --newer=marker -- base`,
  there is no `--remove-files`, and the marker bounds what is **sent** and not
  what **exists**. So every step after the first starts with every earlier
  step's output underneath it. Checked rather than reasoned about, on the
  local provider for nothing: step 2 listed `clip01.txt`, read it, wrote a
  file derived from it, and the two fetches carried 18 B and 31 B -- each file
  exactly once. What `cmds` cannot do is the part that needs ember to
  understand a result: a filename it did not know in advance, a last frame it
  would have to decode, a step counter inside a clip that samples for minutes.
  That is the same seal as the entry above, and it is the claim worth keeping.
  The reasons not to chain video there anyway are an `ffmpeg` seam per clip
  maintained by hand in YAML (the `curl`-in-a-config again, eleven times),
  and a chain whose intermediate state dies with the node -- so a failure at
  clip 7 can only be resumed by paying for clips 1-6 again, and the `ttl` and
  `max_run_usd` decision is made once, up front, over a rental of hours. Per-
  clip overhead is **not estimable** from the figures above: both are images,
  Wan 2.2 is two 14 GB experts, and scaling an sd15 number would be the "~8%"
  mistake. `cmds` is the instrument that settles it -- two identical clips,
  and the difference between the per-step card-time lines is the reload.
  `docs/long-video-is-a-pipeline.md` has the rest.
- **Nothing warns an operator whose `cmds` entries look like a chain, and the
  reason is not only the seal.** Detecting one means reading argv, and a
  heuristic over a prompt and an output path would be the `NormalizeGPU`
  substring failure with deliberately arbitrary input. The stronger reason is
  that the warning would be **false**: the entries do chain. Teaching an
  operator to distrust a mechanism that works is the cost
  `allow_interruptible` already pays for reading like a safety switch while
  guarding nothing. The real gap was that the persistence was an accident of
  implementation nothing stated and nothing checked, so it is written into
  `README.md` beside `cmds` and asserted by
  `TestTransfer_AFetchLeavesTheOutputsOnTheNode`. The tidy-up that would
  break it is obvious -- `tar --remove-files` to spare the node's disk, or an
  `rm` between steps -- and without the lab it would land green.
- **The client's own poll interval hid the entire saving, and only a
  measurement found it.** A flat two-second poll noticed a 0.3-second job up
  to two seconds late, so the marginal cost per job came to 2.08 s served
  against 1.81 s for a fresh process -- the resident model saved ~0.8 s of
  tensor loading per job and ember then spent more than that waiting for it.
  The served path measured **slower at every batch size tried**, 1, 3 and 8,
  with the gap widening. Replacing the flat interval with 50 ms doubling to a
  2 s cap took the marginal to 0.43 s. A fixed cadence is a guess about how
  long the work takes; this one was wrong by a factor of six, and no unit test
  could have said so because the fakes return instantly.
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
  machine's public interface -- the ComfyUI exposure this threat model is
  written against. Same rule and same reason as `tunnel.RemoteLoopback`, and
  nothing binds locally either: ember dials the node's own loopback through
  `direct-tcpip`, so there is no `ports:` entry and no listener outside
  `internal/loopback`.
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

## Style

Comments explain *why*, and name the defect that motivated them — see
`node.NormalizeGPU`, where substring matching once turned `"A100 SXM4 80GB"`
into `a1004-80g` and made a whole provider look like it had no capacity.

Prefer making a bug unrepresentable over testing for it. Fail loudly rather
than skip quietly. Allow-lists for anything dangerous. Parse, don't grep. Pin
external inputs.

Error strings are lowercase, wrapped with `%w`, and carry the remediation.
`main` prints `ember: <err>` to stderr and exits 1, or 2 when there are
findings, or 3 on a usage error.

Document CLI flag, config key and behaviour changes in `README.md` in the same
commit.
