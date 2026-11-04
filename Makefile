# ember — see AGENTS.md for the rules this Makefile is the entry point to.
#
# Targets stay thin on purpose. Anything with logic in it belongs in a Go
# subcommand, not in a recipe: a Makefile is the one place in this repo where
# shell can still accumulate, and go-tun's verify-router target is what sixty
# lines of inlined ssh looks like a year later.

# SHELL and .SHELLFLAGS, because four gates could not fail and one of them was
# what stopped a bad image shipping.
#
# Every lab target pipes `go test` into `tee`, so a human watches the run live
# and the `--- SKIP` grep can read the log back afterwards. A shell pipeline
# reports the LAST command's status, and `tee` always succeeds -- so for the
# life of `test-image-labs`, `test-preview-lab`, `test-serve-lab` and
# `test-image-labs-publish`, **a failing lab exited 0**. The only thing that
# could fail those targets was the grep for a lab that skipped itself: the
# guard against the small failure was the only live one, bolted onto a
# pipeline that discarded the large one. Same shape as `AssertNoStray` running
# after the reap had emptied the daemon, one layer out of the suite.
#
# The fourth is the serious one. `publish-sd-cuda` and
# .github/workflows/workload-image.yml both run `test-image-labs-publish` and
# then push, so AGENTS.md's claim that a bad image never ships rested on a
# gate that could not fail -- a wrong-architecture image, a listener on a port
# that should not be open, a missing `embernode fetch`, every one of them
# checked by a lab whose failure the gate threw away.
#
# It is not only the labs. `cover`'s `go tool cover -func | tail -1` has the
# same shape, and what it loses is smaller but the same kind of thing: a
# profile the tool refuses prints no total and the target says nothing about
# it, so the one number a human reads off `make cover` can simply be absent.
# `cover-check`'s own recipe is a single `go run` whose status make already
# sees, so the gate itself was never at risk there -- only the figure.
#
# `pipefail` and deliberately NOT `-e`. make already runs each recipe line in
# its own shell and already stops the target on a non-zero line, so `-e` buys
# almost nothing here and breaks three recipes that tolerate a failure in
# order to say something useful about it: `workload-sd` copies a checkpoint
# into the tree and removes it from `rc=$$?; rm -f .model.gguf` *after* a
# docker build that may fail -- under `-e` a failed build would leave
# gigabytes behind -- and `web-check` and `workload-sd-cuda-local` read a
# version or an architecture into a variable and then print the command that
# fixes it when nothing came back. pipefail is the part this defect needed,
# and setting it here rather than per target is what keeps the next pipeline
# somebody writes from reintroducing it.
#
# .github/workflows/ci.yml already had all of this right -- `set -o pipefail`
# over both of its `go test | tee` steps and the SKIP grep in a step of its
# own with `if: always()` so it runs over a red suite. The Makefile was the
# odd one out, and the four targets it got wrong are exactly the gates no
# pull request runs: a laptop and the dispatch-only publishing workflow.
#
# .SHELLFLAGS reaches `$(shell ...)` too in GNU make, not only recipes. The
# three expansions below have no pipelines in them, so there is nothing for
# pipefail to change; a pipeline added to one later inherits the fix.
SHELL := /bin/bash
.SHELLFLAGS := -o pipefail -c

# Prefer the pinned toolchain when it is installed, so the Go that builds this
# cannot drift from what go.mod declares just because the system package
# updated. jira-mcp does the same.
GO ?= $(shell if [ -x "$(HOME)/sdk/go1.27.0/bin/go" ]; then echo "$(HOME)/sdk/go1.27.0/bin/go"; else echo go; fi)
export GOTOOLCHAIN ?= go1.27.0

BIN := bin
COVERPROFILE := coverage.out

# The build stamp. `ember version` answered `dev` in every build that has ever
# existed, because internal/cli.Version carried a comment describing a
# mechanism nobody had wired: there was no -ldflags anywhere in this file or in
# any workflow. Every measurement in README.md is a claim about what a specific
# build did, and none of them could name the build that produced it -- the same
# argument this repository makes for refusing a `:latest` image, applied to
# ember itself.
#
# `git describe --tags --always --abbrev=12 --dirty` is the whole decision, and
# what each part answers for:
#
#   --tags      any tag, not only an annotated one, because this repository's
#               only tag so far is a lightweight working marker.
#   --always    falls back to the commit hash when no tag is reachable. There
#               are NO release tags here today, and `git describe` without it
#               fails outright -- so this is the branch every build currently
#               takes: `ember version` prints a bare 12-character revision.
#   --abbrev=12 so a revision from here and one Go's own build info supplies
#               (internal/cli's fromBuildInfo, the fallback when nothing
#               stamps) are the same length. Two spellings of one fact is how
#               an operator ends up unsure which they are reading.
#   --dirty     a tree with uncommitted changes reports `<rev>-dirty`. A
#               measurement taken with a modified tree cannot be reproduced
#               from anything in the repository, which is exactly when saying
#               so is worth something.
#
# NOTHING INVENTS A TAG. A string like `v0.0.0-3-gabc` for a repository with no
# tags would name a release nobody cut, and a version that lies is worse than
# one that admits it: with no git at all -- a source tarball, `go build` outside
# a checkout -- VERSION is empty, the -X sets the var to empty, and
# internal/cli falls through to build info and then to the literal `dev`.
#
# Overridable (?=) so a release build can name itself without a tag being
# reachable, which is also what makes the value testable from a recipe.
VERSION ?= $(shell git describe --tags --always --abbrev=12 --dirty 2>/dev/null)

# -X is a SILENT no-op when the symbol it names does not exist: rename the var
# or move the package and the stamp stops happening with no error from the
# linker and no change in the build -- which is the same class of failure as
# the unwired mechanism above, and undetectable by eye. internal/cli's
# TestVersionStampTargetsThisPackage parses this line and the workflows, and
# fails when what they name is not a string var in that package.
GO_LDFLAGS := -X github.com/Legion112/ember/internal/cli.Version=$(VERSION)

