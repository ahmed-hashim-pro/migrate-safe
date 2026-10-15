# Uses a local Go toolchain if there is one, otherwise golang:1.25 in Docker.
GO_LOCAL := $(shell command -v go 2>/dev/null)
ifeq ($(GO_LOCAL),)
GO = docker run --rm --network host -u $$(id -u):$$(id -g) -e HOME=/tmp -e GOFLAGS=-buildvcs=false \
	-e MIGRATE_SAFE_TEST_DSN -v $(CURDIR):/w -w /w golang:1.25 go
else
GO = go
endif

PG_VERSION ?= 17
PG_PORT    ?= 55432
DSN        ?= postgres://postgres:pg@localhost:$(PG_PORT)/postgres
ROWS       ?= 2000000

.PHONY: build test pg pg-stop integration examples docs demo check clean

build:
	$(GO) build -o bin/migrate-safe ./cmd/migrate-safe

# Unit tests only; no database needed.
test:
	$(GO) test -short ./...

# A throwaway PostgreSQL in Docker for measure, examples and integration.
pg:
	docker run -d --name migrate-safe-pg -p $(PG_PORT):5432 -e POSTGRES_PASSWORD=pg --shm-size=1g postgres:$(PG_VERSION)
	@until docker exec migrate-safe-pg pg_isready -q -U postgres; do sleep 1; done
	@echo "PostgreSQL $(PG_VERSION) ready: $(DSN)"

pg-stop:
	docker rm -f migrate-safe-pg

# Every test, including the corpus and the examples against $(DSN).
integration:
	MIGRATE_SAFE_TEST_DSN=$(DSN) $(GO) test -count=1 ./...

# Measure every example and check the linter against each measurement.
examples: build
	./bin/migrate-safe examples --dsn $(DSN) --rows $(ROWS) --check

# The chart in the README (5M rows, about 15 minutes).
docs: build
	./bin/migrate-safe examples --dsn $(DSN) --rows 5000000 --svg docs/examples.svg

# The terminal recording in the README (needs `make pg` first).
demo: build
	docker run --rm --network host -v $(CURDIR):/vhs -w /vhs ghcr.io/charmbracelet/vhs:v0.10.0 docs/demo.tape

check:
	test -z "$$($(GO) fmt ./...)"
	$(GO) vet ./...
	$(MAKE) test

clean:
	rm -rf bin
