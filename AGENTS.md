# ember — conventions

This project is **Go-first**, like `go-tun`, `transcription` and `sms-gateway`.

- Go **1.27** for all application code and tooling. `go.mod` pins it; the
  Makefile prefers `~/sdk/go1.27.0` so the compiler cannot drift from the
  declaration just because a system package updated.
- Prefer the standard library and the dependencies already here over new ones.
  The whole dependency list is `golang.org/x/crypto`, `github.com/docker/docker`,
  `gopkg.in/yaml.v3` and `golang.org/x/tools` (the coverage gate). Adding a
  fifth needs a reason written down.
- **Do not add** Bash, Python or another scripting language for project
  tooling. Non-trivial logic is a Go subcommand. `make setup` calls
  `ember setup`, never a shell script — `transcription` already took this
  direction and `go-tun`'s sixty lines of inlined `ssh` in a Makefile recipe is
  what the alternative looks like a year later.
- External binaries at runtime (`docker`, `ssh` on the node) are fine when
  invoked from Go through an interface that has an in-process fake.

## Layout

`cmd/<binary>` holds a ten-line `main` that calls `internal/cli`. Subcommand
dispatch is a hand-written switch over `flag.NewFlagSet`; logic lives in
`internal/`. There is no `pkg/`.

`internal/node` **imports nothing else from this module**, and
`TestNodeImportsNothing` fails the build if that changes. It is the layer that
can be tested with no Docker, no network and no provider account, and the
moment it imports a sibling, the sibling's dependencies become its own.

## Testing

- Standard library `testing` only. No testify, no gomock, no testcontainers.
- One narrow interface per external system, with a real implementation and a
  `Recording*` fake carrying `Calls`, `Outputs`, `Errors` and `FailOn` — the
  same field names as go-tun's `linux.RecordingRunner`, so they read without
  being learned.
- `go test ./...` must need no GPU, no Docker, no network and no credentials.
  The labs are separate and gated twice: `//go:build integration` **and**
  `EMBER_INTEGRATION=1`.
- Coverage is gated per package by `cmd/check-coverage`, with `-strict`: a
  package in neither the gated list nor the exclusion list **fails the build**.
  Exclusions are printed on every run so they appear in CI logs and review
  diffs rather than living unread in a Makefile variable.
- **A test that skips itself reports success for something that never ran.** CI
  greps for `--- SKIP` in both the unit and the lab logs and fails on a hit.

## Threat model

ember's posture is one sentence: **the only thing on the internet that can
reach an ember node is sshd, authenticated by a key that never leaves this
machine, verified against a host key ember minted itself.** Everything below is
that sentence made unrepresentable rather than merely tested.

- **Every listener is loopback, and `loopback.Bind` is the only way to name
  one.** `Bind`'s field is unexported with no literal path, so the only Binds
  in the program came from `Port`, `Port6` or `ParseBind`. The bug: a `bind:`
  string in a config reaching `net.Listen` as `":8188"` publishes ComfyUI —
  which has arbitrary file read and arbitrary code execution through custom
  nodes — to the whole coffee-shop LAN.
- **`net.Listen` appears in exactly one package**, and
  `TestNoListenOutsideLoopback` walks the tree and fails otherwise. Every guard
  above it is worth nothing if a provider package opens its own listener
  because it was two lines shorter.
- **`"localhost"` is not a loopback address and `ParseBind` refuses it.** It is
  a name resolved through `/etc/hosts`, NSS and possibly DNS; a host that maps
  it to a LAN address publishes the port, and nothing in the config looks
  wrong. A name is never proof of loopback.
- **`ParseBind` lets `netip` decide before choosing an error message.** The
  first version guessed "is this a hostname?" with a letters-and-dots
  heuristic and classified `":8188"` and `"[::1%eth0]:8188"` as hostnames,
  giving both the wrong advice. Guessing ahead of the real parser sends the
  reader somewhere the bug is not.
- **Both loopback families are bound.** A v4-only listener is invisible to a
  browser that resolves `localhost` to `::1` first, and the operator reads
  connection-refused on a working tunnel as a bug in ember.
