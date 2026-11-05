# Host keys

Minted here and injected, never trusted on first use. `hostkey.Pin` is the
only `Trust` there is — and the pin file that contradicted it is deleted.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **No ember function takes an `ssh.HostKeyCallback`.** `hostkey.Trust` has one
  unexported method and that package holds no permissive implementation, so
  `ssh.InsecureIgnoreHostKey()` cannot be typed into the program — not behind a
  flag, not in a test. It is the most common way an SSH client ships with no
  server authentication at all, and "we grep for it in review" has a 100%
  failure rate eventually.
- **The pin is built from the key ember minted, never from the one the
  provider reports.** ember generated it and injected it, so it already knows
  which key the node must present and never has to ask. Reading the
  provider's answer first was a real bug: the local provider echoes the key
  back on a container label so it worked there, while RunPod reports nothing,
  and ember's first real cloud provision fell through to trust-on-first-use
  against a brand-new IP — the one case TOFU cannot defend, and the exact
  thing minting the key exists to avoid. It logged a warning, and a warning
  is not a security control.

  **Neither was the test.** For the life of both regression tests the final
  assertion was that the log did *not* contain `"first use"` -- and nothing in
  ember has ever logged that string, so the assertion could not fire. The
  positive half above it could not tell the two mechanisms apart either, and
  the reason is an ordering nobody would look twice at: it offered the
  *injected* key first and required acceptance, then a foreign key and
  required refusal, and a trust-on-first-use trust accepts the first key it is
  shown and refuses every other -- so it satisfied both. The lab's dial was no
  better, because the node really does present the key ember injected: a dial
  proves the key matched and cannot say which mechanism decided it.
  Reinstating the one-line fallback -- TOFU when the provider reports no key,
  every other guard in `trustFor` intact -- was green everywhere.

  **So the mechanism is a type now, and the assertion is positive.**
  `provision.trustFor` and `provision.Session.Trust` are `hostkey.Pin`, a
  concrete type whose only constructor is `hostkey.Pinned`, so
  `return o.Pins.TrustOnFirstUse(id), nil` on that path is a **compile error**
  rather than a thing a test has to notice -- the shape `exposure.PortRequest`
  and `loopback.Bind` already use. `hostkey.Trust` stays an interface, because
  its own seal is what keeps `ssh.InsecureIgnoreHostKey` untypeable and the
  dial legitimately takes either; `TrustOnFirstUse` stays, for a provider that
  genuinely cannot accept an injected key, and is no longer reachable from the
  path that rents machines. The honest limit: this seals `trustFor`, not every
  conceivable dial, so both tests now assert the mechanism rather than a
  message -- **a key ember did not mint, offered before anything else has
  touched the trust, must be refused**. TOFU cannot refuse its first key; that
  is what TOFU is. The acceptance is asserted beside the refusal, because a
  refusal alone is consistent with a trust that refuses everything, which is
  what a zero `Pin` is. A zero `Pin` refusing everything is itself the
  decision: `Pinned`'s error used to hand back a nil `Trust`, which panics in
  `Callback`, so a dropped error is now the strictest answer rather than a
  crash. Checked by reinstating the original defect with the type widened back
  -- both tests go red, the lab twice, because the foreign key it refuses
  first is the key TOFU would have pinned.
- **A provider reporting a key that differs from the injected one is a
  refusal.** It has substituted something, and continuing would pin the
  substitution.
- **A node with no minted key is refused, not downgraded.** Reaching that
  branch would mean ember had stopped injecting, which is worth a refusal
  rather than a quiet fallback. This used to say TOFU "is unreachable through
  `WithNode`, which mints for every node", and that was an argument about a
  *branch* rather than about a seal: minting is why nothing took the fallback,
  not why nobody could write one, and one line in `trustFor` reinstated it.
  What makes it unreachable is the return type, two entries up.
- **A provider may name public ports it adds unconditionally, and only
  those.** RunPod maps its web terminal port (19123/http) to every pod with no
  way to disable it — found when ember's first real provision refused the node
  and destroyed it. `Caps.PlatformPorts` names it; everything else still
  destroys the node. The concession is only safe while nothing in the node
  image listens there, so that is a lab assertion about the image rather than
  a promise from the provider, and a workload asking for that port is refused
  outright.
