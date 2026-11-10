# Listeners, ports and what a node publishes

Every listener is loopback and a create request can only ask for 22/tcp.
Also: the label as source of truth, and treating a node's output as untrusted.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **Every listener is loopback, and `loopback.Bind` is the only way to name
  one.** `Bind`'s field is unexported with no literal path, so the only Binds
  in the program came from `Port`, `Port6` or `ParseBind`. The bug: a `bind:`
  string in a config reaching `net.Listen` as `":8188"` publishes a
  generation server, with file paths in its request body, to the whole
  coffee-shop LAN.
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
- **There is no outside-in probe: ember reads the mappings and never dials
  from outside, and the posture is therefore one mechanism rather than two.**
  This bullet used to describe `exposure.Probe` -- a DialFunc seam, a per-port
  timeout, `Open`, `AssertClosed`, 144 lines of tests, this package gated at
  100% -- and nothing outside that test file ever called either method, while
  the bullet sat in a list whose other entries are live seals. Read here it
  claimed a capability, which is why it is a deletion and not a shrug: the
  file's own doc comment named the case it was for, "the provider's API says
  one thing and its network does another", and that case was caught by
  nothing. The live control, `AssertSSHOnly` on what the provider reports back
  plus the destroy, does run and does go red when reinstated.
  **Deleting it was NOT the `sdserver.Client.Cancel` call**, which was written,
  covered, uncalled and wired up because deleting it would have deleted the
  only cancel mechanism. The difference is that the join here is not available:
  **a probe needs an address, and ember has one only for the ports it is
  already deciding about.** An unreported public port has no address by
  construction -- nothing names the host, nothing names the public port -- so a
  probe aimed at the SSH endpoint's host and the node-side port number is
  aimed at an address no provider here serves and answers "closed" always,
  which is `AssertNoStray`-could-not-fail with a dialler in it. A reported one
  is already decided and the node is already destroyed. The one real gap is a
  tolerated `Caps.PlatformPorts` entry, where the concession rests on a
  property of ember's node image rather than a promise from the provider --
  and `node.Exposed` carries the node-side port plus a URL "kept for the error
  message", which on RunPod is `{id}-{port}.proxy.runpod.net` for an http port
  while `mapExposure` writes that field as `ip:publicPort`, and
  `Probe.Open(ctx, host, []uint16)` cannot express either. So the address
  would have to be built from field combinations nobody has captured, which is
  how `allowedCudaVersions` and `capabilities.model` both went wrong.
  **The two judgements are kept as a note where the code was**, the
  `node.RateFromFloat` shape, because whoever writes the next one makes them
  again: a port that answers is conclusive and a port that does not is only
  evidence, since a firewall that DROPs looks exactly like a closed port from
  here; and each port gets its own timeout rather than a share of one budget,
  because a run of dropped ports would otherwise eat the budget between them
  and every later port would report "closed" because the clock ran out --
  a false pass on the only answer that matters. The one dial left in the tree
  is `TestServe_IsReachableFromNowhereElse`, which probes this machine's own
  LAN addresses for the `ember serve` listener; it has an address, and that is
  the whole difference.
- **"Published nothing" and "never reported" are different answers.**
  `node.Exposure()` returns `([]Exposed, bool)` and an unreported node fails
  every assertion. They are the same empty slice, and treating the second as
  the first means a provider whose mapping code forgets to populate exposure
  gets waved through — the check still runs, still passes, and verifies
  nothing. `WithExposed` stores an empty report as an empty slice and never as
  nil for the same reason: `append([]Exposed(nil), es...)` returns nil for
  empty input, which turned the safest possible answer into a failed assertion.
- **`Caps.PublicSSH` defaults to the stricter check.** False — the zero value —
  means the provider publishes nothing at all. A provider author who forgets
  the field gets `AssertNoPublishedPorts`, and the failure is a destroyed node
  with a loud message rather than a public port nobody looks at.
- **Exposure is derived from what the daemon or API reports back, never from
  what the create call asked for.** `dockerengine.Binding` carries the host IP
  and not just the port, because Docker's own default is to publish on
  `0.0.0.0` and the difference between that and `127.0.0.1` is the difference
  between a port only this machine can reach and one the whole LAN can. A
  check that reads ember's own request cannot catch a provider doing something
  else, which is the entire thing it is for.
- **The exposure assertion lives above the providers, not inside them.** A
  provider checking its own exposure is a provider grading its own homework.
- **The label is the source of truth; the journal is a cache.** A record is
  joined to a live node by provider node id *or* by the run id in the node's
  label. Matching on node id alone meant a run killed between `Create`
  returning and the id being written produced two findings -- an orphaned
  record that could not name its node, and an untracked node no record
  mentioned -- and `reap` could act on neither, although both carried the same
  run id the whole time. That defeats the entire reason the record is written
  before the create call.
- **Each record names the process holding its node.** Without it, a run killed
  with SIGKILL leaves a node that is running, recorded and inside its deadline
  -- indistinguishable from a healthy long-running session -- and `ls` prints
  `ok` over a GPU billing with nobody watching. The TTL still bounds the loss;
  the holder makes it visible while it is happening. It answers "not
  abandoned" for everything it cannot be sure of: no holder recorded, a record
  from another machine, a platform where liveness cannot be checked. A false
  "abandoned" would tell an operator their running session is dead, which is
  the one mistake that would make the check not worth having.
