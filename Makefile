# Root Makefile. Targets that have no tooling behind them yet say so instead of
# exiting 0 on nothing - a green stub is worse than a missing one.
BOOKING := services/booking

.PHONY: up down test test-integration test-concurrency lint migrate-up migrate-down proto

up:                     ## Start the local stack (P0: postgres + booking)
	docker compose up -d --build
	@echo "postgres + booking up. Seed job lands with the P0 seat-map migrations."

down:                   ## Tear down the stack and its volumes
	docker compose down -v

test:                   ## Unit tests, all services with a go.mod
	cd $(BOOKING) && go test ./...

test-integration:       ## Integration tests (testcontainers-go, needs Docker)
	@echo "not yet implemented - no repository layer or testcontainers harness yet (P0)"

test-concurrency:       ## THE critical test: N parallel holds on one seat, exactly one wins
	@echo "not yet implemented - write it in P0 with the seat claim; must never be faked green"

lint:                   ## golangci-lint, all services
	@echo "not yet implemented - golangci-lint not installed and .golangci.yml not written yet"

migrate-up:             ## Apply migrations (golang-migrate)
	@echo "not yet implemented - no migrations and no golang-migrate wiring yet (P0)"

migrate-down:           ## Roll back the last migration
	@echo "not yet implemented - no migrations and no golang-migrate wiring yet (P0)"

proto:                  ## Regenerate gRPC stubs from proto/
	@echo "not yet implemented - proto/ is empty; first contracts arrive with the gateway (P1)"
