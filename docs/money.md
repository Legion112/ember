# Money

A leaked node bills by the hour in silence. ember exists to make renting
hardware efficient, so every rule here is about not paying for something nobody
is using.

`AGENTS.md` carries the rules as one-liners. This page is the index to the
measurement and the refused alternative behind each of them.

| page | what is in it |
|---|---|
| [arithmetic.md](money/arithmetic.md) | `int64` microdollars, the hand-written parser, the money libraries that were measured and refused, the fuzz oracle |
| [rental-lifecycle.md](money/rental-lifecycle.md) | TTL and the deadline, destroy on a context that survives Ctrl-C, detaching, the intent record, recording how a rental ended |
| [ceilings.md](money/ceilings.md) | `max_hourly_usd`, `max_run_usd`, `max_day_usd`, `max_month_usd`, and the sweep that found three of five renting paths ungated |
| [cards-and-images.md](money/cards-and-images.md) | image pinning, the wrong-architecture abort, offer filtering and reclaim, why `min_cuda` could never be enforced |
| [stores-and-assets.md](money/stores-and-assets.md) | a store outlives every node that mounts it: mount refusals, digest verification, the S3 client, orphaned uploads, the pre-flight |
| [batching.md](money/batching.md) | overhead is per rental rather than per result, and what a resident model is worth |
| [sd-server.md](money/sd-server.md) | wire types captured from responses rather than guessed, previews, progress, cancelling a job without destroying the rental |
| [speech-and-kinds.md](money/speech-and-kinds.md) | piper, the one-method seam a second kind needed, the artifact type, the voice catalogue that keeps the request seal intact |
| [ledger.md](money/ledger.md) | what every run records, what `ember history` and `ember spend` read, the nodes route, measurements that live nowhere else |
| [scheduler.md](money/scheduler.md) | the claim lease and what reclaims it, capabilities keyed by rental shape, fit-or-create's one relation, and why the worker advertises nothing |

## If you are looking for a figure

Prices, timings and crossover points are scattered by subject rather than
collected, because each one belongs beside the decision it settled. The ones
asked for most often:

- **what a card costs per image**, and the resident-model crossover —
  [batching.md](money/batching.md)
- **what a store costs to keep, to fill and to verify** —
  [stores-and-assets.md](money/stores-and-assets.md)
- **what the per-rental overhead is** and where it goes —
  [batching.md](money/batching.md)
- **what a stalled create-side decision costs**, and what holds a claim —
  [scheduler.md](money/scheduler.md)
- **figures that exist in no other file**, rescued from deleted config
  headers — [ledger.md](money/ledger.md)
