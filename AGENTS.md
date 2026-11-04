# ember — conventions

This project is **Go-first**, like `go-tun`, `transcription` and `sms-gateway`.

- Go **1.27** for all application code and tooling. `go.mod` pins it; the
  Makefile prefers `~/sdk/go1.27.0` so the compiler cannot drift from the
  declaration just because a system package updated.
- Prefer the standard library and the dependencies already here over new ones.
  The list is `golang.org/x/crypto`, `github.com/docker/docker`,
  `gopkg.in/yaml.v3`, `golang.org/x/tools` (the coverage gate, and since #165
  the wiring guard in `internal/cli`, which needs `go/packages` because its
  subject is which VARIABLE a struct field was handed and that is a question
  about `types.Object` identity rather than about a spelling) and
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

  **The same finding one API over, and that one was airtight rather than
  merely untested: `ember store`'s seven subcommands and the store pre-flight
  were paths no lab could reach.** There was no `/v2/network-volumes` route
  either, so `store create`, `rm`, `push`, `sync`, `ls`, `verify` and `abort`
  were exercised against RunPod only by the one-off canned handlers this
  package exists to replace. The pre-flight is the sharper half, because
  nothing about it was a coincidence: every store lab in the tree runs on the
  **local** provider, `local.go` reports `Region: ""` for a Docker volume --
  correctly, it is not in a datacentre -- and `CheckStoreAssets` skips on an
  empty region by design. So it returned `Asked: false` on every lab there is,
  and the suite could not tell the check from its own absence.
  **Measured rather than inferred, and the first wording of this entry was
  wrong about half of it**: with the pre-flight patched never to run, three
  unit tests go red -- `TestUp_RefusesAStoreMissingTheWeightsBeforeChoosingAnOffer`,
  `TestUp_AnEmptyObjectIsNotTheWeights` and
  `TestAppPlanner_FillsAnEmptyStoreOnAMachineWithNoCardRatherThanRefusing` --
  and **106 of the 107 labs pass, the one failure being the new one**. So the
  unit suite did report it and the lab suite reported nothing, which is the
  claim worth making; "`go test ./...` was green too" was an inference from
  one targeted run and is not true.

  **A volume is one host DIRECTORY, and that is the whole point rather than an
  implementation detail.** The thing the pre-flight turns on is a join: the
  sync fills the store by running `embernode fetch` on a rented machine, over
  SSH, into a mounted directory, and the next plan asks the store's own
  endpoint whether that worked. Two in-memory maps -- which is what
  `internal/queue`'s and `internal/cli`'s httptest stand-ins are -- agree with
  each other whatever either half does, the closed loop `capabilities.model`
  sat in. So `Create` honours `mounts.network` by bind-mounting that
  directory, the S3 endpoint serves object calls by reading it, and the asset
  the node downloads is the asset the endpoint lists. Widening the `Nodes`
  seam to carry the mount is the same rule as the fake not being tidier than
  the service: accepting `mounts.network` and starting a container with
  nothing at that path would make a full store and an empty one the same lab.
  **SigV4 is verified by re-signing** -- `awssig.Sign` over a reconstruction
  of the request, the whole `Authorization` header compared -- because a fake
  that accepted an unsigned request cannot catch a signing regression, and a
  403 with no detail is the only other feedback that algorithm has. The
  reconstruction is filtered by `SignedHeaders`, which is what a real server
  does and is not optional: Go's transport adds `Accept-Encoding: gzip`
  **after** the client has signed, so a verifier that hashed every header it
  received rejects every correct request.
  `internal/provider/runpod/testdata/README.md` has the per-behaviour table
  and says which of them were observed, which are inferred, and the two that
  are deliberately not reproduced.

  **Two gaps the lab measured on its first run, both asserted rather than left
  to be noticed.** `fillStore` calls `SyncAssets` with no sink and writes no
  `genstore` row, so `ember history` and `ember spend` are blind to a rental
  the queue started on its own initiative -- the ledger shows one rental where
  two were paid for, against this file's own "a total that is missing part of
  its history says so, every time". And `Queue.once` calls `fail` only when
  `Planner.Plan` fails: the status is written inside `WithServed`'s closure by
  `drain`, so **every** failure after the plan -- the create, the wait for
  sshd, the gpu probe, the asset verify, the model load -- leaves the row with
  the `running` `ClaimNext` wrote and no rental behind it. The queue table
  shows it running for the life of the process and its cancel button reaches
  the provisioning-window 409, whose message says the rental is coming up when
  nothing is; `Run`'s `Interrupt` sweep clears it only at the next start. Both
  are asserted the way `TestConformance_CannotCatchARederivedRunID` is -- a
  limitation nobody can read out of the code is a limitation that will be
  forgotten -- so a change that records either turns the lab red, which is the
  moment to read the comment beside the assertion.

  **BOTH GAPS ARE CLOSED NOW AND THIS PARAGRAPH IS THE RECORD OF WHAT THEY
  WERE, not a description of the code.** The arrangement worked exactly as it
  was written to: #149 fixed the second and #148 the first, each turned this
  lab red, and each inverted the assertion rather than deleting it -- so the
  lab asserts the terminal status and the reason on a rental that failed after
  the node existed, and two ledger rows with the cheap machine's own cost in
  `ember spend`'s period total. The entry at the end of **Money** has #148's
  design call and what the reinstatement measured. Left in place rather than
  rewritten because a prediction this file made and the code then met is worth
  more than a tidy present tense; the tense is corrected here so nobody reads
  the sentences above as a gap that is still open.

  **It drives the queue in process, and that is forced rather than chosen.**
  `app.Deps.CloudBaseURL` and `S3BaseURL` are Go fields, so the compiled
  `ember` cannot be pointed at a fake provider -- which this file already
  records as the reason the offers panel's error path has no lab. What the lab
  builds is `cmd_serve.go`'s own wiring field for field: the same
  `queue.Queue`, the same `queue.AppPlanner`, the same `genstore`. The one
  thing it cannot reach is the `internal/cli` verbs themselves, because
  `runStore` is unexported, so `store verify`'s and `store ls`'s own calls --
  a HEAD per asset, `ListObjectsV2`, `ListMultipartUploads` -- are driven
  through `s3.Client` instead.

  **The asset server binds the Docker bridge gateway, which is the one
  listener in these labs that cannot be loopback.** `embernode fetch` runs
  INSIDE a container, where 127.0.0.1 is the container, and `httptest` binds
  loopback -- so the address is the bridge's own gateway, read from the daemon
  rather than assumed to be 172.17.0.1 (a daemon with another `bip` answers
  something else, and a guessed address fails as a timeout naming nothing).
  Measured before it was built: a container on the default bridge reaches a
  host listener on that address, and the listener is reachable from nothing on
  the LAN. `nolisten_test.go` exempts test files for exactly this.
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

  **`AssertNoStray` is gone, and the name in this document is now the name of
  the lesson rather than of a function** -- the `queue.Cost` arrangement.
  Repairing the ordering above meant spelling snapshot-reap-report in the
  cleanup, because a zero-argument method cannot carry a snapshot taken before
  the reap, and the method was left behind: for the life of the suite it had
  **no caller**, while fourteen comments -- six labs, eight in `internal/` --
  and this entry named it as the thing guarding them, and its own doc comment
  said "it is also run from the lab's own cleanup" (#113). The property was
  checked the whole time, by `reportStray`; what was wrong was that a reader
  following the name arrived at a function that never ran. Nothing automated
  reported it, and that is structural: `TestNoProductionDeadCode` does not
  load `test/`, because a lab caller and no caller at all are the same verdict
  to it -- so the harness is the one tree where this shape has to be found by
  reading, and that refusal is one defect older than it was.
  **Wiring it up was tried first and a measurement refused it.** #113's own
  suggestion was the mid-test home: the three labs that leak on purpose call
  `Reap()` themselves to declare the orphan, and it looks as though nothing
  can then say whether that reap was complete, since the cleanup's snapshot is
  taken afterwards. It can, and the "afterwards" is why -- the snapshot lands
  after the body's reap and *before* the cleanup's own, so whatever a declared
  reap left is exactly what it sees. Reinstated, a `Reap` that keeps its
  containers: both orphan labs go red at `reportStray` with no mid-test
  assertion anywhere, so an `AssertNoStray()` beside their `Reap()` reported
  the same objects one line earlier and nothing else. That is `queue.Cost`
  and not `sdserver.Client.Cancel`: a second path for one check, where the
  other path is the live one. The two judgements worth keeping -- snapshot
  before the reap, and over this suite run's objects rather than the daemon's
  -- are a note where the function was, the `node.RateFromFloat` shape,
  because whoever adds the next stray check makes them again.
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
  releases it when the holder dies, and three labs here SIGKILL ember on purpose;
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

  **`TagImage` is the same rule where the stray check cannot reach at all, and
  the lab holding it asserted neither half of its own name.** A stand-in tag --
  `ember:lab-sm75` and the two beside it, for the labs about which image ember
  *chose* -- is removed by name and never by force, because a forced remove
  takes `ember:lab` out from under every lab after it. Nothing else can see
  either outcome: `labdaemon.Objects` has containers, networks and volumes and
  **no image kind**, `make reap-labs` goes through that same listing, CI's
  stray step is `docker ps -aq`, and a tag inherits its source's labels, so a
  stray one is indistinguishable from `ember:lab` by label. The harness comment
  saying "ownership is the only mechanism there is" was right, and
  `TestImageSelect_AStandInTagSurvivesNothingAndLeaksNothing` -- the lab holding
  that ownership -- checked that the tag existed and that `ember:lab` had not
  been removed **inside its own body** (#114), which are properties of
  `docker tag` and not of the cleanup it is named for. A `TagImage` whose
  cleanup stopped working would have leaked three names onto the developer's
  daemon on every suite run, for good, and that lab would have stayed green.
  It is a cleanup registered *before* `TagImage` now, so it runs after that
  one -- `CreateVolume`'s ordering -- and the in-body checks stay, because
  without them both post-conditions would pass against a tag that was never
  created at all, which is a refusal consistent with refusing everything. **The
  leak is three image NAMES sharing every layer with `ember:lab`: no bytes, no
  money, nothing a later run trips over**, so the answer is an assertion in one
  lab rather than a fourth kind in `labdaemon`, and what was being fixed is a
  name that claimed more than the code. Checked by stopping `TagImage`'s
  cleanup from removing the name it created: red with the assertions, green
  without them.
- **A lab that leaks on purpose says so.** The SIGKILL labs exist to leave a
  node behind, and the first run of the repaired stray check reported them.
  They call `Reap()` themselves once they have finished asserting, which
  declares the orphan as deliberate and keeps the check unconditional for
  every lab that leaks by mistake. Relaxing the check instead would have put
  it straight back where it was. Their orphan is still reaped under the scoping
  above, and must be: it was created during this suite run, which is the only
  thing that distinguishes it from the stale container of a run that died
  yesterday -- and that one is left alone because nothing here made it.
  **There are three of them and this entry said two**: `ember serve`'s,
  `TestOrphan_SigkillLeavesExactlyOneFindableNode`, and
  `TestOrphan_DryRunReapDestroysNothing`, which kills ember as well and reaps
  from a `defer`. The declaration does not turn the final check off, which is
  the thing a reader would reasonably assume and is measured two entries up:
  the cleanup's snapshot is taken after a lab's own reap, so an incomplete one
  is reported against the lab that declared it.
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

  **`internal/loopback` HAS been widened now, it is additive, and every clause
  of the paragraph above is still true except the one about the seal -- which
  was never true of the code.** The two things that changed are that #29
  shipped, so the socket is a *choice* rather than the only way to close the
  every-uid case, and that #146 needs a channel a **browser cannot reach** --
  which is the one property that paragraph lists as a drawback and is, for a
  CLI client, the whole requirement. So the socket is a second channel beside
  the port rather than instead of it: the token stays mandatory, `webapi.New`
  still refuses a zero one, and the token is still the only answer on TCP.
  The forward argument is why: `ssh -L 7777:localhost:7777` puts a TCP port on
  a laptop's loopback and a socket on this machine says nothing about it, so
  deleting the credential would have reopened exactly the threat it was built
  for. `SO_PEERCRED` is still not used and still not needed -- the mode is the
  check, applied by `connect(2)` before ember sees a byte, and it needs no
  code at all.

  **The clause that was untrue: `nolisten_test.go` did not have to stop
  banning `ListenUnix`, because the ban was already "outside this package".**
  The walk skips any file whose path starts `internal/loopback/`, so
  `net.ListenUnix` and `net.Listen("unix", …)` were always legal here and
  illegal everywhere else; `StateSocket` needed no loosening and got none.
  What the entry read as a cost was a cost that did not exist -- the same
  shape as `ember ls --all`, an argument that persuades about a mechanism
  nobody checked. So the re-seal is a **correction and a test** rather than a
  change: the map's comment said "entry points in net that can publish a
  **port**", which stopped being the rule the day a path could be published,
  and the inspection called `t.Errorf` inline so the only evidence it worked
  was a clean tree -- which is also what a broken matcher reports.
  `findListens` is extracted, and `TestNoListenGuardCatchesEverySpelling`
  hands it eleven spellings that must be reported, the unix two among them,
  and five that must not. **The negative control is the half that matters**:
  the first version of this guard flagged `loopback.Listen`, the one call it
  exists to permit, so the correct spellings go through the same function and
  must come back clean. Checked by pasting `net.ListenUnix` and
  `net.Listen("unix", p)` into `internal/webapi` and watching the walk report
  both.

  **`Socket` is `Bind`'s shape and the directory is the caller's only say.**
  Unexported fields, no struct-literal path, `StateSocket` the one
  constructor: a caller names a **state directory**, the way a caller of
  `Port` names a port, and this package appends `socket/serve.sock`. There is
  no `--socket` flag and a lab asserts there is none -- not because the path
  is a secret, it is not, but because a path an operator can type is a path
  outside the directory whose mode ember checked.

  **The mode is the authentication, and `bind(2)` takes no mode argument.**
  That is the one thing about this that is not obvious: the socket is created
  at `0777 &^ umask`, and measured on this machine, whose umask is 0002, a
  socket bound with no intervention is **0775** -- group-writable, and
  `connect(2)` needs *write* permission, so every member of the operator's
  primary group could have reached it. A socket chmod-ed to 0000 refuses its
  own owner, measured, so the mode is a live control rather than advice. The
  obvious repair is `os.Chmod` after the bind, and **between the bind and the
  chmod the socket is already connectable** -- so `ListenSocket` holds the
  umask at 0177 for the length of the bind instead, under a mutex, and the
  socket is 0600 from the moment it exists. What that window costs is
  measured rather than waved at: umask is per-process, so for the length of
  one `bind(2)` anything else ember creates would come out at 0600/0700 --
  and every mode literal in ember's own packages is **already 0600 or 0700**,
  28 of them with not one wider, so nothing can come out different and
  anything that did would come out stricter. It also leaves no uncoverable
  branch, which a chmod's error return would: `internal/loopback` is gated at
  100 and a failure on a file this process has just created in a directory it
  owns is the class this repository excludes elsewhere by argument.

  **Containment twice, by two mechanisms, as the tar extractor is.** The
  socket is 0600 *and* it lives in a directory ember creates at 0700 whose
  parent is refused if group or others can write in it -- so the mode holds if
  the directory is widened and the directory holds if a filesystem ignores the
  mode. The directory check is `keys.LoadSigner`'s rule about a private key's
  parent reached through a socket, and **stricter than its `0o002`** on
  purpose: that one guards a file an operator placed, where a group-writable
  directory may be something they meant, and this guards a directory ember
  makes for itself, so there is no wider mode to tolerate. What a group member
  could do with it is the sharp version -- unlink the socket and leave *their
  own* listener at ember's path, which hands the operator's requests, and the
  rentals they pay for, to somebody else. A subdirectory rather than the state
  directory itself for exactly that reason: ember creates this one and
  therefore owns its mode, while `state_dir` is operator-named and may predate
  the feature.

  **"Arrived on the socket" is a context value and there is no wire spelling
  of it.** This is the one way the change could have been worse than no
  change: a header would be accepted on the loopback port too, and the
  mandatory credential would have been replaced by a string anyone on the box
  can type. The key is an unexported zero-size type used as its own value, so
  nothing outside `internal/webapi` can put it on a context; it is set by one
  `ConnContext`, on one `http.Server`. **Two servers and not one that inspects
  the connection**, because a type switch on a `net.Conn` re-derives the fact
  from the connection where the *listener* is what `loopback` proved -- so the
  difference between the two channels is a missing field rather than a
  condition. And `ServeSocket` takes a `*loopback.SocketListener`, a concrete
  type whose only constructor is `ListenSocket`, so handing it a TCP listener
  is a **compile error** -- `hostkey.Pin` and `exposure.PortRequest`'s shape,
  applied to the thing that would otherwise drift into "either channel is
  fine".

  **`checkOrigin` still runs on the socket and `checkHost` does not, and that
  is a decision rather than a consequence.** `hostOK` is the one place it is
  written, for `guarded` and for the websocket handshake both, because two
  copies of "unless it came in on the socket" is two places for one to be
  forgotten. `checkHost` compares the `Host` header against the port this
  server listens on, and a request that arrived on a socket came through no
  port -- `curl --unix-socket` sends `Host: localhost`, which does not parse
  as host:port at all -- so requiring it would not be a stricter check, it
  would be a channel that refuses everything. `checkOrigin` is about who
  **composed** the request rather than where it arrived, costs nothing, and
  through a proxy an operator puts in front of the socket it is the only thing
  left that refuses a page on another site. The cookie and the `?t=` exchange
  are left reachable and unrefused: both are browser mechanisms on a channel a
  browser cannot open, so a branch for them would be one only the account that
  is already authenticated could take.

  **Windows is a refusal and not a socket, and it is a build tag rather than a
  runtime check.** `net.Listen` there will bind AF_UNIX and `os.Chmod` honours
  only the read-only bit, so the socket would exist, the announcement would
  describe a channel authenticated by the operating system, and the one thing
  authenticating it would not be set -- the "reads as working" defect, on a
  security control. `//go:build !unix` returns an error naming the platform
  and the mode, so there is no branch anywhere that could conclude at run time
  that this machine cannot have a socket when it can.

  **The gain is asserted in two halves and the third is a deduction, which is
  said rather than implied.** The sentence this entry opens with is "answers
  the every-uid case", and what proves it would be a connect from a second
  account -- which a test run cannot make without root, and a lab that needed
  root would be a lab nobody runs. So the socket's 0600 and the directory's
  0700 are asserted against what a real `ember serve` created, `connect(2)`
  needing write permission is measured by hand, and "another uid cannot
  connect" follows from the two. It is a deduction from two measured facts and
  not a third measurement; there is no test in this repository that a second
  uid is refused.
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
- **And the page told every operator there was no authentication for the whole
  of that time, which is the `ember ls --all` class in the running program's
  own UI rather than in a comment or a document (#167).**
  `web/src/views.rs`'s posture line
  ended `· no authentication: any page in this browser can reach this port`,
  unconditionally -- outside the `if c.allow_paid` above it, so on screen in
  both postures on every page load -- and `Posture`'s own doc comment rested
  the `--allow-paid` default on "what bounds the damage of there being no
  authentication". A verbatim survival of the state #29 closed: `webapi.New`
  refuses a zero token, a page on another site fails `checkOrigin` before the
  credential is asked for, and `SameSite=Strict` means a cross-site request
  carries no cookie at all, which is the half that clause was about. **The
  distinguishing claim is the SURFACE and not the audience**, and the first
  wording of this entry had it as "read by whoever next edited the code"
  against "read by the operator" -- which is wrong about this file's own
  record two hundred lines up: the `--all` on an `ember ls` and the
  `ember up --config ember.local.yaml` naming a file that never existed were
  both in `README.md`, which operators read. What is new is that this one was
  in the **program's own UI**, printed by the running process to the person
  deciding whether to walk away from it, where a document is read before the
  decision and a comment is read by nobody making it. And it was wrong in the
  direction that makes a working control look absent. `README.md`'s own
  paragraph was corrected when the token landed and the page was not, which is
  "a claim is worth exactly what the code does" with the two surfaces one
  commit apart.
  **The replacement is `announceToken`'s two limits and not its two
  mechanisms, and the reason it is on the page at all is #105's**: that
  paragraph prints the control and its limits into a terminal, and the
  operator this page is for may never see one -- the one-time URL means
  somebody had a shell, and `ssh -L 7777:localhost:7777` means that shell is
  on the other machine. So "every request on this port needs a token; it does
  not keep out your own uid or root, and it never expires", all three spans,
  because a control whose limits are not printed is a control that gets
  believed past them and a line that said only the first would be the old
  defect inverted. Not a deletion, which was the other candidate: a warning
  naming a condition the program cannot be in is worse than silence, and that
  is an argument about the old clause rather than about a true one.
  **THE FIRST VERSION KEPT ONE LIMIT OF THE TWO AND CALLED ITSELF THAT
  PARAGRAPH COMPRESSED**, which is the same shape one scale down: non-expiry
  was dropped, it is the more surprising of the two and the one that bears
  hardest on "should I leave this running", and the argument for printing the
  control here -- a terminal this operator may never see -- cannot also be the
  argument for withholding a limit of it. Both limits are there now; both
  *mechanisms* (which uid can read the token, and that a restart is what
  revokes it) are not, because a page gives the fact and the terminal and
  `README.md` give the how -- and the clause says so where it is written
  rather than reading as lossless.
  **"On this port" and not "every request", for one channel's worth of
  precision.** `ServeSocket` answers the same routes with no token, because
  the socket is 0600 in a 0700 directory and `connect(2)` is the check. As a
  claim about WHO can reach this server the shorter spelling was already true
  -- 0600 means the operator's own uid -- so it over-claimed only about HOW,
  by one channel, and that is a different defect from the one it replaced: the
  old clause reported a live control as ABSENT, which is a false negative
  about a control, and this would have been a true control described one
  channel too widely. Three words close it, against leaving the next reader to
  re-derive the socket exception.
  **A `WaitVisible` on the one line of that page carrying a security claim is
  what let it stand.** `TestServe_UI` waited for `.posture` to appear and
  asserted nothing about its text, so the lab written to drive the page an
  operator reads passed against either wording for the life of the clause --
  the same shape as the three cross-origin labs above it. It reads the text
  now, and the **absence** of `no authentication` is asserted beside the
  presence of the new sentence, because a check that only looked for the new
  text would pass against a page printing both. The control and each of its
  limits are separate assertions for the same reason: one `Contains` over the
  whole clause cannot tell a dropped limit from a dropped control.
  **The clause is static and there is therefore no `cargo test`**, which is
  stated rather than papered over. The credential is mandatory, so there is no
  state of this program a `Config` field could switch the sentence on -- the
  `Listing::answer` / `Rows::note` / `Stream::note` shape is for a decision,
  and asserting a string literal against itself is the closed loop this file
  refuses elsewhere. The exit code available is the lab's.
  **IT WAS NOT RUN BY THE CHANGE AND WAS RUN BY THE VERIFICATION, AND THAT
  ORDER IS THE ENTRY RATHER THAN A NOTE ON IT.** The commit made no claim
  about it, because a lab takes the flock on the one Docker daemon and this
  file has had to retract a red-test claim twice. Run afterwards, scoped to
  `TestServe_UI` with a `make web` per step and a control at both ends: the
  opening control **0**, the old clause restored **1** with all three of the
  assertions firing, the closing control **0**. And the split is what the
  discriminator measured rather than the redness: dropping **only** the limit
  half fires **exactly one** of them, so the halves do the work claimed for
  them -- three firing together would have proved the suite went red and not
  why. The third span, non-expiry, landed after that measurement and is
  asserted on the same reasoning and no exit code of its own. The scoping is
  not cosmetic either: `make test-serve-lab` is exit 2 on master over a
  deterministic failure in `TestServe_UIResumesAProgressStreamItLost`, which
  is **#185** and nothing to do with this -- and the only reason it surfaced
  as a finding rather than as a false "lab green" in a commit message is that
  the commit declined to assert an exit code nobody had.
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
- **The same shape again, in a field rather than a row: a failed request's
  reason reached the browser and no view read it.** `requestView.Error` is
  populated by `viewRequest` from `requests.err`, on both JSON routes and the
  socket; `web/src/model.rs` decodes it; `views.rs` read it nowhere. Measured
  the only way that means anything -- deleting the field from the Rust wire
  type left the crate compiling and every `cargo test` green, because serde
  drops what nothing asks for. So the whole operator-visible state of a failed
  generation was the word `failed` in one table cell, over a request stopped by
  `max_month_usd`, by a sampler the cached capabilities do not list, or by a
  card that broke. **This one was filed and the others were not, because a
  document claimed it**: the entry above refusing a warning at enqueue says in
  prose that "a refused request carries the reason, which the page shows". It
  did not. A claim in this file is worth exactly what the code does.
  **The trap in the obvious fix is a caption that is true of one kind.**
  `report.KindRunFailed` existed, was listed as urgent, and `model.rs` had an
  arm for it -- shared with `destroy_failed`, whose view prints "THE NODE MAY
  STILL BE RUNNING AND BILLING -- check `ember ls`". A request a money ceiling
  refused never had a node, so emitting that kind would have printed a false
  alarm about a machine that does not exist, on the one page an operator reads
  to decide whether to go and check their bill. That is the false "abandoned"
  this file already refuses, and the `proj` preview caption. So the reason is a
  **third** class with no verdict and no remediation of ember's own -- the
  server's sentence already carries one where there is one -- and `p.fatal`
  stays attached to `destroy_failed` alone, asserted by counting it in the
  browser lab over a request that never reached a provider.
  **`Request::reason` is not gated on `failed`, and that is the decision.** A
  cancelled request carries a reason too and it is the one with money in it --
  "the job in flight cannot be stopped, so it will finish and be paid for" is
  written to the same column -- so a status check there would hide exactly the
  sentence the operator pressed the button to read. Nor is it gated on
  `is_terminal`: nothing writes the column without ending the request, so that
  guard would be a branch nothing can reach.
- **The same shape again, and this one is twenty-two fields rather than one:
  every parameter a generation ran with crossed every seam and was rendered
  nowhere (#181).** No ordinal, because a running total in prose is the thing
  this file has been wrong about before -- what is worth saying is that the
  two entries above it are the same defect in a row and in a field, and this
  is it in a whole type. `genstore` keeps them as one JSON column per
  request, `internal/webapi`'s `viewRequest` sends **all** of them outbound
  under a comment arguing that "rendering a request's jobs without them would
  show an operator a job description that is not the job that ran", and
  `web/src/model.rs`'s `Job` decodes twenty-three fields -- of which
  `views.rs` read twenty-two **in the compose form**, which builds a *new*
  job. So the watcher showed a server-composed `desc`, a file count and a
  pass/step, and seed, sampler, scheduler, cfg, guidance, clip_skip, eta,
  flow_shift, shifted_timestep, strength, init_image, output_format,
  output_compression, slg, hires and vae_tiling were on the wire and off the
  screen. **Measured the only way that means anything**, which is the
  `requestView.Error` entry's own method one axis over: making the parameters
  `skip_deserializing` left `cargo test` at 52 of 53 passing, and the single
  failure was a round-trip assertion whose own comment reads "the same type is
  what a request view returns: a job on screen has to be the job that was
  queued". The claim was in the tree, in a comment, beside a test that could
  not check it -- a claim in this file, or in a comment, is worth exactly what
  the code does.
  **`scheduler` was read by NOTHING, not even the form.** There is no control
  for it, so a browser could never set one; what it reports is a scheduler an
  `ember up` request put in the store, and a page that omitted the row left an
  operator unable to tell which of the two silences they were in.
  **Three words for three claims, because a dash reads as "ember does not
  know".** `unset` is the four `Option` fields nobody set, whose server-side
  defaults are -1, null, null and 0.75, so a `0` there states a value nobody
  chose; `off` is a block that was not sent, which is upstream's own off and a
  fact about the run; and a seed of zero says "not pinned" rather than `0`,
  because `omitempty` means none was sent and that generation is therefore not
  reproducible. The wording is `Job::settings` and not a chain of `if` in a
  view macro, which is this page's settled pattern -- `Rows::note`,
  `Request::reason`, `Stream::note` -- because only a browser can read the
  second one.
  **Sited in the watcher deliberately and expected to move.** #139 puts this
  on a `/requests/{id}` page behind a router that does not exist; the
  component takes a `Job` and reads no signal, so that slice moves the call
  site. What it does NOT move is the closure: `request` is `None` until the
  socket's first envelope arrives, and three keyed-`<For>` defects in this
  page -- the frozen progress row, the stale offers table and the money column
  that read `$0` for every paid rental -- were all a signal read outside the
  closure that renders it, so a parameter table built once against a request
  id would be those defects with twenty more fields.
  Watched to fail, which is the one piece of discipline that is not optional
  here: `opt(None)` returning `"0"` turns `an_unset_parameter_is_not_a_zero_one`
  red, and deleting the call site turns `TestServe_UI` red on the seed and the
  sampler it typed into the page.
  **The first version of that second red was 721 seconds of "context deadline
  exceeded", which is this file's own `waitForText` warning collected.**
  `chromedp.Text` resolves at least one node and then blocks until the
  context ends, so a selector matching NOTHING spends the lab's whole
  twelve-minute budget and names neither the block nor what was on screen
  instead -- and the assertion's whole job here is to report an element that
  is not there. `waitForParams` polls `innerText` through `Evaluate`, which
  answers the empty string for an absent block: the same defect is then red
  in two minutes with the watcher panel's own text in the failure, and the
  repaired tree is green in 46 seconds.
- **The page became four pages, and the one thing that made it a slice rather
  than a file move is that a component is now UNMOUNTED for the first time in
  this program's life (#139 slice 3).** Four `<section class="panel">` shared
  one `<main>` and a `1.3fr 1fr` grid: the queue's six-column table got the
  narrow half -- 407px of a 1088px body, which is what #138 was about -- the
  watcher spanned both columns through a `.panel:nth-child(3)` selector that
  counted its siblings, and since the billing panel landed the `1fr` column
  beside it was EMPTY on every viewport over 60rem, which nothing had named.
  There is one panel per route now, so the grid is one column at every width
  and `nth-child`, `.panel.idle` and the empty half-row are all gone with the
  thing they were placing. Measured on the repaired tree: `/queue` is 1040px
  of a 1040px `<main>` against the 444px it used to get.

  **A HAND-ROLLED HISTORY ROUTER, and the precedent was in the file that would
  otherwise have changed.** `web/Cargo.toml` puts fetch and WebSocket in
  `web-sys` rather than a wrapper crate for *"about eighty lines in api.rs
  against another dependency tree"*, under a comment whose own words are
  "FEATURES AND NOT A CRATE, which is the whole reason web-sys is here rather
  than a wrapper" -- so `History` and `PopStateEvent` are two entries on that
  list against `leptos_router`, which is in no lockfile here and arrives with
  its own tree. **Declared although the build already compiled without them**,
  because cargo features are additive across the graph: `leptos` enables both
  for its own reasons, so `web/` was spending somebody else's feature
  selection, which is the `// indirect` marker read as evidence one ecosystem
  over.

  **WHAT IT COST IS MEASURED AND IS NOT THE FEATURES.** Release profile, same
  toolchain, one commit apart: **1,597,604 bytes of wasm before and 1,921,981
  after, +324,377 (+20.3%)**. It is not the three hundred lines of routing
  either -- it is that `App`'s `<main>` went from four statically typed
  children to a `move ||` match whose five arms each `into_any()`, so four
  large view types get a full `AnyView` vtable where the compiler used to see
  them in place. The trade is taken, because the alternative to a match over a
  route is not having pages, and the figure is written down in three places
  rather than left for somebody to find. `web/Cargo.toml`'s release-profile
  comment claiming `opt-level = "s"` plus lto "takes the wasm well under a
  megabyte" was already 50% wrong before this and is its own issue; this makes
  it worse rather than causes it.

  **AND A THIRD TREE SAYS THE ROUTER IS NOT WHAT COSTS IT**, which is the
  measurement to read before believing the figure above: this branch with the
  five-arm match replaced by static children -- `route.rs` and all four view
  modules still compiled and instantiated -- is **1,558,267 bytes, 39,337
  BELOW the base's 1,597,604** against the 1,921,981 it ships at, so the
  routing and the pages cost nothing net and the whole +324,377 is the
  `AnyView` erasure. Recorded because `+20.3%` is exactly the figure somebody
  will one day blame the hand-rolled router for and propose deleting it over,
  and the erasure is a property of matching on a route rather than of routing.

  **THE FAILURE MODE IS INVISIBLE BY CONSTRUCTION, so the assertion is a
  sentinel and the lab carries its own control.** A link the page does not
  intercept is a full document load: 1.9 MiB of wasm re-fetched under
  `Cache-Control: no-store`, the app mounted again -- and everything WORKS.
  The right page is on screen, no error is logged, and the only symptom is a
  second of latency. A resource count cannot see it either, because the
  timeline starts over with the document, so the probe is a value on `window`,
  which a new document is the only thing that clears. And a sentinel that
  nothing in the lab can clear is an assertion that cannot fail -- so the
  deep link at the end of `TestServe_UINavigatesWithoutReloadingTheApp` is a
  real typed address and the sentinel is asserted GONE after it, which is what
  makes the four `sameDocument` calls above it mean something.

  **`watching` IS DELETED AND THE URL IS THE AUTHORITY.** An
  `RwSignal<String>` in `App`, written by the queue and by a finished
  `queue()`, became a second writable carrier of a fact that is now in the
  path -- which is `queue.Cost`'s shape, and #139 named it in advance. So
  `Watch` is `RequestPage(id: String)`, created per request by the route
  match; the empty case goes with it, because an address naming no request is
  `Route::Unknown` and never reaches the component. The queue's id cell is a
  link rather than a button writing a signal, which is what makes a request's
  page an address somebody can open in a tab, copy, reload or send on.

  **THE HAZARD: A PENDING RE-OPEN OUTLIVING ITS OWNER, AND THE OLD GUARD WAS
  THE READ THAT WOULD FAULT.** `retry` schedules
  `set_timeout(move || connect(w, again), wait)` with a backoff to five
  seconds, and that timeout is held by the BROWSER -- not an arena entry, not
  an `on_cleanup`, nothing Leptos disposes can reach it. While the watcher was
  a static child of `App` that was never unmounted it always landed on live
  storage; under a router it lands on a disposed owner up to five seconds
  after the operator navigated away. Every signal and `StoredValue` in
  `Watcher` is an arena handle whose non-`try` read is an `expect`, and this
  crate is built `panic = "abort"`, so the first read is an `unreachable`
  trap: a `RuntimeError` out of a timer callback and a page that stops. The
  epoch counter each of those four functions opened with was not a substitute
  -- it was the read.

  **`Watcher::alive` IS `try_get_value`, AND IT IS ONE MECHANISM RATHER THAN
  TWO.** `set_timeout_with_handle` plus `on_cleanup` is what `Queue`'s poll
  does and is the obvious second half; it is deliberately not here, because
  `clearTimeout` cannot un-queue a callback the browser has already dispatched
  -- so it is not sufficient alone while this is, and a clear beside it would
  be a second path for one check whose other path is the live one. It would
  also have made neither half's exit code its own. **The three socket handlers
  get no guard and that is a measurement rather than an omission**: they are
  reached only through the `api::Socket` in `w.socket`, which is an arena entry
  disposal drops, and `Socket::drop` clears `onopen`, `onmessage` and
  `onclose` before closing -- so a disposed owner has no socket to call them,
  and a guard there would be a branch nothing can take.

  **AND THE EPOCH COUNTER IS DELETED, because the router made its condition
  unreachable.** It was bumped by the `Effect` that re-opened when `watching`
  changed; the component is created per id now, so there is one open per mount
  and `w.epoch.get_value() != epoch` could never be true. A guard whose
  condition cannot be taken is a guard nobody can show is right.

  **THE LAB MAKES THE BACKOFF WINDOW A WAIT RATHER THAN A RACE, and that is
  the whole of why it is reliable.** A drop with `ember serve` up is answered
  on the first attempt 250ms later, so "navigate inside the window" would be a
  coin flip; `breakSockets` points the page's own `WebSocket` constructor at a
  path that answers no upgrade, so every re-open fails, the backoff walks to
  its five-second cap, and twelve attempts is forty-three seconds of them --
  there is certainly a pending timeout at the moment of the navigation. The
  reinstated hazard is red at `error: Uncaught RuntimeError: unreachable`.

  **A RESTART USUALLY DOES NOT PRODUCE THE WINDOW, AND "CANNOT" IS WHAT THIS
  ENTRY SAID AND IS WRONG -- corrected here before it was merged, by the agent
  who measured it rather than by review of the sentence.** The design argument
  is sound: SIGINT ends the rental, the row is written terminal, and `retry`
  returns `None` for a terminal request, so there is nothing left to schedule.
  What makes it a race rather than an exclusion is DELIVERY -- the page only
  knows the request is terminal if the socket carried that view before it
  closed, and nothing orders those two. When it does not arrive, `retry` sees
  a request it still believes is live and schedules exactly the timeout this
  hazard is about. The evidence is sharp and belongs to **#185**'s pass: a
  reinstatement case whose patch was *a reworded comment* -- a negative
  control, meant to be caught by nothing -- went RED at the restart
  assertions. The window is not structurally excluded; it merely usually
  loses. So the constructor break is what makes the hazard lab deterministic
  rather than lucky, and the next person to read this must not drop it and
  lean on a restart.

  **AND THE WRITER IS `finish`, NOT `endIfUnfinished`**, which this entry also
  had wrong. `drain` calls `q.finish(req.ID, err)` with the error as it came
  back and only then wraps it as `request %s:` for its own return, so the
  reason recorded on that path carries NO prefix -- which is the evidence,
  because `endIfUnfinished` is the outer net for the paths `finish` never
  reached and a row it wrote would show one.

  **AND THE PAGE IS BEHAVING CORRECTLY THROUGHOUT, which is the half that
  makes this worth leaning on at all.** `reopen_after`'s terminal rule and
  `retry`'s `None if terminal` are doing what they were written to do, and the
  browser-side drop arm of the reconnect lab passed the whole way through
  #185's pass: the reconnect feature was never broken in production. So what
  the hazard lab rests on is a deliberate behaviour rather than an accident,
  and `TestServe_UIResumesAProgressStreamItLost`'s empty stream note is about
  the delivery race above and not about a page that stopped reconnecting.

  **AND `console.errors()` WOULD HAVE CAUGHT THIS ONE TOO, which corrects the
  reason rather than the listener.** This file records that `chromedp.Console`
  is blind to an uncaught throw, measured against the `Socket::drop` defect --
  and that defect is a JS-level throw out of wasm-bindgen's glue with no Rust
  panic behind it. A disposed-arena read IS a Rust panic, so `lib.rs`'s own
  hook writes `ember-web panicked: Tried to access a reactive value that has
  already been disposed` to the console before the abort, and `errors()`
  filters for exactly that word. Measured: **both fire.** The page's `error`
  handler stays the assertion because it is the one that sees the TRAP rather
  than the hook's account of it -- which is what a build whose panic hook had
  been changed would leave -- and because the sentence it prints names the
  mechanism. So the rule is narrower than it reads: the console is blind to a
  throw that is not a Rust panic, and a Rust panic in this crate is logged
  twice.

  **THE PASS FOUND TWO DEAD CONDITIONS IN `Route::parse`, which is what a
  reinstatement pass is for and is a finding about the change rather than
  about the suite.** It had a `trim_end_matches('/')` and an `!id.is_empty()`
  guard in front of its `match`, and removing EACH left every test in the
  module green: the one `filter(|s| !s.is_empty())` on the split had already
  answered both, since a leading slash, a trailing one and a doubled one are
  all empty segments and `/requests/` is therefore ONE segment. Both are gone
  and the tests stayed, because what they assert is the ANSWER rather than the
  mechanism -- and the mechanism they now rest on has an exit code: deleting
  the filter turns four of them red, and the obvious alternative
  implementation, `pathname.strip_prefix("/requests/")`, turns three red
  because it hands back `Some("")`.

  **THE SPLIT TOOK TWO OF #138'S OWN ASSERTIONS AND BOTH DELETIONS ARE
  MEASURED RATHER THAN ARGUED.**

  **The height bound first.** It was five rendered lines, and what made it a
  bound was the separation #138 measured: the same 170-character store refusal
  at NINE lines inside `td.status` at the 174px that cell got in the `1fr`
  half of the grid. On the queue's own page the table is 1003px and the cell
  is 62.5px tall -- and `colspan="1"` reinstated is **62.5px too**, because the
  status column simply takes 745px of the leftovers and the sentence wraps to
  the same three lines. So the bound could not fail over the one defect it
  exists for, which is worse than no check. The WIDTH assertion is red on that
  same patch at *"745px inside a 1003px table, 257px short"*, and it is the
  half that holds the `colspan` literal to the six cells beside it, so the
  height is a figure in the measurement log now rather than an assertion.

  **And `onTbody`/`markedRows`, which read `tbody.watching` -- the one
  group-level mark this page could produce, and the discriminator #138 earned
  when moving `class:watching` onto the `<tr>` left the lab green.**
  `class:running` has exactly the same shape and is the only mark left, and
  that lab cannot produce it: `list.running` is the queue's live rental, read
  from the API rather than a column, so it cannot be seeded and the lab is
  deliberately about a request that never reached a provider. The obvious
  replacement was the weaker "nothing marks a `<tr>`", and **measured, with
  `class:running` moved onto the row that count is still ZERO** -- a check
  that reads as protection and reports nothing, which is `AssertNoStray`. Both
  fields are deleted and the gap is written where they were.

  **What survives has its own exit code and the two cases discriminate.** One
  `<tbody>` per request and the reason inside the group that holds its request
  need no mark: reinstated as loose `<tr>`s the pair is red with nothing else
  firing -- Leptos builds this DOM with `createElement`, so there is no HTML
  parser to insert a `<tbody>` and a dropped group really is absent -- while a
  reason given a `<tbody>` of its own is red at `whyOf`'s `following-sibling`
  instead. Two defects, two fixes, two failures. It is the
  `TestConformance_CannotCatchARederivedRunID` arrangement for the mark: the
  suite states what it cannot see, so whoever next moves one in that table
  decides again rather than trusting a green run.
  **And the precondition changed subject with them.** It was "the queue panel
  is narrower than the one the stylesheet makes span both columns", which was
  what stopped the height bound being satisfied by a full-width panel -- and
  there is no such grid and no such bound. What it asserts now is the SPLIT'S
  OWN CLAIM, that the queue has the page: a `Fatalf`, because the width
  measurement under it is about a table in that panel, and the thing it refuses
  is somebody putting a second panel back on this route. **`<main>` and not
  `<body>`**, because the body carries the page's 1.5rem padding, so a panel
  filling its grid is 48px inside it at any width -- a comparison against a
  constant, which the first run printed as a 48px failure.

  **THE ELEVEN LABS WERE THE REAL COST AND IT FAILED LOUDLY, as #139
  predicted.** Each found its panel by heading, which survives a panel moving
  file and does not survive the panel not being on `/`, so **five** needed a
  navigation step: the cost lab, the reason lab and the cancel lab to the
  queue, the billing lab to its own page, and the reconnect lab twice round a
  loop. The cancel lab needs two, and they are two different mechanisms --
  a header link to the queue, and the BACK BUTTON after the row's link, because
  the cancel button is on the queue and the evidence that a job started is on
  the request's page. `navTo` clicks the header link
  rather than navigating, because a `chromedp.Navigate` is a document load and
  would make every lab after it an assertion about a fresh page -- and a page
  whose links were never intercepted would pass all of them. **`rowOf` was
  `td/button` and is `td/a`**, which the first run of the new routing lab is
  what said: the row was on screen, the selector matched nothing, and the
  failure printed the row it could not find.
  **One lab's wait was a match on the stylesheet**, found the same way: `h2`
  carries `text-transform: uppercase` and `innerText` is what is RENDERED, so
  an assertion looking for "there is no page here" was handed "THERE IS NO
  PAGE HERE".

  **AND THE ROUTER MADE `location.pathname` AN INPUT TO A QUERY STRING, which
  is the one correctness defect this slice introduced away from the watcher.**
  `api::Socket::open` builds `ws://{host}/ws?id={id}&after={after}`, and for
  the life of that function its one caller passed a `watching` signal written
  from the server's own listing -- sixteen hex characters. The id comes off the
  path now, so `/requests/x&after=99` is an address an operator can type, and
  unescaped it sends `?id=x&after=99&after=0`: Go's `Query().Get` takes the
  FIRST value of a duplicated key, so the watcher resumes request `x` from a
  cursor nobody chose. No money and no security -- same origin, the cookie,
  loopback, and the operator doing it to themselves -- but it is
  `escapes_what_would_break_a_query`'s own rule, which this repository already
  wrote down for a card filter where the consequence was "the filter silently
  matched something else". So `urlish` **moved into `api`**, where both callers
  now are, rather than being copied: two spellings of one escaping rule is two
  places for one to drift, and a copy in `api` reading one in `views` would
  have the layering backwards besides. For the id ember actually sends it is
  the identity, which is exactly why nothing would have reported it; the test
  is a cargo one and patching `urlish` to `s.to_string()` turns two red.

  **THE EDITOR RACING THE HARNESS, which is the stale-backup shape met from
  the other side: the harness racing itself is what the one-backup-per-path
  rule already fixed, and this is something else writing the tree under it.**
  No ordinal, and that is the file's own rule rather than modesty: the
  sequence above counts to a sixth and then has an unnumbered shape after it,
  because #186's entry dropped its own number -- so a seventh here would be
  wrong, and this count has now been wrong three times. The NAME is the part
  worth keeping.
  The pass takes one backup per path before anything is patched, which is the
  fix for a half-patched file being backed up mid-case -- and it assumes
  nothing else writes the tree. Three files were edited while it ran, so its
  restore put the pre-edit copies back and two comment rewrites were silently
  gone; a fourth file was left half-patched when the pass was stopped, with a
  `REINSTATED` marker still in it. Nothing in the pass could report either:
  every case's own verdict was correct, and `pristine()` compares against the
  backup the edits were made after. What caught it is the tooling's own
  notice that a file had changed on disk, and what closes it is the rule
  rather than a check: **while a reinstatement pass is running the tree is the
  harness's, and an edit waits.** Recovered by stopping the pass, restoring
  the half-patched file, re-applying the two rewrites, re-taking the backup
  and resuming from the case that was interrupted -- then grepping the whole
  tree for `REINSTATED` before believing anything.

  **Three gaps are named rather than left.** A composed batch does not survive
  leaving the compose page -- the draft and the batch live in that component,
  #139 puts the lift in slice 4, and the symptom is a form that comes back
  empty, so `README.md` says so. A request page re-opens its stream from
  cursor zero, so coming back to a long generation replays its tail: nothing
  is wrong, because `Progress::apply` is idempotent per sequence number, and
  it is rows over loopback rather than anything billed. And `#183` -- the
  provider `<select>`'s keyed `<For>` reading `config.get()` in the row body
  -- moved file with no change at all: its semantics are exactly as they were,
  which is deliberate, because half-moving a filed defect is how a reader
  comes to believe it was fixed.
- **`report.KindRunFinished` and `KindRunFailed` are deleted, and the test is
  whether a second live mechanism exists.** Both were declared, both were in
  `Kinds()`, both were rendered as deliberately silent by `Text`, and
  `KindRunFailed` was in the queue's `urgent` list -- the one reserved for
  events that "would be lost rather than late". **Nothing ever emitted
  either**, so that list named an event that never arrived, and `report.go`'s
  comment claimed they "end a run" while nothing in the program ended one by
  saying so. One more of the things found here written, listed, covered and
  connected to nothing -- deliberately not given a number, because the running
  count above reads third, fourth and sixth and no entry claims the fifth.
  Deleted and not wired up, which is the opposite of the `sdserver.Client.Cancel`
  call and the same as `queue.Cost`: for Cancel there was no second mechanism,
  so deleting it would have deleted the mechanism, and here the fact is
  `requests.status` and `requests.err` -- columns, already on every route and
  the socket. An event for the same fact is the "two carriers of one figure"
  mistake, and it is the worse carrier of the two, because current state
  survives a reconnect with no backlog to replay and an event has to be folded
  from one. The only thing a column cannot say is "the stream is over", which
  is all `KindRunFinished` could have meant, and a terminal status answers it.
  `TestText_DecidesAboutEveryKind` walks `report.go` for declared constants, so
  re-adding one without rendering it fails -- checked by doing exactly that.
- **The socket's own comment claimed it sent the request "every time something
  changed" and it sent it every time an EVENT changed.** `send` returned early
  on `len(es) == 0 && !force`, which is right for every state that emits -- and
  the one state that does not is the one this is about: `queue.fail` writes the
  row for a request refused before a provider was reached and emits nothing,
  because there is no sink for a rental that was never opened. So a tab already
  watching such a request sat on the state the socket sent when it connected,
  with the reason in the database and on the wire type, for as long as it stayed
  open. The queue table never had the gap, because it polls -- which is why
  #103's "the watcher and the queue row can both read it without touching Go"
  was half right, and the half that was wrong is a `bytes.Equal` on the
  marshalled view. **The marshalled view and not a few chosen fields**, so the
  comparison cannot come to disagree with what the client is sent: a field
  added later is in it by construction. Both directions are live --
  `TestWS_SaysNothingWhileNothingHappens` is what stops the fix being an
  unconditional re-send four times a second, which would hide the difference
  between "no progress" and "ember stopped".
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
  **This entry read as a sweep of the page and was a repair of one list.**
  Two more `<For>`s were sitting on the same defect when it was written, and
  the second of them is eight lines above the comment that states the rule.
  The offers panel was keyed on the **provider's name**, which does not
  change between two queries of one provider -- so narrowing the card filter
  and pressing the button again rendered the first answer, and a server
  answering zero offers plus `dial tcp: lookup rest.runpod.io: no such host`
  left an H100 at $2.49 in US-KS-2 on screen with no error at all. That is
  the thing the component's own comment forbids ("I could not reach RunPod
  must never render as RunPod has nothing"), defeated by the key two lines
  above it, and the real behaviour is worse than the comment's wording: not
  "RunPod has nothing" but "RunPod has this card at this price". The fix is
  a plain map over the signal and no key at all -- for the outer list and for
  the inner one over a provider's offers, because leaving the inner `<For>`
  keyed on an offer token would have put the same frozen row one level down.
  **Not a key that encodes the row, which is the other answer and the worse
  one**: a key is stable identity, so one built out of the data rebuilds the
  DOM for a price change and leaves the next field somebody adds in the same
  hole. Reading inside the closure keeps the key honest, and here there was
  no identity worth keeping across two queries at all.
  **The decision moved to `Listing::answer`**, which is where `Rows::note`
  and `Request::reason` live and the reason both are methods: a precedence
  written as a chain of `if` inside a view macro can only be checked by a
  browser, and the error-wins-over-the-offers-beside-it rule now has a
  `cargo test` instead of a comment. The error path is the half no lab can
  reach -- `CloudBaseURL` is a Go field on `app.Deps` and not an environment
  variable, so a lab that runs `ember` as a subprocess cannot point a cloud
  provider at a closed port -- so the browser asserts the rows in both
  directions across three presses and `cargo test` asserts the precedence,
  which is the pair rather than either alone.
  **The third was the queue table's price column, and that one is money.**
  Its key is `(id, status)` -- the status is in the key *precisely* so that a
  status change rebuilds the row -- and `requests.cost` is written **after**
  the status is terminal: `finish` writes done/failed/cancelled and
  `Recorder` writes the cost through from `provision.WithNode`'s **deferred**
  teardown, which is the ordering `queue.go` states in words. So the key
  `(id, "done")` is created with a zero and can never change again, and the
  two-second poll re-fetched the right figure hundreds of times into a row
  that would not take it. A rider's zero is blank by design, so the opener's
  cell is the **only** place in that table a paid rental appears -- which
  means an `ember serve` session that rented an H100 for an hour showed a
  queue of finished requests with no money in it at all, and nothing said the
  column was incomplete. The same row one panel over had already been fixed
  for the same reason, and `Progress::rental`'s own doc comment quotes it.
  **The fix is the closure and NOT `(id, status, cost)`**: a key that carries
  the money rebuilds the DOM for a price change and leaves the next field
  somebody adds in the same hole. `Rows::price` is the decision and
  `price_cell` is the read, and the comment now names which fields in that
  row can move on their own -- `status` is in the key, `jobs` and `gpu` are
  written at enqueue, `reason` is written *with* the status, and `cost` is the
  one written after the key is fixed. `class:running` was read outside its
  closure too and is swept: it can only be stale for one poll and it is a CSS
  highlight, so it was not the defect, but one read outside the closure left
  in the row whose other one was leaves the next reader to work out which is
  which.
  **No lab here can produce a non-zero `requests.cost`, and the lab is
  therefore a mid-run seed.** Every lab rents from the local provider, whose
  price is zero -- the same reason `Progress::rental`'s `Accruing` and its
  non-free `Charged` are unreachable from one -- so a lab that rented would
  assert a blank cell against a blank cell and pass against the defect. And
  the seed has to land while the page is ALREADY OPEN, because a page loaded
  after the write shows the figure either way: that is the reload the issue's
  own negative control used to prove the bytes were arriving. Two writers on
  one SQLite file is what `genstore.OpenFile`'s `journal_mode(WAL)` and
  `busy_timeout(5000)` are for, and nothing is rented: the seeded row is
  `running` only for the instant between `Adopt` and the `Update` that ends
  it, and `ClaimNext` takes queued rows alone.
- **A sentence in a table cell is a sentence in whatever width is left over,
  and the fix for #103 landed in one (#138).** The reason a request ended was
  rendered as a `<span class="why">` inside `td.status` -- one column of a
  six-column table under `table-layout: auto`, in the `1fr` half of the page's
  `1.3fr 1fr` grid. **Measured at a 1280px viewport rather than argued: the
  body is 1088, the queue panel 444, its table 407, and that cell gets 174 of
  it** -- so `app.AttachStore`'s store refusal, 170 characters naming
  `ember store create`, rendered **174px tall, nine lines of two or three
  words**, in a 204px row. Five failed requests fill the viewport. The entry
  above about `Request::reason` is right that the page had to show it and says
  nothing about where, and the CSS comment that did -- "it sits under the
  status word, on its own line, because the reason is a sentence and the cell
  is one column wide" -- names the problem and settles for it.
  **WHOLE, AND NOT TRUNCATED BEHIND AN INTERACTION, and the reason is that the
  length was never the defect.** The same sentence across the table is **four**
  lines. So the question "should this be clamped" is downstream of a width that
  is now right, and answering it with a `<details>` or a `title`-only
  truncation would put an operator who does not know to click back where #103
  found them -- reading `failed` and nothing else -- over a sentence whose
  whole point is that it carries `ember store create`. The `title` the old span
  had is **gone** for the same reason it existed: it was there "because some of
  those are long enough to wrap a cell", which is true of a cell and not of a
  row, and a native tooltip is a second carrier of one string that cannot be
  selected, does not appear on a touch device and truncates per platform.
  **`white-space: pre-wrap`, BECAUSE EMBER WROTE THE LINE BREAKS AND HTML WAS
  EATING THEM.** `app.AttachStore` writes "Create it with:\n  ember store
  create\n" so the command is on its own indented line; collapsed, it ran
  together with the prose on both sides. This is `.log`'s rule one class over
  -- the server composed the layout and the page is not a second author of it
  -- and it is not decoration: the lab's existing "the page must not paraphrase
  a refusal" equality goes **red** on exactly those two newlines, in the row
  and again in the watcher, because `.why` is one rule for both.
  **A `<tbody>` PER REQUEST AND NOT TWO LOOSE `<tr>`s**, because the grouping
  decides three things at once that would otherwise be three rules able to
  disagree: `running` and `watching` cover the pair rather than its first half,
  one border is drawn under the request rather than between its own two rows,
  and the reason cannot read as belonging to the request BELOW it -- which with
  loose rows it would, since `td`'s bottom border sits between them. Written as
  `.queue td { border-bottom: 0 }` plus a `tbody tr:last-child` rule rather
  than `:has(+ tr.reason)`, which holds whether or not a reason row is there
  and needs no lookahead.
  **The `colspan` is a literal and the lab is what holds it to the row above
  it.** Nothing in Rust can count the `<td>`s a view macro emits, so the six
  here and the six cells beside it are held together only by something that
  renders them: the lab asserts the reason cell is as wide as its table, which
  is red the day a seventh column arrives. It cannot see a colspan that is too
  LARGE, because the table model clamps one to the columns the table has, and
  that is the direction with no defect behind it.
  **THE LAB'S VIEWPORT IS LOAD-BEARING AND IS ASSERTED RATHER THAN TRUSTED.**
  Headless Chrome's own window is 800x600 and the grid is one column below
  60rem, so at the default the queue panel is already the full width of the
  page and a height bound passes against any markup at all -- the
  `AssertNoStray` shape reached through a viewport. `chromedp.WindowSize` sets
  it and the assertion then re-checks that the grid really split, by comparing
  the queue panel against the one the stylesheet makes span both columns.
  **And the trigger had to change for the bound to be able to fail**: the lab
  refused a `gpu` the local provider does not offer, whose reason is one short
  line -- one short line in a 174px cell as much as in a 407px row. It names a
  store the provider does not have instead, which `NewPlan` refuses at
  plan.go:172 against `PickOffer` at 280, so nothing is created and nothing is
  billed, and the reason is the three-line one the issue's screenshot showed.
  **Six reinstatements, no two of which fire the same assertion, which is the
  half worth recording.** Two of them fire a pair, and that is the shape rather
  than a blur: what matters is that no case is indistinguishable from another.
  The old markup is red at "the reason is somewhere in the panel but not on a
  row of its own"; no reason row at all is red at "the panel never named why
  the request failed", and those are two waits rather than one precisely
  because they are two defects with two fixes; `colspan="1"` is red at the
  width (208 of 407) **and** at the height (7 lines against 5);
  `white-space: normal` is red at the row's text equality **and** at the
  watcher's -- two disjoint assertions, and geometry catches neither, since
  collapsing the line breaks makes the cell *shorter* (62.5px); the viewport
  removed is red at the two-column precondition and at nothing else; and a
  reworded comment came back **not caught**. Controls green at both ends, one
  backup per path, every case ending on a diff against the pristine copy.
  **The precondition is the only thing between a narrowed viewport and a false
  pass, and that has an exit code rather than an argument.** Removing the
  window size **and** the precondition together is **exit 0, green**, at 694px,
  where the height bound is satisfied by a queue panel that is already the full
  width of the page.
  **The harness's own defect is a restore that repairs the SOURCE and not the
  ARTEFACT, and it goes both ways.** It rebuilt the wasm only for the cases
  that patched Rust or CSS, so the one Go-only case ran against
  `internal/webapi/dist` as the case before it had left it -- and reported that
  case's `white-space: normal` as its own finding, a second assertion firing
  that had nothing to do with the patch under test. Caught by asking why a case
  had fired an assertion nobody had aimed at it. **The other direction is the
  dangerous one**: patch `web/style.css`, do not rebuild, and the lab is
  **exit 0 with the geometry byte-identical to the control** -- the same root
  cause, that no test run derives `internal/webapi/dist/style.css` from the
  working tree, and a false GREEN gets committed where a false RED gets
  investigated. It rebuilds unconditionally now.
  **One thing was measured by hand and is not asserted anywhere, and it is
  narrower than it first read**: that Chrome paints an `outline` on a
  `<tbody>` at all. That is a test of Chrome, needs a screenshot, and is a note
  here rather than a lab for exactly that reason -- checked against real Chrome
  on a page holding nothing but a table with three groups. What it was wrongly
  used to decline is a **different property**: whether the mark's SUBJECT is
  the group that holds the reason, which is pure DOM, is a test of ember, and
  is four lines in a lab that already clicks that row. It had no exit code --
  moving `class:watching` back onto the `<tr>` left the lab green -- and now it
  does: `groups`, `onTbody`, `holdsTheReason` and `markedRows` read
  `1/true/true/0` as it ships and `0/false/false/1` with the mark on the row.
  Until that probe, the three things the grouping settles had no exit code
  between them, because the geometry is identical whichever element carries
  the class.
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
- **The progress socket noticed its own close, wrote a signal nothing read,
  and never re-opened -- and the parameter that would have made a reconnect
  lossless had one caller passing the literal `0`.** `connected` was an
  `RwSignal<bool>` with three writers and, by `grep -rn connected web/src`,
  no reader at all -- the only other hit in the crate was the word in a doc
  comment; `api::Socket::open` took an `after`, put it in the URL, and was
  called once with a constant. So a stream that ended mid-generation left the
  last frame on screen under a panel still captioned as though the card were
  sampling -- #132 measured it on the real wasm, the jobs row and the whole
  panel byte-for-byte identical six seconds after the server closed the
  socket, with no console output of any kind. Three comments in
  `web/` described the reconnect, one of them promising that "a page reload
  and a dropped connection are the same thing to it", and the two were not the
  same thing at all: a reload re-opened from zero and a drop did nothing. One
  more of the things here written, documented and connected to nothing, with
  `cancelling` and the preview frame; what is different is that this one's
  three readerless pieces were a signal, a parameter and a wire field at once.
  **The cases are the operator's ordinary ones and the generation carries on
  through all of them**: `ember serve` restarted, which is the remediation
  ember's own cancel 409 prints; a laptop that slept; a forwarded port that
  blipped. The tab is then permanently half-live, because the queue panel
  polls and recovers while the watcher does not -- two panels on one page
  disagreeing about whether ember is there.
  **Three states and not a bool, and the quiet one is the point.**
  `Stream::note` answers `None` while the socket is up, because a line that is
  always there is not a signal, and the absence is asserted beside the
  sentences -- an assertion that only looks for the presence passes against a
  page that prints it unconditionally. The two sentences differ because the
  remedies do: one says ember is trying again, the other says it is not coming
  back and that reloading is what fixes it. Neither offers a verdict about the
  generation, which is the `destroy_failed` caption's rule -- ember cannot
  tell a dropped socket from a stopped server from a sleeping laptop, so the
  line says the one thing that is true, that the step on screen is the last
  one that arrived.
  **The retry is bounded against the SLOWEST case rather than the fastest.**
  Twelve attempts, 250ms doubling to a five-second cap, about forty-three
  seconds: a socket that dropped on its own is back inside a second, and the
  figure is set by a restart typed by hand, because a bound a restart cannot
  fit inside would make the headline case one this never covers. Bounded at
  all because the alternative cannot report -- a page that retries for ever
  can never print the second sentence. A terminal request is not retried,
  which is also what makes the restart case terminate: `queue.Run` sweeps
  everything it finds running to `interrupted` at startup, so the first
  successful reconnect after one arrives at a terminal row and stops.
  **`onopen` is not decoration, and leaving it out turns the bound back into a
  loop.** `WebSocket::new` returns Ok against a server that is not there --
  the handshake is asynchronous -- so "the socket opened" is a thing only the
  browser can say, and a counter reset on the constructor's Ok is reset on
  every attempt. The old code read that Ok as `connected.set(true)`, which was
  a lie nobody could see because nothing read the signal.
  **`Socket::drop` clears the handlers before closing, and that stopped being
  cosmetic the moment the signal got a reader.** `close()` makes the browser
  call `onclose` on a closure about to be freed with the struct --
  `closure invoked recursively or after being dropped`, observed by clicking a
  queue row twice -- and it was harmless only BECAUSE the write it could not
  land was one nobody was looking at. With a reader it is the old socket
  reporting a stop over the new socket's state, and a retry scheduled against
  a request nobody is watching. The ordering that used to save it -- the new
  `Socket` stored before the old one dropped -- was luck rather than a
  decision, and the epoch counter beside it is the guard that does not depend
  on it.
  **`Envelope::next` is not decoded, because it is fully derivable, and the
  derivation is measured rather than argued.** The issue's terms were that it
  either gets a reader or goes, and the first version of this change gave it
  one: `Progress::fold` took the larger of the server's figure and the one
  `apply` derives from the events. That is the `queue.Cost` shape -- two
  writers of one field, where a reader of the code cannot tell which is live
  -- and deleting the read left every `cargo test` green, which is the
  measurement and not a hole. Driven over a real store afterwards to settle
  WHY: `next` is the last event's own seq and not one past it, falling back to
  the cursor the client sent on an empty envelope; `genstore` selects the
  whole tail ascending with no limit; nothing filters the slice;
  `AppendEvents` is `INSERT OR REPLACE` on `(request, seq)`, so a redelivery
  overwrites in place rather than leaving a counted hole; urgent events travel
  the same rows; and a prune leaves `next` at the maximum of what remains.
  Seven event-set shapes times eight cursors, three of them above the
  maximum, plus eight more after a prune: **64 envelopes, and in every one
  `next == max(cursor sent, max seq in the envelope)`** -- with a deliberate
  `+1` the harness goes red, so the figure is not vacuous.
  **And it is the less safe of the two in the one direction that matters.**
  The day this page tolerates an event it cannot decode by dropping it --
  which is exactly what `Rows` already does for a request it cannot read --
  the client-derived cursor stays below that event and a reconnect asks for it
  again, while the server's figure is above it and the reconnect skips it in
  silence. A cursor too LOW costs a replay the fold discards; a cursor too
  HIGH costs the operator the events they never learn about, which is the
  frozen row this whole feature is about.
  **The field's own doc comment argued the opposite and was BACKWARDS**, which
  is the sharper half of why it went rather than being kept with a better
  comment. It read "`fold` takes the larger of the two, so it can never move
  past an event that was not folded" -- and `max` is precisely the operator
  that can; `min` is the one that cannot. Harmless only because `next` never
  exceeds the envelope's maximum on anything this server produces, which makes
  it a comment wrong about why a seal holds: the thing that gets a seal
  removed by somebody being helpful, and here it would have licensed the next
  reader to KEEP the read believing it was a safeguard. So the two judgements
  are a note where the field was, the `node.RateFromFloat` shape -- `min` and
  never `max`, and advance AFTER applying, because `apply` ignores anything at
  or below the cursor and advancing first discards every event in the envelope
  that set it, the whole content of the first send after a reconnect. The Go
  field stays, because `GET /api/requests/{id}/events` answers the same
  question to a client that would rather poll and `next` is its documented
  cursor.
  **THIS ENTRY SHIPPED CLAIMING THE DELETION WHILE THE FIELD WAS STILL THERE
  AND STILL READ, and the cause is the reinstatement harness rather than the
  edit.** The browser pass and the fast pass shared one backup directory, and
  the backup of `model.rs` was taken by the FAST pass -- before the deletion
  -- so the first `restore_all` of the browser pass copied the pre-deletion
  file back over it. That is this file's own fifth harness shape, a rollback
  that restores a stale backup, reached ACROSS two passes rather than within
  one: `backup()` takes one copy per path by design, which is right inside a
  pass and is exactly wrong between two. What let it through to a commit is
  that the control which catches it -- a closing control that is the same
  green as the opening one -- *was* green, because the deletion's own case was
  green either way, so nothing about the suite could tell a restored tree from
  a repaired one. **A control at both ends is not sufficient when the change
  under test is one no test reports**; the check that was missing is reading
  the diff of every file the harness touched before committing, and the rule
  is that a reinstatement pass that patched a file must hand back its diff and
  not its exit code.
  **WHAT NO ASSERTION ON THE PAGE CAN SEE is which cursor the reconnect asked
  from**, because the fold is idempotent per sequence number: a client that
  replayed an hour of progress renders exactly what one that resumed correctly
  renders. That is why the lab reads it off `ember serve --log-level debug`
  instead -- the handler is the only place that knows, which is the same
  argument this file already makes for a `took=` on a debug line rather than a
  span exporter. The line is sealed by
  `TestWS_RecordsTheCursorItWasAskedToResumeFrom`, because a lab resting on a
  log line nothing promises to print is a lab resting on a grep, and the
  cursor is checked in BOTH directions: zero is the defect, and a cursor above
  the request's last sequence number is the other one -- a watcher that never
  learns what it missed, which freezes exactly as it used to.
  **The lab drives THREE drops on one generation, and the first is the one
  that is about the defect.** A close made by the browser, with `ember serve`
  untouched, is the only arrangement in which the generation carries on
  underneath -- which is the whole story: a frozen watcher over a card that is
  still sampling and still billing. The step on screen moves past what it was,
  and nothing else on the page reads a sampling step, so only a resumed stream
  can have brought it. A restart cannot do that, because it kills the thing
  that would produce the new events; what it proves instead is the other half
  of the terminal rule, since a rental stopped with Ctrl-C ENDS its request --
  a page that has learned that says nothing about the stream and re-opens
  nothing. The second drop is the one the note is read on, and the entry at
  the end of this one says why there are three of them rather than the two
  this paragraph used to describe. Reinstated separately, each catches what
  the other cannot: the no-reconnect defect goes red on the step that never
  moved, and the resume-from-zero defect goes red on the cursor.
  **The browser-side close is injected script and not a seam in the page**,
  which is the line `sshx.Runner` is not widened across either: `api::Socket`
  holds the only reference and nothing in `web/` hands it out, so the lab
  wraps the browser's own `WebSocket` constructor -- the same category as
  `offersProbe`. Chrome's own offline emulation would do it and needs
  `cdproto/network`, an indirect requirement this would promote, for a weaker
  drop.
  **The restart's port and token are the lab's own**: a reconnect to a
  different port is not a reconnect, and a MINTED token does not survive a
  restart, so the browser would be holding a cookie for a credential that no
  longer exists and the resumed handshake would be refused. That is a real
  property of the program and the reason the announcement says to set
  `serve_token`; a lab about the reconnect must not be a lab about that. And
  it is **what is NOT asserted anywhere**: an operator who restarts without a
  configured token gets the refusal and has to open the new one-time URL, and
  the page cannot tell that from any other close -- it retries twelve times
  against a server that will refuse all twelve and then says the stream is not
  coming back, which is the right sentence for the wrong reason.
  Distinguishing it means a handshake's refusal reaching a page whose only
  hook is `onclose`, and a close event carries no status for a handshake
  answered with a 401.
  **The note WAS asserted on the restart and is not any more**, because the
  premise it rested on is gone and the entry at the end of this one has the
  measurement. What is still true is why it cannot go on the plain
  browser-side close: a close with the server up is answered on the first
  attempt, 250ms later, so the line is on screen for less time than any poll
  can see. Correct behaviour, and not a thing to assert around -- so the
  window is now MADE, by holding the page off its re-open, rather than found.
  **What a browser here cannot reach, asserted rather than left to be
  noticed -- and the "cannot" is now a "does not", which is corrected rather
  than left to be credited wrongly.** `Stream::Lost` -- the bound spent, "it
  did not come back: reload this page" -- needs a request the page still
  believes is live whose server never returns, and that was argued here as
  structurally unreachable, because a page that has learned its request ended
  stops retrying by design and after a restart `queue.Run`'s sweep has ended
  it. The instrument the entry at the end of this one adds reaches it: holding
  the page off its re-open for the whole bound is twelve attempts over a
  request that is still generating. It is deliberately not asserted -- that is
  forty-three seconds of wall clock for a sentence `cargo test` already
  decides and which renders through the same `<p class="stream">` the
  reconnecting one proves is drawn -- so the lab asserts the sentence it DOES
  see is the reconnecting one and **not** that one. What that assertion is NOT
  is a guard on the bound -- and
  the history of that clause is worth keeping, because it has now been wrong
  in both directions. The first wording claimed it was a guard. That was
  **measured and false of the arrangement it described**: with the note read
  on the RESTART, at `RETRY_ATTEMPTS = 1` -- 250ms, which fits no restart at
  all -- the lab was GREEN, because its first `streamNote()` landed a fraction
  of a millisecond after the close and so always read the reconnecting
  sentence, and `startServeOn` brought the second server up inside that one
  attempt anyway. **It is true again of the arrangement that replaced it**, and
  measured the same way: `noteOnALostStream` captures the line 600ms after the
  close, by which time a page with one attempt has spent it and says the
  stream is not coming back, so the "it says reconnecting" and "and not the
  other sentence" assertions both go red -- observed three times running,
  which is in fact how the stale-wasm shape below was found. The bound is
  still sealed by `cargo test` --
  `the_retry_is_bounded_and_backs_off`, red at exit 101 with
  `RETRY_ATTEMPTS = 1` -- and that remains the gate to credit, because the lab
  is a bound on 600ms of behaviour and not on the figure. Two instances of the
  thing this file keeps recording about itself in one clause: a sentence here
  asserting that a test goes red is worth exactly the exit code nobody
  checked, and both of these were found by somebody checking it.
  **And one line has no exit code anywhere, which is said rather than papered
  over.** Resetting the retry counter from `onopen` rather than from
  `Socket::open` returning Ok is the difference between a bounded retry and a
  loop, and reinstating it was GREEN: the page sets `Live` a moment early, the
  note still appears on the restart because the attempts keep failing while
  the server is down, and the consequence -- a bound that is never reached --
  shows only on a server that never returns, which is the state above. What
  stands in its place is that the constructor's Ok is documented as not a
  connection, where somebody would otherwise read it as one, and that the
  defect it replaced did exactly that. This is the
  `subtle.ConstantTimeCompare` answer to "which of these has no test".
  **The console assertion this lab already had could not fail, and the
  reinstatement is what said so.** `Socket::drop`'s defect is an uncaught
  throw, and `chromedp.Console` streams the console API's own messages while
  an exception nobody caught arrives as a different CDP event -- and
  `consoleLog.errors()` filters to "panicked" and "undecodable" besides, which
  a dropped-closure throw says neither of. So the first run of that case was
  GREEN against a check written for it. The listener is the page's own `error`
  handler now, and deleting that assertion with the defect still in place is
  green, which is what proves nothing else in the tree reports it.
  **The first version of the lab asserted the request's end REASON and it
  never arrived, which is a finding about ember and is NOT fixed here.** A
  request whose rental SIGINT cancelled came back from the restart with an
  empty `requests.err`, so the watcher had nothing to print.
  `Queue.endIfUnfinished` writes through `Store.Update` on the drain loop's
  own context, which is the context the signal cancelled, and that write
  cannot land: measured against a real store, `Update` on a cancelled context
  answers `begin: context canceled` and leaves the row exactly as it was. So
  the row keeps the `running` `ClaimNext` wrote, and the next start's
  `Interrupt` sweep turns it into `interrupted` with no reason at all. That is
  the entry above about a rental that failed after the node existed leaving
  the row reading `running`, reached one layer further out -- and the
  `context.WithoutCancel` rule this file states for a destroy, reached through
  a store write instead. Filed rather than repaired in a change about a
  websocket.
  **Reinstated, judged on exit codes, with a control at both ends of every
  pass -- and, after the restore above, on the DIFF of every file each case
  touched.** Through `cargo test` and `go test`, eight red: the note printed
  unconditionally, an unbounded retry, a terminal request retried, a flat
  cadence in place of the backoff, `RETRY_ATTEMPTS` shortened to one,
  `apply` losing its per-sequence idempotence, the handler recording no
  cursor, and the handler recording a constant one. Two more that are the
  `Envelope::next` read and are the point of it: **the read on its own is a
  COMPILE ERROR** now the field is gone -- `error[E0609]: no field next on
  type &model::Envelope` -- which is this file's own "prefer making a bug
  unrepresentable over testing for it" landing where a red test would have
  done; and **the whole first version put back, the field re-declared and the
  fold reading it, is GREEN at exit 0**, said plainly because it is the
  measurement rather than a gap. Nothing catches that shape and nothing can:
  the figure is derivable, so what makes the deletion right is the derivation
  and the backwards comment, not an exit code.
  Five through the browser: the resume from zero (red on the cursor), nothing
  re-opening at all (red on the step that never moved), the note printed
  unconditionally (red on both its halves), `Socket::drop` keeping its
  handlers (green against the console assertion that was already there, red
  against the page's own `error` handler), and the retry counter reset by the
  constructor (green, the entry above). A reworded comment was green in every
  pass, and the suite was green before the first patch and after the last.
  The harness's own first run reported BUILD-ERROR for six cases that had all
  gone red correctly, because `error: test failed` is how cargo reports a red
  suite and the detector read it as a compile failure -- the "cannot tell a
  build failure from a pass" trap inverted, and the reason that detector now
  looks for `error[E...]` and `could not compile` instead.
- **`make test-serve-lab` exited 2 for twenty-two of the twenty-six hours that
  lab had existed, and the lab was asking for a sentence a correct page does
  not print (#185).** The entry above is where the reconnect landed; this is
  what happened to its own regression test three hours later.
  `TestServe_UIResumesAProgressStreamItLost` failed at
  `the watcher never reported a stopped stream within 1m0s; it says ""`, and
  the first thing to get right is what that empty string was: `streamNote`
  answers `""` for `querySelector` returning **null** as well as for an empty
  element, so the element was ABSENT -- #185's own reading, "found and empty,
  not absent", is wrong in the direction that sends a reader to the renderer
  rather than to the state behind it.
  **MEASURED, in the browser and in the journal, rather than reasoned.** Right
  after the first `ember serve` exited: `requests.status="cancelled"`,
  `requests.err="job 1/1: context canceled"`, `ended` stamped -- and
  `window.__emberSockets.length` **2**, still **2** twenty seconds later with
  no server on the port. So the page had made no reconnect attempt at all, and
  no note, because it had learned the request was over while the socket was
  still open. `retry`'s `None if terminal => {}` and `reopen_after`'s terminal
  rule, doing exactly what they are written to do: nothing more will be said
  about a request that is done, failed, cancelled or interrupted, the final
  status and its reason are on screen, nothing is stale, and a line reporting
  the stop would be noise over a generation that has ended.
  **SO THE PAGE WAS RIGHT AND THE LAB WAS WRONG**, which is the less valuable
  of the two possible outcomes and is said plainly because the other one was
  the fear: the reconnect feature is not broken in production, and its
  headline case -- a browser-side drop with the server up, the step moving on
  afterwards, the cursor above zero and no further than the rows -- passed
  throughout.
  **WHAT BROKE IT IS THE FIX FOR A DEFECT THIS ENTRY'S OWN PREDECESSOR
  FILED.** The paragraph above ends "Filed rather than repaired in a change
  about a websocket": the lab was built on the measurement that
  `endIfUnfinished`'s write could not land on the cancelled context, so a
  restart left the row reading `running` and the watched request therefore
  live. #159 (821a92c) made that write land -- and its entry in **Money**
  opens by NAMING this lab as the thing that filed the defect it is repairing,
  "the websocket lab's first version, which spent twelve minutes waiting for a
  reason that was never going to appear". So the two changes are linked in
  this document, in both directions, 2,401 lines apart, and nobody re-ran the
  lab the first one owns. The interval is the measure of it: the lab landed at
  3d0426c, 2026-11-03 22:26:05, and 821a92c is 2026-11-04 01:29:32 and the
  **first** commit after it to touch `internal/queue/queue.go` -- so it was
  green for 3h03m27s and 14 commits, and red for **22h47m31s** and 23, five
  of which edited that very file. **The first figure written here was 20h35m
  and understated it**, because it measured to when the lab first went green
  again rather than to the commit that fixed it -- a review recomputed it from
  the timestamps and corrected it UPWARD, which is the direction a figure
  about one's own finding is least likely to be wrong in by accident. It was
  red for 88% of a 25h51m life and was found by a review of an unrelated PR.
  **THE REPAIR IS THREE DROPS, AND THE NOTE'S WINDOW IS MADE RATHER THAN
  FOUND.** A browser-side close with the server up is answered on the first
  attempt 250ms later, which is why the note was never asserted there; a
  restart ends the request, which is why it cannot be asserted there either.
  So `refuseSockets` holds the page off its re-open -- the same browser-side
  instrument as `recordSockets`, a flag the wrapped `WebSocket` constructor
  checks before reaching the real one -- over a request that is still
  generating and still billing, which is a **stronger** subject than the
  restart ever had, where the request had ended. Both branches that schedule a
  re-open are exercised: the first attempt arrives through the socket's
  `onclose` and every one after it through `Socket::open` returning Err, which
  is the only place a refused constructor can be retried from. The refusal is
  a flag and not a count of refusals, because a count would have to be
  guessed against the backoff and a guess too small clears the line before the
  poll reads it.
  **AND THE RESTART ARM NOW ASSERTS THE BRANCH THAT BROKE IT**, which no
  browser covered: nothing more will be said about a terminal request, so the
  page says nothing about the stream and re-opens nothing. It is required to
  say nothing for ten seconds with the server gone, to go on saying nothing
  for fifteen more with a server back on the port, and to have opened **no**
  handshake against it -- fifteen being over the backoff's own five-second
  cap, so a watcher still retrying cannot hide inside it. And
  `requireNoStreamNote` spends its whole window even once it has failed,
  precisely so the count after it is read fifteen seconds in rather than a
  fraction of a second in: the first shape returned on the first offending
  read, which would have made the count 0 for a page that was about to
  connect.
  **THE FIRST SHAPE OF THAT ARM WAS A RACE, AND THE NEGATIVE CONTROL IS WHAT
  SAID SO.** It let the SIGINT end the watched request and asserted on what
  the page did afterwards -- which needs the terminal row, written during the
  shutdown, to reach the browser on one more 250ms poll before the process
  exits, and nothing bounds the gap between those two events. Measured rather
  than inferred: a reinstatement case whose patch was **a reworded comment**
  went red at exactly those assertions, which is a flake, and a lab that fails
  because the machine was busy teaches people to re-run rather than to read.
  So the watched request is ended through the API while the server is up and
  healthy, and the page's knowledge is something the lab WAITS FOR: the jobs
  row says `cancelled` only once a `job_cancelled` event has been folded,
  which is this watcher's own socket rather than the queue panel's poll, and
  the queue panel's own `cancelled` then says the row is terminal server-side
  -- a fetch at two seconds against a socket at 250ms, so by then the watcher
  has had several chances at the same row. That is the opposite of the window
  the shutdown offered.
  **AND THE SECOND REQUEST HAS TO BE UNABLE TO FINISH, which the first
  version got wrong and a merge caught.** The arm needs a request the signal
  INTERRUPTS, and `waitForStatus(rider, "running")` establishes that it is
  running *now* rather than that it still will be when the signal lands a
  moment later -- the same gap as the watched request's own precondition, and
  the third time tonight that "it is in state X" was read as "it will be in
  state X". At sixty steps the rider finished inside the arm's own windows, so
  the row read `done`: terminal, so the premise passed, and carrying no reason
  at all, so the sentence the arm ends on could never appear and the failure
  surfaced sixty lines away from its cause. Measured from the journal of a
  failing run -- the watched request `cancelled`, the rider `done`. Six
  hundred steps cannot finish inside a lab that never waits for it, which is
  the device the watched request already uses, and the premise now asserts
  `cancelled` rather than merely terminal so that a rider which finishes early
  is one named message instead of a confusing one later.
  **A SECOND REQUEST IS WHAT KEEPS #159 IN THE ARM**, queued before the cancel
  so the drain loop claims it onto the same rental rather than renting again.
  The shutdown then has a request it really interrupts, its row is read out of
  the journal BEFORE the second server starts -- afterwards `queue.Run`'s own
  `Interrupt` sweep makes a row terminal whether or not the shutdown managed
  to, so a read taken then could not tell the two apart -- and reverting #159
  turns that read red at a named precondition. Without the second request the
  assertion would have been about a row the API cancel had already made
  terminal, which is #159's defect being waved through by the test for it.
  **AND THE ARM'S POSITIVE HALF IS THE SENTENCE #159 BUILT AND NOTHING IN A
  BROWSER ASSERTED.** "It says nothing" is satisfied by a page that renders
  nothing at all -- a wasm that failed to mount answers every absence in that
  arm -- so the lab also requires the interrupted request's recorded reason to
  be on screen, through the queue table's own poll now that there is a server
  to poll. That is the assertion the lab's first version wanted, could not
  have, and filed; so both halves of 821a92c now land in one place, which is
  the arrangement that would have caught this on the day: the reason appears
  and the note does not.
  **AND THE NOTE IS READ IN THE BROWSER, WHICH IS NOT TIDINESS EITHER.**
  `Stream::Reconnecting` is a state the PAGE bounds -- twelve attempts and
  about forty-three seconds -- so a note polled from Go gave the lab that
  whole budget to make one chromedp round trip in, which is enormous and
  still not a bound: measured, with three lab suites on one machine, this lab
  spent seven minutes waiting for the Docker lock and then read
  `Stream::Lost` on its first look, starved past the page's entire retry
  budget. So `noteOnALostStream` is one evaluation and a browser timer: the
  close, the capture 600ms later -- about two refused attempts in -- and the
  release of the refusal all happen in the page, so a Go side that stalls for
  a minute afterwards finds a page that has already recovered rather than one
  that has given up. `waitForStreamNote` is gone with it, and the two
  judgements worth keeping are a note where it was: it must not be
  `waitForText`, which cannot see an element that is absent, and "stopped"
  matches BOTH of the page's sentences, so a poll that was late read the
  wrong one as the answer.
  **AND THE WAY IT BREAKS THE SOCKET IS #194's, NOT ITS OWN, WHICH IS A
  MEASUREMENT RATHER THAN A MERGE RESOLUTION.** Two changes landed within
  hours of each other needing the same browser-side instrument and both
  edited `recordSockets`' one wrapper: #194 rewrites the URL to a path that
  answers no upgrade (`/api/no-such-socket`, a 404 rather than a 101), and
  this one threw out of `WebSocket::new` before the real constructor was
  reached. **The path form subsumes the throw and is also the better
  instrument**, so there is one mechanism: a handshake that reaches a real
  server and is refused is what a dropped stream actually looks like, while a
  constructor that throws is not a state any server can put a browser in --
  the throw was simulating a defect that does not happen. Measured rather
  than reasoned, which is the whole of why the throw went: the path holds the
  page in `Stream::Reconnecting` across the entire window this arm reads, and
  `RETRY_ATTEMPTS = 1` still turns the arm red under it.
  **WHAT UNIFYING COST IS ONE BRANCH, and it is named rather than quietly
  dropped**: a throw made `Socket::open` return Err, which is `connect`'s
  `Err(_) => retry(…)` arm and the only place a constructor that refused can
  be retried from, so that line is now reached by no lab at all. It is a
  defensive branch for a URL no page constructs, and one shared helper
  carrying two mechanisms is the drift this file keeps paying for -- but a
  reader looking for its coverage should know there is none. The path lives in
  `brokenSocketPath` because two labs now break these sockets and a second
  spelling is a second thing to keep in step with what `ember serve` 404s;
  `mendSockets` is the other half of the switch, which the restart arm needs
  and #194's does not, since that one only ever needs the retry to keep
  failing.
  **WHAT IS LOST IS THE CROSS-PROCESS CURSOR CLAIM, and it is lost rather than
  moved.** "A NEW process reading the same rows resumes from at least where
  the page had got to" required the page to reconnect after the restart, which
  a page watching a request that ended correctly does not do. Keeping it would
  mean a second request left queued across the restart, and the restarted
  server would then claim it and rent a second node for the claim's sake. What
  stands in its place is the first drop's own `resumed <= lastSeq`, which reads
  the same number back over the HTTP route and so says the cursor is in the
  rows rather than in anything one connection was holding. Stated here because
  the paragraph above used to claim it and a claim in this file is worth
  exactly what the code does.
  **SIXTY STEPS, because two of the three drops need the request to be still
  generating** and the page's own default of twenty is a job short enough that
  a slow chromedp round trip could see it finish. It costs nothing: the rental
  is local and free, the first step arrives no later for sixty than for twenty,
  and this lab never waits for the job to end -- measured, 47.6s against
  47.9s. No published limit bounds `steps`, which is why the number is not a
  guess at one.
  **Reinstated over three passes, judged on exit codes, controls at both ends
  of each, negative controls reported as not-caught, and every case ending in
  a byte diff against one backup taken once per path.** The third pass is the
  authority, because it is the only one run against the arm shapes that
  shipped and the only one whose restores could not leave a stale artifact;
  its opening and closing controls are green and report the same wasm size,
  which is the eighth shape's own witness. Six red, each at a named
  assertion: `Stream::Reconnecting` printing nothing, which is
  `"(the page printed nothing)"` at both halves of the note AND at the
  handshake pair; a terminal request's lost stream reported as a loss, at
  both of the third drop's silences; `reopen_after` ignoring `terminal`, at
  those two AND at the handshake count, which is what making
  `requireNoStreamNote` spend its whole window bought; the recorded reason
  emptied, at the one assertion that reads what the page DOES say -- the
  queue table showing two `cancelled` rows and no sentence; the lab's own
  refusal instrument made a no-op, at the note, which measures that the
  refusal is load-bearing rather than decoration; and **#159, which is the
  interaction this entry is about**, twice over. Three green and each a
  measurement rather than a gap: two negative controls, and **the interval**
  -- the OLD lab against the PRE-#159 write, the tree as it stood between
  3d0426c and 821a92c -- so the red was introduced by that commit and by
  nothing else. The whole target is 28 labs, 0 failures, 0 skips, 285s,
  exit 0.
  **#159 TWICE BECAUSE THE FIRST PATCH WAS CRUDER THAN THE DEFECT, and it went
  red one assertion earlier than the claim written for it.** Cancelling
  `writeCtx`'s context immediately makes `finish`'s write fail on EVERY path,
  so the row the arm cancels through the API never becomes terminal either and
  the red lands at that precondition -- a true red about the right writers and
  the wrong subject, which is this file's own "ask why a case went the way it
  did" catching a sentence before it shipped. The faithful revert restores the
  parameter the fix removed, `finish(ctx, …)` taking the drain loop's own
  context, which leaves the API-cancel write working and breaks only the write
  the signal races: red at the premise, naming it --
  *the request the shutdown interrupted reads "running", so the write that
  records how a Ctrl-C'd rental ended did not land.*
  **Three of the passes' own cases were void and the three shapes below are
  why**, which is this file's own expectation about a first round holding:
  every pass that reported a result had cases whose verdict was about the
  harness rather than about the patch, and chasing them is what produced the
  race the negative control found and the stale artifact no diff could see.
  **A SEVENTH HARNESS SHAPE, and it is a rollback that restored every file it
  touched.** `make web` compiles `web/src` into `internal/webapi/dist`, which
  `go build` embeds -- so a Rust case's defective wasm survived the restore of
  its `.rs` file, and the next case, which was Go-only and therefore triggered
  no rebuild, ran against it. Both Go cases of the first pass reported **red,
  AS EXPECTED, at the previous case's assertion** -- `serve_image_test.go:1961`,
  "the progress row still reads ... so nothing the resumed stream carried
  reached it", three times in a row at 247, 247 and 248 seconds, which is the
  no-reconnect defect's own signature and nothing to do with either patch. It
  reads exactly like a result: the byte diff against the backup was clean, the
  build check passed, the marker each patch introduced was present, and the
  verdict was about somebody else's defect. Caught by asking *why* a case went
  the way it did rather than by reading its exit code -- the same question that
  caught the false RED in #159's own pass -- and specifically by noticing that
  two unrelated patches produced the same failure at the same line and the
  same duration. The rule to carry forward is that a restore has to cover the
  tree the suite READS and not only the files the patch WROTE.
  **AN EIGHTH SHAPE, WHICH IS THAT FIX NOT WORKING, AND NO DIFF CAN SEE IT.**
  The second pass built the page before every run and was still contaminated,
  because `shutil.copy2` **preserves mtime**: a restored source is therefore
  OLDER than the artifacts cargo built from the patched one, so cargo's own
  freshness check declares the crate up to date and `make web` is a no-op that
  leaves the defect running. Every run after the `RETRY_ATTEMPTS = 1` case
  used that case's wasm -- including the **closing control**, which was GREEN,
  because its one note read landed inside the single remaining attempt. The
  sources were byte-identical the whole time, which is why the diff check this
  file already asks for could not report it, and it then produced two false
  REDs in the repair that followed, each looking exactly like a real defect in
  the page. The witness is the artifact rather than the source: 1,597,559
  bytes patched against 1,597,604 clean, which the third pass prints per case
  and compares at both ends. `os.utime(path, None)` after every restore is the
  fix, and the rule is that a rollback has to touch what it restores, because
  a build system that reads clocks cannot see a file that went backwards.
  **AND A NINTH, WHICH IS A LOCK TIMEOUT READ AS A VERDICT.** Four agent
  worktrees were running lab suites on this one daemon, so
  `labdaemon.Acquire` waited its ten minutes and failed with the sentence it
  is written to fail with -- and a harness that read THAT exit code as a
  result would have reported a defect as caught, or a negative control as
  caught, because the machine was busy. The third pass's own opening control
  caught it and voided the pass, which is the control-at-both-ends rule
  earning its keep for the third time in one change; a retry around each run,
  keyed on the holder sentence rather than on the exit code, is the fix. The
  general rule the three shapes share: a harness has to be able to tell "the
  suite reported" from "the suite never ran", and an exit code alone cannot.
- **"Pins die with the node" is structural now, and the pin FILE that
  contradicted it is deleted.** The entry above says a pin keyed by IP matches
  whoever gets that address next and that the churn is what talks people into
  turning host key checking off -- and `hostkey.Store` sat in the same package
  with an atomic, versioned, 0600 JSON file at
  `<state_dir>/known_hosts.json`, which is persistence across processes for a
  thing that is supposed to die with one. Nothing ever wrote it. The disposal
  is `queue.Cost`'s and not `sdserver.Client.Cancel`'s, and the test is the
  one those two entries set: **is there a second live mechanism for the thing
  this would do**.
  **There was no FIRST mechanism, which is the finding.** The only production
  dial in the tree is `internal/app`'s `dialNode`, reached only from inside
  `provision.WithNode`'s closure and handed a `hostkey.Pin` that `trustFor`
  built in memory from the key `WithNode` minted seconds earlier in the same
  process. Nothing read a pin by id: `Store.Get` had no caller outside the
  store's own tests, and its waiver in `cmd/check-coverage` said so in
  writing -- "for a reader that does not exist yet", which is the admission
  this file's own rules would have caught had anything been looking at it.
  **#146 is the case that looks like it needs persistence and does not.**
  `ember up` is a client of `ember serve` over a unix socket now, so "a
  long-lived process and a CLI that talks to it" is exactly the shape where a
  pin surviving a restart would earn its keep -- and `internal/cli`'s
  `upclient` names neither `hostkey` nor `sshdial` nor `ssh`: it POSTs
  `/api/runs` and follows a report stream, while the rental, and therefore the
  dial, happens in the server process inside `queue.Direct.Run`. The process
  that mints is the process that dials, in every shape ember has.
  **And the file could only ever have held keys ember never authenticated.**
  `tofu.check` was the only writer, so every pin in it was a trust-on-first-use
  pin by construction; persisting one makes a first-sight guess authoritative
  for every later process, which is worse than having no file. What a future
  attach-to-a-running-node verb would need is `hostkey.FromNodeKey` plus
  `Pinned` off what the provider recorded, checked against the minted key --
  which `trustFor` already does, and on a provider that reports nothing there
  is nothing to rebuild from, so a remembered TOFU pin would be the wrong
  answer rather than a missing one.
  **Measured, not read, because this file has asserted a mechanism it did not
  have before.** With `saveLocked` made to panic, exactly two packages in the
  whole unit suite reached it -- `internal/hostkey` and
  `internal/tunnel/sshdial`, both from their own test files -- and
  `internal/provision`, `internal/app` and every other package stayed green:
  nothing in production writes a pin file. And `TrustOnFirstUse`'s waiver kept
  it "for a provider that genuinely cannot accept an injected key, of which
  there is none today", which is a shape ember cannot **represent** rather than
  one it has not met: `node.NewCreateRequest` refuses a zero `HostKey`, and
  dropping that refusal turns `TestNewCreateRequest_Refuses` red at "no host
  key: NewCreateRequest accepted it". A waiver that names a case the type
  system already refuses is a waiver nobody can retire by reading it.
  **It is a strengthening of the seal and not a loosening, which is the
  opposite of what deleting a security type usually means.** `internal/hostkey`'s
  package doc rests the posture on two sentences: Trust's one method is
  unexported, which `TestTrustIsSealed` holds, **and** "this package contains
  no permissive implementation" -- which was **not true** for the life of the
  package, because `tofu` accepted whatever key it was shown first and the only
  thing keeping it off the renting path was #117's return type. `Pin` is the
  only implementation now, and
  `TestTrustHasOneImplementationAndItIsPin` parses the package's own non-test
  files and fails on a second one, because the count and
  `TestPinned_AcceptsTheKeyItHoldsAndRefusesEveryOther` are a pair: a count
  alone is consistent with one permissive implementation. It is a parse because
  reflection cannot enumerate the types a package declares, only ask a value it
  already holds, and the subject is a type nothing constructs yet.
  **That sentence was false when it was first written, and the guard was walked
  around on the first attempt.** A method receiver in Go is `T`, `*T`, `T[P]`
  or `*T[P]`; the first version unwrapped the star, named the `*ast.Ident` and
  **skipped** anything else, so a GENERIC receiver -- `*ast.IndexExpr`, or
  `*ast.IndexListExpr` for two parameters -- took the skip, and a generic type
  is a live `Trust` the moment it is instantiated. Measured, each patch applied
  and reverted: a permissive `check` with a value receiver is exit 1, with a
  pointer receiver in another file exit 1, **with a generic receiver exit 0** --
  and with an exported constructor for it plus `internal/app`'s `dialNode`
  handing it to `session.Dial`, `go build`, `go test ./...`, `go vet` and
  `golangci-lint` are **all exit 0 while ember dials every node it rents with
  no host key verification at all**, `TestTrustIsSealed`,
  `TestNoInsecureIgnoreHostKey`, `TestSessionCarriesAPinAndNotAnArbitraryTrust`
  and `TestNoProductionDeadCode` green beside them, because the types are
  untouched and the constructor has a caller.
  **The fix is `TestFlagTablesClassifyEveryFlagSetMethod`'s rule and the three
  lines are its**: a receiver this guard cannot name is a **fatal** rather than
  a skip, because "the alternative to failing on a method it cannot classify is
  reading straight past a flag, which is one more gate that cannot fail in a
  repository that has already found four." Both generic shapes now exit 1
  naming the receiver kind, a clean tree exits 0, and with the whole attack
  wired into the only production dial the one red light in the entire tree is
  this guard. A guard that can be walked around is worse than none, because it
  reads as protection -- and this one claimed its own completeness in this file
  before anybody had tried to get past it, which is the failure the rest of
  this entry is about, committed by the entry itself.
  **One gap is stated rather than closed**: a permissive `check` in a
  `_test.go` file of `internal/hostkey` is not reported, because the walk skips
  test files -- against this file's own "not behind a flag, not in a test" and
  "no escape hatch for a seal, not even for a test". It is bounded rather than
  fixed: production cannot reference a `_test.go` identifier and this package's
  own tests cannot reach `sshdial`, so such a type can be handed to nothing
  that dials. Including test files would mean the guard reporting itself the
  day a test legitimately needs a stand-in `Trust`, which is the shape of a
  guard people learn to ignore.
  **Nothing was narrowed that could have been, and the reason is where the
  compile error should sit.** `sshdial.Dial` and `session.Dial` still take the
  `Trust` interface although `Pin` is the only thing satisfying it. Narrowing
  them would move #117's compile error off the provisioning path, where the
  defect was, and onto the dial -- where a provider that publishes its
  fingerprint out of band would one day need the signature widened again, and
  widening a signature is how #111 arrived. `trustFor` and `Session.Trust` stay
  typed `Pin`, and the guard on that typing stays too: it is about the type, so
  it holds against the next `Trust` somebody writes, which is the one thing the
  deletion cannot do.
  **The three judgements are kept as a note where the code was**, the
  `node.RateFromFloat` shape, because whoever revives persistence makes them
  again: keyed by node id and never by host:port; write-then-rename **and**
  refuse a file from a newer ember, which are one rule because a pin file that
  will not parse or that quietly lost a pin re-arms trust-on-first-use for
  every node in it without anybody being told; and the file's mode is a control
  rather than hygiene -- a fingerprint is public material, so 0600 in a 0700
  directory is not about secrecy but about who can make their own key
  authoritative for a node ember will dial, which is the MITM the pin exists to
  refuse arriving through the filesystem instead of the network.
  **One thing found and deliberately left alone, and the first reason given for
  leaving it was false.** `node.HostKeyPinned`, the `HostKeySource` whose doc
  said "ember accepted a key it had never seen and recorded it; ember reports
  it so the operator can see that without reading logs", now has no writer
  anywhere and cannot get one. Leaving it is right; the reason written here was
  "a member of a printed enum, so removing it is operator-visible", and
  **nothing prints it**: measured, `grep -rn 'HostKey' --include='*.go'` over
  `internal/reconcile/` and `internal/cli/` returns nothing at all, so
  `ember ls` never reads `node.HostKey`, and the only non-test site that
  formats a `HostKeySource` is a `providertest` failure message. So the doc
  comment was untrue twice over and is corrected -- which matters because the
  comments in `config`, `genstore` and `app` were corrected in the same change
  and this one was not, which is this file's own "a claim is worth exactly what
  the code does" applied to a sweep that missed a line. The honest reason to
  keep the member is one the enum already carries: `HostKeyFromAPI` is a member
  nothing produces and says so plainly, so that is the precedent rather than
  `min_cuda`, and a constant costs nothing while removing one is a decision
  about the enum rather than a consequence of deleting a store.
  `TestNoProductionDeadCode` does not report it either way, because fields and
  types are out of its scope by the argument already recorded above. The stale
  waiver beside it -- `node.(HostKeySource).Authenticated`, excused as "ls
  reports the source itself" -- is wrong in the same way, is older than this
  change, and is left for whoever reads this.
  **And "no writer any more" was too generous to the constant: nothing has ever
  produced it, which makes it this list's first instance in a CONSTANT rather
  than a function.** The first wording in `ssh.go` blamed the deleted
  trust-on-first-use for having been its writer, and TOFU was not: it wrote a
  `Fingerprint` into its own map and never touched a `HostKey.Source`.
  `git log --all -G 'HostKeyPinned'` returns six commits -- the initial one,
  `b42130d`, `e08c99a`, `54393b1` and this change's two -- and at every one of
  them the only non-test appearances are the `const`, the `String()` map entry
  and, from `b42130d` to `54393b1`, a comment in `hostkey/store.go` reading "so
  callers record node.HostKeyPinned". No `Source: HostKeyPinned` assignment
  exists anywhere in that history outside a test file. The sharper measurement
  is that the instruction was unfollowable by the file holding it:
  **`store.go` never imported `internal/node`**, for the whole life of the file
  and not merely at a sampled commit -- `git log --all -G 'internal/node' --
  internal/hostkey/store.go` is **empty**, and that file has exactly two
  commits, `b42130d` creating it and `fae8b7a` deleting it. So the comment
  asked for something the file it sat in could not write, which is the scope
  that matters because `tofu` is declared there; the **package** could, and
  `mint.go` four files over imports `internal/node` and uses
  `node.HostKeyMinted`. That is the `ember ls --all` class in miniature -- an
  argument that persuades about a mechanism nobody wired -- and what #120
  deleted was the comment ASKING for the record, not a writer of it. So the running count above gains an instance whose kind the detector is
  **structurally** unable to report: `TestNoProductionDeadCode` loads functions,
  and the entry for it in this file already says fields and types are out of
  scope and why. A declared constant, printed by a `String()` nothing calls on
  it, asserted by its own test, is the same closed loop one declaration smaller.

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
- **Images are pinned to a tag or a digest; `:latest` is refused.** The point
  of ember is to measure what a model costs and how long it takes, and two runs
  a week apart that both say `:latest` may not have run the same software.
- **A placeholder in a committed config is a sentence whose refusal quotes
  it.** The video configuration (in `docs/configurations.md` since #144, and
  `ember.video.example.yaml` when this was written) carries
  `REPLACE_WITH_THE_DIGEST_PUSH_PRINTS`
  where a digest goes, which is #18: it refuses, correctly, but it reads as a
  configuration with a typo in it rather than as a template. So the
  code-model configuration (same document)'s one missing value -- there is no ember
  workload image carrying an LLM server, because an image has to carry
  `embernode` and sshd and this repository builds none -- is written as
  `EMBER-HAS-NO-LLM-WORKLOAD-IMAGE-SEE-THIS-FILES-HEADER`, with no tag, so the
  existing pin rule refuses it and the error *is* the instruction. Everything
  else in it is real, and the split is stated beside it: `store
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
- **The store is asked whether it holds the weights BEFORE a card is chosen,
  and the check existed the whole time as a command nobody called.**
  `EnsureAssets` runs over the SSH connection, which is the right place for a
  digest and the wrong place to discover an ABSENCE: on 2026-11-03 an
  `ember serve` rented an RTX PRO 6000 at $2.49/hr onto a `flux2` volume
  created two minutes earlier and spent **14m47s of a 15.1-minute rental** --
  **97.9%** -- pulling 56.16 GiB from HuggingFace at 68 MB/s with the GPU at
  0% utilisation and 0 B of VRAM. $0.61 of a $0.63 rental, and no images: it
  was stopped one second after `server_starting`. The queue reported
  `running` throughout, correctly, because **nothing between `node_ready` and
  `node_memory` emits an event**.

  **All three pieces already existed and none was joined to the rental path.**
  `runStoreVerify` is one `HEAD` per asset over the store's own endpoint, for
  nothing, and its own doc comment made the argument -- "Before a card is
  rented, which is the entire point [...] the cheapest moment to discover that
  a 14 GB expert was never uploaded is **currently** after provisioning, the
  image pull and the mount have all been paid for" -- with that "currently"
  true for the life of the command. `Plan.SyncAssets` rents a machine with no
  card for exactly this work and `grep -rn SyncAssets` returned **one**
  non-test caller, `ember store sync`. And verify's missing-asset line already
  printed the remedy. A capability built end to end and connected to nothing,
  reached through a CLI verb instead of a column, and this one has a price tag.

  **The two callers want opposite things from one fact, which is why it is a
  type.** `ember up` refuses with `*AssetsMissingError`, because an
  interactive command must not rent a second machine nobody asked for;
  `AppPlanner` catches it and fills the store on a CPU box first, because a
  queue drains unattended and is where nobody is watching to type the command.
  A plain error would make the second one a string match. The sync is a rental
  and is gated like one -- `refusePaid` and the period ceilings, the same two
  calls the GPU rental gets -- or `--allow-paid` would stop the card and not
  the thing that fetches fifty-six gigabytes.

  **A CPU-only run is not asked, and that is the seal rather than a flag.**
  `ember store sync` is the run whose job is to FILL the store, so refusing it
  for an empty store would refuse the remedy -- and it is also what stops the
  queue recursing, since the sync it starts is itself a plan built this way.

  **NOT a refusal when the check cannot be made.** RunPod issues S3 keys
  separately from the API key and an account can have one and not the other,
  so refusing would break working setups in order to save money on broken
  ones; the local provider has no endpoint at all. `AssetCheck.Asked` is a
  second result rather than an empty `Missing`, the `node.Exposure` shape,
  because a store this build cannot read and a store with everything on it are
  the same zero slice. It is said out loud at info, since a check that
  silently does nothing is how the saving is lost again.

  **A zero-byte object counts as missing**, which is what a PUT that failed
  after creating the key leaves and what `verify: present` would load as a
  model. And an error from the HEAD is returned rather than counted: "I could
  not ask" is not "it is not there", and a 403 counted as an absence would
  send `serve` off to rent a second machine and re-download forty gigabytes
  over a permissions problem.

  **The ORDER is what the tests assert, and the fake has no catalogue route so
  that it can be observed.** A refusal naming the weights means the check ran
  before `PickOffer`; a failure naming `list offers from runpod` means the run
  got past it. Both directions are live in both packages, which is the test of
  the split -- the identical config and the identical empty store make `up`
  refuse and the queue sync. The full-store cases assert the catalogue error
  **positively**, and that is not cosmetic: written as "not the sync refusal"
  they passed against reinstatement 4, where removing the no-credentials skip
  stops the run with an unsigned S3 call instead -- a different sentence and
  the same stopped run. The weak assertion was found by the reinstatement, not
  by review.

  **`AppPlanner.Out` was nil-dereferenced into existence by this change and is
  now `out()`.** Every other write on this path had a non-nil writer from
  `cmd_serve`; the first one reached from a test found the nil, and the panic
  would have been in the drain goroutine -- unwinding past
  `provision.WithNode`'s deferred destroy, with a node running. A leaked
  rental is what this package exists to prevent, so the nil is answered rather
  than documented.

  **Four reinstatements, each red, with a negative control that stayed
  green**, and the harness itself was wrong twice first -- in the shape this
  file already records. A marker that is a PREFIX of its own replacement
  survives the patch and reports a landed edit as "did not apply", and a
  marker matching a second, unpatched line does the same; both read exactly
  like a clean run. The harness asserts the marker the patch **introduces**
  now, never one it removes. The two it could not have caught without that:
  the queue branch and the zero-byte case.
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
  `sshx.Runner` is not widened across either. `tunnel.Forwarder`'s `closeAll`
  is the third of the family and is left as it is, for the reason already
  recorded above: on the cancellation path the leak is unrepresentable, and
  what it guards is the shape.
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
- **The queue's own store-filling rental was in no ledger, and the question
  worth an issue was which row it belongs to.** #142 taught `ember serve` to
  rent a machine with no card when a queued request's store is missing its
  weights -- the measurement behind it is $0.61 of a $0.63 rental spent
  downloading at a card's rate -- and `fillStore` called `SyncAssets`, which
  took no `report.Sink` and passed `p.Options` rather than
  `p.lifecycle(sink)`. So neither of the two moments a caller cannot otherwise
  see reached anybody: the node becoming ready, and what the rental cost,
  which arrives from `provision.WithNode`'s **deferred** teardown. `ember
  history` and `ember spend` were blind to a rental ember chose to make with
  nobody at the keyboard, which is the one kind of spending a ledger exists
  for -- "a total that is missing part of its history says so, every time",
  against the entry that says it. The issue's own first wording overstated it
  as one sync per request; `fillStore` runs on an `AssetsMissingError` and the
  next plan finds the store full, so it is one unrecorded rental per fill.

  **A ROW OF ITS OWN, and the other two candidates are wrong for reasons this
  file already records.** Charging it to the triggering request is the cheap
  answer and makes `requests.cost` hold two rentals, in the one column
  documented, read and summed as one -- so `ember spend`'s cost-per-image
  would divide a bill nobody can decompose, and it is wrong for every request
  after the first, which gets the filled store for free. A `rentals` table is
  the thorough answer and buys nothing today: every rental ember has ever
  recorded is already a request row, `Entry.Rode` already says which rows
  carry a rental's cost, and a second thing for `ember spend` to sum is a
  second place for the total to be wrong -- plus a migration, a fake method
  and conformance rows for a shape with one reader. What the sync IS is a
  rental nobody queued, which is exactly what `genstore.Adopt` and
  `queue.AdoptRun` exist for, so there is **no new column, no migration and no
  reader to teach**: `ember history` already prints a jobs-less run with a
  dash for images and card time and counts it in the footnote saying how many
  rows it left figures out of. What is NOT recorded is which request caused
  it -- there is no column for a parent and inventing one would be a migration
  for a field nothing reads -- so an operator sees two rentals, which is what
  happened.

  **`ember store sync` was the same hole with an operator at the keyboard**,
  and it is the same fix rather than a second one: `recordRental` -- `ember
  up`'s own helper, renamed because a name claiming one caller while two use
  it is the `AssertNoStray` defect -- opens the store, writes the row before
  the create call and cannot fail the run. `report.Text` renders nothing for
  any event a sync emits, so that command's output is byte-for-byte what it
  was.

  **A planner that cannot record refuses to rent, and the governing rule is
  `out()`'s rather than `refuseOverPeriod`'s.** The first wording of this entry
  cited the ceiling, which is the weaker of the two available: that one refuses
  a nil ledger only when a ceiling is **configured**, because what it honours
  is an operator who wrote `max_month_usd`, while this keys on wiring. What
  licenses keying on wiring is `AppPlanner.out()`'s argument -- a nil interface
  here is a crash, and `recordSync`'s first call is `Adopt`, so the panic lands
  in the drain goroutine **before** the sync's node exists: it takes `ember
  serve` down rather than leaving a machine running, and a panic unwinds
  `provision.WithNode`'s deferred destroy on the way out in any case. So
  answering the nil is cheap and the useful answer is a refusal, because an
  unrecorded rental is the defect being fixed and a nil recorder would reopen
  it silently. Nothing an operator can type reaches it: `cmd_serve.go` is the
  one production construction and always wires `Runs`, so it guards the shape.
  `AppPlanner` therefore holds a `RunStore` BESIDE its `Spender` rather than
  one `genstore.Store` -- five methods that can record a run and cannot
  `Enqueue` or `ClaimNext`, because a queued row is what a drain loop takes
  and a layer able to write one could rent a second card from the record of
  the run already in flight. Two narrow types over one store, which is
  `Spender`'s own argument in the other direction: what a layer can do to the
  database is visible in its type. And `Plan`'s doc comment said "rents
  nothing", which stopped being true the day `fillStore` landed; it is
  corrected rather than left as one more argument that persuades about a
  mechanism nobody re-read.

  **The reinstatement found the `--allow-paid` gate on that rental could not
  fail, which is a worse defect than the one being fixed.** This file says the
  sync "is a rental and is gated like one"; the test for it asserted `err !=
  nil`, and `internal/queue`'s fake served **no CPU catalogue** -- so the run
  stopped at a 404 before `refusePaid` was ever reached, and deleting that
  gate outright left the test GREEN. Measured both ways rather than argued:
  against the tree as it was, no CPU route and the weak assertion, deleting
  the gate is **exit 0**; with the captured `cpus.json` served and the
  assertion naming the flag, it is **exit 1**. The fake serves the provider's
  own committed fixture rather than a second copy of a field shape, and there
  is still no `/v2/pods` route -- which is what makes the create failure
  reachable from a unit test at all, and the create failure is the rental most
  worth recording, because the row goes down before the call.

  **Eight defects reinstated, each judged on an exit code, beside a negative
  control (a reworded comment, reported as not caught), that historical
  measurement, and a control at both ends.** Seven of the eight went red where
  expected and the eighth is an asymmetry worth writing down rather than a
  miss. The first wording here said nine and eight -- in this file, which
  records that "a running total in prose is the thing this file has been wrong
  about before" -- while the table it describes lists eight defects, a control
  and a measurement. So the count was wrong about itself on the first run,
  which is one more instance of the thing rather than an exception to it.
  The sink wired and the lifecycle hooks not -- `p.Options` in place of
  `p.lifecycle(sink)`, which is the original defect's own shape -- is **GREEN
  in the unit suite and RED in the lab**, because the unit row's create fails
  before a node exists, so it has no node and no cost either way. The lab is
  the only place in this tree that can assert the MONEY: every other rents
  from the local provider, whose price is zero, so a cost assertion there
  would compare a blank against a blank and pass against the defect.
  `runpodfake` prices a CPU pod at $0.06/hr and a GPU pod at $0.89/hr, read
  back off the pod as the bill rather than as a list price, so the sync row
  carries a real rate and `ember spend`'s period total has to equal both
  rentals -- which is also the first assertion in this repository that the
  aggregate counts a rental ember started on its own initiative.
- **`min_cuda` is gone, the recorded measurement that decided it was verified
  rather than trusted, and the one guard it rests on was made to fail.** The
  entry above refused to enforce the key and left the removal to "whoever reads
  this" -- this is that reading. Three things were checked before anything was
  deleted, because three of the last four audits of this file found a rule it
  states that the code does not. **The measurement HOLDS**, which is the
  outcome nobody would otherwise write down:
  `TestCatalogue_ReportsCudaVersionsAndTheyAreNotAFloorForMinCUDA` exits **0**
  against the committed capture, and `available` still tracks the top-level
  summary on all four rows -- LOW gives every version true, NONE gives every
  version false, the AMD card reports none at all, and the RTX 4090 reads NONE
  overall with `LOW` in US-IL-1 and all eight versions false. It was also made
  to go **red**: flipping one of those eight to `available: true` is exit 1 at
  the named Errorf, so the green is an answer rather than a dead assertion. The
  first attempt at that flip did not apply at all and reported exit 0, which is
  the harness trap this file records, caught by asserting the marker the patch
  introduces.
  **What is removed is the key and its plumbing, not only the documentation.**
  The defect was never the refusal: RunPod's was loud, correct and named
  `gpu_probe`. It was that one documented key meant a refusal on one provider
  and **silence** on the other -- `local` has no CUDA version to compare, so it
  dropped the value -- and the provider is the wrong layer for an answer that
  is the same everywhere. So `config.Config.MinCUDA`, `node.SpecOptions.MinCUDA`,
  `node.Spec.MinCUDA`, the `app.plan` wiring and the RunPod refusal are all
  gone, and `config.rejectRemovedKeys` carries the one sentence a layer up,
  before any provider is chosen. **`KnownFields(true)` already made the removal
  a refusal rather than silence**, which is why this is about the message and
  not about the check: yaml's is "field min_cuda not found in type
  config.Config", which names a Go type an operator should never see and no
  remedy, and the person who hits it is the one who followed `README.md`.
  **Presence and not emptiness, which a test had to be strengthened to see.**
  `min_cuda:` with no value decodes as null, so the pointer version of the
  check reads it as absent -- and it is the spelling an operator reaches for to
  turn a key off without deleting the line. The first version of
  `TestLoad_RefusesAMinCUDAWithNoValue` asserted `err != nil`, which
  `KnownFields` satisfies whatever the check concluded, so it **could not
  fail** -- "what would this say if the bug were present?" answered with "the
  same thing", found by thinking the reinstatement through before running it
  rather than by running it. It asserts the sentence now, and that is what
  goes red.
  **Nothing anywhere gives a CUDA runtime version ember can act on for free,
  and the one new observation is written down rather than built on.**
  `nvidia-smi` **does** report one -- `--version` and `-q` both print
  `CUDA Version`, measured here as 13.0 on driver 580.126.09, and `README.md`
  already records 13.2 on a rented 4090 -- while `--query-gpu` has no such
  field, only `driver_version` and `compute_cap`, which is `node.ComputeCap`
  and a different number. So AGENTS.md was right that `compute_cap` is not it
  and incomplete in implying no host reports one. It is still not a floor ember
  can enforce: it is read on the node, after the pod is billing, and the
  comparison is the unmeasured half -- CUDA minor-version compatibility means
  an image built against 12.6 runs on a 12.x driver older than 12.6, so
  a floor over the driver's figure refuses hosts the image works on,
  which is `hasCapacity`'s own defect reached a third time. Settling it needs a
  rental on a host with an old driver and was not made. `pod_running.json`'s
  `"cudaVersion": "12.4"` is **confirmed** schema-derived and undecoded:
  `grep -n Cuda internal/provider/runpod/types.go` returns nothing.
  **One thing is NOT done and it is bookkeeping rather than behaviour --
  `node.CUDAAtLeast` alone, and the first wording of this entry said "and
  `ParseCUDAVersion`" and was wrong.** That is this file's own recurring defect
  committed by the change that is correcting one, and it was caught in review
  rather than by a test: the parser is **live**, through
  `node.ParseComputeCap` at `compute.go:234` and `local.Provider.Offers` at
  `local.go:270`, which is the
  `compute_cap` path this very entry names as the mechanism that works. So the
  claim was false about the one function #91 settles on, the stated follow-up
  would not have compiled, and the same commit edited `compute.go`'s doc
  comment in a file whose own line three lines above the call already said the
  two share a parser. What makes it worth recording rather than quietly fixing
  is that `TestNoProductionDeadCode` had it right the whole time -- with the
  waiver removed it reports `internal/node.CUDAAtLeast ... (3 site(s))` and
  never names the parser -- so the detector and the comment disagreed and the
  comment was the one a reader would have believed. **A claim about what calls
  what is checkable with one grep, which is exactly why nobody ran it.**
  `CUDAAtLeast` is the callerless one and should go the way
  `node.RateFromFloat` went -- premise read from a schema, and a live second
  mechanism in `gpu_probe`, which are both halves of that precedent. It is
  still here because `cmd/check-coverage`'s `deadExcluded` names it and fails
  over an entry it stops reporting, so the function and its waiver have to be
  deleted in one commit and that file was held by another change (#161).
  **Three edits, not three lines**: the function, the waiver, and
  `TestCUDAAtLeast` at `spec_test.go:313`, which calls it at five sites and
  must go with it or the package will not build. The waiver's stated reason -- "deleting this
  would remove the evidence that the key is unenforced" -- is **stale as of
  this commit**, because the evidence is now the refusal, the capture guard and
  `TestCreate_SendsNoCudaFieldAtAll`, none of which is that function. The two
  judgements worth keeping belong to the parser that stays rather than to the
  comparison that goes: strings rather than a float, because "12.10" is not
  12.1, and compared as a pair of integers for the same reason.
  **And the comment guard caught this change citing `ember ls --all`.**
  `rejectRemovedKeys`'s doc comment reached for that flag as its precedent for
  "a documented thing nobody can use is worse than no documented thing" --
  which is the eighteen-comment sweep walked into by the change removing a key
  for exactly that reason. `TestComments_CiteNoFlagThatNoFlagSetDeclares`
  reported it on the first full run, exit 1, and the citation is gone. A guard
  whose first catch is the commit that was quoting it is the best evidence it
  was worth building.
- **`node.CUDAAtLeast` is gone, and the one-commit constraint was measured
  rather than argued (#161).** The function, its `deadExcluded` waiver and
  `TestCUDAAtLeast` went together, and `ParseCUDAVersion` stayed because
  `ParseComputeCap` calls it. Over the same tree, with `go build ./...` at exit
  0 in every row: all three gone is `go test ./cmd/check-coverage/` **0**;
  function and test gone with the waiver kept is **1** ("deadExcluded names
  internal/node.CUDAAtLeast, which this guard no longer reports"); waiver and
  test gone with the function kept is **1** ("nothing anywhere calls it");
  waiver gone alone is **1** ("only test code calls it (3 site(s))"); and the
  function gone with the test and waiver kept does not compile
  `internal/node`'s tests. Losing only the test is **0** under `go test
  ./cmd/check-coverage/` and **red** under `make cover-check` -- `internal/node`
  at 98.5% against its 100% floor, with the function at 0.0% -- so the test has
  to go either way, and "green" is a claim about one command. The two
  judgements are a note on `ParseCUDAVersion`, the `node.RateFromFloat` shape.
  The waiver's reason, "the evidence that the key is unenforced", was stale the
  moment `min_cuda` went, and left with it.
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
- **`GET /api/nodes` answered 501 in every `ember serve` there has ever been,
  and it is WIRED UP rather than deleted because the browser has no second
  mechanism for the one charge nothing ends (#105).** `webapi.Options` had no
  `Scan` field, `New` never assigned the Server's `scan`, `cmd_serve` could not
  have set it if it had wanted to, and the three assignments in the tree were
  all in test files reaching past the constructor into an unexported field of
  their own package -- so the suite could not tell "the production value is
  missing" from "this test did not set it", which is exactly how it survived.
  `web/` had no type for the answer either, which is why nobody had ever seen
  the refusal. One more of the things here written, covered and connected to
  nothing, and the first where the missing join is one struct field in the
  package's own constructor.

  **The test is this file's own and the answer is `sdserver.Client.Cancel`'s,
  not `queue.Cost`'s.** `queue.Cost` went because `genstore.Spend` answered the
  same question **to the same reader through the same command**; `RateFromFloat`
  went because `Offer.Validate` was the live copy of its ceiling; `exposure.Probe`
  went because the join was not available at all. Here the reader is a browser
  and `ember ls` is a terminal command: there is no second path from the page to
  "what is billing", and the join is one field and one closure. The sharp half
  is that `ember ls` is *not* the second mechanism it looks like -- the one-time
  token URL means the operator had a shell to get the page, but `ssh -L
  7777:localhost:7777` is the deployment this file already argues about, and
  that operator's shell is on the other machine.
  **The forwarded-port operator carries that argument alone, and the stronger
  version of it was written down first and is not supported.** "An operator who
  only ever opens the web UI" needs a configured `serve_token` as well as the
  forward -- a minted one does not survive a restart and is printed on the
  machine running the server -- and no lab and no deployment in `README.md`
  exercises that pair, so the pure case is a shape rather than an observation.
  What does not need it is that **`ember ls` on that laptop is a different
  installation and not the same mechanism reached differently** -- and the
  reason offered for that in review was that #146 made the CLI a socket client
  and a unix socket does not travel through `ssh -L`. **Checked, and it is not
  the reason**: `s.sockMux` carries exactly one pattern beyond the API,
  `POST /api/runs`, and `runLs` calls `app.Scan` directly at both of its sites.
  `ember ls` has never been a client of `ember serve` at all. The true reason
  is larger: it loads the config, reads the credentials and asks the providers
  **itself**, so reaching it from the laptop means a second ember installation
  *with the provider credentials on the laptop* -- which is the thing the
  browser is there to avoid rather than a packaging detail. Recorded at this
  length because the weaker reason is the plausible one, and this file's own
  rule is that an argument about a mechanism is worth one grep.
  And the fact with no other carrier for that operator is the narrow one: the
  **standing storage charge**, which `ember spend` and `ember history`
  deliberately do not price because pricing a store means asking a provider.

  **Wiring the Go half ALONE would have moved the defect rather than closed
  it**, which is why the panel is in the same change: a route that answers, a
  projection that is correct and nothing reading either is "a query layer with
  no reader", one layer out. The issue offered the browser half as possibly its
  own change and that is the one part of it refused.

  **Three defects were found by connecting it, and the first is the whole
  lesson in one line.** `viewFinding` wrote `Kind: string(f.Kind)` and
  `reconcile.Kind` is a `uint8`, so the conversion took the numeric value as a
  rune: every finding reached a client with a kind of `"\x03"` or `"\a"` rather
  than `expired` or `store`. `go vet`'s `stringintconv` does not report it --
  byte-to-string is a legal, ordinary conversion -- and no test could, because
  nothing in either language had ever read the field. Second,
  `nodesView` dropped `app.Scanned.Leftovers` entirely while its doc comment
  said "`ember ls`'s answer" and `Attention` claimed to be "the number `ember
  ls` exits 2 for": the projection was two of three slices and a count over
  two, so a bridge network whose node is gone -- the finding whose cost is
  every local run failing at once weeks later -- was absent from the UI, and
  `viewFinding`'s id switch had no arm for one either, so it would have
  arrived anonymous. Third, the 501 itself.

  **`app.ScanWith` exists so the server does not re-read the authority.**
  `app.Scan` takes a config PATH: it loads the file, reads the credentials and
  builds a registry, every call. `runServe` reads that file once on purpose --
  it is the ceiling, and "a request must never be able to name which file sets
  its own limits" is why the path comes from a flag -- so a handler calling
  `Scan` would have built the second pair `ember reap`'s own comment refuses,
  where "the one that disagrees is the one that does not destroy the node". The
  closure in `cmd_serve` is over the registry and the journal that process
  already holds, and `Scan` is now `ScanWith` plus one config read, so there is
  exactly one classifier.

  **NOT CACHED, AND NOT POLLED, and those are two separate decisions.** Three
  provider sweeps per call, on the HTTP request's own context, which is
  `getOffers`' shape and `getOffers`' exposure. Memoising would be
  `local.DetectGPU`'s defect pointed at money -- a cached "RunPod said nothing"
  is precisely "I could not check RunPod" rendering as a clean bill of health,
  and a cached listing of what is billing is a stale bill -- so the only lever
  left is how often it is asked, and the panel is a button. No clock either:
  `DetectGPU` refused to invent one ("a clock nobody here has measured") and
  the request's context is the bound, as it is for the offers panel.

  **An unreachable provider is a 200 and the 502 is for a sweep that could not
  START.** `reconcile` emits a `KindUnreachable` finding per provider it could
  not ask, so a sweep that reached the local daemon and not RunPod is complete
  about the first and named about the second -- and losing the first to report
  the second would hide what the daemon just said. What the 502 means is that
  the journal of live nodes would not read, where every classification below it
  compares a provider's answer against records that are not there.

  **The `Line` field is deleted and the judgement is kept, which is the
  `Envelope::next` shape.** `findingView` carried `reconcile.Finding.String()`
  beside the parts it is built from, argued for as "an operator reading both
  should see the same sentence in each" -- and that argument does not survive
  contact with a browser: `String` is `%-14s %-28s %s` and HTML collapses runs
  of whitespace, so the string in a page is not the sentence a terminal shows
  unless the page also asks for `white-space: pre`, which would put a
  terminal's column widths inside a table that has columns of its own. The
  parts are what both channels can agree on, because Go still formats every
  money figure exactly once. `Monthly` is added beside `Accrued` for
  reconcile's own reason -- money already spent stops when the node is
  destroyed and a standing charge does not -- and the test of the whole type is
  now that **every field on it has a reader in `web/`**, which is the property
  it did not have. `Provider` is the one that cannot be trimmed: an unreachable
  finding has no node, no record, no store and no leftover, so its id is empty
  by construction.

  **`Attention` stays on the wire and is never derived in the page**, although
  every kind needed to derive it is there. `Kind.NeedsAttention` is "a switch
  rather than `k != KindOK`, so that a kind added later needs attention by
  default", and a second copy of that switch in Rust is a copy whose author
  will not be reading the Go one -- which is why `Money` crosses this wire
  twice as well. It is an `i64` in the page because the Go field is an `int`
  and this seam has already lost a whole table to a number the server could say
  and the client could not hear.

  **No per-row tolerance, and that is `Rows`' own rule rather than an
  omission.** That entry says `requests` ONLY: it is the envelope where one bad
  row costs the view of every good one and the one with somewhere to say so.
  This envelope has an error of its own on the way in -- a 502 reaches the
  panel as `api::reason`'s sentence -- and its numbers are a count and two
  money figures rather than ten `i32`s a request can set.

  **What a browser can check and what it cannot.** It can check the join:
  whether the button fetches, whether Chrome's cookie reaches `/api/nodes`,
  whether Go's bytes decode into `Nodes`, and whether the rows reach the DOM --
  which is the `<For>` class this file has now been bitten by three times, and
  the reason `findings_table` has no key at all. The first assertion is that
  the panel says **nothing** before the first press: `Nodes::default()` has
  three empty slices, so its verdict is "nothing is running and nothing was
  left behind", and rendered unasked that is a clean bill of health over a scan
  that never happened -- the signal is an `Option` for exactly that, which is
  `AssetCheck.Asked`'s distinction one package over. What it cannot check is
  the money: the local provider prices a Docker volume at nothing, so
  `monthly_storage` is zero in every lab here and only its RENDERING is
  assertable -- the same bound that stops a lab producing a non-zero
  `requests.cost`. A non-zero standing charge needs a rented store.
  `Nodes::verdict`'s unreachable clause has no lab either, for the reason the
  offers panel's error path has none: `app.Deps.CloudBaseURL` is a Go field, so
  a lab running `ember` as a subprocess cannot point a cloud provider at a
  closed port. `cargo test` drives it instead, in both directions and over the
  case where the store listing failed and the node listing did not.

  **Reinstated, judged on exit codes, with a control at both ends of every
  pass and four negative controls. Eleven defects, each red.** Through
  `go test`: the `scan: o.Scan` assignment removed from `New` (exit 1 at
  `TestNew_CarriesTheScannerFromItsOptions`); `string(f.Kind)` back (exit 1);
  the `sc.Leftovers` loop removed from `viewNodes` (exit 1); `Attention`
  summed over two slices (exit 1 at 3-want-4); the leftover arm removed from
  `viewFinding`'s id switch (exit 1); and `var v nodesView` in place of the
  three `[]findingView{}` (exit 1 on the bytes). Through `cargo test`, all
  exit 101: `unreachable()` narrowed to `findings`; the verdict's blind
  clause deleted; and `nothing_running` back to two clauses. Through the labs,
  which are the half that proves the production wiring: **`Scan` not set in
  `cmd_serve`** is exit 1 with **two** labs failing, at
  `GET /api/nodes answered 501: this server was built without a node scanner`
  -- the refusal an operator would have got for the life of the feature; the
  dropped leftovers again over the real wire (exit 1, `leftovers = []`);
  `string(f.Kind)` again over the real wire, which names the characters
  themselves (`kind = "\a"`, `kind = "\x06"`); the panel rendering
  `Some(Nodes::default())` instead of `None` (exit 1 in Chrome at *the panel
  answered before anything was asked: verdict="nothing is running and nothing
  was left behind"*); and `findings_table` returning nothing, which is the
  `<For>` class the browser lab exists for (exit 1, *never reported the
  leftover within 1m0s*, with the verdict still reading "1 finding(s) need
  attention" -- so only the rows went, which is the defect's exact shape).
  Four negative controls -- a reworded Go comment, a reworded Rust doc
  comment, a reworded comment through the labs and one through the browser --
  all green. **One attempt reported BUILD-ERROR rather than a result**, which
  is this file's own trap working: a marker appended to `Kind: string(f.Kind),
  Provider` put a `//` comment inside a composite literal, and the harness
  refused to read a tree that would not compile as a failing suite. Both
  controls green at both ends of all four passes, and `git diff` empty over
  every file the harness touched -- which is the check added after a pass
  shipped claiming a deletion that a stale backup had put back.
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
- **Every measurement that was only in a config file's header is in
  `docs/configurations.md` now, and the entry above about the placeholders
  named two files that no longer exist and is REPOINTED rather than left
  (#144).** Leaving it was argued for first, as the record of #18's lesson --
  and the standing practice in this situation is to repoint, which the same
  change applied to `imagelab_test.go` and `served_test.go`, so leaving this
  one was inconsistent with its own commit. One phrase each keeps the #18
  record intact and stops a reader following a filename to nothing: the
  refusals that entry describes are unchanged, and what moved is only where
  the two configurations are written down. The
  numbers that had no second home anywhere in the tree, listed because the
  one unacceptable outcome of that change was losing one: the 70 GiB store's
  standing charge (**$0.0067/hr, $0.16/day, $4.90/month**, which `ember ls`
  prints and only `ember store rm` stops); the weight breakdown behind the
  56.16 GiB (**23.33 GiB of it a prompt-reading LLM, 32.60 GiB the diffusion
  file alone**, against a registry that refuses a layer above ~10 GiB); the
  push arithmetic that `store sync` exists to avoid (**56.16 GiB at 67 Mbps
  is 1h52m**, after about an hour of pulling it home first); the **11 seconds
  and two hundredths of a cent** a sync cost to learn that the older node
  image read `embernode fetch` as "be a node"; **EUR-IS-1 is the only one of
  the ten S3 datacentres with three cards at 80 GiB or more**; the per-image
  dollar table for a resident model against a fresh process at four batch
  sizes on an RTX PRO 6000 at $2.49/hr (`cmds` $0.1936/$0.1895/$0.1696/$0.1075
  against `sd.jobs` $0.2378/$0.1302/$0.1249/$0.1055, crossover between one and
  two images); **sd15 Q8_0 at 256x256, 0.44 s per job served against 1.91 s
  fresh**; **56 GiB of weights in a 125 GiB machine behind an 11 GiB card**;
  and, for the code model, **18,556,689,568 bytes / 17.28 GiB**, its digest
  `fadc3e5f…` confirmed twice for two requests and no bytes, **37 minutes to
  push at 67 Mbps**, about **20 seconds to verify at 868 MiB/s**, and **$8.00
  a day, $160 a month** for a card idle most of an eight-hour session. The
  ones that already had a home in `README.md`, `AGENTS.md`, `docs/` or a Go
  comment -- 67 Mbps, 868 MiB/s, $1.79, 4412 MiB, 35 GB per step, the $0.37
  CPU-fallback run and its 122-second text encoder, 14 GB per Wan expert, the
  33/77/81 frame counts -- were checked for by grep rather than assumed.
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
