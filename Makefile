# The host has neither Go nor ffmpeg installed, so every target runs inside
# a container. This also makes the build reproducible on a CI runner.

# A tagged commit reports its tag (v0.2.0); anything else reports the nearest
# tag plus distance and hash, or just the hash when no tag is reachable.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
IMAGE   ?= transmux
GOIMAGE ?= golang:1.26.8-alpine3.23
DOCKER  ?= docker
ACTIONLINT_IMAGE ?= rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667

# Release archives land under tmp/, which is already ignored by git and
# excluded from the image build context.
DISTDIR ?= tmp/dist
DIST_PLATFORMS ?= linux/amd64 linux/arm64

# VERSION and REVISION reach recipes only as environment variables and are
# always quoted there; they are never pasted into shell code by make, so a
# tag or git describe value such as v1.0.0-`cmd` cannot run anything. (A
# VERSION= given on the make command line is still expanded by make itself,
# so only pass trusted values there.) CHECK_VERSION additionally
# limits VERSION to what a Go -X flag, a file name and (after mapping + to _)
# a Docker tag can all carry.
export VERSION REVISION
CHECK_VERSION = case "$$VERSION" in ''|[.-]*|*[!0-9A-Za-z._+-]*) \
	  echo "invalid VERSION: must be [0-9A-Za-z._+-] and not start with . or -" >&2; \
	  exit 1;; esac

# Expanded by the shell inside the build container, not by make.
LDFLAGS := -s -w -X main.version=$$VERSION

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
build: ## Compile ingest and playback services and print their embedded version
	@$(CHECK_VERSION)
	$(RUNGO) -e CGO_ENABLED=0 -e VERSION $(GOIMAGE) sh -c 'set -e; \
	  go build -trimpath -ldflags "$(LDFLAGS)" -o /tmp/transmuxd ./cmd/transmuxd; \
	  go build -trimpath -ldflags "$(LDFLAGS)" -o /tmp/playbackd ./cmd/playbackd; \
	  printf "transmuxd %s\nplaybackd %s\n" "$$(/tmp/transmuxd -version)" "$$(/tmp/playbackd -version)"'

# Static linux binaries per platform, packed as
# $(DISTDIR)/transmux_<version>_<os>_<arch>.tar.gz plus SHA256SUMS. The release
# workflow calls this with VERSION set to the pushed tag.
.PHONY: dist
dist: ## Cross-compile release archives and checksums into $(DISTDIR)
	@$(CHECK_VERSION)
	rm -rf $(DISTDIR) && mkdir -p $(DISTDIR)
	$(RUNGO) -e CGO_ENABLED=0 -e VERSION -e SOURCE_DATE_EPOCH=$$(git log -1 --format=%ct 2>/dev/null || echo 0) \
	  $(GOIMAGE) sh -c 'set -e; apk add --no-cache tar >/dev/null; \
	  for p in $(DIST_PLATFORMS); do \
	    os=$${p%/*}; arch=$${p#*/}; name="transmux_$${VERSION}_$${os}_$${arch}"; \
	    stage="/tmp/stage/$$name"; mkdir -p "$$stage"; \
	    for cmd in transmuxd playbackd; do \
	      GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o "$$stage/$$cmd" ./cmd/$$cmd; \
	    done; \
	    for f in LICENSE NOTICE README.md README.en.md CHANGELOG.md; do [ ! -f $$f ] || cp $$f "$$stage/"; done; \
	    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@$$SOURCE_DATE_EPOCH \
	      -C /tmp/stage -czf "$(DISTDIR)/$$name.tar.gz" "$$name"; \
	  done; \
	  cd $(DISTDIR) && sha256sum *.tar.gz > SHA256SUMS && cat SHA256SUMS; \
	  chown -R $(shell id -u):$(shell id -g) /src/$(DISTDIR)'

.PHONY: lint-actions
lint-actions: ## Lint GitHub Actions workflows with actionlint
	$(DOCKER) run --rm -v $(CURDIR):/repo -w /repo $(ACTIONLINT_IMAGE) -color

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
# Docker tags cannot contain '+', so SemVer build metadata (v1.0.0+build.1)
# is kept in the binary version but mapped to '_' in the image tag.
image: ## Build the runtime container image
	@$(CHECK_VERSION)
	tag=$$(printf '%s' "$$VERSION" | tr '+' '_'); \
	$(DOCKER) build -f deploy/Dockerfile --target runtime \
	  --build-arg VERSION="$$VERSION" --build-arg REVISION="$$REVISION" \
	  -t "$(IMAGE):$$tag" -t $(IMAGE):latest .

.PHONY: poc-up
poc-up: image ## Bring up the full PoC stack: MediaMTX + fake camera + transmuxd + RustFS (local S3)
	$(DOCKER) compose -f deploy/docker-compose.poc.yml up -d --build --remove-orphans

.PHONY: poc-up-capacity
poc-up-capacity: image ## PoC stack plus the 1080p/D1/720p sources the capacity scripts need
	$(DOCKER) compose -f deploy/docker-compose.poc.yml --profile capacity up -d --build --remove-orphans

.PHONY: poc-logs
poc-logs: ## Follow transmuxd logs in the PoC stack
	$(DOCKER) compose -f deploy/docker-compose.poc.yml logs -f transmuxd

.PHONY: poc-verify
poc-verify: ## Assert the PoC produced a playable, self-consistent stream in the local S3 store
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
	$(DOCKER) compose --env-file .env.solution -f deploy/docker-compose.solution.yml up -d --build --remove-orphans

solution-down: ## Stop the solution stack, preserving recordings and configuration
	$(DOCKER) compose --env-file .env.solution -f deploy/docker-compose.solution.yml down

solution-verify: ## Verify authentication, live playback, archive and exports in the solution stack
	python3 scripts/verify-solution.py
