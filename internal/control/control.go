// Package control is the one channel between ember and the worker on a rented
// node.
//
// WHY A CHANNEL AT ALL, when ember already drives a node perfectly well. It
// drove it through three mechanisms: `ssh exec` for a workload, an SSH
// port-forward for the generation server's HTTP API, and `tar` for results.
// Three channels fail three ways, and a generation that went wrong had to be
// reconstructed by correlating an exec, a forward and a tar -- #203's own
// argument, and the reason this package exists.
//
// WHO OPENS IT, WHICH IS THE WHOLE POSTURE. ember dials; the worker answers.
// The listener is the node's own loopback, reached by the `direct-tcpip`
// channel ember already holds over SSH, which is exactly how sd-server has
// always been reached -- so nothing new is published and `exposure.SSHPort`
// remains the only thing in the program that can mint a public port. A worker
// that *dialled* ember would invert the posture sentence ("the only thing on
// the internet that can reach an ember node is sshd"); a worker reporting over
// a connection ember opened does not. #203 names that distinction: what
// matters is who can stop the work, not who initiates the bytes.
//
// WHAT AUTHENTICATES IT, AND THE LIMIT, SAID PLAINLY. Nothing here. The
// channel is reachable from the node's own loopback and from the far end of a
// `direct-tcpip` channel, and opening one of those requires the key ember
// minted and never let off the operator's machine -- so sshd's authentication
// *is* this channel's, which is the arrangement sd-server already has on the
// same interface. The limit that buys: any process inside the node's own
// container can push a job or read a preview. There are no other tenants in
// an ember node and every process in it is already root, so a token would
// have to be injected, would be a second secret in the node's environment,
// and would protect the channel from nothing that cannot already read
// sd-server's unauthenticated socket beside it. If a node ever runs anything
// it did not start itself, this is the paragraph that stops being true.
//
// THE TRANSPORT IS A WEBSOCKET AND THE PROTOCOL IS NOT PROTOBUF+CONNECT,
// which #192 proposes and this is the written reason against adopting it here
// -- see docs/threat-model/control-channel.md for the whole argument. The
// short form: `github.com/coder/websocket` is already a direct dependency
// with zero dependencies of its own, so this costs no module and no byte of
// anybody's build; Connect's bidirectional streaming requires HTTP/2, and
// over HTTP/1.1 -- the exact property #192 recommends Connect *for* -- jobs
// down and events up would be two calls, which is two channels again.
package control

import "time"

// Port is the node-side loopback port the worker answers on.
//
// A CONSTANT RATHER THAN A SETTING, for sdserver.Port's reason one package
// over: nothing binds on the operator's machine, so there is no collision to
// configure, and a knob here could only ever point ember at a different port
// on the same node -- a way to get it wrong and not a capability. It is also
// what makes the channel need no out-of-band announcement: an ephemeral port
// would have to be reported to ember somehow, and the only mechanisms
// available for that are the three this package exists to collapse.
const Port = 7311

// Path is the endpoint the channel is opened on.
//
// Named rather than served at the root, so the worker's HTTP surface is one
// route and anything else is a 404 an operator can read in the node's log
// rather than a handshake that half-succeeded.
const Path = "/control"

// Version is this build's protocol version, and a mismatch is a refusal.
//
// THE NODE IMAGE AND THE ember BINARY ARE DEPLOYED SEPARATELY. The image is
// built and pushed; the binary is on the operator's machine; a rented pod
// pulls whichever image tag the config names, which may predate the ember
// that is driving it by weeks. Every field below is therefore a field that
// can arrive missing, and the failure mode of a silently-missing field is a
// generation that runs, bills, and reports nothing -- the expensive kind.
//
// So the worker states its version in the first frame it sends and ember
// refuses one it does not know, with a remediation naming the rebuild. A
// handshake is the only point at which this is cheap: afterwards the node is
// up, the card is allocated and the bill is running.
// VERSION 2 IS THE HEARTBEAT, and the bump is what the paragraph above is for.
// KindHeartbeat is a frame ember's own loops refuse as a protocol
// disagreement when they do not know it -- correctly, because a frame nobody
// handles is a job ember would wait for forever -- so a node image carrying
// beats, driven by an ember that predates them, would fail its first batch
// after the card was allocated and the weights were resident. Bumping here
// moves that refusal to the handshake, which is the only moment it is cheap:
// the version is stated before any work is pushed. An older image against this
// ember is refused by the same line and with the same remediation.
const Version = 2

// ResultsDir is where a worker writes what it produced, on the node's own
// disk.
//
// NOT THE STORE, AND THAT IS A CORRECTION TO THE PLAN THIS IMPLEMENTS rather
// than an oversight. docs/money/stores-and-assets.md: a store "bills by the
// gigabyte-month whether or not anything is attached", which is why `/workspace`
// -- "where ember writes outputs" -- is on node.Mount's refusal list, with the
// reason spelled out: "a store there bills for every render for as long as the
// store exists, and the operator finds out on the invoice a month later".
// Nothing in ember deletes a file off a store, so a worker writing every
// generated image there would be a leak of exactly that shape, created by the
// mechanism meant to make the rental tidier.
//
// Everything the plan wanted from a store write survives the correction,
// because none of it was about the store: the write is a plain filesystem
// write, so the node needs no S3 key and no API key and stays
// credential-free; an event names what landed, so ember knows what to fetch
// rather than globbing; and the bytes die with the pod, which costs nothing.
//
// UNDER /tmp FOR logx.NodeLogPath's REASON, which is the precedent this
// follows in full: it is a path both ends know as a constant, so nothing
// crosses the channel that could be wrong, and it is deliberately not under
// the outputs directory, because a file there "would be swept into the run's
// own archive, arrive half-written in the middle of a batch and be
// indistinguishable from a result".
const ResultsDir = "/tmp/ember-results"

