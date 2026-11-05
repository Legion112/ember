# The lab harness

Cleanup ordering, the advisory lock on the Docker daemon, log paths, and the
checks that passed because there was nothing left to see.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **Clean up what you did not think to look for.** The lab harness removed
  containers and not the networks beside them. Nothing bills for a bridge
  network and no assertion mentioned one, so a leak stayed invisible until
  Docker had subnetted its whole address pool and every run failed with "all
  predefined address pools have been fully subnetted". `AssertNoStray` now
  covers networks, and a lab asserts `ember up` leaves none. The check still
  matches on the *presence* of ember's label and never on its value, because a
  sweep that removes only what the harness remembered creating is this leak
  again -- but the set it is taken over is **this suite run's**, not the
  daemon's. That distinction is the entry two below, and it cost two suite
  runs to learn: the breadth is the property and the daemon was never the right
  scope for it.
- **`AssertNoStray` could not fail, and nobody noticed for the life of the
  suite.** The lab's cleanup ran `Reap()` and then `AssertNoStray()` in that
  order, so the reap removed every container and network and the assertion
  then looked at an empty daemon. Its own comment claimed it ran "before
  AssertNoStray records the failure, never instead of it"; the code did the
  opposite. Every lab's final assertion was passing because there was nothing
  left to see — the exact failure mode the `--- SKIP` gate exists to prevent,
  reached without skipping anything. The cleanup now snapshots the daemon,
  then reaps, then reports the snapshot, so both properties hold: one failing
  lab still cannot cascade, and a leak is still named. Found by reinstating a
  deliberate leak and watching the suite stay green. What the snapshot is taken
  *over* was wrong for longer and in the other direction -- it was the whole
  daemon, which is the entry below -- so the two fixes are one rule: the
  cleanup sees everything this suite run produced, at a moment when reaping
  cannot have emptied it, and nothing else. The same shape was then found one
  layer *out* of the suite, in the Makefile: every lab target piped `go test`
  into `tee`, so `make` read tee's status and a failing lab exited 0 -- the
  gate that decides whether a workload image is published among them.
  Measured rather than argued: with the old recipe a deliberately failing
  `TestWorkloadImage_ListensOnNothingButSSH` left `make publish-sd-cuda`
  running `docker tag` and `docker push`, and the only thing that stopped the
  image shipping was that the registry in that experiment was a closed port.
  Both halves of this entry are the one rule -- a gate nobody has watched fail
  is a gate nobody should trust -- and both were found by reinstating the bug.

  **`AssertNoStray` is gone, and the name in this document is now the name of
  the lesson rather than of a function** -- the `queue.Cost` arrangement.
  Repairing the ordering above meant spelling snapshot-reap-report in the
  cleanup, because a zero-argument method cannot carry a snapshot taken before
  the reap, and the method was left behind: for the life of the suite it had
  **no caller**, while fourteen comments -- six labs, eight in `internal/` --
  and this entry named it as the thing guarding them, and its own doc comment
  said "it is also run from the lab's own cleanup" (#113). The property was
  checked the whole time, by `reportStray`; what was wrong was that a reader
  following the name arrived at a function that never ran. Nothing automated
  reported it, and that is structural: `TestNoProductionDeadCode` does not
  load `test/`, because a lab caller and no caller at all are the same verdict
  to it -- so the harness is the one tree where this shape has to be found by
  reading, and that refusal is one defect older than it was.
  **Wiring it up was tried first and a measurement refused it.** #113's own
  suggestion was the mid-test home: the three labs that leak on purpose call
  `Reap()` themselves to declare the orphan, and it looks as though nothing
  can then say whether that reap was complete, since the cleanup's snapshot is
  taken afterwards. It can, and the "afterwards" is why -- the snapshot lands
  after the body's reap and *before* the cleanup's own, so whatever a declared
  reap left is exactly what it sees. Reinstated, a `Reap` that keeps its
  containers: both orphan labs go red at `reportStray` with no mid-test
  assertion anywhere, so an `AssertNoStray()` beside their `Reap()` reported
  the same objects one line earlier and nothing else. That is `queue.Cost`
  and not `sdserver.Client.Cancel`: a second path for one check, where the
  other path is the live one. The two judgements worth keeping -- snapshot
  before the reap, and over this suite run's objects rather than the daemon's
  -- are a note where the function was, the `node.RateFromFloat` shape,
  because whoever adds the next stray check makes them again.
- **A cleanup that deleted another run's live nodes.** `Lab.Nodes` filtered
  containers on the mere presence of `ember.run-id`, with no value, and `Reap`
  force-removed everything it returned plus every labelled network and volume
  -- so a lab's cleanup was a whole-daemon delete. That was safe while this
  repository was one checkout. Agent worktrees put several on one machine, each
  able to run the labs against the one Docker daemon, and the two runs then
  destroyed each other's nodes mid-test: `No such container` and `connection
  refused` for a container that existed a moment earlier, 170s of wall clock
  against the usual 85, and **every failing test passing when run alone**. A
  lab that fails for reasons unrelated to the code is the other half of the
  `--- SKIP` problem -- it teaches people to re-run rather than to read, and a
  re-run that passes is indistinguishable from a flake that was hiding
  something. The fix is an advisory lock on the daemon, taken once per test
  binary (`labdaemon.Acquire`), plus a snapshot of what was already labelled
  when the lock was taken, which every listing subtracts. **A per-run label
  value was the obvious answer and is not sufficient**: it can scope what the
  *harness* looks at, and ember's own leftover sweep is daemon-wide by design
  -- `TestLeftover_ReapRemovesANetworkNothingElseWould` asserts `removed 1 of 1
  leftover`, and a second suite's deliberately-expired network is swept by this
  one's `ember reap` whatever the harness filters on. Only exclusivity makes
  those assertions true. It is an flock and not a pid file because the kernel
  releases it when the holder dies, and three labs here SIGKILL ember on purpose;
  a lock that could go stale would have to be stealable, which is the
  daemon-wide delete again with extra steps.
- **"Another run is in progress" is a wait. "Something ember will report is on
  the daemon" is a refusal.** Failing a suite because the daemon is busy would
  be the `--- SKIP` mistake in red, so a queued suite waits, says who it is
  waiting for, and fails with one sentence only if the holder never finishes
  (`LockWait`, ten minutes, under the binary's own 20). A stale object is the
  other question, and the split between tolerating and refusing one is
  **measured** rather than chosen: with one `ember.kind=node` container left
  over on the daemon, `ember ls` reports it `untracked` -- correctly, that is
  what the command is for -- and
  `TestLeftover_ReapRemovesANetworkNothingElseWould` fails ninety seconds in
  with "ls still reports findings after reap", which names nothing an operator
  can act on. ember's own listings are daemon-wide by design, so the harness
  cannot filter that away; deleting it was the defect; so `Objects.Reported` is
  refused up front, naming each object and `make reap-labs`. Everything else --
  a container wearing only `ember.run-id`, which `local.List` never looks at --
  is left strictly alone: not counted, not reported, not removed, named once in
  the log. Both halves are live code, which is the test of the split: refuse
  everything and the subtraction would be dead, subtract everything and the
  refusal would be.
- **`make reap-labs` is the escape hatch, and it is the one place the refusal
  points at a tool rather than a rule.** It is honest about being a developer
  tool: nothing in CI runs it, and it will remove objects no test created.
  Refusing while a suite holds the lock is right there and waiting would be
  wrong -- a tool whose whole job is a daemon-wide delete must not queue behind
  a suite and then remove what it has just finished producing.
- **A lab log is read back, so two checkouts cannot share its path.** The four
  lab targets `tee`d into fixed `/tmp` names and then grepped them for `---
  SKIP`. A second worktree's run truncates that file as it starts -- before
  `labdaemon.Acquire` has taken the daemon lock, so the lock cannot serialise
  it -- and the first run's gate then greps a log belonging to somebody else,
  over a hole where its own output was: an assertion reading the wrong file,
  which is how the journal's "nodes" subdirectory went unchecked, applied to
  the gate that exists because a skipped lab reports success for something that
  never ran.

  **The first fix for it keyed `LAB_LOG_DIR` to `$(notdir $(CURDIR))`, which is
  `ember` for every clone there is.** Measured: `~/github/ember` and two copies
  under different parents all printed `/tmp/ember-labs-ember`, so the collision
  survived in full for the one naming `git clone` gives by default -- while the
  comment above the line said "keyed to this checkout". Nothing had bitten only
  because the agent worktrees that motivated the fix are named `wt-…` and
  `agent-…` and happen to differ. It is a sha256 of the whole path now, and
  **not** `$(subst /,-,$(CURDIR))`: that substitution is not injective over a
  `-`, and `/w/ember-wt/ember` and `/w/ember/wt/ember` both become
  `-w-ember-wt-ember`, which is this defect again one character further down. A
  digest also cannot contain a space, which matters because `LAB_CHECK`'s
  `grep -q -- '--- SKIP' $(1)` is unquoted and would be handed two filenames.
  `TestLabLogDirIsKeyedToTheWholeCheckoutPath` is the exit code the comment was
  standing in for: it asks make for the value from two directories of the same
  name and from one whose name holds a space. It **evaluates** the Makefile
  rather than parsing it, which is the opposite of the other guards in
  `cmd/check-coverage`, because the defect was in an evaluation -- banning
  `$(notdir` by name would fire on a correct use of it, which is
  `TestNoListenOutsideLoopback`'s own first version flagging `loopback.Listen`.
  Both halves were reinstated: the basename, and an empty digest, which is what
  a missing `sha256sum` leaves behind under pipefail and which the Makefile now
  refuses with `$(error)` rather than quietly sharing one path again. And the
  guard was deleted with the basename still in place, because `go test ./...`
  is entirely green over that defect -- nothing else in the tree reports it.
- **A test that asserts on a port number is asserting about the whole
  machine.** `TestForwarder_CancelReleasesEveryPort` checks the real property
  -- after `Run` returns, the same ports must be bindable again -- and
  `freePort` gets those ports by asking the kernel for an ephemeral one and
  giving it straight back. Between the release and the re-bind, anything else
  on the machine can take it; CI runs every package's test binary at once,
  each handing out ephemeral ports, and it did:
  `bind: address already in use` on a pull request that had not touched that
  package, and never once in 300 local runs. A single attempt turns that into
  a red build on an unrelated change, which is how a working check gets
  ignored rather than obeyed. The assertion retries to a bound now, which
  keeps the property -- a forwarder that really held its listener still fails,
  a second or two later -- and loses only the false failure. `EADDRINUSE`
  cannot say who holds the port, so the message says that rather than
  accusing the forwarder.
- **And that test could not have failed anyway, which is the more useful
  finding.** On the cancellation path the leak is unrepresentable: closing the
  listener is what unblocks a goroutine parked in `Accept`, so `wg.Wait`
  cannot return until every listener is closed and `Run` returning *implies*
  the ports are free. Measured rather than reasoned -- deleting `closeAll`
  from the exit path entirely leaves the test green, because
  `context.AfterFunc` has already done it. What it guards is therefore the
  shape: the day somebody unblocks `Accept` another way, `closeAll` becomes
  the only thing releasing the ports. The leak that IS reachable is the
  early-return path, and reinstating it there -- a `Run` that refuses after
  opening a listener and forgets to close it -- is what proves the retry did
  not turn either assertion into a no-op.
- **A lab helper owns what it creates.** `CreateVolume` registers its own
  cleanup rather than leaving each lab to remember, because cleanups run
  last-registered-first: the seeded volume goes before the stray check looks,
  while a volume ember itself left behind is still reported. The first lab
  that forgot would otherwise have turned a working check into a failing
  suite, which is how a working check gets weakened instead of obeyed.

  **`TagImage` is the same rule where the stray check cannot reach at all, and
  the lab holding it asserted neither half of its own name.** A stand-in tag --
  `ember:lab-sm75` and the two beside it, for the labs about which image ember
  *chose* -- is removed by name and never by force, because a forced remove
  takes `ember:lab` out from under every lab after it. Nothing else can see
  either outcome: `labdaemon.Objects` has containers, networks and volumes and
  **no image kind**, `make reap-labs` goes through that same listing, CI's
  stray step is `docker ps -aq`, and a tag inherits its source's labels, so a
  stray one is indistinguishable from `ember:lab` by label. The harness comment
  saying "ownership is the only mechanism there is" was right, and
  `TestImageSelect_AStandInTagSurvivesNothingAndLeaksNothing` -- the lab holding
  that ownership -- checked that the tag existed and that `ember:lab` had not
  been removed **inside its own body** (#114), which are properties of
  `docker tag` and not of the cleanup it is named for. A `TagImage` whose
  cleanup stopped working would have leaked three names onto the developer's
  daemon on every suite run, for good, and that lab would have stayed green.
  It is a cleanup registered *before* `TagImage` now, so it runs after that
  one -- `CreateVolume`'s ordering -- and the in-body checks stay, because
  without them both post-conditions would pass against a tag that was never
  created at all, which is a refusal consistent with refusing everything. **The
  leak is three image NAMES sharing every layer with `ember:lab`: no bytes, no
  money, nothing a later run trips over**, so the answer is an assertion in one
  lab rather than a fourth kind in `labdaemon`, and what was being fixed is a
  name that claimed more than the code. Checked by stopping `TagImage`'s
  cleanup from removing the name it created: red with the assertions, green
  without them.
- **A lab that leaks on purpose says so.** The SIGKILL labs exist to leave a
  node behind, and the first run of the repaired stray check reported them.
  They call `Reap()` themselves once they have finished asserting, which
  declares the orphan as deliberate and keeps the check unconditional for
  every lab that leaks by mistake. Relaxing the check instead would have put
  it straight back where it was. Their orphan is still reaped under the scoping
  above, and must be: it was created during this suite run, which is the only
  thing that distinguishes it from the stale container of a run that died
  yesterday -- and that one is left alone because nothing here made it.
  **There are three of them and this entry said two**: `ember serve`'s,
  `TestOrphan_SigkillLeavesExactlyOneFindableNode`, and
  `TestOrphan_DryRunReapDestroysNothing`, which kills ember as well and reaps
  from a `defer`. The declaration does not turn the final check off, which is
  the thing a reader would reasonably assume and is measured two entries up:
  the cleanup's snapshot is taken after a lab's own reap, so an incomplete one
  is reported against the lab that declared it.
- **The run id travels into `Destroy`; it is never re-derived from the thing
  being destroyed.** `local.Destroy` used to read it off the container's own
  label in order to find the bridge network beside it -- and a container
  removed by anything but ember (`docker rm`, a prune, a crashed lab's
  cleanup) made that inspect return not-found, which Destroy treats as
  success, so the run id stayed empty and `Destroy` returned `nil` over a
  network that is still there. Nothing bills for a bridge network, which is
  why it was invisible; what it costs is every local run failing at once,
  weeks later, with "all predefined address pools have been fully subnetted",
  an error naming Docker and not ember. Every caller already had the run id
  -- provision is destroying a run it created, reap reads it off the finding,
  `ember down` off the record -- so the parameter asks for nothing that was
  not known. The container's own label still wins where there is one: the
  label cannot be wrong about which run a container belongs to and the
  argument can, and believing a wrong argument would remove a *live* run's
  network.
- **A leftover is swept by label, because the label is what survives.**
  Passing the run id closes the case going forward and can do nothing for a
  network that leaked before, or for `ember down provider/id` on a node with
  no record whose container somebody already removed -- there, nothing
  anywhere knows which run it was. So `provider.LeftoverAPI` lists networks
  whose run has no node container and `ember reap` removes them, reported by
  `ember ls` rather than discovered by an exhausted address pool.
  Containment twice, by two mechanisms, as the tar extractor is: the argument
  cannot help what is already on the daemon, and the sweep cannot act until a
  deadline has passed.
- **A leftover needs attention and a store does not, because ember can tell.**
  A store existing is the expected state and ember cannot know whether the
  weights on it are still wanted, so flagging one would teach the operator to
  ignore exit 2. A network whose node is gone is unambiguously garbage.
- **The sweep is bounded by the deadline, and that is the create window.**
  `local.Create` makes the network before the container, so for a moment every
  healthy run has a labelled network with nothing attached -- identical to
  wreckage. A deadline in the future cannot be a run that has finished, so a
  leftover inside its deadline is not reported *at all* rather than reported
  and not reaped: a finding that is sometimes false is worse than one that
  arrives up to a TTL late, and the deadline is already reap's authority over
  nodes. A zero deadline -- an ember label too old to parse -- counts as past,
  because the alternative is that it leaks for good.
- **`AssertLeftoverCapsMatch` checks the claim in both directions.** A
  provider that implements `LeftoverAPI` and forgets `Caps.Leftovers` is never
  swept, which is the direction that leaks; the mirror of
  `AssertStoreCapsMatch`, and the reason both are assertions rather than
  comments.
- **`node.Leftover` carries a `Kind string` and that is deliberate.** There is
  one kind today. A typed enum would be a closed set invented from a single
  example, which is the mistake a driver interface designed from one server
  would be; the field is read from the label the provider wrote and is only
  ever displayed.
- **A lab seeds state out-of-band, through Docker, not through the code under
  test.** Widening `sshx.Runner` with stdin support so a test could write a
  file would be a production interface shaped by a test's convenience.
- **Tests inject credentials rather than resolving them.** Left to read the
  environment, every CLI test picks up the developer's own `RUNPOD_API_KEY`,
  registers the real provider and makes live API calls -- behaving differently
  from one machine to the next. `deps.CloudBaseURL` exists for the same
  reason: a test that registers a cloud provider must not be able to reach it.
