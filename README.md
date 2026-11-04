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
| `internal/catalogue` | the availability series — append-only, and the one thing no later run can backfill |
| `internal/sshx` + `sshexec` | run one command on a node, stream its output |
| `internal/transfer` | bring the outputs home — tar over SSH, into an `os.Root` |
| `internal/workload` | run the command the node was rented for, streaming |
| `internal/reconcile` | pure scan over providers and records — what `ls` and `reap` act on |
| `internal/session`, `internal/config`, `internal/cli` | wiring |

Every verb `ember` dispatches, with the one-line summary it prints for itself:

<!-- verbs: checked against internal/cli.commands() by TestREADME_NamesEveryVerb.
     Name, summary and order must match the dispatch table exactly, in both
     directions: a verb added there and not here fails the build, and so does a
     row for a verb that no longer exists. This list said six while the program
     had twelve, because it was maintained by hand and nothing compared them. -->

| verb | what it does |
|---|---|
| `version` | print the ember version |
| `up` | rent a node, forward its ports to loopback, destroy it |
| `down` | destroy a node by handle |
| `ls` | list nodes ember created, across every configured provider |
| `serve` | run the web UI and a queue that drains onto one rental |
| `reap` | destroy every node past its deadline |
| `spend` | report what the recorded generations cost |
| `history` | list the generations ember has run, and measure or prune what they left |
| `offers` | list rentable offers matching a filter |
| `store` | create or destroy persistent storage that outlives a node |
| `catalogue` | record what a provider has available, and read the series back |
| `archs` | print the CUDA architectures ember publishes an image for |
| `images` | assemble an images: block from the digests a build published |

`ls` asks every provider rather than trusting the local records, and exits 2
when anything needs attention — including a provider it could not reach.

The RunPod provider has rented real hardware with real money. Eight rented
cards across seven architectures are measured below, each with the price paid
and the wall-clock it bought; the exposure check earned its keep on the first real
provision by refusing a node and destroying it, and the store, the volume
throughput and the resident-model comparison are all live figures.
`internal/provider/runpod/testdata/README.md` records which of the wire
fixtures were captured from the live API and which were written from
documentation, because this repository has twice paid for believing a schema
over a response.

