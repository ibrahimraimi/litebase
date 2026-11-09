# 0010: Migration history lives inside each database

- **Status:** Accepted
- **Date:** 2026-10-29 (v0.0.2-beta; supersedes the original design)

## Context

In v0.0.1-beta, applied-migration history was stored in the control database, keyed by database id. That caused real problems:

- Applying a migration and recording it were two separate steps. A crash between them left the schema migrated but unrecorded, so the next deploy failed.
- History did not travel with the data. A branch started with no history. A restore to an older snapshot kept the newer history, so a later deploy skipped migrations the schema no longer contained. `pull` and `push` moved the file without its history.

## Decision

Migration history is stored in a **`_litebase_migrations` table inside each user database** and is written **in the same transaction** as the migration itself.

- The table name is reserved. The authorizer blocks user writes to `_litebase_*` tables, and migrations use a privileged internal path.
- Existing data directories are **upgraded in place on first open**: history from the control database is copied into the database file.
- The control database copy is no longer used for decisions.

## Consequences

- Snapshots, branches, `pull` and `push` carry migration history with the data automatically.
- A migration is either fully applied and recorded, or not applied at all.
- The privileged internal write path is part of the security boundary and must stay narrow.
