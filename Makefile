REGISTRY ?= ghcr.io/kelos-dev
VERSION ?= latest
IMAGE_PLATFORMS ?= linux/$(shell go env GOARCH)
PUSH ?= false

BINARY ?= bin/aloop
TEST_FLAGS ?=
comma := ,

.PHONY: build verify test test-e2e image

build: ## Build the aloop executable.
	CGO_ENABLED=0 go build -o "$(BINARY)" ./cmd/aloop

verify: ## Check formatting, module files, and Go diagnostics.
	@set -e; files=$$(gofmt -l .); if [ -n "$$files" ]; then echo "Run gofmt on:"; echo "$$files"; exit 1; fi
	go mod tidy -diff
	go vet ./...

test: ## Run unit and component tests.
	go test $(TEST_FLAGS) -skip '^TestE2E' ./...

test-e2e: ## Run end-to-end scenarios using local fixtures.
	go test $(TEST_FLAGS) -count=1 -v -run '^TestE2E' ./cmd/aloop ./internal/personal

image: ## Build and load the container image, or publish it with PUSH=true.
	@set -e; for platform in $(subst $(comma), ,$(IMAGE_PLATFORMS)); do \
		arch=$${platform#linux/}; \
		$(MAKE) build GOOS=linux GOARCH=$$arch BINARY=bin/aloop-linux-$$arch; \
	done
	docker buildx build --platform "$(IMAGE_PLATFORMS)" \
		$(if $(filter true,$(PUSH)),--push,--load) \
		-t "$(REGISTRY)/aloop:$(VERSION)" -f cmd/aloop/Dockerfile bin
