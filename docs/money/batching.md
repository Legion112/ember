# Batching and per-rental overhead

What the overhead is per rental rather than per result, what `cmds` can and
cannot do, and what a resident model is worth.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

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
