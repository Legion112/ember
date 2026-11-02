# Long video is a pipeline

**Status: not built, and deliberately not sized. What is decided is that the
chain does not go in `cmds` and that nothing refuses or warns about one.
What is open is the whole feature, which stays behind #18 (one clip measured)
and #16 (a job that can ask for video at all). The issue this note settles,
#20, asked for the reasoning to be written down rather than for anything to
be built.**

**One claim in that issue is corrected here.** It states that `cmds` entries
have "no way to name a file the step before produced". They do, and it was
checked rather than reasoned about: a `cmds` chain works. What does not work
is everything that depends on ember *reading* a result, which is the claim
worth keeping.

## The capability, and why it is a pipeline

`README.md` records it under Video: no open-weight model generates a minute in
one pass. 81 frames at 16 fps is about five seconds, and the frame counts that
map to clean latent lengths are 33, 77 and 81. A minute is twelve clips, each
continued from the last frame of the one before — eleven seams, and the seams
rather than the fidelity inside a clip are what a long-video feature would be
judged on.

So the question is where the chain lives. `cmds` looks like the answer: it
already runs several commands on one rental and brings each one's output home
before the next starts.

## What was measured

`cmds` chains. Entry *n+1* can read entry *n*'s output, because the output is
still on the node.

Run on the local provider — free, no card, the `ember:lab` node image — with
this configuration:

```yaml
provider: local
image: ember:lab
ttl: 10m
cmds:
  - [sh, -c, "mkdir -p /workspace/out && echo 'frames of clip 01' > /workspace/out/clip01.txt && ls -l /workspace/out"]
  - [sh, -c, "echo '--- step 2 sees on the node: ---'; ls -l /workspace/out; sed 's/^/from step 1: /' /workspace/out/clip01.txt > /workspace/out/clip02.txt && cat /workspace/out/clip02.txt"]
outputs:
  - /workspace/out:renders
```

`ember up` printed:

```
step 1/2: sh -c 'mkdir -p /workspace/out && echo '\''frames of clip 01'\'' ...
total 4
-rw-r--r-- 1 root root 18 Nov  2 20:15 clip01.txt
fetched 1 file(s), 18 B
step 2/2: sh -c 'echo '\''--- step 2 sees on the node: ---'\''; ...
--- step 2 sees on the node: ---
total 4
-rw-r--r-- 1 root root 18 Nov  2 20:15 clip01.txt
from step 1: frames of clip 01
fetched 1 file(s), 31 B
```

Step 2 listed step 1's file, read it, and wrote a file derived from it. Both
arrived home, each sent exactly once: 18 bytes on the first fetch and 31 on
the second, which is `clip02.txt` alone.

**Why, in the mechanism.** `transfer.TarCommand` builds
`tar -czf - -C <dir> --newer=<marker> -- <base>`. That is a read: there is no
`--remove-files` and nothing else on the fetch path writes or deletes on the
node — the package comment says so in its first paragraph, *"nothing written
on the node"*. The marker bounds what is **sent**, not what **exists**: it is
`/tmp/.ember-fetch-mark`, touched by `runSteps` between steps so a file
already home is not transferred twice. The node's filesystem lives for the
rental, so every step after the first starts with every earlier step's output
underneath it.

That property is now asserted, by
`TestTransfer_AFetchLeavesTheOutputsOnTheNode` in
`test/integration/transfer_test.go`, because it had been an accident of
implementation that a configuration may now depend on. The obvious future
tidy-up — `tar --remove-files` to save the node's disk, or an `rm` between
steps — would silently break every chain while the whole suite stayed green.
Checked by doing exactly that: `--remove-files` added to `TarCommand` makes the
lab report *"the fetch removed the output from the node, so no cmds chain can
work"*, and removing it makes it pass again.

**One thing the lab found about `--newer` itself.** GNU tar compares **ctime
as well as mtime**, so the first version of the lab — which set the first
file's mtime to a fixed old date with `touch -d` — saw tar send it again, and
blamed the marker for the test's own setup. In the real flow a step's output
is written naturally, so mtime and ctime agree and the marker behaves; the lab
now orders its writes the way `runSteps` does rather than dating them. Worth
recording because it is the one way the per-step fetch could re-send a clip
that is already home: anything that touches an earlier output's metadata makes
it new again to tar.

