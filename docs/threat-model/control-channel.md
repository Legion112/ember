# The control channel, and the worker that answers it

One channel between ember and a rented node, who opens it, what authenticates
it, and why it is not protobuf over Connect.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

## The posture

- **ember dials and the worker answers, and that is the whole of why this is
  not a posture change.** The listener is the node's own loopback, reached by
  the `direct-tcpip` channel ember already holds over SSH — which is exactly
  how sd-server has always been reached. Nothing new is published,
  `exposure.SSHPort()` is still the only thing in the program that can mint a
  public port, and 22/tcp is still the only port a node publishes. #203 named
  the alternative and its cost: a worker that *dialled* ember inverts the
  posture sentence, because ember would then have to be reachable from a
  rented machine. The distinction that matters is **who can stop the work**,
  not who initiates the bytes.
- **The listener goes through `loopback.Listen` and is therefore
  unrepresentably non-loopback**, rather than being exempt from the rule.
  #192 named this hazard exactly — "a gRPC server on the node would be a
  listener, and `TestNoListenOutsideLoopback` fails the build for `net.Listen`
  outside one package" — and routing through the package that owns the rule is
  the answer to it. `internal/loopback` imports only the standard library, so
  the node binary takes on nothing it did not already have.
- **The port is a constant and not a setting**, for `sdserver.Port`'s reason:
  nothing binds on the operator's machine, so there is no collision to
  configure, and a knob could only point ember at a different port on the same
  node. It is also what makes the channel need no out-of-band announcement — an
  ephemeral port would have to be reported to ember somehow, and the only
  mechanisms available for that are the three this channel exists to collapse.
- **sshd's authentication IS this channel's, and the limit is written down.**
  There is no token. The channel is reachable from the node's own loopback and
  from the far end of a `direct-tcpip` channel, and opening one of those needs
  the key ember minted and never let off the operator's machine. What that
  concedes: any process inside the node's own container can push a job or read
  a preview. There are no other tenants in an ember node and every process in
  it is already root, so a token would have to be injected, would be a second
  secret in the node's environment, and would protect the channel from nothing
  that cannot already read sd-server's unauthenticated socket beside it. **If
  a node ever runs anything it did not start itself, this paragraph stops
  being true.**
- **A cross-origin handshake is refused by default**, which `websocket.Accept`
  does on its own and which costs nothing here: ember's client sends no Origin
  at all, so the default can only ever turn away a browser, and a browser
  cannot reach this interface in the first place.
- **One channel per worker, and the second is refused with a 409.** A node is
  one rental. Two embers pushing into one in-memory queue is two rentals
  believing they own one machine, which is precisely the invariant `max_nodes`
  bounds on the other side of the wire — so it is refused loudly, in the node's
  own log, which is the only place that could ever say it happened.
- **A node is untrusted input, so every index a frame carries is checked
  against the batch ember pushed.** `run.Jobs[f.Index-1]` on a number a rented
  machine chose is an out-of-range panic in the process holding the operator's
  provider credentials. The same rule covers a landed file's name: what ember
  builds out of it is a **remote** path for a tar command, so a name with a
  slash in it is refused here as well as contained by `transfer.Extract`'s
  `os.Root` — a message about a root is a message about the wrong thing.
- **The read limits are asymmetric and that is the seal, not an oversight.**
  `JobLimit` is 16 MiB because a job can carry an init image —
  `blob.MaxInputBytes` is 8 MiB and base64 is four bytes on the wire for three
  of image, and a test holds the two numbers in step. `EventLimit` is 4 MiB,
  because nothing large comes **up**: previews are one small PNG and finished
  images do not travel on the channel at all. That smaller number is what
  decides how much of the operator's memory a rented machine can make ember
  allocate. The library's own default is 32 KiB, which would have cut every
  job carrying an image at the moment the node was already rented.
- **A node image older than the ember driving it is refused by name, before
  any work is pushed.** The image and the binary ship separately: a rented pod
  pulls whichever tag the config names, which may predate the ember driving it
  by weeks, so every field on a frame is one that can arrive missing. The
  worker states its protocol version in the first frame it sends and ember
  refuses one it does not know, with a remediation naming the rebuild. A
  version of **zero** is refused too, which is what an `omitempty` field a
  sender never set decodes to — a check written as "refuse anything greater
  than mine" would wave through exactly the case that happens in practice.

