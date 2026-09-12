# The host has neither Go nor ffmpeg installed, so every target runs inside
# a container. This also makes the build reproducible on a CI runner.

VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
IMAGE   ?= transmux
GOIMAGE ?= golang:1.26.8-alpine3.23
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

.PHONY: fmt-check
fmt-check: ## Check Go formatting
	$(RUNGO) $(GOIMAGE) sh -c 'files=$$(gofmt -l cmd internal); test -z "$$files" || { printf "%s\n" "$$files"; exit 1; }'

.PHONY: vuln
vuln: ## Verify modules and scan reachable Go vulnerabilities
	$(RUNGO) $(GOIMAGE) go mod verify
	$(RUNGO) $(GOIMAGE) go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

.PHONY: build
build: ## Compile ingest and playback services
	$(RUNGO) $(GOIMAGE) go build -trimpath -o /tmp/transmuxd ./cmd/transmuxd
	$(RUNGO) $(GOIMAGE) go build -trimpath -o /tmp/playbackd ./cmd/playbackd

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

.PHONY: poc-up-capacity
poc-up-capacity: image ## PoC stack plus the 1080p/D1/720p sources the capacity scripts need
	$(DOCKER) compose -f deploy/docker-compose.poc.yml --profile capacity up -d --build

.PHONY: poc-logs
poc-logs: ## Follow transmuxd logs in the PoC stack
	$(DOCKER) compose -f deploy/docker-compose.poc.yml logs -f transmuxd

.PHONY: poc-verify
poc-verify: ## Assert the PoC produced a playable, self-consistent stream in MinIO
	$(DOCKER) compose -f deploy/docker-compose.poc.yml exec -T verifier /verify.sh

.PHONY: measure-cpu
measure-cpu: ## Measure precise per-channel CPU cost (needs poc-up-capacity)
	./scripts/measure-cpu.sh --profile hd 1 10 25 50

.PHONY: poc-down
poc-down: ## Tear down the PoC stack and its volumes
	$(DOCKER) compose -f deploy/docker-compose.poc.yml --profile capacity down -v

.PHONY: clean
clean:
	-$(DOCKER) volume rm $(GOCACHE_VOL) $(GOMOD_VOL)

.PHONY: solution-env solution-up solution-down solution-verify
solution-env: ## Generate local solution credentials (existing values are preserved)
	python3 scripts/solution-env.py

solution-up: solution-env ## Run private storage, ingest, playback console and three simulated cameras
	$(DOCKER) compose --env-file .env.solution -f deploy/docker-compose.solution.yml up -d --build

solution-down: ## Stop the solution stack, preserving recordings and configuration
	$(DOCKER) compose --env-file .env.solution -f deploy/docker-compose.solution.yml down

solution-verify: ## Verify authentication, live playback, archive and exports in the solution stack
	python3 scripts/verify-solution.py