# Packages whose statements must be covered, and the floor for each. A package
# in neither this list nor COVER_EXCLUDE fails the gate; see -strict.
#
# Pure packages are at 100 and stay there. The ones below it are all the same
# shape, and the floor is a decision rather than a drift: what is uncovered is
# (a) stdlib error returns that cannot fire -- crypto/rand never fails, and
# neither does marshalling a valid ed25519 key -- and (b) write, close, chmod
# and rename failures on a file that was just created, which need a full disk
# or an injected filesystem to provoke. Reaching them means a seam in every
# os call, which is more code and more surface than the branches are worth.
#
# Everything that *decides* anything -- every refusal, every parse, every
# trust check, every price comparison -- is covered. If a floor needs lowering
# to land a change, that is the signal to split the logic out of the I/O
# rather than to edit this line.
#
# internal/app and internal/cli are the honest exception: what is left
# uncovered is the wiring that constructs a real Docker client and resolves a
# real home directory, which the labs exercise. When a *decision* lived in the
# CLI -- when to retry an SSH dial, when to refuse one -- it was moved to
# internal/session, which is at 96 and tests against a real SSH server. That
# is the rule above being followed rather than the floor being lowered.
#
# The pipeline moved out of internal/cli into internal/app, and the floors
# moved with it rather than being inherited. Coverage is attributed to the
# package the code lives in -- `go test` runs with -coverpkg over the whole
# tree -- so moving ~470 statements lost nothing: internal/app's 83.6 comes
# out of tests that still largely live in internal/cli, and the gated overall
# did not move.
#
# internal/cli is RAISED from 83 to 88 in the same change, and that is the
# point of mentioning it. It measures 90.8 now that the untested wiring is
# only part of what is left, and leaving the floor at 83 would have been seven
# points of slack -- room for an entire new subcommand to arrive untested
# without the gate noticing. A floor that no longer tracks the package is not
# a floor.
#
# internal/app is at 81 against a measured 82.7, and the gap is deliberate
# twice over rather than generous.
#
# First, internal/tunnel's reason: orExited races an operation against the
# server's exit, and when ember stops the server by cancelling the context both
# arms of that select are ready at once, so which one runs is up to the
# scheduler. Consecutive runs of this gate have measured 83.6 and 83.4.
# Pinning the floor to the lucky number would make the build flake, and a gate
# that fails for no reason is a gate people learn to re-run rather than read.
#
# Second, Plan.WithServed's own body. It is provision.WithNode plus an SSH
# dial, and driving it in process would mean a dialer seam on Plan -- a
# production interface shaped by a test's convenience, which is the mistake
# sshx.Runner was kept from making. The decisions inside it are covered
# elsewhere: Served.Generate against a real sd-server client, the refusal of a
# config with no sd.serve, and the whole drain loop in internal/queue against
# fakes. What is left is the wiring, and the serve lab is what exercises it --
# the same arrangement internal/cli has had for the Docker client it builds.
#
# internal/tunnel is the one exception to that rule and is at 94 rather than
# its isolated 96.6: when the forwarder is torn down, pipe's select has both
# its channels ready at once -- the copy finishes because the connection is
# closing, and the context is cancelled -- so which arm runs is up to the
# scheduler. Pinning the floor to the lucky number would make the gate flake.
# Kept on one line on purpose: a make line continuation joins with a space,
# the shell then splits the result into two words, and Go's flag package stops
# parsing at the first non-flag argument -- so a wrapped list silently drops
# every package after the break *and* -overall and -strict with it. The gate
# reported success while checking almost nothing. check-coverage now refuses
# stray arguments so this cannot happen quietly again.
#
# internal/repotree is at 100 and is one function: it answers "is this
# directory a checkout of its own" for the guards that walk this tree, and
# both the answer and its negation decide whether a seal reads the right
# source. A floor below 100 on four lines would mean one of those four was
# never run.
COVER_PKGS := internal/repotree=100,internal/catalogue=96,internal/assets=91,internal/blob=97,internal/genstore=93,internal/queue=94,internal/webapi=92,internal/awssig=100,internal/s3=97,internal/workload=98,internal/transfer=95,internal/sshx=100,internal/provider/runpod=98,internal/httpx=98,internal/exposure=100,internal/reconcile=96,internal/node=100,internal/report=100,internal/loopback=100,internal/app=81,internal/cli=88,internal/provider=100,internal/keys=85,internal/hostkey=93,internal/statestore=88,internal/tunnel=94,internal/tunnel/sshdial=97,internal/provider/local=98,internal/dockerengine=100,internal/provision=92,internal/config=100,internal/session=96,internal/provider/providertest=98,internal/gpucheck=100,internal/nodemem=100,internal/sdserver=99,internal/tts=100,internal/logx=100
# cmd/reap-labs joins the other cmd mains here for their reason and one of its
# own: every statement in it is a Docker call or a line of output, the decisions
# it could get wrong live in test/integration/harness/labdaemon, and a unit test
# for it would have to be given a daemon to delete things on -- which is a lab,
# gated twice, and not the `go test ./...` the gate runs.
COVER_EXCLUDE := internal/sshx/sshexec,cmd/ember,cmd/emberwl,cmd/embernode,cmd/check-coverage,cmd/reap-labs,internal/dockerengine/dockerclient
# internal/assets is below the rest because the paths it cannot reach are
# filesystem failures with no portable way to provoke: a rename that fails
# after the digest matched, a chmod that fails after the rename, a close that
# fails after a successful write. The two that could be reached -- a truncated
# response and a read-only store -- are tested; the rest would need a fake
# filesystem, and an interface introduced to raise a coverage number is an
# interface the production code did not need.
# internal/genstore is at 93 against a measured 94.6 for internal/assets'
# reason, one layer down: what it cannot reach are row-scan failures and
# rows.Err() returns, which need a database that answers a query and then lies
# about a column type. Every path that *decides* anything is covered, and so is
# every error return reachable by closing the database under it -- which is not
# a contrivance, since `ember serve` shuts down by closing what it owns while a
# run may still be finishing. internal/blob is at 97 and the one thing it
# cannot reach is an os.OpenRoot that fails after the MkdirAll beside it
# succeeded, which needs a race.
#
# internal/catalogue is at 96 against a measured 96.1, and what it cannot
# reach is internal/assets' list one more time: a write, an fsync and a close
# that fail on a file this process has just created, plus a read of a day file
# that exists and cannot be opened. Every decision is covered -- what counts
# as capacity, what a level nobody has seen means, which day file a sample
# lands in, which rows a period includes, and the refusal of a batch with one
# unplaceable row in it. Those are the ones that would corrupt a series
# nothing can rebuild.
#
# internal/queue is at 94 against a measured 95.5. What it cannot reach is the
# one line that hands a resolved plan to provision.WithNode, which needs a real
# node and is what the serve lab exercises. Everything that *decides* anything
# -- which request a rental may also serve, whether a failure stops it, what a
# store that broke does to a generation already paid for -- is covered against
# fakes, which is the point: the expensive bugs here are in the loop rather
# than in the provisioning it calls.
#
# internal/webapi is at 92 against a measured 93.5. Its handlers are all
# httptest, including the listener -- loopback.Listen needs no daemon, so
# Serve is tested for real on a port the test picked, and the "nothing answers
# off this machine" assertion probes every non-loopback address this host has.
# What is left is write failures on a response whose status has already been
# sent, which need a ResponseWriter that fails mid-encode.
#
# internal/sdserver is at 99 and not 100 for internal/assets' reason: what it
# cannot reach are the two filesystem failures in WriteImages, a write that
# fails after the file was created and a close that fails after a successful
# write. Both need a fake filesystem to provoke, and an interface introduced to
# raise a coverage number is an interface the production code did not need.
# internal/hostkey went from 90 to 93 when #120 deleted the pin store, and the
# raise buys nothing against a deleted test -- which is worth writing down,
# because that is what it looks like it is for. Measured: disabling
# TestPinned_AcceptsTheKeyItHoldsAndRefusesEveryOther alone takes the package
# to 75.6%, so the package is small enough that any real test deletion blows
# past 90 as well.
#
# THAT FIGURE READ 68.9% HERE FIRST, and how it got wrong is the more useful
# half, in the one comment whose subject is that a stated measurement is worth
# its exit code. The first pass renamed a second test out of the run and did
# not put it back before renaming this one, so 68.9% is TWO tests disabled --
# a patch left in place from the previous step, which is the
# reinstatement-harness trap this file records, reached inside a measurement
# rather than inside a defect. Re-measured with t.Skip and under
# -covermode=set, count and atomic alike: 75.6%, three times. The conclusion
# never moved, because 75.6 < 90 < 93 either way.
#
# What the raise buys is that the floor stops describing a package that no
# longer exists. 90 was slack sized for saveLocked's file
# operations -- a CreateTemp, a Chmod, a Write, a Close and a Rename that fail
# on a file this process just made, internal/assets' list -- and with those
# gone the only uncovered statements in the whole package are Mint's three
# crypto error returns. So 93 against a measured 93.3 is the measurement
# rather than a margin, which is the same thing internal/s3=97 is.
#
# Overall across the gated set. Below 100 only because internal/keys and
# internal/hostkey carry the documented stdlib-error gaps above.
COVER_OVERALL := 94

