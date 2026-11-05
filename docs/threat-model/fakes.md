# Fakes that are not tidier than the thing they stand in for

Every behaviour a fake reproduces here cost money to find once. A fake kinder
than the service tests a service that does not exist.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **A fake must reproduce the behaviours that cost money to find, including
  the unhelpful ones.** `runpodfake` reports no host key, leaves `ssh.direct`
  null for a while after the status reads RUNNING, nulls `ssh` and `runtime`
  on every list entry, and publishes a web terminal port nobody asked for --
  because each of those is a real RunPod behaviour that broke ember once. A
  fake that is tidier than the service it stands in for tests a service that
  does not exist. The previous RunPod tests used one-off handlers returning a
  canned blob, which cannot express the thing that actually breaks: time --
  and the conformance suite now points the real provider at this fake instead,
  which is what that comment was asking for.

  **The fake one layer down was tidier, and it said in words that it was
  not.** `providertest.RecordingProvider` echoes the injected host key back,
  which is what a provider supporting injection does and is the right default
  -- and its comment read "leave it zero to exercise the trust-on-first-use
  fallback", while zero *is* the echo. So the one RunPod behaviour ember's
  quietest bug was about, reporting nothing, was unreachable through it, and
  no assertion the `provision` unit tests carried could have caught the
  reinstated defect: not a dead assertion but a case the fake could not
  produce. `ReportsNoHostKey` is that behaviour, a named flag rather than a
  sentinel, because the zero value already means something else. Measured: the
  defect is green against every other case and red against this one.

  It was tidier in one place, and the conformance suite is what found it: a
  `DELETE` of a pod already deleted answered **200**, because the handler
  checked whether the pod was in its map and not whether it was gone. The
  live API 404s, `httpx.Delete` turns that into success, and
  destroy-is-idempotent therefore passed against this fake *without the
  not-found path ever being taken* -- the exact shape of
  `AssertNoStray`-could-not-fail, in a fake rather than in a cleanup order.

  **A route it does not serve is a path no lab can reach.** There was no
  `/v2/catalog/cpus` handler, so a request for it 404'd and `cpuOffers` --
  what `ember store sync` rents its cheap downloaders through -- had never run
  against the fake at all. Two money invariants live behind that endpoint:
  `Filter.CPUOnly`'s exclusivity, and the container-disk clamp, which was
  exercised only against canned bodies. The clamp is the sharper case, because
  a clamp means nothing unless the service would have said no: with the fake
  accepting any disk size, **deleting the clamp passed everything**. It now
  answers the live `400 "Container Disk must be less than or equal to 20"` for
  a CPU pod and nothing for a GPU one, refuses `include=AVAILABILITY` without
  `product`, and reports no availability levels when `include` does not ask
  for them -- a catalogue read that forgot the parameter is a priced catalogue
  with no capacity in it, which presents as "that card is sold out
  everywhere". It also stopped echoing a `gpu` object and a 4090's hourly rate
  onto every pod, because an assertion about whether a CPU-only run got a card
  passed against either answer while it did.

  The catalogue bodies are the provider's own committed fixtures, served
  verbatim rather than copied here, and that is the opposite of the choice the
  rest of this fake makes. What a fixture cannot express is time, and a
  catalogue read has none; what is left is a field shape, and two copies of a
  field shape is how `dataCenter` against `dataCenterId` stayed invisible. The
  query refusals above are behaviour a file cannot hold, so they are code over
  that one body rather than a second body. `internal/provider/runpod/testdata/README.md`
  says which of them were observed and which rest on the endpoint's own
  description.

  **The same finding one API over, and that one was airtight rather than
  merely untested: `ember store`'s seven subcommands and the store pre-flight
  were paths no lab could reach.** There was no `/v2/network-volumes` route
  either, so `store create`, `rm`, `push`, `sync`, `ls`, `verify` and `abort`
  were exercised against RunPod only by the one-off canned handlers this
  package exists to replace. The pre-flight is the sharper half, because
  nothing about it was a coincidence: every store lab in the tree runs on the
  **local** provider, `local.go` reports `Region: ""` for a Docker volume --
  correctly, it is not in a datacentre -- and `CheckStoreAssets` skips on an
  empty region by design. So it returned `Asked: false` on every lab there is,
  and the suite could not tell the check from its own absence.
  **Measured rather than inferred, and the first wording of this entry was
  wrong about half of it**: with the pre-flight patched never to run, three
  unit tests go red -- `TestUp_RefusesAStoreMissingTheWeightsBeforeChoosingAnOffer`,
  `TestUp_AnEmptyObjectIsNotTheWeights` and
  `TestAppPlanner_FillsAnEmptyStoreOnAMachineWithNoCardRatherThanRefusing` --
  and **106 of the 107 labs pass, the one failure being the new one**. So the
  unit suite did report it and the lab suite reported nothing, which is the
  claim worth making; "`go test ./...` was green too" was an inference from
  one targeted run and is not true.

  **A volume is one host DIRECTORY, and that is the whole point rather than an
  implementation detail.** The thing the pre-flight turns on is a join: the
  sync fills the store by running `embernode fetch` on a rented machine, over
  SSH, into a mounted directory, and the next plan asks the store's own
  endpoint whether that worked. Two in-memory maps -- which is what
  `internal/queue`'s and `internal/cli`'s httptest stand-ins are -- agree with
  each other whatever either half does, the closed loop `capabilities.model`
  sat in. So `Create` honours `mounts.network` by bind-mounting that
  directory, the S3 endpoint serves object calls by reading it, and the asset
  the node downloads is the asset the endpoint lists. Widening the `Nodes`
  seam to carry the mount is the same rule as the fake not being tidier than
  the service: accepting `mounts.network` and starting a container with
  nothing at that path would make a full store and an empty one the same lab.
  **SigV4 is verified by re-signing** -- `awssig.Sign` over a reconstruction
  of the request, the whole `Authorization` header compared -- because a fake
  that accepted an unsigned request cannot catch a signing regression, and a
  403 with no detail is the only other feedback that algorithm has. The
  reconstruction is filtered by `SignedHeaders`, which is what a real server
  does and is not optional: Go's transport adds `Accept-Encoding: gzip`
  **after** the client has signed, so a verifier that hashed every header it
  received rejects every correct request.
  `internal/provider/runpod/testdata/README.md` has the per-behaviour table
  and says which of them were observed, which are inferred, and the two that
  are deliberately not reproduced.

  **Two gaps the lab measured on its first run, both asserted rather than left
  to be noticed.** `fillStore` calls `SyncAssets` with no sink and writes no
  `genstore` row, so `ember history` and `ember spend` are blind to a rental
  the queue started on its own initiative -- the ledger shows one rental where
  two were paid for, against this file's own "a total that is missing part of
  its history says so, every time". And `Queue.once` calls `fail` only when
  `Planner.Plan` fails: the status is written inside `WithServed`'s closure by
  `drain`, so **every** failure after the plan -- the create, the wait for
  sshd, the gpu probe, the asset verify, the model load -- leaves the row with
  the `running` `ClaimNext` wrote and no rental behind it. The queue table
  shows it running for the life of the process and its cancel button reaches
  the provisioning-window 409, whose message says the rental is coming up when
  nothing is; `Run`'s `Interrupt` sweep clears it only at the next start. Both
  are asserted the way `TestConformance_CannotCatchARederivedRunID` is -- a
  limitation nobody can read out of the code is a limitation that will be
  forgotten -- so a change that records either turns the lab red, which is the
  moment to read the comment beside the assertion.

  **BOTH GAPS ARE CLOSED NOW AND THIS PARAGRAPH IS THE RECORD OF WHAT THEY
  WERE, not a description of the code.** The arrangement worked exactly as it
  was written to: #149 fixed the second and #148 the first, each turned this
  lab red, and each inverted the assertion rather than deleting it -- so the
  lab asserts the terminal status and the reason on a rental that failed after
  the node existed, and two ledger rows with the cheap machine's own cost in
  `ember spend`'s period total. The entry at the end of **Money** has #148's
  design call and what the reinstatement measured. Left in place rather than
  rewritten because a prediction this file made and the code then met is worth
  more than a tidy present tense; the tense is corrected here so nobody reads
  the sentences above as a gap that is still open.

  **It drives the queue in process, and that is forced rather than chosen.**
  `app.Deps.CloudBaseURL` and `S3BaseURL` are Go fields, so the compiled
  `ember` cannot be pointed at a fake provider -- which this file already
  records as the reason the offers panel's error path has no lab. What the lab
  builds is `cmd_serve.go`'s own wiring field for field: the same
  `queue.Queue`, the same `queue.AppPlanner`, the same `genstore`. The one
  thing it cannot reach is the `internal/cli` verbs themselves, because
  `runStore` is unexported, so `store verify`'s and `store ls`'s own calls --
  a HEAD per asset, `ListObjectsV2`, `ListMultipartUploads` -- are driven
  through `s3.Client` instead.

  **The asset server binds the Docker bridge gateway, which is the one
  listener in these labs that cannot be loopback.** `embernode fetch` runs
  INSIDE a container, where 127.0.0.1 is the container, and `httptest` binds
  loopback -- so the address is the bridge's own gateway, read from the daemon
  rather than assumed to be 172.17.0.1 (a daemon with another `bip` answers
  something else, and a guessed address fails as a timeout naming nothing).
  Measured before it was built: a container on the default bridge reaches a
  host listener on that address, and the listener is reachable from nothing on
  the LAN. `nolisten_test.go` exempts test files for exactly this.