- **Host keys are minted here and injected, not trusted on first use.** TOFU
  against an IP that has never had a key is a free MITM for anyone on the path.
  Injecting through the provider's API concedes the key to the provider, which
  already has root on the node and therefore learns nothing; it closes the
  window against everyone else.
- **Pins are keyed by node id, not host:port, and die with the node.** A pin
  keyed by IP matches whoever gets that IP next, and the resulting churn is
  exactly what talks people into disabling host-key checking.
- **A host key mismatch is a hard error with no `--force`.** A prompt is a
  bypass with extra steps; the correct response for an ephemeral node is
  always to destroy it.
- **`node.Secret` carries key material and prints a fingerprint.** One
  `slog.String("hostkey", string(pem))` at debug level writes a private key
  into the journal permanently, and nothing about that line looks wrong in
  review. `String()` is redacted too, because a stray `%s` in an error is the
  other way it escapes.
- **No secret ever reaches argv.** `/proc/<pid>/cmdline` is world-readable, so
  a key or token passed as a flag is readable by every user on the box for the
  life of the process. Secrets live in `~/.config/ember/credentials.yaml`
  (0600) or the environment, and reach providers in the API's JSON body.

- **"Pins die with the node" is structural now, and the pin FILE that
  contradicted it is deleted.** The entry above says a pin keyed by IP matches
  whoever gets that address next and that the churn is what talks people into
  turning host key checking off -- and `hostkey.Store` sat in the same package
  with an atomic, versioned, 0600 JSON file at
  `<state_dir>/known_hosts.json`, which is persistence across processes for a
  thing that is supposed to die with one. Nothing ever wrote it. The disposal
  is `queue.Cost`'s and not `sdserver.Client.Cancel`'s, and the test is the
  one those two entries set: **is there a second live mechanism for the thing
  this would do**.
  **There was no FIRST mechanism, which is the finding.** The only production
  dial in the tree is `internal/app`'s `dialNode`, reached only from inside
  `provision.WithNode`'s closure and handed a `hostkey.Pin` that `trustFor`
  built in memory from the key `WithNode` minted seconds earlier in the same
  process. Nothing read a pin by id: `Store.Get` had no caller outside the
  store's own tests, and its waiver in `cmd/check-coverage` said so in
  writing -- "for a reader that does not exist yet", which is the admission
  this file's own rules would have caught had anything been looking at it.
  **#146 is the case that looks like it needs persistence and does not.**
  `ember up` is a client of `ember serve` over a unix socket now, so "a
  long-lived process and a CLI that talks to it" is exactly the shape where a
  pin surviving a restart would earn its keep -- and `internal/cli`'s
  `upclient` names neither `hostkey` nor `sshdial` nor `ssh`: it POSTs
  `/api/runs` and follows a report stream, while the rental, and therefore the
  dial, happens in the server process inside `queue.Direct.Run`. The process
  that mints is the process that dials, in every shape ember has.
  **And the file could only ever have held keys ember never authenticated.**
  `tofu.check` was the only writer, so every pin in it was a trust-on-first-use
  pin by construction; persisting one makes a first-sight guess authoritative
  for every later process, which is worse than having no file. What a future
  attach-to-a-running-node verb would need is `hostkey.FromNodeKey` plus
  `Pinned` off what the provider recorded, checked against the minted key --
  which `trustFor` already does, and on a provider that reports nothing there
  is nothing to rebuild from, so a remembered TOFU pin would be the wrong
  answer rather than a missing one.
  **Measured, not read, because this file has asserted a mechanism it did not
  have before.** With `saveLocked` made to panic, exactly two packages in the
  whole unit suite reached it -- `internal/hostkey` and
  `internal/tunnel/sshdial`, both from their own test files -- and
  `internal/provision`, `internal/app` and every other package stayed green:
  nothing in production writes a pin file. And `TrustOnFirstUse`'s waiver kept
  it "for a provider that genuinely cannot accept an injected key, of which
  there is none today", which is a shape ember cannot **represent** rather than
  one it has not met: `node.NewCreateRequest` refuses a zero `HostKey`, and
  dropping that refusal turns `TestNewCreateRequest_Refuses` red at "no host
  key: NewCreateRequest accepted it". A waiver that names a case the type
  system already refuses is a waiver nobody can retire by reading it.
  **It is a strengthening of the seal and not a loosening, which is the
  opposite of what deleting a security type usually means.** `internal/hostkey`'s
  package doc rests the posture on two sentences: Trust's one method is
  unexported, which `TestTrustIsSealed` holds, **and** "this package contains
  no permissive implementation" -- which was **not true** for the life of the
  package, because `tofu` accepted whatever key it was shown first and the only
  thing keeping it off the renting path was #117's return type. `Pin` is the
  only implementation now, and
  `TestTrustHasOneImplementationAndItIsPin` parses the package's own non-test
  files and fails on a second one, because the count and
  `TestPinned_AcceptsTheKeyItHoldsAndRefusesEveryOther` are a pair: a count
  alone is consistent with one permissive implementation. It is a parse because
  reflection cannot enumerate the types a package declares, only ask a value it
  already holds, and the subject is a type nothing constructs yet.
  **That sentence was false when it was first written, and the guard was walked
  around on the first attempt.** A method receiver in Go is `T`, `*T`, `T[P]`
  or `*T[P]`; the first version unwrapped the star, named the `*ast.Ident` and
  **skipped** anything else, so a GENERIC receiver -- `*ast.IndexExpr`, or
  `*ast.IndexListExpr` for two parameters -- took the skip, and a generic type
  is a live `Trust` the moment it is instantiated. Measured, each patch applied
  and reverted: a permissive `check` with a value receiver is exit 1, with a
  pointer receiver in another file exit 1, **with a generic receiver exit 0** --
  and with an exported constructor for it plus `internal/app`'s `dialNode`
  handing it to `session.Dial`, `go build`, `go test ./...`, `go vet` and
  `golangci-lint` are **all exit 0 while ember dials every node it rents with
  no host key verification at all**, `TestTrustIsSealed`,
  `TestNoInsecureIgnoreHostKey`, `TestSessionCarriesAPinAndNotAnArbitraryTrust`
  and `TestNoProductionDeadCode` green beside them, because the types are
  untouched and the constructor has a caller.
  **The fix is `TestFlagTablesClassifyEveryFlagSetMethod`'s rule and the three
  lines are its**: a receiver this guard cannot name is a **fatal** rather than
  a skip, because "the alternative to failing on a method it cannot classify is
  reading straight past a flag, which is one more gate that cannot fail in a
  repository that has already found four." Both generic shapes now exit 1
  naming the receiver kind, a clean tree exits 0, and with the whole attack
  wired into the only production dial the one red light in the entire tree is
  this guard. A guard that can be walked around is worse than none, because it
  reads as protection -- and this one claimed its own completeness in this file
  before anybody had tried to get past it, which is the failure the rest of
  this entry is about, committed by the entry itself.
  **One gap is stated rather than closed**: a permissive `check` in a
  `_test.go` file of `internal/hostkey` is not reported, because the walk skips
  test files -- against this file's own "not behind a flag, not in a test" and
  "no escape hatch for a seal, not even for a test". It is bounded rather than
  fixed: production cannot reference a `_test.go` identifier and this package's
  own tests cannot reach `sshdial`, so such a type can be handed to nothing
  that dials. Including test files would mean the guard reporting itself the
  day a test legitimately needs a stand-in `Trust`, which is the shape of a
  guard people learn to ignore.
  **Nothing was narrowed that could have been, and the reason is where the
  compile error should sit.** `sshdial.Dial` and `session.Dial` still take the
  `Trust` interface although `Pin` is the only thing satisfying it. Narrowing
  them would move #117's compile error off the provisioning path, where the
  defect was, and onto the dial -- where a provider that publishes its
  fingerprint out of band would one day need the signature widened again, and
  widening a signature is how #111 arrived. `trustFor` and `Session.Trust` stay
  typed `Pin`, and the guard on that typing stays too: it is about the type, so
  it holds against the next `Trust` somebody writes, which is the one thing the
  deletion cannot do.
  **The three judgements are kept as a note where the code was**, the
  `node.RateFromFloat` shape, because whoever revives persistence makes them
  again: keyed by node id and never by host:port; write-then-rename **and**
  refuse a file from a newer ember, which are one rule because a pin file that
  will not parse or that quietly lost a pin re-arms trust-on-first-use for
  every node in it without anybody being told; and the file's mode is a control
  rather than hygiene -- a fingerprint is public material, so 0600 in a 0700
  directory is not about secrecy but about who can make their own key
  authoritative for a node ember will dial, which is the MITM the pin exists to
  refuse arriving through the filesystem instead of the network.
  **One thing found and deliberately left alone, and the first reason given for
  leaving it was false.** `node.HostKeyPinned`, the `HostKeySource` whose doc
  said "ember accepted a key it had never seen and recorded it; ember reports
  it so the operator can see that without reading logs", now has no writer
  anywhere and cannot get one. Leaving it is right; the reason written here was
  "a member of a printed enum, so removing it is operator-visible", and
  **nothing prints it**: measured, `grep -rn 'HostKey' --include='*.go'` over
  `internal/reconcile/` and `internal/cli/` returns nothing at all, so
  `ember ls` never reads `node.HostKey`, and the only non-test site that
  formats a `HostKeySource` is a `providertest` failure message. So the doc
  comment was untrue twice over and is corrected -- which matters because the
  comments in `config`, `genstore` and `app` were corrected in the same change
  and this one was not, which is this file's own "a claim is worth exactly what
  the code does" applied to a sweep that missed a line. The honest reason to
  keep the member is one the enum already carries: `HostKeyFromAPI` is a member
  nothing produces and says so plainly, so that is the precedent rather than
  `min_cuda`, and a constant costs nothing while removing one is a decision
  about the enum rather than a consequence of deleting a store.
  `TestNoProductionDeadCode` does not report it either way, because fields and
  types are out of its scope by the argument already recorded above. The stale
  waiver beside it -- `node.(HostKeySource).Authenticated`, excused as "ls
  reports the source itself" -- is wrong in the same way, is older than this
  change, and is left for whoever reads this.
  **And "no writer any more" was too generous to the constant: nothing has ever
  produced it, which makes it this list's first instance in a CONSTANT rather
  than a function.** The first wording in `ssh.go` blamed the deleted
  trust-on-first-use for having been its writer, and TOFU was not: it wrote a
  `Fingerprint` into its own map and never touched a `HostKey.Source`.
  `git log --all -G 'HostKeyPinned'` returns six commits -- the initial one,
  `b42130d`, `e08c99a`, `54393b1` and this change's two -- and at every one of
  them the only non-test appearances are the `const`, the `String()` map entry
  and, from `b42130d` to `54393b1`, a comment in `hostkey/store.go` reading "so
  callers record node.HostKeyPinned". No `Source: HostKeyPinned` assignment
  exists anywhere in that history outside a test file. The sharper measurement
  is that the instruction was unfollowable by the file holding it:
  **`store.go` never imported `internal/node`**, for the whole life of the file
  and not merely at a sampled commit -- `git log --all -G 'internal/node' --
  internal/hostkey/store.go` is **empty**, and that file has exactly two
  commits, `b42130d` creating it and `fae8b7a` deleting it. So the comment
  asked for something the file it sat in could not write, which is the scope
  that matters because `tofu` is declared there; the **package** could, and
  `mint.go` four files over imports `internal/node` and uses
  `node.HostKeyMinted`. That is the `ember ls --all` class in miniature -- an
  argument that persuades about a mechanism nobody wired -- and what #120
  deleted was the comment ASKING for the record, not a writer of it. So the running count above gains an instance whose kind the detector is
  **structurally** unable to report: `TestNoProductionDeadCode` loads functions,
  and the entry for it in this file already says fields and types are out of
  scope and why. A declared constant, printed by a `String()` nothing calls on
  it, asserted by its own test, is the same closed loop one declaration smaller.
