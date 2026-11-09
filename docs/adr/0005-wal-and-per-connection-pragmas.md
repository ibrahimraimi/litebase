# 0005: WAL mode and per-connection pragmas

- **Status:** Accepted
- **Date:** 2026-10-29

## Context

Several SQLite settings are per connection, not per database. A pooled connection that is recycled and reopened silently loses them, which can turn off foreign key enforcement or the busy timeout without any error.

## Decision

- Every database runs in **WAL mode**.
- The following are applied on **every connection** Litebase opens, including reopened ones: `foreign_keys = ON`, `busy_timeout = 5000`, `temp_store = MEMORY`, and `synchronous`.
- `synchronous` is configurable **per database** (`NORMAL` or `FULL`). `NORMAL` is safe against corruption in WAL mode but can lose the most recent commits on power loss. `FULL` avoids that at a throughput cost. This trade-off must be documented.
- Read connections additionally open with `mode=ro` and `PRAGMA query_only = ON`.
- The writer uses a single connection (pool capped at one) with no maximum lifetime, so it is not recycled on a timer.

## Consequences

- WAL needs shared memory on one host. **Databases must not live on NFS or other network filesystems.**
- A long-lived read transaction can block checkpointing, so the WAL file can grow. WAL size is exposed per database in metrics so this can be alerted on.
- Copying a live database with `cp` is not a valid backup. Use snapshots (see ADR 0012).
