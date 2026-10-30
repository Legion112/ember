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
gh workflow run "workload image" -f tag=sd-cuda --ref master   # in CI
make publish-sd-cuda TAG=sd-cuda                               # from here
```

The local path needs `docker login ghcr.io -u <user>` with a classic token
carrying `write:packages`, and never takes the token on a command line.

Both build the image, run the publish lab set named once as `PUBLISH_LABS` in
the `Makefile`, and push only if it passes. That sharing is the point: the
workflow used to carry its own list of labs, and it drifted — a lab written to
catch a published image whose `embernode` did not understand `embernode fetch`
was not in it, so the one publish it existed to guard was the one place it
never ran.

Locally the compile takes about nine minutes on sixteen cores against thirty
on a four-vCPU runner, and the push is seconds rather than minutes: the runtime
base is pinned by digest, so only the layers that actually changed go up.

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

## Build

```
make            # fmt, vet, test, build
make test       # unit tests, race detector, no Docker or network needed
make lint
make cover-check
make test-integration   # Docker labs; needs a working Docker daemon
```

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

```yaml
min_cuda: "12.6"
```

`min_cuda` restricts placement to hosts whose CUDA runtime is at least this,
which matters because the image carries a runtime and a host older than it
cannot run the image at all. Empty sends no constraint — which is what ember
did for its whole life before this key existed. It is empty by default because
nothing has yet confirmed against the live API that the field actually filters
placement, and a constraint that narrows capacity without doing anything would
be worse than none.

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

`mount` is refused if it would shadow `/root` or `/etc`, which hold the
authorized_keys and host key `embernode` writes at startup — a store there
leaves sshd with neither and a handshake failure that reads exactly like an
attack. `/workspace` is refused too: it is the provider's default mount path
*and* where ember writes outputs, so a store there bills for every render for
as long as the store exists.

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
