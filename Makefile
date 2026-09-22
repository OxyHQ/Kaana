# The gates CI runs, in one executable place.
#
# AGENTS.md asks for "the commands CI runs, not near-equivalents". Until now
# those commands existed only as prose in AGENTS.md and docs/operating.md and as
# YAML in .github/workflows/ci.yml, which is three copies and no single source
# anyone can execute. This file is that source: every recipe below is the exact
# command from ci.yml, and the pinned versions are the same literals.
#
# It deliberately does NOT wrap the toolchain. There is no version manager here
# and nothing is installed for you: a Makefile that silently fetched a different
# Go than CI pins would reintroduce the near-equivalent it exists to remove.
# `make tools` reports what is missing and what version is expected.

# Pinned to match .github/workflows/ci.yml. A bump is a change to both.
GO_VERSION       := 1.26.7
GOLANGCI_VERSION := v2.13.2
BUN_VERSION      := 1.3.14

CONTRACT_DIR := tools/contract
DESCRIPTOR   := internal/contract/descriptor.json

.DEFAULT_GOAL := check

.PHONY: check ci fmt build vet lint test test-integration contract contract-generate contract-types contract-validate cloudflare tools help

## check: every gate CI runs, in CI's order. The one command to run before a PR.
check: fmt build vet lint test contract cloudflare
	@echo "all gates passed"

## ci: alias for check, for anyone reaching for the conventional name.
ci: check

## fmt: gofmt over the tree.
#
# `gofmt -l` prints unformatted files and exits 0 either way, so the exit status
# is not the gate — the OUTPUT is. A recipe that just ran `gofmt -l .` would
# pass forever while printing the files it was supposed to fail on.
fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "these files are not gofmt'd:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

## build: compile every package and command.
build:
	go build ./...

## vet: the standard vet suite.
vet:
	go vet ./...

## lint: golangci-lint at the pinned version.
#
# The version is checked rather than assumed: a locally installed lint that is
# newer than CI's reports findings CI will not, and one that is older misses the
# findings CI will fail you for. Both waste a round trip.
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint is not installed; CI pins $(GOLANGCI_VERSION)"; \
		exit 1; \
	fi
	@installed="$$(golangci-lint version 2>&1 | grep -oE 'v?[0-9]+\.[0-9]+\.[0-9]+' | head -1)"; \
	want="$(GOLANGCI_VERSION)"; \
	if [ "$${installed#v}" != "$${want#v}" ]; then \
		echo "golangci-lint $$installed is installed; CI pins $$want"; \
		exit 1; \
	fi
	golangci-lint run ./...

## test: the unit and conformance suites.
#
# -race because the data plane is concurrent by nature: a stream, a cancellation
# and an upstream read all touch the same request. -count=1 because a cached
# pass is not a run.
test:
	go test -race -count=1 ./...

## test-integration: the PostgreSQL suites, which need a real database.
#
# CI provides postgres:17-alpine with the five credential roles created. Locally
# you supply KAANA_POSTGRES_TEST_URL yourself. This target REFUSES an unset
# variable rather than skipping: the Go tests skip silently without it, so a
# target that inherited that skip would report a green integration run that
# executed no SQL at all.
test-integration:
ifndef KAANA_POSTGRES_TEST_URL
	$(error KAANA_POSTGRES_TEST_URL is unset; these suites would skip and report green. See docs/operating.md)
endif
	go test -race -count=1 -run '^TestDeploymentBindingPostgresLifecycle$$' ./internal/credentialstore
	go test -race -count=1 ./internal/credentialstore/...

## contract: the full contract-drift gate.
contract: contract-generate contract-types contract-validate

## contract-generate: regenerate the descriptor and fail on any diff.
#
# Catches a hand-edited descriptor and a version bump nobody re-derived. The
# package version is pinned exactly, so regeneration is deterministic and any
# diff is a real change. Never edit $(DESCRIPTOR) by hand.
contract-generate:
	cd $(CONTRACT_DIR) && bun install --frozen-lockfile && bun run generate
	@if ! git diff --exit-code -- $(DESCRIPTOR); then \
		echo "$(DESCRIPTOR) does not match the pinned @oxyhq/contracts package."; \
		echo "Run 'bun install --frozen-lockfile && bun run generate' in $(CONTRACT_DIR) and review the diff."; \
		exit 1; \
	fi

## contract-types: the Go types match the published contract.
contract-types:
	go test -count=1 ./internal/contract/...

## contract-validate: the shapes Kaana produces parse under the published Zod schemas.
#
# Reads the fixtures contract-types wrote, so it runs after it and not before.
# It fails on an empty fixture directory and on any rejection control it
# accepts, so a silent no-op cannot pass as a clean run.
contract-validate:
	cd $(CONTRACT_DIR) && bun run validate

## cloudflare: the reconcilers fail closed. Pure Python, no dependencies.
#
# -B so a gate run leaves no __pycache__ behind. The directory is not ignored,
# and a target that dirties `git status` every time it runs trains people to
# ignore `git status`.
cloudflare:
	python3 -B .github/scripts/cloudflare_rate_limit_test.py
	python3 -B .github/scripts/cloudflare_dns_test.py

## tools: report which pinned tools are present, and at what version.
tools:
	@printf '%-16s %s\n' "expected" "found"
	@printf '%-16s ' "go $(GO_VERSION)"; command -v go >/dev/null 2>&1 && go version || echo "NOT INSTALLED"
	@printf '%-16s ' "golangci $(GOLANGCI_VERSION)"; command -v golangci-lint >/dev/null 2>&1 && golangci-lint version 2>&1 | head -1 || echo "NOT INSTALLED"
	@printf '%-16s ' "bun $(BUN_VERSION)"; command -v bun >/dev/null 2>&1 && bun --version || echo "NOT INSTALLED"
	@printf '%-16s ' "python3"; command -v python3 >/dev/null 2>&1 && python3 --version || echo "NOT INSTALLED"

## help: list the targets.
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed -e 's/## //' -e 's/:/\t/' | awk -F'\t' '{printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
