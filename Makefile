GO ?= go
BINARY ?= hextet-lg
BUILD_DIR ?= build

.PHONY: all build run test coverage vet fmt fmt-check tidy clean help docker release-check snapshot

all: build

build:
	$(GO) build -o $(BUILD_DIR)/$(BINARY) ./cmd/hextet-lg

run:
	$(GO) run ./cmd/hextet-lg

test:
	$(GO) test ./...

coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$($(GO) fmt ./...)" || { echo 'Go files need formatting; run make fmt'; exit 1; }

tidy:
	$(GO) mod tidy

docker: ## build the local development image hextet-lg:local (version "dev")
	docker build --build-arg REVISION=$$(git rev-parse HEAD 2>/dev/null || echo unknown) --build-arg CREATED=$$(date -u +%Y-%m-%dT%H:%M:%SZ) -t hextet-lg:local .

release-check: ## validate .goreleaser.yaml
	goreleaser check

snapshot: ## local GoReleaser build into dist/; publishes nothing
	goreleaser release --snapshot --clean

clean:
	rm -rf $(BUILD_DIR) dist coverage.out

help:
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "%-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