## What `cmds` still cannot do, which is the claim worth keeping

The data flows. What does not is everything that needs ember to understand a
result:

- **A filename ember did not know in advance.** Chaining works only because
  the operator fixed every output path in the argv they wrote. A workload that
  names its own output — a seed, a timestamp, a hash — cannot be chained
  without a shell that globs for it.
- **A last frame.** The chain's real input is not the previous clip but its
  final frame, which means a decode between every pair of generations. Twelve
  clips is twenty-three `cmds` entries, eleven of them `ffmpeg`.
- **Anything inside a step.** A clip is minutes of sampling and `cmds` reports
  one line per entry: a description, a duration, a cost. There is no step
  counter and no preview, which is #16's argument arriving in the mode where
  it hurts most.

The seal holds in all three: ember does not read argv, so it cannot learn the
filename, cannot find the frame and cannot report the pass. That is the same
sentence as the entry in `AGENTS.md` about `cmds` versus `sd.jobs` — *"the one
place ember stops being workload-agnostic"* — reached from the video side.

## Why you should not chain video in `cmds` anyway

Four reasons, none of which is "it does not work".

**It is a `curl` in a config.** Each seam is a shell one-liner that knows
`ffmpeg`'s flags, the container format the generator wrote, and which frame
index is last. `AGENTS.md` already rejected that shape for the served path:
*"a `curl` in a config is the same knowledge maintained by hand"*. Here it is
maintained by hand eleven times, in YAML, with nothing checking that entry 7
names the file entry 6 wrote. A typo there is found at step 7 of a rental
whose first six steps have been paid for.

**It is all-or-nothing per rental.** The clips come home as they are produced,
which is the per-step fetch doing exactly its job — but the *chain's* state
does not. A failure at clip 7 stops the batch, and clip 6's last frame dies
with the node. Resuming means either re-generating the first six clips and
paying again, or getting that frame back onto a new node — and ember's only
inbound path to a node is the store, whose assets are verified by a digest
known before the run, and whose writability is the cross-run channel the
digests exist to detect in the first place. So the `ttl` has to cover all
twelve clips, and `max_run_usd` is checked against the offer's price times
that `ttl` before anything is rented. A twelve-clip chain is one ceiling
decision made up front about a rental of hours.

**The reporting is wrong for the shape.** Above, and worth repeating as a cost
rather than as a limitation: an operator watching a twelve-clip chain watches
twelve lines appear over hours, and cannot tell a clip that is sampling from
one that is wedged. That is the condition `ready_timeout` and the
exited-server race were written against on the served path, with no equivalent
here.

**Intermediates come home as noise.** Every `ffmpeg` frame written inside an
`outputs:` path is fetched, so a twelve-clip chain delivers eleven stray PNGs
beside the clips. Avoidable — write them outside the output paths — and worth
knowing before rather than after.

## What the chain would cost if it were built

It belongs on the served path, for the reason progress streaming does: that is
the one place ember reads its own payload, so it is the one place that can take
the last frame of job *n* and hand it to job *n+1*. #16 is the prerequisite —
`vid_gen`, `supported_modes`, frame-count validation, a video output writer.

**It is a different decision from the `sd.serve` one, not a larger instance of
it.** `sd.serve` bought one piece of knowledge: the shape of a generation
*request* and of its progress. ember learned what a sampler is — parameters it
writes itself and a counter it reads back. A chain asks for a second kind: the
*relationship between two results*, which means reading what the model
produced rather than what ember asked for. The last frame of a clip is pixels,
not parameters.

The two trades also differ in how well they are known. `sd.serve` was decided
against a measurement — 1.81 s per job fresh against 0.43 s served, a fixed
~6 s to start the server, crossover at about five jobs — and the measurement
corrected the plan in both directions. A chain has no such number, and the
feature's own shape is not settled: whether an acceptable seam needs one frame
of context, several frames, or the model's own latent continuation decides
whether this is a small feature, a medium one, or a model-specific one. Those
are three different features and nothing observed so far picks between them.

