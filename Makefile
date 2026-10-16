BINARY := feather
OUT    := bin/$(BINARY)
PKG    := github.com/unifers/feather/internal/buildinfo

# Version comes from the nearest git tag; a working tree with uncommitted
# changes is marked -dirty so a build can never be mistaken for a release.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "(devel)")
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG).version=$(VERSION) \
	-X $(PKG).commit=$(COMMIT) \
	-X $(PKG).date=$(DATE)

.PHONY: build test test-race test-integration vet fmt fmt-check lint vuln check clean install \
        rig-up rig-down rig-seed rig-serve version

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(OUT) ./cmd/feather

version:
	@echo $(VERSION)

test:
	go test -race -count=1 ./...

# Without -race, for a quick inner loop. CI always uses `test`.
test-fast:
	go test ./...

# The end-to-end suite against the local rig. Skips itself when the rig is not
# up, so it is safe to run unconditionally.
test-integration:
	@FEATHER_TEST_MONGO_URI=mongodb://127.0.0.1:27018 \
	FEATHER_TEST_S3_ENDPOINT=http://127.0.0.1:9000 \
	FEATHER_TEST_POSTGRES_DSN='postgres://feather:feather@127.0.0.1:5433/feather?sslmode=disable' \
	go test -race -count=1 -tags=integration -timeout 20m ./internal/e2e/... ./internal/pgsrc/... ./internal/runner/...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then echo "not gofmt'd:"; echo "$$files"; exit 1; fi

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# What CI runs. Run this before pushing.
check: fmt-check vet lint test vuln

clean:
	rm -rf bin dist

install: build
	install -m 0755 $(OUT) /usr/local/bin/$(BINARY)

# Local verification rig: Mongo, PostgreSQL and MinIO in Docker.
rig-up:
	docker compose -f testdata/docker-compose.yml up -d

rig-down:
	docker compose -f testdata/docker-compose.yml down -v

rig-seed:
	go run ./testdata/seed

# Import the rig's env into a fresh database and open the control panel.
rig-serve: build
	./bin/feather import --env testdata/.env.rig --from-env testdata/.env.rig --from-configs ./testdata/configs
	./bin/feather serve --env testdata/.env.rig
