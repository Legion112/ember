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
  DROPs looks exactly like a closed port from here.
- **No ember function takes an `ssh.HostKeyCallback`.** `hostkey.Trust` has one
  unexported method and that package holds no permissive implementation, so
  `ssh.InsecureIgnoreHostKey()` cannot be typed into the program — not behind a
  flag, not in a test. It is the most common way an SSH client ships with no
  server authentication at all, and "we grep for it in review" has a 100%
  failure rate eventually.
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
