# ARCHITECTURE ESSENTIALS — Concert Ticket Reservation System

> Quick-reference distillation of `ARCHITECTURE.md`. Read this before writing code; read the full document when you need the reasoning behind a rule.

## The one invariant

> **A given seat for a given event is confirmed to at most one booking, under any level of concurrency.**

Every rule below exists to protect this. If a change threatens it, the change is wrong.

## Services (5)

| Service | Owns | Store |
|---|---|---|
| **Gateway** | Routing, rate limiting, JWT validation, correlation IDs | — |
| **Auth** | Users, credentials, roles, refresh tokens | `auth_db` |
| **Booking** | Events, seat maps, **seat state, reservations, bookings, tickets** | `booking_db` + Redis |
| **Payment** | Charges, refunds, provider integration | `payment_db` |
| **Notification** | Async delivery (email), delivery log | `notif_db` |

## Critical decisions

**D1 — Seat catalog and reservations stay in ONE service.**
Reserving a seat is a mutation of seat state; the check and the write must be atomic. Splitting them turns a single DB transaction into a distributed transaction on the hottest, most contended path in the system, for zero benefit. **Do not split the Booking service.**

**D2 — Database per service. No shared schemas, ever.**
Cross-service data access goes through APIs or events. Never a direct query into another service's tables.

**D3 — Pessimistic locking, not optimistic.**
Contention concentrates on a few desirable seats. Optimistic (version-column) retry loops fail hardest exactly when load is highest. Use `SELECT ... FOR UPDATE` (P0) then Redis `SET NX EX` (P3).

**D4 — Never hold a DB transaction across an external call.**
Claim seats → COMMIT → *then* call Payment. A lock spanning a network call turns a 5 ms transaction into a multi-second one under load.

**D5 — Lock multi-seat requests in deterministic order (sorted by seat ID).**
Inconsistent lock ordering across code paths causes deadlocks.

**D6 — Holds are all-or-nothing.**
N requested seats: either all N are held, or none are and the request fails with `409 Conflict`. No partial holds.

**D7 — Orchestrated Saga, Booking as orchestrator.**
One readable place holding the flow beats distributed event chains for both correctness review and explicability.

**D8 — Never blind-release seats on a payment timeout.**
Unknown outcome ≠ failure. The charge may have succeeded. Leave the reservation `pending` and let the reconciliation job resolve it by idempotency key.

**D9 — Transactional outbox for all event publication.**
Write state change + outbox row in one DB transaction; a relay publishes. Prevents lost events on crash.

**D10 — All consumers are idempotent.**
At-least-once delivery means duplicates will arrive. Track processed `event_id`s.

**D11 — Idempotency keys on hold, confirm, charge, and refund.**
Without them, a retry is a double-charge or a double-book.

**D12 — Fail closed on Redis loss.**
If holds can't be taken safely, reject new holds. Never fall back to an unlocked path.

**D13 — Database-level backstop.**
A partial unique index enforcing one active claim per seat. If application logic is ever wrong, the database still refuses.

**D14 — The gateway validates access tokens locally; it never calls Auth per request.**
Signature, `exp`/`nbf`/`iat`, `iss`/`aud`, claim shape and role-vs-route are all decided from the token itself, with the algorithm pinned to HS256. What that cannot see — a user deleted, demoted or logged out since the token was minted — is bounded by the 15-minute access TTL and closed at the next refresh, which is a call to Auth by definition. Introspecting a token on every request would make Auth a hard dependency of every request in the system. See ARCHITECTURE.md §4.4.

## Tech stack (short form)

Go 1.22+ · gRPC + protobuf (internal, generated with `buf`) · REST/JSON (external) · PostgreSQL 16 · `pgx` + `sqlc` (no ORM — locking semantics must be visible) · Redis 7 · RabbitMQ · JWT HS256 + Argon2id · OpenTelemetry → Jaeger · Prometheus + Grafana · `zerolog` · Docker + Compose · `testcontainers-go` · `k6`

**Not used, deliberately:** Kubernetes, service mesh, Kafka, CQRS, event sourcing, ORM.

## Seat state machine

```
available ──hold──> held ──confirm──> booked ──refund──> available
    ▲                 │
    └──expire/release─┘
```
Transitions validated in `domain`. Illegal transition = domain error, never a silent no-op.

## Saga: happy path

`hold seats (atomic, commit)` → `call Payment (idempotency key)` → `payment.succeeded` → `seats held→booked, reservation→confirmed, issue ticket, outbox booking.confirmed` → `Notification sends ticket`

## Saga: compensation

| Failure | Action |
|---|---|
| Payment declined | Release seats, reservation → `failed` |
| Payment timeout (unknown) | Stay `pending`; reconciliation resolves by idempotency key (**D8**) |
| Hold expired | TTL/sweeper releases, reservation → `expired` |
| Paid but confirm failed | Reconciliation retries confirm; if unrecoverable → auto-refund |
| Refund requested | Payment refunds → Booking releases seats, booking → `refunded` |

## Internal layout (every service)

```
cmd/<service>/main.go
internal/domain/       ← entities, state machines. Imports NOTHING outward.
internal/usecase/      ← orchestration, transaction boundaries. Depends on interfaces.
internal/repository/   ← Postgres/Redis implementations
internal/transport/    ← grpc/, http/
internal/events/       ← publishers, consumers, outbox relay
migrations/  proto/
```
Dependencies point inward, always.

## Resilience checklist (every network call)

- [ ] Timeout set
- [ ] Retry with exponential backoff + jitter (idempotent operations only)
- [ ] Circuit breaker on the dependency
- [ ] Correlation ID propagated
- [ ] Failure path defined and tested

## Build order (do not skip ahead)

| Phase | Build | Exit criteria |
|---|---|---|
| **P0** | Booking monolith, Postgres `FOR UPDATE`, `held_until` column | **Concurrency test passes** |
| **P1** | Auth service + Gateway | Independent services, routing works |
| **P2** | Payment service + Saga | Payment failure demonstrably releases seats |
| **P3** | Redis TTL holds, multi-replica Booking | Holds expire without sweeper; concurrency test green with 2+ replicas |
| **P4** | Event bus + Notification | Killing Notification doesn't break booking |
| **P5** | Tracing, metrics, circuit breakers, DLQ | Request traceable end-to-end |

P0 must pass before any split. Correctness precedes distribution.

## The test that matters most

Fire 50+ concurrent hold requests at a single seat against a real Postgres (`testcontainers-go`). Assert **exactly one** succeeds. Write it in P0. Keep it green through every phase. Re-run it after every architectural change.