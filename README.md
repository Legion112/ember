<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img src="assets/logo-light.svg" alt="ember" width="320">
  </picture>
</p>

<p align="center">
  <em>Rent a GPU, run an open model on it, take the results, destroy the node.</em>
</p>

<p align="center">
  <a href="https://github.com/Legion112/ember/actions/workflows/ci.yml"><img src="https://github.com/Legion112/ember/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/go-1.27-00ADD8" alt="Go 1.27">
  <img src="https://img.shields.io/badge/licence-MIT-blue" alt="MIT">
</p>

---

An ember, not a furnace: nodes are meant to live for minutes, and nothing ember
creates is reachable from the internet.

```yaml
# ember.yaml
provider: runpod
gpu: rtx4090
image: ghcr.io/you/ember-sd:v1       # sshd + embernode + the model
ttl: 30m
cmd: [sd-cli, -m, /opt/models/sd15.gguf, -p, "a lovely orange cat", -o, /workspace/out/cat.png]
outputs: [/workspace/out:renders]
```

That rents a node, waits for it, runs the command with its output streaming to
your terminal, fetches `/workspace/out` back over SSH, and destroys the node —
including when you interrupt it. Leave `cmd` out and it forwards the ports
instead and holds, which is the interactive shape.

That rents a node, waits for it, forwards `8188` to `127.0.0.1:8188` over SSH,
waits for you to finish, fetches the outputs and destroys the node — including
when you interrupt it.

Every run reports what it cost, and `ember ls` reports what each node has cost
so far — the figure that turns "an orphaned H100 burns about 175 ₽/hour in
silence" from a sentence in a README into something to act on this afternoon.
The estimate says the provider's minimum billing unit out loud when it applies,
because "it ran for four seconds and cost a minute" is surprising exactly once.

Outputs come back as a tar stream over the same SSH connection and are
extracted through an `os.Root`, so nothing a rented machine puts in an archive
can land outside the run's own directory. Symlinks, hard links and device nodes
are refused rather than recreated, and the names that were refused are
reported — a render has no business being a block device, and a silent skip
would look like a fetch that worked.

## Why

Renting is roughly two orders of magnitude cheaper than owning while you are
still finding out what you need. An RTX PRO 6000 costs about 2.23M ₽ to buy and
about 134 ₽/hour to rent: at four hours a day that is an eleven-year payback,
and the card will be obsolete long before. ember exists to make the rented path
convenient enough that buying hardware becomes a decision backed by
measurements instead of by spec sheets.

## Status

`ember up --provider local` works end to end today: it rents a container on
your own Docker daemon, authenticates against a host key ember minted and
injected, forwards the ports you asked for to loopback, holds them, and
destroys the node — including after Ctrl-C.

```
make docker-build
ember up --provider local --image ember:lab --port demo:22 --ttl 30m
```

ember runs **one container that is both the node and the workload**, so the
image has to carry sshd and `embernode` alongside whatever you came to run.
`node.Dockerfile` builds that base: locally as `ember:lab` for the labs, and
published by CI to `ghcr.io/legion112/ember-node` for a rented pod. The labs
and a rented pod run the same bytes, which is the point — a lab that tested a
different image would prove nothing about what the provider pulls.

| | |
|---|---|
| `internal/node` | domain core — nodes, offers, specs, integer money, states |
| `internal/loopback` | the only listener in the program, provably loopback |
| `internal/hostkey` | sealed trust — `InsecureIgnoreHostKey` cannot be typed in |
| `internal/exposure` | what may face the internet — a public port cannot be spelled |
| `internal/keys` | the operator's SSH identity, agent fallback |
| `internal/tunnel` + `sshdial` | the forward and the one place a host-key callback is set |
| `internal/provider` + `local` | provider seam and the free Docker-backed one |
| `internal/provider/runpod` | RunPod: wire types, pure mapping, thin client |
| `internal/httpx` | one place for timeouts, body closing and readable API errors |
| `internal/provision` | rent, wait, hold, destroy — with the money discipline |
| `internal/statestore` | the journal that makes an orphan findable |
| `internal/sshx` + `sshexec` | run one command on a node, stream its output |
| `internal/transfer` | bring the outputs home — tar over SSH, into an `os.Root` |
| `internal/workload` | run the command the node was rented for, streaming |
| `internal/reconcile` | pure scan over providers and records — what `ls` and `reap` act on |
| `internal/session`, `internal/config`, `internal/cli` | wiring |

Every verb is implemented: `up`, `down`, `ls`, `reap`, `offers`, `version`.
`ls` asks every provider rather than trusting the local records, and exits 2
when anything needs attention — including a provider it could not reach.

The RunPod provider is written and tested against committed fixtures and an
`httptest` server, but has never spoken to the live API — see
`internal/provider/runpod/testdata/README.md` for exactly what that does and
does not prove.

Next: a first real-money smoke test against RunPod, then Vast.ai. See
`AGENTS.md` for the rules all of it is written against.

## Two properties that outrank features

**Nothing of yours is reachable from the internet.** Every forwarded port binds
to loopback on your machine; the only thing ember asks a provider to expose is
sshd,
key-only, verified against a host key ember generated and injected itself.
`net.Listen` appears in exactly one package and a test fails the build if that
changes. A public port cannot be *spelled*: the type carrying one has no
exported field, so `exposure.SSHPort()` is the only thing in the program that
can produce one. After create, ember reads back what the provider says it
published and destroys the node if that is anything but sshd — on RunPod a port
declared `8188/http` is served at `{id}-8188.proxy.runpod.net` with no
authentication at all — RunPod's own documentation says so: *"the service
becomes publicly accessible. While the Pod ID provides some obscurity,
implement proper authentication in your application."*

That check earns its keep. On ember's first real provision it refused the node
and destroyed it, because RunPod maps its web terminal port to every pod and
gives no way to turn it off. A provider may now *declare* such a port, and
only such a port; ember's node image listens on nothing but 22, which a lab
asserts, and a workload asking for the proxied port is refused.

**A leaked node costs real money.** An orphaned H100 burns about 175 ₽/hour in
silence. Every node carries a deadline enforced in several independent places,
destroy runs on a context that survives Ctrl-C, and `ember ls` exits non-zero
when any provider could not be reached — because "I could not check" must
never look like "nothing is running". Each record also names the process
holding its node, so a run killed with `SIGKILL` is reported as billing with
nobody watching instead of printing `ok`.

**A destroy that succeeded can still leave something behind.** On the local
provider a node is two objects — the container and the bridge network it sits
on — and `Destroy` used to find the network by reading the run id off the
container, which does not work when something else removed the container
first. Nothing bills for a bridge network, so the leak is silent until Docker
has subnetted its whole address pool and every local run fails at once with an
error naming Docker rather than ember. The run id now travels into `Destroy`
from the caller, which already had it, and `ember ls` reports any network whose
node is gone as a `leftover` while `ember reap` removes it:

```
$ ember ls
leftover       local/ember-0123456789abcdef  network left behind by run 0123456789abcdef, whose
                                             node is gone; nothing bills for it and nothing else
                                             will remove it. Remove it with: ember reap
$ ember reap
destroyed 0 of 0 expired node(s)
removed 1 of 1 leftover(s)
```

A leftover counts towards the exit-2 attention total, unlike a store: ember
cannot tell a store that is finished with from one being kept, and it can tell
that a network whose node is gone is garbage. One inside its deadline is not
reported, because `Create` makes the network before the container and a live
run would otherwise look identical to wreckage for that moment.

## Workload images

ember runs **one container that is both the node and the workload**, so the
image carries sshd and `embernode` alongside whatever you came to run.

