# 0012: Snapshots use `VACUUM INTO`; restore keeps the original aside

- **Status:** Accepted
- **Date:** 2026-10-29 (v0.0.2-beta moved snapshots off the writer)

## Context

Copying a live SQLite database file is not a valid backup: the main file, the `-wal` file and in-flight writes can be out of step. A snapshot must be a consistent, standalone database.

## Decision

- Snapshots are created with **`VACUUM INTO`**, which produces a consistent, compact copy. Since v0.0.2-beta this runs on a **dedicated connection**, so a snapshot does not stall writes to that database.
- Snapshots live under `<data-dir>/snapshots/<db>/` and are listed in the control database.
- **Restore** stages the snapshot file, waits for in-flight requests to drain, closes the database, moves the current file aside as `.pre-restore`, removes stale `-wal` and `-shm` files, and renames the staged file into place. The database is then reopened and checked with `PRAGMA integrity_check`.
- An **automatic `pre-restore` snapshot** is taken before every restore (and a `pre-push` snapshot before a push).
- Restore holds a per-database restoring state, so unrelated databases keep working.

## Consequences

- Stale `-wal` and `-shm` files must never be left next to a replaced database file.
- Automatic snapshots accumulate unless something prunes them. Retention is addressed in ADR 0015.
- The swap has a crash window that needs startup recovery. See ADR 0016.
