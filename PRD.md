# PRD — Concert Ticket Reservation System

## 1. Overview

A backend system that lets users browse concerts, select specific seats, hold them during checkout, pay, and receive a ticket. The defining constraint is **seat-level concurrency**: a seat is a unique, non-substitutable resource, and the system must guarantee that it is never sold twice — including during a high-traffic on-sale rush.

This is a portfolio/learning project. The goal is to demonstrate distributed backend engineering (service boundaries, distributed transactions, concurrency control, resilience, observability), not to ship a commercial product.

### Non-negotiable correctness property

> **Under any level of concurrency, a given seat for a given event is confirmed to at most one booking.**

Every design decision in this project is subordinate to that invariant. Features may be cut; this may not.

## 2. Target users

| Actor | Description | Primary needs |
|---|---|---|
| **Attendee** | End user buying tickets | Browse events, view a live seat map, select seats, pay within a predictable time window, receive a ticket |
| **Organizer** | Creates and manages events | Create an event, define a venue seat map and pricing tiers, view sales status |
| **Admin** | Operates the platform | Inspect bookings, issue refunds, resolve stuck reservations |
| **Reviewer (implicit)** | Engineer evaluating this project | Run the whole stack with one command, read clear docs, see tests that prove the concurrency guarantee |

The reviewer is a real audience here. Ease of local startup and demonstrable correctness are product requirements, not afterthoughts.

## 3. Scope

### In scope (v1)

- Registration, login, JWT-based auth with refresh tokens, role-based access (attendee / organizer / admin)
- Event creation with venue seat maps and pricing tiers
- Event and seat-availability browsing
- Seat selection with a time-boxed hold (checkout window)
- Payment via a mock/sandbox payment provider
- Booking confirmation with a generated ticket (QR/reference code)
- Async notifications (email) on booking confirmation, expiry, and refund
- Automatic release of abandoned holds
- Admin-initiated refund and cancellation

### Out of scope (v1)

- Real money movement / production payment provider certification
- Frontend application beyond a minimal seat-map demo UI
- Secondary market / ticket resale / transfer
- Dynamic pricing, promo codes, discounts
- Multi-currency, tax calculation, invoicing
- Ticket scanning / turnstile entry at the venue
- Mobile apps

### Explicitly deferred (v2 candidates)

- Virtual waiting room / queue for on-sale spikes
- Kubernetes deployment
- Seat recommendation ("best available N seats together")

## 4. Core user flows

### 4.1 Reservation flow (the critical path)

1. Attendee browses an event and views the seat map with current availability.
2. Attendee selects one or more seats and requests a hold.
3. System atomically claims the seats. If any requested seat is already taken, the whole request fails and no seats are held (all-or-nothing).
4. On success, the attendee receives a `reservation_id` and an explicit expiry timestamp. The checkout window is **10 minutes**.
5. Attendee submits payment against the `reservation_id`.
6. On payment success, seats become permanently `booked`, a ticket is issued, and a confirmation notification is dispatched.
7. On payment failure, timeout, or explicit cancellation, seats return to `available` and become immediately purchasable by others.
8. If the attendee abandons checkout, the hold expires on its own without any user or operator action.

### 4.2 Event setup flow

1. Organizer creates an event (name, venue, datetime).
2. Organizer defines the seat map — sections, rows, seat identifiers — and assigns each seat to a pricing tier.
3. Organizer sets the on-sale time. Before that time, hold requests are rejected.
4. Event goes on sale and appears in browse results.

### 4.3 Refund flow

1. Admin (or attendee, within a policy window) requests a refund on a confirmed booking.
2. Payment service issues a refund against the original charge.
3. On success, booking is marked `refunded` and its seats return to `available`.
4. Refund notification is dispatched.

## 5. Functional requirements

### FR-1 — Identity and access
- FR-1.1 Users register with email + password; passwords stored using a memory-hard hash (Argon2id or bcrypt).
- FR-1.2 Login issues a short-lived access token (15 min) and a long-lived refresh token (7 days).
- FR-1.3 Refresh tokens are revocable; logout invalidates them.
- FR-1.4 Every endpoint declares its required role. Organizer endpoints reject attendee tokens.

