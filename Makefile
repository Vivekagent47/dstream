.PHONY: help dev build test lint tidy sqlc compose-up compose-down clean schema-diff schema-lint migrate-up migrate-status migrate-hash db-reset

BIN := bin/dstream
PKG := github.com/Vivekagent47/dstream

ATLAS     := atlas
ATLAS_ENV := local

help:
	@echo "make dev            - run server + worker locally (assumes compose-up done)"
	@echo "make build          - build binary into $(BIN)"
	@echo "make test           - run all tests"
	@echo "make lint           - run go vet"
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

server:
	go run ./cmd/dstream server

worker:
	go run ./cmd/dstream worker

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
	dropdb --if-exists dstream && createdb dstream && $(MAKE) migrate-up

compose-up:
	docker compose -f deploy/docker/docker-compose.yml up -d

compose-down:
	docker compose -f deploy/docker/docker-compose.yml down

clean:
	rm -rf bin .data
