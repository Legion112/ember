# What only a browser can check

The class of defect no unit test reaches, and the assertions that turned out
unable to fail.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

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
- **A page cannot read an artifact's extension, so it is TOLD what the file is
  -- and the thing it must not be told is the request's KIND (#190).** The
  browser half of speech is two decisions and both had an obvious wrong input.
  `imageRef.Path` carries no extension by design: the route is
  `/images/{job}/{index}` and the blob key is the server's business, so a
  gallery choosing between an `<img>` and an `<audio>` has nothing on the wire
  to choose from. The short answer is the request's kind, which is already
  there for the asking -- and a kind is a property of the REQUEST while an
  element is a property of the FILE. They agree today, because a speech rental
  writes wavs and an image rental writes pngs; they stop agreeing the first
  time one kind writes two sorts of artifact, at which point a page switching
  on the kind renders every one of them as whichever the kind implied, with
  nothing reporting it. It is also a second reading of a question Go answers
  already, which is `queue.Cost`'s shape with a media type in it. So
  `imageRef.Media` is the SAME `artifactType` the route calls over the SAME
  key -- one derivation, two readers -- and the page's element and the
  response's `Content-Type` cannot come apart.
  **The join is the assertion and not the pair of values.**
  `TestArtifact_TheViewDeclaresWhatTheRouteWillSend` seeds a png and a wav on
  ONE request -- which no real rental produces, because `kind` is on the
  request so one rental runs one driver -- and compares each artifact's
  declared `media` against the `Content-Type` that fetching it actually sends.
  Both kinds in one request is the point rather than a convenience: it is the
  only arrangement in which a `media` keyed off the request's kind gives a
  different answer from the route, and a test with one row each could not see
  it. The two being DIFFERENT is asserted beside it, because a view and a
  route that both answer one type for everything would pass the comparison.
  **The error is dropped and the field goes empty, which is the one place this
  projection may not refuse.** A request view that failed because one of
  twenty artifacts has an extension the switch does not know would take the
  other nineteen with it -- the prompts, the cost and the reason it ended.
  That is `Rows`' rule one envelope over, reached through a media type instead
  of an `i32`, and the empty field is not a hole: the route answers 500 for
  that same key, so it is a defect ember is already refusing to serve.
  `Artifact::Unknown` SAYS so rather than falling back to an image, because an
  `<img>` and an `<audio>` both render perfectly over a fetch that failed and
  show nothing -- the "reads as working" defect in one element, where the two
  candidates for the fallback are the two elements that share it.
- **The window `/api/config` leaves open is closed by the selector having no
  options, and the method's arm for it is the second mechanism.** The config
  is fetched after the page mounts, so there is a window in which a kind
  selector exists and ember has not said whether it has a voice -- and an
  empty `<select>` posts a blank `voice_id`, which the server refuses after
  the operator has typed their sentence. The selector is built from
  `config.kinds` and from nothing else, so during that window it has no `tts`
  option and the window cannot be entered through the page at all.
  `Config::composing`'s unanswered arm is what holds if that stops being true
  -- a `?kind=` in the URL, a remembered choice, a default taken from
  somewhere else -- and it is reachable from the type rather than from the
  DOM, which is what lets `cargo test` hold it. Containment twice, by two
  mechanisms, as the tar extractor is.
  **THE TWO REFUSALS ARE TWO SENTENCES BECAUSE THE REMEDIES ARE.** A config
  that has not answered is a wait; a config with no `tts.voices` is an edit to
  `ember.yaml` and a restart. One wording for both would tell an operator
  whose file is correct to go and change it, and an operator whose file is
  empty to keep waiting -- so the test asserts each sentence AND the absence
  of the other, which is the arrangement the posture line's three spans
  already use. The discriminator is `kinds.is_empty()`, and it is sound only
  while no real response can carry an empty list: `genstore.Kinds()` is
  derived from nothing, and `TestConfig_PublishesAtLeastOneKind` is the exit
  code behind that rather than a property nobody checked. A projection that
  one day filtered the kinds by what this config can run -- a plausible
  change, and the page has no way to see it coming -- would publish `[]` and
  the page would sit saying it was still waiting.
- **The posture line told the configuration speech is FOR that it could run
  nothing, and the browser lab's own refusal probe is what found it.** The
  header's red warning was `!servable`, and the sentence it printed said "a
  queued request would have nowhere to run" -- true while an image generation
  was the only workload, and false for a `tts.voices` list with no `sd:`
  block, which runs speech perfectly. In red, above a compose panel that was
  about to work. That is the `proj` caption's defect -- a caption true of one
  mode -- on the one line a page uses to say what it cannot do, and a warning
  that is wrong about the thing the operator is looking at is worse than no
  warning at all. `Config::unrunnable` is one sentence per state now, and it
  stays silent before `/api/config` answers because a default `Config` is
  neither servable nor voiced.
  **Found by the harness rather than by review, and by the half of it that was
  itself wrong.** The lab's "race the outcome against what means it is never
  coming" probe read every `.fatal` on the page, including the header's, so
  the opening control stopped at the first poll naming that sentence. The
  probe is scoped to `main` plus the app's own unreachable line now -- the
  header's is a standing fact about the config and not a refusal of this
  request -- and the lab asserts the corrected wording in BOTH directions,
  because a check for the new sentence alone would pass against a header
  printing both.
