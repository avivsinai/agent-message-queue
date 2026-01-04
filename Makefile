.PHONY: build test fmt fmt-check vet lint ci smoke sync-skills check-skills

GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

build:
	go build -ldflags "-X main.version=$(VERSION)" -o amq ./cmd/amq

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
	golangci-lint run

smoke:
	./scripts/smoke-test.sh

ci: check-skills fmt-check vet lint test smoke

sync-skills:
	@echo "Syncing skills from .claude/skills/ to .codex/skills/ and skills/..."
	@mkdir -p .codex/skills/amq-cli skills/amq-cli
	@if command -v rsync >/dev/null 2>&1; then \
		rsync -a --delete .claude/skills/amq-cli/ .codex/skills/amq-cli/; \
		rsync -a --delete .claude/skills/amq-cli/ skills/amq-cli/; \
	else \
		cp -R .claude/skills/amq-cli/. .codex/skills/amq-cli/; \
		cp -R .claude/skills/amq-cli/. skills/amq-cli/; \
	fi
	@echo "Done."

check-skills:
	@diff -ru .claude/skills/amq-cli .codex/skills/amq-cli
	@diff -ru .claude/skills/amq-cli skills/amq-cli
