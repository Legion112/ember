# ember — conventions

**This file is the index.** Each rule below is one line. The argument behind it,
the defect that motivated it and the measurement that settled it are in the
linked document — go there when you need the detail, not before.

## Language and tooling

- Go **1.27** for application code and tooling. `go.mod` pins it; the Makefile
  prefers `~/sdk/go1.27.0` so the compiler cannot drift from the declaration.
- Prefer the standard library and what is already here. **Adding a dependency
  is fine** when it saves writing something that already exists — do not
  re-implement a library to avoid a `go get`.
- **No Bash, Python or another scripting language for project tooling.**
  Non-trivial logic is a Go subcommand: `make setup` calls `ember setup`.
- External binaries at runtime (`docker`, `ssh` on the node) are fine, invoked
  from Go through an interface that has an in-process fake.
- The Rust/Leptos UI lives in `web/`; nothing in `internal/` imports from it.
  `make web` builds it.
- OpenTelemetry is linked, stays a no-op, and the SDK is not adopted —
  [docs/opentelemetry.md](docs/opentelemetry.md).
- Why each current dependency is here — [docs/dependencies.md](docs/dependencies.md).

## Layout

`cmd/<binary>` holds a ten-line `main` that calls `internal/cli`. Subcommand
dispatch is a hand-written switch over `flag.NewFlagSet`; logic lives in
`internal/`. There is no `pkg/`.

`internal/node` **imports nothing else from this module**, and
`TestNodeImportsNothing` fails the build if that changes. It is the layer that
can be tested with no Docker, no network and no provider account.

## State

ember keeps three stores, and they have deliberately different lifetimes:

| store | holds | lifetime |
|---|---|---|
| `internal/genstore` (SQLite) | the queue, and every request's history, parameters and cost | outlives the process |
| `internal/statestore` | the journal of nodes that are live right now | emptied by a clean run |
| the provider's own labels | the run id and the deadline | dies with the node |

The **label is the source of truth** about which run a node belongs to; the
journal is a cache. The event stream is the truth about what happened; the
columns beside it are a derived index.

## Testing

```
make test        # unit suite — no GPU, no Docker, no network, no credentials
make test-all    # everything, including the integration labs — needs Docker
make all         # fmt, vet, test, build
```

- Standard library `testing` only. No testify, no gomock, no testcontainers.
- One narrow interface per external system, with a `Recording*` fake.
- **Nothing may skip.** A test that skips itself reports success for something
  that never ran, and CI fails on `--- SKIP`.
- **Every new check is proved by reinstating the bug it guards and watching
  the suite go red**, judged on exit codes and never on greps.

When a particular test fails and the reason is not obvious, the full record —
what each gate is for, the defect it was written against, and the harness traps
that produce a false green — is in [docs/testing.md](docs/testing.md).

## Threat model

**The only thing on the internet that can reach an ember node is sshd,
authenticated by a key that never leaves this machine, verified against a host
key ember minted itself.**

Each rule below is that sentence made *unrepresentable* rather than merely
tested:

- Every listener is loopback, and `loopback.Bind` is the only way to name one.
- A create request can only ask for 22/tcp; the post-create check destroys a
  node whose provider published anything else.
- Host keys are minted here and injected, never trusted on first use.
  `hostkey.Pin` is the only `Trust` there is, so `ssh.InsecureIgnoreHostKey`
  cannot be typed into the program — not behind a flag, not in a test.
- No secret ever reaches argv. `ember serve` requires a token, and it is
  mandatory rather than advisory.
- **A request names a provider, a card, a kind and a batch of jobs, and nothing
  else** — no image, no store, no mount, no argv, no assets. Nothing an
  operator writes in a config reaches a node as a command either: every argv
  ember sends over SSH is one ember built.
- A tar stream from a rented machine is untrusted input; containment is
  enforced twice, by two different mechanisms.

Every seal, with the defect it was written against and the measurement that
proved it goes red — [docs/threat-model.md](docs/threat-model.md).

## Money

**A leaked node bills by the hour in silence.** ember exists to make renting
hardware efficient, so every rule here is about not paying for something nobody
is using:

- Money is `int64` microdollars, never a float.
- TTL is mandatory, and the deadline is derived in one place — written to the
  provider's label, the local record, and the node's own self-destruct timer.
- Destroy runs on `context.WithoutCancel`: Ctrl-C must never leak a GPU.
- The intent record is written **before** the create call.
- A provider that could not be reached is a finding, not a silence.
- Images are pinned to a tag or a digest; `:latest` is refused.
- Weights are verified by digest before any GPU time is spent on them.
- `max_hourly_usd`, `max_run_usd`, `max_day_usd` and `max_month_usd` bound a
  card, a rental and a calendar period; every path that can rent is projected
  against all four before anything is created.

The measurements behind each of those, the crossover points that decide a
design, and the options that were refused — [docs/money.md](docs/money.md).

## Style

Comments explain *why*, and name the defect that motivated them.

Prefer making a bug unrepresentable over testing for it. Fail loudly rather
than skip quietly. Allow-lists for anything dangerous. Parse, don't grep. Pin
external inputs.

Error strings are lowercase, wrapped with `%w`, and carry the remediation.
`main` prints `ember: <err>` to stderr and exits 1, or 2 when there are
findings, or 3 on a usage error.

Document CLI flag, config key and behaviour changes in `README.md` in the same
commit — guards read the tree and fail on an undocumented one.

The passes that measured whether the code follows these — [docs/style.md](docs/style.md).
