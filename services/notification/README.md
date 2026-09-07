# notification service — not built yet

Built in phase **P4**. See `ARCHITECTURE-ESSENTIALS.md` § "Build order".

Owns: Async email delivery, delivery log, consumer idempotency (notif_db)

Until then this directory is a placeholder. Do not implement it early —
AGENTS.md §2.13: P0's concurrency test must pass before any service split.
