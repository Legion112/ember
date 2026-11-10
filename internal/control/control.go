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
const Version = 1

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
