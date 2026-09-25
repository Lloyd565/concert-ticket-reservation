# ROLE & GOAL
I am migrating my existing personal repo (Concert Ticket Reservation System — Go microservices, finished through phase P4) into a single NestJS + TypeScript REST API, to satisfy a university assignment. Keep the concert-ticketing domain, but shrink it to ONE monolithic NestJS app.

Language: talk to me in Bahasa Indonesia. Code, identifiers, and commit messages in English. README.md in Bahasa Indonesia.

# ASSIGNMENT REQUIREMENTS (acceptance criteria — every one must be met)
a. At least 2 related CRUD resources.
b. Data stored in a SQL database.
c. API authentication using JWT.
d. E2E tests for the token/auth API.
e. Use a project pattern I commonly use (defined in Stage 2).
f. README on GitHub explaining WHY that pattern was chosen.

# WORKING RULES
- Inspect the repo before assuming anything (layout, existing docs, git state).
- Work in stages. At the end of each stage: run what can be run, summarize in Bahasa Indonesia (what was done, how to verify, what is next), then STOP and wait for my "lanjut". Do not start the next stage on your own.
- Get each stage working at small scale before moving on. Never build ahead.
- Keep it SIMPLE. No microservices, gRPC, message broker, Redis, CQRS, or event sourcing. No features beyond this spec (Swagger docs are the only allowed extra).
- Before implementing the two hardest ideas (JWT guard request flow in Stage 3, transaction + row locking in Stage 4), explain each in 3–5 sentences, intuition first, then formal terms.
- Annotate non-obvious code with short comments explaining WHY, not WHAT.
- One commit per stage on the feature branch, Conventional Commits. Never force-push, never delete or move the snapshot tag.

# STAGE 0 — Snapshot & migration plan (git only, no code changes yet)
1. Check `git status`. If the tree is dirty, stop and ask me.
2. Create annotated tag `go-microservices-p4` on the current HEAD, then create branch `feat/nestjs-rest-api`.
3. Inspect the repo and propose a migration plan: what gets removed from the working tree (Go code is recoverable via the tag), what old design docs are kept under `docs/archive/`, and what gets replaced. `AGENTS.md` and the ARCHITECTURE docs are Go-specific and will mislead future agents, so plan to replace them with a new short `CLAUDE.md` for the NestJS stack.
4. Show me the plan and WAIT for approval before touching any file.

# STAGE 1 — Scaffold
- NestJS project at repo root, TypeScript strict mode, npm, ESLint + Prettier.
- `@nestjs/config` with env validation at boot (fail fast on missing or invalid vars). Commit `.env.example`, gitignore `.env`.
- PostgreSQL via `docker-compose.yml`. TypeORM with migrations (never `synchronize: true`). Scripts: `migration:generate`, `migration:run`, `migration:revert`.
- Global `ValidationPipe` (whitelist, forbidNonWhitelisted, transform).
Checkpoint: `docker compose up -d db && npm run start:dev` boots and connects to the DB.

# STAGE 2 — Architecture skeleton (the pattern for requirement e)
Pattern: layered / Clean-Architecture-style feature modules, dependencies pointing inward. This is the same principle as my Go layout (domain <- usecase <- repository/transport), adapted idiomatically to NestJS.

  src/
    main.ts, app.module.ts
    config/
    common/            guards, decorators, filters, shared db helpers
    modules/<feature>/
      presentation/    controllers + request/response DTOs (class-validator)
      application/     services (use cases), transaction boundaries
      domain/          domain models, business rules, domain errors,
                       repository interface (abstract class as DI token)
      infrastructure/  TypeORM entities, repository implementations, mappers

Rules:
- Controllers hold no business logic.
- Services never import TypeORM or write SQL.
- `domain/` imports nothing from NestJS or TypeORM.
- Infrastructure implements domain ports, wired via `{ provide: XRepository, useClass: TypeOrmXRepository }`.
- Domain errors are mapped to HTTP status codes in one place (exception filter or mapping layer), not scattered through services.
- Enforce the dependency rule with ESLint `no-restricted-imports` (e.g. `domain/` must not import `@nestjs/*` or `typeorm`).
Checkpoint: skeleton compiles, lint passes, and you have shown me the layer diagram for one module.

# STAGE 3 — Auth & JWT
Users: id (uuid), email (unique, stored lowercase), passwordHash (bcrypt), role (USER | ADMIN), createdAt.
- POST /auth/register -> 201. Always creates role USER (the client can never set the role). Never returns the hash. Duplicate email -> 409. Password min 8 chars.
- POST /auth/login -> 200 { accessToken, expiresIn }. Unknown email and wrong password must return the SAME 401 response.
- GET /auth/me -> protected, returns { id, email, role }.
- `@nestjs/jwt` + `passport-jwt`. HS256 only (pin `algorithms: ['HS256']` on verify so `alg: none` is rejected). Payload: `sub`, `role`, `iat`, `exp`. Secret and expiry come from env; validate secret length at boot; default expiry 15m.
- The JWT strategy verifies the user still exists.
- `JwtAuthGuard` applied GLOBALLY (secure by default) with a `@Public()` decorator to opt out. `RolesGuard` + `@Roles(Role.ADMIN)`.
- Admin bootstrap: an idempotent startup routine creates one ADMIN from `ADMIN_EMAIL` / `ADMIN_PASSWORD` env vars if no admin exists, so a fresh clone is demoable without manual SQL.
Before coding: explain the request flow (header -> guard -> strategy -> request.user -> RolesGuard -> controller).
Checkpoint: manual curl of register, login, and /auth/me works.

