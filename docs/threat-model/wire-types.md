# The Go to Rust seam

A nil slice is not an empty list, a Go `int` is not an `i32`, and a tag that
never fires reads exactly like one that works.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

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