- **One name for a path, not one per call site.** The journal's "nodes"
  subdirectory was a literal in the CLI and a second literal in the lab
  harness. They drifted, the harness read the wrong directory and found
  nothing, and a lab asserting "a clean run leaves no records" passed without
  checking anything. The harness now reads through `statestore` itself.
- **A provider API's int port is range-checked, never narrowed.**
  `uint16(70000)` is 4464. On the endpoint path ember would dial a port the
  node is not listening on and blame the node for the timeout; on the exposure
  path a public port would be reported as a *different* port, making an
  assertion about what is reachable an assertion about the wrong number.
- **A filter matches a model name it is a prefix of, at the capacity boundary
  only.** `NormalizeGPU` deliberately keeps the capacity suffix, because an
  A100 40G and an A100 80G are not substitutes -- so `--gpu h100` has to
  accept `h100-80g`, and string equality meant it accepted neither. `a100`
  accepts `a100-40g` and `a100-80g` but not `a1000`, because the next
  character must be the separator. Same failure as the substring bug
  `noiseWords` exists to prevent, reached from the other direction: the card
  was rentable, the filter matched nothing, and the output was identical to a
  provider with no capacity.
- **Memory type is not memory capacity.** `HBM3` and `GDDR6X` are noise words;
  `80GB` is not. RunPod spells a card "NVIDIA H100 80GB HBM3", and leaving the
  memory type in welded it onto the capacity suffix as `h100-80ghbm3`.
- **No escape hatch for a seal, not even for a test.** The RunPod mapper's
  refusal of a public non-SSH port is unreachable, because only
  `exposure.SSHPort` can mint a public request. The first attempt to cover it
  exported a helper from `exposure` that built one -- a hole in the wall the
  seal exists to be. The branch stays uncovered; an escape hatch for tests is
  an escape hatch.
- **A tar stream from a rented machine is untrusted input.** Containment is
  enforced twice, by two different mechanisms: the member name is checked
  component by component, and every write goes through an `os.Root`. A string
  check misses a spelling; a Root cannot tell ember the archive was trying
  something.
- **A hostile member name is refused, not cleaned.** `path.Clean("/../a.png")`
  returns `/a.png`: it *absorbs* the `..` because nothing is above root. An
  earlier version cleaned first and then looked for a leading `..`, so the
  check could never fire -- every hostile name was silently rewritten into a
  valid one, nothing escaped because the Root held, and ember reported success
  for a file called something else. An absolute member name is refused for the
  same reason: ember's own tar command never produces one.
- **Only regular files and directories are extracted.** A symlink, hard link,
  device node or fifo is skipped and *named* in an error. ember is retrieving
  generated media from a machine it rented, and refusing the kinds outright is
  shorter than reasoning about where each one could point.
- **A remote command is argv, and every word is quoted.** An SSH exec request
  is handed to the remote shell, so a command assembled by concatenation is a
  shell injection with the operator's own config as the input. ember used to
  refuse anything unquotable, which was right while every remote command was
  tar over config-file paths; a workload cannot work that way, because a
  prompt is arbitrary text. So `sshx.Quote` wraps in single quotes, where
  every character except the single quote itself is literal, and escapes that
  one as `'\''`. A unit test checks the string ember builds; a lab
  round-trips the same hostile inputs through a real sshd and a real `/bin/sh`
  and compares what arrived, because whether the shell agrees is a question
  for a shell. Both were checked by breaking the quoting and watching them
  fail.
- **A workload runs over the SSH connection, not as the entrypoint.** A node
  ember cannot reach is a node ember cannot destroy, so there is no value in
  a workload outliving the connection — and running it here means the output
  streams live and the exit status is known when it happens rather than
  polled for through a file.
  **The worker is the one thing that is part of the entrypoint, and it obeys
  the same rule rather than being an exception to it**: it stops when the
  channel ember opened goes away, so a job finished after the laptop died is
  not a card paid for a picture nobody will fetch. See
  [control-channel.md](control-channel.md).
- **A node rented to run something stops when it has run it.** Holding
  afterwards bills for a machine with nothing left to do.
- **The exit status is read after the stream, and always.** A `tar` that failed
  halfway produces a valid prefix of an archive: the extraction is happy, the
  data is incomplete, and that is exactly how an output fetch loses files
  quietly. GNU tar over a missing path exits non-zero and writes a valid
  *empty* archive, which without this would read as a successful fetch of
  nothing.
- **Outputs are fetched on a context that survives Ctrl-C.** The node is about
  to be destroyed either way, whatever it produced is about to stop existing,
  and a run that failed halfway is often the one whose partial output and logs
  are worth having.
- **"Already there" has two meanings.** `mkdir` returning EEXIST over a
  *directory* is fine -- two outputs can land in overlapping trees. Over a
  *file* it is not: nothing under it can be written, and treating them alike
  reported success for a directory that was never created.