# STAGE 4 — Two related CRUDs
Concerts (public read, ADMIN write):
  id, name, artist, venue, startsAt, capacity, price, createdAt
  GET /concerts (paginated: page, limit) and GET /concerts/:id -> @Public()
  POST /concerts, PATCH /concerts/:id, DELETE /concerts/:id -> ADMIN only
  DELETE on a concert with active reservations -> 409.

Reservations (authenticated):
  id, userId, concertId, quantity, status (ACTIVE | CANCELLED), createdAt
  Relations: Reservation N:1 Concert, Reservation N:1 User.
  POST /reservations { concertId, quantity }
  GET /reservations (own only; ADMIN sees all), GET /reservations/:id
  PATCH /reservations/:id -> change quantity (ACTIVE only; re-check capacity)
  DELETE /reservations/:id -> cancel (status = CANCELLED, seats are freed)
  A user requesting another user's reservation gets 404 (do not leak existence).

Core invariant carried over from the original design: for a concert, the sum of ACTIVE reservation quantities must NEVER exceed capacity, even under concurrent requests. Enforce it inside a DB transaction that takes a pessimistic write lock on the concert row (SELECT ... FOR UPDATE). Keep the lock in the repository method and the transaction boundary in the service, behind a small transaction-runner abstraction so services still never import TypeORM. Tell me which abstraction you chose and why before implementing.
Before coding: explain why the lock is needed, using a concrete two-request race example.
All schema changes go through migrations.

# STAGE 5 — E2E tests (requirement d — highest priority)
Jest + supertest, in `test/`, against a REAL PostgreSQL (separate database with a `_test` suffix via `.env.test`; schema from migrations; data reset between tests). Refuse to run if the DB name does not end in `_test` so the dev DB can never be wiped.

`test/auth-token.e2e-spec.ts` must cover:
 1. register -> 201, response has no passwordHash
 2. register with duplicate email -> 409
 3. login valid -> 200; decoded token has sub, role, exp; header alg is HS256
 4. login with wrong password AND unknown email -> 401 with identical body
 5. protected route with no Authorization header -> 401
 6. malformed header / non-Bearer scheme -> 401
 7. tampered token (altered payload or swapped signature) -> 401
 8. token signed with a different secret -> 401
 9. expired token (real secret, exp in the past) -> 401
10. `alg: none` token -> 401
11. valid token -> GET /auth/me returns the correct user
12. valid token whose user was deleted -> 401
13. RBAC: USER token on POST /concerts -> 403; ADMIN token -> 201
14. public route GET /concerts works without a token

`test/reservations.e2e-spec.ts` (smaller): user A cannot read user B's reservation (404); over-capacity request -> 409; cancel frees seats.

Add a few fast unit tests for the domain capacity rule (no DB).
Stretch, ONLY after everything else is green: a concurrency e2e test firing N parallel reservations at a concert with capacity K, asserting total ACTIVE quantity <= K, verified by querying the DB directly and not only by HTTP status codes.
`npm run test:e2e` must work from a clean clone following only the README steps.

# STAGE 6 — README, docs, CI
README.md (Bahasa Indonesia): overview, tech stack, prerequisites, quick start (copy .env, docker compose up, migrate, run), env var table, endpoint table (method, path, auth required, role), short auth-flow explanation, how to run unit + e2e tests, project structure tree.
It MUST contain a section "Mengapa Menggunakan Pola Ini?" covering:
  - what the pattern is and how it maps onto this codebase (point to real folders/files);
  - why: separation of concerns, testability (services can be tested with a fake repository), swappable persistence via the repository port, fit with Nest's DI, the inward dependency rule that keeps business rules independent of framework and DB;
  - that I have used the same inward-dependency layering in my previous Go microservices project, and that this is why I choose it by default;
  - honest trade-offs (extra boilerplate such as mappers and ports for a small app) and why it is still worth it;
  - what I deliberately did NOT use (e.g. microservices) and why it is out of scope here.
Also: add `@nestjs/swagger` at /docs with bearer auth; write a new short `CLAUDE.md` (stack, commands, layer rules, test rules); optional GitHub Actions workflow running lint, build, and e2e with a Postgres service container.

# DEFINITION OF DONE
- [ ] All six assignment requirements (a–f) demonstrably met, and you have mapped each to a file or endpoint in your final summary
- [ ] `npm run lint`, `npm run build`, `npm run test`, and `npm run test:e2e` all pass from a clean clone
- [ ] Snapshot tag `go-microservices-p4` intact; no Go code or stale AGENTS.md left in the branch working tree
- [ ] README explains the pattern choice

Start with Stage 0 only.