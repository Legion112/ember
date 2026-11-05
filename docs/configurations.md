# The committed configurations, and what each one measured

**Status: this is the reasoning half of the five `ember*.yaml` files at the
repository root, plus the two shapes that are written down and cannot be run
yet. The files themselves hold configuration and one-line notes about what a
wrong value costs; everything that argues, measures or compares is here.**

Why the split exists at all: the seven files this document was extracted from
were 1,158 lines of which 329 were configuration — 71% comment, and the
code-model one was 85% prose. One file was doing two jobs, which is the same
conflation [#144](https://github.com/Legion112/ember/issues/144) complains
about one level up, between the durable profile (who the provider is, which
images, which store, which ceilings) and the ephemeral job (`cmd`, `cmds`,
`sd.jobs`). Separating the prose does not fix the profile/job conflation —
that needs several model stacks in one file, and `SDConfig.Serve` is a single
`[]string` — but it makes the configuration readable, and it puts the
measurements somewhere a reader can find them without opening a YAML file.

**Half of that sentence has stopped being true and is corrected here rather
than left to be contradicted by the code.** `workloads:`
([#200](https://github.com/Legion112/ember/issues/200)) is a list of declared
entries, each with its own `image:`, `images:` and driver block — so two `sd`
entries in one file ARE two model stacks, with two `sd.serve` argvs, and
`SDConfig.Serve` being a single `[]string` is no longer the bound. What is
still unfixed is the ephemeral half: a declared entry's `sd.jobs` and
`tts.jobs` are the job, so one file still describes one batch per workload,
and `cmd`/`cmds` have no entry form at all. The five files below declare no
workloads and nothing about them changed.

**The rule, so the next reader does not have to guess it.** A comment in a
committed configuration says what a wrong value costs, in a line or two.
Anything that argues, measures or compares belongs here.

## The five files

| file | provider | weights | job shape | costs |
|---|---|---|---|---|
| `ember.example.yaml` | runpod | none | none — holds the node open | a 4090 for the TTL |
| `ember.flux2.yaml` | runpod | FLUX.2 [dev] Q8_0, on a store | `cmd:` — one `sd-cli` per run | ~$1.79/hr plus the store |
| `ember.sd-server.example.yaml` | runpod | the same weights, same store | `sd:` — one resident `sd-server`, a list of jobs | the same, divided over the batch |
| `ember.local.example.yaml` | local | the same weights, a Docker volume | `cmd:` | nothing |
| `ember.local.sd-server.example.yaml` | local | sd15, baked into `ember:sd` | `sd:` — what `ember serve` needs | nothing |

The count cannot go below five without code. `{local, rented} × {cmd, sd}` is
four distinct shapes, a spec cannot be built from two job shapes in one file,
and the starter names no job at all because the interactive shape is the one
that holds the node open with its ports forwarded.

**Where that refusal lives, because the first wording of this paragraph named
the wrong function and the YAML in the wrong direction.** It read "the config
language cannot express two job shapes in one file (`ParseSD` refuses `cmd`,
`cmds` and `sd.serve` together)", and both halves are false. `ParseSD` has
exactly one cross-field check — `sd.jobs` and `sd.serve` must come as a pair
— and never looks at `Cmd` or `Cmds` at all. The language *does* express it: a
file holding both `cmd:` and `sd:` loads through `config.Load` and passes the
guard below. What refuses is `node.NewSpec`, through `namedWorkloads`, with
*"cmd, cmds and sd.serve are set; use one"* — and the reason written beside it
is better than the one invented here: a run with both would start a server,
hold the weights in VRAM, and then execute a separate `sd-cli` that loads them
a second time, paying for the resident copy and not using it. The conclusion
is untouched, which is exactly what makes it worth recording: a wrong
mechanism under a correct argument is the `ember ls --all` class, where the
argument persuades and the thing it cites does not do the job.

The two aspirational shapes are at the bottom of this document rather than at
the root, because neither can be run: one's image is a sentence and the
other's asset digest is a placeholder, both deliberately. A configuration that
refuses by construction is a worked example.

## `ember.flux2.yaml` — FLUX.2 [dev] on a store

### What is on the store, and why it could not be anywhere else

56.16 GiB of weights, of which **23.33 GiB is a 24-billion-parameter LLM used
only to read the prompt**. The diffusion file alone is **32.60 GiB** and a
registry refuses a layer above about 10 GiB, so nothing here could be baked
into an image even if the layer digest were worth having — and nothing here
fits on a consumer card.

### Why `ember store sync` exists, in one subtraction

The weights are not uploaded from home. At the **67 Mbps** upstream measured
in `README.md`, 56.16 GiB is **one hour and fifty-two minutes** of pushing —
after about an hour of pulling it down to the house first. `ember store sync`
rents a CPU machine at **six cents an hour** and has the datacentre fetch them
instead. That is the whole reason the command exists.

### The standing charge, which is the one thing nothing destroys

The store bills whether or not anything is attached, with no deadline and no
node: for the 70 GiB this file asks for, **$0.0067 an hour, $0.16 a day,
$4.90 a month**. `ember ls` is what prints it. `ember store rm` is the only
thing that stops it, and it has to be typed.

That is also what makes the shape worth it: the 56.16 GiB is fetched once and
then re-read by every later run in about a minute, so prompt iteration is
`ember up` again and not `store sync` again, and each `ember up` destroys its
GPU on the way out — `provision.WithNode`'s deferred destroy does that even if
the workload fails or the laptop is shut.

### Why the datacentre is in the file

A store pins every node that mounts it to one datacentre, so the datacentre is
chosen for what is available *in* it rather than for the store. **EUR-IS-1 is
the only one of the ten that serve the S3 API with three cards at 80 GiB or
more**, which is the only hedge there is against a capacity refusal.

### Why no `gpu:` and `min_vram_gib: 80` instead

The requirement is VRAM, not a model: 56.16 GiB resident plus activations at
1536x1536 needs 80 GiB, and the budget ceiling then picks the cheapest card
that has it — in EUR-IS-1, the **A100 80GB at $1.79**.

This briefly had `gpu: rtxpro6000` hard-coded, because the published image was
built with `CUDA_ARCHS="86;89"` and the A100 is `sm_80`: PTX JITs forward,
never back, so the cheapest qualifying card was the one card that could not run
the image. It would have failed with *no kernel image is available for
execution on the device* after paying for the pull and the 56 GiB store read.
The filter can say what it means again, and for a better reason than a wider
fatbin: the `images:` map covers every architecture ember can select, so the
cheapest qualifying card is always one there is an image for — or the run
refuses before renting it.

### The scalar `image:` beside the `images:` map

A CPU box has no compute capability, so `images:` cannot choose for it and is
deliberately not consulted. Without the scalar, `ember store sync` on this
very file refuses with *there is no architecture to choose an image by* — the
config could not populate the store it depends on, which is a bug this
repository shipped and `TestExampleConfigs_CanSyncTheirOwnStore` is the
regression test for.

It is the **node** image and not a workload one: that machine only runs
`embernode fetch`, and pulling 2.9 GB of CUDA to download files would be
billed for nothing. Pinned to a node image whose `embernode` *knows* `fetch`
— the digest an earlier `ember.example.yaml` carried predates the subcommand,
so it reads `embernode fetch …` as "be a node" and dies with *EMBER_HOST_KEY
is empty*, because sshd builds a fresh environment per session. **Measured by
running it: the sync failed in 11 seconds and cost two hundredths of a cent to
learn.**

### The probe, which this configuration already paid to learn

An A100-SXM4-80GB came up, the weights verified, and `sd-cli` reported:

```
ggml_cuda_init: failed to initialize CUDA: system not yet initialized
[W] auto-fit: no GPU devices; using the default backend
[V] Using CPU backend
```

and then started a 32-billion-parameter model on 128 CPU threads. The text
encoder alone took **122 seconds where a working card takes about one**.
Nothing in ember thought anything was wrong, because the workload was running.
**That cost $0.37 and produced nothing** — see `internal/gpucheck`.

A pipeline rather than `nvidia-smi`, because `nvidia-smi` can list a card on a
host whose NVSwitch fabric is not up, which is the exact failure above.
`--list-devices` makes `sd-cli` enumerate ggml backends, so it answers the only
question that matters: will the program we are about to run find a GPU.

**Two questions, because one probe used to answer only one of them.** It does
*not* catch a wrong-architecture image. Measured: with an `sm_75` image on an
`sm_89` card, `--list-devices` exits zero and names the card, because listing
devices asks the driver what hardware exists and launches no kernel. The probe
passed, the workload started, and it aborted a second into the first
generation with *no kernel image is available for execution on the device* —
after the pull and a 1.6 GB weight fetch, all billed. So the probe compares the
image to the card first, reading `/etc/ember-cuda-arch`, which the published
images write for exactly this; an empty or missing file **fails** the test
rather than passing it, because a comparison against nothing would admit
everything.

`images:` already refuses a card it has no entry for, before renting. The probe
catches what a map cannot see: the right digest filed under the wrong key, or a
scalar `image:` pinned by hand while debugging.

**`gpu_probe_timeout: 2m` and not one attempt**, because the driver said "not
yet": an SXM board waits for `nvidia-fabricmanager` on the host, and probing at
the earliest possible moment turns a card that needed forty seconds into a
refusal.

### Why the digests cost nothing to obtain

They are the LFS object ids from the Hugging Face tree API, so none of these
files has been downloaded to get them. If one is wrong, the mismatch surfaces
during `store sync` on a six-cent CPU machine rather than on a card that bills
$1.79 an hour.

`black-forest-labs/FLUX.2-dev/ae.safetensors` is gated and answers 401 without
a licence click. What the file names instead is the ungated encoder/decoder
from `FLUX.2-small-decoder`, which upstream's own `docs/flux2.md` names as
the alternative and the `flux2-dev` recipe accepts under
`required_any: [[vae, tae]]`.

### The generation parameters, and the one absence that is the point

From `examples/server/recipes/flux2-dev.json` at the pinned commit: euler,
`cfg-scale 1`, `guidance 4`, `size_multiple 16`. The recipe defaults to
1024x1024 and 20 steps; 1536 (96 × 16) and 28 steps is what the headroom is
for. `cfg` and `guidance` are different numbers, and conflating them generates
at the wrong one.

**`--offload-to-cpu` is absent, and that absence is the point.** Every example
in upstream's `docs/flux2.md` passes it, because upstream is testing on
hardware that has to. Both models stay resident here.

### Licence

FLUX.2 [dev] is non-commercial. The weights and anything generated from them
carry that restriction; this repository's MIT licence does not extend to the
output of that file or of the served one beside it.

## `ember.sd-server.example.yaml` — the same weights, driven the other way

`ember.flux2.yaml` runs one `sd-cli` per `ember up`. This one starts one
`sd-server` and sends it a list of jobs. The durable half of the two files is
identical — the same `images:` map, the same scalar `image:`, the same store,
assets, `verify`, probe and ceilings — and that duplication is exactly what
#144 is about and what #144 cannot fix without code.

### What it buys, and what it does not

`cmds` already removed the costs paid per rental: waiting for the machine,
pulling the image, reading and verifying 56 GiB off the store. What it cannot
remove is the **model load**, because every entry in `cmds` is a fresh process
that reads the weights into VRAM again.

**Measured**, on this model and this card. FLUX.2 [dev] Q8_0 at 2048x2048, 28
steps, on a rented RTX PRO 6000 at $2.49/hr with the weights on a network
volume: eight runs, four batch sizes, both paths, same prompts and seeds, and
all eight images byte-identical between the two. Per image:

| images per rental | 1 | 2 | 4 | 8 |
|---|---|---|---|---|
| `cmds` | $0.1936 | $0.1895 | $0.1696 | $0.1075 |
| `sd.jobs` | $0.2378 | $0.1302 | $0.1249 | $0.1055 |

The crossover is between one and two images — the server costs about a minute
to stand up and there is nothing to amortise that over at n = 1.

**The mean is not the point.** `sd.jobs` is flat: every job after the first
took 140.80–140.88 s, on every node. `cmds` was not. On a node that kept the
56 GiB in page cache a step took 145.3 s; on one that did not, 226.6 s, because
each step re-read the whole model off the store. That is **81 s and $0.056 per
image of variance** between runs of the same configuration against the same
card in the same datacentre, and what decides it is not known — see
`AGENTS.md`, and `ember history --memory`, which records the node's own cgroup
ceiling on every rental so the next FLUX.2 run settles it. So the argument for
the served file is less that it is cheaper on average than that **its price is
the same on every node**.

It is worth much more the other way round. On short generations — prompt
iteration at 512x512, four steps — the reload is most of the step rather than a
tenth of it, and that is the case this shape exists for: **sd15 Q8_0 at
256x256 measured 0.44 s per job against 1.91 s for a fresh `sd-cli`.**

**Still unmeasured: previews.** Both arms above ran `preview: none`, so these
numbers price the resident model and nothing else. The progress stream — the
reason ember learned this API at all — has not been priced on real hardware.
The same warning applies to video, below.

### The two timeouts, which are sized from different things

`ttl: 1h` is four images at roughly a hundred seconds of sampling each, plus
about three minutes of setup paid once. The ceiling is what stops a batch that
wedged from billing until somebody notices, so it is set from the job rather
than left at a default — and a batch is longer than a single run, which is the
one thing about TTL that changes on this path.

`sd.ready_timeout: 10m` is longer than the GPU probe's two minutes because the
thing being waited for is bigger: 56 GiB read off a network volume, measured at
about a gigabyte a second, before anything is resident. ember prints what it
timed itself from starting the process to `models/status` reporting ready,
which is the figure this whole shape exists to pay once — and not
`load_elapsed_seconds`, which sd-server reported as **551 ms** for those same
56 GiB.

### Previews, written out so they can be read

Upstream defaults `preview_interval` to 1 — a frame every single step — and
`tae` and `vae` run a decode to produce each frame, so the upstream default is
the expensive one on a card billing by the second. `proj` is a projection
rather than a decode. Use `none` for a batch nobody is watching. A negative
interval is refused rather than clamped, because upstream clamps a
non-positive value to 1: writing `-1` to mean "off" would buy the most
expensive setting there is.

### `extra:`

Everything the typed fields do not name is reachable through `extra`,
including nested objects, so no part of the API is out of reach from a config.
A key ember already set is refused rather than resolved. `extra` is reachable
from a config and **not** from the web UI — the field does not exist on the
wire type.

## `ember.local.example.yaml` — the same weights, on the card in this machine

**The point is not that this is fast.** It is not: the weights do not fit in an
11 GiB card, so `sd-cli` keeps them in system RAM and streams them into VRAM
per layer, and roughly **35 GB crosses PCIe on each of 28 steps**. A rented
A100 does the same image in minutes.

The point is that it costs nothing. Most iterations of a prompt are asking "is
the composition right", and that question does not need an A100. Get the prompt
and the seed right here for free, then render once on a rented card with
`ember.flux2.yaml` — same assets, same digests, same command, one line
different.

`--offload-to-cpu` is what makes it possible at all: **56 GiB of weights in a
125 GiB machine behind an 11 GiB card.**

**1024 and not 1536.** The DiT's compute buffer alone was **4412 MiB at
1536x1536 on a card with 80 GiB to spare**; on 11 GiB, beside a streaming
layer, it is the first thing that will not fit. Raise it once a run has proved
it fits.

**`verify: present` and not `always`.** The rented config re-hashes 56 GiB on
every run because a network volume is writable by every node that mounts it — a
cross-run channel worth a minute of an A100. A Docker volume on this machine is
not that: nothing else writes it, and re-reading 56 GiB off this disk before
every local iteration would make the free path the slow one to start.

**The image is built for the card in this machine.** `make
workload-sd-cuda-local` reads the compute capability from `nvidia-smi` and
builds for exactly that, under the ordinary per-architecture tag — so adjust
the `sm_` number in the file if the card here is not an `sm_75`. There used to
be a separate `ember:sd-cuda-local` tag, because the published set excluded
this machine's card; it does not any more, so a local build and a published one
are the same artifact under the same naming, and two names for one thing is how
they drift.

The probe matters *more* here than on the rented path, because the published
image genuinely cannot run on this card: it is what turns that into a refusal
instead of a silent CPU fallback. The two questions it asks are the ones
written out under `ember.flux2.yaml` above.

## `ember.local.sd-server.example.yaml` — the web UI, for nothing

`ember serve` needs an `sd:` block, because the serve argv names the weights
and a request arriving over HTTP may not. None of the other committed examples
is usable for that here: the local `cmd:` one is not a served shape, and the
rented served one wants FLUX.2 from RunPod. This is the one that costs nothing.

```bash
make docker-build
make workload-sd MODEL=/path/to/v1-5-pruned_bf16.gguf
make web
ember serve --config ember.local.sd-server.example.yaml
```

**What it proves, which is why it is worth having beyond convenience:** the
whole path a rented card takes — provision, SSH, host-key pin, resident
`sd-server`, progress stream, images home, destroy — running against a real
sshd in a real container, with a real generation at the end of it, on a laptop.
The only thing it does not exercise is the provider's API.

`--model` and **not** `--diffusion-model`: `ember:sd` carries a whole
checkpoint in one file. `--diffusion-model` is for a standalone DiT, which is
what FLUX.2 needs, and the flag that works for one silently cannot load the
other.

`ready_timeout: 2m` rather than the ten-minute default, which is right for
56 GiB off a network volume and far too patient for a laptop CPU loading sd15:
a server that died wants noticing now, not at the deadline.

`preview_interval: 2` rather than ember's usual 5, because a generation here is
short enough that 5 would produce one frame and nothing to watch.

The ceilings are nominal — the local provider is free — and are written out so
the UI has something to display and so the shape is copyable to a
configuration that is not free.

`sd.jobs` there is a placeholder: `ember serve` replaces it with whatever the
browser queues. It cannot be *empty*, because `ParseSD` refuses a server with
no jobs — that configuration rents a card, loads the weights and generates
nothing. It is also what `ember up --config
ember.local.sd-server.example.yaml` would run, which is a useful way to check
the configuration without a browser.

## Two shapes that are written down and cannot be run

Neither of these is a file at the repository root, and that is the point: one's
`image:` is a sentence and the other's asset digest is a placeholder, both
deliberately, so both refuse before anything is rented. A configuration that
refuses by construction is a worked example, and a worked example belongs in
prose.

What that costs is stated rather than left to be noticed: a fenced block is
loaded by `internal/config`'s `TestExampleConfigs_LoadAndParse`, which reads
every `yaml` fence under `docs/` for exactly this reason, so `KnownFields`
still refuses a misspelled key in either of them — but
`TestExampleConfigs_CanSyncTheirOwnStore` is now about the three root files
that can really sync, which is the honest subject: `store sync` on either
block below fails anyway, one at `ParseAssets` over the placeholder digest and
the other at `ValidateImage` over the untagged sentence.

### Video from a model on a store

This is the configuration the store exists for. Wan 2.2's experts are 14 GB
each and could not be baked into an image if anybody wanted to — the registry
refuses a layer above about 10 GB — so the weights live on a volume, pushed
once, and the node verifies them before the GPU is asked to do anything.

**Nothing here has been run end to end.** It is written from the measured API
shapes and the `sd-cli` flags in this repository's own image, and the first
real run is a calibration rather than a lab: it costs money, and what it
produces is a number and a fixture, not a pass or a fail.

33 frames is one of the counts that maps to a clean latent length — 33, 77 and
81 — and at 16 fps it is about two seconds. A minute of video is twelve clips
of 81 chained on the last frame of the one before, which is a pipeline rather
than a command and does not belong in `cmd:`. The reasoning, the measurement
that corrected the original premise, and the list of numbers that cannot be
estimated before one real clip exists are in
[Long video is a pipeline](long-video-is-a-pipeline.md).

`store push ./wan2.1-t2v-1.3b.gguf` prints the sha256 to paste over the
placeholder. The digest is not optional: a store is writable by every node that
mounts it, and this is what stands in for a read-only mount.

`outputs:` is `/workspace` and never the store — a render written onto a
mounted volume bills by the month for as long as the store exists, and
`node.MountAt` refuses a store mounted there for the same reason.

```yaml
provider: runpod

images:
  sm_75: ghcr.io/legion112/ember-sd:sd-cuda-sm75@sha256:2da9f94d608d2f757dbf2f38f309ce73a6a41b872257c88eb9c2aadd822e20c9
  sm_80: ghcr.io/legion112/ember-sd:sd-cuda-sm80@sha256:468cbde2fdbec8d83e403b32cda8aa4db214c1fd555ddab182225cab7d442f20
  sm_86: ghcr.io/legion112/ember-sd:sd-cuda-sm86@sha256:e7d1e42c94b29146425c134d4d91da646854b8325e5229b790739ecf9f1e8709
  sm_89: ghcr.io/legion112/ember-sd:sd-cuda-sm89@sha256:e4654d841c966b042493d81757feb397817a7e3ff5da8e2bdd397885e901f0c5
  sm_90: ghcr.io/legion112/ember-sd:sd-cuda-sm90@sha256:01be5129dbfc61936bb4c4dff5d3a571838b9e1257dc736427c79324a9c3b69e
  sm_100: ghcr.io/legion112/ember-sd:sd-cuda-sm100@sha256:ef93ef0ee1e6097473a2b36af9ae4066a3ea98912cfde38d4768d8ab60c7883f
  sm_103: ghcr.io/legion112/ember-sd:sd-cuda-sm103@sha256:8ff5fd43523f999a116afe5821bef067290e8d2c2416a13b24a1fef9f4899c88
  sm_120: ghcr.io/legion112/ember-sd:sd-cuda-sm120@sha256:e2447c2d0e884da44c1d33b09abb4366354e606903f2f1dcd8fe2bd835d87f7f

# The CPU machine `ember store sync` rents: node image, no CUDA to pull.
image: ghcr.io/legion112/ember-node@sha256:70de4503dfeee1ecc09347fb3a8998d293b36ddf67691dd3af925950e787aa2d

# Video is minutes per clip, not the second an image takes, so the ceiling is
# set from the job rather than left at a default.
ttl: 2h
gpu: rtx4090
max_hourly_usd: "3.00"
max_run_usd: "8.00"

# EU-RO-1 is the widest of the ten datacentres that both take a volume and
# serve the S3 API. The store pins every node that mounts it to its own.
store:
  name: models
  size_gib: 50
  region: EU-RO-1
  mount: /models

assets:
  - url: https://huggingface.co/EXAMPLE/wan2.1-t2v-1.3b-gguf/resolve/main/wan2.1-t2v-1.3b-Q8_0.gguf
    sha256: REPLACE_WITH_THE_DIGEST_PUSH_PRINTS
    as: wan2.1-t2v-1.3b.gguf

verify: always

cmd:
  - sd-cli
  - -M
  - vid_gen
  - -m
  - /models/wan2.1-t2v-1.3b.gguf
  - -p
  - "a paper boat on a slow river, morning light"
  - --video-frames
  - "33"
  - -W
  - "832"
  - -H
  - "480"
  - --steps
  - "20"
  - --seed
  - "1312"
  - -o
  - /workspace/out/boat.mp4

outputs:
  - /workspace/out:renders
```

### An open code model, over one forward

A coding-agent endpoint is `cmd` plus `ports`, and it needed no new code: the
forwards run *alongside* the workload rather than instead of it, so a server as
the workload and a forward beside it has always been a supported shape.
`llama-server` does not exit, so *a node rented to run something stops when it
has run it* never fires and the node holds until Ctrl-C, `--hold`, or the TTL.
The agent runs here and talks to `127.0.0.1:8080`, which is ember's whole
posture already: nothing inbound, everything over SSH.

**One piece of this does not exist, and it is the image.** An ember workload
image has to carry `embernode` and sshd — ember runs the workload as `ssh host
command`, so an image that is only a server has nothing to connect to — and
this repository builds no such image for an LLM server. So `image:` below is
not a value, it is a sentence, and ember refuses it by the rule that already
exists:

```
ember: image "EMBER-HAS-NO-LLM-WORKLOAD-IMAGE-SEE-THIS-FILES-HEADER" has no
tag: pin it to a tag or a digest, or two runs will not have measured the same
software
```

That is deliberate, and it is #18's lesson applied before it could be
repeated: the video block above carries
`REPLACE_WITH_THE_DIGEST_PUSH_PRINTS` where a digest goes, which reads as a
configuration with a typo in it. A placeholder has to be unmistakable for a
value, and the loudest version of that is one whose refusal quotes itself. It
is a scalar and not an `images:` map although a CUDA image is
per-architecture everywhere else here, because a map is validated entry by
entry and a committed map of fiction would fail this repository's own test
over every other example at the same time.

**Everything else here is real and runnable today.** `ember store create`,
`ember store push`, `ember store ls` and `ember store verify` need no node and
no image, so the store half can be done now.

**The weights, and a digest nobody guessed.**
Qwen3-Coder-30B-A3B-Instruct at Q4_K_M is **18,556,689,568 bytes, 17.28 GiB**
— 30B total with about 3B active per token, which is why a card that holds it
serves it quickly rather than merely holding it. Hugging Face stores LFS
objects under their own sha256 and two endpoints report it: the tree API's
`lfs.oid`, and the `X-Linked-ETag` header on a HEAD of the resolve URL. Both
answered `fadc3e5f…` for this file, which is **two independent confirmations
of one number for the cost of two requests and no bytes** — the calibration
habit the RunPod volume API set.

```
curl -sI https://huggingface.co/unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF/\
  resolve/main/Qwen3-Coder-30B-A3B-Instruct-Q4_K_M.gguf | grep -i linked-etag
```

Getting it onto the store: `ember store push` uploads from here, bounded by
this link — **17.28 GiB at 67 Mbps is about thirty-seven minutes**. `ember
store sync` rents the cheapest CPU box at ~$0.06/hour and has it download
instead, which is minutes and a fraction of a cent — but it needs an image, and
the one above is fiction, so until that exists this is a push.

**`verify: always`, and 17 GiB is why it is affordable.** At the 868 MiB/s
measured off a volume that is about **twenty seconds** of a card you are about
to hold for eight hours. It matters more here than anywhere else in this
repository: a substituted image model produces a picture that looks wrong; a
substituted code model produces code, and you paste it.

**A hard stop, not a session length, and this is the caveat that matters most.**
The deadline is written in three places — the provider's label, the local
journal record, and the node's own self-destruct timer — and the third is armed
from `EMBER_TTL_SECONDS`, read once by `cmd/embernode` at container start.
Nothing on the node listens for a change, so a running node's deadline is not
re-armable: at 8h the endpoint the editor is pointed at disappears, mid-sentence
if that is where you are, and the remedy is a new node, a new 17 GiB read off
the store and a new forward. The ceiling above it is `node.MaxTTL`, a day.

**And the ceiling that is not a meter.** `max_run_usd` is checked against the
offer's hourly price times the TTL, at selection time, before anything is
created — so it cannot bite halfway through a session, and it has to be at
least `max_hourly_usd` times `ttl` or every offer is refused. **An eight-hour
working day at $1.00/hour is $8.00, and twenty of them is $160 a month** for a
card that is idle most of those eight hours. That number is the point of this
configuration as much as the endpoint is: ember exists to make the rented path
convenient enough that buying hardware becomes a decision backed by
measurements, and this is the workload where renting is least favourable.

`min_vram_gib: 40` and no `gpu:` pin: 17.28 GiB of weights plus a 32k KV cache
does not leave working room on a 24 GiB card, and a server that runs out of
VRAM forty minutes into a session is worse than one that was never rented.
40 GiB is the first size that is not a judgement call. Any card with the memory
will serve this model at a usable rate, so pinning one would refuse the
cheapest offer that fits for no reason; `ember offers --min-vram-gib 40` prints
what is rentable at the ceiling.

The probe has no second half asking the server itself, the way the sd configs
run `sd-cli --list-devices`: the equivalent question for a server is answered
by its own startup log, which streams to the terminal, and a probe cannot read
it.

**No API key on the `cmd` line.** `/proc/<pid>/cmdline` is world-readable, and
on a rented machine that is every process on it. If the server you pin offers a
flag that reads a key from a file, that is the one to use, with the file on the
store; the flag that takes the key itself is the one that leaks it. What the
endpoint is exposed to instead is in `README.md` under *what this does not
protect you from*. The flag spellings below are llama.cpp's, which is a
property of the image — the thing that does not exist yet — so check them
against the build you end up pinning rather than against this document.

```yaml
provider: runpod

# Not a value. See above. One line to change, and the rest is what you keep.
image: EMBER-HAS-NO-LLM-WORKLOAD-IMAGE-SEE-THIS-FILES-HEADER

ttl: 8h
max_hourly_usd: "1.00"
max_run_usd: "8.00"
min_vram_gib: 40

gpu_probe:
  - sh
  - -c
  - "arch=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader | head -1 | tr -d ' .'); img=$(cat /etc/ember-cuda-arch 2>/dev/null); echo \"card is sm_$arch, image is sm_$img\"; test -n \"$img\" && test \"$arch\" = \"$img\""
gpu_probe_timeout: 1m

store:
  name: models
  size_gib: 25
  region: EU-RO-1
  mount: /models

assets:
  - url: https://huggingface.co/unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF/resolve/main/Qwen3-Coder-30B-A3B-Instruct-Q4_K_M.gguf
    sha256: fadc3e5f8d42bf7e894a785b05082e47daee4df26680389817e2093056f088ad
    as: qwen3-coder-30b-a3b-q4_k_m.gguf

verify: always

cmd:
  - llama-server
  - --model
  - /models/qwen3-coder-30b-a3b-q4_k_m.gguf
  - --host
  - 127.0.0.1
  - --port
  - "8080"
  # Every layer on the card. The probe is what makes an unusable card a
  # failure rather than a silent fallback.
  - --n-gpu-layers
  - "999"
  # 32k, not the model's maximum. The KV cache is VRAM, an agent fills a
  # context with repository rather than conversation, and a window that does
  # not fit is discovered by the server at the moment it stops answering.
  - --ctx-size
  - "32768"
  # Tool calls come out of the model's own chat template. An agent whose tool
  # calls arrive as prose reads as a model that cannot use tools.
  - --jinja
  - --alias
  - qwen3-coder-30b

ports:
  - llm:8080

# No outputs: an interactive session's product is in the editor and on this
# machine already, and the node's own log comes home regardless. An empty
# fetch would be a tar over a path nothing wrote.
```
