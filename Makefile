.PHONY: help dev build test cover cover-report lint tidy sqlc compose-up compose-down clean schema-diff schema-lint migrate-up migrate-status migrate-hash db-reset load

BIN := bin/dstream
PKG := github.com/Vivekagent47/dstream

ATLAS     := atlas
ATLAS_ENV := local

# Minimum backend statement coverage. `make cover` and CI both fail below
# it; raise it as coverage climbs, never lower it to make a branch pass.
COVERAGE_MIN ?= 95

# Go packages, minus the one third-party package vendored inside the npm
# tree (web/node_modules/flatted/golang/...). `go list ./...` picks it up,
# so without this every test run compiles and "tests" someone else's code
# and drags the coverage total down with it.
GO_PKGS = $$(go list ./... | grep -v /web/node_modules/)

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
	go test $(GO_PKGS) -race -count=1

# Coverage gate. CI calls this target (.github/workflows/ci.yml), so the
# threshold and the exclusions below live in exactly one place.
#
# sqlc output is stripped from the profile before the total is computed:
# it is machine-written, nobody tests it directly, and counting it means
# adding a query silently lowers coverage. Keep the pattern in sync with
# sqlc.yaml if the generated file set changes.
#
# Needs the same environment as `make test`: a migrated DSTREAM_TEST_DB_URL
# and DSTREAM_REDIS_ADDR. Without them the DB tests skip, and a skipped
# test still reports as covered-nothing — a passing run with a meaningless
# number. See PLAN.md section 9 for the command.
cover:
	@# -coverpkg is not optional here. Without it Go credits a statement only
	@# to the package whose own test binary ran it, and this suite tests most
	@# handlers through the router that mounts them: internal/api's tests drive
	@# internal/api/identity, so identity measured 0.3% while being heavily
	@# exercised. Attributing honestly moved the total from 42.8% to 55.5%
	@# without a single new test.
	go test $(GO_PKGS) -race -count=1 -covermode=atomic \
		-coverpkg=$$(go list ./... | grep -v /web/node_modules/ | paste -sd, -) \
		-coverprofile=cover.out
	@grep -vE '\.sql\.go:|internal/store/(models|db|querier)\.go:' cover.out > cover.real.out
	@go tool cover -func=cover.real.out | tail -1
	@total=$$(go tool cover -func=cover.real.out | awk 'END { gsub("%","",$$3); print $$3 }'); \
	 awk -v t="$$total" -v m="$(COVERAGE_MIN)" 'BEGIN {\
	   if (t+0 < m+0) { printf "FAIL: backend coverage %.1f%% is below the %s%% minimum\n", t, m; exit 1 }\
	   printf "ok: backend coverage %.1f%% meets the %s%% minimum\n", t, m }'

# Per-package breakdown of the last `make cover` run. Blocks repeat in a
# -coverpkg profile (one copy per test binary), so dedupe on block position
# and keep the highest count before summing, or every number comes out
# multiplied by the number of packages that ran.
cover-report:
	@test -f cover.real.out || (echo "no profile — run: make cover"; exit 1)
	@awk 'NR > 1 { n[$$1] = $$2; if ($$3 + 0 > c[$$1] + 0) c[$$1] = $$3 + 0 } \
	      END { \
	        for (k in n) { \
	          split(k, a, ":"); f = a[1]; \
	          sub(/^github\.com\/Vivekagent47\/dstream\//, "", f); \
	          pkg = f; sub(/\/[^\/]+$$/, "", pkg); \
	          total[pkg] += n[k]; if (c[k] == 0) uncovered[pkg] += n[k] \
	        } \
	        for (p in total) \
	          printf "%6d uncovered  %6d total  %5.1f%%  %s\n", \
	            uncovered[p], total[p], 100 * (total[p] - uncovered[p]) / total[p], p \
	      }' cover.real.out | sort -rn

lint:
	go vet $(GO_PKGS)
	@# gofmt is a gate, not a suggestion: one unformatted file makes every
	@# later diff that touches it carry formatting noise alongside the change.
	@unformatted=$$(gofmt -l cmd db internal tools); \
	 if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

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
