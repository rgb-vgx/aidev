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

# Every aidev setting lives in the conf.json file this names. Copy
# conf/conf.example.json to conf/conf.json and edit it to match the database.
AIDEV_CONFIG ?= $(CURDIR)/conf/conf.json

.PHONY: help build install test test-integration test-e2e test-db-create fmt fmt-check vet lint check \
        db-up db-down db-reset db-logs migrate clean \
        langfuse-up langfuse-down langfuse-reset langfuse-logs langfuse-env langfuse-credentials \
        jaeger-up jaeger-down jaeger-env \
        shim-test shim-install

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

# Only the repository's own files. `gofmt -l .` walks ignored directories too,
# so a cloned upstream repo under .probe/ made `make check` fail on code aidev
# does not own and must not reformat. --others is needed as well as --cached: an
# agent's brand-new file is untracked when verification runs, and a gate that
# skips exactly the files under review is no gate.
GOFILES = git ls-files -z --cached --others --exclude-standard '*.go'

fmt: ## format all Go code
	@$(GOFILES) | xargs -0 --no-run-if-empty gofmt -w

fmt-check: ## fail if any file needs formatting
	@out=$$($(GOFILES) | xargs -0 --no-run-if-empty gofmt -l); \
	if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

vet: ## run go vet
	$(GO) vet ./...

lint: ## run staticcheck when it is installed
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; \
	elif [ -x "$$($(GO) env GOPATH)/bin/staticcheck" ]; then "$$($(GO) env GOPATH)/bin/staticcheck" ./...; \
	else echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; fi

check: fmt-check vet lint test shim-test ## the gate: formatting, static analysis, tests
	@echo "all checks passed"

# tools/anthropic-shim lets Claude Code's auto mode work through 9router with
# muse-spark (docs/research.md 7f). Its self-test runs in the gate so the tool
# cannot rot unnoticed; like staticcheck, it is skipped where python3 is absent.
shim-test: ## self-test tools/anthropic-shim (skipped without python3)
	@if command -v python3 >/dev/null 2>&1; then python3 tools/anthropic-shim/anthropic_shim.py --self-test 2>/dev/null; \
	else echo "python3 not installed; skipping anthropic-shim self-test"; fi

shim-install: shim-test ## install anthropic-shim as a systemd user service on 127.0.0.1:20198
	install -D -m 0755 tools/anthropic-shim/anthropic_shim.py $(HOME)/.local/share/anthropic-shim/anthropic_shim.py
	install -D -m 0644 tools/anthropic-shim/anthropic-shim.service $(HOME)/.config/systemd/user/anthropic-shim.service
	systemctl --user daemon-reload
	systemctl --user enable anthropic-shim
	systemctl --user restart anthropic-shim

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

# Jaeger: one container, shows a trace immediately, no credentials. It is the
# fastest way to tell "aidev's instrumentation is wrong" from "the backend is not
# showing it", which is a distinction that cost real time before this target
# existed.
JAEGER_CONTAINER ?= aidev-jaeger

jaeger-up: ## start Jaeger to view traces (one container, UI on :16686)
	-docker rm -f $(JAEGER_CONTAINER) >/dev/null 2>&1
	docker run -d --name $(JAEGER_CONTAINER) \
		-p 127.0.0.1:16686:16686 -p 127.0.0.1:4318:4318 \
		jaegertracing/all-in-one:latest >/dev/null
	@printf 'waiting for jaeger'
	@for i in $$(seq 1 60); do \
		if curl -fsS -m 2 http://localhost:16686/ >/dev/null 2>&1; then echo " ready: http://localhost:16686"; exit 0; fi; \
		printf '.'; sleep 1; \
	done; echo " timed out"; exit 1

jaeger-down: ## stop and remove Jaeger
	-docker rm -f $(JAEGER_CONTAINER)

jaeger-env: ## print the export lines that point aidev at the local Jaeger
	@echo "export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318"
	@echo "export OTEL_SERVICE_NAME=aidev"
	@echo "unset OTEL_EXPORTER_OTLP_HEADERS"

LANGFUSE_COMPOSE := deployments/langfuse/docker-compose.yml
LANGFUSE_ENV     := deployments/langfuse/.env

langfuse-up: $(LANGFUSE_ENV) ## start self-hosted Langfuse for viewing traces (heavy: 6 services)
	docker compose -f $(LANGFUSE_COMPOSE) --env-file $(LANGFUSE_ENV) up -d
	@printf 'waiting for langfuse'
	@for i in $$(seq 1 120); do \
		if curl -fsS http://localhost:3000/api/public/health >/dev/null 2>&1; then echo " ready: http://localhost:3000"; exit 0; fi; \
		printf '.'; sleep 2; \
	done; echo " timed out; check: make langfuse-logs"; exit 1

langfuse-down: ## stop Langfuse, keeping its data
	docker compose -f $(LANGFUSE_COMPOSE) --env-file $(LANGFUSE_ENV) down

langfuse-reset: ## destroy Langfuse and all of its data
	docker compose -f $(LANGFUSE_COMPOSE) --env-file $(LANGFUSE_ENV) down -v

