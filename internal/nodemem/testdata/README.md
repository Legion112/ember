# Captured probe output

Real output from `nodemem.Script`, not written by hand. The wire guesses in
this repository have been wrong twice -- `capabilities.model` is an object and
not a string, and RunPod's `allowedCudaVersions` is in a schema and not in a
response -- both times because a **schema** was read in place of a
**response**. These are responses.

Captured on 2026-11-02, on a Linux 6.17 host with cgroup v2 and 125.7 GiB of
RAM, by running the script under `docker run`:

| file | how |
|---|---|
| `container_limited.txt` | `docker run --rm --memory 512m debian:stable-slim` |
| `container_unlimited.txt` | `docker run --rm debian:stable-slim` |
| `host_root_cgroup.txt` | `sh` on the host itself |

## What they settle, and what they do not

**A container's `/proc/meminfo` reports the HOST's memory.** `container_limited`
is a container bounded at **512 MiB** reporting **125.7 GiB** of `MemTotal` --
a factor of 252. That is the mechanism behind the line in `AGENTS.md` that all
eight nodes of the FLUX.2 measurement "reported 620-708 GiB free RAM": that
figure was never about the limit the page cache lived under, so it ruled
nothing out.

**"No limit" is the literal string `max`.** `container_unlimited` shows it.
Nothing in the kernel documentation ember was working from says which spelling
a container gets, and a parser that read it as a number would have reported
zero -- which `Limit` would then have had to read as "a container that cannot
allocate anything".

**The root cgroup has no `memory.max` at all.** `host_root_cgroup` has no
cgroup line: the file does not exist outside a cgroup with a limit set, which
is why an unreadable file and a readable-but-unlimited one are both zero and
why `CgroupVersion` is what says whether anything was read.

**What they do not settle: what RunPod sets.** These are Docker's defaults on
one host. Whether a RunPod pod's `memory.max` is below its asset set -- the
actual question #30 asks -- needs a rental, and the honest statement is that
nothing here answers it yet. `ember history --memory` is what will, on the
next one.

**Not captured: cgroup v1.** This host runs the unified hierarchy only, so
`cgroup1_max` and `cgroup1_current` have never been observed in the wild by
this repository. The v1 paths are named rather than derived for that reason,
and the sentinel value is covered by a unit test rather than by a capture.
