.DEFAULT_GOAL := help
SHELL := /bin/bash

DB_URL      ?= postgres://notes:notes@localhost:5433/notes?sslmode=disable
TEST_DB_URL ?= postgres://notes:notes@localhost:5433/notes_test?sslmode=disable

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: test-deps
test-deps: ## Start Postgres and MinIO, and create the test database
	docker compose up -d postgres minio createbucket
	@until docker compose exec -T postgres pg_isready -U notes -d notes >/dev/null 2>&1; do sleep 1; done
	@docker compose exec -T postgres psql -U notes -d postgres -tAc \
		"select 1 from pg_database where datname='notes_test'" | grep -q 1 || \
		docker compose exec -T postgres psql -U notes -d postgres -c "create database notes_test"
	@echo "dependencies ready"

.PHONY: test
test: test-deps ## Run the full suite against a real database
	REQUIRE_TEST_DATABASE=1 TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... -count=1

.PHONY: test-unit
test-unit: ## Run only the tests that need no database
	go test ./internal/authz/... ./internal/auth/... ./internal/config/... ./internal/server/... -count=1

.PHONY: migrate
migrate: ## Apply migrations to the development database
	DATABASE_URL="$(DB_URL)" go run ./cmd/migrate up

.PHONY: migrate-status
migrate-status: ## Show migration state
	DATABASE_URL="$(DB_URL)" go run ./cmd/migrate status

.PHONY: generate
generate: ## Regenerate sqlc code from schema and queries
	sqlc generate

.PHONY: run
run: ## Run the API locally against the compose stack
	set -a && . ./.env.example && set +a && go run ./cmd/api

.PHONY: lint
lint: ## Vet and check formatting
	go vet ./...
	@unformatted=$$(gofmt -l . | grep -v '^internal/store/sqlcgen/' || true); \
	if [ -n "$$unformatted" ]; then echo "unformatted files:"; echo "$$unformatted"; exit 1; fi

.PHONY: down
down: ## Stop the local stack
	docker compose down

.PHONY: clean
clean: ## Stop the local stack and delete its data
	docker compose down -v
