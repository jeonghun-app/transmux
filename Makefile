# The host has neither Go nor ffmpeg installed, so every target runs inside
# a container. This also makes the build reproducible on a CI runner.

VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
IMAGE   ?= transmux
GOIMAGE ?= golang:1.23-alpine
DOCKER  ?= docker

# Cache the module and build cache on the host so repeat runs are fast.
GOCACHE_VOL := transmux-gocache
GOMOD_VOL   := transmux-gomodcache

RUNGO = $(DOCKER) run --rm \
	-v $(CURDIR):/src -w /src \
	-v $(GOCACHE_VOL):/gocache \
	-v $(GOMOD_VOL):/gomodcache \
	-e GOCACHE=/gocache -e GOMODCACHE=/gomodcache \
	-e GOFLAGS=-buildvcs=false

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Resolve dependencies and write go.sum
	$(RUNGO) $(GOIMAGE) go mod tidy

.PHONY: vet
vet: ## go vet
	$(RUNGO) $(GOIMAGE) go vet ./...

.PHONY: build
build: ## Compile the daemon
	$(RUNGO) $(GOIMAGE) go build -trimpath -o /tmp/transmuxd ./cmd/transmuxd

.PHONY: test
test: ## Unit tests
	$(RUNGO) $(GOIMAGE) go test ./... -count=1

.PHONY: race
race: ## Unit tests under the race detector
	$(RUNGO) -e CGO_ENABLED=1 $(GOIMAGE) sh -c \
	  'apk add --no-cache gcc musl-dev >/dev/null && go test -race ./... -count=1'

.PHONY: test-ffmpeg
test-ffmpeg: ## Tests that need a real ffmpeg binary (build tag: ffmpeg)
	$(DOCKER) build -f deploy/Dockerfile --target test -t $(IMAGE):test .
	$(DOCKER) run --rm $(IMAGE):test \
	  go test ./... -count=1 -tags ffmpeg -v -timeout 10m

.PHONY: image
image: ## Build the runtime container image
	$(DOCKER) build -f deploy/Dockerfile --target runtime \
	  --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: poc-up
poc-up: image ## Bring up the full PoC stack: MediaMTX + fake camera + transmuxd + MinIO
	$(DOCKER) compose -f deploy/docker-compose.poc.yml up -d --build

.PHONY: poc-logs
poc-logs: ## Follow transmuxd logs in the PoC stack
	$(DOCKER) compose -f deploy/docker-compose.poc.yml logs -f transmuxd

.PHONY: poc-verify
poc-verify: ## Assert the PoC produced a playable, self-consistent stream in MinIO
	$(DOCKER) compose -f deploy/docker-compose.poc.yml exec -T verifier /verify.sh

.PHONY: measure-cpu
measure-cpu: ## Measure precise per-channel CPU cost (needs poc-up)
	./scripts/measure-cpu.sh --profile hd 1 10 25 50

.PHONY: poc-down
poc-down: ## Tear down the PoC stack and its volumes
	$(DOCKER) compose -f deploy/docker-compose.poc.yml down -v

.PHONY: clean
clean:
	-$(DOCKER) volume rm $(GOCACHE_VOL) $(GOMOD_VOL)