| file | what it builds |
|---|---|
| `node.Dockerfile` | the bare node: sshd and `embernode`, nothing else |
| `workload-sd.Dockerfile` | node + stable-diffusion.cpp (CPU) + a GGUF model baked in |
| `workload-sd-cuda.Dockerfile` | the same compiled against CUDA — **no model**, weights come from a store |

```
make workload-sd MODEL=/path/to/v1-5-pruned_bf16.gguf
ember up --config sd.yaml     # provider: local — free, CPU, a few seconds
```

#### Publishing the CUDA image

Either machine can publish it, and both run the same checks first.

```bash
gh workflow run "workload image" -f tag=sd-cuda --ref master   # in CI, every architecture
make publish-sd-cuda CUDA_ARCH=89 TAG=sd-cuda                   # from here
```

The local path needs `docker login ghcr.io -u <user>` with a classic token
carrying `write:packages`, and never takes the token on a command line.

Both build the image, run the publish lab set named once as `PUBLISH_LABS` in
the `Makefile`, and push only if it passes. That sharing is the point: the
workflow used to carry its own list of labs, and it drifted — a lab written to
catch a published image whose `embernode` did not understand `embernode fetch`
was not in it, so the one publish it existed to guard was the one place it
never ran.

**One image per architecture, built in parallel.** The workflow is a matrix:
one job per architecture, each compiling for exactly one, so the wall clock is
one compile rather than seven. The architecture list is not written in the
YAML — a `plan` job asks `ember archs --json` for it, because a list in a
workflow that drifts from the list it mirrors is the defect `PUBLISH_LABS`
exists to prevent. `fail-fast` is off: one architecture failing must not cancel
six that were fine, and a partial set is safe by construction, since ember
refuses a card with no `images:` entry before it rents anything.

A final `report` job prints one pasteable `images:` block and **exits non-zero
naming any architecture that published nothing** — a map missing a key is a
card ember will refuse to rent, so a block pasted over a complete map would
take those cards away silently.

Dispatch one architecture with `-f archs=89` to republish just that one.

Locally one architecture takes about four minutes on thirty-two threads
against roughly twenty on a four-vCPU runner, and the push is seconds rather
than minutes: the runtime base is pinned by digest, so only the layers that
actually changed go up.

stable-diffusion.cpp rather than ComfyUI or diffusers, deliberately: no Python
and no PyTorch anywhere, and 2.45 GB rather than a dozen. On a rented GPU that
difference is pull time, and pull time is billed at GPU rates.

Measured end to end on a rented RTX 4090 at $0.89/hour, generating one
384x384 image at 8 steps:

| | |
|---|---|
| whole pod life, rent to destroyed | **1m7s** |
| of which, sampling | **1.15s** |
| cost | **$0.0166** |

The generation is about a tenth of a cent. Everything else is pulling a
container and waiting for a machine to exist, and that overhead is per *pod* —
so one image per pod is the worst possible shape, and a hundred in one session
cost barely more than one.

**That measurement is why the CUDA image no longer carries a model.** The
checkpoint was 51% of the image and 92% of what the run cost, and a `COPY`'d
model gets a layer digest that is yours alone — so it can never hit a shared
cache on any host, and the identical bytes sit on the same machines under
somebody else's digest while yours is re-downloaded on every pod. Above about
10 GB it stops being possible at all: the registry refuses a larger layer, and
Wan 2.2's experts are 14 GB each.

Weights go on a store instead: `ember store push` once, `assets:` names them
with a digest, and the node verifies what it mounts before the GPU is asked to
do anything.

The CPU variant still bakes one in. It exists to prove a pipeline locally for
nothing and has no store to mount.

### Every published architecture, on the card it was built for

The same workload — SD 1.5 Q8_0, 512×512, 20 steps of Euler A, seed 42 — run
once on each. Every image came from the matrix; none of these numbers is an
estimate.

| arch | card | $/hr | it/s | generate | run cost |
|---|---|---|---|---|---|
| `sm_75` | RTX 2080 Ti *(local)* | — | 6.35 | 4.31 s | free |
| `sm_80` | A100 80GB PCIe | 1.79 | — | 2.01 s | $0.047 |
| `sm_86` | A40 | 0.59 | — | 2.75 s | $0.017 |
| `sm_89` | RTX 2000 Ada | 0.24 | 6.56 | 4.30 s | $0.014 |
| `sm_90` | H100 NVL | 2.89 | 17.76 | 2.38 s | $0.076 |
| `sm_100` | B200 | 7.99 | 21.81 | 2.18 s | $0.151 |
| `sm_103` | B300 SXM6 | 8.99 | 21.19 | 11.09 s | $0.346 |
| `sm_120` | RTX PRO 4000 Blackwell | 0.57 | 14.28 | 2.64 s | $0.016 |
| `sm_120` | RTX PRO 6000 MIG 1g.24gb | 0.69 | 9.73 | 1.92 s | $0.023 |

Three things in that table are worth more than the confirmation.

**The cheapest card is not the slowest.** An RTX 2000 Ada at $0.24/hr and an
H100 NVL at $2.89 differ by 2.7× in sampling rate and 12× in price. For a
workload this size the fixed costs — provisioning, the image pull, the weight
fetch — dominate, so the expensive card mostly buys a shorter tail. Pick on
VRAM and price; `images:` makes either safe.

**The B300 row is why `sm_103` exists at all.** The old lab table grouped it
with the B200 at `sm_100`; the card reports compute capability 10.3. Renting
one for two minutes cost $0.24 and replaced a guess that would have had ember
pull a 2.65 GB image onto an $8.99/hour card and abort on a missing kernel,
every time.

**A MIG slice is a real option.** A 24 GiB partition of an RTX PRO 6000 rents
at $0.69/hr against $2.49 for the whole card, runs the same `sm_120` image,
and sampled faster than a 4090 does. It was outside ember's card table until
one was rented, because the question was never the silicon — a partition is
the same chip — but whether `sd-cli` runs on one. It does.

### Why the image is not built for the machine that builds it

ggml defaults `GGML_NATIVE` to **on**, which is `-march=native` — the CPU doing
the build. From the same Dockerfile and the same source:

| built on | AVX-512 instructions |
|---|---|
| a Zen 3 desktop | 0 |
| a GitHub runner | 23,458 |

A binary full of AVX-512 runs on the hosts that have it and dies on the ones
that do not. On a rented RTX A4000 that was

```
ggml_cuda_init: found 1 CUDA devices ... compute capability 8.6
Illegal instruction (core dumped)
```

a `SIGILL` on the host CPU with nothing naming the CPU as the cause — and an
earlier run of the *same tag* on a different host had worked. The identical
digest behaving differently on two machines is worse than the `:latest`
problem pinning exists to prevent, because nothing in the reference can warn
you.

So `GGML_NATIVE=OFF`, with AVX2, FMA and F16C — Haswell, 2013, universal on
anything rentable — and the build refuses a binary containing a single AVX-512
instruction. The CPU path barely matters here anyway; the work is on the GPU.

## Build

```
make            # fmt, vet, test, build
make test       # unit tests, race detector, no Docker or network needed
make lint
make cover-check
make test-integration   # Docker labs; needs a working Docker daemon
make test-preview-lab   # the served path against a real sd-server, no GPU
```

`make test-preview-lab` is the one that watches a generation happen. It starts
`sd-server` inside `ember:sd` — the CPU workload image, so no GPU is involved —
drives it over a real SSH connection through `direct-tcpip`, and asserts on the
progress frames that come back: a 64×64 image in six steps produces seventeen
frames, every step from 1/6 to 6/6, each a decodable PNG. It also checks that
the server binds `127.0.0.1` and nothing else. Twenty-one seconds on a laptop;
build the image first with `make workload-sd MODEL=/path/to/a.gguf`.

