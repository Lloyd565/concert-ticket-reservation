# Root Makefile. Targets that have no tooling behind them yet say so instead of
# exiting 0 on nothing - a green stub is worse than a missing one.
#
# Note: `make` lives in WSL on the dev machine this was built on, while Go and
# Docker live on the Windows side. Run the test targets from a shell that has
# both (Git Bash or PowerShell with make installed).

BOOKING := services/booking
AUTH    := services/auth
GATEWAY := services/gateway
PAYMENT := services/payment
# Every Go module in the repo, so `test` and `lint` cannot silently skip one.
MODULES := $(BOOKING) $(AUTH) $(GATEWAY) $(PAYMENT) proto
# Modules with integration tests, which need Docker for testcontainers.
INTEGRATION_MODULES := $(BOOKING) $(AUTH) $(PAYMENT)

MIGRATE := go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.1
BUF     := go run github.com/bufbuild/buf/cmd/buf@v1.57.2

# Local overrides come from .env; .env.example documents the keys.
ifneq (,$(wildcard .env))
include .env
export
endif
BOOKING_DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/booking_db?sslmode=disable
AUTH_DATABASE_URL ?= postgres://postgres:postgres@localhost:5433/auth_db?sslmode=disable
PAYMENT_DATABASE_URL ?= postgres://postgres:postgres@localhost:5434/payment_db?sslmode=disable

.PHONY: up down test test-integration test-concurrency test-saga lint migrate-up migrate-down proto sqlc

up:                     ## Start the local stack (P2: postgres, auth_db, payment_db, booking, auth, payment, gateway) and migrate
	docker compose up -d --build --wait
	$(MAKE) migrate-up

down:                   ## Tear down the stack and its volumes
	docker compose down -v

test:                   ## Unit tests (no infrastructure), all modules
	@for m in $(MODULES); do echo "== $$m"; (cd $$m && go test ./...) || exit 1; done

test-integration:       ## Integration tests against real Postgres (testcontainers; needs Docker)
	@for m in $(INTEGRATION_MODULES); do echo "== $$m"; (cd $$m && go test -tags=integration -count=1 ./...) || exit 1; done

test-concurrency:       ## THE critical test: 64 parallel holds on one seat, exactly one wins
	cd $(BOOKING) && go test -tags=integration -count=1 -v -run 'TestHoldSeatsConcurrent' ./internal/usecase/...

test-saga:              ## Saga compensation paths, including a forced payment timeout and a forced decline
	cd $(BOOKING) && go test -tags=integration -count=1 -v -run 'TestPay|TestReconcile|TestSweeper' ./internal/usecase/...
	cd $(PAYMENT) && go test -tags=integration -count=1 -v -run 'TestCharge|TestRefund|TestGetCharge|TestConcurrent' ./internal/usecase/...

lint:                   ## golangci-lint, all modules
	@for m in $(MODULES); do echo "== $$m"; (cd $$m && golangci-lint run ./...) || exit 1; done

migrate-up:             ## Apply migrations (booking, auth and payment)
	cd $(BOOKING) && $(MIGRATE) -path migrations -database "$(BOOKING_DATABASE_URL)" up
	cd $(AUTH) && $(MIGRATE) -path migrations -database "$(AUTH_DATABASE_URL)" up
	cd $(PAYMENT) && $(MIGRATE) -path migrations -database "$(PAYMENT_DATABASE_URL)" up

migrate-down:           ## Roll back the last migration of each service
	cd $(BOOKING) && $(MIGRATE) -path migrations -database "$(BOOKING_DATABASE_URL)" down 1
	cd $(AUTH) && $(MIGRATE) -path migrations -database "$(AUTH_DATABASE_URL)" down 1
	cd $(PAYMENT) && $(MIGRATE) -path migrations -database "$(PAYMENT_DATABASE_URL)" down 1

# buf carries its own protobuf compiler, so there is no protoc to install; the
# two code-generator plugins are Go binaries and are installed on demand.
proto:                  ## Regenerate gRPC stubs from proto/
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	cd proto && PATH="$$(go env GOPATH)/bin:$$PATH" $(BUF) generate
	cd proto && go mod tidy

sqlc:                   ## Regenerate typed query code from queries.sql
	cd $(BOOKING) && sqlc generate
	cd $(AUTH) && sqlc generate
	cd $(PAYMENT) && sqlc generate
