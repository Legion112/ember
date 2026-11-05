# Generation parameters and uploaded images

Which parameters are typed and why the rest are not, zero as a value rather
than a sentinel, and an upload keyed by its own content.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

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