# LAB_IMAGE is the local tag for the node image. The published one is built
# from the same node.Dockerfile by .github/workflows/node-image.yml, so the
# labs and a rented pod run the same bytes.
LAB_IMAGE := ember:lab

# Where a lab target's output is kept, so the `--- SKIP` gate can read it back.
#
# Keyed to the WHOLE of this checkout's path, because it used to be four fixed
# paths under /tmp and two worktrees of this repository on one machine share
# those. `tee` truncates the file as the second run starts -- which is before
# `labdaemon.Acquire` has taken the daemon lock, so the lock cannot serialise
# it -- and the first run's `grep -- '--- SKIP'` then greps a log belonging to
# somebody else, over a hole where its own output was. What it is asserting is
# that no lab reported success for something that never ran. Same shape as
# AssertNoStray reading a daemon another run had emptied, and the same reason
# the lab lock exists: several checkouts on one machine is now the normal way
# this is developed.
#
# `$(notdir $(CURDIR))` was the first attempt at that and keyed it to a string
# that is `ember` for every clone of this repository. Measured: `~/github/ember`
# and two copies under different parents all printed `/tmp/ember-labs-ember`,
# so the collision the paragraph above describes survived in full for the name
# `git clone` gives by default -- the comment claimed the checkout and the code
# named its last component (#112). A sentence in a comment asserting a property
# is worth exactly what its exit code is, so
# `TestLabLogDirIsKeyedToTheWholeCheckoutPath` asks make for this value from two
# directories of the same name and fails when they agree.
#
# A digest and not `$(subst /,-,$(CURDIR))`: the substitution is not injective
# over a `-`, and `-` is in the directory names this is actually developed in --
# `/w/ember-wt/ember` and `/w/ember/wt/ember` both become `-w-ember-wt-ember`,
# which is this defect again one character further down. A digest also cannot
# contain a space, which the basename could: `LAB_CHECK`'s
# `grep -q -- '--- SKIP' $(1)` is unquoted, so a space in the path makes the
# gate read two filenames and the one that explains it is neither. Nothing
# readable is lost that `make print-LAB_LOG_DIR` does not answer, which is how
# README.md tells a developer to find their log.
#
# Guarded, and recursive (=) so the guard fires where it matters: under
# .SHELLFLAGS' pipefail a missing sha256sum makes that pipeline produce
# NOTHING, and an empty key is one fixed path shared by every checkout again --
# the defect rather than an error. `=` keeps the refusal inside the targets that
# write a log and inside `print-LAB_LOG_DIR`; `make build` has no business
# failing over a lab log path.
LAB_LOG_KEY := $(shell printf %s '$(CURDIR)' | sha256sum | cut -c1-12)
LAB_LOG_DIR = /tmp/ember-labs-$(if $(LAB_LOG_KEY),$(LAB_LOG_KEY),$(error could not derive a lab log directory for $(CURDIR): sha256sum printed nothing, and an empty key is one log path shared by every checkout))

# One name per log, not one per call site. Each of these was spelled twice --
# once in the `tee` and once in the grep that reads it back -- which is the
# drift the "nodes" subdirectory literal already cost this repository once.
#
# Recursive (=), not simple (:=), for the two that carry the architecture, so
# the log follows a CUDA_ARCH set on the command line the way SD_CUDA_IMAGE
# does.
IMAGE_LABS_LOG = $(LAB_LOG_DIR)/image-labs-sm$(CUDA_ARCH).log
PUBLISH_LABS_LOG = $(LAB_LOG_DIR)/publish-labs-sm$(CUDA_ARCH).log
PREVIEW_LAB_LOG := $(LAB_LOG_DIR)/preview-lab.log
SERVE_LAB_LOG := $(LAB_LOG_DIR)/serve-lab.log
TTS_LAB_LOG := $(LAB_LOG_DIR)/tts-lab.log

