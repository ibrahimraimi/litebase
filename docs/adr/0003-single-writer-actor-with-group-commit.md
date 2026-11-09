# 0003: One writer goroutine per database, with group commit

- **Status:** Accepted
- **Date:** 2026-10-29

## Context

In WAL mode SQLite allows many concurrent readers and exactly one writer. If a second writer arrives, or a transaction that began as a read tries to upgrade to a write while another writer holds the lock, SQLite returns `SQLITE_BUSY` immediately, and `busy_timeout` does not help in the upgrade case.

A plain connection pool pushes this problem onto every caller. Litebase serves many concurrent HTTP clients, so it must hide it.

## Decision

Each database has **one goroutine that owns its only write connection** (the writer actor).

- Write requests become jobs on a **bounded queue**. When the queue is full, new work is rejected with a retryable error (backpressure) instead of piling up.
- The actor drains up to a batch of jobs and runs them in **one transaction started with `BEGIN IMMEDIATE`** (group commit).
- Each job runs inside its own **`SAVEPOINT`**, so a failing job is rolled back alone and does not fail the rest of the batch.
- A job's result is returned **only after `COMMIT` succeeds**. If the commit fails, every job in that batch fails.
- Reads use a **separate pool of read-only connections** (`mode=ro`, `PRAGMA query_only = ON`). Read-only statements are routed there automatically.

## Consequences

- Callers never see `SQLITE_BUSY`. Writes to one database are serialized, which matches SQLite's own limit.
- Commit cost (fsync) is shared across a batch, so throughput under concurrency is much better than one transaction per request.
- Databases are isolated from each other: a hot database cannot starve another.
- Maintenance operations that need the file (snapshot, restore, branch, upload) must coordinate with the actor and must not open their own write path.
- Any new code path that writes to a user database must go through the actor. See [docs/design/concurrency.md](../design/concurrency.md) for the invariants.