Set `EMBER_CAPTURE_DIR` on it to write the server's own responses out as
fixtures — `internal/sdserver/testdata/README.md` records, per file, whether a
fixture was captured from a running server or written from documentation, and
why that distinction has already cost this repository money.

There is also a stateful fake of RunPod's API (`test/integration/runpodfake`)
whose control plane is pretend and whose data plane is a real container: the
provider, the exposure check, the SSH handshake against the minted host key,
the forward and the output fetch all run for real against it. It reproduces
the behaviours that cost money to discover — no host key reported, `ssh.direct`
appearing after the status reads RUNNING, and the web terminal port RunPod
publishes unbidden — so none of them can come back silently.

The labs run the real `ember` binary against a real Docker daemon: a real
sshd, a real SSH handshake against a pinned host key, a real forward, and a
`SIGKILL` mid-provision to produce the orphan that `ls` and `reap` exist to
find. Nothing in them skips — a missing daemon or image is a hard failure
naming the command that fixes it, because a lab that skips itself reports
success for a security control that never ran.

Go 1.27. The Makefile prefers `~/sdk/go1.27.0` when present so the compiler
cannot drift from what `go.mod` declares.

## Configuration

`ember.yaml` holds image names, GPU filters, ports and budgets, and is safe to
commit. Credentials are not in it: they come from the environment
(`RUNPOD_API_KEY`, `VAST_API_KEY`) or from `~/.config/ember/credentials.yaml`,
which ember refuses to read unless it is mode 0600 — checked on every load,
because a file created correctly once and chmodded later is the common case.
The environment wins over the file, so a one-off run can use another account
and CI needs nothing on disk.

A provider with no credential is left out of the registry rather than
registered and left to fail. `ember ls` exits 2 on a provider it could not
reach, so registering RunPod on a machine that has never had a token would
make every clean run look like a problem.

Nothing secret is ever passed as a command-line flag, because
`/proc/<pid>/cmdline` is world-readable — on a rented machine, readable by
whoever else is on that host.

### Several results from one rental

`cmd` runs one thing. `cmds` runs several, in sequence, on one node:

```yaml
cmds:
  - [sd-cli, --diffusion-model, /models/flux2.gguf, --prompt, "a cat",  -o, /workspace/out/01.png]
  - [sd-cli, --diffusion-model, /models/flux2.gguf, --prompt, "a dog",  -o, /workspace/out/02.png]
```

Every per-rental cost is paid once instead of once per result: waiting for the
machine, pulling the image, reading and verifying the weights, loading them.
Measured on a rented A100 that is about three minutes against roughly 100
seconds of generation, so ten images rented ten times spend more than half the
bill re-doing setup.

Still opaque argv, deliberately — ember does not know what `sd-cli`'s flags
mean, the same reason `gpu_probe` is a command rather than a built-in check.
Use `cmd` or `cmds`, never both.

**Outputs come home after each step, not once at the end.** A ten-step batch
is over an hour on a rented card, and fetching only at the end means an
expired deadline or a closed laptop loses all of it rather than everything so
far. The first step fetches everything; each later one fetches only what is
newer than the previous fetch, bounded by a marker file touched on the node —
a file rather than a timestamp, because ember's clock and the node's are not
the same clock.

A failed step stops the batch, and its output is still collected: the step
that failed is often the one whose output explains why.

Each step reports what it cost, and the total so far:

```
step 2/3: sd-cli --diffusion-model /models/flux2.gguf --prompt 'a dog' ...
workload finished in 1m43s
fetching 1 output path(s) into ~/ember/outputs/20261101-0912-5f3a...
fetched 1 file(s), 1.4 MiB
step 2/3 took $0.0261 of card time, $0.0519 so far
```

The figure covers the fetch as well as the generation, because the card is
rented for both — on the minutes of video the store was built for, the
transfer is not a rounding error.

**Card time, not the step's share of the bill.** The rental also paid for
provisioning, the image pull and the asset verify, and a batch pays those once
for all of it rather than once per step — which is the whole point. Counting
them into a per-step figure would overstate what batching saves.

`max_run_usd` is not affected by a batch and never reports a step: it is
checked against the offer's price times the TTL *before* anything is rented,
so it bites at selection and there is no step to name yet. A batch that needs
longer needs a larger `ttl`, and the ceiling then refuses or admits the whole
rental exactly as it did for one command.

### One resident model instead of one process per image

`cmds` pays each per-rental cost once. It cannot pay the **model load** once,
because every entry is a fresh `sd-cli` that reads the weights into VRAM again.
`sd:` starts one `sd-server` on the node, holds the weights, and drives it over
the same SSH connection.

Measured on an RTX 2080 Ti, sd15 Q8_0 at 256x256 and four steps, same prompts
and seeds both ways — all sixteen images byte-identical:

| jobs | `cmds` | `sd.jobs` | |
|---|---|---|---|
| 1 | 3.68 s | 9.72 s | starting a server costs ~6 s |
| 3 | 7.37 s | 10.60 s | |
| 8 | 16.40 s | 12.78 s | 22% faster |
| 16 | 30.82 s | 16.23 s | 47% faster |

**Each job costs 0.43 s instead of 1.81 s**, so the crossover is around five
jobs and the saving grows after that. Below five, `cmds` is the faster choice
and the simpler one — this is for batches, and for models whose load dominates
a short generation.

```yaml
sd:
  serve: [sd-server, --diffusion-model, /models/flux2-dev-Q8_0.gguf,
          --llm, /models/Mistral-Small-3.2-24B-Instruct-2506-Q8_0.gguf,
          --vae, /models/flux2-ae.safetensors]
  ready_timeout: 10m
  jobs:
    - prompt: "a paper boat on a slow river, morning light"
      width: 1536
      height: 1536
      steps: 28
      sampler: euler
      cfg: 1.0
      guidance: 4.0
      seed: 20261101
      output: boat
    - prompt: "the same river at night"
      output: river-night
```

`--diffusion-model` is right for FLUX.2, which names its diffusion model, text
encoder and VAE as separate files. A single-file checkpoint such as sd15 needs
`--model` instead; the wrong one of the two fails with
`get sd version from file failed`.

Use `cmd`, `cmds` **or** `sd.serve` — never two. A served run is refused before
anything is rented if the jobs and the server are not both there, and every job
is checked against what the server reports it can do before the first one is
submitted, so a mistyped `sampler` costs the model load and nothing else.

**This is the one place ember understands a workload's arguments.** Everywhere
else a command is opaque argv on purpose. That position does not survive
progress streaming: a program that will not read its own payload cannot report
that job two is fourteen steps into its first pass, and a `curl` in a config
file is not an integration — it is the same knowledge, maintained by hand.

### What a job can be told

The eleven fields above are what every job has. These are the rest of what a
text-to-image generation takes, all optional and all unset by default:

