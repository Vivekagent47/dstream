.PHONY: help dev build test lint tidy migrate-up migrate-down sqlc compose-up compose-down clean

BIN := bin/dstream
PKG := github.com/streamingo/dstream

help:
	@echo "make dev           - run server + worker locally (assumes compose-up done)"
	@echo "make build         - build binary into $(BIN)"
	@echo "make test          - run all tests"
	@echo "make lint          - run go vet"
	@echo "make tidy          - go mod tidy"
	@echo "make sqlc          - regenerate sqlc code"
	@echo "make migrate-up    - apply DB migrations"
	@echo "make migrate-down  - rollback last migration"
	@echo "make compose-up    - start postgres + redis + minio"
	@echo "make compose-down  - stop dev infra"

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

migrate-up:
	go run ./cmd/dstream migrate up

migrate-down:
	go run ./cmd/dstream migrate down

compose-up:
	docker compose -f deploy/docker/docker-compose.yml up -d

compose-down:
	docker compose -f deploy/docker/docker-compose.yml down

clean:
	rm -rf bin .data
