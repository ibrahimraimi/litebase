# Litebase

**The open-source, developer-first control plane for SQLite.**

Litebase brings a Postgres- and Supabase-inspired developer workflow—**migrations, database branching, consistent snapshots, and a secure HTTP API**—to stock SQLite, packaged as a single self-hostable Go binary.

---

## Documentation Index

| Guide | Description |
|---|---|
| **[Installation](./INSTALLATION.md)** | Build from source, run with Docker, and verify your environment. |
| **[Setup & Configuration](./SETUP.md)** | Configure `litebase serve`, `litebase.toml`, scoped API tokens, TLS, and data directories. |
| **[Usage & API Reference](./USAGE.md)** | End-to-end CLI workflows (`init`, `migrate`, `deploy`, `snapshot`, `branch`) and REST API reference. |
| **[OpenAPI 3.1 Specification](../api/openapi.yaml)** | Machine-readable contract for all `/v1` HTTP endpoints and schemas. |

---

## Why Litebase?

SQLite is fast, reliable, and zero-configuration, but running it as a shared service for web apps, serverless functions, or team workflows exposes gaps: concurrent write locking (`SQLITE_BUSY`), ad-hoc schema changes, and unsafe file copying.

Litebase solves this without forking SQLite or introducing distributed consensus complexity:

- **Git-like Workflow (`init` -> `link` -> `deploy`)**: Manage schema changes through versioned, checksum-verified SQL migrations in Git.
- **Database Branching (`branch create / diff / merge`)**: Spin up isolated database copies per pull request via `VACUUM INTO`, inspect DDL diffs against `main`, and merge schema changes atomically.
- **Zero-`SQLITE_BUSY` Write Engine**: Every database is owned by a dedicated single-writer actor goroutine using `BEGIN IMMEDIATE` group commits and per-request `SAVEPOINT` isolation.
- **Lossless 64-Bit Value Encoding**: Preserves full `int64` precision (`> 2^53`) and binary `BLOB` values over JSON, with an opt-in `?format=plain` mode for convenience.
- **Defense-in-Depth SQL Security**: Enforces `sqlite3_set_authorizer` on every connection, blocking `ATTACH`, `DETACH`, `load_extension`, and unsafe `PRAGMA` statements.

---

## 60-Second Quickstart

### 1. Start the server

```bash
export LITEBASE_BOOTSTRAP_TOKEN="lbk_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
./bin/litebase serve --addr 127.0.0.1:8080 --data-dir ./litebase-data
```

### 2. Create a database and run an atomic batch

```bash
# Create the "main" database
curl -s -X POST http://127.0.0.1:8080/v1/databases \
  -H "Authorization: Bearer $LITEBASE_BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"main","durability":"normal"}'

# Create a table and insert rows in a single atomic transaction
curl -s -X POST http://127.0.0.1:8080/v1/databases/main/batch \
  -H "Authorization: Bearer $LITEBASE_BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "statements": [
      {"sql": "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INTEGER NOT NULL)"},
      {"sql": "INSERT INTO users (name, age) VALUES (?, ?)", "params": [{"type": "text", "value": "Ada"}, {"type": "integer", "value": "36"}]}
    ]
  }'
```

### 3. Query your data

```bash
curl -s -X POST http://127.0.0.1:8080/v1/databases/main/query \
  -H "Authorization: Bearer $LITEBASE_BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sql": "SELECT id, name FROM users WHERE age > ?", "params": [{"type": "integer", "value": "21"}]}'
```

Response:

```json
{
  "columns": ["id", "name"],
  "rows": [
    [
      {"type": "integer", "value": "1"},
      {"type": "text", "value": "Ada"}
    ]
  ],
  "rows_affected": 0,
  "last_insert_rowid": null,
  "duration_ms": 0.42
}
```

---

## Architecture Overview

