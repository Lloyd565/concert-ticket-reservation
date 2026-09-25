# ARCHITECTURE — Concert Ticket Reservation System

> Companion documents: `PRD.md` (what and why), `ARCHITECTURE-ESSENTIALS.md` (condensed decision reference), `AGENTS.md` (conventions for AI coding agents).

## 1. Architectural style

**Microservices**, chosen deliberately to demonstrate distributed systems competence: service boundaries, inter-service communication, distributed transactions, and the resilience patterns that only become necessary once calls cross a network.

The tradeoff is accepted knowingly. A modular monolith would be simpler and adequate for the traffic this project will ever see. Microservices are chosen because the *demonstration* of distributed patterns is a project goal, not because the load demands it.

That said, complexity is only introduced where it buys something. The most important architectural decision in this document is a decision **not** to split (§3.2).

## 2. System overview

```
                        ┌──────────────┐
                        │  Client apps │
                        └──────┬───────┘
                               │ HTTPS / REST
                        ┌──────▼───────┐
                        │ API Gateway  │  routing, rate limiting,
                        │              │  JWT validation, correlation ID
                        └──┬────┬───┬──┘
              ┌────────────┘    │   └────────────┐
              │ gRPC            │ gRPC           │ gRPC
       ┌──────▼──────┐  ┌───────▼───────┐  ┌─────▼────────┐
       │Auth Service │  │Booking Service│  │Payment Service│
       │             │  │  (seat state  │  │              │
       │             │  │   authority)  │  │              │
       └──────┬──────┘  └───┬───────┬───┘  └──────┬───┬───┘
              │             │       │             │   │
        ┌─────▼─────┐ ┌─────▼───┐ ┌─▼─────┐ ┌─────▼──┐│
        │ auth_db   │ │booking_ │ │ Redis │ │payment_││
        │ (Postgres)│ │   db    │ │(holds)│ │  db    ││
        └───────────┘ └─────────┘ └───────┘ └────────┘│
                            │                          │
                            │  publish       publish   │
                            └──────────┬───────────────┘
                                 ┌─────▼──────┐
                                 │ Event Bus  │
                                 │ (RabbitMQ) │
                                 └─────┬──────┘
                                       │ consume
                              ┌────────▼─────────┐
                              │Notification Svc  │
                              │  + notif_db      │
                              └──────────────────┘
```

Cross-cutting: OpenTelemetry collector, Jaeger (traces), Prometheus + Grafana (metrics), centralized structured logs.

## 3. Service boundaries

Boundaries are drawn along **consistency requirements** and **failure domains**, not along nouns in the domain model. Two things belong in the same service if they must change atomically.

### 3.1 Service catalog

| Service | Owns | Data store | Why it's separate |
|---|---|---|---|
| **API Gateway** | Routing, rate limiting, JWT signature validation, correlation ID injection | none | Single entry point; edge concerns don't belong in business services |
| **Auth Service** | Users, credentials, roles, refresh tokens | `auth_db` | Independent concern, near-zero coupling to booking, different security posture |
| **Booking Service** | Events, venues, seat maps, seat state, reservations, bookings, tickets | `booking_db` + Redis | The consistency core — see §3.2 |
| **Payment Service** | Charges, refunds, provider integration, payment state | `payment_db` | Distinct failure domain (external provider), distinct security posture (credentials), independent retry/idempotency logic |
| **Notification Service** | Notification templates, delivery attempts, delivery log | `notif_db` | Purely async, eventually consistent, must never block booking |

### 3.2 The critical non-split: catalog and reservations stay together

**Decision: seat catalog, seat availability state, and reservations live in a single Booking Service with a single database.**

The intuitive split — a "Catalog/Event Service" holding seat maps and a separate "Reservation Service" holding bookings — is wrong here, and it is the most common way this project gets built badly.

Reserving a seat *is* a mutation of that seat's state. "Check that seat A12 is available and mark it held" must be atomic. If catalog and reservations are separate services with separate databases:

- A single `SELECT ... FOR UPDATE` + `UPDATE` becomes a distributed transaction.
- Preventing double-booking now requires a Saga with compensation for the most contended operation in the entire system.
- Every seat claim pays a network round-trip while holding a lock.
- The core correctness invariant (NFR-1.1) becomes dramatically harder to prove.

All of that cost buys nothing: seats and reservations have identical scaling profiles, identical availability requirements, and always change together.

**Rule of thumb applied throughout:** if two pieces of data must be consistent within a single transaction, they belong in the same service.

Event metadata (name, venue, datetime, description) could technically live elsewhere, but is kept in Booking to avoid a cross-service join on the hottest read path — the seat map view.

### 3.3 Failure domain reasoning

| Service down | Consequence |
|---|---|
| Auth | New logins and refreshes fail; existing valid access tokens continue to work (gateway validates signature locally — see §4.4) |
| Booking | Core outage — browsing and reservations unavailable |
| Payment | Checkout fails; browsing and holds continue; holds expire naturally |
| Notification | Zero user-facing impact on booking; messages queue up and drain on recovery |
| Event bus | Booking and payment continue (events buffered in an outbox); notifications delayed |
| Redis | Holds unavailable → fall back to rejecting new holds rather than risking double-booking. **Fail closed, never open.** |

## 4. Communication patterns

### 4.1 Synchronous — gRPC (internal), REST (external)

- **Client → Gateway:** REST/JSON over HTTPS. Documented with OpenAPI.
- **Gateway → Services:** gRPC. Chosen for typed contracts via protobuf, better performance, and because defining service contracts explicitly is itself a demonstration goal.
- **Service → Service:** gRPC, used sparingly. Only Booking → Payment exists as a synchronous inter-service call, because the user is waiting on the result.

