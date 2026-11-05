# Authenticating `ember serve`

A loopback port is reachable by every uid. The mandatory token, the cookie
that carries it, the unix socket beside it, and their stated limits.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **`ember serve`'s `Origin` and `Host` checks bound a browser and nothing
  else.** Nothing it serves is reachable off the machine, so the threat is
  not an attacker on the network -- it is any page the operator has open, which
  can `fetch` a loopback port and queue a generation. The browser hides the
  *response* cross-origin and the side effect has already happened, so CORS is
  not a control and a server relying on it would be relying on a thing that
  does not work. `--allow-paid` is off by default, so a cross-site POST to a
  default invocation can start a free local container and spend nothing. The
  `Origin` and `Host` checks on every route are not authentication; the second
  is the DNS rebinding case, where an attacker's domain resolving to 127.0.0.1
  makes their page same-origin with its own name -- passing the first check and
  failing the second.
- **A loopback port is reachable by every uid, and a request with no `Origin`
  is allowed on purpose, so those two checks closed nothing against a client
  that is not a browser.** `curl` sends no `Origin` and neither does any script
  an operator writes, so refusing a request without one would break the command
  line without stopping a page -- and that allowance is indistinguishable from
  the account next door. On `ember serve --allow-paid` a second uid was one
  `POST /api/requests` from renting an H100 on somebody else's card, and `ssh -L
  7777:localhost:7777` -- the deployment ember is otherwise exactly the tool for
  -- puts the same port on a laptop's loopback with the same property. So there
  is a credential, it is **mandatory**, and `webapi.New` refuses a zero token:
  an unauthenticated `ember serve` is not a configuration this program can be
  in rather than one the documentation warns about, because the operator who
  did not read the documentation is the one with another account on the box.
  It is **additive** -- the credential runs after `checkHost` and
  `checkOrigin`, all three or nothing -- and `--allow-paid` is untouched and
  still off by default, because authentication answers who is asking and the
  flag is a ceiling on what the answer permits.
- **`internal/loopback` was NOT widened, and that was the alternative worth
  taking seriously.** A unix socket at 0600 answers the every-uid case with no
  secret at all and would be the stronger mechanism -- and it needs
  `nolisten_test.go` to stop banning `ListenUnix` by name, which is the one
  seal every other guard here rests on. It also answers nothing about the
  forward: the client end of `ssh -L` is a TCP port on loopback again, so the
  second uid reappears one hop away, and a browser cannot speak to a unix
  socket without a forwarder in front of it that would have the same problem.
  `SO_PEERCRED` is a unix-socket mechanism and its TCP equivalent is matching
  a source port against `/proc/net/tcp` -- racy, Linux-only, and wrong through
  a forward, where the connection is made by sshd as the logged-in user and
  would therefore pass. One token answers both threats and widens no seal.

  **`internal/loopback` HAS been widened now, it is additive, and every clause
  of the paragraph above is still true except the one about the seal -- which
  was never true of the code.** The two things that changed are that #29
  shipped, so the socket is a *choice* rather than the only way to close the
  every-uid case, and that #146 needs a channel a **browser cannot reach** --
  which is the one property that paragraph lists as a drawback and is, for a
  CLI client, the whole requirement. So the socket is a second channel beside
  the port rather than instead of it: the token stays mandatory, `webapi.New`
  still refuses a zero one, and the token is still the only answer on TCP.
  The forward argument is why: `ssh -L 7777:localhost:7777` puts a TCP port on
  a laptop's loopback and a socket on this machine says nothing about it, so
  deleting the credential would have reopened exactly the threat it was built
  for. `SO_PEERCRED` is still not used and still not needed -- the mode is the
  check, applied by `connect(2)` before ember sees a byte, and it needs no
  code at all.

  **The clause that was untrue: `nolisten_test.go` did not have to stop
  banning `ListenUnix`, because the ban was already "outside this package".**
  The walk skips any file whose path starts `internal/loopback/`, so
  `net.ListenUnix` and `net.Listen("unix", …)` were always legal here and
  illegal everywhere else; `StateSocket` needed no loosening and got none.
  What the entry read as a cost was a cost that did not exist -- the same
  shape as `ember ls --all`, an argument that persuades about a mechanism
  nobody checked. So the re-seal is a **correction and a test** rather than a
  change: the map's comment said "entry points in net that can publish a
  **port**", which stopped being the rule the day a path could be published,
  and the inspection called `t.Errorf` inline so the only evidence it worked
  was a clean tree -- which is also what a broken matcher reports.
  `findListens` is extracted, and `TestNoListenGuardCatchesEverySpelling`
  hands it eleven spellings that must be reported, the unix two among them,
  and five that must not. **The negative control is the half that matters**:
  the first version of this guard flagged `loopback.Listen`, the one call it
  exists to permit, so the correct spellings go through the same function and
  must come back clean. Checked by pasting `net.ListenUnix` and
  `net.Listen("unix", p)` into `internal/webapi` and watching the walk report
  both.

  **`Socket` is `Bind`'s shape and the directory is the caller's only say.**
  Unexported fields, no struct-literal path, `StateSocket` the one
  constructor: a caller names a **state directory**, the way a caller of
  `Port` names a port, and this package appends `socket/serve.sock`. There is
  no `--socket` flag and a lab asserts there is none -- not because the path
  is a secret, it is not, but because a path an operator can type is a path
  outside the directory whose mode ember checked.

  **The mode is the authentication, and `bind(2)` takes no mode argument.**
  That is the one thing about this that is not obvious: the socket is created
  at `0777 &^ umask`, and measured on this machine, whose umask is 0002, a
  socket bound with no intervention is **0775** -- group-writable, and
  `connect(2)` needs *write* permission, so every member of the operator's
  primary group could have reached it. A socket chmod-ed to 0000 refuses its
  own owner, measured, so the mode is a live control rather than advice. The
  obvious repair is `os.Chmod` after the bind, and **between the bind and the
  chmod the socket is already connectable** -- so `ListenSocket` holds the
  umask at 0177 for the length of the bind instead, under a mutex, and the
  socket is 0600 from the moment it exists. What that window costs is
  measured rather than waved at: umask is per-process, so for the length of
  one `bind(2)` anything else ember creates would come out at 0600/0700 --
  and every mode literal in ember's own packages is **already 0600 or 0700**,
  28 of them with not one wider, so nothing can come out different and
  anything that did would come out stricter. It also leaves no uncoverable
  branch, which a chmod's error return would: `internal/loopback` is gated at
  100 and a failure on a file this process has just created in a directory it
  owns is the class this repository excludes elsewhere by argument.

  **Containment twice, by two mechanisms, as the tar extractor is.** The
  socket is 0600 *and* it lives in a directory ember creates at 0700 whose
  parent is refused if group or others can write in it -- so the mode holds if
  the directory is widened and the directory holds if a filesystem ignores the
  mode. The directory check is `keys.LoadSigner`'s rule about a private key's
  parent reached through a socket, and **stricter than its `0o002`** on
  purpose: that one guards a file an operator placed, where a group-writable
  directory may be something they meant, and this guards a directory ember
  makes for itself, so there is no wider mode to tolerate. What a group member
  could do with it is the sharp version -- unlink the socket and leave *their
  own* listener at ember's path, which hands the operator's requests, and the
  rentals they pay for, to somebody else. A subdirectory rather than the state
  directory itself for exactly that reason: ember creates this one and
  therefore owns its mode, while `state_dir` is operator-named and may predate
  the feature.

  **"Arrived on the socket" is a context value and there is no wire spelling
  of it.** This is the one way the change could have been worse than no
  change: a header would be accepted on the loopback port too, and the
  mandatory credential would have been replaced by a string anyone on the box
  can type. The key is an unexported zero-size type used as its own value, so
  nothing outside `internal/webapi` can put it on a context; it is set by one
  `ConnContext`, on one `http.Server`. **Two servers and not one that inspects
  the connection**, because a type switch on a `net.Conn` re-derives the fact
  from the connection where the *listener* is what `loopback` proved -- so the
  difference between the two channels is a missing field rather than a
  condition. And `ServeSocket` takes a `*loopback.SocketListener`, a concrete
  type whose only constructor is `ListenSocket`, so handing it a TCP listener
  is a **compile error** -- `hostkey.Pin` and `exposure.PortRequest`'s shape,
  applied to the thing that would otherwise drift into "either channel is
  fine".

  **`checkOrigin` still runs on the socket and `checkHost` does not, and that
  is a decision rather than a consequence.** `hostOK` is the one place it is
  written, for `guarded` and for the websocket handshake both, because two
  copies of "unless it came in on the socket" is two places for one to be
  forgotten. `checkHost` compares the `Host` header against the port this
  server listens on, and a request that arrived on a socket came through no
  port -- `curl --unix-socket` sends `Host: localhost`, which does not parse
  as host:port at all -- so requiring it would not be a stricter check, it
  would be a channel that refuses everything. `checkOrigin` is about who
  **composed** the request rather than where it arrived, costs nothing, and
  through a proxy an operator puts in front of the socket it is the only thing
  left that refuses a page on another site. The cookie and the `?t=` exchange
  are left reachable and unrefused: both are browser mechanisms on a channel a
  browser cannot open, so a branch for them would be one only the account that
  is already authenticated could take.

  **Windows is a refusal and not a socket, and it is a build tag rather than a
  runtime check.** `net.Listen` there will bind AF_UNIX and `os.Chmod` honours
  only the read-only bit, so the socket would exist, the announcement would
  describe a channel authenticated by the operating system, and the one thing
  authenticating it would not be set -- the "reads as working" defect, on a
  security control. `//go:build !unix` returns an error naming the platform
  and the mode, so there is no branch anywhere that could conclude at run time
  that this machine cannot have a socket when it can.

  **The gain is asserted in two halves and the third is a deduction, which is
  said rather than implied.** The sentence this entry opens with is "answers
  the every-uid case", and what proves it would be a connect from a second
  account -- which a test run cannot make without root, and a lab that needed
  root would be a lab nobody runs. So the socket's 0600 and the directory's
  0700 are asserted against what a real `ember serve` created, `connect(2)`
  needing write permission is measured by hand, and "another uid cannot
  connect" follows from the two. It is a deduction from two measured facts and
  not a third measurement; there is no test in this repository that a second
  uid is refused.
