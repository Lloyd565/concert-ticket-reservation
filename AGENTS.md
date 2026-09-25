# AGENTS.md — Operating Guide for AI Coding Agents

Conventions, constraints, and workflow rules for agents working on the Concert Ticket Reservation System.

**Read first:** `ARCHITECTURE-ESSENTIALS.md`. Read `ARCHITECTURE.md` when you need reasoning. Read `PRD.md` for requirements and scope.

## 1. Prime directive

> **A given seat for a given event is confirmed to at most one booking, under any level of concurrency.**

Any change that weakens this is rejected, regardless of what else it improves. If you are unsure whether a change affects it, assume it does and run the concurrency test.

## 2. Non-negotiable rules

These correspond to decisions D1–D13 in `ARCHITECTURE-ESSENTIALS.md`. Violating one is a defect even if tests pass.

1. **Do not split the Booking service.** Seat catalog, seat state, and reservations stay together in one service with one database.
2. **Do not query another service's database.** Cross-service data access is via gRPC or events only.
3. **Do not hold a database transaction across an external network call.** Claim → commit → then call out.
4. **Do not introduce an ORM.** Locking semantics must be visible in SQL. Use `pgx` + `sqlc`.
5. **Always lock multiple seats in sorted order by seat ID.** Every code path, no exceptions.
6. **Always make holds all-or-nothing.** Partial holds are a defect.
7. **Never blind-release seats on a payment timeout.** Unknown ≠ failed. Leave `pending` for reconciliation.
8. **Never publish an event outside the transactional outbox.**
9. **Never write a non-idempotent consumer.**
10. **Never add a mutating endpoint without an idempotency key.**
11. **Never fall back to an unlocked path when Redis is unavailable.** Fail closed.
12. **Never commit secrets.** Config comes from environment variables only.
13. **Never skip build phases.** P0's concurrency test must pass before any service split (see `ARCHITECTURE-ESSENTIALS.md` build order).

## 3. Repository layout

```
/
├── PRD.md
├── ARCHITECTURE.md
├── ARCHITECTURE-ESSENTIALS.md
├── AGENTS.md
├── docker-compose.yml
├── Makefile
├── proto/                       # shared protobuf definitions
├── services/
│   ├── gateway/
│   ├── auth/
│   ├── booking/
│   ├── payment/
│   └── notification/
└── deploy/                      # prometheus, grafana, jaeger config, seed job
```

Each service directory:

```
services/<name>/
├── cmd/<name>/main.go           # wiring, config, graceful shutdown
├── internal/
│   ├── domain/                  # entities, state machines, domain errors
│   ├── usecase/                 # orchestration, transaction boundaries
│   ├── repository/              # Postgres/Redis adapters
│   ├── transport/{grpc,http}/
│   ├── events/                  # publishers, consumers, outbox relay
│   └── config/
├── migrations/
├── Dockerfile
└── go.mod
```

## 4. Layering rules

Dependencies point **inward only**.

| Layer | May import | Must never import |
|---|---|---|
| `domain` | stdlib only | anything else in the project |
| `usecase` | `domain`, repository *interfaces* | `repository` implementations, `transport`, database drivers |
| `repository` | `domain`, drivers | `usecase`, `transport` |
| `transport` | `usecase`, `domain` | `repository` |

Specifically:
- Seat state transition rules live in `domain`. Never inline them in a handler.
- Repository interfaces are declared in `usecase`, implemented in `repository`.
- Transaction boundaries are opened in `usecase`, never in `transport` or `domain`.
- No `database/sql` or `redis` imports outside `repository`.

## 5. Code conventions

**Go**
- Go 1.22+. `gofmt` and `goimports` clean. `golangci-lint run` must pass.
- Errors wrapped with context: `fmt.Errorf("claim seats: %w", err)`. Never discard an error silently.
- Domain errors are typed sentinels (`domain.ErrSeatUnavailable`) and mapped to transport codes at the boundary — never leak SQL errors to clients.
- `context.Context` is the first parameter of every function that does I/O. Every I/O call respects its deadline.
- Exported identifiers have doc comments. Unexported ones get comments when the *why* isn't obvious.
- Money is `int64` cents. Never a float.
- Timestamps are UTC, stored as `timestamptz`, serialized as RFC3339.
- IDs are UUIDv7 where ordering helps, UUIDv4 otherwise.

**Comments**
Annotate non-obvious logic, especially concurrency, locking, and saga control flow. State *why*, not *what*:

```go
// Lock in sorted order: inconsistent ordering across concurrent multi-seat
// requests is the classic deadlock scenario here (see ESSENTIALS D5).
sort.Slice(seatIDs, func(i, j int) bool { return seatIDs[i] < seatIDs[j] })
```

**SQL**
- Parameterized queries only. Never string concatenation.
- Locking queries carry a comment explaining the locking intent.
- Every migration is reversible and has a matching `.down.sql`.
- Never edit an applied migration — add a new one.

