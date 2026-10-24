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

# Packages whose statements must all be covered, and the floor for each. A
# package in neither this list nor COVER_EXCLUDE fails the gate; see -strict.
COVER_PKGS := internal/node=100,internal/loopback=100,internal/cli=100
COVER_EXCLUDE := cmd/ember,cmd/emberwl,cmd/check-coverage
COVER_OVERALL := 100

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
	$(GO) vet -tags=integration ./test/... 2>/dev/null || true

fmt:
	$(GO) fmt ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed; CI runs it"; exit 0; }
	golangci-lint run

docker-build:
	docker build -t $(LAB_IMAGE) -f test/integration/Dockerfile .

# The labs refuse to skip themselves: a missing image is a hard failure with
# the command to fix it, never a t.Skip. A silently skipped lab is worse than
# a red build because it reports success for something that never ran.
test-integration: docker-build
	EMBER_INTEGRATION=1 $(GO) test -tags=integration -count=1 -timeout 20m -v ./test/integration/...

clean:
	rm -rf $(BIN) $(COVERPROFILE)
