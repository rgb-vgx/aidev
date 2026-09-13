# Development tasks for aidev.
#
# `make check` is the gate: it is what runs after every phase and what CI should
# run. Integration tests skip themselves unless TEST_DATABASE_URL is set, so
# `make check` passes on a machine with no database.

GO      ?= go
BIN     := bin/aidev
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Matches docker-compose.yml, which publishes PostgreSQL on 5434 because 5432
# and 5433 are commonly already taken (docs/research.md 5).
DB_PORT      ?= 5434
DB_CONTAINER ?= $(or $(AIDEV_DB_CONTAINER),aidev-postgres)
DB_URL  ?= postgres://aidev:aidev@127.0.0.1:$(DB_PORT)/aidev?sslmode=disable
TEST_DB_URL ?= postgres://aidev:aidev@127.0.0.1:$(DB_PORT)/aidev_test?sslmode=disable

.PHONY: help build install test test-integration test-e2e test-db-create fmt fmt-check vet lint check \
        db-up db-down db-reset db-logs migrate clean

help: ## show this help
	@grep -hE '^[a-z0-9-]+:.*?##' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## build the aidev binary into bin/
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/aidev

install: ## install aidev into GOPATH/bin
	$(GO) install -ldflags "-X main.version=$(VERSION)" ./cmd/aidev

test: ## run unit tests (integration tests skip without a database)
	$(GO) test ./...

test-integration: ## run every test, including those needing PostgreSQL
	TEST_DATABASE_URL="$(TEST_DB_URL)" $(GO) test ./... -count=1

fmt: ## format all Go code
	gofmt -w .

fmt-check: ## fail if any file needs formatting
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

vet: ## run go vet
	$(GO) vet ./...

lint: ## run staticcheck when it is installed
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; \
	elif [ -x "$$($(GO) env GOPATH)/bin/staticcheck" ]; then "$$($(GO) env GOPATH)/bin/staticcheck" ./...; \
	else echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; fi

check: fmt-check vet lint test ## the gate: formatting, static analysis, tests
	@echo "all checks passed"

db-up: ## start PostgreSQL and wait for it to accept connections
	docker compose up -d
	@printf 'waiting for postgres'
	@for i in $$(seq 1 60); do \
		if [ "$$(docker inspect -f '{{.State.Health.Status}}' $(DB_CONTAINER) 2>/dev/null)" = healthy ]; then echo " ready"; exit 0; fi; \
		printf '.'; sleep 1; \
	done; echo " timed out"; exit 1

db-down: ## stop PostgreSQL, keeping data
	docker compose down

db-reset: ## destroy the database and its data, then start fresh
	docker compose down -v
	$(MAKE) db-up

db-logs: ## follow PostgreSQL logs
	docker compose logs -f postgres

migrate: build ## apply pending migrations to the development database
	DATABASE_URL="$(DB_URL)" $(BIN) migrate

test-e2e: ## run the end-to-end test against the real opencode (slow, needs postgres)
	TEST_DATABASE_URL="$(TEST_DB_URL)" AIDEV_TEST_OPENCODE=1 \
		$(GO) test ./tests/e2e/ -count=1 -v -timeout 20m

test-db-create: ## create the database used by integration tests
	docker compose exec -T postgres psql -U aidev -d postgres -c "SELECT 1 FROM pg_database WHERE datname='aidev_test'" | grep -q 1 \
		|| docker compose exec -T postgres createdb -U aidev aidev_test

clean: ## remove build output
	rm -rf bin