// JobLimit bounds one frame ember may send the worker.
//
// Sized by the one field that can be large: a job's init image, which
// blob.MaxInputBytes caps at 8 MiB and which travels base64 in the generation
// server's own request body -- four bytes on the wire for every three of
// image. 16 MiB is that figure with room for the rest of the body, and
// TestJobLimit_HoldsTheLargestInitImageTheBlobStoreAccepts is what keeps the
// two from drifting apart silently.
//
// A LIMIT AT ALL BECAUSE THE LIBRARY'S DEFAULT IS 32 KiB and would have cut
// every job carrying an image, at the moment the node is already rented.
const JobLimit int64 = 16 << 20

// EventLimit bounds one frame the worker may send ember, and it is the
// asymmetric half on purpose.
//
// A node is untrusted input -- the rule internal/transfer enforces twice over
// a tar stream -- so this is the number that decides how much a rented
// machine can make the process holding the operator's credentials allocate.
// It is much smaller than JobLimit because nothing large comes up this
// direction: previews are a single small PNG, and finished images do not
// travel on the channel at all. 4 MiB is a 2048-square preview with room to
// spare, and a worker that tried to send more is a worker ember stops reading
// rather than a worker ember grows to accommodate.
const EventLimit int64 = 4 << 20

// HandshakeTimeout bounds the opening of the channel.
//
// The worker is started by embernode at boot and is listening before sshd has
// accepted anything, so a dial that does not complete promptly means the
// worker is not there -- a port collision in the workload image, or a worker
// that failed to start, both of which the node's own log explains. Bounded
// because the alternative is a rental that hangs at the one step with no
// output: ember has a card allocated and is waiting on a connection that is
// never going to be made.
const HandshakeTimeout = 30 * time.Second

// HeartbeatEvery is how often the worker says it is still there.
//
// WHY A HEARTBEAT AND NOT A HEALTH CHECK ON THE WORKER. A /healthz on the node
// means the worker listens somewhere a prober can reach, which means a public
// port, which means a second minter beside exposure.SSHPort -- the one seal
// the whole exposure posture rests on ("a create request can only ask for
// 22/tcp"). It would also double a node's reachable surface to tell ember
// something it is in a position to know already: ember holds this channel for
// the whole rental.
//
// AND IT IS STRICTLY MORE INFORMATIVE THAN A REACHABILITY PROBE, which is the
// reason it is worth the frames rather than merely cheaper. A port check has
// two answers; this has three, and the middle one is the expensive one:
//
//	channel open, beat current  -- the worker is there
//	channel open, beat STALE    -- the worker is not answering, and ember is
//	                               paying for the card it is holding
//	channel closed              -- gone; reconcile and the deadline take over
//
// WHAT IT CANNOT SEE, said plainly rather than left to be assumed. This is the
// CHANNEL's liveness and not the generation server's. The beat is sent by a
// goroutine of its own, so a worker whose sd-server has stopped answering polls
// keeps beating -- what that looks like is a rental whose job never ends, which
// is the request's own row rather than this. What it does see is a node that
// froze or was partitioned while the TCP connection stayed open, a worker
// process that died without the SSH channel closing, and a worker whose sends
// are blocked -- that last one because the beat takes Conn's send mutex, which
// is the same lock the runner needs, so a worker that cannot write cannot beat
// either.
//
// FIVE SECONDS, against the two numbers on either side of it. The worker's own
// poll of the generation server settles at jobPollMax (2s), so a beat is never
// the busiest thing on this channel; and `ember serve`'s fleet view is a button
// press rather than a poll (see docs/threat-model/the-page.md), so an operator
// who presses it wants an answer about the last few seconds and not about the
// last minute. The traffic is one ~20-byte text frame per interval per rental:
// 720 of them in an hour, against a single preview PNG that is up to 4 MiB.
const HeartbeatEvery = 5 * time.Second

// HeartbeatStale is how old the last beat may get before a worker is reported
// as not answering.
//
// FOUR INTERVALS, AND THE FLOOR IS Conn.Send's OWN BOUND rather than taste.
// Sends on this channel are serialised and each one is bounded at ten seconds,
// so a beat queued behind a stalled write -- a 4 MiB preview frame up a slow
// uplink, which is the largest thing a worker sends -- can legitimately be ten
// seconds late through no fault of the node. A threshold at two intervals would
// sit inside that window and report a working rental as wedged, which is the
// direction this must not be wrong in: an operator who learns that "not
// answering" sometimes means "busy" stops acting on it, exactly as `ember ls`
// would stop being acted on if every store exited 2.
//
// So: ten seconds of worst-case lateness, plus one interval for the beat that
// was due in it, rounded up to a whole number of intervals. Twenty seconds.
const HeartbeatStale = 4 * HeartbeatEvery
