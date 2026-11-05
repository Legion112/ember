# Stores, S3 and assets

A store outlives every node that mounts it. Mount refusals, digest
verification, the S3 client, orphaned uploads and the pre-flight.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

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
