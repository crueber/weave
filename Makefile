VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG := github.com/crueber/weave/internal/domain/admin
VPKG := github.com/crueber/weave/internal/version
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(VPKG).Version=$(VERSION)
WEAVE_URL ?= http://localhost:8080

.PHONY: build cli skill skill-check release-local test vet docker run e2e ui-smoke

# Regenerates skills/weave/** and internal/cli/snapshot.json from the op registry. Run after ANY
# op change; `go test ./cmd/weave-skillgen` fails when they are stale.
skill:
	go run ./cmd/weave-skillgen

skill-check:
	go run ./cmd/weave-skillgen -check

# full binary (server + CLI), used by Docker
build: skill
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o weave ./cmd/weave

# slim CLI only (no server, no SQLite): what the release archives ship as `weave`
cli: skill
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o weave-cli ./cmd/weave-cli

# cross-builds the whole release matrix into dist/ (same packager CI uses)
release-local:
	VERSION=$(VERSION) scripts/release-local.sh

vet:
	go vet ./...

test: vet
	go test ./...

run: build
	./weave serve

docker:
	docker build --build-arg VERSION=$(VERSION) -t weave:$(VERSION) .

# Needs a running server and an admin API key: make e2e WEAVE_URL=... WEAVE_TOKEN=wk_...
e2e: build
	WEAVE_URL=$(WEAVE_URL) WEAVE_BIN=./weave scripts/e2e.sh

ui-smoke:
	WEAVE_URL=$(WEAVE_URL) scripts/ui-smoke.sh