- **Replacing `subtle.ConstantTimeCompare` with `==` is caught by nothing, and
  that is written down rather than papered over.** Thirteen of the fourteen
  defects reinstated for this change went red; that one did not, and no test
  in this repository could. A timing oracle is a statistical property of a
  machine under load, and a test that asserted one would either be flaky in CI
  or be a test of the CI runner. What stands in its place is that the
  comparison is four lines in one function with the reason written above it,
  and that `s.tokenDigest` is the only thing the comparison can reach -- the
  raw token is on `opts` for the printed URL and nothing else. This is the
  honest answer to "which of your guards has no test", and the kind of guard
  that gets lost in a refactor nobody reviews closely.
- **The token is compared as a digest, never as a string.**
  `subtle.ConstantTimeCompare` is constant time in the length of its arguments
  and returns immediately when they differ, so comparing raw values leaks the
  token's length; `==` leaks the matching prefix one byte at a time. Hashing
  both sides first makes every comparison the same thirty-two bytes, which is
  also why "a token of the wrong length" is not a case in the code -- it cannot
  be reached as one.
- **The browser's carrier is a cookie, and that is what let the frontend stay
  unchanged.** A header is the obvious answer and it fails in three places at
  once: `coder/websocket` cannot set a request header from a browser, so the
  socket would need the token in its query string; the gallery's `<img src>`
  cannot set one either, so the image route would need it in a URL too; and
  `sessionStorage` is per-tab, so a second tab is unauthenticated. An
  `HttpOnly; SameSite=Strict; Path=/` cookie is carried on `fetch`, on the
  handshake and on an `<img>` alike, cannot be read back out by a wasm panic or
  a stray console, and `Strict` closes the one browser hole `checkOrigin`
  deliberately leaves open -- a no-`Origin` GET, which is what a top-level
  navigation and an `<img src>` are. Not one line of `web/` changed.