```yaml
    - prompt: "a cat"
      clip_skip: 2              # many SD1.5 finetunes are trained for 2
      eta: 0.0                  # noise added back by the ancestral samplers
      flow_shift: 3.0           # the rectified-flow models (FLUX, SD3)
      shifted_timestep: 0
      output_format: webp       # from the server's own list; png, jpeg, webp
      output_compression: 90    # jpeg and webp only
      slg:                      # skip-layer guidance; scale 0 is off
        layers: [7, 8, 9]
        layer_start: 0.01
        layer_end: 0.2
        scale: 2.5
      hires:                    # a second, higher-resolution pass
        enabled: true
        upscaler: Latent        # from the server's own list
        scale: 2.0
        steps: 10
        denoising_strength: 0.7
        target_width: 0         # an explicit size instead of a scale
        target_height: 0
        upscale_tile_size: 128
      vae_tiling:               # decode in tiles, for VRAM a whole decode
        enabled: true           # would not fit
        tile_size_w: 512
        tile_size_h: 256
        target_overlap: 0.5
        rel_size_w: 0.0         # a fraction of the image instead of a size
        rel_size_h: 0.0
```

`hires.enabled` and `vae_tiling.enabled` are switches: with the switch off,
ember sends no block at all, and with it on and nothing else set the server
uses its own defaults. `slg.scale` is the switch for skip-layer guidance,
because upstream's own "off" **is** a zero scale — so layers with no scale
configure nothing.

`clip_skip`, `eta` and `flow_shift` are **absent when the key is absent**, and
`0` is a value rather than "unset": the server's own defaults for them are -1,
null and null, unlike every field above where 0 means "leave it to the server".

`output_format` and `hires.upscaler` are checked against the lists the server
publishes, so a typo is refused before anything is rented. Everything else
with a range — a denoising strength, a tiling overlap, a compression — is
refused when the config is read.

**ember's names are not always upstream's.** Four are renamed on the way out,
so when reading sd-server's `api.md` beside an `ember.yaml`:

| `ember.yaml` and the UI | the request body |
|---|---|
| `steps` | `sample_params.sample_steps` |
| `sampler` | `sample_params.sample_method` |
| `scheduler` | `sample_params.scheduler` |
| `cfg` | `sample_params.guidance.txt_cfg` |
| `guidance` | `sample_params.guidance.distilled_guidance` |
| `eta`, `flow_shift`, `shifted_timestep` | `sample_params.<same>` |
| `slg.*` | `sample_params.guidance.slg.*` |
| `vae_tiling.*` | `vae_tiling_params.*` |
| `clip_skip`, `output_format`, `output_compression`, `hires.*` | the same, top level |
| `output` | nothing — it is the local filename stem |

`cfg` and `guidance` are the two worth knowing: both get called "cfg" in
conversation and they are different fields. The FLUX.2 recipe sets `cfg: 1`
with `guidance: 4`, so setting the wrong one generates at the wrong strength
and nothing reports it.

**What is not typed, and why.** An image input — img2img, inpainting,
ControlNet, IP-Adapter — and LoRA are not reachable from `sd.jobs` or from the
web UI: the API takes images as base64 in the request body, which a YAML file
cannot hold and the UI has no upload route for, and `lora[].path` and
`hires.upscaler` are names the server resolves against a directory of its own.
`strength`, `img_cfg`, `control_strength`, `ip_adapter_strength` and
`increase_ref_index` do nothing without one of those images, so they are not
typed either. `cache_mode`, `cache_option`, `scm_mask`, `scm_policy_dynamic`,
`qwen_image_layers` and the two `custom_sigmas` are untyped because nothing
here knows what values they accept.

All of those remain reachable from a config: `extra` reaches every field of
the API ember does not type, including nested ones, so nothing is out of
reach:

```yaml
    - prompt: "a cat"
      extra:
        strength: 0.6
        lora: [{ path: "detail", multiplier: 0.8 }]
        sample_params: { custom_sigmas: [14.6, 10.0, 0.0] }
```

`extra` is a **config-only** field. A request over HTTP has no such key and is
refused if it sends one, because `extra` reaches the fields naming file paths
on the node — so the web UI is bounded by what `sd.jobs` types, which is what
the list above is about.

A key in `extra` that ember also set is **refused**, not resolved. A seed or a
step count decided the wrong way makes two runs that claim to be the same
measurement produce different images.

Images arrive in the HTTP response rather than as a tar, so they are written as
they finish — `<run>/<output>-00.png` under the same output directory everything
else lands in, beside a `<output>-preview-00.png` that is overwritten as the
generation proceeds. `outputs:` still works and is fetched once at the end,
which is where the server's own log belongs.

Progress is reported as a pass and a step within it, never as one percentage:
sd-server's step count describes the current sampling pass, and passes restart
for each batch item, each noise stage and each highres pass.

**Previews cost GPU time, not just bandwidth.** ember defaults to `preview: proj`
every 5 steps. Upstream's default is a frame every single step, and the `tae`
and `vae` modes run a decode to produce each one — so the upstream default is
the expensive one, billed by the second, for frames nobody in an unattended
batch is watching. `preview: none` is the only free setting.

### Generating on this machine's GPU

A model that does not fit in VRAM is not a model that cannot run. sd-cli will
keep weights in system RAM and stream them into VRAM per layer
(`--offload-to-cpu`), and its auto-fit planner already does so when VRAM is
short:

```
auto-fit plan:
  compute-device weight cache priority: diffusion > te > vae
  components (params: compute device -> RAM -> other GPU -> disk)
```

So 56 GiB of weights run behind an 11 GiB card. Slowly — roughly 35 GB crosses
PCIe on each of 28 steps — but for nothing. Most iterations of a prompt are
asking "is the composition right", and that question does not need a rented
A100.

```bash
make workload-sd-cuda-local          # builds for the card in this machine
ember up --config ember.local.yaml   # same assets, same digests, free
```

[`ember.local.example.yaml`](ember.local.example.yaml) is the same
configuration as the rented one with the provider changed and
`--offload-to-cpu` added. The store is a Docker named volume — the same code
path a rented network volume uses, costing nothing and surviving between runs.

`make workload-sd-cuda-local` reads this machine's compute capability from
`nvidia-smi` and builds for exactly it. There is nothing special about the
result: one image per architecture is how every image is built now, so a local
build and a published one are the same artifact under the same naming.

That used to be false. The published image carried a *list* of architectures —
chosen narrowly, because each one cost build time — and a desktop card was
usually outside it, so the local build needed its own tag. A build matrix
removed the reason: one architecture per job means the wall clock is one
compile rather than seven, and `sm_75` is in the published set.

Measured on a 2080 Ti, driver 580.126.09 reporting CUDA 13.0, against the
single-architecture `sm_75` image:

```
ggml_cuda_init: found 1 CUDA devices (Total VRAM: 10813 MiB):
  Device 0: NVIDIA GeForce RTX 2080 Ti, compute capability 7.5, VMM: yes
CUDA0   NVIDIA GeForce RTX 2080 Ti
```

It selects the CUDA backend rather than falling back to the CPU — which is
the whole claim, since falling back is what a wrong-architecture image does
without failing. Note the driver version: CUDA 13.0 is the floor the 13.0.1
base needs, so this machine is exactly at it.

What that does **not** answer is whether a 35 GB model set is usable on 11 GiB
— whether 1536×1536 fits beside a 4412 MiB compute buffer, and what a step
costs when the weights cross PCIe per layer. `sd-cli` will stream them
(`--offload-to-cpu`) and its auto-fit planner already does when VRAM is short,
so it is slow rather than impossible; how slow is unmeasured.

Running the published image on an unsupported card aborts: ggml raises `no
kernel image is available for execution on the device` and sd-cli dies about a
second into the first generation. Loud, but late — the node exists, the image
is pulled and the weights are fetched before it happens, and all of that is
billed. `images:` is what moves the refusal to before the rental, and
`gpu_probe` is what catches a hand-pinned mismatch the map cannot see.