# The tail of every lab target, in the SAME shell line as the `go test` whose
# log it reads. $(1) is that log.
#
# The spelling is the fix. A recipe line that fails stops the target, so the
# grep as a line of its own would never run once the pipeline above it could
# fail -- and "it failed **and** it skipped" is two problems rather than one:
# a suite that reported a failure *and* reported success for something that
# never ran has lost more than the failure says. So the status of the pipeline
# is captured first, the log is grepped either way, and whichever went wrong
# exits non-zero. The log is still written live by `tee`, pass or fail,
# because a grep cannot read a log that only exists when the test succeeded --
# that would trade one blind spot for another.
#
# A grep that could not READ the log is its own failure and not a clean run.
# `grep -q` answers 1 for "no such line" and 2 for "could not look", and
# treating the second as the first is how this whole entry happened: a check
# that reports success for something it never did. The lesson is the check's
# own, so it applies to itself.
LAB_CHECK = rc=$$?; grep -q -- '--- SKIP' $(1); g=$$?; if [ $$g -eq 0 ]; then echo "a lab skipped itself"; rc=1; elif [ $$g -gt 1 ]; then echo "could not read $(1), so the --- SKIP check did not run"; rc=1; fi; exit $$rc

# The head of every lab target: a -run expression that selects nothing is not
# a pass. $(1) is the expression, $(2) the fewest tests it must select.
#
# `go test -run` over an expression matching fewer tests than intended prints
# at most "warning: no tests to run" and **exits 0**, so the third gate in a
# row that could not fail was this one: `make test-image-labs-publish
# PUBLISH_LABS=TestWorkloadImage_ThisNameDoesNotExist` exited 0 having run
# none of the five assertions that stop a bad workload image reaching a
# registry, and `publish-sd-cuda` went on to `docker tag` and `docker push`.
# The realistic case is worse because it is silent: one rename drops the list
# from five names to four and nothing is printed at all.
#
# `pipefail` fixed "a lab failed and make said 0" and cannot touch "a lab was
# never selected and make said 0" -- the same consequence with no symptom. So
# the count is asserted, and it is asserted FIRST, before an image inspect or
# a daemon lock, because `-list` needs no Docker, no image and no lock: it
# compiles the test binary and prints the names. Push what is local and free
# before what is remote and slow.
#
# `|| true` on the grep because `grep -c` exits 1 on a count of zero, which
# under .SHELLFLAGS' pipefail would fail the line before the message that
# explains it -- and zero is the answer this exists to report.
LAB_SELECT = sel=$$($(GO) test -tags='$(LAB_TAGS)' -list $(1) ./test/integration/ 2>/dev/null | grep -c '^Test' || true); \
	if [ "$$sel" -lt "$(2)" ]; then \
		echo "-run $(1) selects $$sel test(s) in ./test/integration/, and this gate names $(2)."; \
		echo "a lab renamed out of the expression is a lab the gate does not run, and go test -run exits 0 either way."; \
		exit 1; \
	fi; \
	echo "selection: $$sel test(s) match $(1)"

.PHONY: all build web web-check web-test test-serve-lab test test-race cover cover-check vet fmt lint docker-build test-integration reap-labs test-image-labs test-image-labs-publish test-preview-lab test-tts-lab publish-sd-cuda workload-sd workload-sd-cuda workload-sd-cuda-local workload-tts clean print-%

all: fmt vet test build

build:
	$(GO) build -ldflags '$(GO_LDFLAGS)' -o $(BIN)/ember ./cmd/ember

test:
	$(GO) test -race -count=1 ./...

cover:
	$(GO) test -race -count=1 -coverpkg=./internal/...,./cmd/... -coverprofile=$(COVERPROFILE) ./...
	$(GO) tool cover -func=$(COVERPROFILE) | tail -1

cover-check: cover
	$(GO) run ./cmd/check-coverage \
		-profile=$(COVERPROFILE) \
		-module=github.com/Legion112/ember \
		-packages=$(COVER_PKGS) \
		-exclude=$(COVER_EXCLUDE) \
		-overall=$(COVER_OVERALL) \
		-strict

# Every build tag that gates Go source in this tree, in one place.
#
# It used to be spelled three times -- here and in two ci.yml steps -- and
# .golangci.yml held a fourth list that named only `integration`, so the
# image labs were vetted three times over and linted nowhere. The
# workflows ask make for the tags now, the way workload-image.yml already
# asks it for the image tag, and cmd/check-coverage's
# TestEveryBuildTagReachesTheGates fails when this list or .golangci.yml's
# does not name a tag the tree actually uses -- which is the only part of
# this that cannot drift back.
LAB_TAGS := integration imagelab

# vet runs three times. The default invocation never looks at files behind a
# build tag, so without the tagged lines the entire integration lab compiles
# only on the machine that happens to run it.
# The labs exist now, so this is unconditional. The earlier spelling skipped
# the tagged vet when it found no lab sources, which was honest while there
# were none and is a silent pass the moment someone deletes or moves them.
vet:
	$(GO) vet ./...
	# `integration` alone, literally, because this is the tree as
	# `make test-integration` builds it: the full set below would hide a lab
	# that only compiles when imagelab is also set.
	$(GO) vet -tags=integration ./test/...
	# And behind every tag. Nothing else compiles the image labs: the ordinary
	# CI run exercises only -tags=integration, and the workflow that uses them
	# is dispatch-only, so a file that stopped compiling would not be noticed
	# until a publish -- after a twenty-minute build, which is exactly how
	# `ember:lab is missing` reached a publish once.
	$(GO) vet -tags='$(LAB_TAGS)' ./test/...

# Every directory in this checkout that holds Go source, in one place, because
# gofmt is pointed at these and never at `.`.
#
# `.` is the whole DIRECTORY, and an agent worktree is a second checkout of
# this repository at `.claude/worktrees/<name>/` -- inside the one it was cut
# from. So `gofmt -l .` reported another developer's files as this commit's,
# and `make fmt`, which is the same walk with `-w`, REWROTE them: the #143
# defect at its fourth site and the only one of the four that writes.
# Measured: with one nested checkout holding an unformatted file,
# `gofmt -l .` named it and nothing else did.
#
# A list and not a prune, because gofmt has no exclusion flag and no notion of
# a module -- so the derivation has to happen somewhere that can do it, and a
# list checked against nothing is what LAB_TAGS and PUBLISH_LABS both exist
# not to be. cmd/check-coverage's TestGofmtReachesEveryGoFileInThisCheckout is
# the check: it walks this tree, skips nested checkouts through
# internal/repotree, and fails both on a .go file under none of these paths
# and on a path here that holds no .go file.
GOFMT_PATHS := cmd internal test

