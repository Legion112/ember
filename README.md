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

```
ember up --provider runpod --gpu rtx4090 \
         --image ghcr.io/example/comfyui:v0.3.1 \
         --port comfyui:8188 --ttl 30m
```

That rents a node, waits for it, forwards `8188` to `127.0.0.1:8188` over SSH,
waits for you to finish, fetches the outputs and destroys the node — including
when you interrupt it.

## Why

Renting is roughly two orders of magnitude cheaper than owning while you are
still finding out what you need. An RTX PRO 6000 costs about 2.23M ₽ to buy and
about 134 ₽/hour to rent: at four hours a day that is an eleven-year payback,
and the card will be obsolete long before. ember exists to make the rented path
convenient enough that buying hardware becomes a decision backed by
measurements instead of by spec sheets.

## Status

Early. Working today:

| | |
|---|---|
| `internal/node` | domain core — nodes, offers, specs, integer money, states |
| `internal/loopback` | the only listener in the program, provably loopback |
| `internal/cli` | subcommand dispatch, exit codes |
| `cmd/check-coverage` | per-package coverage gate with `-strict` |

Next: the `local` Docker provider and the Docker lab, then SSH tunnelling, then
RunPod and Vast.ai. See `AGENTS.md` for the rules all of it is written against.

## Two properties that outrank features

**Nothing is reachable from the internet.** Every forwarded port binds to
loopback on your machine; the only thing a provider exposes publicly is sshd,
key-only, verified against a host key ember generated and injected itself.
`net.Listen` appears in exactly one package and a test fails the build if that
changes.

**A leaked node costs real money.** An orphaned H100 burns about 175 ₽/hour in
silence. Every node carries a deadline enforced in several independent places,
destroy runs on a context that survives Ctrl-C, and `ember ls --all` exits
non-zero when any provider could not be reached — because "I could not check"
must never look like "nothing is running".

## Build

```
make            # fmt, vet, test, build
make test       # unit tests, race detector, no Docker or network needed
make cover-check
make test-integration   # Docker labs; needs a working Docker daemon
```

Go 1.27. The Makefile prefers `~/sdk/go1.27.0` when present so the compiler
cannot drift from what `go.mod` declares.

## Configuration

`ember.yaml` holds image names, GPU filters, ports and budgets, and is safe to
commit. Credentials are not in it: they come from the environment
(`RUNPOD_API_KEY`, `VAST_API_KEY`) or from `~/.config/ember/credentials.yaml`
with mode 0600. Nothing secret is ever passed as a command-line flag, because
`/proc/<pid>/cmdline` is world-readable.

## Licence

MIT.