- **What `requestView` does not carry is the text, and the gap is stated
  rather than worked around.** A request's jobs are projected to the browser
  in the image shape -- `newJob`, with no `tts` -- and the view carries no
  `kind` either, so a finished speech request arrives at the page as one job
  with every field at its zero. Queueing and playing work; reading back what
  was said does not, and the page cannot even tell a speech job from an empty
  one. What that would otherwise cost is the "what it ran with" block:
  `Job::settings` pushes `seed`, `sampler`, `scheduler`, `clip skip`, `eta`,
  `flow shift` and `skip-layer guidance` unconditionally, so a default job
  renders seven rows of dashes under an empty prompt -- a page that reads as
  ember having lost the request. `Job::has_parameters` is the honest render in
  the meantime and not a fix: it is `*self != Job::default()`, derived from
  the type rather than from a list of field names (`sdFieldsSet`'s rule in
  Rust), and it stops applying the moment the projection sends an utterance,
  which is the right coupling -- whoever adds the field has to add its rows.
  The fix is a `tts` on `newJob`'s outbound half and belongs with whoever
  makes it.
- **The browser lab is the user's sentence and the duration is why it is worth
  running.** Type text, choose speech, submit, and get audio that plays. Every
  decision under it has a `cargo test` -- which controls a kind shows, whether
  an utterance can be sent, which element an artifact wants -- and all of them
  are green with no browser anywhere; what no Rust test can see is whether the
  three are JOINED to the page and to the server. And "the page has an
  `<audio>` tag" is satisfied by a page that is completely broken, because an
  element whose source 404s renders perfectly: `duration > 0` is the browser
  saying it demuxed a stream out of what the route sent, which cannot be true
  unless the id resolved against the catalogue ember was started with, piper
  ran, the row was keyed `.wav`, the route declared `audio/wav` rather than
  the `image/png` it declared unconditionally until a second kind existed, and
  `nosniff` beside that declaration did not stop Chrome playing it. Measured
  on this tree across five runs of one sentence: **3.37s to 3.52s** of audio,
  from `/api/requests/{id}/images/1/0`. **The spread is unexplained and is
  recorded as unexplained**: either piper's own sampling is not deterministic
  at its default `noise_scale`, or `duration` under `preload="metadata"` is a
  progressive estimate and the poll returns on the first value above zero.
  Nothing here distinguishes them, which is why the assertion is a floor under
  the sentence rather than an equality -- a figure this lab cannot explain is
  not a figure to compare against.
  **A media error is conclusive and is not waited out**, which is `orExited`'s
  rule as the browser labs spell it and which this file already records
  costing one run fourteen minutes: a 404 or a refused type is exactly what
  this lab is for, and spending two minutes on it would report a timeout for a
  failure already decided. The same race covers the queue wait, against the
  page's own refusal -- a body `crossKind` would not take leaves the panel
  showing the server's sentence and the loop with nothing coming.
  **AND THE LAB'S OWN FIRST VERSION OF THAT LOOP COULD NOT FAIL, which the
  pass found and review did not.** The assertion that the image controls are
  GONE asked for an `<input>` under each label -- and `preview` is a
  `<select>`, as `sampler` becomes once a rental has cached a capability list.
  So the one case it exists for, the preview row left rendering over a speech
  request, was **GREEN** against a patch that put it back: "what would this
  assertion say if the bug were present?" answered "nothing, it cannot see
  that element". It asks about the LABEL now, which is tag-agnostic and cannot
  go stale when a control's element changes, and the reinstatement is red
  naming both controls in the row.
- **Nineteen reinstatements, each judged on an exit code, two negative
  controls green, controls at both ends of both passes, one backup per path
  taken once, and every case ending in a comparison against that backup.**
  Fourteen through `go test` and `cargo test`: the declared type not
  projected, `kinds` filtered to nothing, audio rendered as an image, an
  untyped artifact guessed at, the two speech refusals collapsed into one,
  speech offered with no voices, each half of `Utterance::refusal` dropped
  separately, `kind` sent on every request, `tts` sent as `null` on an image
  one, a batch row describing an utterance as an empty prompt, a voice offered
  with no text, the parameter block rendered over a job nobody described, and
  a hard-coded `"wav"` in a download link. Five through Chrome, which are the
  five nothing else reports: the preview row unconditional, the gallery
  rendering `<img>` for every artifact, the speech job built from the image
  draft (red at the server's own `crossKind` sentence), the text area not
  writing the draft (red at `nothing to say`), and the header telling a
  speech-only config it runs nothing.
  **Three harness defects came first and each is one of the shapes this file
  already records.** The dropped-field patch left a Go local declared and not
  used, so the first attempt was a BUILD-ERROR rather than a verdict -- the
  trap working, since a build failure read as a failing suite is a false red.
  `cargo fmt` had reflowed the arm one patch was anchored on, so that case
  reported ANCHOR-MISS. And the Rust negative control is ADDITIVE -- its
  anchor is a prefix of its replacement -- so asserting the anchor was gone
  reported NOT-REPLACED, which is the fourth shape and is now a per-case flag
  rather than a check relaxed for everything.
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