- **`?t=` is honoured on the app's own route and nowhere else.** The token has
  to reach the browser somehow, and a URL fragment -- the candidate the issue
  described -- never reaches the server, which is its one advantage and is paid
  for by having to serve the page unauthenticated so a script can read the
  fragment: the one route a browser navigates to would be the one route with no
  check on it. So the server consumes a query parameter instead, on the app
  route only, answers with the `Set-Cookie` and a **303 to the same path
  without it**, and the whole surface stays gated. The address bar, the session
  history and every `Referer` the page sends carry nothing afterwards. It is an
  online guessing oracle in principle, which is why a minted token is
  `crypto/rand.Text` and not something an operator chose.
- **No `--token` flag, and a lab reads `/proc/<pid>/cmdline` to prove it.** The
  rule is already written down for provider tokens; this is the first
  credential where the uid that could read argv is the same uid the credential
  exists to exclude, so the assertion is against the running server rather than
  against the source. `EMBER_SERVE_TOKEN`, `serve_token` in
  `credentials.yaml` (0600, `node.Secret`), or `crypto/rand`.
- **A minted token is printed and a configured one is not.** A credential the
  operator did not choose has to be printed or it is unusable; one they put in
  a file does not, and a second copy on a stdout that a systemd unit may be
  writing to a world-readable journal is a copy they did not ask for. The
  fingerprint goes there instead. Nothing else prints either: the refusal log
  records the path and which carrier was present, and **not a digest of what
  was presented** -- an operator who pasted the wrong password would otherwise
  have it hashed into the journal permanently, which is the thing `node.Secret`
  exists to prevent rather than an application of it.
