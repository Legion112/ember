# Batching and per-rental overhead

What the overhead is per rental rather than per result, and what a resident
model is worth.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

- **A resident model removes a variance of 81s and $0.056 per image, and what
  caused that variance is still not known.**
  FLUX.2 [dev] Q8_0 at 2048x2048, 28 steps, on a rented RTX PRO 6000 at
  $2.49/hr with the weights on a network volume, four batch sizes, same
  prompts and seeds. A resident server is flat -- every job after the first
  took 140.80-140.88s, on every node. Reading the weights per image was not:
  on a node that kept the 56 GiB in page cache that read cost 145.3s, and on
  one that did not, 226.6s, because it came off the store at a measured
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
  "551ms of loading is paid once rather than 8 times" while the other arm of
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
  re-doing setup. A batch runs them on one node and pays it once. The cost was
  worst exactly where ember is supposed to help.
- **A batch brings each result home as it is produced.** Collecting once at
  the end means an expired TTL, a dropped connection or a closed laptop loses
  everything rather than everything so far -- and an operator cannot look at
  the third image and decide to stop paying for the other seven. A ten-job
  batch is over an hour of a rented card, which is a long time to be holding
  every result on a machine that is about to be destroyed.
- **A failed job stops the batch, and that job's output is still collected.**
  A batch where job 3 failed and 4 through 10 ran anyway is a batch nobody can
  trust, and the remaining jobs bill for work the operator no longer wants.
  The output of the job that failed is often the one that explains why, so it
  is collected before the stop -- on a context that survives Ctrl-C, for the
  reason every other fetch is.
- **A job reports card time, not its share of the bill.** The rental also
  paid for provisioning, the image pull and the asset verify, and a batch pays
  those once for all of it rather than once per job -- which is the entire
  saving. Folding them into a per-job figure would overstate it, so the
  wording says "of card time". It is timed across bringing the result home as
  well as the generation: the card stays rented while the output comes over
  the wire, so a figure that stopped at the generation would understate every
  job that moved anything large -- which is the direction a money figure must
  never be wrong in.
- **`max_run_usd` cannot name the job it bit on, because it bites before any
  job exists.** The issue asked for that message; the premise was wrong.
  `Budget.Allow` is checked against the offer's price times the TTL at
  selection time, so a batch does not make it fire later -- it is a ceiling on
  the rental, not a running meter. A batch that needs longer needs a larger
  `ttl`. Written down because the request looks reasonable and implementing it
  would have meant inventing a mid-run meter nothing else needs.
- **`rejectSecrets` does not scan a prompt, and should not.** The markers are
  substring matches on lowercase -- `secret`, `token`, `password` -- and a
  prompt is arbitrary English. "A token of affection" is a legitimate prompt
  and refusing it would be a false positive on the one field an operator edits
  most. The argv rule is enforced where it belongs instead: ember passes no
  *credential* as a flag, and `/proc/<pid>/cmdline` being world-readable is
  why.

- **One resident server instead of a process per image, and the one place
  ember stops being agnostic about what it is running.** The weights are read
  into VRAM once when `sd.serve` starts and held there for every job after
  it. Measured on an RTX 2080 Ti with sd15 Q8_0 at 256x256 and four steps,
  same prompts and seeds either way:

  | jobs | a process per image | one resident server |
  |---|---|---|
  | 1 | 3.68 s | 9.72 s |
  | 3 | 7.37 s | 10.60 s |
  | 8 | 16.40 s | 12.78 s |
  | 16 | 30.82 s | 16.23 s |

  Marginal cost per job: **1.81 s against 0.43 s**, consistent across 1→8 and
  1→16, against a fixed **~6 s** to start the server. So the crossover is at
  about **five jobs**, and above it the saving grows -- 22% at eight, 47% at
  sixteen, approaching 4.2x per job. All sixteen images are byte-identical
  either way, which is what makes the comparison a comparison.
  **The ~8% this was planned around was arithmetic from the issue's A100
  table, and it was wrong in both directions**: the per-job saving is far
  larger than 8%, and there is a fixed cost that makes a short batch slower.
  The cost of getting it is that ember knows what a sampler is: progress
  streaming is impossible for a program that refuses to read its own payload,
  and a `curl` in a config is the same knowledge maintained by hand.
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