- **Create requests can only ask for 22/tcp.** `exposure.PortRequest`'s
  `public` field is unexported and only `SSHPort()` sets it. The bug: one
  `8188/http` port on RunPod serves the workload at
  `{id}-8188.proxy.runpod.net` with **no authentication**, scanned within
  minutes.
- **The post-create exposure check destroys the node on failure.** A provider
  changing a default reintroduces public exposure that the request shape cannot
  see. A warning is useless here: the node is already wrong.
- **The outside-in probe is evidence, not proof, and says so.** A firewall that
  DROPs looks exactly like a closed port from here. Each port gets its own
  timeout, not a share of one budget: a run of dropped ports would otherwise
  eat the budget between them and the rest would report "closed" because the
  clock ran out, which is a false pass on the only answer that matters.
- **"Published nothing" and "never reported" are different answers.**
  `node.Exposure()` returns `([]Exposed, bool)` and an unreported node fails
  every assertion. They are the same empty slice, and treating the second as
  the first means a provider whose mapping code forgets to populate exposure
  gets waved through — the check still runs, still passes, and verifies
  nothing. `WithExposed` stores an empty report as an empty slice and never as
  nil for the same reason: `append([]Exposed(nil), es...)` returns nil for
  empty input, which turned the safest possible answer into a failed assertion.
- **`Caps.PublicSSH` defaults to the stricter check.** False — the zero value —
  means the provider publishes nothing at all. A provider author who forgets
  the field gets `AssertNoPublishedPorts`, and the failure is a destroyed node
  with a loud message rather than a public port nobody looks at.
- **Exposure is derived from what the daemon or API reports back, never from
  what the create call asked for.** `dockerengine.Binding` carries the host IP
  and not just the port, because Docker's own default is to publish on
  `0.0.0.0` and the difference between that and `127.0.0.1` is the difference
  between a port only this machine can reach and one the whole LAN can. A
  check that reads ember's own request cannot catch a provider doing something
  else, which is the entire thing it is for.
- **The exposure assertion lives above the providers, not inside them.** A
  provider checking its own exposure is a provider grading its own homework.
- **The label is the source of truth; the journal is a cache.** A record is
  joined to a live node by provider node id *or* by the run id in the node's
  label. Matching on node id alone meant a run killed between `Create`
  returning and the id being written produced two findings -- an orphaned
  record that could not name its node, and an untracked node no record
  mentioned -- and `reap` could act on neither, although both carried the same
  run id the whole time. That defeats the entire reason the record is written
  before the create call.
- **Each record names the process holding its node.** Without it, a run killed
  with SIGKILL leaves a node that is running, recorded and inside its deadline
  -- indistinguishable from a healthy long-running session -- and `ls` prints
  `ok` over a GPU billing with nobody watching. The TTL still bounds the loss;
  the holder makes it visible while it is happening. It answers "not
  abandoned" for everything it cannot be sure of: no holder recorded, a record
  from another machine, a platform where liveness cannot be checked. A false
  "abandoned" would tell an operator their running session is dead, which is
  the one mistake that would make the check not worth having.
- **One name for a path, not one per call site.** The journal's "nodes"
  subdirectory was a literal in the CLI and a second literal in the lab
  harness. They drifted, the harness read the wrong directory and found
  nothing, and a lab asserting "a clean run leaves no records" passed without
  checking anything. The harness now reads through `statestore` itself.
- **A provider API's int port is range-checked, never narrowed.**
  `uint16(70000)` is 4464. On the endpoint path ember would dial a port the
  node is not listening on and blame the node for the timeout; on the exposure
  path a public port would be reported as a *different* port, making an
  assertion about what is reachable an assertion about the wrong number.
- **A filter matches a model name it is a prefix of, at the capacity boundary
  only.** `NormalizeGPU` deliberately keeps the capacity suffix, because an
  A100 40G and an A100 80G are not substitutes -- so `--gpu h100` has to
  accept `h100-80g`, and string equality meant it accepted neither. `a100`
  accepts `a100-40g` and `a100-80g` but not `a1000`, because the next
  character must be the separator. Same failure as the substring bug
  `noiseWords` exists to prevent, reached from the other direction: the card
  was rentable, the filter matched nothing, and the output was identical to a
  provider with no capacity.
