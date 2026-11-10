# The committed configurations, and what each one measured

**Status: this is the reasoning half of the four `ember*.yaml` files at the
repository root. The files themselves hold configuration and one-line notes
about what a wrong value costs; everything that argues, measures or compares
is here.**

Why the split exists at all: the files this document was extracted from were
1,158 lines of which 329 were configuration — 71% comment. One file was doing
two jobs, which is the same conflation
[#144](https://github.com/Legion112/ember/issues/144) complains about one
level up, between the durable profile (who the provider is, which images,
which store, which ceilings) and the ephemeral job (`sd.jobs`, `tts.jobs`).
Separating the prose does not fix the profile/job conflation — that needs
several model stacks in one file, and `SDConfig.Serve` is a single `[]string`
— but it makes the configuration readable, and it puts the measurements
somewhere a reader can find them without opening a YAML file.

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

## The four files

| file | provider | weights | job shape | costs |
|---|---|---|---|---|
| `ember.example.yaml` | runpod | none | none — a provider smoke test | a minute of a 4090 |
| `ember.sd-server.example.yaml` | runpod | FLUX.2 [dev] Q8_0, on a store | `sd:` — one `sd-server`, a list of jobs | ~$2.49/hr divided over the batch, plus the store |
| `ember.local.example.yaml` | local | the same weights, a Docker volume | `sd:` | nothing |
| `ember.local.sd-server.example.yaml` | local | sd15, baked into `ember:sd` | `sd:` — what `ember serve` needs | nothing |

The count cannot go below four. `{local, rented}` is two profiles, the two
local files differ in which weights they hold — 56 GiB streamed off this
machine's disk against sd15 baked into the image — and the starter names no
jobs at all, which is what makes it a credentials-and-image check rather than
a generation.

**A configuration names one kind of job, and the refusal is
`node.NewSpec`'s.** `sd.serve` and `sd.jobs` come as a pair, `tts.jobs` is the
other kind, and a file naming both is refused by `app.NewPlan` before
anything is rented — a run with two answers to "what does this node generate"
is one where an operator watches the wrong thing execute.

## `ember.sd-server.example.yaml` — FLUX.2 [dev] on a store

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

### The generation parameters

From `examples/server/recipes/flux2-dev.json` at the pinned commit: euler,
`cfg-scale 1`, `guidance 4`, `size_multiple 16`. The recipe defaults to
1024x1024 and 20 steps; 1536 (96 × 16) and 28 steps is what the headroom is
for. `cfg` and `guidance` are different numbers, and conflating them generates
at the wrong one.

**`--offload-to-cpu` is absent from `sd.serve`, and that absence is the
point.** Every example in upstream's `docs/flux2.md` passes it, because
upstream is testing on hardware that has to. Both models stay resident here.

### Licence

FLUX.2 [dev] is non-commercial. The weights and anything generated from them
carry that restriction; this repository's MIT licence does not extend to the
output of this file or of the local one beside it.

### What one resident server buys

The costs paid per rental — waiting for the machine, pulling the image,
reading and verifying 56 GiB off the store — are paid once for the whole
batch. So is the **model load**, which is the expensive one: the weights are
read into VRAM when the server starts and held there for every job after it.

**Measured**, on this model and this card. FLUX.2 [dev] Q8_0 at 2048x2048, 28
steps, on a rented RTX PRO 6000 at $2.49/hr with the weights on a network
volume, four batch sizes, same prompts and seeds. Per image:

| images per rental | 1 | 2 | 4 | 8 |
|---|---|---|---|---|
| `sd.jobs` | $0.2378 | $0.1302 | $0.1249 | $0.1055 |

The server costs about a minute to stand up, so a batch of one pays for a
residency it never uses; from two images on it is the whole saving.

**The price is the same on every node, and that is worth more than the mean.**
Every job after the first took 140.80–140.88 s, on every node — the variance a
node whose page cache could not hold 56 GiB would otherwise add is paid once
at startup rather than once per image. `ember history --memory` records the
node's own cgroup ceiling on every rental, which is what will settle why one
node caches and another does not.

It is worth much more on short generations. At prompt-iteration sizes the
model load is most of a job rather than a tenth of it: **sd15 Q8_0 at 256x256
measured 0.44 s per job against 1.91 s for a fresh process.**

**Still unmeasured: previews.** The figures above ran `preview: none`, so they
price the resident model and nothing else. The progress stream — the reason
ember learned this API at all — has not been priced on real hardware.

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
11 GiB card, so `sd-server` keeps them in system RAM and streams them into
VRAM per layer, and roughly **35 GB crosses PCIe on each of 28 steps**. A
rented A100 does the same image in minutes.

The point is that it costs nothing. Most iterations of a prompt are asking "is
the composition right", and that question does not need an A100. Get the prompt
and the seed right here for free, then render once on a rented card with
`ember.sd-server.example.yaml` — same assets, same digests, same jobs, one
line different.

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
written out under the rented configuration above.

## `ember.local.sd-server.example.yaml` — the web UI, for nothing

`ember serve` needs an `sd:` block, because the serve argv names the weights
and a request arriving over HTTP may not. Neither other `sd:` example is
usable for that here: the local one streams 56 GiB of FLUX.2 off this disk,
and the rented one wants FLUX.2 from RunPod. This is the one that costs
nothing.

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