**Protobuf**
- One package per service under `proto/`.
- Fields are never renumbered or reused. Deprecate, don't repurpose.
- Regenerate stubs with `make proto` — never hand-edit generated files.

## 6. Commands

```bash
make up              # docker compose up, full stack + seed
make down            # tear down, remove volumes
make proto           # regenerate gRPC stubs
make migrate-up      # apply migrations (all services)
make migrate-down    # roll back last migration
make test            # unit tests, all services
make test-integration# integration tests (testcontainers; needs Docker)
make test-concurrency# THE critical test — run before every commit touching booking
make test-events     # outbox atomicity, relay confirms, a replayed event sends one email
make lint            # golangci-lint, all services
make load-test       # k6 concurrency scenario
```

If a command doesn't exist yet, add it to the `Makefile` rather than documenting a raw invocation.

## 7. Testing requirements

| Change type | Required tests |
|---|---|
| Domain logic | Unit tests covering valid and invalid transitions |
| Repository | Integration test against real Postgres/Redis via `testcontainers-go` |
| **Anything touching seat state** | **`make test-concurrency` must pass** |
| Saga logic | Integration test including forced payment failure and forced timeout |
| Consumer | Duplicate-delivery test proving idempotency |
| Endpoint | Contract test + auth/authorization test |

**Never mock the database in a test that validates locking or concurrency.** Mocks cannot exhibit race conditions; the test would prove nothing.

The concurrency test spins N goroutines requesting the same seat and asserts exactly one success and N−1 `ErrSeatUnavailable`. It must remain green across every phase.

## 8. Workflow expectations

**Before starting**
- Read `ARCHITECTURE-ESSENTIALS.md`.
- Identify which build phase the task belongs to. Do not implement a later phase's features early.
- Locate existing patterns in the codebase and follow them rather than inventing new ones.

**While working**
- Validate at small scale before scaling up: get one seat, one service, one path correct before generalizing.
- Prefer the simplest implementation that satisfies the invariant. Complexity requires justification in a comment or PR description.
- When a design decision isn't covered by the docs, state your assumption explicitly in the PR description rather than silently choosing.

**Before finishing**
- `make lint` clean.
- `make test` green.
- `make test-concurrency` green if seat state was touched.
- Migrations have down files.
- No new secrets, no new direct cross-service DB access, no new ORM.
- Update `ARCHITECTURE.md` if a boundary, contract, or decision changed. Update `ARCHITECTURE-ESSENTIALS.md` only if a *critical* decision changed.

**Commits**
Conventional commits, scoped by service:
```
feat(booking): add multi-seat atomic hold with sorted locking
fix(payment): make refund idempotent by provider reference
test(booking): raise concurrency test to 100 parallel holders
docs(architecture): record decision to keep catalog and reservations together
```

## 9. When to stop and ask

Raise these with the human rather than deciding alone:

- A task appears to require splitting the Booking service, or reads like it assumes a different service boundary.
- A requirement conflicts with the prime directive or with rules in §2.
- A fix would require holding a lock across a network call.
- The correct compensation for a new failure mode is genuinely ambiguous.
- A task requires a new infrastructure dependency not listed in the tech stack.
- The concurrency test fails and the cause isn't clearly in the code you changed — this may indicate a real architectural problem, not a flaky test.

**Never mark a task complete with the concurrency test failing or skipped.** Never disable, weaken, or reduce the parallelism of that test to make a change pass.

## 10. Common traps

| Trap | Correct approach |
|---|---|
| Check availability, then update in separate statements | Claim with `SET NX` (the taking *is* the check); at confirmation, `SELECT ... FOR UPDATE` then update, in one transaction |
| Multi-seat claims as separate Redis round trips | One Lua script: partial holds and cross-caller `DEL`s live in the gaps between round trips (ARCHITECTURE.md §5.2.1) |
| Locking seats in request order | Sort by seat ID first |
| Calling Payment inside the claim transaction | Commit first, then call |
| Treating a payment timeout as a failure | Leave `pending`; reconcile by idempotency key |
| Publishing an event right after `COMMIT` | Write to outbox inside the transaction; relay publishes |
| Holding outbox rows locked while publishing | Lease them in one statement, commit, then publish with nothing locked (D4) |
| Enqueueing an event on a context with no transaction | Refused on purpose. Open `WithinTx` around the state change and its event |
| Assuming exactly-once event delivery | Track processed `event_id`s |
| Retrying a charge without an idempotency key | Always pass one |
| Reintroducing a sweeper to expire holds | Redis TTL. The sweeper was retired in P3; a hold recorded in Postgres is a hold nothing expires without one. The P4 expirer moves a reservation's status and writes its event — never let it touch a seat, a key, or a reservation with `payment_pending_since` set |
| Assuming a TTL respects `payment_pending_since` | It does not. Push the hold out with `Saga.KeepHold` (D8) |
| Adding a "quick" cross-service DB read | Use gRPC or an event |
| Hiding `FOR UPDATE` behind an ORM helper | Explicit SQL, always |