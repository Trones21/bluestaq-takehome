.DEFAULT_GOAL := help
SHELL := /bin/bash

DEV_DB_URL  ?= postgres://notes:notes@localhost:5433/notes?sslmode=disable
TEST_DB_URL ?= postgres://notes:notes@localhost:5433/notes_test?sslmode=disable

.PHONY: help
help: ## Show available targets
	@echo "Notes service — see README.md for the full walkthrough"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# --- local development ------------------------------------------------------

.PHONY: dev-up
dev-up: ## Start Postgres + MinIO and create both databases
	docker compose up -d postgres minio createbucket
	@until docker compose exec -T postgres pg_isready -U notes -d notes >/dev/null 2>&1; do sleep 1; done
	@docker compose exec -T postgres psql -U notes -d postgres -tAc \
		"select 1 from pg_database where datname='notes_test'" | grep -q 1 || \
		docker compose exec -T postgres psql -U notes -d postgres -c "create database notes_test" >/dev/null
	@echo "✅ postgres :5433, minio :9000 (console :9001)"

.PHONY: dev-down
dev-down: ## Stop the local stack, keeping data
	docker compose down

.PHONY: dev-reset
dev-reset: ## Stop the local stack and delete all data
	docker compose down -v

.PHONY: migrate
migrate: ## Apply migrations to the dev database
	DATABASE_URL="$(DEV_DB_URL)" go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	DATABASE_URL="$(DEV_DB_URL)" go run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: ## Show which migrations have been applied
	DATABASE_URL="$(DEV_DB_URL)" go run ./cmd/migrate status

.PHONY: run
run: ## Run the API (loads scripts/env/local.sh)
	@set -a && source scripts/env/local.sh && set +a && go run ./cmd/api

.PHONY: seed
seed: ## Load demo users, teams and notes
	@set -a && source scripts/env/local.sh && set +a && go run ./cmd/seed

# --- verification -----------------------------------------------------------

.PHONY: test
test: dev-up ## Run every test against a real database
	REQUIRE_TEST_DATABASE=1 TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... -count=1

.PHONY: test-unit
test-unit: ## Run only tests that need no database
	go test ./internal/authz/... ./internal/auth/... ./internal/config/... ./internal/server/... -count=1

.PHONY: flows-setup
flows-setup: ## Create the Python venv for the noCRUD flow runner
	cd nocrud && python3 -m venv .venv && ./.venv/bin/pip install -q -r requirements.txt
	@echo "✅ flow runner ready"

.PHONY: flows
flows: build ## Run every noCRUD flow (each provisions its own db + app)
	cd nocrud && ./.venv/bin/python noCRUD.py -req
	cd nocrud && ./.venv/bin/python noCRUD.py -crud

.PHONY: flows-list
flows-list: ## List the registered flows
	cd nocrud && ./.venv/bin/python noCRUD.py -l

.PHONY: smoke
smoke: ## Run the flows against a deployment (BASE_URL=...)
	./scripts/smoke.sh

.PHONY: ci
ci: ## Everything that must pass before a deploy
	./scripts/ci.sh

.PHONY: lint
lint: ## Vet and check formatting
	go vet ./...
	@unformatted=$$(gofmt -l . | grep -v '^internal/store/sqlcgen/' || true); \
	if [ -n "$$unformatted" ]; then echo "unformatted files:"; echo "$$unformatted"; exit 1; fi

.PHONY: generate
generate: ## Regenerate sqlc code from schema and queries
	sqlc generate

# --- deployment -------------------------------------------------------------

.PHONY: build
build: ## Build both binaries into ./bin
	CGO_ENABLED=0 go build -trimpath -o bin/api ./cmd/api
	CGO_ENABLED=0 go build -trimpath -o bin/migrate ./cmd/migrate

.PHONY: image
image: ## Build the production container image
	docker build -t notes-api:latest .

.PHONY: deploy
deploy: ## Run checks, then deploy to the host (prompts before shipping)
	./scripts/deploy.sh