```text
                 ┌────────────────────────────────────────┐
  litebase CLI ─▶│  HTTP API (net/http, Problem JSON)     │
  SDKs / curl  ─▶│  Bearer Auth Middleware → Router       │
                 └──────────────────┬─────────────────────┘
                                    │
                 ┌──────────────────▼─────────────────────┐
                 │  Database Manager                      │
                 │  Lazy open, ref-counted idle eviction  │
                 └─────────┬──────────────────┬───────────┘
                           │                  │
                ┌──────────▼─────┐   ┌────────▼──────────┐
                │ Writer Actor   │   │ Reader Pool       │
                │ 1 goroutine    │   │ N read-only conns │
                │ 1 connection   │   │ mode=ro           │
                │ Group Commit   │   │ query_only = ON   │
                └────────┬───────┘   └────────┬──────────┘
                         └─────────┬──────────┘
                           ┌───────▼────────┐
                           │ <db>.sqlite    │  WAL mode
                           └───────┬────────┘
                                   │ VACUUM INTO
                           ┌───────▼────────┐
                           │ Snapshots &    │
                           │ Branches       │
                           └────────────────┘

  control.db (SQLite): projects, databases, tokens, migrations_applied, snapshots, branches, audit_log
```

### Concurrency Model

1. **Single Writer Actor per Database**: One dedicated goroutine owns the single read-write connection (`SetMaxOpenConns(1)`, `SetConnMaxLifetime(0)`). Incoming write jobs are queued on a bounded channel.
2. **Group Commit with Savepoints**: The writer actor drains queued jobs into a single `BEGIN IMMEDIATE ... COMMIT` transaction to amortize disk `fsync` overhead. Each job runs inside its own `SAVEPOINT` so a failing query rolls back cleanly without aborting sibling jobs in the batch.
3. **Concurrent Reader Pool**: `SELECT` and read-only statements route automatically to a concurrent pool of `mode=ro` connections with `PRAGMA query_only = ON`. Readers never block the writer, and the writer never blocks readers.

---

## Durability Modes

Each database configures its own SQLite WAL synchronization policy at creation time:

| Mode | SQLite Pragma | Behavior & Trade-off |
|---|---|---|
| `normal` *(default)* | `PRAGMA synchronous = NORMAL` | High throughput. Safe against application crashes and `SIGKILL` (`kill -9`). An OS kernel panic or sudden hardware power cut may roll back recent uncheckpointed WAL frames. |
| `full` | `PRAGMA synchronous = FULL` | Syncs the WAL file to disk on every group commit. Guarantees zero acknowledged write loss across power loss and `kill -9` at the cost of higher write latency. |

---

## Security Model

- **SQLite Authorizer Callback (`sqlite3_set_authorizer`)**: Attached to every reader and writer connection. Denies `ATTACH`, `DETACH`, `load_extension`, `fts3_tokenizer`, modifications to internal `_litebase_*` tables, and any `PRAGMA` outside the read-only inspection allow-list (`table_info`, `index_list`, `index_info`, `foreign_key_list`, `integrity_check`, `wal_checkpoint`, `page_count`, `page_size`, `freelist_count`).
- **Scoped Bearer Tokens**: `lbk_`-prefixed 256-bit random tokens are displayed once on creation and stored as SHA-256 hashes in `control.db`. Tokens support `read`, `write`, and `admin` scopes, optional database binding, and expiration timestamps.
- **Strict Identifier Validation**: Database, branch, migration, and snapshot names are validated against `^[a-z0-9][a-z0-9_-]{0,62}$` and checked against directory traversal before constructing filesystem paths.
- **Resource & Rate Limits**: Enforces maximum request body size (`4 MiB`), maximum statements per batch (`256`), maximum returned rows per query (`10,000` with `truncated: true`), per-query execution timeouts, and per-token rate limiting (`429` / `503` with `Retry-After`).

---

## What Litebase Is Not

- **Not a distributed Raft cluster**: Litebase runs on a single host with local SSD storage. It does not use Raft or FUSE.
- **No interactive client-held HTTP transactions**: Holding `BEGIN` open across network round-trips stalls the single writer. Use `POST /v1/databases/{db}/batch` to execute multi-statement transactions atomically on the server.
- **No automatic data merging across branches**: `litebase branch merge` diffs and applies **schema DDL changes** (`CREATE TABLE`, `ALTER TABLE ADD COLUMN`, `CREATE INDEX`, `CREATE TRIGGER`, `CREATE VIEW`) from a branch to its parent database. Row-level data is isolated per branch.