### FR-2 — Event and seat catalog
- FR-2.1 Organizers can create, update, and publish events.
- FR-2.2 A seat map is defined per event; each seat has a stable identifier, section, row, and pricing tier.
- FR-2.3 Seat maps are immutable once an event goes on sale.
- FR-2.4 Availability queries return per-seat status: `available`, `held`, `booked`.
- FR-2.5 Browse/list endpoints are paginated and filterable by date, city, and search term.

### FR-3 — Reservation and seat locking
- FR-3.1 A hold request for N seats succeeds only if **all** N are available; otherwise it fails atomically with no partial holds.
- FR-3.2 Concurrent hold requests for the same seat resolve so that exactly one succeeds. Losers receive a distinguishable "seat unavailable" error, not a generic 500.
- FR-3.3 Every hold carries an explicit expiry timestamp returned to the client.
- FR-3.4 Expired holds are released without operator intervention.
- FR-3.5 When multiple seats are locked in one operation, they are locked in a deterministic order (sorted by seat ID) to prevent deadlock.
- FR-3.6 A user may hold at most 6 seats at a time across all active reservations (anti-hoarding).
- FR-3.7 Hold, confirm, and release operations are idempotent — a retried request with the same idempotency key produces the same result without side effects.

### FR-4 — Payment
- FR-4.1 Payment is initiated against a `reservation_id` and validated against that reservation's amount and expiry.
- FR-4.2 Payment against an expired or already-confirmed reservation is rejected.
- FR-4.3 All charge requests carry an idempotency key; a retry never double-charges.
- FR-4.4 Payment provider calls are made with a timeout and bounded retries.
- FR-4.5 Payment state transitions are persisted before external calls (so an in-flight charge is recoverable after a crash).
- FR-4.6 Refunds reference the original charge and are also idempotent.

### FR-5 — Booking lifecycle and consistency
- FR-5.1 Booking states: `pending` → `confirmed` | `expired` | `failed` | `refunded`. Transitions are one-way and validated.
- FR-5.2 The reservation → payment → confirmation sequence is coordinated as a Saga with compensating actions (see ARCHITECTURE.md).
- FR-5.3 If payment succeeds but confirmation cannot be recorded, the system converges — either by completing the confirmation on retry or by refunding — and never leaves a paid-but-unbooked customer.
- FR-5.4 All state transitions emit domain events.

### FR-6 — Notifications
- FR-6.1 Notifications are dispatched asynchronously and never block the booking path.
- FR-6.2 Triggered on: booking confirmed, hold expired, payment failed, refund completed.
- FR-6.3 Failed sends are retried with backoff; permanently failed sends land in a dead-letter queue.
- FR-6.4 Notification delivery failure never rolls back a confirmed booking.

## 6. Non-functional requirements

### NFR-1 — Correctness
- NFR-1.1 Zero double-bookings under concurrent load. Enforced by an automated test that fires ≥50 concurrent hold requests at a single seat and asserts exactly one success.
- NFR-1.2 No seat is left permanently stuck in `held` after its expiry.
- NFR-1.3 No paid booking is left unconfirmed.

### NFR-2 — Performance (small-scale targets, measured locally)
- NFR-2.1 Seat availability read: p95 < 200 ms.
- NFR-2.2 Hold request: p95 < 500 ms.
- NFR-2.3 System sustains 200 concurrent hold attempts against one event without deadlock or unbounded lock waiting.

### NFR-3 — Resilience
- NFR-3.1 Payment provider unavailability degrades checkout only — browsing and holds keep working.
- NFR-3.2 Notification service being down does not affect booking.
- NFR-3.3 Synchronous inter-service calls use timeouts, bounded retries with exponential backoff and jitter, and a circuit breaker.
- NFR-3.4 Services start independently and in any order; no start-up ordering dependency.

