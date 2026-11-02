# ember — see AGENTS.md for the rules this Makefile is the entry point to.
#
# Targets stay thin on purpose. Anything with logic in it belongs in a Go
# subcommand, not in a recipe: a Makefile is the one place in this repo where
# shell can still accumulate, and go-tun's verify-router target is what sixty
# lines of inlined ssh looks like a year later.

# Prefer the pinned toolchain when it is installed, so the Go that builds this
# cannot drift from what go.mod declares just because the system package
# updated. jira-mcp does the same.
GO ?= $(shell if [ -x "$(HOME)/sdk/go1.27.0/bin/go" ]; then echo "$(HOME)/sdk/go1.27.0/bin/go"; else echo go; fi)
export GOTOOLCHAIN ?= go1.27.0

BIN := bin
COVERPROFILE := coverage.out

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
COVER_PKGS := internal/assets=91,internal/blob=97,internal/genstore=93,internal/queue=94,internal/webapi=92,internal/awssig=100,internal/s3=97,internal/workload=98,internal/transfer=95,internal/sshx=100,internal/provider/runpod=98,internal/httpx=98,internal/exposure=100,internal/reconcile=96,internal/node=100,internal/report=100,internal/loopback=100,internal/app=81,internal/cli=88,internal/provider=100,internal/keys=85,internal/hostkey=90,internal/statestore=88,internal/tunnel=94,internal/tunnel/sshdial=97,internal/provider/local=98,internal/dockerengine=100,internal/provision=92,internal/config=100,internal/session=96,internal/provider/providertest=98,internal/gpucheck=100,internal/nodemem=100,internal/sdserver=99
COVER_EXCLUDE := internal/sshx/sshexec,cmd/ember,cmd/emberwl,cmd/embernode,cmd/check-coverage,internal/dockerengine/dockerclient
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
# Overall across the gated set. Below 100 only because internal/keys and
# internal/hostkey carry the documented stdlib-error gaps above.
COVER_OVERALL := 94

# LAB_IMAGE is the local tag for the node image. The published one is built
# from the same node.Dockerfile by .github/workflows/node-image.yml, so the
# labs and a rented pod run the same bytes.
LAB_IMAGE := ember:lab

.PHONY: all build web web-check web-test test-serve-lab test test-race cover cover-check vet fmt lint docker-build test-integration test-image-labs test-image-labs-publish test-preview-lab publish-sd-cuda workload-sd workload-sd-cuda workload-sd-cuda-local clean print-%

all: fmt vet test build

build:
	$(GO) build -o $(BIN)/ember ./cmd/ember

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
# image labs were vetted and linted nowhere for their whole life. The
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

# gofmt directly, and not `go fmt ./...`.
#
# `go fmt` resolves packages through the build context, so it never sees a
# file behind a build tag -- the integration labs, every one of them. CI
# checks with `gofmt -l .`, which walks the tree and does, so `make fmt` left
# exactly the files CI would fail on. Same gap the vet target above documents,
# found the same way: by CI disagreeing with a laptop.
fmt:
	gofmt -l -w .

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
	@docker image inspect $(SD_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd MODEL=/path/to/a.gguf"; exit 1; }
	@docker image inspect $(SD_CUDA_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_CUDA_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd-cuda CUDA_ARCH=$(CUDA_ARCH)"; exit 1; }
	EMBER_SD_CUDA_IMAGE=$(SD_CUDA_IMAGE) EMBER_CUDA_ARCH=$(CUDA_ARCH) \
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v ./test/integration/... \
		-run 'TestWorkloadImage_' | tee /tmp/ember-image-labs-sm$(CUDA_ARCH).log
	@if grep -q -- '--- SKIP' /tmp/ember-image-labs-sm$(CUDA_ARCH).log; then \
		echo "a lab skipped itself"; exit 1; fi

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
	@docker image inspect $(SD_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd MODEL=/path/to/a.gguf"; exit 1; }
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v \
		-timeout 25m ./test/integration/... \
		-run 'TestPreview_' | tee /tmp/ember-preview-lab.log
	@if grep -q -- '--- SKIP' /tmp/ember-preview-lab.log; then \
		echo "a lab skipped itself"; exit 1; fi

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
	@docker image inspect $(SD_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd MODEL=/path/to/a.gguf"; exit 1; }
	@docker image inspect $(LAB_IMAGE) >/dev/null 2>&1 || { \
		echo "$(LAB_IMAGE) is missing. Build it with:"; \
		echo "  make docker-build"; exit 1; }
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v \
		-timeout 30m ./test/integration/... \
		-run 'TestServe_' | tee /tmp/ember-serve-lab.log
	@if grep -q -- '--- SKIP' /tmp/ember-serve-lab.log; then \
		echo "a lab skipped itself"; exit 1; fi

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
PUBLISH_LABS := 'TestWorkloadImage_(ListensOnNothingButSSH|CarriesNoWeights|UnderstandsTheFetchCommand|CarriesOneArchitectureAndNoPTX)'

# The subset of the image labs that needs only the CUDA image and ember:lab.
test-image-labs-publish:
	$(require-arch)
	@docker image inspect $(SD_CUDA_IMAGE) >/dev/null 2>&1 || { \
		echo "$(SD_CUDA_IMAGE) is missing. Build it with:"; \
		echo "  make workload-sd-cuda CUDA_ARCH=$(CUDA_ARCH)"; exit 1; }
	@docker image inspect $(LAB_IMAGE) >/dev/null 2>&1 || { \
		echo "$(LAB_IMAGE) is missing. Build it with:"; \
		echo "  make docker-build"; exit 1; }
	EMBER_SD_CUDA_IMAGE=$(SD_CUDA_IMAGE) EMBER_CUDA_ARCH=$(CUDA_ARCH) \
	EMBER_INTEGRATION=1 $(GO) test -tags='integration imagelab' -count=1 -v ./test/integration/... \
		-run $(PUBLISH_LABS) | tee /tmp/ember-publish-labs-sm$(CUDA_ARCH).log
	@if grep -q -- '--- SKIP' /tmp/ember-publish-labs-sm$(CUDA_ARCH).log; then \
		echo "a lab skipped itself"; exit 1; fi

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

clean:
	rm -rf $(BIN) $(COVERPROFILE)