- **One refusal message for no credential, a wrong one and a stale cookie.** A
  caller learning which of the three it was learns something about the secret.
  The page load's refusal is **plain text** rather than the API's JSON, because
  that response is what an operator sees when they type the port from memory or
  open a bookmark from before a restart -- and a raw JSON object rendered in a
  browser reads as "ember is broken", which is the failure mode that gets a
  control worked around instead of used.
- **The browser lab drives both directions, because every claim about the
  cookie is a claim about Chrome.** `TestServe_UI` loads the page with no
  credential and asserts the app did *not* mount, then opens the one-time URL
  and asserts the token left the address bar, the offers load, the progress row
  advances and the image decodes. The third of those is the one no Go test can
  stand in for: whether Chrome attaches a `SameSite=Strict` cookie to a
  websocket handshake. A unit test asserting that ember accepts a cookie it was
  handed proves ember agrees with itself, which is the closed loop
  `capabilities.model` and the unrecorded preview frame both sat in. The
  refusal half lives in the same lab rather than a second one so that
  weakening either is impossible without the other going red.
- **The cross-origin labs now carry the token.** Three of them asserted a 403
  and would have gone on passing with the `Origin` and `Host` checks removed,
  because a request with no credential is refused anyway. A refusal test that
  can pass for either reason proves neither, and that is how a working seal
  gets quietly weakened while the suite stays green -- the `AssertNoStray`
  failure, reached from a third direction.
