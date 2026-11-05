# Threat model

ember's posture is one sentence: **the only thing on the internet that can
reach an ember node is sshd, authenticated by a key that never leaves this
machine, verified against a host key ember minted itself.**

`AGENTS.md` carries the seals as one-liners. This page is the index to each
one: the defect it was written against, and the measurement that proved it
goes red when reinstated.

## The posture itself

| page | what is in it |
|---|---|
| [exposure.md](threat-model/exposure.md) | every listener is loopback, a create request can only ask for 22/tcp, the label is the source of truth, a node's output is untrusted |
| [host-keys.md](threat-model/host-keys.md) | minted here and injected, never trusted on first use; `hostkey.Pin` is the only `Trust` there is |
| [serve-auth.md](threat-model/serve-auth.md) | a loopback port is reachable by every uid — the mandatory token, the cookie, the unix socket, and their stated limits |
| [request-seal.md](threat-model/request-seal.md) | what a request may name and what it structurally cannot; two rentals at once are unrepresentable |
| [forwards-and-reconnect.md](threat-model/forwards-and-reconnect.md) | the remote end of a forward is built, never parsed from input; the progress socket that never re-opened |

## How the seals are held

| page | what is in it |
|---|---|
| [fakes.md](threat-model/fakes.md) | every behaviour a fake reproduces cost money to find once; a fake kinder than the service tests a service that does not exist |
| [lab-harness.md](threat-model/lab-harness.md) | cleanup ordering, the daemon lock, log paths, and the checks that passed because nothing was left to see |
| [browser-labs.md](threat-model/browser-labs.md) | the class of defect no unit test reaches, and the browser assertions that turned out unable to fail |

## What crosses a boundary

| page | what is in it |
|---|---|
| [wire-types.md](threat-model/wire-types.md) | a nil slice is not an empty list, a Go `int` is not an `i32`, a tag that never fires reads like one that works |
| [the-page.md](threat-model/the-page.md) | routes, keyed lists that froze, and controls built and then thrown away |
| [parameters-and-uploads.md](threat-model/parameters-and-uploads.md) | which parameters are typed and why the rest are not; zero as a value rather than a sentinel; an upload keyed by its content |
| [recording.md](threat-model/recording.md) | the schema and its migrations, the event stream as truth and the columns as a derived index, the node memory probe |