# gofmt directly, and not `go fmt ./...`.
#
# `go fmt` resolves packages through the build context, so it never sees a
# file behind a build tag -- the integration labs, every one of them. CI
# checks with gofmt over the same paths, which does, so `make fmt` used to
# leave exactly the files CI would fail on. Same gap the vet target above
# documents, found the same way: by CI disagreeing with a laptop.
fmt:
	gofmt -l -w $(GOFMT_PATHS)

# A missing linter is a hard failure, not a skip. The earlier spelling put the
# guard and the run on separate recipe lines, where `exit 0` only leaves that
# line's subshell: make ran the linter regardless, and when it really was
# absent the target reported success without linting anything. That is how ten
# findings reached CI once. The version here must match .github/workflows/ci.yml:
# a linter that disagrees with CI makes `make lint` theatre.
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { 		echo "golangci-lint is not installed. Install it with:"; 		echo "  $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0"; 		exit 1; 	}; 	golangci-lint run

docker-build:
	docker build -t $(LAB_IMAGE) -f node.Dockerfile .

# The web UI: Rust compiled to WebAssembly.
#
# NO trunk AND NO wasm-pack. `cargo build --target wasm32-unknown-unknown`
# followed by `wasm-bindgen --target web` is all either of those does here,
# and the CLI is a tool the operator already has to install for any of this to
# work -- so the choice is one tool or two.
#
# THE TWO VERSIONS MUST MATCH. wasm-bindgen generates glue that its own CLI
# post-processes, and a mismatch between the crate and the binary is a hard
# error with a message about a version nobody chose. Cargo.toml pins the crate
# exactly; this target checks the binary agrees and says which to change,
# rather than letting the build fail forty seconds later with somebody else's
# wording. Same rule and same shape as require-arch.
#
# The output goes into internal/webapi/dist, beside a committed placeholder
# index.html that exists so //go:embed -- which fails at COMPILE time on a
# pattern matching nothing -- does not make `go build ./...` depend on a Rust
# toolchain.
WASM_BINDGEN_VERSION := $(shell sed -n 's/^wasm-bindgen = "=\(.*\)"$$/\1/p' web/Cargo.toml)
WEB_DIST := internal/webapi/dist

web: web-check
	cargo build --manifest-path web/Cargo.toml --release --target wasm32-unknown-unknown
	wasm-bindgen --target web --no-typescript \
		--out-dir $(WEB_DIST) --out-name ember \
		web/target/wasm32-unknown-unknown/release/ember_web.wasm
	cp web/index.html web/style.css $(WEB_DIST)/
	@echo
	@echo "built $(WEB_DIST):"
	@ls -la $(WEB_DIST) | awk 'NR>1 {printf "  %8d  %s\n", $$5, $$9}'
	@echo
	@echo 'ember serve will now serve it, and go build embeds it.'

# The checks `make web` refuses to build without, each naming its own fix.
web-check:
	@command -v cargo >/dev/null 2>&1 || { \
		echo "cargo is not installed. ember's web UI is Rust compiled to WebAssembly;"; \
		echo "see https://rustup.rs. Everything else in this repository is Go and"; \
		echo "needs none of it -- `make build` and `make test` work without cargo."; \
		exit 1; }
	@rustup target list --installed 2>/dev/null | grep -qx wasm32-unknown-unknown || { \
		echo "the wasm32-unknown-unknown target is not installed. Add it with:"; \
		echo "  rustup target add wasm32-unknown-unknown"; \
		exit 1; }
	@command -v wasm-bindgen >/dev/null 2>&1 || { \
		echo "wasm-bindgen is not installed. Install the version web/Cargo.toml pins:"; \
		echo "  cargo install -f wasm-bindgen-cli --version $(WASM_BINDGEN_VERSION)"; \
		exit 1; }
	@have=$$(wasm-bindgen --version | awk '{print $$2}'); \
	if [ "$$have" != "$(WASM_BINDGEN_VERSION)" ]; then \
		echo "wasm-bindgen $$have is installed and web/Cargo.toml pins $(WASM_BINDGEN_VERSION)."; \
		echo "The generated glue and the CLI that post-processes it have to be the"; \
		echo "same version. Either install the pinned one:"; \
		echo "  cargo install -f wasm-bindgen-cli --version $(WASM_BINDGEN_VERSION)"; \
		echo "or change the pin in web/Cargo.toml to $$have and commit Cargo.lock."; \
		exit 1; \
	fi

# The Rust checks, which are to web/ what vet, lint and test are to the Go.
web-test:
	cargo fmt --manifest-path web/Cargo.toml --check
	cargo clippy --manifest-path web/Cargo.toml --target wasm32-unknown-unknown -- -D warnings
	cargo test --manifest-path web/Cargo.toml

# A workload image is the node image plus whatever it was rented to run.
#
# The CUDA image carries no model: weights go on a store, pushed once with
# `ember store push` and named by `assets:` with a digest. MODEL is still
# needed by the CPU variant, which exists to prove a pipeline locally for
# nothing and has no store to mount.
SD_IMAGE := ember:sd

# The speech image.
#
# No per-architecture variant and no CUDA_ARCH, which is a measurement rather
# than an omission: piper's released binary links onnxruntime's CPU execution
# provider only, so there is no kernel for a card to be wrong about and nothing
# for `images:` to key on. Measured on this machine -- en_US-lessac-medium
# synthesises at a real-time factor of about 0.037, so 4.6 s of audio costs
# 170 ms of one CPU -- which is why the whole driver is a CPU story. See
# internal/tts.
#
# It carries its voice, which is the opposite of the rule for a diffusion
# checkpoint and the same choice SD_IMAGE's Dockerfile makes for sd15: 60 MiB
# against a 2.2 GB checkpoint, and what it buys is a local run that needs no
# store, no mount and no sync.
TTS_IMAGE := ember:tts

# One image per architecture, so the tag carries it.
#
# Recursive (=), not simple (:=): the tag has to follow CUDA_ARCH when it is
# set on the command line. A single ember:sd-cuda with no architecture in the
# name is how two local builds overwrite each other and the image lab asserts
# against whichever ran last.
#
# CUDA_ARCH is singular and has no default here or in the Dockerfile. A
# default is how a path that forgot to pass it produces a wrong-architecture
# image that looks correct.
CUDA_ARCH ?=
SD_CUDA_IMAGE = ember:sd-cuda-sm$(CUDA_ARCH)
MODEL ?=

