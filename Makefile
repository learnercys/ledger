.DEFAULT_GOAL := help
BUF := go tool -modfile=tools/go.mod buf

.PHONY: help
help: ## Show this help
	@grep -E '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Start Postgres, Keycloak and the ledger service
	docker compose up -d --build --wait

.PHONY: down
down: ## Stop everything and delete data
	docker compose down -v --remove-orphans

.PHONY: demo
demo: ## Walk through the API with curl and real Keycloak tokens (needs `make up`)
	./scripts/demo.sh

.PHONY: test
test: ## Unit tests (no Docker needed)
	go test -race ./...

.PHONY: test-integration
test-integration: ## Postgres integration + property-based tests
	docker compose up -d --wait postgres
	go test -race -tags integration -count=1 ./internal/store/

.PHONY: bench
bench: ## Throughput: spread traffic vs one hot account
	docker compose up -d --wait postgres
	go run ./cmd/bench

.PHONY: generate
generate: ## Regenerate code from proto/
	$(BUF) lint
	$(BUF) generate

.PHONY: lint
lint: ## go vet, gofmt, buf lint
	go vet ./...
	go vet -tags integration ./...
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal && exit 1)
	$(BUF) lint
