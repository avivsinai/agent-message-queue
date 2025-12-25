.PHONY: build test fmt fmt-check vet lint ci

GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')

build:
	go build -o amq ./cmd/amq

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

ci: fmt-check vet lint test
