# 0009: `deploy` applies migrations; it never uploads a database

- **Status:** Accepted
- **Date:** 2026-10-29

## Context

The CLI vocabulary is `init`, `link` and `deploy`. "Deploy" could mean shipping a local database file to the server, or applying schema changes. Shipping a local file over a production database destroys the production data.

## Decision

- **`litebase deploy` applies pending migrations** (ordered `NNNN_<name>.sql` files) to the linked remote database. `deploy --dry-run` shows what would run.
- Each applied migration is recorded with a **SHA-256 checksum**. If an already-applied file has changed, `deploy` fails.
- Moving a whole database file is a **separate, explicit** operation: `litebase pull`, and `litebase push --force`. A snapshot is taken automatically before a push replaces data.

## Consequences

- Schema changes are reviewable in Git, which is the "Git-like" part of the workflow.
- Data never travels through `deploy`.
- Whole-file push is a sharp tool and is gated behind `--force`.