langfuse-logs: ## follow Langfuse logs
	docker compose -f $(LANGFUSE_COMPOSE) --env-file $(LANGFUSE_ENV) logs -f

# Secrets are generated rather than committed. Upstream's compose ships
# placeholders (mysalt, mysecret, miniosecret); shipping those in a repository
# would be worse than no secret at all, because they look deliberate.
$(LANGFUSE_ENV):
	@command -v openssl >/dev/null || { echo "openssl is required to generate secrets"; exit 1; }
	@umask 077; { \
		echo "# Generated by \`make langfuse-up\`. Not committed: see .gitignore."; \
		echo "SALT=$$(openssl rand -hex 32)"; \
		echo "ENCRYPTION_KEY=$$(openssl rand -hex 32)"; \
		echo "NEXTAUTH_SECRET=$$(openssl rand -hex 32)"; \
		echo "LANGFUSE_DB_PASSWORD=$$(openssl rand -hex 16)"; \
		echo "CLICKHOUSE_PASSWORD=$$(openssl rand -hex 16)"; \
		echo "REDIS_AUTH=$$(openssl rand -hex 16)"; \
		echo ""; \
		echo "# One secret, four variables. Upstream gives all four the same default"; \
		echo "# (miniosecret) because Langfuse authenticates to MinIO with these keys:"; \
		echo "# the shared default encodes a relationship, and generating them"; \
		echo "# independently breaks S3 uploads with a 500 that says nothing useful."; \
		minio_secret=$$(openssl rand -hex 16); \
		echo "MINIO_ROOT_PASSWORD=$$minio_secret"; \
		echo "LANGFUSE_S3_EVENT_UPLOAD_SECRET_ACCESS_KEY=$$minio_secret"; \
		echo "LANGFUSE_S3_MEDIA_UPLOAD_SECRET_ACCESS_KEY=$$minio_secret"; \
		echo "LANGFUSE_S3_BATCH_EXPORT_SECRET_ACCESS_KEY=$$minio_secret"; \
		echo ""; \
		echo "# Headless bootstrap: Langfuse creates this org, project, user and"; \
		echo "# API key pair on first start, so no clicking through the UI is needed."; \
		echo "LANGFUSE_INIT_ORG_ID=aidev"; \
		echo "LANGFUSE_INIT_ORG_NAME=aidev"; \
		echo "LANGFUSE_INIT_PROJECT_ID=aidev"; \
		echo "LANGFUSE_INIT_PROJECT_NAME=aidev"; \
		echo "LANGFUSE_INIT_PROJECT_PUBLIC_KEY=pk-lf-$$(openssl rand -hex 16)"; \
		echo "LANGFUSE_INIT_PROJECT_SECRET_KEY=sk-lf-$$(openssl rand -hex 16)"; \
		echo "LANGFUSE_INIT_USER_EMAIL=aidev@example.com"  # a TLD is required: aidev@localhost is rejected; \
		echo "LANGFUSE_INIT_USER_NAME=aidev"; \
		echo "LANGFUSE_INIT_USER_PASSWORD=$$(openssl rand -hex 12)"; \
	} > $@
	@echo "generated $@ with fresh secrets"

langfuse-env: $(LANGFUSE_ENV) ## print the export lines that point aidev at the local Langfuse
	@set -a; . ./$(LANGFUSE_ENV); set +a; \
	auth=$$(printf '%s:%s' "$$LANGFUSE_INIT_PROJECT_PUBLIC_KEY" "$$LANGFUSE_INIT_PROJECT_SECRET_KEY" | base64 -w0); \
	echo "export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:3000/api/public/otel"; \
	echo "export OTEL_EXPORTER_OTLP_HEADERS='Authorization=Basic $$auth,x-langfuse-ingestion-version=4'"; \
	echo "export OTEL_SERVICE_NAME=aidev"

langfuse-credentials: $(LANGFUSE_ENV) ## print the Langfuse UI login for the bootstrapped user
	@set -a; . ./$(LANGFUSE_ENV); set +a; \
	echo "http://localhost:3000"; \
	echo "  email:    $$LANGFUSE_INIT_USER_EMAIL"; \
	echo "  password: $$LANGFUSE_INIT_USER_PASSWORD"

db-logs: ## follow PostgreSQL logs
	docker compose logs -f postgres

migrate: build ## apply pending migrations to the development database
	AIDEV_CONFIG="$(AIDEV_CONFIG)" $(BIN) migrate

test-e2e: ## run the end-to-end test against the real opencode (slow, needs postgres)
	TEST_DATABASE_URL="$(TEST_DB_URL)" AIDEV_TEST_OPENCODE=1 \
		$(GO) test ./tests/e2e/ -count=1 -v -timeout 20m

test-db-create: ## create the database used by integration tests
	docker compose exec -T postgres psql -U aidev -d postgres -c "SELECT 1 FROM pg_database WHERE datname='aidev_test'" | grep -q 1 \
		|| docker compose exec -T postgres createdb -U aidev aidev_test

clean: ## remove build output
	rm -rf bin