Next: Vast.ai, which nothing has spoken to — see
[#22](https://github.com/Legion112/ember/issues/22). See `AGENTS.md` for the
rules all of it is written against.

`ember version` reports what built it: `git describe --tags --always
--abbrev=12 --dirty`, so a measurement can name the ember that produced it. See
[Build](#build) for what a tagless, dirty or git-less build prints and why none
of them invents a tag.

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

One exception, and it is the one worth reading twice: an `ember up` **submitted
to a running `ember serve`** detaches on Ctrl-C instead of destroying, because
the run is in that process and not in your terminal. The deadline is then the
whole of what stops the bill, which is why the client prints it as it detaches.
See ["`ember up` is that channel's first
client"](#ember-up-is-that-channels-first-client).

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

#### What the node image is published as

`node.Dockerfile` is built by `.github/workflows/node-image.yml` on every push
to `master` that touches it, and pushed to
`ghcr.io/legion112/ember-node:<commit sha>`. The run's summary prints the
digest, which is what `image:` should name.

**There is no `:latest` tag, and that is not an oversight.** ember runs one
container that is both the node and the workload, so the node image *is* the
`image:` key and goes through the same refusal as any other reference: a
`:latest` image, or one with no tag at all, is rejected before anything is
rented. A published `:latest` could therefore never be provisioned from — it
was pushed until this was noticed, and nothing in the repository ever named
it. If you were pulling it, pull the sha tag instead and put the digest in
your config.

Renaming it to something like `:main` would be worse. ember refuses the
*name* `latest`, not the behaviour "this tag moves", so a moving tag under
another name is accepted and silently run — which is the mistake the refusal
exists to prevent, with the one word that triggers it removed. The CUDA
workload images do publish a moving `<tag>-sm<arch>`, but the reference they
hand you carries the digest as well, and the digest is what decides.

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

**The list is checked against the tree, in both directions.** `go test -run`
over an expression naming a test that does not exist prints at most a warning
and exits 0, so a rename used to drop a lab out of the publish gate in
silence and a typo used to drop all of them — with `docker tag` and
`docker push` running afterwards either way.
`TestPublishGateNamesEveryWorkloadImageLab` fails on a name the list has and
the tree does not **and** on a `TestWorkloadImage_` lab the list does not
name, a deliberate omission needing a written reason; and the target itself
refuses to go on when `go test -list` selects fewer tests than the list names,
before it touches Docker.

**One image per architecture, built in parallel.** The workflow is a matrix:
one job per architecture, each compiling for exactly one, so the wall clock is
one compile rather than seven. The architecture list is not written in the
YAML — a `plan` job asks `ember archs --json` for it, because a list in a
workflow that drifts from the list it mirrors is the defect naming the lab set
once exists to prevent. That sentence used to say "the defect `PUBLISH_LABS`
exists to prevent", which was true of two copies becoming one and silent about
the copy that was left: one list, checked against nothing, selecting the labs
that stand between a built image and a registry.

`fail-fast` is off: one architecture failing must not cancel
six that were fine, and a partial set is safe by construction, since ember
refuses a card with no `images:` entry before it rents anything.

A final `report` job prints one pasteable `images:` block and **exits non-zero
naming any architecture that published nothing** — a map missing a key is a
card ember will refuse to rent, so a block pasted over a complete map would
take those cards away silently.

That job is `ember images --from <dir>`: each matrix job writes a fragment
holding one `sm_NN: <reference>` line, and this reads them back and assembles
the map, because Actions does not merge matrix outputs and a single output
would be last-writer-wins. `--archs` says which architectures have to be
present, so a deliberate one-architecture rebuild does not report the other
six as missing; both spellings, `90` or `sm_90`, are accepted. The fragments
are found at any depth under the directory, since `download-artifact` with
`merge-multiple` flattens them and `gh run download` does not.

Dispatch one architecture with `-f archs=89` to republish just that one.

Locally one architecture takes about four minutes on thirty-two threads
against roughly twenty on a four-vCPU runner, and the push is seconds rather
than minutes: the base is pinned by digest, so only the layers that actually
changed go up.

stable-diffusion.cpp rather than ComfyUI or diffusers, deliberately: no Python
and no PyTorch anywhere, and 1.41 GB rather than a dozen. On a rented GPU that
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

**And it is built on `nvidia/cuda:*-base`, not `*-runtime`, for the same
reason.** `-runtime` is that base plus one apt layer holding every CUDA library
NVIDIA publishes, and measured on the `sm_75` image that layer was **1496.7 MB
of a 1724.1 MB pull — 87% of what a node downloads**. What `sd-cli` and
`sd-server` link out of it is cublas, cublasLt and nccl; the other ~1264 MiB
was cufft, cusparse, cusolver, curand, nvrtc, nvJitLink and the NPP family,
none of which appears even as a string in either binary. The image carries
those three by an explicit `COPY` from the stage that compiled against them —
not `apt install`, so the version cannot drift from the one the binary was
linked against — and the pull is **0.83 GB rather than 1.72 GB**, 1.41 GB
rather than 2.75 GB unpacked. `/usr/local/cuda-13.0/compat` is in both bases,
so nothing about PTX forward-compatibility changes in either direction.

A hand-picked library list is not the vendor-supported shape, so
`TestWorkloadImage_LinksNothingItDoesNotShip` runs `ldd` on both binaries in
the image and fails on anything unresolved except `libcuda.so.1`, which the
container runtime injects from the host driver. That is link-time evidence
rather than proof about a run — it cannot see a `dlopen` of a name the string
table never held — and it costs no GPU, no network and no weights, so it runs
on every build and a CUDA bump that renames a soname fails on a laptop instead
of on a rented pod.

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
make reap-labs          # clear every ember-labelled object off the daemon
```

`make test-preview-lab` is the one that watches a generation happen. It starts
`sd-server` inside `ember:sd` — the CPU workload image, so no GPU is involved —
drives it over a real SSH connection through `direct-tcpip`, and asserts on the
progress frames that come back: a 64×64 image in six steps produces seventeen
frames, every step from 1/6 to 6/6, each a decodable PNG. It also checks that
the server binds `127.0.0.1` and nothing else. Twenty-one seconds on a laptop;
build the image first with `make workload-sd MODEL=/path/to/a.gguf`.

It also drives the two ways a served run goes wrong, which is what an operator
reads when it does. A load that cannot succeed exits **before** the port opens,
so `get sd version from file failed` arrives at once rather than after
`ready_timeout`; and a request the server refuses outright — a generation asked
for before the weights are in — now reports the server's own sentence,
`submit job to sd-server: refused with status 503: model_not_loaded: No model
is loaded`, in place of the JSON body it used to print inside a message about
a status code. A job that fails *after* being accepted reports its own code and
message, and the reason behind a generic one is in the server's log, which
ember streams.

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

**One suite at a time per Docker daemon.** A lab's cleanup removes every
ember-labelled container, network and volume the run produced — found by the
label rather than by a list the harness kept, which is what catches the leaked
bridge network nobody thought to look for. Two checkouts on one machine used to
make that a whole-daemon delete and destroyed each other's nodes mid-test, so a
suite now takes an advisory lock on the daemon: a second one waits, says who it
is waiting for, and after ten minutes fails naming the process that is holding
it. Anything ember-labelled that was *already* on the daemon when the lock was
taken is left strictly alone — not counted, not reported, not removed — so a
container you started by hand, or one a `SIGKILL`ed lab left yesterday,
survives the suite. `make reap-labs` is what clears those; it refuses while a
suite holds the lock.

Each lab target tees its output into a directory of its own under `/tmp` and
then greps that log back for `--- SKIP`, so two checkouts must not share one:
`tee` truncates the file as the second run starts, and the first run's gate
would be reading it. The directory is named from a digest of the checkout's own
path — `git clone` calls every clone `ember`, so the name alone is not enough —
and `make print-LAB_LOG_DIR` is how to ask which one is this checkout's.

Go 1.27. The Makefile prefers `~/sdk/go1.27.0` when present so the compiler
cannot drift from what `go.mod` declares.

### What `ember version` reports

`make build` stamps the binary with `git describe --tags --always --abbrev=12
--dirty`:

```
$ make build && ./bin/ember version
0f109263a709-dirty
```

| the tree it was built from | what it prints |
|---|---|
| a release tag | `v1.2.0` |
| three commits past one | `v1.2.0-3-g0f109263a709` |
| **no tags at all** — today's state | `0f109263a709`, the bare revision |
| any of the above, uncommitted changes present | the same with `-dirty` |
| no git: a source tarball, or `go build` outside a checkout | `dev` |

**Nothing invents a tag.** There are no release tags in this repository, so
every build today takes the third row. A string like `v0.0.0-3-g0f10926` would
name a release nobody cut, which is worse than `dev` — the same argument this
project makes for refusing a `:latest` image, turned on itself. Every
measurement here is a claim about what a specific build did, so a version that
lies is worse than one that admits it.

A plain `go build ./cmd/ember` with no `-ldflags` prints the same thing, because
`ember version` falls back to Go's own build information. It reads the recorded
revision rather than the main-module version: the toolchain synthesises
`v0.0.0-20261102192343-0f109263a709+dirty` for a build from a checkout, which is
a real revision wearing the shape of a release at v0.0.0.

`-ldflags -X` is a **silent** no-op when the symbol it names does not exist, so
renaming the variable or moving the package would stop the stamp with no error
from anything. `internal/cli`'s `TestVersionStampTargetsThisPackage` reads the
Makefile and every workflow and fails when what they name is not a string
`var` in that package; a sibling test fails when a workflow builds `ember`
directly rather than through `make build`, because two spellings of one build
command is how one of them ends up without the flags.

## Configuration

`ember.yaml` holds image names, GPU filters, ports and budgets, and is safe to
commit. Credentials are not in it: they come from the environment
(`RUNPOD_API_KEY`, `VAST_API_KEY`) or from `~/.config/ember/credentials.yaml`,
where the same two are spelled `runpod_api_key` and `vast_api_key`, and which
ember refuses to read unless it is mode 0600 — checked on every load,
because a file created correctly once and chmodded later is the common case.
The environment wins over the file, so a one-off run can use another account
and CI needs nothing on disk.

A provider with no credential is left out of the registry rather than
registered and left to fail. `ember ls` exits 2 on a provider it could not
reach, so registering RunPod on a machine that has never had a token would
make every clean run look like a problem.

Nothing secret is ever passed as a command-line flag, because
`/proc/<pid>/cmdline` is world-readable — on a rented machine, readable by
whoever else is on that host, and on this one readable by every other account.
`serve_token`, which is `ember serve`'s own credential, lives in the same file
for that reason and has no flag either.

### The committed configurations

Every command reads `ember.yaml` in the working directory by default, so
`--config` is for naming one of the others rather than for ordinary use. Five
are committed, and they are the four shapes `{this machine, a rented card} ×
{one process per run, one resident server}` plus the starter:

| file | runs on | job shape |
|---|---|---|
| `ember.example.yaml` | a rented 4090 | none — holds the node open with its ports forwarded |
| `ember.flux2.yaml` | a rented 80 GiB card | `cmd:` — one `sd-cli` per `ember up` |
| `ember.sd-server.example.yaml` | the same card and store | `sd:` — one resident `sd-server`, a list of jobs |
| `ember.local.example.yaml` | the GPU in this machine | `cmd:` |
| `ember.local.sd-server.example.yaml` | this machine, free | `sd:` — what `ember serve` needs |

A configuration holds configuration: a comment in one says what a wrong value
costs, in a line or two. Every measurement that chose a value — the store's
$4.90-a-month standing charge, the per-image cost of a resident model against
a fresh process at four batch sizes, the two runs that paid for `gpu_probe` —
is in [the committed configurations](docs/configurations.md), along with the
two shapes that are written down and cannot be run yet because one's image is
a sentence and the other's asset digest is a placeholder.

The durable half of `ember.flux2.yaml` and `ember.sd-server.example.yaml` is
identical — same images, store, assets, probe and ceilings — and that
duplication is not something a config can currently remove: `node.NewSpec`
refuses `cmd`, `cmds` and `sd.serve` together — a run with both would start a
server, hold the weights in VRAM and then run a separate `sd-cli` that loads
them a second time — so one file describes one job shape. See
[#144](https://github.com/Legion112/ember/issues/144).

### What a flag may do that a config key may not

A flag **replaces** the config's value for that run rather than narrowing it.
`ember up --max-hourly-usd 999` *raises* a `max_hourly_usd` of `"1.50"`,
because the person typing it is the person who set the ceiling. Nothing that
takes a value from somewhere else may reuse that rule: a request arriving at
`ember serve` can only ever ask for **less**, and the smaller of the two
values wins. `--provider`, `--image`, `--gpu`, `--ttl` and `--port` replace
the same way, and `--image` also clears an `images:` map, so a one-off pin
cannot be silently ignored on a config that has one.

`ember offers --max-hourly-usd 1.50` is the same flag read through the same
code, and all it does there is hide rows — it rents nothing, which makes it
the cheap way to see what a ceiling would admit before `ember up` runs under
it. A second `offers`-only ceiling would read as the friendlier design and
would let a listing show an offer `up` then refuses, which is the one thing a
preview of a price must not do.

`ember up --cmd` is `cmd:` typed at a command line, **one word per flag, in
order**: `--cmd sd-cli --cmd -p --cmd "a lovely orange cat"`. A single string
would have to be split by a quoting rule ember does not own, and a remote
command is argv. There is no flag for `cmds`.

Those were documented nowhere. `AGENTS.md`'s last line names three things to
document in the commit that changes them — a CLI flag, a config key and a
behaviour — and the config keys have had a guard that reads the tree since
before the section below was written, while the flags had nothing and seven of
them were missing, `--max-hourly-usd` among them.
`internal/cli`'s `TestREADME_DocumentsEveryFlag` now reads every flag out of
`internal/cli`'s own source with `go/parser` and fails on one this document
has never heard of. It fails the other way too, on a flag this document types
at ember that no `flag.FlagSet` declares, because a stale line waives nothing
and hides the next real one: its first run found a `--all` on an `ember ls`
here that has never been a flag.

That sweep corrected this document and `AGENTS.md`, and left **eighteen Go
comments** citing the same invocation as a design precedent, because a guard
that reads documents does not read comments.
`TestComments_CiteNoFlagThatNoFlagSetDeclares` runs the same window over every
comment in the tree against the same declared set: on the tree as it stood it
found 48 flags cited at ember, 30 of them real and all 18 misses that one. It
has one direction only — a comment is not documentation, and nothing obliges
one to name a flag.

### Placement, disk and where ember keeps things

These keys were implemented and documented nowhere, which is the half of
`AGENTS.md`'s "document a config key in the same commit" rule that had been
skipped. `internal/cli`'s `TestREADME_DocumentsEveryConfigKey` reads the
`yaml:` tags out of `internal/config` and fails on a key this document has
never heard of, so the next one cannot arrive silently.

```yaml
# where a node may be placed, and how much disk it gets
data_centers: [US-IL-1, EU-RO-1]   # RunPod only; empty lets RunPod choose
disk_gib: 80                       # RunPod only; the container disk
runpod_cloud: COMMUNITY            # or SECURE, which is RunPod's own default
allow_interruptible: true          # required by COMMUNITY; see below

# where ember keeps its own files; all three are resolved when empty
config_dir: ~/.config/ember        # keys and credentials.yaml
state_dir: ~/.local/state/ember    # node records, the queue, staged inputs
output_dir: ~/renders              # fetched outputs, one directory per run
```

`data_centers` reaches **RunPod only** — the local provider's Docker daemon is
wherever it is, not in a datacentre. **A store overrides it rather than
intersecting with it**: a volume lives in exactly one site and a pod that
mounts it has to be created there, so an intersection could be empty while the
store's own site has capacity.

`disk_gib` also reaches RunPod only, and a value below 1 silently means ember's
default of 50 rather than an error. A CPU-only machine is clamped to 20, which
is a property of the product and not a default that could be lowered for
both — the API answers `Container Disk must be less than or equal to 20` with a
400 above it.

The three directories default to `$XDG_CONFIG_HOME/ember`, `$XDG_STATE_HOME` or
`~/.local/state/ember`, and `<state_dir>/outputs`. Outputs live under the state
directory so a run's results sit beside the journal record of the run that
produced them, and so one `XDG_STATE_HOME` move takes both. The state directory
is deliberately *not* the config one: a node record is a machine-local fact
about a node that is running now, and a config directory synced between
machines would carry records for nodes the other machine cannot see.

**Nothing in there is a host key.** ember mints a host key per node and injects
it through the create call, so the pin is built in memory from a key this
process generated and is gone when the run is — there is no `known_hosts` file
to go stale, to be synced to another machine, or to be edited. A
`<state_dir>/known_hosts.json` used to be written by a code path nothing ever
reached; it is gone, and a file of that name left behind by an older ember is
read by nothing and can be deleted.

`allow_interruptible` bids on hardware the provider can take back mid-run. It
defaults to false, which refuses it — and **`runpod_cloud: COMMUNITY` needs it
set**, because every community pod runs on a machine somebody else owns and can
reclaim. The two keys are one decision spelled in two places: `runpod_cloud`
picks the tier and `allow_interruptible` says whether ember will rent from it.

> **Behaviour change.** Until this was fixed, `runpod_cloud: COMMUNITY` rented
> reclaimable hardware with `allow_interruptible` left at its default. The
> guard read an `Interruptible` field on the offer that **no provider ever
> set** — the only field of that name anything assigned was the provider-level
> `Caps().Interruptible` one layer up, which the filter never consulted — so it
> ran on every offer and refused none. A community-tier config that worked
> before now gets no offers at all until `allow_interruptible: true` is added,
> and the refusal says so by name rather than reading as "that card is sold out
> everywhere". Setting it restores exactly the previous behaviour, with the
> difference that it is now a choice somebody made.

What a reclaim costs is the one failure ember's TTL and journal do not cover:
the node goes away without ember destroying it, mid-generation, and whatever it
had produced that was not fetched yet is gone. A batch brings each result home
as it is produced for the same reason, so the loss is bounded by one step
rather than by the whole run.

An offer that does not say either way counts as reclaimable. That is the whole
shape of the fix rather than a detail: a provider author who forgets to
populate `node.Offer.Reclaim` gets their offers refused, not rented, which is
the direction `Caps.PublicSSH` already defaults in. `provider.Caps` carries a
separate `InterruptibleTier` for a provider whose whole catalogue is
reclaimable — RunPod's community tier is one — and
`providertest.RunConformance` fails a provider whose offers and capability
disagree, or whose offers say nothing at all.

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

**A step's output stays on the node, so a later step can read it.** The fetch
is a read — `tar` streams the files out and nothing removes them, and the
marker bounds only what is *sent* — so the node's filesystem carries every
earlier step's output for the whole rental. A `cmds` entry can therefore name
a file an earlier entry wrote:

```yaml
cmds:
  - [sd-cli, ..., -o, /workspace/out/01.png]
  - [sh, -c, "convert /workspace/out/01.png -resize 50% /workspace/out/01-small.png"]
```

That is documented because configurations may rely on it, not because it is
encouraged. ember still does not read argv, so it does not know that entry 2
depends on entry 1: nothing checks that the filename is right, a typo is found
at step 7 of a rental whose first six steps are paid for, and the dependency
is invisible to every figure ember prints. For a long video — twelve clips,
each continued from the last frame of the one before — those costs dominate
and the chain belongs somewhere ember can read a result. See
[Long video is a pipeline](docs/long-video-is-a-pipeline.md).

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
  preview: proj                   # none, proj, tae or vae
  preview_interval: 5             # sampling steps between frames
  jobs:
    - prompt: "a paper boat on a slow river, morning light"
      negative_prompt: "blurry, watermark"
      width: 1536
      height: 1536
      steps: 28
      batch_count: 2              # images from this one job, same prompt
      sampler: euler
      cfg: 1.0
      guidance: 4.0
      seed: 20261101
      output: boat
    - prompt: "the same river at night"
      output: river-night
```

`batch_count` is checked against the `max_batch_count` the server reports
before anything is submitted, and each image it produces is numbered —
`boat-00.png`, `boat-01.png` — so a `batch_count` of one and a `batch_count` of
four produce the same shape of filename. A batch restarts the sampling pass per
image, so two images at 28 steps is 56 steps of progress reported as two passes
rather than one bar to 56.

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

`width`, `height` and the `hires` target are refused above **8192** on each
axis, and refused for being negative, without asking a server anything. That
is the largest geometry any sd-server here has published —
`limits.max_upscale_width` and `max_upscale_height` — so it cannot refuse a
size a server would have taken, and the per-axis generation maxima
(`limits.max_width`, 4096 on the captured build) are still applied on top of
it the moment a rental has reported them. The bound is there because those
maxima are a *response*: before the first rental of an `ember serve` there was
nothing to check a geometry against, so `POST /api/requests` answered 202 to a
width of three billion, kept the row, and rented a card for a batch sd-server
was always going to refuse.

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
| `strength` | the same, top level |
| `init_image` (a web-UI upload) | `init_image`, as bare base64 of the bytes |
| `output` | nothing — it is the local filename stem |

`cfg` and `guidance` are the two worth knowing: both get called "cfg" in
conversation and they are different fields. The FLUX.2 recipe sets `cfg: 1`
with `guidance: 4`, so setting the wrong one generates at the wrong strength
and nothing reports it.

**What is not typed, and why.** An image input is reachable from the **web
UI** and not from `sd.jobs`: the API takes images as base64 in the request
body, which a YAML file cannot hold, so a file is uploaded to `ember serve`
and the job names its digest. That is `init_image` and `strength`, and
["Generating from an image you upload"](#generating-from-an-image-you-upload)
is the section about it.

The other four image-dependent parameters are still absent, and the reason is
no longer "there is no upload route" — each was sent to a live sd-server and
none of them changed anything that could be observed:

| parameter | what the server did with it |
|---|---|
| `img_cfg` | accepted at `0`, at `-1` and at `99`; echoed at none of them, on a build that publishes `generation_features.img_cfg` as **true** |
| `control_strength` | accepted, echoed nowhere; needs `control_image` **and** a ControlNet in the serve argv |
| `ip_adapter_strength` | accepted, echoed nowhere; needs `ip_adapter_image` **and** an IP-Adapter model |
| `increase_ref_index` | accepted and echoed as `ref_image_args: ref_index_mode=increase`; needs `ref_images`, which is an edit model's input |

So each of them would be a control that takes a value, sends it and changes no
pixel, with no flag ember could read to say so in advance. `strength` is the
one with the opposite evidence: the server echoes
`generation_info.strength` back, including an explicit `0`, and the two arms
of a measurement with and without an init image produce different images.
Inpainting (`mask_image`) is absent because it unblocks none of these — a mask
is a feature of its own rather than a parameter, and nothing in the list above
needs one.

`cache_mode`, `cache_option`, `scm_mask`, `scm_policy_dynamic`,
`qwen_image_layers` and the two `custom_sigmas` are untyped because nothing
here knows what values they accept.

`lora` is not typed either, and that one is no longer a question about safety.
`lora[].path` never reaches a path join: the server matches it for equality
against the ids it published in `capabilities.loras[]`, and anything else —
`../..`, an absolute path, even the published id with a `./` in front of it —
is refused with a 400 before the generation starts. `AGENTS.md` has the
upstream lines and the lab that measured it. So a `lora` field is a
`slices.Contains` against that published list, the same three lines
`hires.upscaler` already has; it is not built yet because this change's
deliverable was the answer rather than the control.

**What that allow-list would not buy, which matters more than what it
would.** A published id whose file sd-server cannot read — a truncated
download, a quantisation the build does not support, a half-finished copy —
is a *warning*, not a refusal. The generation runs, the image comes back, the
rental is billed, and nothing in the exchange says the LoRA was skipped: the
job is `completed` with `error: null`, the pixels are exactly those of the
same prompt and seed with no LoRA at all, and the one field that mentions it
— `generation_info.loras[]` — is the request echoed back, so it asserts the
LoRA was used. The PNG goes further and writes `<lora:name:1.0>` into its own
`parameters` metadata, which is what a gallery will show you about an image
generated without it. The server's only account of the failure is a
`cannot register LoRA source` line on its stderr, which ember prints live and
does not record. This is reachable from a config today — `extra` carries a
`lora` array to the server, as the example below does — and the measurement
is in `AGENTS.md` beside the asset-verification rules, with why ember does
not read that log.

**What is greyed out, and why.** The seven advanced controls that have a
feature flag in one of the server's two maps — the image input, clip skip,
flow shift, shifted timestep, skip-layer guidance, vae tiling and the hires
pass — are visibly unavailable in the web UI when the loaded model does not
take them, and refused at enqueue rather than after a card is rented. A flag
the server did not publish gates nothing: ember tells "the server says no"
apart from "the server said nothing", because collapsing the two would grey
every one of these against any build that does not carry the maps.

`eta` is the one with no flag in either map, so ember has no advance answer
about it and does not invent one. The UI says which controls this model denied,
in words, rather than leaving it to be inferred from which boxes are grey.

In practice nothing is greyed on a server `ember up` or `ember serve` starts:
ember hands the weights over as argv, which upstream treats as a model
declaring every feature. The flags matter when a server is loaded some other
way, and the point of reading them is that ember reports the server's answer
instead of assuming it.

All of those remain reachable from a config: `extra` reaches every field of
the API ember does not type, including nested ones, so nothing is out of
reach:

```yaml
    - prompt: "a cat"
      extra:
        lora: [{ path: "detail", multiplier: 0.8 }]
        sample_params:
          custom_sigmas: [14.6, 10.0, 0.0]
          guidance: { img_cfg: 1.5 }
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

**Previews cost GPU time, not just bandwidth.** ember defaults to `preview:
proj` with `preview_interval: 5`. Upstream's default is a frame every single
step, and the `tae` and `vae` modes run a decode to produce each one — so the
upstream default is the expensive one, billed by the second, for frames nobody
in an unattended batch is watching. `preview: none` is the only free setting. A
negative `preview_interval` is refused rather than clamped, because upstream
clamps a non-positive value to 1: writing `-1` to mean "off" would buy the most
expensive setting there is.

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
ember up --config ember.local.example.yaml   # same assets, same digests, free
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

The `nvidia-smi` probe behind that runs once per process and its answer is
remembered, because `ember offers` and every rental ask for the listing again
and the card does not come and go. What is remembered is only an answer:
"there is no card" is one, and "the probe could not run" is not. That
distinction is `ember serve`'s, where the listing is served on a browser
request's own context — before it, one reload that abandoned the first
`/api/offers` poll cached "this machine has no GPU" for the life of the
server, so the card disappeared from the listing an operator picks hardware
from, `--gpu` read as sold out everywhere, and the
`nvidia-container-toolkit` explanation could not be printed because it is
only offered for a card that was found. A listing nobody waited for now
re-probes on the next one instead.

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

### There is no `min_cuda`

**`min_cuda` was a config key and is not one any more.** A config that still
carries the line is refused by name, with the remedy below, rather than having
the value quietly dropped. Delete the line.

It was added from RunPod's published schema, as `allowedCudaVersions`. The
wrong schema: `rest.runpod.io/v1/pods` accepts that field, and
`api.runpod.io/v2/pods` — which is where ember creates — declares eighteen
properties with `unevaluatedProperties: false` and has no CUDA field at all.
Every create with `min_cuda` set died at

```
status 422: additional properties 'allowedCudaVersions' not allowed
```

Filtering offers instead is not available either, and this section used to
give the wrong reason: it said the live catalogue does not return
`cudaVersions`. It does — the captured catalogue in
`internal/provider/runpod/testdata/gpus.json` carries it on three of its four
entries, and it had been committed for two days when that sentence was
written.

What the field cannot be is a floor. `available` tracks whether the card has
capacity rather than what a host can run: the captured RTX 4090 reads `NONE`
overall, `LOW` in `US-IL-1` — rentable — and reports **all eight** of its CUDA
versions unavailable, so a floor built on it would refuse that card for every
value of `min_cuda`, including values the card satisfies. Read without the
flag, the versions describe the catalogue and not the host the pod lands on,
and upstream describes them as suitable for the one create property that
answers 422. The field also sits on the GPU type rather than per datacentre,
so it cannot be re-read through a store's site the way availability is.

So the constraint cannot be expressed at create time or at selection time.
What the key did instead was refuse on RunPod and do **nothing at all** on the
local provider, which has no CUDA version to compare against — one documented
key, two different meanings, and silence on the one an operator is most likely
to try first. A key that cannot be honoured anywhere is worse than no key, so
it is gone and the refusal moved up to where the config is read, which is
before any provider is chosen.

This is the second time RunPod's two APIs have disagreed about a field; the
first was `dataCenter` against `dataCenterId` on network volumes.

**`gpu_probe` is what enforces a driver floor.** It runs on the node before
the workload and costs the probe rather than the run. In practice the floor is
not tight anyway: a rented RTX 4090 reported driver 595.91.07 with
`CUDA Version: 13.2`, ahead of the 13.0.1 the images are built against — which
is also the number a node-side floor would have to be compared against.

One is not built, and the reason is a reading rather than a measurement, which
is worth separating. **Measured:** `nvidia-smi --version` and `nvidia-smi -q`
both report a `CUDA Version`, so the number exists and ember could read it
(`--query-gpu` does not offer it — that one gives `compute_cap`, a compute
capability and a different number). **Read from NVIDIA's published
documentation and observed nowhere in this repository:** that CUDA
minor-version compatibility lets an image built against 12.6 run on a 12.x
driver older than 12.6 — which, if it holds, means comparing the two as
versions would refuse hosts the image works on. Nothing here has watched that
hold or fail, and settling it needs a rental on a host whose driver is older
than the image, which is not something a create call can ask for.

The conclusion is the same either way, which is why the floor is not built on
either reading: if the compatibility guarantee holds, a version comparison
over-refuses; if it does not, there is still no observation of the failure to
calibrate one against, and the figure is read on the node after the pod is
already billing. So the probe is the honest mechanism: it asks the card to do
the thing, rather than asking a number.

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

`ember archs --only 90,sm_89` narrows both of those to a subset, which is what
a rebuild of one architecture passes. It refuses a number no card ember can
select needs, naming the set it could have been, rather than buying twenty
minutes of `nvcc` and an image nothing will ever pull.

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

#### The weights are checked before a card is chosen

An asset that is missing used to be discovered **on the node**, after the
machine had been created, the image pulled and the store mounted — so the card
spent its first minutes downloading, billed at the card's rate. Measured on
2026-11-03: an RTX PRO 6000 at $2.49/hr spent 14m47s of a 15.1-minute rental
pulling 56.16 GiB at 68 MB/s, GPU at 0% throughout. 97.9% of the bill.

So the question is now asked before anything commits to hardware — one `HEAD`
per asset over the store's own S3 endpoint, which rents nothing and downloads
nothing. What happens next depends on who is asking:

- **`ember up` refuses**, naming `ember store sync`. An interactive command
  should not go and rent a second machine you did not ask for.
- **`ember serve` fills the store first**, on a machine with no card, and then
  rents the GPU. A queue drains unattended, which is both the case this is
  worth money in and the case where nobody is watching to type the command.
  That rental is gated by `--allow-paid` and projected against `max_day_usd`
  and `max_month_usd` like any other.

That second rental is **its own row** in the generation history, so
`ember history` lists it and `ember spend` counts it. It is a rental ember
chose to make with nobody at the keyboard, which is exactly the spending a
ledger exists for — an operator reconciling an invoice would otherwise find a
line nothing here could account for. It shows up as a run with no card, no
jobs and a dash for images and card time, which is the same shape a `cmds` run
has, and it is counted once on its own row rather than folded into the cost of
the request that triggered it: that column is one rental's cost, and the
requests after the first get the filled store for free. Nothing records which
request caused it. `ember store sync` is recorded the same way, because it is
the same rental typed by hand — its output is byte-for-byte what it was when
the history opens, and it says so on the one line it prints when it cannot.
A planner with no generation history to write to refuses to fill a store
rather than renting a machine it cannot account for, which `ember serve`
cannot reach: it always wires one, so that refusal guards the shape rather
than anything an operator can type.

It does not check digests, for the reason `ember store verify` does not: that
would mean reading every object back through this process to re-derive a number
the node derives anyway, at datacentre bandwidth, before the workload starts.
This answers whether the files are there; the node still answers whether they
are the right bytes, and a mismatch is still a refusal. An object present at
**zero bytes** counts as missing — that is what a PUT which failed after
creating the key leaves behind.

The check is skipped, with a line saying so, when it cannot be made: RunPod
issues S3 keys separately from the API key, so an account with one and not the
other keeps working exactly as before, and the local provider has no S3
endpoint at all because a Docker volume is not in a datacentre.

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

`ember store push --as flux2.gguf ./flux2-dev-Q8_0.gguf` chooses what the file
is called on the store, which is the name `assets:` then has to use; the
default is the file's own basename. The name is checked against the same rule
a node applies to an asset, so one a node would refuse fails here instead of
after forty gigabytes have gone up. Flags come before the file, as in every
`store` verb, and a file named after one of them is a usage error rather than
a push of something else.

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

`ember store sync --vcpu 8` rents more cores than the two every family's
minimum gives you. Two is the default because the job is bound by the network
and the disk, and a core that waits on them is a core paid for nothing —
RunPod prices a CPU machine per core, so eight of them is four times the
$0.06/hour above, and a value past a family's maximum drops that family from
the offers rather than being clamped into it.

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

### Which site will still have the card next month

A store makes the datacentre a commitment. `volumeId` is immutable after
create, the volume lives in exactly one site, and every node that mounts it is
placed there — so the question stops being "where can I rent a 4090 today" and
becomes "which site will reliably have one for as long as the weights are
there". Moving later means a second store somewhere else and re-uploading: 56
GiB at this link's measured 67 Mbps is 1h52m.

RunPod cannot answer it. `availability` is a bare `HIGH|MEDIUM|LOW|NONE` on a
card and on each of its datacentres, with **no timestamp and no window**;
both live OpenAPI documents were checked and neither has anything historical.
Every read is a point sample, and nothing on the provider's side remembers the
previous one. **This is the only data in ember that cannot be collected
later.**

```bash
ember catalogue snapshot
```

```
observed 2026-11-02T10:00:00Z
  runpod     3 gpu row(s), 1 cpu row(s), 2 site(s), 1 type(s) with no site named
appended 4 row(s) to ~/.local/state/ember/catalogue/2026-11-02.jsonl

This is the first snapshot here, and its whole value is the series: one
sample answers nothing a live `ember offers` does not. Schedule it hourly --
cron or a systemd timer, see README -- because an hour nobody sampled cannot
be sampled later.
```

Two free GETs — `/v2/catalog/gpus` and `/v2/catalog/cpus`, both with
`include=AVAILABILITY&product=POD`. No pod, no GPU, nothing billed. It writes
**one row per type per datacentre** into an append-only JSON Lines file, one
file per day, and the only field RunPod does not supply is `observed_at`.

**Per site, never the global summary.** The live catalogue has reported an RTX
4090 as `NONE` overall while its `US-IL-1` entry read `LOW` — the observation
`hasCapacity` exists for. A collector that stored the summary would record
"unavailable" for cards that were rentable all along, which is worse than no
data: it is data that looks like an answer. The summary is kept beside each
site's level precisely so the report can say how often the two disagree.

```bash
ember catalogue report
```

```
  site           samples  rentable  high   med   low  none  unseen  masked  secure         community

cpu  cpu3c  (Compute-Optimized)
  EU-RO-1              3      100%     3     0     0     0       0       0  $0.03/hr       -

gpu  AMD Instinct MI300X OAM  (MI300X, 192 GiB)
  (no site)*           3        0%     0     0     0     3       0       0  $2.39/hr       -

gpu  NVIDIA GeForce RTX 4090  (RTX 4090, 24 GiB)
  EU-RO-1              3      100%     3     0     0     0       0       3  $0.89/hr       $0.34/hr
  US-IL-1              3      100%     0     0     3     0       0       3  $0.89/hr       $0.34/hr

* the provider named no datacentre for this type in those samples, so the
  levels shown are its overall summary -- the only figure it gave.

12 sample(s) in 4 row(s) over 1 day file, 2026-11-02T10:00:00Z to 2026-11-02T12:00:00Z.
The series covers 2 hour(s). Availability moves on the scale of a day, so this is
closer to a point sample than to a history -- it becomes an answer after weeks.
```

That is real output, from three hourly snapshots against a catalogue shaped
like the captured one — the levels are a real response rather than this hour's.
`masked` is the column the summary is stored for: three samples where the
card's overall figure said `NONE` while the site had capacity. `unseen` counts
samples where the provider gave no level at all, which is **not** the same as
`NONE` — a site that reported nothing prints `-` in `rentable` rather than 0%,
because 0% means "never available" and would send an operator to move a store
over an absent field.

`--gpu` matches as `ember up --gpu` does, so `--gpu h100` finds
`NVIDIA H100 80GB HBM3` and `--gpu a100` does not match an A1000. `--kind
gpu|cpu`, `--since`/`--until` (UTC dates, half-open as `ember spend`'s period
is) and `--json` narrow it further. Exit 2 means the reader found lines it
could not place or files it did not read — a series quietly missing part of
itself is the clean-bill-of-health failure `ember ls` exits non-zero to
avoid, over a file nothing can rebuild.

**Scheduled from outside ember.** A process that must stay running to collect
data is a process that will be found stopped, so there is no daemon: hourly is
plenty, because capacity is four levels rather than a number and sampling
faster than it can visibly change buys nothing.

```
# crontab -e
17 * * * * cd ~/work && /usr/local/bin/ember catalogue snapshot >> ~/.local/state/ember/catalogue.log 2>&1
```

```ini
# ~/.config/systemd/user/ember-catalogue.service
[Service]
Type=oneshot
WorkingDirectory=%h/work
ExecStart=/usr/local/bin/ember catalogue snapshot

# ~/.config/systemd/user/ember-catalogue.timer
[Timer]
OnCalendar=hourly
Persistent=true
[Install]
WantedBy=timers.target
```

```bash
systemctl --user enable --now ember-catalogue.timer
```

The file is append-only and nothing in ember rewrites it: the value is
entirely in the series, and a format that can rewrite history is one that can
lose it to a bug in the writer. A day's rows go out in one write, because two
embers appending at once — an hourly timer and a run by hand — must not
interleave half a line into the one file that cannot be regenerated. Removing
old days is `rm`, where the operator can see what they are destroying.

The capability is RunPod-specific on purpose: `provider.CatalogueAPI` is an
optional interface gated on `Caps.Catalogue`, in the shape `StoreAPI` and
`LeftoverAPI` already use, and its row type lives in `internal/catalogue`
rather than in `internal/node` because two cloud tiers with separate prices
and separate per-pod ceilings is this provider's product and not the domain. A
closed interface invented from one example is a mistake this repository has
named twice.

## The web UI

`ember serve` runs a local web UI and a queue that drains onto **one rental**.
It is the `sd.jobs` shape driven from a browser: pick a provider and a card
from live offers, type a prompt and its parameters, push a batch, and watch the
sampling steps and preview frames arrive while the images land on disk.

There are five panels: four in the order the flow runs — what can be rented,
what to generate, what is queued, what is happening — and a fifth,
[what is billing](#what-the-billing-panel-shows), which is `ember ls` in a
browser and is asked when pressed rather than on a timer.

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

### What the offers panel shows

Pressing **what is available** shows what each provider answered to *that*
press. Narrowing the card filter and pressing again replaces the table with
the new answer — `local has nothing matching` when the filter excludes
everything it has, and `runpod could not be asked: <why>` when the provider
could not be reached at all.

It used to show the **first** answer for ever. The provider blocks were a
keyed list whose key was the provider's name, which does not change between
two queries of one provider, so every press after the first was discarded by
the renderer: a filter narrowed to a card nobody has went on showing the card
from the press before it, and a provider that answered nothing but a `dial
tcp: no such host` went on showing an H100 at $2.49 in US-KS-2 with no error
anywhere. The one control that varies between two presses is the card filter,
and that is exactly the case the key could not refresh — changing the provider
clears the list, which is why this was invisible.

### What the watcher says about money

The progress panel's money row is the **rental**, and it says which of five
states that rental is in rather than printing a number that is not yet one:

| on screen | what it means |
|---|---|
| *(no row)* | this request has not run, so there is no rental to price |
| `not priced yet` | it opened a rental that is still up; a rental's cost is recorded when its node is destroyed |
| `accruing` | the same, and card time has already been charged to it, so the rate is above zero |
| `—` | it generated on a rental **another request opened**, and that request is the one charged for it |
| `$0.0478` | the rental was torn down and this is what it cost; `$0` here means free and says so |

The row used to be captioned *card time*, gated on the card time, and display
`requests.cost` — which is the whole rental, written once from the deferred
teardown and only to the request that **opened** the rental. So it appeared at
the first completed job and read `$0` for the rest of the generation, and `$0`
for ever on a request that rode an existing rental. A money row of zeros reads
as free, which is the one direction it must never be wrong in, and
`ember history`'s `rode along` column exists for exactly that reason. Which
state it is in is decided by asking **who was told the rental's facts** — a
`node_ready` reaching this request's stream is the same routing decision that
decides which request is charged — and never by the zero itself, because the
local provider's rentals really are free and cost alone cannot tell "somebody
else paid" from "nobody did".

There is **no dollar figure for card time on this page**, and that is a limit
rather than an omission. Money crosses the wire twice — the integer to compare
on and the string Go rendered — so that the browser is never a second
formatter of it, and nothing on the browser's wire carries a rendered
card-time figure: `JobRun.Cost` is kept off the posted `Job` on purpose, so
the figure an operator is billed against is not settable by the thing being
billed. [`ember history`](#which-run-was-it) is where that column lives.

### What the queue table's price column shows

The queue panel's price column is the **rental**, charged to the request that
opened it. A blank cell means there is no figure for that row — a request that
has not run, one that rode a rental somebody else opened, or a rental that was
genuinely free — and a figure appears there when the rental is torn down and
its cost is recorded, **without reloading the page**.

It used to appear only on a reload, and for most sessions that meant never.
The rows were keyed on `(id, status)`, which is right for the status — the key
moves when the status does, so the row is rebuilt — and wrong for the money,
because `requests.cost` is written *after* the status is terminal: the rental's
cost comes from the deferred teardown, which runs once the request has already
ended. So the key `(id, "done")` was created with a zero and never changed
again, the two-second poll re-fetched the right figure hundreds of times, and
the row would not take it. A rider's zero is blank by design, so the opener's
cell is the only place in that table a paid rental appears — which made an
hour of an H100 show up as a queue of finished requests with no money in it.

### What the billing panel shows

Pressing **check what is running** asks every configured provider what is
wearing ember's label and prices the stores. It is what
`ember ls` prints, in a browser: the same sweep, the same
classifier, the same words — `expired`, `orphan`, `untracked`,
`destroy-failed`, `unreachable`, `leftover`, `store` — so an operator
comparing the page and the command reads one vocabulary rather than two.

It answers on a **press and never on a timer**, which is the one thing worth
knowing before using it. The queue table beside it polls SQLite four times a
minute; this reaches the providers — three sweeps per press, one for nodes, one
for stores and one for what a destroyed node left behind — so a poll here would
be an API call a second for every open tab. It also caches nothing: a cached
listing of what is billing is a stale bill, and a cached "RunPod said nothing"
is exactly `I could not check RunPod` rendering as a clean bill of health.

Nothing is shown until that press. An empty panel means nobody has asked; the
sentence `nothing is running and nothing was left behind` means ember asked and
got that answer. A provider it could not reach is named, and the verdict then
says *so this is not the whole picture* — the same distinction `ember ls` makes
by exiting non-zero rather than printing a clean bill.

Two of the rows are the point of the panel. A **store** bills by the
gibibyte-month whether or not anything is attached to it: there is no deadline,
no reaper and no Ctrl-C that ends it, so the run where every node is correctly
destroyed is exactly the run in which a forgotten store is invisible, and the
total is printed under the table. A **leftover** is a bridge network whose node
is gone; nothing bills for one, and what it costs is every local run failing at
once, weeks later, with `all predefined address pools have been fully
subnetted`. Every remedy is a command — `ember reap`, `ember down`,
`ember store rm` — and there are deliberately no buttons here: a destroy from a
browser is a side effect this server's threat model has never argued for.

`GET /api/nodes` is the route behind it, and until now it answered **501** in
every `ember serve` there has ever been. The handler, the projection, the
monthly storage total and the attention count were all written and all
correct; nothing assigned the scanner they needed, no Rust type existed for
the answer, so nobody had ever seen the refusal. An operator who only opened
the browser had no path to the one charge nothing ever ends.

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

Each envelope carries the events **and** the request, and it is written
whenever either has changed. Both halves matter: the request on its own would
re-send four times a second, and the events on their own miss the one state
with nothing behind them — a request refused before a provider was reached
writes its row and emits no event at all, so a tab watching it used to sit on
whatever the socket said when it connected.

**A watcher whose stream ends reconnects, from the sequence number it had
reached.** The page says so while it is trying — one line under *what is
happening*, saying the step on screen is the last one that arrived — and the
line goes away when the socket is back. The cases are an `ember serve` that
was restarted, a laptop that slept, and a forwarded port that blipped; in all
three the generation may well still be running and billing, so a watcher that
simply froze was a picture of a generation captioned as though the card were
sampling. The retry is **bounded** at about forty seconds of backoff —
long enough to outlast a restart typed by hand, short enough that a tab left
open against a server that has gone says it is not coming back and that
reloading is what fixes it. A request that has already ended is not retried at
all: everything that was going to be said has been. Resuming is safe to
overlap, because folding an event is idempotent on its sequence number, and
`ember serve --log-level debug` records the cursor each watcher resumed from —
the one thing about a reconnect that is otherwise invisible from either end.

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

**Every request needs a token**, and there is no flag for it — see
[What is guarded, and what is not](#what-is-guarded-and-what-is-not). It comes
from `EMBER_SERVE_TOKEN` or `serve_token` in `credentials.yaml`, and otherwise
ember mints one and prints the URL that carries it.

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
| `max_hourly_usd` and `max_run_usd`, up to the config's | `ports:`, `max_day_usd` and `max_month_usd` |
| prompt, size, steps, batch count, seed, sampler, scheduler, cfg, guidance, output name | `sd.serve`, `gpu_probe`, `assets:`, `verify:` |
| clip skip, eta, flow shift, shifted timestep, output format and compression | `extra` |
| the `hires`, `vae_tiling` and `slg` blocks | `lora` |
| an image it has already uploaded, by digest, and its `strength` | |
| preview mode and interval | |

**A request that failed says why, in the row and in the watcher.** The reason
is already a column — `requests.err`, written whether the refusal came before
anything was rented (a money ceiling, no capacity, a sampler the cached
capabilities do not list) or on a card that broke — and it has been on the wire
to the browser on every route and the socket since the API was written. Nothing
read it: the whole operator-visible state of a failed generation was the word
`failed` in one table cell. `ember history` printed the reason and the page did
not, which is the one place a document here claimed something the code did not
do.

The queue gives it **a row of its own** across the whole table and the watcher
prints it as a line of its own, and **neither of them is the destroy warning.**
"The node may still be running and billing — check `ember ls`" belongs to a
destroy that failed and to nothing else: a request a ceiling refused never had a
node, and sending an operator to check their bill for a machine that never
existed is a false alarm on the one page they would read to decide. The same
reason the sweep answers "not abandoned" for everything it cannot be sure of.

**A row of its own, because a cell is not a place to put a sentence.** It used
to render inside the status cell — one column of a six-column table, in the
narrower half of the page's two-column grid. Measured at a 1280px viewport: the
table is 407px, that cell gets 174px, and the store refusal — 170 characters
naming `ember store create` — came out 174px tall, nine lines of two or three
words. Five failed requests filled the screen and the queue stopped being
readable. Across the table the same sentence is four lines. Nothing is
truncated and nothing is hidden behind a click: the sentence is the server's
own and carries the command that fixes it, so an operator who does not know to
click would be back to reading `failed` and nothing else.

Ember's own line breaks survive too. `app.AttachStore` writes the remediation
on its own indented line, and HTML collapses a run of whitespace — so `ember
store create` used to run together with the prose on both sides of it. The
reason is rendered as the server wrote it.

**A request the page cannot read is named, and does not blank the table.** The
queue listing crosses a language: a number is a Go `int` on the way out and an
`i32` on the way in, so the server can say something the browser cannot hear —
and one such row used to fail the whole envelope, taking every healthy request
with it. The table then kept whatever it last showed, which on a fresh page is
`nothing queued`, for the life of the tab, and nothing anywhere said why. The
listing decodes row by row now: a row that will not decode is counted and
printed under the table, and a listing that could not be fetched at all says
the table is stale rather than leaving it to be believed. `sdserver.MaxAxis`
above is the other half — the server stops sending the value — and neither
half covers what the other does, because only four of those fields have a
published maximum to bound them by.

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

The image is the one line of that table that moved, and what a request may name
is a **digest** — sixty-four lowercase hex characters, or it is refused. Not a
path, not a filename, not a URL. The next section is what that buys and what it
does not.

### Generating from an image you upload

`ember serve` takes a png or a jpeg from the compose panel's file picker and a
job generates from it instead of from noise. `strength` is how much of it
survives: `0` keeps the image, `1` replaces it, and leaving the box empty uses
the server's own default of `0.75`. An explicit `0` and an empty box are
different requests, so ember sends them differently.

It is the only inbound byte path in ember, so it is worth saying exactly what
happens to a file:

1. The browser posts the **bytes** to `POST /api/inputs` — no form, no
   filename, no declared type. Authenticated and Origin-checked like every
   other route: an ungated upload would be a loopback port on which any other
   account on the machine could write files into your state directory.
2. ember bounds the body at **8 MiB** and **refuses** past it rather than
   truncating — a truncated image whose header survived still decodes, so a
   silent cut would stage something that is not the file you picked.
3. It reads the header with the standard library's `image.DecodeConfig`: a png
   or a jpeg, with each axis between 1 and **8192**, or it is refused by name.
   The axis bound is not a size limit — a two-kilobyte png can declare
   100000x100000, and the thing that would then allocate four bytes per pixel
   is sd-server, on a rented machine.
4. It keys the file by the **sha256 of its content**, under
   `<state_dir>/inputs`. The filename from your disk is discarded rather than
   sanitised: a name that never reaches a path cannot be a traversal.
5. The job carries that digest. Loading it re-hashes what it read and refuses a
   mismatch, so the reference is a claim about content that is checked rather
   than trusted — and it is checked **three times**: at enqueue, again before
   anything is rented, and again when the bytes are sent.

```
$ curl -s -H "Authorization: Bearer $EMBER_SERVE_TOKEN"     --data-binary @boat.png http://127.0.0.1:7777/api/inputs
{"id":"sha256:4f1c...","format":"png","width":1024,"height":768,"bytes":418233}
```

**Inputs live under the state directory and not the outputs**, which is a
decision about lifetime. An uploaded image is an argument to a run rather than
something a run produced, so it is not a generated image that
`ember history prune` is free to remove. What cleans it up instead: every
upload sweeps staged files that **no unfinished request names** and that are
more than a day old. The age bound is what stops a sweep deleting the file you
picked a second ago, and it is the same rule `ember store abort --older-than`
follows — an upload in progress and an abandoned one look the same from here.
The thing that creates the garbage is the thing that removes it, so the
directory is bounded by one session's uploads rather than by all of history.

**The server resizes it.** A 1024x768 input asked for at 512x512 comes back
512x512; there is no need to match the geometry. And `strength` is refused
without an image, because a live sd-server accepts that combination, generates,
and reports the strength nowhere — which reads as working.

The whole path is a web-UI feature: `sd.jobs` in `ember.yaml` has no image
field and `ember up` has no staging directory, so the config path cannot name
one at all.

### What bounds a queue

`max_hourly_usd` bounds a card. `max_run_usd` bounds a rental. Nothing bounded
a **sequence** of rentals, and `ember serve` is the first thing here that can
produce one without the operator being at the keyboard for each: thirty queued
requests at $1.79/hour on a one-hour TTL are inside every ceiling ember had,
and are $54.

```yaml
max_day_usd: "20.00"
max_month_usd: "250.00"
```

Both are UTC calendar periods, and a rental is refused when the offer's price
over the TTL would take the period past its ceiling — the same arithmetic
`max_run_usd` is compared against, at the same moment, after the offer is
chosen and before anything is created. **They bound every rental ember makes**:
a queued request, the machine with no card `ember serve` rents to fill an empty
store, a run submitted to a daemon, `ember up` in your own shell, and
`ember store sync`. `ember serve` prints where both stand at startup, and a
refused request carries the reason, so it is in the page and in
`ember history`:

```
the month beginning 2026-11 has spent $212.3 of its max_month_usd ceiling of
$250, and this rental is projected at $38.7 -- which would take it to $251.
Nothing was rented. Raise max_month_usd in ember.yaml, or wait for the month
to turn over
```

This is **not** the mid-run meter that was asked for and refused. That one
needed a clock, a price and a running total kept correct *while* a rental is in
flight. This needs the completed cost of finished rentals, which is already
recorded, numbered and durable — one more `SUM` over rows that exist.

Five decisions, with what each costs.

**Calendar, not rolling**, because the thing being defended against is an
invoice and an invoice is calendar. The cost is a real hole: a day ceiling
resets at midnight UTC, so a queue can spend one day's allowance twice in two
hours across the boundary. What bounds that is the month ceiling, which is why
both exist and why the month is the one to set if you set only one.

**It projects rather than reports.** A check that refuses only once a period has
already been exceeded is a ceiling every period breaches by one rental, and one
rental is the $54 above.

**It refuses rather than quietly renting something cheaper.** Narrowing the run
ceiling to the remaining headroom was the first design, and it would have meant
asking for an H100 and getting a 4090 because the month was nearly spent. ember
exists to measure what a card costs, and a run that silently happened on
different hardware is a measurement nobody can use.

**It counts rentals and not store standing charges.** A store bills by the
gigabyte-month whether or not anything runs, so a ceiling that ignores a
$40/month store is wrong by a known amount. Counting it would mean a provider
scan inside the drain loop, and a provider that could not be reached would
leave two choices: block the queue, which is worse than the gap, or carry on
without that store's charge, which is the gap with a false sense that it was
covered. `ember ls` is where a standing charge is reported, and it exits
non-zero over a provider it could not ask.

**It bites at selection and not at enqueue.** Refusing when a request is
queued would be kinder and is not sound in the direction that matters: it is a
*false* refusal across a period boundary, because work queued on the 30th for
next month would be rejected although the queue would have run it after the
turnover. A ceiling reached by something else while a request sat in the queue
is the other half of the same argument. The refusal reaches the operator
anyway — the request is marked failed with the reason.

**They bound `ember up` and `ember store sync` too, and that is a change.**
This section used to end "`ember up` does not read these at all — it is one
rental the operator started themselves". It read them nowhere, and nor did a
run submitted to a daemon: five paths write the cost column these totals are
summed from and only two were projected against a ceiling, so the key that
says "spend no more than $20 today" bounded the web UI's queue and nothing
else. There is no flag for either to go beside `--max-hourly-usd`,
deliberately: unlike the hourly ceiling these are not a
property of one rental, so a flag that raised one would be a flag that spent
another command's allowance. Edit the file, or point `--config` at a different
one.

What that means in practice, for the two commands that used to be unbounded:

- **A refusal where there used to be a rental.** `ember up` with a
  `max_month_usd` your month has nearly spent stops before anything is
  created, with the same message the queue gets, and exits non-zero. `--dry-run`
  is never refused, because it rents nothing.
- **A ceiling it cannot check is a refusal, not a pass — for a rental it could
  refuse.** Recording a run in the generation history is best-effort: a
  database that will not open prints one line and the run goes ahead. A
  *configured* ceiling with no readable history refuses, because "I could not
  read what has been spent" is not "it is allowed" — the same rule makes
  `ember ls` exit non-zero over a provider it could not ask. But a **free**
  rental is exempt from both: it projects nothing, so there is no total a
  missing history could hide.
- **`max_day_usd: "banana"` is now an error.** It was accepted and ignored on
  these paths, because nothing between loading the file and renting read the
  key.

One thing they still do not do. **A free rental is never refused**: the local
provider's price is zero, so its projection is zero and it adds nothing to any
period, which is most of what this repository measures with.

That sentence was true of the design and **false of the code** in the first
version of this, which is worth saying rather than quietly correcting. The
refusal above asked only whether a ceiling was *configured* and never whether
the rental could breach one, so `ember up --provider local` with `max_month_usd`
set and an unreadable history was refused outright — against this paragraph,
against the comment four lines above the branch, and against a test that
asserted it. Review found it. The question asked now is "could this rental be
refused at all", in one place that both no-history refusals share.

What it reads is complete, which is worth saying because `ember spend` has to
warn that its per-job figures are not. `requests.cost` was a column before the
execution columns existed, so the period total has no gap in it.

### How a ceiling is read

Every `*_usd` key is a decimal string read into integer microdollars without
going through a float, because `ParseFloat("2.01")*1e6` is 2009999.9999999998
and a ceiling that is occasionally wrong by a rounding error is a ceiling
nobody trusts. What that accepts: a leading `$` with or without a space after
it, surrounding whitespace, a leading `.` (`".5"`), leading zeros, and up to
six fractional digits. A seventh digit and beyond are **truncated**, because
providers publish prices like `0.4290000000000001` after a float round-trip of
their own.

What it refuses, each with the reason in the error: an empty string, a
negative (a negative price is a field ember has misread, not a discount), an
exponent (`"1e3"`), a thousands separator (`"1,000"`), a sign (`"+1"`), a
trailing point (`"5."`), anything that is not an ASCII digit, and a space
inside the number.

A refusal names the key it came from and quotes the value exactly as you typed
it, and for every spelling refused for its shape the message carries the
accepted form rather than only the word "malformed":
`max_hourly_usd: malformed price "$1,000": write a plain decimal number of
dollars, optionally after a $ -- 1.79, 0.429, $2.49 -- with no thousands
separator, sign, exponent, …`. A whole part above 922337203685 dollars is
refused as out of range, and the message states that bound.

It also refuses a **positive amount below one microdollar** — `"0.0000001"`
and anything smaller. Those used to truncate to zero and parse successfully,
and zero means *no ceiling* everywhere in ember, so a ceiling written in the
wrong unit silently removed the hourly ceiling, the run ceiling and the price
filter together. Write `"0"` for no limit, or a figure of at least
`"0.000001"`.

### What is guarded, and what is not

**Every request needs a token.** This section used to open by saying there was
no authentication, which was true and is not any more.

The server listens on loopback only and nothing it serves is reachable from
another machine — but **a loopback port is reachable by every uid on this
machine**, and a request with no `Origin` header is deliberately allowed,
because that is what `curl` and every script an operator writes send. So before
the token, a second account on a shared workstation, a devcontainer, or
anything reaching an `ssh -L` forward could `POST /api/requests` and, under
`--allow-paid`, rent an H100 on the operator's card. That is the threat the
credential is for: **a client that is not the operator's own page.**

`ember serve` resolves it from, in order, `EMBER_SERVE_TOKEN` in the
environment, `serve_token` in `credentials.yaml`, or `crypto/rand` — and prints
a URL ending in `?t=<token>` when it minted one. Opening that line is the whole
of the browser setup: the server exchanges the token for an `HttpOnly;
SameSite=Strict` cookie and redirects to the same path without it, so the
address bar, the session history and every `Referer` the page sends carry
nothing. The cookie is then carried on `fetch`, on the gallery's `<img src>`
and on the websocket handshake alike, which is why there is no token-handling
code in the frontend at all.

A minted token lives in the process and is written nowhere, so it changes on
restart. Set `serve_token` for one that does not — that is what a bookmark and
a script against the local API both want.

**There is no `--token` flag, and there will not be one.** `/proc/<pid>/cmdline`
is world-readable, so a credential in argv is readable by every uid on the box
for the life of the process — which is precisely the uid this token keeps out.
A lab reads the running server's own `cmdline` and fails if the token is in it.

`--allow-paid` **is unchanged and still off by default**: without it
`ember serve` refuses every provider that is not free. Authentication answers
*who is asking* and `--allow-paid` is a ceiling on what the answer permits, and
this project does not trade one control for the other.

The two checks that are **not** authentication are still made, on every route,
and the credential is additive rather than a replacement — all three or
nothing. The `Origin` must be this server's own, and the `Host` must be a
loopback name at this port; the second is the DNS rebinding case, where an
attacker's domain resolves to 127.0.0.1 and their page is therefore same-origin
with its own name, which passes the first check and fails the second.

**What the token does not do**, said plainly because a control whose limits are
not written down gets believed past them:

- It does not defend against **your own uid, or root**. Either can read
  `EMBER_SERVE_TOKEN` out of `/proc/<pid>/environ`, read `credentials.yaml`,
  ptrace the process, or read the browser profile holding the cookie. A secret
  held by a uid cannot authenticate against that uid.
- It does not **expire or rotate**. It is a bearer token; anyone who learns it
  holds it until the process restarts, which is how you revoke one.
- It does not make a **public listener** acceptable. `loopback.Bind` stays
  sealed, and widening it was not needed for any of this. The deployment story
  is still an SSH forward, which the token is what makes safe to use.

#### And a second channel, where the operating system answers instead

`ember serve` also listens on a **unix socket at `<state_dir>/socket/serve.sock`**,
mode **0600**, in a directory ember creates at **0700**. It serves exactly the
same routes as the port, and a request that arrives on it carries **no token at
all**: `connect(2)` on a unix socket needs write permission on the socket, so
the kernel refuses every account but yours before ember sees the request. There
is nothing a token could add to that, and nothing for a local client to go and
find.

The two channels answer the same question for different clients and neither
replaces the other:

| | loopback port 7777 | `<state_dir>/socket/serve.sock` |
|---|---|---|
| who can reach it | every uid on this machine, and anything on an `ssh -L` forward | the owner only, enforced by the mode |
| what authenticates a request | the token, mandatory | `connect(2)` |
| `Origin` checked | yes | yes |
| `Host` checked against the port | yes | no — a socket request arrives on no port |
| a browser can use it | yes | no |

**The token is still the only answer on the port.** Nothing a request can say
marks it as having arrived on the socket: the mark is a value on the request's
context, set by the one `http.Server` that serves the socket, so there is no
header, query parameter or proxy header that claims it. The two listeners are
served by two servers whose only difference is that field.

`Host` is not checked on the socket because it is a statement about a TCP port
and a socket request arrives on none — `curl --unix-socket` sends
`Host: localhost`, which has no port in it at all. `Origin` *is* still checked,
because it is about who composed the request rather than where it arrived: if
you put a proxy in front of the socket, it is the only thing left that refuses
a page on another site.

A clean exit unlinks the socket. A process killed outright leaves the path
behind, and the next `ember serve` clears it — but only after finding that
nothing is listening on it: a socket a running `ember serve` is accepting on is
a **refusal** to start rather than something to take, because taking it would
leave the first server answering a path nothing can reach.

There is **no `--socket` flag**. The path is composed from the state directory
by `internal/loopback`, which is the only package that opens a listener of
either kind, so a socket in a directory whose mode ember has not checked is not
a thing this program can be asked for. On a platform with no file modes
(Windows), `ember serve` refuses to start rather than offering a channel it
describes as authenticated and cannot authenticate.

What this is **not**: a reason to bind off loopback. The TCP listener is still
`127.0.0.1` and `[::1]` only.

#### `ember up` is that channel's first client

**With an `ember serve` running on this machine, `ember up` hands the run to it
and streams the result. With none, it rents here, exactly as it always has.**
Two code paths, deliberately, and the differences are below rather than left to
be discovered.

```
$ ember serve --allow-paid &
$ ember up --gpu rtx4090 --ttl 2h
submitted to the ember serve on /home/you/.local/state/ember/socket/serve.sock as 7f3a1c…
rented runpod/abc123 at 10.0.0.4:22; it destroys itself at 2026-11-03T18:40:00Z (1h59m58s from now)
running: sd-cli …
^C
detached from 7f3a1c…. THE RUN IS STILL GOING and the node is still billing.
  it destroys itself at 2026-11-03T18:40:00Z (1h59m52s from now), whether or not ember is running.
  ember ls           what is billing, asked of every provider and not of the daemon
  ember down runpod/abc123  destroy it now
  ember history      what this run came to
```

**Ctrl-C detaches; it does not destroy.** That is the resilience this is for —
the run survives the terminal that started it — and it removes the thing that
used to guarantee no leak when you walk away. What is left is the **TTL**,
which is mandatory and enforced in three independent places including the
node's own self-destruct timer, so **the `ttl` you chose is now the real
exposure** rather than a ceiling Ctrl-C usually beat. The notice above says so
with the deadline in it, because that is the one fact an operator needs at the
moment they walk away. The daemon's own shutdown still destroys the rental:
`ember serve` waits for the queue's goroutine, and that is unchanged.

**One rental at a time is unchanged, and it is still structural.** A submitted
run does not start beside the queue — it is handed to the same single goroutine
a queued request's rental lives in, over an unbuffered channel only that
goroutine reads from. So a run asked for while the queue is mid-rental **waits**
for it; it is not refused, and it cannot become a second GPU, because there is
nowhere else for a rental to start. `ember up` blocks (and says what it is
doing) for exactly as long as the daemon is busy.

**The config path travels, and that is a deliberate reversal.** Everywhere else
in `ember serve` the config is read once at startup and never from a request —
*"or the browser would be naming which file sets its own limits"*. This route
accepts one, and `--max-hourly-usd`, `--ttl`, `--image`, `--gpu`, `--provider`,
`--port` and `--cmd` **replace** the file's values rather than narrowing them,
which is what they do in your own shell. The reversal is sound for one reason
only: the route is on the **socket's** route table and not the port's, so the
caller is an account `connect(2)` has already proved is yours, with the same
authority as typing the command. A page, a `curl` on port 7777 and anything
reaching an `ssh -L` forward — token or no token — reach no handler at all,
because none is registered there.

**`--allow-paid` still bounds it**, and this is the one place the socket is not
simply your own shell. An `ember serve` you left running without the flag is a
process you have said must not spend money, and a channel that spent anyway
would make the flag mean "unless you came in the other door". So a paid
provider submitted to a daemon started without it is refused, naming the flag —
while the same command with no daemon running spends, because then you are at
the keyboard. The flag is a property of the long-running process.

**`max_day_usd` and `max_month_usd` bound it from BOTH files, and that is the
other place this channel is not simply your own shell.** A ceiling is a
question about a ledger — "has this month spent more than this" — and the
ledger a submitted run is recorded in is the daemon's, so the config
`ember serve` was started with is consulted as well as the one you named. Both
bind and neither widens the other: the tightest of the two applies, per period,
with an unset ceiling meaning no ceiling. Reading only the daemon's would leave
`ember.yaml`'s ceiling unenforced through a daemon started with a different
`--config`, which is the divergence between the two paths this whole channel
exists to remove; reading only yours would leave the daemon's allowance
spendable by a file it never read.

**And it is checked twice, the second time on the rental loop.** The first
check happens while your `ember up` is still waiting, and a run asked for while
the queue is mid-rental **waits** — so the total it read can go stale in
exactly the window it is waiting through, because a rental's cost is recorded
when the node is destroyed. The second check runs on the goroutine that is
about to rent, where no other rental can be in flight and the total is
therefore complete. A run refused there is already a row, so it ends as
`failed` with the reason, which the client you detached from (or `ember
history`) prints. Nothing is created either way.

Three smaller differences worth knowing:

- **`--dry-run` never asks a daemon.** It rents nothing and prints what a
  rental would be, so making the cheapest thing in ember depend on a process
  being up would be a poor trade.
- **The node's own output arrives as events** rather than straight onto your
  terminal, so `ember history` has it and a client that reattaches can read it.
  The rendering is the same function on both paths, so what is printed is the
  same; what differs is that a submitted run's log is also in the database, and
  `ember history rm` is what removes it.
- **`--log-level` on a submitted run reaches nothing.** A submitted run's own
  log lines are written by the daemon, at the level `ember serve` was started
  with, to the daemon's terminal — so `ember up --log-level debug` through a
  daemon still refuses a bad level and changes nothing about the run. Carrying
  the level over the socket would only make a terminal you are not reading
  noisier. Turn it up on `ember serve`.

**Recording is unchanged.** Either path writes one row through the same
`Adopt`, and `ember history`, `ember spend --by gpu|day|provider|model` and
every per-job column see both. The row is `running` from the moment it is
written and never `queued`, because a queued row is exactly what the drain loop
claims — an `ember serve` in another terminal would otherwise rent a second
card and generate the batch again from the record of the run still happening.

**Auto-starting a daemon is not offered.** A background process that can spend
money, started because somebody typed `ember up`, is not a thing ember does.

### Where the state goes

The queue and the history are a SQLite database beside the node journal, under
the state directory. Images are written where `ember up` has always written
them, one directory per request, and the same path is the key a later storage
backend would use. An **uploaded** image is the other way round: it goes in
`<state_dir>/inputs`, keyed by its own sha256, because it is an argument to a
run rather than something a run produced — see
["Generating from an image you upload"](#generating-from-an-image-you-upload)
for what sweeps it. Progress is stored as numbered events, so a page reload and
a dropped websocket are the same code path: a client says which sequence number
it last saw and is told everything since.

A request that was generating when the process died is marked **interrupted**
rather than failed or re-queued. Its rental is gone so it cannot be resumed,
and renting a second card for work the operator may no longer want is not a
decision ember makes on their behalf. Whatever images it produced stay listed,
because they were paid for.

A request whose **rental** failed is marked **failed**, with the reason, as
soon as the rental unwinds. That covers everything between the machine being
created and the first image being asked for — waiting for sshd, the GPU probe,
the asset verify, the model load — all of which happen after the card is
already being paid for. It used to read `running` until `ember serve` was
restarted, which was its own trap: restarting is what destroys any *other*
rental in flight, and the cancel button meanwhile answered with the
provisioning-window message saying the rental was coming up, which by then was
false.

**Stopping `ember serve` with Ctrl-C ends the request it was running as
cancelled, with the reason, rather than leaving it to the sweep above.** SIGINT
cancels the context the rental was using, and that cancellation *is* the error
being recorded — so the three writes that record how a request ended travel on
a context of their own, detached from the run's and bounded at ten seconds.
Before that they could not land at all: the write was refused before the
database was reached, the row kept the `running` the claim had written, and the
next start marked it `interrupted` with nothing in the reason column — the one
column the watcher and `ember history` print. The sweep still covers what it
was written for, a process that never got the chance to write: SIGKILL, an OOM
kill, a closed laptop. Cancelled and not failed, because the operator pressed
Ctrl-C or the node reached its deadline, and calling that a failed generation
sends them looking for a bug that is not there — a provider that did not answer
inside its own timeout reads `cancelled` too, which is one answer for three
causes and is why the reason column is the thing to read rather than the
status. A write that cannot finish inside its bound is abandoned and logged,
because `ember serve` waits for the queue before it exits and a write that
never returns is a server that never stops.

A request that finished normally keeps the status its own generation wrote,
even when the rental then failed to tear down. The teardown error is still
reported — a node that may still be billing is the one thing an operator must
hear about — but it does not turn a completed request into a failed one.

### Checking it works

```bash
make test-integration    # the serve labs that need no workload image
make test-serve-lab      # one rental drained by a queue, and the real UI
```

The first group runs on every pull request and is the security half: that the
server answers on both loopback families and on none of this machine's other
addresses, that a page from somewhere else is refused, that **every route
including `/ws` is refused without the token** and nothing is queued by the
attempt, that the one-time URL comes back as a `SameSite=Strict` cookie, that
the token is not in the running process's `argv`, that a paid provider is
refused without the flag, that SIGINT destroys the rental and says so, and that
SIGKILL leaves exactly one finding `ember ls` reports as needing attention.

`make test-serve-lab` needs [`ember:sd`](#workload-images) and a Chrome or
Chromium binary. It queues a three-job batch, waits until it is generating,
queues a second request, and asserts from ember's own output that **one** node
served both — then drives the real page in a real browser: refused with no
credential, then the one-time URL opened, the token gone from the address bar,
offers listed, prompt typed, generate clicked, the sampling step advancing, the
money row saying the rental is not priced yet rather than printing a `$0`, and
an image that decodes.

A second browser lab presses **cancel** in both directions on one request: once
while the rental is still being provisioned, where the row reads `running`, the
queue has nothing to signal, and the page has to print the 409 that says the
rental is coming up — *and not* the one that asks for a restart; then once it
really is generating, where the page has to say `cancelling` before the next
poll and take the button away. A lab that
asserted only the accepted path would pass against a page that swallows every
refusal, which is what it did. That lab's config gives `gpu_probe` a sleep, so
the first window is twenty-five deterministic seconds rather than a race
against the listing poll.

Both directions in one browser, because every claim about the cookie is a claim
about Chrome rather than about ember: that a 303 leaves the token out of the
history, that the cookie reaches an `<img src>`, and — the one no Go test can
stand in for — that Chrome attaches a `SameSite=Strict` cookie to a websocket
handshake, which is how the progress row learns anything at all.

That browser lab is not completeness. Three bugs in this feature were invisible
to every Go test and every `cargo test` and obvious the moment a page loaded: a
nil slice marshalled as `null` dropped whole websocket updates, a keyed list
never re-rendered so the progress row froze, and a cached wasm meant a reload
after `make web` served the previous app. It is the only thing in the
repository that can see them.

The file picker is in that lab for the same reason and is the clearest case of
it: there is no `File`, no `FileList` and no `Blob.arrayBuffer` outside a
browser, so `cargo test` can check that a struct serialises and nothing else.
A second lab picks a real file in a real Chrome and asserts that the request
the page queued names **the sha256 the lab computed itself** — the join
between the two halves, rather than either half agreeing with itself. A third
runs the same prompt, seed, size and step count twice on one rental, once with
an uploaded image and once without, and asserts the two result files
**differ**: a server that accepted `init_image` and dropped it would produce
two identical images, and a parameter that changes no pixel is the defect the
whole inbound path exists to avoid.

### Stopping it

Ctrl-C destroys any rental before the process exits, and says so. `ember serve`
waits for the queue's goroutine as well as the HTTP server: a rental does not
live in a request, so draining connections would say nothing about a node.

Cancelling a request is a button whether it is **queued** or **generating**.
`POST /api/requests/{id}/cancel` takes both.

**It does not take a run submitted by `ember up`.** Such a row is `running`
from the moment it is adopted and never becomes *generating* — there is no
sd-server job to stop, because the only way to end an `ember up` is to destroy
the node — so the cancel route answers the provisioning-window 409, whose "ask
again once it is generating" is a promise it cannot keep there. `ember down
<node>` is the command that ends one, and the detach notice names it. Telling
the two states apart needs a fourth answer from the queue, which belongs with
`ember down` over the socket rather than here; a test asserts the current
answer, so the day it changes somebody reads the comment beside it.

Cancelling a generating one stops its current image and drops the jobs behind
it; the rental is left alone, so anything else queued onto it keeps running on
the same warm card. That is the difference between cancelling a *job* and
cancelling a *rental*: sd-server's own `POST /sdcpp/v1/jobs/{id}/cancel` ends
one generation and leaves the process, the node and the resident weights
exactly as they were. Stop `ember serve` to end the rental itself.

The in-flight job is where it gets interesting, and the honest answer is not
"always". ember stops it only if the server reported
`features.cancel_generating`, read from `/capabilities` when the weights went
resident. If it did not, ember **does not refuse the cancel** — it drops the
request's remaining jobs, which on a batch is most of the money, leaves the
one in flight to finish, and says so on the progress stream: *"job 3/8: cannot
be stopped, so it will finish and be paid for (…); 5 job(s) after it were
dropped"*. Refusing instead would leave an operator who is trying to stop
spending with no lever but stopping `ember serve`, which destroys the rental
and takes every other request on it.

A cancel reaches **one** request. Cancelling a request the queue is not
generating answers 409 and records nothing: "I could not stop it" and "it is
stopped" are different answers, and the first must not render as the second
over a card that may still be billing.

**Two different things reach that 409 and they get different sentences**,
because one of the remediations is destructive in the other's state. A row
reads `running` from the moment it is claimed and the queue can only signal a
batch that has started generating, so clicking cancel while the node is still
coming up — the plan, the create, the `gpu_probe`, the asset verify, the model
load — lands in a window where a rental *is* in this process and is seconds
from running. That answer says the rental is coming up, to ask again, and
explicitly **not** to restart `ember serve`, which would destroy the rental
the request is waiting for. The other case is a row left `running` by an ember
that is gone, which nothing here holds and which a restart really does
reconcile (`queue.Run` marks what it finds in flight as `interrupted`); that
one keeps the restart advice. ember can tell them apart without a counter
because there is one drain goroutine and a rental lives inside one iteration
of its loop.

**The page says which of the three happened**, and until recently it said none
of them: the answer was decoded into a dynamic value and the whole `Result`
thrown away, so a refused cancel left a row that still read `running` with a
`cancel` button still on it — indistinguishable from a click that never
registered, which is what sent an operator clicking again over a card that may
still be billing.

- A **refused** one prints the server's own sentence, in full — which of the
  two 409s it was, and therefore whether the right move is to ask again or to
  restart. The button comes back, because nothing was stopped.
- An **accepted** one on a generating request says `cancelling` beside a status
  that still honestly reads `running` — the queue is polling sd-server for the
  job's own answer and is the one that records how the batch ended, so the
  handler does not write a second answer over it. That word is the `cancelling`
  flag the response carries, and it arrives before the two-second poll does.
- An **accepted** one on a queued request says `cancelled`, which is the status
  the handler wrote itself: a queued request has no rental, so there was
  nothing to signal and nothing left to wait for.

In all three the button goes away the moment the click lands and the cell says
`asking` until the server answers, because a double-clicked button is the
ordinary case and the second ask is the one that comes back 409 — a refusal
printed over a cancel that worked is worse than no message at all. `asking` is
a different word from `cancelling` on purpose: one is this page having sent a
request and the other is the server having accepted one.

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
62% of billable rental time was spent generating.
1 request(s) contribute their rental cost and no per-job figures -- they ran
before ember recorded them, or finished without a job completing. So per image
is an upper bound here, and steps/s is computed only over what was recorded.
Nothing backfills them.
```

`--by` takes `provider`, `gpu`, `day` or `model`; `--since` and `--until` are
UTC dates and the period is half-open, so consecutive periods tile without
counting a day twice. `--json` prints the rows with money as integer microdollars, and
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

**Grouping by model took a column, and it is the grouping that makes the
others readable.** `--by gpu` on its own groups the dimension that is nearly
useless alone: "an A100 costs $X per image" means nothing without the model,
and the two measurements in this file differ by a factor of about three
hundred in sampling time on hardware a factor of four apart. It could not be
offered at all until `requests.model` existed, because `sd.serve` is config
rather than request and the only stored copy of a model was one cached
capability row that the next rental overwrites — so a historical request's
model was not knowable, and grouping by it would have meant reading a value
from the wrong place and believing it.

```
$ ember spend --by model
model                reqs  jobs images      rental   card time    overhead  per image steps/s
-                       1     1      0        $0.5          $0        $0.5          -       -
flux2-dev-Q8_0.gguf     1     4      4       $1.79       $1.52       $0.27    $0.4475    0.05
sd15-Q8_0.gguf          2     8      8       $0.89       $0.76       $0.13   $0.11125    0.15

total: 4 request(s), 12 image(s), $3.18 of rental, $2.28 of card time
62% of billable rental time was spent generating.
1 request(s) contribute their rental cost and no per-job figures -- they ran
before ember recorded them, or finished without a job completing. So per image
is an upper bound here, and steps/s is computed only over what was recorded.
Nothing backfills them.
```

Three things about that column. **It is a response, not a reading of the
config.** The value is `capabilities.model.name`, out of the same
`/capabilities` answer ember caches to validate a sampler — so a `sd.serve`
edited between two rentals cannot attribute last week's spend to this week's
weights. **It is what the server calls the model, which is the diffusion
model.** sd-server answers the basename of `--model`, or of
`--diffusion-model` where there is no `--model`, so a FLUX.2 configuration
naming its diffusion model, text encoder and VAE separately records the first
and says nothing about the other two: two setups differing only in those read
as one model. That is a limit of what the server reports rather than a
decision here, and ember prefers it to a name ember composed from the serve
argv, which would be a reading of the config again. **Nothing backfills, and
here there is not even a wrong answer available** — no event carries the
model, so the only thing a backfill could read is that cached capability row,
which would stamp every row in the history with whatever is loaded today. A
request from before the column, or one that never reached a node, groups under
a dash and `ember history` says how many it had to leave unnamed.

**`ember up` is in here now**, which it was not when these columns were
added: `ember serve` was the only writer to the history, so the command every
measurement in this file was taken with printed its cost to a terminal and
kept nothing. It records by default — there is no flag to remember, and
nothing to opt into — and a database that will not open prints one line and
does not stop the run, because refusing to rent would be ember declining to do
the thing it is for because it could not take notes.

What it records depends on which shape it ran, and the difference is
deliberate:

| `ember up` | what is recorded |
|---|---|
| with `sd:` jobs | the rental, and a row per job: its cost, span, images, passes and steps, as a queued generation gets |
| with `cmds` | the rental, and each step's cost and span **as events**. No job rows: a step is not a job, and images-per-step and steps-per-second are not things an `ffmpeg` invocation has |
| forwarding ports | the rental. That is the whole record, and it is the cheapest thing ember does and the easiest to leak |

The last two have no jobs, so their per-job columns are a **dash** and never a
zero, and both readers say how many rows they had to leave out. A column of
zeros in a money ledger reads as free. They are no longer the only jobless
rows: the machine with no card that fills a store has none either, and the
footnote names all three because nothing in a row says which it was.

Two things are not recorded and are worth naming. A run refused **before** the
create call — an unpinned image, a budget that would not admit the offer, a
store that does not exist — has no row, because nothing was rented and nothing
billed; a run that got as far as asking a provider for a machine always does,
including one whose create call failed, because that is the window where a
rental nothing recorded is born. And `ember up` writes no `images` rows: its
files go into a timestamped run directory it prints to you, which no gallery
serves and `ember history rm` must not offer to remove. The image counts and
bytes are columns on the job either way, which is what the ledger and the
aggregate read.

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
when (utc)       request          status      provider   card          jobs images      wall      rental   card time model
2026-10-31 18:19 080f21c0cc2f5be8 done        runpod     rtx4090          3      3     8m30s           -      $0.285 sd15-Q8_0.gguf
2026-10-31 18:02 5f324627337df6f5 done        runpod     rtx4090          5      5     17m0s       $0.89      $0.475 sd15-Q8_0.gguf
2026-10-29 09:14 15d87a1aae672727 done        runpod     a100-80g         4      4    41m12s       $1.79       $1.52 flux2-dev-Q8_0.gguf
2026-10-24 11:40 1393e5b386595420 done        runpod     rtx4090          1      -     33m0s        $0.5           - -
2026-10-22 14:05 9c1ee0f3a8d7b215 done        runpod     rtx4090          0      -     1m30s       $0.07           - -

total: 5 request(s), 12 image(s), $3.25 of rental, $2.28 of card time
1 request(s) rode along on a rental another request opened, so their rental
cost is a dash rather than zero: the node was charged once, to whichever
request paid for it to exist.
1 request(s) finished with no per-job figures -- from before ember recorded
them, or without a job completing -- so their images and card time are a dash
rather than zero. Nothing backfills them.
1 request(s) have no jobs, so their per-job columns are a dash rather than
zero: there was never anything to report. That is an `ember up` forwarding
ports or running `cmds`, and the machine with no card that fills a store --
which `ember serve` rents on its own initiative and `ember store sync` rents
by hand. The rental is the whole record in each case; a `cmds` run's
per-step costs are the one thing beyond it, in its event stream.
1 request(s) have no model: they never reached a node, or they ran before
ember recorded which model served a request. Nothing backfills them -- the only
stored copy of a past model is a cached capability row the next rental
overwrites.
```

The last row is an `ember up` that forwarded a port for ninety seconds: no
jobs, no images, seven cents of rental. It is in the table because the
alternative is a rental nothing can see, and its per-job cells are dashes
because there was never a per-job figure to lose — which is a different fact
from the row above it, where there was one and it went missing. The two have
separate footnotes for that reason.

The model column is last for one reason: it is the only cell whose width is
not ember's to choose. A model is a filename, and `flux2-dev-Q8_0.gguf` is
nineteen characters against `rtx4090`'s seven — so anywhere else a long name
pushes every figure after it out of its column, and truncating it to fit
would make two quantisations of one model read as the same model.

`--since` and `--until` are UTC dates and half-open, the same rule `ember
spend` uses, so the two tile alike. `--provider`, `--gpu` and `--status` match
exactly; `--limit` bounds the page, which defaults to the newest 200. `--json`
prints the rows with money as integer microdollars and the full text of any
error, where the table shows one line of it.

**The two money dashes are the point of the table.** A rental serves every
queued request it can, and its cost is charged to the request that *opened*
it — so every request that joined a rental already up has a rental cost of
zero by design. A money column of zeros that means "charged on another row"
is the misreading this marker exists to prevent, and it is decided by asking
whether another request on the same rental carries the cost, not by the zero
itself: the local provider's rentals are free, so cost alone cannot tell the
two apart. The second dash is a request that finished with no per-job figures,
which is every request from before the execution columns existed and also one
that broke before a job completed. Both divide a rental cost into no images,
which is why they are counted together and why `ember spend` calls per image
an upper bound when there are any.

A dash in the model column is a third fact and not one of those two: nobody
read a model for that request, because it never reached a node or because it
ran before ember recorded which model served one. It is said out loud under
the table for the reason the other two are — a column of dashes otherwise
reads as ember having lost the names. A row with no jobs is left out of that
count on purpose: it started no generation server, so there was no
`/capabilities` response to read a model from, and counting it would have made
the footnote claim an `ember up` that forwarded a port successfully "never
reached a node".

**It reports what each node said about its memory.** `--memory` prints the
same rows with the memory facts instead of the costs:

```
$ ember history --memory
when (utc)       request          provider   card         cgroup       limit       using  host total     weights    headroom
2026-10-31 18:02 5f324627337df6f5 runpod     rtx4090          v2    32.0 GiB    31.0 GiB   700.0 GiB    56.0 GiB   -24.0 GiB
2026-10-29 09:14 15d87a1aae672727 runpod     a100-80g         v2    64.0 GiB    58.0 GiB   700.0 GiB    56.0 GiB     8.0 GiB
```

Those two rows are the measurement this exists for. Eight runs of FLUX.2 on
the same card in the same datacentre split in two: a node that kept the 56 GiB
of weights in page cache spent 145.3 s per step, and one that did not spent
226.6 s, because every step re-read the whole model off the network store at a
measured 868 MiB/s. **81 seconds and $0.056 per image of variance between two
runs of the same configuration**, and nothing in ember could see why — all
eight nodes reported 620–708 GiB of free RAM.

That figure was the wrong number. `/proc/meminfo` inside a container reports
the **host's** memory: measured with `docker run --memory 512m`, a container
bounded at 512 MiB reports 125.7 GiB of `MemTotal`, a factor of 252. So the
host's view never said anything about the limit the page cache actually lives
under, which is the container's own `memory.max` — and nothing read it. Two
file reads on a node that is already billing, and ember was not doing them.

Both views are recorded now, on every rental, before the workload. `limit` is
the cgroup's and `host total` is `/proc/meminfo`'s; `headroom` is the ceiling
the page cache lived under less the weights that had to fit in it, and a
negative number there is a node that re-read the model every step. A dash is
"nobody read it", which is a different claim from zero — `cgroup` says which
hierarchy answered, and `v1`'s paths are named rather than derived because a
guess at where a hierarchy is mounted reports "no limit" on the one kernel it
is wrong about.

**There is no verdict column, deliberately.** What to *do* about a node that
cannot hold the weights — a warning naming the expected re-read cost per step,
or a corrected note if the guess turns out wrong — is a decision to take once
there are measurements, and a column saying "this will be slow" would be that
decision taken from a guess. The probe cannot fail a run either: it is four
file reads and a `stat`, and a node that will not answer them is a node whose
generation should still happen.

**What is still unknown is what RunPod sets.** The captures in
`internal/nodemem/testdata/` are Docker's defaults on one host; whether a pod's
`memory.max` is below its asset set is the question, and it needs a rental.
The point of recording it is that the next one answers it without anybody
remembering to type a command at the right moment.

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

### What the history is occupying, and what may be removed

Nothing in ember deleted a generation. The schema carried three
`ON DELETE CASCADE` clauses and no statement anywhere that removes a row, and
`transfer.RunDir`'s one-directory-per-run is how it stayed, forever. What that
costs is not dollars but disk, which is why it is easy to leave for a year:

```
$ ember history du
database  128.0 KiB   ~/.local/share/ember/state/generations.db
          3 request(s), 8 job(s), 224 event(s), 8 image row(s) naming 30.8 MiB
outputs   34.7 MiB    ~/.local/share/ember/outputs
          9 file(s)

3.9 MiB under outputs is named by no image row. `ember up` writes a timestamped
run directory here and records no image rows for it, so `ember history rm`
can never reach these.

140 event rows are older than history.keep_events (720h0m0s);
see `ember history prune`.
```

**The third figure is the one that would otherwise mislead.** `ember serve`
writes a request's images under its own request id, which is what the `images`
rows name; `ember up` writes a timestamped run directory into the same tree
and records no image rows for it — it records the rental and what each job did,
and its files are yours rather than a gallery's. Without that line an operator
reads one total, removes every request's images, frees a fraction of it and
concludes ember is broken. The database figure includes SQLite's write-ahead log, which is disk
in use and can be larger than the file beside it.

**What may be removed is a retention decision, and it is not the one the
cascades describe.** A cascade makes "delete a request" and "delete its
images" one operation, and those two pull in opposite directions — the rows
are small and are the only record that money was spent, the pictures are large
and are the reason to prune at all. So:

| | may it go | by age | what removes it |
|---|---|---|---|
| events | yes | yes | `ember history prune` |
| images, and the rows naming them | yes | only with an explicit `--older-than` | `ember history rm` |
| requests, jobs, every cost column | **no** | no | nothing |

**Events are the easy case.** They are the bulk — a batch at 28 steps emits
rows per step per job per pass, and passes restart for each batch item and
each highres pass — and the execution columns above are a *derived index* of
exactly these rows, so a prune changes no figure `ember spend` or
`ember history` prints. What it costs is the replay: the event stream is the
record ember treats as the truth, so anything not already in a column is gone
with the rows, and nothing backfills them.

**Requests and jobs are not prunable at all, and that is the narrowing.**
`requests.cost` is the only record anywhere that a rental happened and
`ember spend` is a `SUM` over it. Every other way of being short of history
here announces itself — `Bucket.Unrecorded` counts the requests whose per-job
figures are missing and `ember spend` prints how many it had to leave out —
and a deleted row cannot be counted, because there is nothing left to count.
It would make a period total quietly smaller than the invoice, with no
footnote possible, which is the one direction a money figure must never be
wrong in. What it would buy is a few hundred bytes a request. The cascades
therefore stay unreachable, deliberately: dropping a foreign key in SQLite
means rebuilding the table, and if a request row ever *were* deleted,
cascading is the only behaviour that would not leave orphans.

So `ember history rm` removes the pictures and keeps the money:

```
$ ember history rm --older-than 720h
  e7d790ed48488f6e   4 image(s)    22.5 MiB
  4b9de52ed51a105d   1 image(s)     1.7 MiB
2 request(s), 5 image(s), 24.2 MiB
ember: add --yes to remove them; a generated image is not recoverable, and the costs and counts stay either way
$ echo $?
3

$ ember history rm --older-than 720h --yes
  e7d790ed48488f6e   4 image(s)    22.5 MiB
  4b9de52ed51a105d   1 image(s)     1.7 MiB
2 request(s), 5 image(s), 24.2 MiB
removed 5 image(s), 24.2 MiB, and 5 row(s).
compacted ~/.local/share/ember/state/generations.db.
```

`ember history` prints the same three rows after that as before — the same
eight images, the same `$3.18` of rental — because a ledger row's image and
byte counts come from the job rows and never from the `images` table. The
images table is the *pointer to the files*, not the measurement of them. What
says the files are gone is `du`:

```
2 finished requests produced images and have none on disk, which is what
`ember history rm` leaves: the costs and the counts stayed and the pictures
are gone.
```

**Both verbs print what would go and then refuse until `--yes`, and the
refusal exits 3.** Not 0, so a script that forgot the flag cannot read the
exit status as a removal. `--dry-run` is the spelling for asking on purpose,
and it wins over `--yes` rather than losing to it, so `--yes --dry-run`
removes nothing — a flag that is sometimes a dry run is worse than none. A
prompt would have been the wrong shape here: ember's exit codes are a
contract with cron and CI, and a command that blocks on stdin hangs there
instead of answering.

**`VACUUM` runs only after a prune that removed something**, and the guard is
inside the function rather than at the call site, so there is no unguarded
entry point to forget. SQLite does not return freed pages without one, so a
prune that skipped it would free disk nobody can see; a `VACUUM` rewrites the
whole file, so running it after a prune that removed nothing makes the no-op
case the expensive one on exactly the database large enough to be worth
pruning.

**`history.keep_events` is the default horizon and nothing applies it on its
own.** Absent, or zero, keeps every event forever; set, it is what
`ember history prune` uses when `--older-than` is not given, and what `du`
counts against. A negative value is refused rather than clamped, as a negative
`--older-than` is: it reads as "no bound", and "finished before now *plus* a
month" selects every request that has ever ended.

```yaml
history:
  keep_events: 720h   # 30 days; absent means keep them forever
```

The only process that *could* run a prune automatically is `ember serve`,
which is the process that writes the events — a prune on its startup would
delete the replay of the run the operator came back to look at, with no
confirmation and no output they would ever see. That is the inverse of the rule
that stops a rental creating a standing monthly charge: nothing that runs a
generation may delete an older one as a side effect, and events cost disk
rather than dollars, so no deadline forces the issue the way a billing node
forces `reap`.

**The horizon is measured from when a request finished**, which is where the
period spellings in this repository legitimately part. `--since` and `--until`
bound when a request was *created*, because an operator asks what last week
cost; a retention horizon asks how long after it ended, and for a request
created in March and finished yesterday the two answers are forty weeks apart.
A request that has not finished is unreachable from either verb by
construction rather than by a filter — it has no end to compare — so a browser
streaming a run in flight cannot have its rows pulled out from under it, and a
job three images into eight cannot have the first three removed.

**A request id is the key to a path, so nothing here builds one.** Every byte
removed goes through `blob.Store` under the key the database holds: the name
is checked component by component and the removal goes through an `os.Root` —
containment twice by two mechanisms, the rule `internal/transfer` follows for
a tar member. A hand-edited row naming `../../../etc/passwd` is refused and
named rather than cleaned. `Delete` also takes the directory its removal
emptied, because one inode per pruned request left behind forever would be
disk `du` reports and no command in ember can free.

## Logging

`ember up` and `ember serve` take `--log-level` and `--log-format`:

```
--log-level debug|info|warn|error    default info, or EMBER_LOG_LEVEL
--log-format text|json               default text, or EMBER_LOG_FORMAT
```

The flag wins over the variable, and a value neither recognises is refused
with exit 3 rather than ignored — a stale `EMBER_LOG_LEVEL=verbose` that
silently fell back to `info` would mean turning the logs up, seeing no change
and concluding there is nothing there to see.

They are only on those two verbs because those are the only two that build a
logger. `text` is `slog`'s own logfmt, which is what a terminal reads and what
Loki and Vector ingest; `json` is the same records for anything else. Nothing
is shipped anywhere — there is no exporter and no egress, which is now a
decision rather than a gap; see below.

**`debug` was unreachable until now, and there were seventeen debug lines
waiting for it.** The handler was constructed in two places with the same
literal and both pinned the level to `info`, so not one of them could be
displayed by any invocation of ember that shipped. The four worth knowing
about:

| line | answers |
|---|---|
| `node running but not yet reachable` | why a provision is still waiting |
| `sd-server exited` | why a served run is about to time out at `ready_timeout` |
| `asset ready` | how long each asset took to verify |
| `could not read the record to estimate the cost` | why a teardown printed no cost |

`internal/logx` is the one place that builds the handler, and
`TestNoHandlerOutsideLogx` walks the tree and fails the build if a second
place appears — the shape `TestNoListenOutsideLoopback` already uses for
`net.Listen`.

### Finding the lines that belong to one rental

Every line written inside a rental carries `run=<id>`, attached once in
`provision.WithNode` and inherited by everything handed that logger — the SSH
session, the forwards, the GPU probe, the generation server and ember's own
pipeline. `ember serve` adds `request=<id>` where a queued request is what the
rental is serving.

```
$ ember up --log-level debug --log-format json | jq -c 'select(.run=="8f3c...")'
```

These are not the generation history. `ember history` and `ember spend` read
`report.Event` rows — what ember *did*, numbered and durable. A log line is
how ember *behaved*: a retry, a fallback, a timing that surprised it. They
share the `run` and `request` labels so a line and a row can be put side by
side, and nothing routes one into the other, because a history that is also a
log sink is a history nothing can bound.

### The node's own log comes home

`embernode` — the program ember runs on the rented machine — installs the host
key, installs the authorized key, starts `sshd` and arms the self-destruct
timer. It used to write plain text to a stream nothing in ember ever read, on a
machine ember then destroys. So the component whose failures are hardest and
most expensive to diagnose was the one whose evidence was thrown away: the
host-key incident, the trust-on-first-use fallback, the missing `/workspace`
and the wrong-architecture abort were every one of them diagnosed by somebody
watching a terminal while it happened.

It now logs in the same format as the CLI, to its own stderr *and* to a file,
and ember copies that file into the run's output directory as `embernode.log`
before the node is destroyed — on a context that survives Ctrl-C, like every
other fetch, because an interrupted run is the one whose log is worth most.

```
~/renders/20261102-2312-8f3c91.../
├── embernode.log
└── renders/
```

Two things it does not do. **It cannot fail a run**: an older node image, an
`sshd` already gone or a read-only `/tmp` are all ordinary, and the failure is
reported at `debug` and swallowed — logging is the one subsystem that must
never be load-bearing. **A `SIGKILL`ed ember leaves it on the node**, because
nothing is left to fetch it with; the TTL still destroys the machine.

The keys are logged as fingerprints and never as material, which is what
`node.Secret` exists for — and now matters more than it did, because the file
is on the operator's disk rather than in a stream nobody reads.
`internal/logx`'s `TestNoKeyMaterialInALogCall` walks the tree and fails the
build if any log call unwraps a secret.

### Nothing is exported, and OpenTelemetry is in the dependency list anyway

**ember sends no telemetry anywhere.** There is no exporter, no collector
endpoint, no flag and no environment variable that turns one on. An audit of
ember's dependencies will nevertheless find seven `go.opentelemetry.io`
modules, so the honest answer to why:

```
$ go mod why go.opentelemetry.io/otel
github.com/Legion112/ember/internal/dockerengine/dockerclient
github.com/docker/docker/client
go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
go.opentelemetry.io/otel
```

Docker's client wraps its own HTTP transport in tracing unconditionally, and
ember uses Docker's client. So the otel **API** is linked into every ember
binary — 530 symbols — and the **SDK is not**:

```
$ go tool nm bin/ember | grep -c opentelemetry
530
$ go tool nm bin/ember | grep -c 'otel/sdk'
0
```

The API with no registered provider does nothing: spans are created,
discarded and never leave the process. That is the state ember ships in, and
it is held there by a test rather than by nobody having got round to it —
`internal/logx`'s `TestNoTelemetrySDKRegistered` walks the tree and fails the
build on an SDK or exporter import or on an `otel.Set…Provider` call, and
`TestOTelStaysIndirect` fails if an otel module loses its `// indirect`
marker in `go.mod`.

Why it was decided that way, with the numbers, is in `AGENTS.md`'s dependency
bullet. The short version: a span sink that needs no network costs +2.5% of
the binary and has no viewer, the one with a viewer costs +47% and a gRPC
stack, the free Docker spans cover tens of milliseconds of control plane out
of the ~27 s a rental spends on overhead, and `--log-level debug` with the
`run` key already answers the question that was being asked. If an exporter
is ever added, its destination is an allow-list and the allow-list is
loopback — what is new about an exporter is not the connection but the
payload, because a span of a generation carries the prompt.

## Video

The [video configuration](docs/configurations.md#video-from-a-model-on-a-store)
is what the store exists for. It is written down rather than committed as an
`ember*.yaml`, because its asset digest is a placeholder on purpose and so it
refuses before anything is rented — a configuration that cannot be run is a
worked example. Wan 2.2's experts are 14 GB each, which could not be
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

#### If you are about to reach for `cmds` to make a long clip

It will work, and you should not.

Twelve `cmds` entries *do* chain: a step's output stays on the node, so entry
*n+1* can name the file entry *n* wrote. That is stated above under `cmds` and
asserted by a lab. What it costs is everything ember cannot see:

- Each seam needs the previous clip's **last frame**, so a real chain is
  twenty-three entries — twelve generations and eleven `ffmpeg` decodes,
  written by hand in YAML, with nothing checking that entry 7 names the file
  entry 6 wrote. A typo there surfaces at step 7 of a rental whose first six
  steps have been paid for.
- A clip is **minutes of sampling reported as one line**. `cmds` prints a
  description, a duration and a cost per entry; there is no step counter and
  no preview, so a clip that is working and a clip that is wedged look the
  same for as long as it takes.
- The clips come home as they are produced, but **the chain's state dies with
  the node**. A failure at clip 7 leaves six clips on your disk and clip 6's
  last frame on a machine that is being destroyed; resuming means paying for
  the first six again. So `ttl` has to cover all twelve and `max_run_usd` —
  checked against the offer's price times that `ttl`, before anything is
  rented — is one decision made up front about a rental of hours.

The chain belongs on the served path, which is the one place ember reads its
own payload and so the one place that can take the last frame of job *n* and
hand it to job *n+1*. That is not built, and it is not *sized* either: what a
`vid_gen` response even carries decides whether a seam needs a decode at all,
and the captured `capabilities.json` answers that for `img_gen` only. The
reasoning, the measurement that corrected the original premise, and the list
of numbers that cannot be estimated before one real clip exists are in
[Long video is a pipeline](docs/long-video-is-a-pipeline.md).

One thing `cmds` is good for here: two identical clips as two entries, and the
difference between the two per-step card-time lines is the per-clip model
reload — the number a served path would be removing, which nobody has.

**Nothing has run it end to end.** It is written from the measured API shapes
and the flags in this repository's own image. The first real run is a
calibration — it costs money, and what it produces is a number and a fixture
rather than a pass or a fail.

## An open code model, over one forward

The [code-model configuration](docs/configurations.md#an-open-code-model-over-one-forward)
serves an
open code model on a rented card and forwards it to `127.0.0.1:8080`, so an
editor or a coding agent on this machine talks to an open model. It needed no
new code: the forwards run *alongside* the workload rather than instead of it,
so a server as the `cmd` and a `ports` entry beside it has always been a
supported shape. `llama-server` does not exit, so [a node rented to run
something stops when it has run it](#two-properties-that-outrank-features)
never fires, and the node holds until Ctrl-C, `--hold`, or the TTL.

```yaml
cmd:   [llama-server, --model, /models/coder.gguf, --host, 127.0.0.1, --port, "8080"]
ports: [llm:8080]
ttl:   8h
```

**The TTL is a hard stop, not a session length.** This is the part to decide
before you start rather than to discover at hour eight. The deadline is
written in three places — the provider's label, the local journal record, and
the node's own self-destruct timer — and the third is armed from an
environment variable that `embernode` reads *once*, at container start.
Nothing on the node listens for a change, so a running node's deadline is not
re-armable. At the deadline the endpoint your editor is pointed at goes away,
mid-sentence if that is where you are, and the remedy is a new node, another
read of the weights off the store, and a new forward. The ceiling above
whatever you write is a day.

That stays the answer rather than becoming a feature, and the three copies are
why: three copies computed at three call sites are three copies that can
disagree, and the one that disagrees is the one that does not stop the bill. A
re-arm has to update all three atomically — including a timer armed from an
immutable environment variable, which means changing *how* a node learns its
deadline — and that is the one invariant the money discipline here rests on.
Trading it for not having to restart a session is not a trade this repository
makes quietly.

**Nothing stops the bill while you read.** A coding agent's card is busy while
a completion streams and idle while you read it, think, edit and run tests —
the lowest duty cycle any workload here has been pointed at, billed by the
hour of wall clock. There is no idle-stop, and ember cannot infer one from the
tunnel: `tunnel.Forwarder` pipes bytes with `io.Copy` and counts none of them,
and an idle TCP connection is still a connection, so an editor holding a
keep-alive open looks identical to one being typed into. What "idle" means
here is "no tokens generated for N minutes", which lives in the server's own
payload — the thing ember reads only for `sd:`, and only with a written
reason. So `ttl` and `--hold` are the whole of it, and the honest use of this
configuration is a session you intended to have rather than one left running.

### What it does not protect you from

The forward is loopback on **both** ends and neither end is configurable. The
local end can only be named through `loopback.Bind`, whose field is unexported
— and both families are bound, so a client that resolves `localhost` to `::1`
first does not read connection-refused on a working tunnel. The remote end is
built by `tunnel.RemoteLoopback(port)`, which takes a port and not an address,
because SSH's `direct-tcpip` connects anywhere the node can reach — another
tenant, the provider's metadata service, the open internet — and a free-form
remote host would make every ember node an open relay billed to you.

So nothing off this machine can reach the model. What is left:

- **Every uid on your own machine can.** The endpoint has no authentication,
  and a loopback port is not a boundary between local users. That is the same
  threat `ember serve`'s token is about, arriving from the other direction.
- **The provider has root on the node.** Your prompts are your source code,
  and they travel to a machine somebody else owns. SSH protects them in
  transit and from nobody else; `runpod_cloud: COMMUNITY` makes the owner a
  stranger rather than a company.
- **`--host 0.0.0.0` is not what would publish it, and ember now says so.** A
  workload port is never published — `exposure.PortRequest`'s `public` field is
  unexported, only `SSHPort()` sets it, and the post-create check destroys the
  node if the provider published anything else. An operator who writes
  `0.0.0.0` has concluded something false, so ember prints a warning naming the
  flag, the address and its position in the argv, before anything is rented. It
  is a warning and not a refusal because `cmd` is opaque argv on purpose:
  refusing would be ember claiming to know what `--host` means to a program it
  has never heard of. On the **local** provider the belief is partly true — the
  container sits on a Docker bridge, so a `0.0.0.0` bind really is reachable
  from this machine at the container's address.
- **A store is a channel between runs.** It is writable by every node that
  mounts it and there is no read-only mount, so the digest in `assets:` is what
  stands in for one. It matters more here than anywhere else in this
  repository: a substituted image model produces a picture that looks wrong, and
  a substituted code model produces code you paste.

### The image is the one piece that does not exist

An ember workload image has to carry `embernode` and sshd — ember runs the
workload as `ssh host command`, so an image that is only a server has nothing
to connect to — and this repository builds no such image for an LLM server.
An off-the-shelf `llama.cpp` image cannot be used as it stands for that
reason. So the `image:` line in that configuration is a sentence rather than a
value, and ember refuses it by the rule that already exists:

```
ember: image "EMBER-HAS-NO-LLM-WORKLOAD-IMAGE-SEE-THIS-FILES-HEADER" has no
tag: pin it to a tag or a digest, or two runs will not have measured the same
software
```

Everything else in it is real. The weights are pinned to a digest that
was read from Hugging Face rather than guessed — the tree API's `lfs.oid` and
the `X-Linked-ETag` on a HEAD of the resolve URL both answer it, which is two
independent confirmations for two requests and no bytes downloaded — and
`store create`, `store push`, `store ls` and `store verify` need no node and
no image, so the store half can be done today. That is deliberate: a committed
example whose placeholder reads as a value is what
[#18](https://github.com/Legion112/ember/issues/18) is about, and the loudest
placeholder is one whose refusal quotes itself.

It is also why this one is written down in `docs/` rather than committed as an
`ember*.yaml`: a configuration that refuses by construction is a worked
example, not an alternate configuration an operator would run.

### Whether to do this at all

An eight-hour working day at $1.00/hour is $8.00, and twenty of them is about
$160 a month for a card that is idle most of those eight hours. That figure is
as much the point of this configuration as the endpoint is: this is the
workload where renting is least favourable, and ember exists to make that a
decision backed by a measurement instead of a spec sheet. `ember spend` is
where the number lands, and it is worth having even when it argues against the
feature.

## Design notes

Things worked out but not built, kept where the reasoning was fresh rather
than in a commit message nobody will search for.

- [Long video is a pipeline](docs/long-video-is-a-pipeline.md) — **not built,
  and deliberately not sized.** Where a twelve-clip chain goes, why it is not
  `cmds` even though `cmds` turns out to chain, and which numbers cannot be
  estimated before one real clip has been generated.
- [Collecting GPU availability over time](docs/gpu-availability-history.md) —
  **built**, as `ember catalogue snapshot` and `ember catalogue report`; the
  note is kept as the reasoning the implementation follows, and says where the
  two differ. RunPod reports capacity only as it is right now, and a store
  pins every future run to one datacentre, so "which site reliably has this
  card" is a question a single sample cannot answer.

## Licence

MIT.