# require-arch fails with the command that fixes it rather than twenty minutes
# into nvcc, the same shape as require-model below.
#
# The set comes from `ember archs` rather than being listed here, because a
# copy of it in a Makefile is a copy that is wrong the day a card is added.
define require-arch
	@if [ -z "$(CUDA_ARCH)" ]; then \
		echo "CUDA_ARCH is not set. One image per architecture, so the build has to be told which:"; \
		echo "  make $@ CUDA_ARCH=89      # 4090 / L40S"; \
		echo; \
		echo "architectures ember publishes an image for:"; \
		$(GO) run ./cmd/ember archs --cards 2>/dev/null | sed 's/^/  /'; \
		exit 1; \
	fi
endef

# require-model fails with the command that fixes it rather than a build error
# three minutes in.
define require-model
	@if [ -z "$(MODEL)" ]; then \
		echo "MODEL is not set. Point it at a GGUF checkpoint, for example:"; \
		echo "  make $@ MODEL=/path/to/v1-5-pruned_bf16.gguf"; \
		exit 1; \
	fi
	@if [ ! -f "$(MODEL)" ]; then echo "no such model: $(MODEL)"; exit 1; fi
endef

# The CPU variant, for proving a pipeline locally for nothing. It uses an
# upstream prebuilt binary, so it builds in seconds.
workload-sd:
	$(require-model)
	cp "$(MODEL)" .model.gguf
	docker build -t $(SD_IMAGE) -f workload-sd.Dockerfile \
		--build-arg NODE_IMAGE=$(LAB_IMAGE) --build-arg MODEL_PATH=.model.gguf . ; \
	rc=$$?; rm -f .model.gguf; exit $$rc

# The speech image. Seconds: a prebuilt binary and two downloads, both pinned
# by digest, and no model to pass in because the voice is one of them.
workload-tts:
	docker build -t $(TTS_IMAGE) -f workload-tts.Dockerfile \
		--build-arg NODE_IMAGE=$(LAB_IMAGE) .

# The CUDA variant, for a rented GPU. Upstream publishes no prebuilt Linux
# CUDA binary, so this compiles stable-diffusion.cpp against the toolkit --
# minutes, not seconds.
#
# CUDA_ARCH is required and names exactly one architecture. The Dockerfile
# verifies with cuobjdump that the binary carries that one and no PTX, so a
# wrong image fails the build rather than being published and noticed later.
workload-sd-cuda:
	$(require-arch)
	docker build -t $(SD_CUDA_IMAGE) -f workload-sd-cuda.Dockerfile \
		--build-arg CUDA_ARCH="$(CUDA_ARCH)" .

# The same image, built for the card in *this* machine.
#
# A thin wrapper now. It still reads the architecture off the card rather than
# taking it as an argument, because a number typed by hand is a number that is
# wrong after a hardware change -- and wrong here does not fail, it falls back
# to the CPU backend and looks like a slow success.
#
# What it produces is an ordinary per-architecture image under the ordinary
# per-architecture tag. The separate ember:sd-cuda-local tag is gone: it
# existed because the published set excluded this machine's card, sm_75 is in
# that set now, and two names for one artifact is how they drift.
workload-sd-cuda-local:
	@command -v nvidia-smi >/dev/null 2>&1 || { \
		echo "nvidia-smi not found: this target builds for the GPU in this machine"; exit 1; }
	@arch=$$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader | head -1 | tr -d '. '); \
	test -n "$$arch" || { echo "could not read a compute capability from nvidia-smi"; exit 1; }; \
	echo "building for sm_$$arch ($$(nvidia-smi --query-gpu=name --format=csv,noheader | head -1))"; \
	$(MAKE) workload-sd-cuda CUDA_ARCH=$$arch