Every synchronous call carries: a deadline/timeout, bounded retries with exponential backoff + jitter, a circuit breaker, and a propagated correlation ID.

Contracts live in `proto/`, one package per service, and are compiled by `make proto`. Generated stubs are a shared Go module that each service depends on through a local `replace` directive; the consequence is that Docker build contexts are the repository root rather than the service directory.

Services defined as of P2:

| Contract | RPCs | Called by |
|---|---|---|
| `auth.v1.AuthService` | `Register`, `Login`, `Refresh`, `Logout` | Gateway |
| `booking.v1.BookingService` | `SeedEvent`, `HoldSeats`, `PayReservation` | Gateway |
| `payment.v1.PaymentService` | `Charge`, `GetCharge`, `Refund` | Booking |

Booking exposes gRPC *in addition to* its REST handler, not instead of it. Both transports call the same use case; neither reimplements a seat-state rule. `HoldSeatsRequest.user_id` is set by the gateway from the validated token's subject and overwrites whatever the client sent — Booking is not the component that decides who the caller is.

Booking's call to Payment carries a deadline and a circuit breaker (`services/booking/internal/breaker`) from P2, because the saga's compensation logic depends on being able to tell "no" apart from "we do not know" and a breaker's fast-fail is one of the ways "we do not know" arises. It carries no retry: the safe place to retry a charge is inside Payment, where the idempotency key is already being passed through to the provider.

Retries and circuit breakers on the gateway's outbound calls are deferred to P5 with the rest of the resilience work. A retry added before the saga's idempotency story is complete would be a way to double-book, not a way to be resilient. Deadlines and correlation-ID propagation are in place from P1.

### 4.2 Asynchronous — RabbitMQ

Used for anything the caller doesn't need an immediate answer to. Topic exchange, durable queues, publisher confirms.

**Events published:**

| Event | Publisher | Consumers |
|---|---|---|
| `reservation.held` | Booking | (audit/metrics) |
| `reservation.expired` | Booking | Notification |
| `booking.confirmed` | Booking | Notification |
| `booking.refunded` | Booking | Notification |
| `payment.succeeded` | Payment | Booking (saga), Notification |
| `payment.failed` | Payment | Booking (saga) |
| `refund.completed` | Payment | Booking |

**Event envelope (all events share this shape):**

```json
{
  "event_id": "uuid",
  "event_type": "booking.confirmed",
  "occurred_at": "RFC3339 timestamp",
  "correlation_id": "uuid",
  "version": 1,
  "payload": { }
}
```

Consumers must be **idempotent** — at-least-once delivery means duplicates will happen. Each consumer records processed `event_id`s and skips repeats.

**As built in P4**, the only consumer is Notification. The saga stays orchestrated over gRPC (D7), so Booking does not consume `payment.*`; and Booking consuming `refund.completed` waits for a refund-request flow, which does not exist yet (§6.2 row 5). Every event in the table is still published.

**Topology.** One durable topic exchange, `concert.events`, with the event type as routing key; every publisher and consumer declares it, so no start order matters. Each consumer owns and declares its own durable queue, its bindings and its dead-letter queue — Notification's are `notification.events` (bound to `booking.confirmed`, `reservation.expired`, `booking.refunded`) and `notification.dlx` → `notification.dlq`. An event no queue is bound to is confirmed by the broker and dropped, which is correct: nobody has asked for it.

A queue declared by its consumer exists only after that consumer has started once, and an event published before then is unroutable. Compose waits for Notification's readiness, and ready means consuming, so on every normal start the queue exists before anything can be booked. Loading the topology from a broker definitions file is the upgrade if Notification must be able to miss the broker's very first boot.

### 4.3 Transactional outbox

A service must never write to its database and publish to the broker as if they were one atomic operation — they aren't, and a crash between them silently loses the event.

Pattern: within the same DB transaction as the state change, insert the event into an `outbox` table. A separate relay polls unpublished outbox rows, publishes them to RabbitMQ, and marks them sent. This guarantees at-least-once publication with no lost events.

Applied in: Booking and Payment.

