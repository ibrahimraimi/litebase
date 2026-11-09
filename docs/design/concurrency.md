# Concurrency and the write path

How Litebase keeps many concurrent HTTP clients safe on top of SQLite. Read this before changing anything under `internal/engine` or `internal/manager`. The reasoning is in [ADR 0003](../adr/0003-single-writer-actor-with-group-commit.md) and [ADR 0005](../adr/0005-wal-and-per-connection-pragmas.md).

## The constraint

In WAL mode SQLite allows many readers and one writer. A second writer, or a read transaction that tries to become a write while another writer holds the lock, fails immediately with `SQLITE_BUSY`. Litebase never lets that reach a caller.

## Shape

```
                HTTP request (auth, limits, validation)
                               |
                    statement classification
                    (read-only or write?)
                    /                     \
                   v                       v
        read-only connection pool     writer actor (one goroutine
        mode=ro, query_only           per database, owns the only
                                      write connection)
                   \                       |
                    \              bounded job queue
                     \                     |
                      \         drain a batch of jobs
                       \                   |
                        \       BEGIN IMMEDIATE
                         \      SAVEPOINT per job
                          \     run job, release or roll back savepoint
                           \    COMMIT
                            \              |
                             +----> <db>.sqlite (WAL mode)
```

## Write path, step by step

1. A request is authenticated, size-limited and validated. The authorizer is installed on the connection.
2. The statement is classified. Anything that writes becomes a **job** and is placed on the database's bounded queue. If the queue is full, the request is rejected with HTTP 503 and the problem code `queue_full`.
3. The writer actor takes the first job, then drains more without blocking, up to a batch limit.
4. It starts **one transaction with `BEGIN IMMEDIATE`**, so it takes the write lock up front.
5. For each job it creates a `SAVEPOINT`, runs the job's statements, and either releases the savepoint (success) or rolls back to it (failure). A failing job does not affect the others.
6. It **commits once**.
7. Only after `COMMIT` succeeds are results sent back. If `COMMIT` fails, every job that had succeeded within the batch is failed.

## Read path

Read-only statements run on a pool of read-only connections (`mode=ro`, `PRAGMA query_only = ON`). They never queue behind writers and never block them. Each query has a timeout, a row cap and a response byte budget.

## Invariants (do not break these)

1. **All writes to a user database go through its writer actor.** No other goroutine opens a write connection to it.
2. **Every write transaction starts with `BEGIN IMMEDIATE`.**
3. **A caller is told "success" only after `COMMIT` returned.**
4. **One failing job never fails an unrelated job in the same batch.**
5. **Every connection gets the per-connection settings** (`foreign_keys`, `busy_timeout`, `temp_store`, `synchronous`). A reopened connection must not lose them.
6. **The queue is bounded.** Overload produces a clear, retryable error, not unbounded memory growth.
7. **No transaction is held open across requests.** See [ADR 0004](../adr/0004-no-interactive-transactions.md).
8. **User SQL passes the authorizer.** Internal writes to `_litebase_*` tables use a narrow, privileged path.

## Maintenance operations

Snapshot, restore, branch creation, upload and shutdown all touch the database file. They must cooperate with the actor:

- **Snapshots** use `VACUUM INTO` on a dedicated connection, so they do not occupy the writer.
- **Restore** marks the database as restoring, waits for in-flight requests to drain, closes the database, swaps the file, and reopens it. Other databases are unaffected.
- **Shutdown** stops accepting work, drains the queue, performs a final WAL checkpoint, and exits.
- **Idle eviction** must wait for the actor to drain before closing a database.

## Failure modes

| Failure | Behavior |
|---|---|
| Queue full | Request rejected with a retryable error |
| A job fails inside a batch | Rolled back to its savepoint; the rest of the batch commits |
| `COMMIT` fails | Whole batch rolls back; all affected requests get an error |
| Process killed (SIGKILL) | WAL recovery on next open; every acknowledged write is present with `synchronous = FULL` |
| Long-lived reader | Can delay WAL checkpoints; WAL size is exposed in metrics |

## Where the code lives

- `internal/engine/writer.go`: the actor, batching and savepoints.
- `internal/engine/query.go` and `database.go`: connection setup, routing, read pool.
- `internal/engine/authorizer.go` and `sqlparse.go`: the SQL security boundary.
- `internal/manager/manager.go`: database lifecycle, restore, eviction, quotas.

## Tests that guard this

- Many concurrent writers: all succeed or get a clean overload error, with no `SQLITE_BUSY` visible.
- A counter incremented concurrently ends at exactly N (no lost updates).
- A failing statement does not fail the batch (savepoint isolation).
- Kill the process during writes (SIGKILL), reopen, and check `PRAGMA integrity_check` plus that every acknowledged write is present.
- Run everything with `go test -race`.