### NFR-4 — Observability
- NFR-4.1 Structured JSON logs with a correlation ID propagated across all services.
- NFR-4.2 Distributed tracing spanning gateway → services → database.
- NFR-4.3 Metrics: request rate, error rate, latency per endpoint, hold success/conflict ratio, saga compensation count.
- NFR-4.4 Each service exposes `/health` (liveness) and `/ready` (readiness, including dependency checks).

### NFR-5 — Security
- NFR-5.1 No secrets in source control; all config via environment variables.
- NFR-5.2 Input validation at the service boundary; parameterized queries only.
- NFR-5.3 Rate limiting at the gateway, per IP and per authenticated user.
- NFR-5.4 The payment service is the only component that ever touches payment credentials.

### NFR-6 — Developer experience
- NFR-6.1 Entire stack starts with a single `docker compose up`.
- NFR-6.2 Seed data provides a ready-to-demo event with a populated seat map.
- NFR-6.3 Every service has a documented, runnable test suite.
- NFR-6.4 API contracts are documented (OpenAPI for external, protobuf for internal).

## 7. Success criteria

The project is complete when:

1. The concurrency test passes reproducibly — 50+ parallel requests for one seat, exactly one winner, every time.
2. A payment failure mid-Saga demonstrably releases the held seats, verifiable in logs and traces.
3. A distributed trace shows a single reservation request crossing gateway → booking → payment.
4. An abandoned checkout releases its seats automatically with no manual action.
5. `docker compose up` on a clean machine produces a working, seeded, demoable system.
6. A reader can follow ARCHITECTURE.md to understand why each service boundary exists.

## 8. Delivery phases

Each phase must be validated before the next begins. Complexity is added only after the simpler version is proven correct.

| Phase | Deliverable | Exit criteria |
|---|---|---|
| **P0** | Monolithic booking core: Postgres, `SELECT FOR UPDATE` seat claim, `held_until` timestamp column, no Redis, no other services | Concurrency test passes against the single service |
| **P1** | Split out Auth service; add API gateway; JWT validation at the edge | Both services run independently; gateway routes correctly |
| **P2** | Split out Payment service; implement the reservation Saga with compensation | Payment failure demonstrably releases seats |
| **P3** | Add Redis holds with TTL, replacing the timestamp-column approach; run multiple Booking instances | Holds expire without a cleanup job; concurrency test passes with 2+ Booking replicas |
| **P4** | Add event bus and Notification service | Notifications fire on confirmation; killing the notification service does not break booking |
| **P5** | Observability: tracing, metrics, dashboards; resilience: circuit breakers, DLQ | A single request is traceable end-to-end; a dead payment provider trips the breaker |

Phases P0–P2 constitute a defensible minimum. P3–P5 are what make the project stand out.

## 9. Key risks

| Risk | Impact | Mitigation |
|---|---|---|
| Splitting seat catalog from reservation state | Turns a simple DB transaction into a distributed one; likely correctness bugs | Keep both in the Booking service. Documented as a firm architectural decision. |
| Long-held DB locks across a payment call | Lock contention, timeouts under load | Never hold a DB transaction across an external call. Claim → commit → then call payment. |
| Deadlock on multi-seat holds | Requests hang under contention | Always lock seats in sorted order (FR-3.5). |
| Retry logic causing double charges | Real financial correctness bug | Idempotency keys on all payment operations (FR-4.3). |
| Over-engineering before correctness | Project stalls, core invariant unproven | Phased delivery; P0 must pass the concurrency test before any service split. |
| Scope creep into frontend work | Backend depth diluted | Frontend explicitly out of scope beyond a minimal demo seat map. |

## 10. Open questions

- Should attendees self-serve refunds within a policy window, or is refund admin-only in v1? *(Current assumption: admin-only, with a policy window as a stretch goal.)*
- Should the checkout window be extendable on user request? *(Current assumption: no — fixed 10 minutes, keeps expiry logic simple.)*
- Is the 6-seat-per-user limit enforced per event or globally? *(Current assumption: per event.)*