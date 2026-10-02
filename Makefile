.PHONY: help dev build test lint tidy sqlc compose-up compose-down clean schema-diff schema-lint migrate-up migrate-status migrate-hash db-reset load

BIN := bin/dstream
PKG := github.com/Vivekagent47/dstream

ATLAS     := atlas
ATLAS_ENV := local

help:
	@echo "make dev            - run server + worker locally (assumes compose-up done)"
	@echo "make build          - build binary into $(BIN)"
	@echo "make test           - run all tests"
	@echo "make lint           - run go vet"
	@echo "make load           - ingest load test (URL=... [RATE DUR CONC DB])"
	@echo "make tidy           - go mod tidy"
	@echo "make sqlc           - regenerate sqlc code"
	@echo "make schema-diff    - generate a new migration (NAME=add_foo)"
	@echo "make schema-lint    - lint the latest migration"
	@echo "make migrate-up     - apply pending migrations"
	@echo "make migrate-status - show migration status"
	@echo "make migrate-hash   - recompute atlas.sum"
	@echo "make db-reset       - drop, recreate, and migrate the dev DB"
	@echo "make compose-up     - start postgres + redis + minio"
	@echo "make compose-down   - stop dev infra"

dev:
	@echo "Run 'make server' and 'make worker' in separate terminals."

# Source repo-root .env before running (config reads env only — no dotenv loader).
# `set -a` exports every var to the go process; compose loads .env on its own.
server:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/dstream server

worker:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/dstream worker

# Standalone ingest load-test harness (tools/loadtest). URL required.
# Env overrides: RATE(100) DUR(60s) CONC(50) DB(local test DB). Always -sink.
load:
	@test -n "$(URL)" || (echo "usage: make load URL=http://localhost:8080/e/<token> [RATE=100 DUR=60s CONC=50 DB=...]"; exit 1)
	set -a; [ -f .env ] && . ./.env; set +a; go run ./tools/loadtest \
		-url "$(URL)" \
		-rate $(or $(RATE),100) \
		-dur $(or $(DUR),60s) \
		-conc $(or $(CONC),50) \
		-db "$(or $(DB),postgres://dstream:dstream@localhost:5433/dstream?sslmode=disable)" \
		-sink

build:
	mkdir -p bin
	go build -o $(BIN) ./cmd/dstream

test:
	go test ./... -race -count=1

lint:
	go vet ./...

tidy:
	go mod tidy

sqlc:
	sqlc generate

schema-diff:
	@test -n "$(NAME)" || (echo "usage: make schema-diff NAME=add_foo"; exit 1)
	@# Atlas recreates `public` in the dev database on every diff, which drops
	@# the extensions schema.sql depends on. Reinstall them first or the diff
	@# fails on the first citext column.
	@psql "$${DSTREAM_ATLAS_DEV_URL:?set DSTREAM_ATLAS_DEV_URL}" -q \
		-c 'CREATE EXTENSION IF NOT EXISTS pgcrypto' \
		-c 'CREATE EXTENSION IF NOT EXISTS citext'
	$(ATLAS) migrate diff $(NAME) --env $(ATLAS_ENV)

schema-lint:
	$(ATLAS) migrate lint --env $(ATLAS_ENV) --latest 1

migrate-up:
	$(ATLAS) migrate apply --env $(ATLAS_ENV)

migrate-status:
	$(ATLAS) migrate status --env $(ATLAS_ENV)

migrate-hash:
	$(ATLAS) migrate hash --env $(ATLAS_ENV)

db-reset:
	@# DESTRUCTIVE. Two guards, both earned the hard way:
	@#
	@# 1. CONFIRM=yes. This target used to be the thing an operator reached for
	@#    when `make migrate-up` failed, which cost a working database more than
	@#    once. migrate-up is fixed now, but the reflex is worth blocking.
	@# 2. It derives the target from DSTREAM_DB_URL instead of calling bare
	@#    dropdb/createdb, which used libpq defaults — no host, no port. With a
	@#    second Postgres listening on 5432 and the dev stack on 5433, that
	@#    dropped a database on the wrong server while leaving the real one
	@#    untouched.
	@test "$(CONFIRM)" = "yes" || (echo "db-reset is destructive: it drops the database named in DSTREAM_DB_URL.\nRe-run with: make db-reset CONFIRM=yes"; exit 1)
	@test -n "$$DSTREAM_DB_URL" || (echo "DSTREAM_DB_URL is not set"; exit 1)
	@DB=$$(printf '%s' "$$DSTREAM_DB_URL" | sed -E 's#.*/([^/?]+)(\?.*)?$$#\1#'); \
	ADMIN=$$(printf '%s' "$$DSTREAM_DB_URL" | sed -E "s#/$$DB(\?|$$)#/postgres\1#"); \
	echo "dropping and recreating \"$$DB\""; \
	psql "$$ADMIN" -q -c "DROP DATABASE IF EXISTS \"$$DB\" WITH (FORCE)" \
	               -c "CREATE DATABASE \"$$DB\""
	$(MAKE) migrate-up

compose-up:
	docker compose -f deploy/docker/docker-compose.yml up -d

compose-down:
	docker compose -f deploy/docker/docker-compose.yml down

clean:
	rm -rf bin .data
