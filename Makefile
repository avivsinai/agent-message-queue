.PHONY: build test fmt fmt-check vet lint ci smoke contract-check check-skills hook-env-check

GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
# GoReleaser {{.Version}} is bare semver; strip one leading v from VERSION.
EMBED_VERSION := $(patsubst v%,%,$(VERSION))
GOLANGCI_LINT_CACHE ?= $(CURDIR)/.golangci-cache
# Keep this in lockstep with the version pinned in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := 2.13.1
# Use go.mod's toolchain so direct gofmt does not drift from setup-go.
GO_TOOLCHAIN := $(shell sed -n 's/^toolchain //p' go.mod)
GOFMT := $(shell GOTOOLCHAIN=$(GO_TOOLCHAIN) go env GOROOT)/bin/gofmt

build:
	go build -ldflags "-X main.version=$(EMBED_VERSION)" -o amq ./cmd/amq
	go build -ldflags "-X main.version=$(EMBED_VERSION)" -o amq-keepalive ./cmd/amq-keepalive
	go build -ldflags "-X main.version=$(EMBED_VERSION)" -o amq-bridge ./cmd/amq-bridge
	go build -ldflags "-X main.version=$(EMBED_VERSION)" -o amq-acp ./cmd/amq-acp

test:
	go test ./...

fmt:
	$(GOFMT) -w $(GO_FILES)

fmt-check:
	@test -z "$(shell $(GOFMT) -l $(GO_FILES))"

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Install from https://golangci-lint.run/usage/install/"; exit 1; }
	@actual="$$(golangci-lint version --short 2>/dev/null || true)"; \
	if [ "$$actual" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) required (found $${actual:-unknown}); install that version from https://golangci-lint.run/usage/install/"; \
		exit 1; \
	fi
	GOLANGCI_LINT_CACHE="$(GOLANGCI_LINT_CACHE)" golangci-lint run

# Keep this hostile AMQ context tuple aligned with smoke-test.sh's startup scrub.
smoke:
	AM_ROOT=/smoke/inherited/root \
	AM_ROOT_ID=smoke-inherited-root-id \
	AM_ME=smoke-caller \
	AM_BASE_ROOT=/smoke/inherited/base \
	AM_BASE_ROOT_ID=smoke-inherited-base-root-id \
	AM_SESSION=smoke-session \
	AMQ_GLOBAL_ROOT=/smoke/inherited/global \
	AMQ_WAKE_OWNER=smoke-inherited-wake-owner \
	./scripts/smoke-test.sh

ci: check-skills fmt-check vet lint test smoke contract-check hook-env-check

hook-env-check:
	@sh scripts/test_pre_push_hook_env.sh

contract-check:
	@sh scripts/probe-codex-app-execute-javascript_test.sh
	@bash scripts/check-keepalive-amq-contract_test.sh
	@candidate_dir="$$(mktemp -d "$${TMPDIR:-/tmp}/amq-keepalive-candidate.XXXXXX")"; \
		trap 'rm -rf "$$candidate_dir"' EXIT; \
		go build -o "$$candidate_dir/amq" ./cmd/amq; \
		bash scripts/check-keepalive-amq-contract.sh "$$candidate_dir/amq"

# Skill integrity: skills/ is canonical; .claude, .agents, and .grok skills are symlinks.
# The negative fixture must run first so a fail-open checker cannot hide behind a later live pass.
check-skills:
	@bash scripts/test_check_skills.sh
	@bash scripts/check-skills.sh