- **Memory type is not memory capacity.** `HBM3` and `GDDR6X` are noise words;
  `80GB` is not. RunPod spells a card "NVIDIA H100 80GB HBM3", and leaving the
  memory type in welded it onto the capacity suffix as `h100-80ghbm3`.
- **No escape hatch for a seal, not even for a test.** The RunPod mapper's
  refusal of a public non-SSH port is unreachable, because only
  `exposure.SSHPort` can mint a public request. The first attempt to cover it
  exported a helper from `exposure` that built one -- a hole in the wall the
  seal exists to be. The branch stays uncovered; an escape hatch for tests is
  an escape hatch.
- **A tar stream from a rented machine is untrusted input.** Containment is
  enforced twice, by two different mechanisms: the member name is checked
  component by component, and every write goes through an `os.Root`. A string
  check misses a spelling; a Root cannot tell ember the archive was trying
  something.
- **A hostile member name is refused, not cleaned.** `path.Clean("/../a.png")`
  returns `/a.png`: it *absorbs* the `..` because nothing is above root. An
  earlier version cleaned first and then looked for a leading `..`, so the
  check could never fire -- every hostile name was silently rewritten into a
  valid one, nothing escaped because the Root held, and ember reported success
  for a file called something else. An absolute member name is refused for the
  same reason: ember's own tar command never produces one.
- **Only regular files and directories are extracted.** A symlink, hard link,
  device node or fifo is skipped and *named* in an error. ember is retrieving
  generated media from a machine it rented, and refusing the kinds outright is
  shorter than reasoning about where each one could point.
- **A remote command is argv, and every word is quoted.** An SSH exec request
  is handed to the remote shell, so a command assembled by concatenation is a
  shell injection with the operator's own config as the input. ember used to
  refuse anything unquotable, which was right while every remote command was
  tar over config-file paths; a workload cannot work that way, because a
  prompt is arbitrary text. So `sshx.Quote` wraps in single quotes, where
  every character except the single quote itself is literal, and escapes that
  one as `'\''`. A unit test checks the string ember builds; a lab
  round-trips the same hostile inputs through a real sshd and a real `/bin/sh`
  and compares what arrived, because whether the shell agrees is a question
  for a shell. Both were checked by breaking the quoting and watching them
  fail.
- **A workload runs over the SSH connection, not as the entrypoint.** A node
  ember cannot reach is a node ember cannot destroy, so there is no value in
  a workload outliving the connection — and running it here means the output
  streams live and the exit status is known when it happens rather than
  polled for through a file.
- **A node rented to run something stops when it has run it.** Holding
  afterwards bills for a machine with nothing left to do.
- **The exit status is read after the stream, and always.** A `tar` that failed
  halfway produces a valid prefix of an archive: the extraction is happy, the
  data is incomplete, and that is exactly how an output fetch loses files
  quietly. GNU tar over a missing path exits non-zero and writes a valid
  *empty* archive, which without this would read as a successful fetch of
  nothing.
- **Outputs are fetched on a context that survives Ctrl-C.** The node is about
  to be destroyed either way, whatever it produced is about to stop existing,
  and a run that failed halfway is often the one whose partial output and logs
  are worth having.
- **"Already there" has two meanings.** `mkdir` returning EEXIST over a
  *directory* is fine -- two outputs can land in overlapping trees. Over a
  *file* it is not: nothing under it can be written, and treating them alike
  reported success for a directory that was never created.
- **A fake must reproduce the behaviours that cost money to find, including
  the unhelpful ones.** `runpodfake` reports no host key, leaves `ssh.direct`
  null for a while after the status reads RUNNING, nulls `ssh` and `runtime`
  on every list entry, and publishes a web terminal port nobody asked for --
  because each of those is a real RunPod behaviour that broke ember once. A
  fake that is tidier than the service it stands in for tests a service that
  does not exist. The previous RunPod tests used one-off handlers returning a
  canned blob, which cannot express the thing that actually breaks: time.
