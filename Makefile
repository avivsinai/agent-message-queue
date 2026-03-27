.PHONY: build test fmt fmt-check vet lint ci smoke check-skills

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