The local provider reports a real offer when it finds a card, so `ember
offers` and `min_vram_gib` work the way they do against a cloud provider.
Without one it reports no GPU, exactly as before, which is what keeps the labs
working on machines that have never had one.

### Checking the card before trusting it

A GPU that cannot initialize does not fail. It silently becomes a slow CPU, at
the GPU's price. One run learned this the expensive way: an A100 came up, CUDA
reported `system not yet initialized`, sd-cli fell back to its CPU backend, and
a 32-billion-parameter model began running on 128 threads. ember thought the
workload was fine, because it was running.

```yaml
gpu_probe: ["sh", "-c", "sd-cli --list-devices | grep -qi cuda"]
gpu_probe_timeout: 2m
```

`gpu_probe` runs on the node before the assets and before the workload, and
must exit zero. Empty runs no probe. It is a command rather than a check ember
makes itself, because what proves a card usable belongs to the image — and
deliberately not `nvidia-smi`, which will list a card on a host whose NVSwitch
fabric is down, which is exactly the failure above.

`gpu_probe_timeout` is how long the card has to come up, retried every five
seconds, default two minutes. Retried rather than checked once because the
driver's own words were *not yet*: an SXM board waits for
`nvidia-fabricmanager` on the host, and probing at the earliest possible moment
turns a card that needed forty seconds into a refusal.

`min_cuda` exists and **RunPod refuses it**. Setting it is an error naming
this paragraph, rather than a constraint that silently does nothing.

It was added from RunPod's published schema, as `allowedCudaVersions`. The
wrong schema: `rest.runpod.io/v1/pods` accepts that field, and
`api.runpod.io/v2/pods` — which is where ember creates — declares eighteen
properties with `unevaluatedProperties: false` and has no CUDA field at all.
Every create with `min_cuda` set died at

```
status 422: additional properties 'allowedCudaVersions' not allowed
```

Filtering offers instead is not available either: the v2 schema puts
`cudaVersions` on `GpuType`, and the live catalogue does not return it. So the
constraint cannot be expressed at create time or at selection time, and the
honest answer is to say so — a constraint an operator believes is in force and
is not would be worse than none.

This is the second time RunPod's two APIs have disagreed about a field; the
first was `dataCenter` against `dataCenterId` on network volumes.

**`gpu_probe` is what enforces a driver floor.** It runs on the node before
the workload and costs the probe rather than the run. In practice the floor is
not tight anyway: a rented RTX 4090 reported driver 595.91.07 with
`CUDA Version: 13.2`, ahead of the 13.0.1 the images are built against.

### Choosing the image by the card

An image built for one CUDA architecture does not run on a card it has no
kernel for. Measured, on a rented RTX 2000 Ada carrying an `sm_75` image:

```
[E] ggml: CUDA error: no kernel image is available for execution on the device
/src/ggml/src/ggml-cuda/ggml-cuda.cu:113: CUDA error
Aborted (core dumped)
```

It aborts about a second into the first generation — but only after the node
exists, the image is pulled and the weights are fetched, which is all billed.
So the cheapest moment to catch a mismatch is before anything is rented.

`images:` maps a compute capability to the image built for it, and ember picks
the entry for the card it is about to rent:

```yaml
images:
  sm_80:  ghcr.io/you/ember-sd:sd-cuda-sm80@sha256:…   # A100
  sm_89:  ghcr.io/you/ember-sd:sd-cuda-sm89@sha256:…   # 4090, L40S
  sm_90:  ghcr.io/you/ember-sd:sd-cuda-sm90@sha256:…   # H100, H200
```

The key is the architecture as `nvcc` spells it: compute capability 9.0 is
`sm_90`. The tag is there to be read and the digest is what runs, so a tag
naming a different architecture than its key is refused rather than ignored —
six lines of hex under six keys is somewhere a mis-paste is invisible.

**A card with no entry is refused, and ember will not fall back to `image:`.**
That is the whole point: falling back would run a wrong-architecture image,
which is the silent CPU run this key exists to prevent. The refusal names the
architecture, the card and the command that builds one.

**This is not Docker multi-arch.** In Docker, "arch" means CPU platform, and a
manifest list lets one reference resolve to `amd64` or `arm64` automatically.
There is no such field for a GPU compute capability, so `docker pull` cannot
pick `sm_90` over `sm_89` and a manifest list would select nothing useful.
Eight tags plus a map in the config is the shape that is actually available —
which is also why ember has to do the choosing.

`image:` is still the supported shape for a single image, and both keys may be
set — a run that needs no card at all, such as `ember store sync` on a CPU
machine, has no architecture to choose by and uses `image:`. `-image` on the
command line replaces the map for that run.

`ember archs` prints the architectures an image is published for, and
`ember archs --cards` says which cards each one is for — so finding the key a
card needs costs nothing and rents nothing:

```
$ ember archs --cards
89      sm_89   l4 l40 l40s rtx4080 rtx4090
90      sm_90   h100 h100-80g h200
```

ember knows which architecture a card is from a table in
[`internal/node/compute.go`](internal/node/compute.go). A card it has never
heard of is refused rather than guessed at, for the same reason as everything
else here: a guess rents.

One naming detail follows from that. `ada` is **not** treated as noise in a
GPU name, unlike every other architecture word — because it is the only one
NVIDIA puts in a product name to tell two different cards apart. A Turing
Quadro RTX 6000 is `sm_75` and an RTX 6000 Ada Generation is `sm_89`, so they
normalise to `rtx6000` and `rtx6000ada` and `--gpu rtx6000` matches only the
Turing one. Folding them together made the name unable to say which silicon it
was, which cost the cheapest card RunPod rents — an RTX 4000 Ada at $0.28/hr.

On the local provider none of this applies, because `nvidia-smi` reports the
capability of the card in the machine directly.

### Stores

A **store** is persistent storage that outlives the node that mounts it —
RunPod calls it a network volume. It exists so model weights stop being baked
into the workload image: a `COPY`'d checkpoint gets a layer digest that is
yours alone and can never hit a shared cache on any host, and at 14 GB per
expert a modern video model exceeds the registry's per-layer limit outright.

```yaml
store:
  name: models          # the provider sees `ember-store-models`
  size_gib: 40          # minimum 10, which is what the API enforces
  region: EU-RO-1       # a store pins every node that mounts it to its own datacentre
  mount: /models        # default; /workspace and /root are refused
```

Naming a store here does **not** create it — `ember store create` does, and
nothing that rents a node will bring one into existence as a side effect. A
store is a standing monthly charge with no deadline, no reaper and no Ctrl-C
that ends it, so it takes a command somebody typed.

`ember ls` prints every store ember owns and what it costs per month, on every
run, including the run where there are no nodes at all — the clean-bill-of-
health case is exactly the one a charge with nothing attached would otherwise
hide in. It does not call an unused store a leak: ember cannot tell one that
is finished with from one being kept between runs, and a line that said
"leaked" about weights you deliberately uploaded invites an `ember store rm`
that costs a re-upload. It names the command and leaves the decision alone.

### Assets

Weights are listed, not baked in. Each one is fetched onto the store once and
verified against its digest before any workload runs:

```yaml
assets:
  - url: https://huggingface.co/.../v1-5-pruned_bf16.gguf
    sha256: d44a17e278412bd45c1847a91d30a69146b9bb9144c91a61fa0e357d7e2466af
    as: sd15.gguf
verify: always        # or `present` to skip the re-read
```

The digest is **not optional**. A store is writable by every node that mounts
it, which makes it a channel between runs that are otherwise isolated — one
workload can leave something behind that the next one loads as its model.
RunPod offers no read-only mount, so the write cannot be prevented; it is
detected instead, and detected before the GPU has been asked to do anything.