- **A regression test that has never failed proves nothing.** Each of these
  was checked by reinstating the bug and watching the lab go red. The
  trust-on-first-use lab fails with the exact warning the live run printed.
- **Clean up what you did not think to look for.** The lab harness removed
  containers and not the networks beside them. Nothing bills for a bridge
  network and no assertion mentioned one, so a leak stayed invisible until
  Docker had subnetted its whole address pool and every run failed with "all
  predefined address pools have been fully subnetted". `AssertNoStray` now
  covers networks, and a lab asserts `ember up` leaves none.
- **`AssertNoStray` could not fail, and nobody noticed for the life of the
  suite.** The lab's cleanup ran `Reap()` and then `AssertNoStray()` in that
  order, so the reap removed every container and network and the assertion
  then looked at an empty daemon. Its own comment claimed it ran "before
  AssertNoStray records the failure, never instead of it"; the code did the
  opposite. Every lab's final assertion was passing because there was nothing
  left to see — the exact failure mode the `--- SKIP` gate exists to prevent,
  reached without skipping anything. The cleanup now snapshots the daemon,
  then reaps, then reports the snapshot, so both properties hold: one failing
  lab still cannot cascade, and a leak is still named. Found by reinstating a
  deliberate leak and watching the suite stay green.
- **A lab helper owns what it creates.** `CreateVolume` registers its own
  cleanup rather than leaving each lab to remember, because cleanups run
  last-registered-first: the seeded volume goes before the stray check looks,
  while a volume ember itself left behind is still reported. The first lab
  that forgot would otherwise have turned a working check into a failing
  suite, which is how a working check gets weakened instead of obeyed.
- **A lab that leaks on purpose says so.** The two SIGKILL labs exist to leave
  a node behind, and the first run of the repaired stray check reported both.
  They call `Reap()` themselves once they have finished asserting, which
  declares the orphan as deliberate and keeps the check unconditional for
  every lab that leaks by mistake. Relaxing the check instead would have put
  it straight back where it was.
- **Known gap:** `local.Destroy` finds the run id by inspecting the container,
  so a node whose container was already removed by something else leaves its
  network behind. `reconcile` holds the run id and is the right place to fix
  it; nothing does yet.
- **A lab seeds state out-of-band, through Docker, not through the code under
  test.** Widening `sshx.Runner` with stdin support so a test could write a
  file would be a production interface shaped by a test's convenience.
- **Tests inject credentials rather than resolving them.** Left to read the
  environment, every CLI test picks up the developer's own `RUNPOD_API_KEY`,
  registers the real provider and makes live API calls -- behaving differently
  from one machine to the next. `deps.CloudBaseURL` exists for the same
  reason: a test that registers a cloud provider must not be able to reach it.
- **No ember function takes an `ssh.HostKeyCallback`.** `hostkey.Trust` has one
  unexported method and that package holds no permissive implementation, so
  `ssh.InsecureIgnoreHostKey()` cannot be typed into the program — not behind a
  flag, not in a test. It is the most common way an SSH client ships with no
  server authentication at all, and "we grep for it in review" has a 100%
  failure rate eventually.
- **The pin is built from the key ember minted, never from the one the
  provider reports.** ember generated it and injected it, so it already knows
  which key the node must present and never has to ask. Reading the
  provider's answer first was a real bug: the local provider echoes the key
  back on a container label so it worked there, while RunPod reports nothing,
  and ember's first real cloud provision fell through to trust-on-first-use
  against a brand-new IP — the one case TOFU cannot defend, and the exact
  thing minting the key exists to avoid. It logged a warning, and a warning
  is not a security control.
- **A provider reporting a key that differs from the injected one is a
  refusal.** It has substituted something, and continuing would pin the
  substitution.
- **A node with no minted key is refused, not downgraded.** TOFU is
  unreachable through `WithNode`, which mints for every node; reaching that
  branch would mean ember had stopped injecting, which is worth a refusal
  rather than a quiet fallback.
