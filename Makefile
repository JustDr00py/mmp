# MMP — Go IPTV multiplexer / failover proxy

BIN      := mmp
GO       ?= go
GOFLAGS  ?=
# Postgres password from .env (read literally, not as make syntax), falling
# back to the example password of a bare `make up` without .env.
POSTGRES_PASSWORD ?= $(or $(shell sed -n 's/^POSTGRES_PASSWORD=//p' .env 2>/dev/null),mmp)
DSN      ?= postgres://mmp:$(POSTGRES_PASSWORD)@localhost:5432/mmp?sslmode=disable
# Server the integration tests create their databases on.
export MMP_TEST_PG ?= postgres://mmp:$(POSTGRES_PASSWORD)@localhost:5432

# Prefer a locally installed sqlc; fall back to go run (slower first time).
SQLC     := $(shell command -v sqlc 2>/dev/null || echo "$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@latest")

# docker compose or podman compose, e.g. make up COMPOSE="podman compose".
COMPOSE  ?= docker compose

.PHONY: build dev test test-integration lint vet staticcheck generate migrate up down

build:
	$(GO) build $(GOFLAGS) -o $(BIN) ./cmd/mmp

# Run Postgres (compose) and start mmp against it with live reload of config.
dev:
	$(GO) run ./cmd/mmp -config mmp.yaml

test:
	$(GO) test -race ./...

# Needs a running Postgres (see `make up`). Packages run serially because
# integration tests recreate their databases.
test-integration:
	$(GO) test -race -p 1 -tags=integration ./...

lint: vet staticcheck

vet:
	$(GO) vet ./...

staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

generate:
	$(SQLC) generate

# Migrations run automatically at startup; this forces migrate-then-exit.
migrate:
	$(GO) run ./cmd/mmp -config mmp.yaml -migrate

up:
	$(COMPOSE) up -d postgres

down:
	$(COMPOSE) down