That digest is also why plain HTTP is allowed. TLS authenticates the server;
the digest authenticates the bytes, which is the stronger claim and the one
that matters for somebody else's weights. An attacker on the path can deny
service — the fetch fails closed and nothing is written — but cannot
substitute content.

`verify: always` is the default and re-reads what is already there, at roughly
a gigabyte a second. `verify: present` trades that check for the time it costs
— about forty seconds on a forty-gigabyte model set. Worth having as a choice;
not worth being the default, because the run it protects costs dollars and the
check costs cents.

A download is written beside its destination and renamed only once the digest
matches, so a file at the final name is always one that has been verified at
least once. An interrupted fetch leaves nothing behind.

### Filling a store

```bash
ember store push ./v1-5-pruned_bf16.gguf
```

Nothing is rented. The file goes straight to the volume over RunPod's
S3-compatible endpoint, so the only charge is the storage that was going to
exist anyway — fetching the same file from a node would bill the download at
GPU rates, which is about ten minutes of an H100 for a 40 GB model set, every
time a store is filled.

It prints the digest at the end, because that is the value `assets:` needs and
reading it from the same pass that uploaded the file is how the two cannot
disagree.

This needs a **second credential**: RunPod issues S3 keys separately from the
API key, on the Credentials page under "S3 API keys", and shows the secret
once. They go in `credentials.yaml` as `runpod_s3_access_key` and
`runpod_s3_secret`, or in `RUNPOD_S3_ACCESS_KEY` / `RUNPOD_S3_SECRET`.

### Or let a rented machine fetch them

```bash
ember store sync
```

`push` sends bytes from here; `sync` rents the cheapest machine RunPod has —
**$0.06/hour against $0.89 for a 4090** — and has it download the URLs in
`assets:` at datacentre bandwidth. No GPU is rented, because the job is bound
by the network and the disk.

Which one is right depends on where the files already are:

| | from here (`push`) | from the internet (`sync`) |
|---|---|---|
| sd15, 2.21 GB | 4 min of your upstream | **2m27s, $0.0024** |
| a 40 GB model set | **81 min** of your upstream | minutes, about a cent |

Measured, not estimated: 67 Mbps up from here, and sd15 fetched and verified
on a `cpu3c` for a quarter of a cent.

Only **10 of RunPod's 23** volume-capable datacentres serve the S3 API. A
store created outside those can only be filled by renting something to
download into it, so the datacentre is worth choosing deliberately — see
[docs/gpu-availability-history.md](docs/gpu-availability-history.md) for what
else that choice decides.

### Reading one back

`ember store` could write to a store and not read from it, which had two
costs. The smaller one: a store's contents were knowable only from a rented
node, because assets are verified over the SSH connection the workload uses —
so the cheapest moment to learn that a 14 GB expert was never uploaded came
after provisioning, the image pull and the mount had all been paid for.

```bash
ember store verify
```

One `HEAD` per asset over the store's own endpoint. Nothing is rented and
nothing is downloaded, and exit 2 means at least one asset is missing or
sitting there at zero bytes — which is what a `PUT` that failed after creating
the key leaves behind, and what `verify: present` on a node would load as a
model. It does **not** check digests: that would mean reading every object
back through this process to re-derive a number the node derives anyway, at
datacentre bandwidth, before the workload starts. It says so rather than
letting the output imply otherwise.

The larger cost is a leak. **An interrupted multipart upload keeps every part
that arrived and bills for them by the gigabyte-month** — and it is not an
object, so it appears in no object listing and `ember ls` cannot see it
either. The abort that exists runs from a defer on a context Ctrl-C cannot
cancel, which covers Ctrl-C and covers neither SIGKILL, nor a panic, nor a
laptop closing, nor an OOM kill. Until these commands existed nothing in ember
could find one: it was the only money leak here with no detection path at all.

```
$ ember store ls
models (runpod/vol_abc, 40 GiB, EU-RO-1) at $2.8

objects
     2.0 GiB  2026-11-01T09:00:00Z  sd15.gguf
    16.0 GiB  2026-11-01T09:00:00Z  wan22-high.gguf
    18.0 GiB in 2 object(s)

incomplete uploads
   384.0 MiB    3 part(s)  2026-10-31T13:48:09Z  abandoned   flux2.gguf
   384.0 MiB in 1 upload(s), costing $0.02625/month on top of the store itself
These are not objects: they are in no object listing and `ember ls` cannot
see them either. They bill until something removes them.
1 has been there longer than 24h0m0s. Remove it with:
  ember store abort models
```

```bash
ember store abort
```

Both are bounded by `--older-than`, a day by default, and that bound is the
whole safety of the command: an upload in progress and an abandoned one are
the same thing to S3 — both are started and not completed. Without it,
`abort` would kill the push running in another terminal, and `ls` would exit 2
over a healthy one, which teaches an operator to ignore exit 2. `ls` lists
every incomplete upload either way and marks which ones it is calling
abandoned, because somebody checking on their own push wants to see it. The
same reasoning bounds the leftover-network sweep by a node's deadline.

Nothing is lost by waiting out the bound: the parts bill by the month, so a
day costs about a thousandth of what they cost unnoticed. The cost is priced
from bytes rather than whole gibibytes, because a 384 MiB orphan rounds to
zero gibibytes and **a leak that reports as free is a leak nobody removes.**

`mount` is refused if it would shadow `/root` or `/etc`, which hold the
authorized_keys and host key `embernode` writes at startup — a store there
leaves sshd with neither and a handshake failure that reads exactly like an
attack. `/workspace` is refused too: it is the provider's default mount path
*and* where ember writes outputs, so a store there bills for every render for
as long as the store exists.

## The web UI

`ember serve` runs a local web UI and a queue that drains onto **one rental**.
It is the `sd.jobs` shape driven from a browser: pick a provider and a card
from live offers, type a prompt and its parameters, push a batch, and watch the
sampling steps and preview frames arrive while the images land on disk.

