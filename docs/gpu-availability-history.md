# Collecting GPU availability over time

**Status: not implemented. This is a design note, written while the reason for
it was fresh.**

## The question

RunPod reports whether a GPU is available right now. Does it report what was
available last week?

**No.** Checked against both live OpenAPI documents on 2026-10-30:

| API | paths | anything historical |
|---|---|---|
| `api.runpod.io/v2` | 44 | none |
| `rest.runpod.io/v1` | 23 | none |

`availability` is a bare enum — `HIGH`, `MEDIUM`, `LOW`, `NONE` — on
`GpuType`, `CpuType` and `DataCenterAvailability`. It carries no timestamp, no
window, and no history. Every read is a point sample, and nothing on the
provider's side remembers the previous one.

So if ember wants to know how capacity behaves over time, ember has to keep
the samples.

## Why it is worth keeping, and why it became worth keeping *now*

Two observations.

**Availability moves on the scale of a day.** The committed fixture
`internal/provider/runpod/testdata/gpus.json`, captured 2026-10-29, has the
RTX 4090 at `NONE` overall with `US-IL-1` at `LOW`. A live read on 2026-10-30
showed the same card at `HIGH` in `EU-RO-1`. One day apart, a different
answer, and not a small one — `NONE` means you cannot rent it at all.

**A store turns the datacentre into a commitment.** Before stores, the
datacentre was a per-run detail: ember took whatever had capacity and the
question never came up twice. A network volume lives in exactly one site and
pins every node that mounts it there, and `volumeId` is immutable after
create — so choosing a site is choosing where every future run of that store
is placed, for as long as the store exists. Moving later means creating a
second store somewhere else and re-uploading the weights.

That is the change that makes this data worth having. One-off, the question
is "where can I rent a 4090 today"; with a store it is "which site will
reliably have a 4090 for the next six months", and a single sample cannot
answer it. The sample we happened to take could be the good day or the bad
one, and there is no way to tell which from inside it.

The constraint narrows further than it first looks. Of RunPod's 23
volume-capable datacentres, the S3 API serves only 15, and the intersection
is **10**. Pick outside those and weights can only be uploaded by renting
something to download them — so the real choice is across ten sites, and
their capacity is exactly what nobody has a record of.

## What to collect

The catalogue endpoint already returns everything, for free and with no
compute running:

```
GET /v2/catalog/gpus?include=AVAILABILITY&product=POD
```

One row per `(observed_at, gpu_id, datacentre)`:

| field | source | why |
|---|---|---|
| `observed_at` | the collector | the only field RunPod does not provide, and the whole point |
| `gpu_id` | `gpus[].id` | `"NVIDIA GeForce RTX 4090"` |
| `datacentre` | `gpus[].dataCenters[].id` | per-site, never the global summary — see below |
| `availability` | `gpus[].dataCenters[].availability` | the measurement |
| `overall` | `gpus[].availability` | kept to show how often it disagrees with every site |
| `price_secure`, `price_community` | `gpus[].price` | prices move too, and nobody records that either |
| `max_count` | `gpus[].maxCount` | how many of one card a single pod may have |
| `cloud` | derived | `secure` and `community` have separate capacity |

**Per-datacentre, not the summary.** The global `availability` field reads
`NONE` while an individual site still has capacity — that is a real
observation from the live catalogue, and it is why `hasCapacity` exists in
`map.go`. A collector that stored only the summary would record "unavailable"
for cards that were rentable all along, which is worse than no data: it is
data that looks like an answer.

CPU types carry the same field and would be collected the same way. They
matter for the CPU-pod population path, where the question is whether a cheap
CPU instance is reliably available in the store's own datacentre.

## How, if it is built

- **A Go subcommand**, `ember catalogue snapshot`, following the project rule
  that non-trivial logic is Go and never a shell script. `make setup` already
  calls `ember setup` for the same reason.
- **Scheduled by something outside ember** — a systemd timer or cron entry
  invoking the binary. ember does not become a daemon for this; a process
  that must stay running to collect data is a process that will be found
  stopped.
- **Append-only, one file per day**, JSON lines under the state directory.
  Append-only because the value is entirely in the series: a format that can
  rewrite history is one that can lose it to a bug in the writer.
- **Hourly is plenty.** Capacity is reported as four levels, not a number;
  sampling faster than it can visibly change buys nothing and makes a year of
  data large enough to need thinking about.
- **No credentials beyond the existing API key**, no pod, no GPU. The
  collector costs nothing to run, which is the main argument for starting
  early: the data cannot be collected retroactively, and a year of it is
  worth more than the hour it takes to write.

## What it would answer

- Which of the ten store-capable datacentres actually has a 4090 most of the
  time, rather than which had one on the afternoon somebody looked.
- Whether `NONE` on a card means "sold out for an hour" or "sold out for a
  week" — the difference between waiting and moving the store.
- Whether prices drift, and whether the cheap sites are the ones that are
  always empty.
- Whether a card worth buying is one that is persistently unrentable, which
  is the rent-versus-own question this project exists to answer, asked from
  the supply side instead of the cost side.

## Why not now

Nothing in ember needs it to work, and a collector with three days of data
answers nothing a single live query does not. The cost of starting late is
only that the series starts late — and the cost of building it now is a
feature nobody is using while the store path is unfinished.

The argument for starting *soon*, rather than never: this is the only kind of
data that cannot be backfilled. Every day without a collector is a day that
cannot be recovered later, and the decision it informs — which datacentre
holds the weights — gets harder to change the longer the store exists.