**The crash this exists for.** `ConfirmPaid` commits the booking — seats booked, reservation confirmed, tickets issued — and the process dies before its publish of `booking.confirmed` reaches the broker (a deploy's SIGKILL, an OOM kill). Nothing afterwards knows an event was owed: no error was logged by the dead process, a retried `Pay` answers from the existing booking, and reconciliation never scans a confirmed reservation. The customer has paid and never receives their tickets, and no table, log or metric shows it. Publishing before `COMMIT` swaps the failure for a phantom one — tickets emailed for a booking that rolled back — and publishing without confirms loses an event whenever the broker dies with it still in a buffer.

**Joining the transaction.** Booking's `Repo.Enqueue` takes the ambient transaction from the context and *refuses* to run without one, rather than falling back to the pool: an event written on its own connection is exactly the two-step write this rules out. Payment has no transactions (every write is one statement), so the settle `UPDATE` and the outbox `INSERT` are one statement — a data-modifying CTE whose `INSERT` selects from the `UPDATE`. The event exists exactly when the settlement does, and a duplicate that loses the `status = 'pending'` guard announces nothing.

| Event | Written by, in the same transaction or statement |
|---|---|
| `reservation.held` | `Holder.HoldSeats`, with the reservation and its seats |
| `booking.confirmed` | `Saga.ConfirmPaid`, with the booking and its tickets — the payload carries the tickets |
| `booking.refunded` | `Saga.ReleaseRefunded` |
| `reservation.expired` | `Expirer.ExpireOnce`, with the `pending → expired` update (§6.2) |
| `payment.succeeded` / `payment.failed` | `SettleCharge` — never for a charge left `pending` (D8) |
| `refund.completed` | `SettleRefund`, for a succeeded refund only |

A release on decline or on "no charge" publishes nothing: the customer learned the outcome synchronously, and no consumer needs it.

**The relay** (`services/{booking,payment}/internal/events/relay.go`) runs once per process, every second:

1. Connect first. A relay that cannot reach the broker leases nothing, so it never holds rows back from one that can.
2. Lease a batch: one statement sets `claimed_until = now() + 30s` on up to 100 unpublished, unleased rows and commits. `FOR UPDATE SKIP LOCKED` lives inside that statement only.
3. Publish the batch with nothing locked — a transaction may not span the broker call (D4) — as persistent messages, `message_id` = event ID, with publisher confirms.
4. Mark only the rows the broker acked. A nack, a timeout or a dropped channel leaves the row leased until the lease lapses, and it is published again.

The lease, not a row lock, is what keeps two Booking replicas from both publishing a row, and it is measured on the database clock so replica skew cannot shorten it.

| Failure | Outcome |
|---|---|
| Crash before `COMMIT` | Neither the change nor its event exists |
| Crash after `COMMIT`, before publish | The row waits; the next pass publishes it |
| Crash after publish, before the confirm | Unmarked; published again once the lease lapses — a duplicate |
| Crash after the confirm, before the mark | The same duplicate |
| Broker down | Nothing is leased; rows accumulate; no request fails (§3.3) |

Publication is therefore at-least-once and never at-most-once, and D10 is what makes the duplicates harmless. Within one batch events go out oldest first; across two replicas' batches they can interleave, so consumers must not depend on order. Published rows are kept — a retention job is the upgrade when the table's size matters.

### 4.4 Access-token validation is local to the gateway

**Decision: the gateway validates access tokens itself and never calls Auth to check one.**

The alternative — a token-introspection call to Auth on every request — makes Auth a hard synchronous dependency of every request in the system. One Auth restart becomes a total outage, and every request pays a network round trip on the hot path. That is the opposite of the failure isolation §3.3 claims.

**Checked locally, on every request:**

| Check | Why it needs no network call |
|---|---|
| HS256 signature | The MAC is over the bytes the client presented; the secret is already held |
| `exp`, `nbf`, `iat` | Time is local |
| `iss`, `aud` | A token minted by another system with the same secret is not a token for this API |
| Structure and required claims (`sub`, `role`) | Present in the token |
| Role vs. route (organizer/admin for event seeding) | The role is a signed claim |

Algorithm pinning is mandatory, not incidental: without it a token names its own algorithm, and both `alg: none` and the RS256-verified-as-HS256 confusion become forgeries the gateway accepts.

**Not knowable locally:** whether the user was deleted, demoted or logged out since the token was minted.

**Why that is acceptable.** The staleness window is bounded by the access-token TTL — 15 minutes — and applies only to a token issued before the change. Refresh tokens are the stateful half: they are opaque, stored hashed in `auth_db.refresh_tokens`, and checked against that table on every refresh. Revoking one therefore ends the session within one access-token lifetime with no per-request call. Rotation on refresh means a replayed refresh token is detectable, and a detected replay revokes every live session for that user.

The cost is 15 minutes of stale authorization. The purchase is that Auth is not on the critical path of any request except the ones that are addressed to it. If sub-minute revocation is ever required, the upgrade is a small revocation set (user IDs plus a not-before instant) pushed to gateways — still not a synchronous call.

**Fine-grained authorization stays downstream.** The gateway decides *who* the caller is and whether their role may reach a route; it never decides whether they own a particular reservation. That question belongs to the service that owns the data.

Symmetric HS256 is used because the only verifier is the gateway, inside the same trust boundary. It does mean the gateway holds a key that can mint tokens; the move to RS256/EdDSA with a JWKS endpoint is warranted as soon as anything that should only verify needs the key.

### 4.5 Consuming events: Notification

Notification consumes `notification.events` with one consumer and a prefetch of 1, and every message goes through the same steps (`internal/usecase/notify.go`):

1. **Has this event been processed?** A row in `processed_events` means yes: acknowledge and do nothing. This is asked before anything else, because duplicates are the normal case — the relay republishes after a crash between confirm and mark, and the broker redelivers whatever was not acknowledged.
2. **Render** the email. Its notification ID *is* the event ID, so a redelivered event resumes its notification, and its attempt count, instead of starting another.
3. **Send**, and on success record the attempt, mark the notification sent and insert `processed_events` in **one statement** — "sent" and "processed" cannot disagree.
4. On failure, log the attempt and retry in process with exponential backoff and jitter (0.5s doubling, 5 attempts), the message still unacknowledged.

The handler's error decides the message's fate: none acknowledges it; a permanent one — undecodable, an unknown event type, a malformed ID, or out of attempts — rejects it without requeue into `notification.dlq`, so a poison message never blocks the queue; anything else (the database is down, the service is shutting down) requeues it after a short delay. A dead-lettered event is not recorded as processed, so moving it back from the DLQ once delivery works sends the email.

Two windows are left open deliberately, and both are marked in the code. A crash between the send and the statement that records it resends once on redelivery; closing it needs a provider that honours the notification ID as an idempotency key, as Payment's does. And duplicates are caught when they arrive in sequence, which one consumer with prefetch 1 guarantees; two replicas could each take a copy of one event at the same instant, so scaling out needs a lease on the notification row first, the relay's pattern.

The mock mail provider addresses users by ID. Addresses live in `auth_db`, which Notification may not read (D2), and no event carries one yet.

## 5. Seat locking design

This is the technical heart of the system. See PRD §4.1 and FR-3.

### 5.1 The problem

Check-then-act race (TOCTOU): two requests read "seat available" before either writes, both proceed, both book. Application-level checks cannot fix this — the check and the act must be atomic at the storage layer.

### 5.2 Phased implementation

**Phase P0 — Postgres pessimistic locking (build this first)**

```sql
BEGIN;

-- Lock target rows in deterministic order to prevent deadlock (FR-3.5).
-- Any concurrent transaction attempting the same rows blocks here until commit.
SELECT id, status FROM seats
WHERE event_id = $1 AND id = ANY($2::uuid[])
ORDER BY id
FOR UPDATE;

-- Application verifies ALL requested seats are 'available'.
-- If any is not -> ROLLBACK, return 409 Conflict (all-or-nothing, FR-3.1).

UPDATE seats
SET status = 'held',
    held_by_reservation = $3,
    held_until = now() + interval '10 minutes'
WHERE id = ANY($2::uuid[]);

INSERT INTO reservations (...) VALUES (...);
INSERT INTO outbox (...) VALUES (...);  -- reservation.held

COMMIT;
```

Expiry in P0 is handled by a background sweeper that releases seats where `held_until < now()`.

**Hard constraint: the transaction must not span an external call.** Claim, commit, release the lock — *then* call the payment service. A lock held across a network call is how you turn a 5 ms transaction into a 5 s one under load.

**Phase P3 — Redis holds with TTL (built)**

The timestamp-column approach has two limits, and the second is the one that blocks replication: expiry accuracy is bounded by the sweeper interval, and the sweeper adds load that grows with hold volume. Worse, *somebody* has to run it — which makes a second Booking replica a question rather than a shrug. Redis replaces it:

```
SET seat:hold:{event_id}:{seat_id} {reservation_id} NX EX 600
```

`NX` makes the claim itself atomic (set only if absent — this *is* the lock). `EX 600` makes abandonment self-healing with no sweeper, no ticker, and no replica having to be the one that owns expiry. That is what makes Booking replicable: two replicas share one Redis and one Postgres and coordinate through nothing else.

**Multi-seat holds run as a Lua script** (`services/booking/internal/repository/redis/holds.go`). Redis executes a script to completion with nothing interleaved, so the loop of `SET NX`s and the rollback that undoes them are one indivisible step. Issuing the same commands one at a time from Go would not be: between two round trips another caller can take the seat the first round trip did not reach yet, and the "release what we already took" path becomes a second window in which somebody else's key can be deleted. See §5.2.1.

**What lives where.** Postgres is the source of truth for *confirmed* bookings; Redis holds transient state only. The `seats` row therefore has two states, `available` and `booked` — `held` is not representable, deliberately, because a hold recorded in Postgres is a hold nothing expires without a sweeper. On confirmation: write the booking to Postgres in one transaction, then delete the Redis key.

The ordering of the two stores is load-bearing in both directions:

- **Holding** claims the Redis key *first*, then reads the seat catalog. Confirmation commits `booked` and only then deletes the key, so a claim won just after that delete is guaranteed to read `booked`. Reading first and claiming second would reopen exactly that window.
- **Confirming** checks that the reservation still owns its keys *before* opening the transaction (a network call may not happen inside one — D4), then takes `SELECT ... FOR UPDATE` on the seats. The Redis check is what replaces P0's "the sweeper already released these seats"; the row lock is what actually keeps the invariant if that check goes stale.

**Fail closed (D12).** Every method on the hold store returns `domain.ErrHoldsUnavailable` when Redis cannot be reached, and there is no fallback path to Postgres locking. A hold taken without the lock is a double-booking waiting for its second customer. Readiness fails too, so a replica that cannot claim seats leaves the rotation rather than answering 503 to every request.

**D8 against a TTL.** Retiring the sweeper does not retire the second way to violate D8, it changes who carries it — see §6.2.

**D13 restated.** The backstop index used to mean "one live claim per seat", which cannot survive here: an abandoned hold expires in Redis with no writer left to stamp its Postgres claim released, so a stale claim would block the seat forever. It is now `UNIQUE (seat_id) WHERE confirmed_at IS NOT NULL AND released_at IS NULL` — one *confirmed* claim per seat, which is the invariant word for word. Unconfirmed claims are inert.

#### 5.2.1 Why a Lua script and not three round trips

A three-seat hold issued as separate commands is three separate moments, and both of the failure modes it introduces are silent.

Take two callers wanting seats {A, B} and {B, C}. Caller 1 takes A. Caller 2 takes B. Caller 1 tries B and fails. Caller 2 tries C and succeeds. Caller 1 must now roll back, releasing A — which is correct — but the interleaving has already produced a partial hold that existed, was visible to everyone, and briefly made A unavailable to a caller who would have got it. With enough contention on a popular row, two callers can also fail each other's second seat and both roll back, leaving neither with seats that were free the whole time: livelock, not deadlock, and it does not show up as an error anywhere.

The rollback itself is the sharper problem. Between caller 1 failing on B and caller 1 deleting A, the TTL on A can expire and caller 3 can claim it. Caller 1's `DEL A` then deletes caller 3's hold, and caller 3 walks to checkout holding a seat the store no longer believes is theirs — which a fourth caller can now take. A compare-then-delete from the application does not fix it either; the compare and the delete are themselves two round trips with the same gap between them.

Inside a script both disappear, because there is no "between". Redis runs the script to completion single-threaded: no other client observes A held while B is being attempted, no other client can take A during the rollback, and the whole claim is one atomic decision — which is what D6 asks for and what a sequence of round trips cannot give.

**Why not optimistic locking?** A version-column approach suits low contention spread across many rows. Ticketing is the opposite: thousands of users contend for the same handful of good seats. Optimistic locking would produce a high retry-failure rate exactly when it matters most. It is the wrong tool for this access pattern.

### 5.3 Seat state machine

```
available ──hold──> held ──confirm──> booked ──refund──> available
    ▲                 │
    └──expire/release─┘
```

Since P3 the `held` state lives in Redis, not on the seat row: `hold`, `expire` and `release` are writes to a key, and the Postgres row moves straight from `available` to `booked` when a hold is redeemed. The `domain.Seat` state machine models the row, so it has the two states the row has. Illegal transitions raise a domain error, never a silent no-op.

## 6. Reservation Saga

The reservation → payment → confirmation sequence spans Booking and Payment. It is coordinated as an **orchestrated Saga** with Booking as orchestrator (chosen over choreography because a single readable place holding the flow is more valuable, and more explicable to a reviewer, than distributed event chains).

### 6.1 Happy path

1. **Booking** claims seats atomically → state `held`, reservation `pending`.
2. **Booking** commits and releases the DB lock.
3. **Booking** calls **Payment** (gRPC) with `reservation_id`, amount, and an idempotency key.
4. **Payment** persists a `pending` charge, calls the provider, persists the result.
5. **Payment** returns success and publishes `payment.succeeded`.
6. **Booking** transitions seats `held → booked`, reservation → `confirmed`, issues a ticket, writes `booking.confirmed` to the outbox.
7. **Notification** consumes `booking.confirmed` and sends the ticket.

### 6.2 Compensation paths

| Failure point | Compensating action |
|---|---|
| Payment declined | Release seats → `available`, reservation → `failed`, publish `payment.failed` |
| Payment call times out (result unknown) | Leave reservation `pending` — it was marked for reconciliation before the call, so this holds even if Booking dies mid-call; reconciliation job queries Payment by idempotency key. If charged → complete confirmation. If not → release seats. **Never blind-release on timeout** — the charge may have succeeded |
| Hold expires before payment | The Redis TTL releases the seats, unaided, and the saga refuses to charge against a closed window (FR-4.2). Since P4 the expirer then moves the reservation `pending → expired` and writes `reservation.expired` in that transaction — a status and nothing else: no seat, no key, and never a reservation whose payment outcome is unknown (D8). See "The expirer is not the sweeper" below |
| Payment succeeded but confirmation write fails | Reservation stays `pending`; reconciliation retries confirmation. If unrecoverable → automatic refund. Satisfies FR-5.3: never a paid-but-unbooked customer |
| Refund requested on confirmed booking | Payment refunds → publishes `refund.completed` → Booking releases seats, booking → `refunded` |

Each row is a separate named function in `services/booking/internal/usecase/saga.go`, not a branch of one handler: `ReleaseDeclined`, `LeavePendingForReconciliation`, `RefundUnconfirmable`, `ReleaseRefunded`. Row 3's seat release has no function at all — it is the Redis TTL; its status change and event are `Expirer` in `expire.go`. Row 5's *trigger* is the `refund.completed` event: Payment publishes it from P4, but nothing in Booking consumes it yet, because no flow exists that refunds a *confirmed* booking. Its action half was built in P2 because row 4 is unfinished without it, and writes `booking.refunded` since P4.

**The second way to violate D8.** The saga leaving a timed-out reservation `pending` is only half the rule, and the other half changed shape in P3.

In P0 the danger was the sweeper: it released any `pending` reservation past `expires_at`, and from its point of view one of these was just an abandoned checkout, so left alone it would blind-release the seats ten minutes later through a different code path. The guard was a `WHERE` clause — `reservations.payment_pending_since IS NULL` — in one query.

Retiring the sweeper does not retire the danger. The Redis TTL now does exactly what the sweeper did, and there is no `WHERE` clause to add to it: it is not running any SQL and nobody asks it anything. So the guard has to be an *action* rather than a condition. `Saga.KeepHold` pushes the hold out by another checkout window — once when the outcome first becomes unknown, and again on every reconciliation pass that fails to resolve it, because one extension is good for one window and the case this exists for is Payment being unreachable for longer than that. `reservations.payment_pending_since` still marks the reservation, and reconciliation is still the only owner of those seats, which is why its interval must stay well inside the hold TTL; Booking refuses to start if it does not.

Deleting that extension is a one-line change that breaks nothing visible and sells a paid customer's seat ten minutes later, so it has a test of its own (`TestHoldIsKeptAliveWhileThePaymentOutcomeIsUnknown`).

**The expirer is not the sweeper.** P3 left a lapsed reservation `pending` with a past deadline, and that was right for seats. P4 needs `reservation.expired`, and an event needs a transaction to be written in (D9): emitting one for a reservation nothing updates is the lost-event write the outbox forbids. So `Expirer` moves `pending → expired` and records the event in that transaction. It differs from the retired sweeper in every way that mattered: it releases no seat and touches no Redis key — the TTL still owns the hold; it runs on every replica, because each row is claimed with `FOR UPDATE SKIP LOCKED` and guarded by its status in one statement; and it is late by up to a pass without consequence, because only an email waits on it.

It is also the third place D8 has to hold. The statement skips any reservation with `payment_pending_since` set: expiring one would make a possibly-paid reservation one confirmation refuses, turning a charge that succeeded into a guaranteed refund. The guard races Pay safely in both directions — a mark committed first excludes the row, and an expiry committed first fails Pay's mark, which only counts a `pending` row, so no charge starts (`TestExpirerLeavesReservationsReconciliationOwns`).

**A charge that succeeded but could not be confirmed** — the database was briefly unreachable, Redis was, the seats had already gone — is handed to reconciliation rather than returned as an error nobody follows up. There is a charge with no delivery, and reconciliation is the only component that resolves that: it asks Payment, gets `succeeded`, retries the confirm, and refunds if confirmation is genuinely impossible (row 4, FR-5.3).

### 6.3 Reconciliation job

A periodic job scans reservations stuck in `pending` past a grace period and resolves each against the Payment service by idempotency key. This is what makes the "unknown outcome" case converge, and it is the difference between a Saga that looks right in a diagram and one that is actually correct.

The charge's idempotency key is derived from the reservation ID (`reservation:<id>`) rather than chosen by the client. Two consequences: two different client keys for one reservation are still one charge, and the job can ask Payment what happened without Booking having stored anything at the moment things went wrong — which is precisely the moment a write is least likely to have succeeded.

Payment answers with one of four things, each with exactly one correct action:

| Answer | Action |
|---|---|
| `succeeded` | Confirm. If confirmation is impossible, refund (§6.2 row 4) |
| `declined` / `failed` | Release the seats, reservation → `failed` (§6.2 row 1, resolved late) |
| no charge under this key | Release the seats — the only evidence that makes releasing safe after a timeout — and queue the reservation to be asked about once more after the grace period; a charge found then is refunded (see below) |
| `pending` | Payment holds a charge it never settled. **Re-drive it** under the same key, which settles it into one of the three above |

The last row is what makes the job converge rather than poll. A charge left `pending` — Payment wrote the row, then its own provider call was interrupted — is not going to settle on its own: nothing else re-drives it, nothing else may release the reservation, and the customer is left with a hold that never resolves. So reconciliation calls `Charge` again rather than only `GetCharge`. Payment resumes the existing charge instead of creating a second one and passes the same key to the provider, so no money moves twice (FR-4.5).

**Compensation runs on a detached context.** Everything the saga writes after the payment call — the reconciliation mark, the confirmation, the release — uses `context.WithoutCancel` with its own deadline. The moment those writes are most likely to be skipped is when the request context has already been cancelled (the gateway's deadline fired, the customer closed the tab), which is exactly the moment a charge is most likely to be in flight. Inheriting that cancellation leaves the reservation pending with nothing marking it and its hold never extended, and the TTL then releases seats that may have been paid for: D8 violated by way of a context, with no code path that looks wrong.

**The mark is written before the call, not after it fails.** A mark written in response to a failed call needs Booking to survive the call. If Booking dies mid-call nothing marks the reservation, this job never scans it, and once the window closes the saga refuses the customer's retry (FR-4.2): a charge that went through is never delivered and never refunded. Marked first, a crash anywhere after the mark leaves a reservation this job will find, and a mark that cannot be written stops the call.

**"No charge" is asked twice.** Payment's "no charge under this key" is true when it is said. A pay retry already past the mark can reach Payment after the job has released the seats on that answer, and take the money for a reservation that is now `failed` — which the pending-only scan never looks at again. Two things close it. The mark is a fence: it counts the row only while the reservation is `pending`, and the saga calls Payment only if it did, so no charge can start after a release commits. And that release queues the reservation in `charge_rechecks` in the same transaction; once the grace period has passed — longer than one payment call, which Booking refuses to start without — the job asks again. A charge found then is refunded (§6.2 row 4), a `pending` one is re-driven first, and none found closes the entry. Only the "no charge" release is queued: a declined or failed key is settled for good, and Payment answers every later charge under it from that row.

Payment applies the detached-context rule one layer down: once the provider has answered, the settle write ignores the caller's cancellation. Otherwise a caller giving up a moment too early turns a recorded approval into a `pending` row that only a re-drive — and the provider's replay of its key — can recover.

**Deadlines nest.** `PAYMENT_PROVIDER_TIMEOUT` (1s) is per attempt, and both attempts plus backoff sit inside `BOOKING_PAYMENT_TIMEOUT` (3s), which gRPC carries into Payment — at 3s per attempt the first attempt used the whole deadline and the retry never ran. `BOOKING_PAYMENT_TIMEOUT` in turn sits inside `GATEWAY_UPSTREAM_TIMEOUT` (5s). If the gateway gives up first, the customer gets a 504 instead of the `202 Accepted` that tells them the payment is still settling for a reservation that is alive and being reconciled.

## 7. Tech stack

| Layer | Choice | Rationale |
|---|---|---|
| **Service language** | Go 1.22+ | Strong concurrency primitives (directly relevant to this domain), first-class gRPC support, small containers, fast start-up. Swap for NestJS/TypeScript if preferred — the architecture is unchanged; keep one language across services to reduce operational surface |
| **HTTP framework** | stdlib `net/http` (Go 1.22 routing patterns) | Method-and-path patterns cover what these services need; `chi` earns its place when middleware ordering does, not before |
| **Internal RPC** | gRPC + protobuf | Typed contracts, versionable schemas, demonstrates explicit contract design |
| **Protobuf codegen** | `buf` (via `go run`) | Carries its own compiler, so there is no `protoc` binary to install; `make proto` works from a clean checkout with only Go present |
| **API Gateway** | Custom Go service (or Kong/Traefik) | A custom gateway shows understanding of what a gateway actually does; a managed one saves time. Prefer custom for portfolio value |
| **Primary database** | PostgreSQL 16 | Row-level locking, strong transactional guarantees, `SELECT FOR UPDATE`. One database per service, no shared schemas |
| **DB access** | `pgx` + `sqlc` | Explicit SQL (locking semantics must be visible, not hidden by an ORM), with generated type-safe Go |
| **Migrations** | `golang-migrate` | Versioned, per-service, checked into the repo |
| **Cache / holds** | Redis 7 | Atomic `SET NX`, TTL-based expiry, Lua for multi-key atomicity |
| **Message broker** | RabbitMQ | Simpler operationally than Kafka; topic routing, DLQ, publisher confirms all built in. Kafka is over-provisioned for this scale |
| **Auth** | JWT HS256 (15 min access + 7 day refresh), Argon2id hashing | Stateless validation at the gateway, so Auth is not on the critical path of every request (§4.4). Refresh tokens are opaque and stored hashed, which is what makes revocation possible at all |
| **Payment provider** | Sandbox/mock adapter behind an interface | Real money is out of scope; the interface makes the integration pattern demonstrable and testable |
| **Tracing** | OpenTelemetry → Jaeger | Cross-service request tracing is mandatory once requests span services (NFR-4.2) |
| **Metrics** | Prometheus + Grafana | RED metrics per service plus domain metrics (hold conflict ratio, saga compensations) |
| **Logging** | `zerolog`, JSON, correlation ID on every line | Structured logs are the only tractable way to debug multi-service flows |
| **Containerization** | Docker, multi-stage builds, distroless base | Small images, fast rebuilds |
| **Local orchestration** | Docker Compose | Single-command startup (NFR-6.1) |
| **CI** | GitHub Actions | Per-service lint, test, build, image push |
| **Testing** | `testing` + `testify`, `testcontainers-go` | Real Postgres/Redis in integration tests — the concurrency guarantee cannot be validated against mocks |
| **Load testing** | `k6` | Drives the concurrency validation in NFR-2.3 |

### 7.1 Deliberate omissions

- **Kubernetes** — Compose is sufficient for demonstration. Adding K8s multiplies operational work without adding architectural insight at this scale. Stretch goal only.
- **Service mesh** — resilience patterns are implemented in-code, where they're visible and explicable, rather than delegated to a sidecar.
- **CQRS / event sourcing** — the read/write split here doesn't justify the complexity. Mentioning the consideration is more honest than implementing it performatively.
- **Kafka** — RabbitMQ covers every requirement; Kafka's log-retention and replay strengths aren't needed.

## 8. Data model (essential entities)

### auth_db
```
users          (id, email UNIQUE, password_hash, role, created_at)
refresh_tokens (id, user_id, token_hash, expires_at, revoked_at)
```

### booking_db
```
venues        (id, name, address)
events        (id, venue_id, organizer_id, name, starts_at, on_sale_at, status)
pricing_tiers (id, event_id, name, price_cents)
seats         (id, event_id, section, row, number, tier_id, status)
               -- status is available|booked. 'held' is a Redis key, not a
               -- column: a hold recorded here is a hold nothing expires
               -- without a sweeper (§5.2)
               UNIQUE (event_id, section, row, number)
reservations  (id, user_id, event_id, status, total_cents,
               expires_at, idempotency_key UNIQUE, created_at,
               payment_pending_since)   -- set when the payment outcome is
                                        -- unknown; while set, ONLY the
                                        -- reconciliation job may release
                                        -- these seats (D8)
reservation_seats (reservation_id, seat_id, confirmed_at, released_at)
               PK(reservation_id, seat_id)
               UNIQUE (seat_id) WHERE confirmed_at IS NOT NULL
                                  AND released_at IS NULL   -- D13 backstop:
               -- one CONFIRMED claim per seat, which is the invariant verbatim
bookings      (id, reservation_id UNIQUE, user_id, payment_id,
               confirmed_at, status)
tickets       (id, booking_id, seat_id, qr_code UNIQUE, issued_at)
outbox        (id, aggregate_id, event_type, payload, created_at, published_at,
               claimed_until)  -- id is the event_id; payload is the complete
                               -- envelope; claimed_until is a relay's lease (§4.3)
```

### redis
```
seat:hold:{event_id}:{seat_id} -> {reservation_id}   -- SET NX EX 600
```
The hold itself, not a cache of one. Lost on restart, which is safe and is why the container runs with no persistence: a hold is minutes of transient state, and Postgres owns every confirmed booking.

Key constraints: the partial unique index on `reservation_seats` above, enforcing at most one **confirmed** claim per seat, as a database-level backstop to the Redis claim path. Defense in depth — if the application logic is ever wrong, the database still refuses the double-booking.

### payment_db
```
charges          (id, reservation_id, user_id, amount_cents, status,
                  provider_ref, idempotency_key UNIQUE, created_at, updated_at)
refunds          (id, charge_id, amount_cents, status, provider_ref,
                  idempotency_key UNIQUE, created_at)
outbox           (id, aggregate_id, event_type, payload, created_at, published_at,
                  claimed_until)   -- written by the settle statement itself (§4.3)
```

`charges.status` is `pending | succeeded | declined | failed`, and `pending` is not an implementation detail: it is written **before** the provider is called (FR-4.5) and left in place when the call does not answer. A caller reading `pending` has an unknown outcome, not a failed one — the distinction the whole saga rests on. The `outbox` tables in both services arrived with the relay in P4 (§4.3).

P2 prices a seat map at a flat per-seat rate (`seats.price_cents`), summed into `reservations.total_cents` under the same lock that claims the seats. `pricing_tiers` and `seats.tier_id` arrive with the organizer flow; nothing in the saga or in Payment changes when they do, because both already work from the stored total.

### notif_db
```
notifications     (id, user_id, type, channel, status, payload, created_at)
                  -- id IS the event_id: one event, one notification, and a
                  -- redelivery resumes it and its attempt count (§4.5)
delivery_attempts (id, notification_id, attempt_no, status, error, attempted_at)
                  UNIQUE (notification_id, attempt_no)
processed_events  (event_id PK, processed_at)   -- consumer idempotency; written
                  -- by the same statement that marks the notification sent
```

## 9. Internal service structure

Every service follows the same layered / ports-and-adapters layout, so the codebase stays navigable and business logic remains testable without infrastructure:

```
cmd/<service>/main.go        — wiring, config, graceful shutdown
internal/
  domain/                    — entities, value objects, state machines, domain errors
                               (no imports from infrastructure)
  usecase/                   — application services, orchestration, transaction boundaries
                               (depends on repository *interfaces*)
  repository/                — Postgres/Redis implementations of those interfaces
  provider/                  — outbound adapters that are not databases
                               (Payment only: the payment provider). Same layer
                               as repository — imports domain and drivers,
                               never usecase
  transport/
    grpc/                    — gRPC handlers
    http/                    — REST handlers (gateway-facing services only)
  events/                    — publishers, consumers, outbox relay
  config/                    — env loading and validation
migrations/                  — versioned SQL
proto/                       — protobuf definitions
```

Dependencies point inward. `domain` imports nothing from outer layers. Seat state transition rules live in `domain`, not scattered across handlers.

## 10. Resilience patterns

| Pattern | Applied where | Note |
|---|---|---|
| Timeouts | Every network call | No unbounded waits, ever |
| Retry + exponential backoff + jitter | Transient failures on idempotent operations only | Never retry a non-idempotent operation without an idempotency key |
| Circuit breaker | Booking → Payment, Payment → provider | Prevents cascading failure and pointless load on a struggling dependency |
| Idempotency keys | Hold, confirm, charge, refund | Makes retries safe (FR-3.7, FR-4.3) |
| Transactional outbox | Booking, Payment | No lost events on crash |
| Consumer idempotency | Notification, Booking saga consumer | At-least-once delivery guarantees duplicates |
| Dead-letter queue | All consumers | Poison messages don't block the queue |
| Graceful shutdown | All services | Drain in-flight requests; nack unacked messages |
| Fail closed on Redis loss | Booking holds | Reject new holds rather than risk a double-booking |

## 11. Deployment topology (local)

`docker compose up` brings up: gateway, auth, booking, payment, notification, three Postgres instances (or one instance with three isolated databases — acceptable locally, but never a shared schema), Redis, RabbitMQ, Jaeger, Prometheus, Grafana, and a seed job that creates a demo event with a full seat map.

**First-admin bootstrap.** Only an admin may register a privileged account, so an empty `auth_db` is unbootstrappable: there is no first admin and no way to make one. Auth therefore creates one at startup from `BOOTSTRAP_ADMIN_EMAIL` / `BOOTSTRAP_ADMIN_PASSWORD`, guarded by "does any account hold the admin role" — so it is a no-op on every start after the first, and on a database seeded by any other means.

Three properties are deliberate:

- **It never promotes an existing account.** Reading a role change out of an environment variable would make "who can edit the deployment config" equal to "who can become an admin". A taken address is reported and skipped.
- **It does not block startup.** Auth starts independently of its own database (§3.3); a bootstrap that blocked on a query would give that up. It runs in the background and retries, so the ordinary first-run case — the container up before migrations have created the schema — resolves itself. A configuration error, which will never resolve itself, is reported once and abandoned.
- **The guard is the role, not the address.** Concurrent replicas race to the `users.email` unique index; the loser treats the conflict as success and carries on.

That is the whole of admin management. There is no promotion endpoint and no admin CLI — the bootstrap exists to satisfy PRD §7 criterion 5, not to become a user-administration surface.

As of P2 that is: `postgres` (booking_db), `auth_db`, `payment_db`, `booking`, `auth`, `payment`, `gateway`. There are no `depends_on` edges between the four services — each waits only on its own database, the gateway waits on nothing because it dials its upstreams lazily, and Booking does not wait on Payment for the same reason. Starting them in any order, or starting the gateway with every upstream down, is a supported configuration and is the compose-level expression of §3.3. Booking with Payment down serves seat maps and holds normally; only the pay step reports an unknown outcome, and reconciliation resolves those once Payment returns.

`PAYMENT_PROVIDER_MODE` (`succeed` | `decline` | `hang`) selects how the mock provider answers. `hang` is the interesting one: it produces the unknown outcome D8 exists for, in a running stack, and lets an operator watch a hold's TTL get pushed out (`TTL seat:hold:...` in `redis-cli`) instead of lapsing, and the reconciliation job resolve it.

P4 adds `rabbitmq`, `notif_db` and `notification`. Only Notification waits on the broker, which is its only source of work; Booking and Payment dial it lazily and keep serving with it down, their events waiting in the outbox. Nothing waits on Notification: stop it and bookings confirm exactly as before, while its durable queue fills and then drains when it returns. Its compose healthcheck is readiness — consuming — so `docker compose up --wait` does not return before its queue exists. RabbitMQ runs with a fixed hostname and a volume, because its data directory is keyed on the node name and a recreated container would otherwise abandon the durable queues and every message in them. `NOTIFICATION_MAIL_MODE=fail` is Notification's equivalent of `hang`: every email is refused, the attempts land in `delivery_attempts`, and the message moves to `notification.dlq`, visible in the management UI on port 15672.

Each service has its own Dockerfile and its own CI pipeline. Independent deployability is the entire justification for this architecture — if services can only be released together, the split has bought nothing.

## 12. Testing strategy

| Level | Scope | Tooling |
|---|---|---|
| Unit | Domain logic, state machines, saga decision logic | `testing` + `testify`, no infrastructure |
| Integration | Repositories against real Postgres and Redis | `testcontainers-go` |
| **Concurrency** | **The core invariant: N parallel holds on one seat → exactly one wins** | Goroutines + real Postgres. **This is the single most important test in the project** |
| Contract | gRPC handlers against protobuf definitions | Generated stubs |
| Saga | Compensation paths, including forced payment failure and forced timeout | Integration test with a fault-injecting payment mock |
| End-to-end | Full reservation flow across the running stack | Compose + HTTP client |
| Load | 200 concurrent holds, no deadlock, latency targets met | `k6` |

The concurrency test is not optional and is not a formality. It is the executable proof of the property in PRD §1 — write it in P0, keep it green through every subsequent phase, and run it again after each architectural change.