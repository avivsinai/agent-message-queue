.PHONY: build test fmt fmt-check vet lint ci smoke contract-check check-skills hook-env-check

GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
# GoReleaser {{.Version}} is bare semver; strip one leading v from VERSION.
EMBED_VERSION := $(patsubst v%,%,$(VERSION))
GOLANGCI_LINT_CACHE ?= $(CURDIR)/.golangci-cache

build:
	go build -ldflags "-X main.version=$(EMBED_VERSION)" -o amq ./cmd/amq
	go build -ldflags "-X main.version=$(EMBED_VERSION)" -o amq-keepalive ./cmd/amq-keepalive
	go build -ldflags "-X main.version=$(EMBED_VERSION)" -o amq-bridge ./cmd/amq-bridge
	go build -ldflags "-X main.version=$(EMBED_VERSION)" -o amq-acp ./cmd/amq-acp

test:
	go test ./...

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@test -z "$(shell gofmt -l $(GO_FILES))"

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Install from https://golangci-lint.run/usage/install/"; exit 1; }
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
	@bash scripts/check-keepalive-amq-contract_test.sh
	@candidate_dir="$$(mktemp -d "$${TMPDIR:-/tmp}/amq-keepalive-candidate.XXXXXX")"; \
		trap 'rm -rf "$$candidate_dir"' EXIT; \
		go build -o "$$candidate_dir/amq" ./cmd/amq; \
		bash scripts/check-keepalive-amq-contract.sh "$$candidate_dir/amq"

# Skill integrity: skills/ is canonical, .claude/skills/ and .agents/skills/ are symlinks
check-skills:
	@echo "Checking skill symlinks..."
	@for skill in amq-cli amq-spec; do \
		test -L .claude/skills/$$skill || (echo "❌ .claude/skills/$$skill is not a symlink" && exit 1); \
		test -L .agents/skills/$$skill || (echo "❌ .agents/skills/$$skill is not a symlink" && exit 1); \
		test "$$(readlink .claude/skills/$$skill)" = "../../skills/$$skill" || (echo "❌ .claude/skills/$$skill target wrong" && exit 1); \
		test "$$(readlink .agents/skills/$$skill)" = "../../skills/$$skill" || (echo "❌ .agents/skills/$$skill target wrong" && exit 1); \
	done
	@diff -rq skills/amq-cli .claude/skills/amq-cli || (echo "❌ amq-cli content mismatch" && exit 1)
	@diff -rq skills/amq-spec .claude/skills/amq-spec || (echo "❌ amq-spec content mismatch" && exit 1)
	@echo "✓ Skill symlinks valid"