- **And the page told every operator there was no authentication for the whole
  of that time, which is the `ember ls --all` class in the running program's
  own UI rather than in a comment or a document (#167).**
  `web/src/views.rs`'s posture line
  ended `· no authentication: any page in this browser can reach this port`,
  unconditionally -- outside the `if c.allow_paid` above it, so on screen in
  both postures on every page load -- and `Posture`'s own doc comment rested
  the `--allow-paid` default on "what bounds the damage of there being no
  authentication". A verbatim survival of the state #29 closed: `webapi.New`
  refuses a zero token, a page on another site fails `checkOrigin` before the
  credential is asked for, and `SameSite=Strict` means a cross-site request
  carries no cookie at all, which is the half that clause was about. **The
  distinguishing claim is the SURFACE and not the audience**, and the first
  wording of this entry had it as "read by whoever next edited the code"
  against "read by the operator" -- which is wrong about this file's own
  record two hundred lines up: the `--all` on an `ember ls` and the
  `ember up --config ember.local.yaml` naming a file that never existed were
  both in `README.md`, which operators read. What is new is that this one was
  in the **program's own UI**, printed by the running process to the person
  deciding whether to walk away from it, where a document is read before the
  decision and a comment is read by nobody making it. And it was wrong in the
  direction that makes a working control look absent. `README.md`'s own
  paragraph was corrected when the token landed and the page was not, which is
  "a claim is worth exactly what the code does" with the two surfaces one
  commit apart.
  **The replacement is `announceToken`'s two limits and not its two
  mechanisms, and the reason it is on the page at all is #105's**: that
  paragraph prints the control and its limits into a terminal, and the
  operator this page is for may never see one -- the one-time URL means
  somebody had a shell, and `ssh -L 7777:localhost:7777` means that shell is
  on the other machine. So "every request on this port needs a token; it does
  not keep out your own uid or root, and it never expires", all three spans,
  because a control whose limits are not printed is a control that gets
  believed past them and a line that said only the first would be the old
  defect inverted. Not a deletion, which was the other candidate: a warning
  naming a condition the program cannot be in is worse than silence, and that
  is an argument about the old clause rather than about a true one.
  **THE FIRST VERSION KEPT ONE LIMIT OF THE TWO AND CALLED ITSELF THAT
  PARAGRAPH COMPRESSED**, which is the same shape one scale down: non-expiry
  was dropped, it is the more surprising of the two and the one that bears
  hardest on "should I leave this running", and the argument for printing the
  control here -- a terminal this operator may never see -- cannot also be the
  argument for withholding a limit of it. Both limits are there now; both
  *mechanisms* (which uid can read the token, and that a restart is what
  revokes it) are not, because a page gives the fact and the terminal and
  `README.md` give the how -- and the clause says so where it is written
  rather than reading as lossless.
  **"On this port" and not "every request", for one channel's worth of
  precision.** `ServeSocket` answers the same routes with no token, because
  the socket is 0600 in a 0700 directory and `connect(2)` is the check. As a
  claim about WHO can reach this server the shorter spelling was already true
  -- 0600 means the operator's own uid -- so it over-claimed only about HOW,
  by one channel, and that is a different defect from the one it replaced: the
  old clause reported a live control as ABSENT, which is a false negative
  about a control, and this would have been a true control described one
  channel too widely. Three words close it, against leaving the next reader to
  re-derive the socket exception.
  **A `WaitVisible` on the one line of that page carrying a security claim is
  what let it stand.** `TestServe_UI` waited for `.posture` to appear and
  asserted nothing about its text, so the lab written to drive the page an
  operator reads passed against either wording for the life of the clause --
  the same shape as the three cross-origin labs above it. It reads the text
  now, and the **absence** of `no authentication` is asserted beside the
  presence of the new sentence, because a check that only looked for the new
  text would pass against a page printing both. The control and each of its
  limits are separate assertions for the same reason: one `Contains` over the
  whole clause cannot tell a dropped limit from a dropped control.
  **The clause is static and there is therefore no `cargo test`**, which is
  stated rather than papered over. The credential is mandatory, so there is no
  state of this program a `Config` field could switch the sentence on -- the
  `Listing::answer` / `Rows::note` / `Stream::note` shape is for a decision,
  and asserting a string literal against itself is the closed loop this file
  refuses elsewhere. The exit code available is the lab's.
  **IT WAS NOT RUN BY THE CHANGE AND WAS RUN BY THE VERIFICATION, AND THAT
  ORDER IS THE ENTRY RATHER THAN A NOTE ON IT.** The commit made no claim
  about it, because a lab takes the flock on the one Docker daemon and this
  file has had to retract a red-test claim twice. Run afterwards, scoped to
  `TestServe_UI` with a `make web` per step and a control at both ends: the
  opening control **0**, the old clause restored **1** with all three of the
  assertions firing, the closing control **0**. And the split is what the
  discriminator measured rather than the redness: dropping **only** the limit
  half fires **exactly one** of them, so the halves do the work claimed for
  them -- three firing together would have proved the suite went red and not
  why. The third span, non-expiry, landed after that measurement and is
  asserted on the same reasoning and no exit code of its own. The scoping is
  not cosmetic either: `make test-serve-lab` is exit 2 on master over a
  deterministic failure in `TestServe_UIResumesAProgressStreamItLost`, which
  is **#185** and nothing to do with this -- and the only reason it surfaced
  as a finding rather than as a false "lab green" in a commit message is that
  the commit declined to assert an exit code nobody had.
