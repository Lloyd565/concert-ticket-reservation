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

## Rules
- Never `synchronize: true`; every schema change is a migration.
- Config only from env vars, validated at boot in `src/config/env.validation.ts`.
- Commit messages: one-line Conventional Commit, no trailers.