The five rows of the compose panel are the whole job on most runs; an
**advanced** section below them holds the rest of what a generation can be
told — clip skip, the sampler's eta and flow shift, the output format, a hires
pass, VAE tiling and skip-layer guidance. Every label has an explanation on
hover. What the UI cannot send, and why, is the list at the end of
[what a job can be told](#what-a-job-can-be-told).

```bash
make web                 # builds the Rust/WASM frontend
ember serve              # http://127.0.0.1:7777/
```

### How it fits together

Everything below the dashed line is what `ember up` already did; `ember serve`
adds a queue in front of it and a second reader of the progress it was already
producing.

```
  browser                        ember serve (loopback only)
 ┌──────────────┐   POST /api/requests   ┌──────────────────────────────────┐
 │ web/ Leptos  │ ─────────────────────► │ internal/webapi                  │
 │ → WASM       │                        │   own wire types, never a domain │
 │              │ ◄───── GET /ws ─────── │   struct; Origin + Host checked  │
 └──────────────┘    events after <seq>  └───────┬──────────────────────────┘
        ▲                                        │ enqueue
        │ images                                 ▼
        │                  ┌──────────────────────────────────────────┐
        │                  │ internal/genstore — SQLite               │
        └────────────────► │   requests · events · images · caps      │
          blob.Dir         └───────┬──────────────────────────────────┘
          (one dir per req)        │ ClaimNext: one statement, claim or empty
 - - - - - - - - - - - - - - - - - ▼ - - - - - - - - - - - - - - - - - - - - -
                           ┌──────────────────────────────────┐
                           │ internal/queue — ONE drain       │
                           │ goroutine. A rental lives inside │
                           │ one iteration, so two at once    │
                           │ cannot be expressed.             │
                           └───────┬──────────────────────────┘
                   queue.Narrow(cfg, req): min() of the request and ember.yaml
                                   ▼
                           ┌──────────────────────────────────┐
                           │ internal/app → provision.WithNode│
                           │ mint host key · create · verify  │
                           │ exposure · destroy on the way out│
                           └───────┬──────────────────────────┘
                                   │ ssh, host key pinned to the minted one
                                   ▼
   rented node ─────────────────────────────────────────────────────────────
   sshd ─► sd-server --listen-ip 127.0.0.1, reached by direct-tcpip only
           weights resident: loaded once for the whole batch
```

Progress travels one way and is stored once. Every emitter writes a
`report.Event` into a `report.Stream`, which stamps the sequence number, and
the stream fans out to the terminal renderer and to a recorder that appends to
SQLite. **The socket reads those rows rather than subscribing to the stream**,
so a reconnect and a page reload are literally the same code path and a client
can never be told something a reconnect would not tell it. A preview frame is
written to the run directory and recorded as an image row with `preview` set,
which is how a step and its picture reach the page together.

The frontend is Rust compiled to WebAssembly, in [`web/`](web). It needs
`cargo`, the `wasm32-unknown-unknown` target and `wasm-bindgen-cli` at the
version [`web/Cargo.toml`](web/Cargo.toml) pins — `make web` checks all three
and names the command that fixes each. **Nothing else here needs any of it:**
`make build`, `make test` and `make cover-check` work on a clean clone with no
Rust installed, because `internal/webapi/dist` carries a committed placeholder
page. Without `make web`, `ember serve` says so and the JSON API under
`/api/` works anyway.

```
      --allow-paid   let the web UI rent from providers that cost money
      --config       path to ember.yaml (default "ember.yaml")
      --dist         serve the UI from this directory instead of the built-in copy
      --port         loopback port to serve on (default 7777)
```

**One rental, drained, with no idle wait.** A request is a batch of jobs; while
a node is up the queue keeps claiming newly queued requests whose provider and
card match, and sends their jobs to the `sd-server` that is already holding the
weights. The moment nothing compatible is left it returns, which destroys the
node at once. There is no idle timeout, because a node with nothing to do is a
node billing for nothing.

That is structural rather than checked: there is one drain goroutine and a
rental lives inside one iteration of its loop, so two concurrent rentals cannot
be expressed. A double-clicked button cannot rent twice because there is
nothing for the second click to start.

**It needs an `sd:` block**, because the serve argv names the weights and a
request may not. `ember serve` refuses to start without one and says which
example to copy.

**Watching it sample.** A request carries a preview mode and an interval, and
the page shows each frame as it arrives beside the step that produced it.
`proj` projects the latent -- nearly free, and a 512x512 generation previews
as a 64x64 smear; `tae` and `vae` decode a real image every frame, which is a
decode billed to the card. The default is `proj` every 5 steps, not upstream's
every step, and a negative interval is refused rather than clamped because
sd-server would read it as 1 -- the most expensive setting rather than the
least. Every field in the form explains itself on hover.

### What a request may and may not choose

| a request may narrow | only `ember.yaml` decides |
|---|---|
| provider, card, region | `image:` and the `images:` map |
| `ttl`, up to the config's | `store:` and its mount |
| `max_hourly_usd` and `max_run_usd`, up to the config's | `ports:` |
| prompt, size, steps, batch count, seed, sampler, scheduler, cfg, guidance, output name | `sd.serve`, `gpu_probe`, `assets:`, `verify:` |
| clip skip, eta, flow shift, shifted timestep, output format and compression | `extra` |
| the `hires`, `vae_tiling` and `slg` blocks | an image input and `lora` |
| preview mode and interval | |

A request asking for a *higher* ceiling gets the config's, not a refusal: a UI
offering a slider should not have to know every limit before it can offer
anything. Everything in the right column stays in the file, which is what keeps
every seal in `internal/node`, `internal/exposure` and `internal/loopback`
upstream of anything a request can influence — `node.MountAt`'s refusal of
`/root`, `ValidateImage`'s refusal of `:latest`,
`sdserver.ServeCommand`'s refusal of a listen flag.

`extra` is absent from the wire type entirely. It reaches every field of
sd-server's request body that ember does not type, including the ones naming
file paths on the node, so a request that could set it could point a running
server at weights the config never mentioned.

### What is not guarded

**There is no authentication.** The server listens on loopback only and nothing
it serves is reachable from another machine — but any page open in the
operator's browser can reach a loopback port, and the browser hides only the
*response*. CORS is not a control here.

What bounds the damage is **`--allow-paid`, which is off by default**: without
it `ember serve` refuses every provider that is not free, so a cross-site POST
to a default invocation can start a free local container and spend nothing.
That is the reason the flag exists and the reason it defaults off.

Two things that are not authentication are checked anyway, on every route: the
`Origin` must be this server's own, and the `Host` must be a loopback name at
this port. The second is the DNS rebinding case, where an attacker's domain
resolves to 127.0.0.1 and their page is therefore same-origin with its own
name — which passes the first check and fails the second.

### Where the state goes

The queue and the history are a SQLite database beside the node journal, under
the state directory. Images are written where `ember up` has always written
them, one directory per request, and the same path is the key a later storage
backend would use. Progress is stored as numbered events, so a page reload and
a dropped websocket are the same code path: a client says which sequence number
it last saw and is told everything since.

A request that was generating when the process died is marked **interrupted**
rather than failed or re-queued. Its rental is gone so it cannot be resumed,
and renting a second card for work the operator may no longer want is not a
decision ember makes on their behalf. Whatever images it produced stay listed,
because they were paid for.

### Checking it works

```bash
make test-integration    # the serve labs that need no workload image
make test-serve-lab      # one rental drained by a queue, and the real UI
```

The first group runs on every pull request and is the security half: that the
server answers on both loopback families and on none of this machine's other
addresses, that a page from somewhere else is refused, that a paid provider is
refused without the flag, that SIGINT destroys the rental and says so, and that
SIGKILL leaves exactly one finding `ember ls` reports as needing attention.

`make test-serve-lab` needs [`ember:sd`](#workload-images) and a Chrome or
Chromium binary. It queues a three-job batch, waits until it is generating,
queues a second request, and asserts from ember's own output that **one** node
served both — then drives the real page in a real browser: offers listed,
prompt typed, generate clicked, the sampling step advancing, and an image that
decodes.

That browser lab is not completeness. Three bugs in this feature were invisible
to every Go test and every `cargo test` and obvious the moment a page loaded: a
nil slice marshalled as `null` dropped whole websocket updates, a keyed list
never re-rendered so the progress row froze, and a cached wasm meant a reload
after `make web` served the previous app. It is the only thing in the
repository that can see them.

### Stopping it

Ctrl-C destroys any rental before the process exits, and says so. `ember serve`
waits for the queue's goroutine as well as the HTTP server: a rental does not
live in a request, so draining connections would say nothing about a node.

Cancelling a **queued** request is a button. Cancelling one that is already
generating is not: it would mean destroying the node, which would take every
other request on the same rental with it. Stop `ember serve` to end a rental.

### What it cost

Everything needed to say what a job cost was being **emitted** from the
beginning — integer-microdollar cost, measured against reported durations,
elapsed against billable, the pass and the step within it — and almost none of
it was **queryable**. It went into one opaque JSON blob per event, and the
`jobs` table, the row a job *is*, had no execution columns at all: no started,
no ended, no cost, no steps sampled. The only aggregate anywhere in ember was
a Go loop over one request's entire event stream, and nothing called it.

So every measurement in this file was computed by hand from a run somebody
watched, which means nothing trends any of them and nobody would notice
ember's own overhead regressing.

```
$ ember spend --by gpu
card          reqs  jobs images      rental   card time    overhead  per image steps/s
a100-80g         1     4      4       $1.79       $1.52       $0.27    $0.4475    0.05
rtx4090          3     9      8       $1.39       $0.76       $0.63   $0.17375    0.15

total: 4 request(s), 12 image(s), $3.18 of rental, $2.28 of card time
41% of billable rental time was spent generating.
1 request(s) contribute their rental cost and no per-job figures -- they ran
before ember recorded them, or finished without a job completing. So per image
is an upper bound here, and steps/s is computed only over what was recorded.
Nothing backfills them.
```

`--by` takes `provider`, `gpu` or `day`; `--since` and `--until` are UTC dates
and the period is half-open, so consecutive periods tile without counting a
day twice. `--json` prints the rows with money as integer microdollars, and
without the derived figures: cost per image and steps/s are divisions of
columns that are there, and a stored quotient is a number that can be wrong
while its terms are right.

Four things it deliberately does not do.

**The two cost columns are not interchangeable.** The rental is what the
provider charges; card time is what the jobs themselves took. A batch pays
provisioning, the image pull, the store read and the model load once for
everything it serves, and the gap between the columns is that overhead —
measured, now, rather than estimated by hand at 7%. `rental` is summed over
the request that *opened* each rental, so a rental is counted once however
many requests rode along on it.

**It cannot group by model.** Nothing records which model served a request:
`sd.serve` is config rather than request, and the only stored copy is a single
cached capability row that the next rental overwrites. Grouping by it would
mean guessing, which is the `a100-40g` is not `a100-80g` mistake one layer up.
Recording it is a column written at claim time and a small follow-up.

**`ember up` is not in here.** It prints its cost and keeps nothing — only
`ember serve` writes to the generation history — so about half the
measurements in this file came from runs this command cannot see. Making `up`
record too is the largest remaining question and its own change.

**Nothing backfills.** A request from before these columns existed has its
rental cost and no per-job figures, and `ember spend` says how many it had to
leave out on every run. The facts are in those requests' event bodies, so a
backfill is possible — and it would have to re-derive the step counts from
progress frames whose poll interval is unknown for those runs, which is the
one number this schema is careful not to infer. "ember did not record it then"
is the honest answer.

The step count is worth one more line, because it is the easiest figure here
to get wrong. A pass restarts for each batch item, each high- and low-noise
stage and each highres pass, so the `steps` a job asked for is the steps of
*one* pass and an it/s built from it understates every batched job. And the
steps of a pass are read off that pass's own declared total rather than
counted from the frames ember saw: ember deduplicates progress on the step, so
at a poll slower than the card samples it observes fewer frames than there
were steps, and a count of frames would measure the poll interval instead of
the work.

### Which run was it

`ember spend` answers what a period cost. The next question is always which
run it was, and before these two commands there was nowhere to ask it: the
generation history was **write-only** from the command line. The queue filled
it, the web page read one request's rows, and `grep -rln genstore internal/cli/`
named one file — `cmd_serve.go`, which only starts the queue. Looking at a
finished run meant starting a server that can rent a card and clicking through
a browser, or opening SQLite by hand.

```
$ ember history
when (utc)       request          status      provider   card          jobs images      wall      rental   card time
2026-10-31 18:19 080f21c0cc2f5be8 done        runpod     rtx4090          3      3     6m45s           -      $0.285
2026-10-31 18:02 5f324627337df6f5 done        runpod     rtx4090          5      5    14m30s       $0.89      $0.475
2026-10-29 09:14 15d87a1aae672727 done        runpod     a100-80g         4      4    41m12s       $1.79       $1.52
2026-10-24 11:40 1393e5b386595420 done        runpod     rtx4090          1      -     33m0s        $0.5           -

total: 4 request(s), 12 image(s), $3.18 of rental, $2.28 of card time
1 request(s) rode along on a rental another request opened, so their rental
cost is a dash rather than zero: the node was charged once, to whichever
request paid for it to exist.
1 request(s) finished with no per-job figures -- from before ember recorded
them, or without a job completing -- so their images and card time are a dash
rather than zero. Nothing backfills them.
```

`--since` and `--until` are UTC dates and half-open, the same rule `ember
spend` uses, so the two tile alike. `--provider`, `--gpu` and `--status` match
exactly; `--limit` bounds the page, which defaults to the newest 200. `--json`
prints the rows with money as integer microdollars and the full text of any
error, where the table shows one line of it.

**The two dashes are the point of the table.** A rental serves every queued
request it can, and its cost is charged to the request that *opened* it — so
every request that joined a rental already up has a rental cost of zero by
design. A money column of zeros that means "charged on another row" is the
misreading this marker exists to prevent, and it is decided by asking whether
another request on the same rental carries the cost, not by the zero itself:
the local provider's rentals are free, so cost alone cannot tell the two
apart. The second dash is a request that finished with no per-job figures,
which is every request from before the execution columns existed and also one
that broke before a job completed. Both divide a rental cost into no images,
which is why they are counted together and why `ember spend` calls per image
an upper bound when there are any.

**It cannot spend.** No provider is constructed anywhere on the path, and the
only statement it runs is a `SELECT` — so "a listing command must not be able
to rent, enqueue or claim" is a property of what the command has rather than a
rule somebody has to keep. A test lists three times and then asserts the
queued request is still claimable.

**It does not read the prompts.** A ledger row carries the counts and the
money and no job text, because reading the prompts is a query per row for
something the table never prints; `ember serve`'s own API is where one
request's jobs come from. The per-request figures and `ember spend`'s sums
come out of one roll-up, so the two cannot come to disagree about what a run
produced — a conformance test sums the ledger and compares it against the
aggregate, in both stores.

## Video

[`ember.video.example.yaml`](ember.video.example.yaml) is the configuration
the store exists for. Wan 2.2's experts are 14 GB each, which could not be
baked into an image if anybody wanted to — the registry refuses a layer above
about 10 GB — so the weights go on a volume and the node verifies them before
the GPU is asked to do anything.

The pinned `stable-diffusion.cpp` supports it: `-M vid_gen`, `--video-frames`,
Wan's own latent format and the Wan 2.2 S2V audio encoder are all in the
binary this repository builds, asserted by a lab so the answer cannot quietly
stop being true when the pin moves. Both images are pinned to the same
upstream commit, which the same lab checks.

Two things that configuration does not do, deliberately:

**It generates one clip, not a minute.** No open-weight model generates a
minute in one pass — 81 frames at 16 fps is about five seconds, and the frame
counts that map to clean latent lengths are 33, 77 and 81. A minute is twelve
clips chained on the last frame of the one before, which is a pipeline rather
than a command and does not belong in `cmd:`. The seams between those clips,
not the fidelity within them, are what makes a long video hard.

**Nothing has run it end to end.** It is written from the measured API shapes
and the flags in this repository's own image. The first real run is a
calibration — it costs money, and what it produces is a number and a fixture
rather than a pass or a fail.

## Design notes

Things worked out but not built, kept where the reasoning was fresh rather
than in a commit message nobody will search for.

- [Collecting GPU availability over time](docs/gpu-availability-history.md) —
  RunPod reports capacity only as it is right now, and a store pins every
  future run to one datacentre, so "which site reliably has this card" is a
  question a single sample cannot answer and nobody is recording.

## Licence

MIT.