## What crosses it

- **A frame that is not text is refused rather than decoded, and so is one
  with no kind.** This build sends nothing but text, so a binary message is
  either a different protocol or a far end choosing the parse — and `{}`
  decodes into a frame that would fall through every switch at the other end
  and be ignored, which is a frame that did nothing and said nothing.
- **A job's body crosses as bytes nobody parses, and the reason is a number.**
  A body decoded into `map[string]any` comes back with every number as a
  `float64`, so a seed above 2^53 — or anything an operator put in `extra:` —
  would be re-encoded as a **different** number, and the node would generate a
  picture ember did not ask for while recording the one it did.
  `json.RawMessage` makes that unrepresentable, and it leaves `Job.Body` as the
  one place that decides what a job looks like on a wire.
- **What the generation server can do crosses as the decoded value**, and the
  asymmetry with the body above is deliberate. `sdserver.Capabilities` is
  strings, numbers, bools and `*bool` with no `map[string]any` anywhere, so it
  survives the round trip exactly — including the nil-versus-false distinction
  in `Supported`, where nil is "this build published no such flag" and false is
  "the server says no". The server's document is therefore decoded **once**, on
  the node, by the mapper that learned `capabilities.model` is an object and
  not the string api.md's field list implies; a second run of it at ember's end
  would be a second place for that to be wrong.
- **No argv ever crosses it.** ember still starts the generation server itself,
  over `ssh exec`, with argv ember built from the operator's config — which is
  what keeps "nothing an operator writes in a config reaches a node as a
  command" true after the arrival of a daemon on the node. The worker speaks a
  closed set of typed frames and has no argv for anything to reach it through.
- **Money never crosses it.** The node reports what happened — which job
  started, which step a preview is of, what landed, how a job ended — and ember
  stamps the price from its own clock and its own `node.Rate`. A rented machine
  has no business having an opinion about what it cost.
- **An index is one-based, because `omitempty` cannot tell a zero from an
  absent field.** `report.Event` numbers from one for the same reason and the
  two have to agree, since ember stamps one from the other.

## The protocol decision

**It is `github.com/coder/websocket` over the SSH channel, and not protobuf
over Connect as #192 proposes.** Three reasons, in the order they decide it:

1. **Connect's bidirectional streaming requires HTTP/2.** This channel is
   full-duplex by design — jobs going down while events and previews come up —
   and over HTTP/1.1, which is the exact property #192 recommends Connect
   *for*, bidi is not available. The two halves would be two calls, which is
   two channels again, which is the thing being collapsed.
2. **Zero new modules against at least two, in the binary that cannot be
   redeployed.** `coder/websocket` is already a direct dependency with **zero
   dependencies of its own** — most of the argument for it when the browser
   stream adopted it — and the worker is its second consumer rather than
   Connect's third. The measurement this repository already owns is the shape
   of the risk: `otlptracehttp` brought **11 new modules, 1,922 gRPC symbols
   and +10.3 MB / +47.3%** of binary. Connect does not need gRPC's runtime, so
   that figure is an upper bound rather than a prediction — and **nobody here
   has measured Connect + protobuf**, which #192 says itself and which is still
   true: measuring it means adding the dependency, and that is the decision, not
   the experiment.
3. **It is `cmd/embernode` that pays.** `internal/logx` records the cost of
   that binary's first intra-module import, and the thing that makes it
   expensive is that a node image cannot be redeployed to a pod that is already
   running. A protobuf runtime there is a change to the component with the
   longest feedback loop in the program.

**What is given up, said plainly.** A generated contract would make the drift
class impossible, and that argument is real — #192 is right that it is stronger
than #191's on the one thing both are about. What stands in its place here is
weaker and is enough for this channel: both ends are compiled from one tree, so
a renamed field changes both at once, and the version handshake is what refuses
the only version skew that can exist — a node image from an older tree. A
third-party consumer of this channel would change that calculation, and there
is none.

**What was NOT chosen and why it is worth recording**: a hand-rolled
length-prefixed framing over the raw TCP channel. It is about sixty lines and
it has no dependency at all, and it would have been re-implementing a library
already in the tree — which AGENTS.md refuses in the other direction: "adding a
dependency is fine when it saves writing something that already exists". The
websocket also carries ping/pong, which is the heartbeat Phase 5's wedged-worker
state needs, for no further bytes.