- **A provider may name public ports it adds unconditionally, and only
  those.** RunPod maps its web terminal port (19123/http) to every pod with no
  way to disable it — found when ember's first real provision refused the node
  and destroyed it. `Caps.PlatformPorts` names it; everything else still
  destroys the node. The concession is only safe while nothing in the node
  image listens there, so that is a lab assertion about the image rather than
  a promise from the provider, and a workload asking for that port is refused
  outright.
- **Host keys are minted here and injected, not trusted on first use.** TOFU
  against an IP that has never had a key is a free MITM for anyone on the path.
  Injecting through the provider's API concedes the key to the provider, which
  already has root on the node and therefore learns nothing; it closes the
  window against everyone else.
- **Pins are keyed by node id, not host:port, and die with the node.** A pin
  keyed by IP matches whoever gets that IP next, and the resulting churn is
  exactly what talks people into disabling host-key checking.
- **A host key mismatch is a hard error with no `--force`.** A prompt is a
  bypass with extra steps; the correct response for an ephemeral node is
  always to destroy it.
- **`node.Secret` carries key material and prints a fingerprint.** One
  `slog.String("hostkey", string(pem))` at debug level writes a private key
  into the journal permanently, and nothing about that line looks wrong in
  review. `String()` is redacted too, because a stray `%s` in an error is the
  other way it escapes.
- **No secret ever reaches argv.** `/proc/<pid>/cmdline` is world-readable, so
  a key or token passed as a flag is readable by every user on the box for the
  life of the process. Secrets live in `~/.config/ember/credentials.yaml`
  (0600) or the environment, and reach providers in the API's JSON body.
- **The remote end of a forward is built by `tunnel.RemoteLoopback(port)` and
  never parsed from input.** `direct-tcpip` connects anywhere the node can
  reach, including the provider's metadata service; a free-form remote host
  makes every ember node an open relay billed to the operator.

## Money

A leaked node bills by the hour in silence. These are not style preferences.

- **`node.State.Billable()` is true for everything except `StateDestroyed`**,
  including `StateInvalid`, `StateFailed` and `StateStopped`. A stopped RunPod
  pod still bills for its volume; "not running" is not "not charging". A state
  added later is billable by default, so the author has to think about it to
  make it free.
- **Money is `int64` microdollars, never a float.** `0.1+0.2 > 0.3` is true in
  float64, so a budget ceiling built by arithmetic occasionally refuses an
  offer at exactly the ceiling and occasionally admits one above it. A budget
  that is sometimes wrong by a rounding error is a budget nobody trusts.
  `ParseUSD` is hand-written for the same reason: `ParseFloat("0.29")*1e6` is
  289999.
- **TTL is mandatory and the deadline is derived in one place.** The deadline
  is written into the provider's label, the local state record and the node's
  own self-destruct timer; three copies computed at three call sites are three
  copies that can disagree, and the one that disagrees is the one that does not
  stop the bill.
- **Destroy runs on `context.WithoutCancel`.** Ctrl-C cancels the context the
  run was using, and a destroy call on that context fails instantly — the
  single most likely way to leak a GPU.
- **The intent record is written before the create call.** The window between
  the request and reading the response is where an orphan with no local record
  is born.
- **A provider that could not be reached is a finding, not a silence.**
  `ember ls --all` exits non-zero when any provider failed to answer: "I could
  not check RunPod" must never render as a clean bill of health.
- **Images are pinned to a tag or a digest; `:latest` is refused.** The point
  of ember is to measure what a model costs and how long it takes, and two runs
  a week apart that both say `:latest` may not have run the same software.
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

## Style

Comments explain *why*, and name the defect that motivated them — see
`node.NormalizeGPU`, where substring matching once turned `"A100 SXM4 80GB"`
into `a1004-80g` and made a whole provider look like it had no capacity.

Prefer making a bug unrepresentable over testing for it. Fail loudly rather
than skip quietly. Allow-lists for anything dangerous. Parse, don't grep. Pin
external inputs.

Error strings are lowercase, wrapped with `%w`, and carry the remediation.
`main` prints `ember: <err>` to stderr and exits 1, or 2 when there are
findings, or 3 on a usage error.

Document CLI flag, config key and behaviour changes in `README.md` in the same
commit.
