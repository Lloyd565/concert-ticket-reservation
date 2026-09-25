# CLAUDE.md

NestJS + TypeScript REST API (monolith) for concert ticket reservations, PostgreSQL via TypeORM.
The task brief is `docs/assignment-brief.md`; work stage by stage and stop after each one.
`docs/archive/` is the retired Go microservices design (tag `go-microservices-p4`) — historical only, not rules for this code.

## Commands
- `docker compose up -d db` — Postgres on host port `DB_PORT` (5433); also creates `concert_test`
- `npm run start:dev` — run the API (needs `.env`, copy from `.env.example`)
- `npm run migration:generate -- src/database/migrations/<Name>` / `migration:run` / `migration:revert`
  (prefix `ENV_FILE=.env.test` to target the test database)
- `npm run lint`, `npm run build`, `npm test`, `npm run test:e2e`

## Layers (`src/modules/<feature>/`, enforced by `no-restricted-imports` in `eslint.config.mjs`)
- `presentation/` controllers + DTOs. No business logic; may import application and domain.
- `application/` services (use cases), transaction boundaries. Never imports TypeORM or infrastructure.
- `domain/` models, rules, `DomainError` subclasses, repository ports (abstract classes used as DI tokens). Plain TS: no `@nestjs/*`, no TypeORM, no class-validator.
- `infrastructure/` TypeORM entities, repository implementations, mappers. Implements domain ports; wired with `{ provide: XRepository, useClass: TypeOrmXRepository }`.
- Domain errors extend `src/common/domain/domain-error.ts`; HTTP status is mapped only in `src/common/filters/domain-exception.filter.ts`.
- Global pipe/filter are `APP_PIPE`/`APP_FILTER` providers in `AppModule` (not `main.ts`) so e2e tests get them too.

## Rules
- Never `synchronize: true`; every schema change is a migration.
- Config only from env vars, validated at boot in `src/config/env.validation.ts`.
- Commit messages: one-line Conventional Commit, no trailers.
