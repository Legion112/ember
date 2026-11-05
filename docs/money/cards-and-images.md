# Cards, images and architectures

Image pinning, the wrong-architecture abort, offer filtering and reclaim, and
why `min_cuda` could never be enforced.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

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