## Supervision, and what the deadline stops

- **`cmd/embernode`'s run loop was a two-case select over one child**, and a
  second child cannot be added to a select without a multiplexer. The property
  that had to survive is the meaning of the sshd arm — *"sshd exited on its
  own; nothing can connect to this node"* — because that is the case which
  makes a node observable at all: "no hostkeys available -- exiting" is what a
  private key one byte short produces, it happens in the second before ember's
  first dial, and what ember then reports is a connection refused on a node
  that looks perfectly healthy.
- **The obvious refactor is wrong and is what the guard catches.** A
  multiplexer that returned on whichever child exited first would turn a worker
  that finished its queue into a node that tears down its own sshd — while
  ember is still fetching the results that worker just named, and the node's
  own log, over that connection, and is about to run the destroy. So the
  worker's exit is reported and supervision carries on; sshd's still returns.
- **The worker's exit therefore does not stop the bill**, and the plan this
  implements said it would: "a pod whose container exits stops billing". That
  is true of a container whose only process exits and it does not apply to a
  supervised worker, because `embernode` is PID 1 and stays. The worker's exit
  is the *signal* that the work is done — carried on the channel as well —
  ember's destroy is what ends the bill, and the TTL is what ends it when ember
  is gone.
- **`armDeadline` signals every child and not sshd alone.** It took sshd's
  `*exec.Cmd`, and with a second child beside it a deadline that stopped sshd
  would have stopped the one process ember uses to *reach* the node and left
  the one *spending* the card running: the connection goes, the generation
  carries on, and nothing is watching. A deadline that stops half a node is
  advisory, and the half it leaves running is the expensive half. On a rented
  machine this timer is what ends the bill when the operator's laptop dies
  mid-run.
- **A worker that could not start is not fatal, and the trade is stated.** The
  choice is between a node that cannot work and a node that cannot be asked
  why. Exiting PID 1 would stop the container and ember would report a
  handshake failure with the real reason — a port collision in the workload
  image, a read-only `/tmp` — sitting in a log file on a machine that no longer
  exists and was never reachable, which is the failure `log.go` was written
  against one layer out. Keeping sshd means ember's first push is refused, that
  request fails, the rental ends and the destroy runs: the loss is bounded by
  one request rather than by the TTL, and the node's log comes home with the
  sentence in it.
- **The worker's environment holds no key material.** It is started after the
  `EMBER_HOST_KEY` and `EMBER_AUTHORIZED_KEY` unset, so `/proc/<worker>/environ`
  carries neither — the same property that unset was written for, extended to
  the one process that did not exist when it was.

## Where the results go, which is a correction

**Results are written to the node's own disk and not to the mounted store.**
The plan this implements says the store, and that is a money defect:
`docs/money/stores-and-assets.md` records that a store "bills by the
gigabyte-month whether or not anything is attached", which is why `/workspace`
is on `node.Mount`'s refusal list with the reason spelled out — *"a store there
bills for every render for as long as the store exists, and the operator finds
out on the invoice a month later"*. Nothing in ember deletes a file off a
store, so a worker writing every generated image there would be a leak of
exactly that shape, created by the mechanism meant to make a rental tidier.

Everything the plan wanted from a store write survives the correction, because
none of it was about the store: the write is a plain filesystem write, so the
node needs no S3 key and no API key and stays credential-free; an event names
what landed, so ember fetches exactly those paths rather than globbing; and the
bytes die with the pod, which costs nothing.

**A fresh directory per job**, because a rental serves several requests and two
of them can name the same output. Without it ember's fetch of `boat-00.png`
could bring home the previous request's picture — a result that is plausible,
paid for, and wrong, which is the worst of the three. The directory is the
worker's and travels on the landed frame rather than being derived twice, the
same rule that keeps `safeStem` the only thing deciding a filename.

**What it costs, said plainly**: one `tar` over SSH per file, where the images
used to arrive inside the poll response ember was already reading. For a
512-square image that is a few milliseconds of SSH session against the 80-96 s
the card spent producing it, and it buys a node that writes its results down
before anything fetches them — so a dropped connection loses the fetch rather
than the picture.
