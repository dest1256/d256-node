# d256-node task runner. Every target runs inside the dev container; nothing is installed on the host.
# On a host with make: `make test`. Without make: `./dev make test`.

ifeq ($(D256_IN_DEV),1)
RUN :=
else
RUN := ./dev
endif

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

.PHONY: devimage
devimage: ## Build the dev container image
	docker build -t d256-node-dev .devcontainer

.PHONY: proto
proto: ## Verify the panel's proto module resolves and compiles (the contract lives in ../d256-panel/proto)
	$(RUN) go build github.com/dest1256/d256-panel/proto/...

.PHONY: lint
lint: ## golangci-lint + go vet + staticcheck
	$(RUN) sh -c 'golangci-lint run && go vet ./... && staticcheck ./...'

.PHONY: test
test: ## Unit tests (shuffle, no cache)
	$(RUN) go test -shuffle=on -count=1 ./...

.PHONY: race
race: ## Tests with the race detector
	$(RUN) go test -race -shuffle=on -count=1 ./...

.PHONY: vulncheck
vulncheck: ## govulncheck
	$(RUN) govulncheck ./...

.PHONY: build
build: ## Static binary ./build/d256-node
	$(RUN) sh -c 'mkdir -p build && CGO_ENABLED=0 go build -trimpath -ldflags "-w -s -X github.com/dest1256/d256-node/internal/version.Version=$${VERSION:-dev}" -o build/d256-node ./cmd/d256-node'

.PHONY: image
image: ## Runtime image (needs ../d256-panel next to this repo)
	docker build -f Dockerfile -t d256-node:local --build-context panel=../d256-panel ..

.PHONY: verify
verify: lint test build vulncheck ## Full gate (mirrors CI)
	@echo "verify: OK"

.PHONY: release
release: verify image ## Full gate, then the runtime image