# Assertions about the published workload images, behind their own tag.
#
# Not part of test-integration because they need images that `make
# docker-build` does not produce and the CUDA one takes forty minutes to
# compile. The workflow that builds an image runs these against it before
# pushing, which is a better place than CI: a bad image never ships.
#
# They are not skipped -- they are not compiled unless the tag says the images
# are here. A lab that skips itself reports success for something that never
# ran.
test-image-labs:
	$(require-arch)
	@$(call LAB_SELECT,'TestWorkloadImage_',1)
	@docker image inspect $(SD_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd MODEL=/path/to/a.gguf"; exit 1; }
	@docker image inspect $(SD_CUDA_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_CUDA_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd-cuda CUDA_ARCH=$(CUDA_ARCH)"; exit 1; }
	@mkdir -p $(LAB_LOG_DIR)
	EMBER_SD_CUDA_IMAGE=$(SD_CUDA_IMAGE) EMBER_CUDA_ARCH=$(CUDA_ARCH) \
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v ./test/integration/... \
		-run 'TestWorkloadImage_' | tee $(IMAGE_LABS_LOG); \
	$(call LAB_CHECK,$(IMAGE_LABS_LOG))

# The served path against a real sd-server, including the progress stream.
#
# Needs only ember:sd -- the CPU build -- because sd-server there starts
# without a GPU, which the CUDA build cannot: it wants libcuda.so.1 to do
# anything at all. The two are pinned to the same upstream revision, and
# TestWorkloadImages_PinTheSameUpstreamRevision is what keeps that true, so
# this says something about the program that goes to RunPod rather than about
# a different one.
#
# Not part of test-image-labs-publish: a publish of the CUDA image should not
# require an unrelated image to exist, which is the same reason the video lab
# is absent from it.
#
# The whole thing runs in about twenty seconds on a laptop. Set
# EMBER_CAPTURE_DIR to have it write the server's own responses out as
# fixtures; see internal/sdserver/testdata/README.md for what promoting one
# means.
test-preview-lab:
	@$(call LAB_SELECT,'TestPreview_',1)
	@docker image inspect $(SD_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd MODEL=/path/to/a.gguf"; exit 1; }
	@mkdir -p $(LAB_LOG_DIR)
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v \
		-timeout 25m ./test/integration/... \
		-run 'TestPreview_' | tee $(PREVIEW_LAB_LOG); \
	$(call LAB_CHECK,$(PREVIEW_LAB_LOG))

# Text to speech end to end on the local provider, for nothing.
#
# Needs only ember:tts, which is built from ember:lab and takes seconds. There
# is no CUDA variant to wait for, because piper is a CPU story -- so unlike
# every other image lab here this one is cheap enough that the only reason it
# is behind a tag at all is that `make docker-build` does not produce the
# image.
#
# Not part of test-image-labs-publish: a publish of the CUDA image must not
# require an unrelated image to exist, which is the same sentence the video
# lab is absent by.
test-tts-lab:
	@$(call LAB_SELECT,'TestTTS_|TestWorkloadImage_Speech|TestWorkloadImage_Speaks',4)
	@docker image inspect $(TTS_IMAGE) >/dev/null 2>&1 || { \
		echo "$(TTS_IMAGE) is missing. Build it with:"; \
		echo "  make workload-tts"; exit 1; }
	@docker image inspect $(LAB_IMAGE) >/dev/null 2>&1 || { \
		echo "$(LAB_IMAGE) is missing. Build it with:"; \
		echo "  make docker-build"; exit 1; }
	@mkdir -p $(LAB_LOG_DIR)
	EMBER_TTS_IMAGE=$(TTS_IMAGE) \
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v \
		-timeout 20m ./test/integration/... \
		-run 'TestTTS_|TestWorkloadImage_Speech|TestWorkloadImage_Speaks' | tee $(TTS_LAB_LOG); \
	$(call LAB_CHECK,$(TTS_LAB_LOG))

# `ember serve` end to end: one rental drained by a queue, and the real UI in
# a real browser.
#
# Needs ember:sd -- the CPU build -- because sd-server there starts without a
# GPU, which the CUDA build cannot: it wants libcuda.so.1 to do anything at
# all. The two are pinned to the same upstream revision, so this says
# something about the program that goes to RunPod rather than about a
# different one.
#
# Also needs the web UI built, because the browser lab drives the real page.
# A missing one is a hard failure naming the command, never a skip.
#
# The serve labs that need no workload image are in the ordinary
# `make test-integration` run instead -- they are the loopback, cross-origin
# and teardown assertions, and they are the ones worth having on every pull
# request.
test-serve-lab: web
	@$(call LAB_SELECT,'TestServe_',1)
	@docker image inspect $(SD_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd MODEL=/path/to/a.gguf"; exit 1; }
	@docker image inspect $(LAB_IMAGE) >/dev/null 2>&1 || { \
		echo "$(LAB_IMAGE) is missing. Build it with:"; \
		echo "  make docker-build"; exit 1; }
	@mkdir -p $(LAB_LOG_DIR)
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v \
		-timeout 30m ./test/integration/... \
		-run 'TestServe_' | tee $(SERVE_LAB_LOG); \
	$(call LAB_CHECK,$(SERVE_LAB_LOG))

# The labs a publish must pass, named once so CI and a laptop run the same set.
#
# This is the parity that makes two publish paths safe. The workflow used to
# carry its own `-run` expression, which drifted: a lab added to catch a
# published image that lacked `embernode fetch` was not in it, so the one thing
# it existed to guard was the one thing it did not run against.
#
# The video lab is deliberately absent. It needs ember:sd, the CPU build, and
# a publish of the CUDA image should not require an unrelated image to exist.
# TestWorkloadImages_PinTheSameUpstreamRevision is what keeps the two honest.
#
# LinksNothingItDoesNotShip belongs here for the reason the paragraph above
# exists. The image's final stage is nvidia/cuda:*-base plus an explicit COPY
# of the three CUDA libraries the binaries link, rather than *-runtime, which
# carries every library NVIDIA publishes -- so a dependency the binary gained,
# or one a CUDA bump renamed, is a library the loader cannot resolve. That is
# `error while loading shared libraries` after a pod was rented, the image
# pulled and the weights fetched, all of it billed, and the only place it can
# be caught for free is before the push. It needs no GPU, no network and no
# weights, so there is no reason for it to be outside the gate.
#
# ShipsTheServerItAlreadyCompiles is here by that same sentence, and was
# outside the gate until the list was checked against the tree: it is a
# Dockerfile text assertion about the image being published, costing
# milliseconds and needing nothing at all. Being free and about the published
# artifact is the whole of the argument.
#
# **A list of names and not an alternation**, because the gate has to be able
# to count what it selected: `$(words)` gives the number this expression
# names, and the `-run` regex is derived from the same list rather than
# written beside it. Three comments in this tree cite PUBLISH_LABS as the
# precedent for not maintaining a list beside the thing it mirrors, while it
# was exactly such a list, checked against nothing (#115).
# cmd/check-coverage's TestPublishGateNamesEveryWorkloadImageLab checks it
# against the tree in both directions; LAB_SELECT checks what it actually
# selected, which is the half a Go test cannot see when the variable is
# overridden on the command line.
PUBLISH_LABS := \
	TestWorkloadImage_ListensOnNothingButSSH \
	TestWorkloadImage_CarriesNoWeights \
	TestWorkloadImage_UnderstandsTheFetchCommand \
	TestWorkloadImage_CarriesOneArchitectureAndNoPTX \
	TestWorkloadImage_LinksNothingItDoesNotShip \
	TestWorkloadImage_ShipsTheServerItAlreadyCompiles

# One anchored alternation, derived. Anchored because an unanchored name is a
# substring match: `-run TestWorkloadImage_CarriesNoWeights` would also select
# a `..._CarriesNoWeightsAnyMore` added later, so the count could be satisfied
# by a test nobody named. Recursive (=) so a PUBLISH_LABS set on the command
# line is still the thing the expression is built from.
labs_space := $(subst x,,x x)
PUBLISH_LABS_RUN = '^($(subst $(labs_space),|,$(strip $(PUBLISH_LABS))))$$'

# The subset of the image labs that needs only the CUDA image and ember:lab.
test-image-labs-publish:
	$(require-arch)
	@$(call LAB_SELECT,$(PUBLISH_LABS_RUN),$(words $(PUBLISH_LABS)))
	@docker image inspect $(SD_CUDA_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_CUDA_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd-cuda CUDA_ARCH=$(CUDA_ARCH)"; exit 1; }
	@docker image inspect $(LAB_IMAGE) >/dev/null 2>&1 || { \
		echo "$(LAB_IMAGE) is missing. Build it with:"; \
		echo "  make docker-build"; exit 1; }
	@mkdir -p $(LAB_LOG_DIR)
	EMBER_SD_CUDA_IMAGE=$(SD_CUDA_IMAGE) EMBER_CUDA_ARCH=$(CUDA_ARCH) \
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v ./test/integration/... \
		-run $(PUBLISH_LABS_RUN) | tee $(PUBLISH_LABS_LOG); \
	$(call LAB_CHECK,$(PUBLISH_LABS_LOG))

# Publishing the CUDA image from this machine, as an equal of the CI workflow.
#
# The workflow's header used to give two reasons for CI-only publishing: no
# registry credential on a laptop, and a home upload link too slow for several
# gigabytes. The first is the repository owner's call and they have made it.
# The second turned out to be wrong by a wide margin -- the runtime base is
# pinned by digest, so a rebuild sends only the layers that changed: 1.66 GiB
# of image, 29 seconds of push, because nine of seventeen layers were already
# there.
#
# What does matter is that the two paths stay honest about the same things, so
# this runs the same labs the workflow runs, and refuses to push if they fail.
# A local publish that skipped them would be strictly worse than no local
# publish at all -- the image that lacked `embernode fetch` shipped exactly
# that way.
#
# Authenticate first, and never on a command line:
#
#   docker login ghcr.io -u <user>
#
# Usage:  make publish-sd-cuda TAG=sd-cuda
GHCR_OWNER ?= legion112
GHCR_IMAGE := ghcr.io/$(GHCR_OWNER)/ember-sd
TAG ?= sd-cuda

# Publishing one architecture's image from this machine, as an equal of one
# matrix job. Both still run the labs named once as PUBLISH_LABS above.
#
# Usage:  make publish-sd-cuda CUDA_ARCH=75 TAG=sd-cuda
#
# The reference is rebuilt from GHCR_IMAGE and the digest rather than printed
# straight from RepoDigests, because that is a list whose order nothing
# documents -- an image carrying two tags can report either one first, and a
# config naming the wrong repository is a pull that fails at create.
publish-sd-cuda: workload-sd-cuda docker-build test-image-labs-publish
	docker tag $(SD_CUDA_IMAGE) $(GHCR_IMAGE):$(TAG)-sm$(CUDA_ARCH)
	docker push $(GHCR_IMAGE):$(TAG)-sm$(CUDA_ARCH)
	@echo
	@echo "published. put this under images: in ember.yaml:"
	@ref=$$(docker image inspect --format '{{index .RepoDigests 0}}' $(GHCR_IMAGE):$(TAG)-sm$(CUDA_ARCH)); \
	echo "  sm_$(CUDA_ARCH): $(GHCR_IMAGE):$(TAG)-sm$(CUDA_ARCH)@sha256:$${ref##*@sha256:}"

# print-<VAR> so a workflow can ask make for a name instead of spelling it a
# second time. No file is produced, so it always runs.
print-%:
	@echo $($*)

# The labs refuse to skip themselves: a missing image is a hard failure with
# the command to fix it, never a t.Skip. A silently skipped lab is worse than
# a red build because it reports success for something that never ran.
# The labs are gated twice: the build tag keeps them out of `go test ./...`,
# and EMBER_INTEGRATION keeps them out of a tagged run by somebody who did not
# mean to start containers. Neither gate makes them skip -- a missing daemon or
# a missing image is a hard failure naming the command that fixes it.
test-integration: docker-build
	EMBER_INTEGRATION=1 $(GO) test -tags=integration -count=1 -timeout 20m -v ./test/integration/...

# Clear every ember-labelled object off the local daemon.
#
# A developer tool and not an assertion, which is why it is a target of its own
# and why nothing in CI calls it. The labs used to do this implicitly -- each
# one's cleanup force-removed every ember-labelled container, network and
# volume on the daemon, which is why two suites at once deleted each other's
# live nodes. They now leave alone whatever was there before the suite started,
# so a container from a run that was SIGKILLed yesterday survives, and this is
# how it goes. It refuses while a suite holds the lab lock.
reap-labs:
	$(GO) run ./cmd/reap-labs

clean:
	rm -rf $(BIN) $(COVERPROFILE)

# go.mod says what ember chose to depend on, and nothing was checking that it
# still did.
#
# WHAT WAS WRONG. `go build` and `go get` add a requirement without correcting
# its `// indirect` marker, so the marker is only ever as true as the last
# `go mod tidy` somebody remembered to run -- and nobody had for long enough
# that four modules stood recorded as if nobody asked for them: chromedp,
# coder/websocket, yaml.v3 and modernc.org/sqlite. Three of those four get a
# paragraph of argument each in AGENTS.md's dependency bullet, which is the
# document this file's direct requirements are supposed to agree with. They
# disagreed, silently, because a stale comment is not a build failure.
#
# WHY IT IS A GATE AND NOT A ONE-OFF TIDY. #33's TestOTelStaysIndirect reads
# that marker as evidence: an otel line losing `// indirect` means a file in
# this module imported an OpenTelemetry SDK, which is the decision that test
# exists to refuse. A seal resting on a field known to be stale four lines
# away is the shape AGENTS.md names for AssertNoStray -- a check that passes
# because what it reads has stopped meaning anything. Tidying once fixes today
# and drifts again at the next `go get`.
#
# IT LEAVES THE TIDIED FILES IN PLACE, deliberately: the diff it just computed
# IS the fix, so whoever runs this and reads the error has the correction
# applied rather than a command to go and run. The message says so, because a
# target that quietly rewrote two tracked files and did not mention it would
# be worse than one that never ran.
#
# NOT A GO TEST, which is where this repository's other seals of this kind
# live -- TestNoHandlerOutsideLogx, TestNoListenOutsideLoopback, and
# cmd/check-coverage's build-tag guard. Every one of those parses files that
# are already on disk. This one has to run `go mod tidy`, which resolves
# modules, and `go test ./...` must need no GPU, no Docker, no network and no
# credentials. A seal that bought the whole unit suite a network dependency
# would cost more than the drift it prevents, so the rule wins and this is a
# target that CI calls.
#
# THE MODULE CACHE IS WHY THE NETWORK IS NOT A WORRY. setup-go caches the
# module download cache keyed on go.sum, and a tidy of an already-tidy tree
# resolves everything out of it: `GOPROXY=off $(GO) mod tidy` succeeds here
# and changes nothing. The proxy is reached only when the answer was going to
# be a failure anyway -- a requirement that is missing or a package nothing
# records -- which is the case where resolving it is the entire point.
.PHONY: tidy-check
tidy-check:
	$(GO) mod tidy
	@git diff --exit-code go.mod go.sum || { \
		echo; \
		echo "go.mod/go.sum are not tidy. The diff above IS the fix, and it is"; \
		echo "already applied to your tree -- read it and commit it. AGENTS.md's"; \
		echo "dependency bullet is the list these direct requirements answer to."; \
		exit 1; \
	}
