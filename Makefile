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
# internal/cli is at 77 and is the honest exception: what is left uncovered
# there is the wiring that constructs a real Docker client and resolves a real
# home directory, which the labs exercise. When a *decision* lived there --
# when to retry an SSH dial, when to refuse one -- it was moved to
# internal/session, which is at 96 and tests against a real SSH server. That
# is the rule above being followed rather than the floor being lowered.
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
COVER_PKGS := internal/exposure=100,internal/reconcile=96,internal/node=100,internal/loopback=100,internal/cli=83,internal/provider=100,internal/keys=85,internal/hostkey=90,internal/statestore=88,internal/tunnel=94,internal/tunnel/sshdial=97,internal/provider/local=98,internal/dockerengine=100,internal/provision=92,internal/config=100,internal/session=96,internal/provider/providertest=98
COVER_EXCLUDE := cmd/ember,cmd/emberwl,cmd/embernode,cmd/check-coverage,internal/dockerengine/dockerclient
# Overall across the gated set. Below 100 only because internal/keys and
# internal/hostkey carry the documented stdlib-error gaps above.
COVER_OVERALL := 94

LAB_IMAGE := ember:lab

.PHONY: all build test test-race cover cover-check vet fmt lint docker-build test-integration clean

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

# vet runs twice. The default invocation never looks at files behind a build
# tag, so without the second line the entire integration lab compiles only on
# the machine that happens to run it.
vet:
	$(GO) vet ./...
	@if ls test/integration/*.go >/dev/null 2>&1; then \
		$(GO) vet -tags=integration ./test/...; \
	else \
		echo "no lab sources yet; skipping the tagged vet"; \
	fi

fmt:
	$(GO) fmt ./...

# A missing linter is a hard failure, not a skip. The earlier spelling put the
# guard and the run on separate recipe lines, where `exit 0` only leaves that
# line's subshell: make ran the linter regardless, and when it really was
# absent the target reported success without linting anything. That is how ten
# findings reached CI once. The version here must match .github/workflows/ci.yml:
# a linter that disagrees with CI makes `make lint` theatre.
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { 		echo "golangci-lint is not installed. Install it with:"; 		echo "  $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0"; 		exit 1; 	}; 	golangci-lint run

docker-build:
	docker build -t $(LAB_IMAGE) -f test/integration/Dockerfile .

# The labs refuse to skip themselves: a missing image is a hard failure with
# the command to fix it, never a t.Skip. A silently skipped lab is worse than
# a red build because it reports success for something that never ran.
test-integration: docker-build
	EMBER_INTEGRATION=1 $(GO) test -tags=integration -count=1 -timeout 20m -v ./test/integration/...

clean:
	rm -rf $(BIN) $(COVERPROFILE)