## What is not estimable until #18 runs

Not "the cost is unknown" generally. These specifically, and each names what
would settle it.

**What a `vid_gen` response carries.** The captured
`internal/sdserver/testdata/capabilities.json` has one key under
`output_formats_by_mode` — `img_gen`, with `["png","jpeg","webp"]`. Nothing
says whether a video job answers with a container file, a frame sequence, or
base64 frames. That single fact decides whether a seam needs a decode at all:
if the response carries frames, the last one is already in hand and the
`ffmpeg` entry above does not exist. Settled by one clip.

**Whether `end_image` is reachable on a loaded video model.** #20 reads the
captured capabilities as a hint that upstream expects this shape, and it is
one — but a weaker one than it looks. `end_image` and `ref_videos` appear
under `generation_features`, which is the binary's build-time list of 27
features. The loaded model's own lists, `features` and
`features_by_mode.img_gen`, carry 12 keys and `end_image` is **not** among
them. Whether `features_by_mode["vid_gen"]` carries it for a Wan checkpoint
has never been observed. Believing the build-time list instead would be the
same mistake as `capabilities.model` and RunPod's `allowedCudaVersions`: a
schema read in place of a response, which this repository has now made twice.

**Whether a frame count is even bounded by the server.** `limits` in the same
capture carries `max_batch_count`, `min`/`max_width`, `min`/`max_height`,
`max_queue_size` and the upscale bounds — and **no frame-count key at all**.
The counts 33, 77 and 81 in `README.md` come from the latent arithmetic, not
from anything a server reported. #16 wants a count outside them to be a
refusal rather than a clamp; today there is nothing published to refuse it
against, so the refusal would be built from a number ember asserted rather
than read.

**Per-clip overhead.** Unknown, and not extrapolable from what is measured.
The two figures that exist are both images: 1.81 s against 0.43 s per job on
sd15 Q8_0, and 4.5 s against 86 s per image on FLUX.2 depending on whether the
node held 56 GiB in page cache. Wan 2.2 is two 14 GB experts, so the reload a
served path would remove is larger than either figure was taken against, and
an estimate built by scaling one of them would be arithmetic from the wrong
table — the mistake the "~8% this was planned around" entry in `AGENTS.md`
records. **The measurement is cheap and `cmds` is the instrument:** two
identical clips as two `cmds` entries, and the difference between the two
per-step card-time lines ember already prints is the per-clip reload. That is
the one good reason to reach for `cmds` here.

**Seam quality.** Not a number and not a measurement — a judgement on a real
clip, which is exactly why #20 depends on #18 rather than the other way
round.

## Should `cmds` refuse, or warn, when its entries look like a chain?

No, and the usual argument is not the strongest one available.

The usual argument is the seal: telling a chain from a batch means reading
argv, and `cmds` exists not to. Any heuristic would be a substring match over
text that is deliberately arbitrary — a prompt is English and an output path
is a path — which is the `NormalizeGPU` substring failure with worse input.

The stronger argument is that the warning would be false. A refusal or a
warning would say "these entries will not chain", and they will: the run above
is three lines of evidence that they do. The confusion #20 anticipated — three
entries producing "three independent runs and a confusing result" — comes from
a belief about ember that is not true. Warning about it would teach an
operator to distrust a mechanism that works, which is the same cost as
`allow_interruptible` reading like a safety switch while guarding nothing.

What the gap actually was is documentation, and it is closed in two places
rather than one: the persistence is stated in `README.md`'s `cmds` section, so
an operator can rely on it, and asserted by a lab, so a future tidy-up cannot
remove it quietly. A property an operator is told to depend on and nothing
checks is the shape of defect this repository keeps finding.

## What would make this note wrong

- A `vid_gen` response that carries frames rather than a container file: the
  `ffmpeg` seam disappears and a `cmds` chain gets considerably less ugly,
  though every other objection above survives.
- A measured seam that is acceptable with one frame of context: the served
  feature is then small enough that arguing about `cmds` was the expensive
  part.
- Anything that removes a step's output from the node between steps: the chain
  stops working, and the lab named above is what should stop that from landing
  unnoticed.
