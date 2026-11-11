# Usage & API Reference

This guide walks through the daily Litebase developer workflow—from initializing a project and deploying SQL migrations to branching databases for pull requests and calling the `/v1` HTTP REST API.

---

## 1. Project & Migration Workflow (`init` -> `link` -> `deploy`)

Litebase treats database schema changes like Git commits: ordered `.sql` files in your repository, verified by SHA-256 checksums, and applied atomically on the remote writer actor.

### Step 1: Initialize a project

```bash
litebase init --project myapp
```

This creates:
- `litebase.toml` (project configuration)
- `migrations/` (directory for ordered SQL migrations)
- `.litebase/local.sqlite` (local development database)

### Step 2: Link to your Litebase server and create a remote database

```bash
export LITEBASE_TOKEN="lbk_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

# Point litebase.toml at your server and target database
litebase link http://127.0.0.1:8080 --project myapp --database main

# Create the "main" database on the server (durability: normal | full)
litebase db create main --durability normal

# List remote databases
litebase db list
```

### Step 3: Create a migration

```bash
litebase migrate new create_users
```

Output:

```text
Created migrations/0001_create_users.sql
```

Add your SQL DDL statements to `migrations/0001_create_users.sql`:

```sql
-- Migration: 0001_create_users.sql

CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE INDEX idx_users_email ON users(email);
```

### Step 4: Check migration status and deploy

```bash
# Compare local files in ./migrations against remote applied migrations
litebase migrate status
# 0001	PENDING	create_users

# Preview pending migrations without modifying the remote database
litebase deploy --dry-run
# Dry-run pending migrations for "main": [0001]

# Apply pending migrations atomically
litebase deploy
# Applied 1 migration(s) to "main": [0001]
```

> [!IMPORTANT]
> **`deploy` applies migrations only—it never overwrites your remote SQLite file with a local file.**
> Once a migration is applied, Litebase records its SHA-256 checksum in `control.db`. If an already-applied `.sql` file is modified later, `litebase migrate status` reports `CHECKSUM_MISMATCH` and `litebase deploy` fails loudly to prevent schema drift.

---

## 2. Ad-Hoc Queries & Interactive Shell

### Run a single SQL statement (`litebase query`)

By default, `litebase query` runs against your local `.litebase/local.sqlite` database. Pass `--remote` to execute against the linked remote database:

```bash
# Insert a row on the remote database
litebase query --remote "INSERT INTO users (email, created_at) VALUES ('ada@example.com', '2026-10-29T12:00:00Z')"
# OK (rows_affected=1, duration_ms=0.58)

# Query rows on the remote database
litebase query --remote "SELECT id, email, created_at FROM users"
```

Output:

```text
id  email            created_at
1   ada@example.com  2026-10-29T12:00:00Z
```

### Interactive SQL REPL (`litebase shell`)

```bash
# Local REPL
litebase shell

# Remote REPL connected to the active database/branch
litebase shell --remote
```

Inside the shell, enter SQL statements terminated by a newline, and type `.exit`, `.quit`, or `exit` to leave:

```text
litebase> SELECT id, email FROM users;
id  email
1   ada@example.com
litebase> .exit
```

---

## 3. Snapshots, Scheduled Backups & Disaster Recovery

Litebase creates consistent online backups using SQLite's `VACUUM INTO` without blocking readers, verifies SHA-256 checksums and `PRAGMA integrity_check` before any restore, and supports scheduled local and S3-compatible remote backups.

### Create, Pin, List, Delete & Prune Snapshots

```bash
# Create a manual snapshot (add --pin to protect it from deletion and pruning)
litebase snapshot create --label pre-release --pin
# Created snapshot snap_94a1f02b8c12e301 (label=pre-release, size=16384 bytes)

# List local snapshots (or pass --remote to list objects in remote backup storage)
litebase snapshot list
litebase snapshot list --remote

# Delete an unpinned snapshot that is not referenced by an active branch
litebase snapshot delete snap_94a1f02b8c12e301

# Preview or run retention pruning
litebase snapshot prune --dry-run
litebase snapshot prune
```

**Retention Rules**:
- `scheduled` snapshots are pruned according to the database's backup policy (`keep_last` and `keep_days`).
- Automatic `pre-restore` and `pre-push` snapshots are capped (`keep_last = 5`, `keep_days = 7`).
- `manual` snapshots, `branch` parent snapshots, and any snapshot with `pinned = true` are **never** auto-pruned.

### Scheduled Backups & Verification Drills (`litebase backup`)

Backup policies are opt-in per database:

```bash
# Enable hourly scheduled backups, keeping the last 24 and 14 days, and uploading to remote storage
litebase backup policy set --every 1h --keep-last 24 --keep-days 14 --remote

# Inspect or disable the policy
litebase backup policy show
litebase backup policy disable

# Trigger an immediate backup and inspect status
litebase backup run
litebase backup status

# Run a non-destructive restore drill in a temporary directory (never touches the live database)
litebase backup verify --remote --sample users
```

### Restore a Database (Local, Remote, or Into a New Database)

```bash
# Restore the active database from a local snapshot ID
litebase snapshot restore snap_94a1f02b8c12e301

# Restore from the latest remote backup
litebase snapshot restore --from-remote latest

# Restore a snapshot or remote backup into a new database for side-by-side inspection
litebase snapshot restore --from-remote latest --as main_recovery_check
```

During a restore, Litebase:
1. Verifies the snapshot's SHA-256 checksum and runs read-only `PRAGMA integrity_check` before touching the live database.
2. Automatically takes a safety snapshot labeled `pre-restore`.
3. Drains and closes active connections on the target database.
4. Atomically replaces the database file, removes stale `-wal` and `-shm` files, and runs `PRAGMA integrity_check`.

### Disaster Recovery from an Empty Disk (`restore-control`)

If the server's disk is lost and remote backups (`--remote`) were enabled:

```bash
# 1. Restore control.db (tokens, database records, branches, policies, and audit log) before starting the server
litebase restore-control --from-remote latest --data-dir /var/lib/litebase

# 2. Start the Litebase server against the restored data directory
litebase serve --data-dir /var/lib/litebase

# 3. Restore each database from its latest remote backup and verify it
litebase snapshot restore --database main --from-remote latest
litebase backup verify --db main --remote
```

### Whole-File Download & Upload (`pull` & `push --force`)

When you need to pull a full copy of a remote database locally or seed a remote database from a local SQLite file:

```bash
# Download a consistent VACUUM INTO copy of the remote database to .litebase/local.sqlite
litebase pull
# Pulled remote database "main" to .litebase/local.sqlite (16384 bytes)

# Overwrite the remote database with a local SQLite file (requires --force; takes an automatic pre-push snapshot)
litebase push --force
# Pushed .litebase/local.sqlite to remote database "main" (size=16384 bytes)
```

Pass `--file <path>` to either command to read from or write to a custom SQLite file path.

---

## 4. Database Branching (`branch`)

Branches allow you to create an isolated copy of a database (for example, per pull request or feature branch), test migrations and schema changes safely, inspect the DDL diff, and merge the schema back into `main`.

### Step 1: Create and check out a branch

```bash
# Create an isolated branch copy from the active database ("main")
litebase branch create feature-billing
# Created branch "feature-billing" from "main" (id=br_7c3e9a1102f4b821)

# List branches of "main"
litebase branch list
# feature-billing	2026-10-29T15:15:00Z

# Switch your local litebase.toml target to the new branch
litebase branch checkout feature-billing
# Switched active database to "feature-billing"
```

### Step 2: Evolve the schema on the branch

```bash
litebase query --remote "ALTER TABLE users ADD COLUMN plan TEXT NOT NULL DEFAULT 'free'"
litebase query --remote "CREATE TABLE invoices (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, amount_cents INTEGER NOT NULL)"
```

### Step 3: Diff and merge the branch schema back into `main`

```bash
# Switch back to the parent database
litebase branch checkout main

# Inspect the generated SQL DDL diff between "main" and "feature-billing"
litebase branch diff feature-billing
```

Output:

```sql
CREATE TABLE invoices (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, amount_cents INTEGER NOT NULL);
ALTER TABLE "users" ADD COLUMN plan TEXT NOT NULL DEFAULT 'free';
```

Apply those schema changes atomically onto `main`:

```bash
litebase branch merge feature-billing
# Merged 2 schema statement(s) from "feature-billing" into "main"

# Delete the branch when done
litebase branch delete feature-billing
# Deleted branch "feature-billing"
```

---

## 5. HTTP REST API Reference (`/v1`)

All `/v1` endpoints expect `Authorization: Bearer lbk_<token>` and JSON request bodies (`Content-Type: application/json`).

### Endpoint Summary

| Method | Path | Required Scope | Description |
|---|---|---|---|
| `POST` | `/v1/databases` | `admin` | Create a new database (`normal` or `full` durability) |
| `GET` | `/v1/databases` | `read` | List all databases in the project |
| `GET` | `/v1/databases/{db}` | `read` | Get metadata and byte size for `{db}` |
| `DELETE` | `/v1/databases/{db}` | `admin` | Delete `{db}` and remove its SQLite files |
| `POST` | `/v1/databases/{db}/query` | `read` *(SELECT)* / `write` *(mutations)* | Execute a single parameterized SQL statement |
| `POST` | `/v1/databases/{db}/batch` | `write` | Execute multiple SQL statements atomically in one savepoint |
| `GET` | `/v1/databases/{db}/schema` | `read` | Inspect all user tables, indexes, triggers, and views |
| `GET` | `/v1/databases/{db}/migrations` | `read` | List applied migrations and their SHA-256 checksums |
| `POST` | `/v1/databases/{db}/migrations/apply` | `write` | Apply ordered migrations (or preview with `"dry_run": true`) |
| `POST` | `/v1/databases/{db}/snapshots` | `write` | Create a consistent snapshot via `VACUUM INTO` (optional `"pinned": true`) |
| `GET` | `/v1/databases/{db}/snapshots` | `read` | List local snapshots for `{db}` (or remote backups with `?remote=true`) |
| `DELETE` | `/v1/databases/{db}/snapshots/{id}` | `admin` | Delete a snapshot (refuses pinned or branch-referenced snapshots with `409 SNAPSHOT_PROTECTED`) |
| `POST` | `/v1/databases/{db}/snapshots/prune` | `admin` | Prune expired snapshots according to retention policy (`"dry_run": true` to preview) |
| `GET` | `/v1/databases/{db}/backup-policy` | `read` | Get the backup schedule and retention policy for `{db}` |
| `PUT` | `/v1/databases/{db}/backup-policy` | `admin` | Set and enable the backup schedule and retention policy for `{db}` |
| `DELETE` | `/v1/databases/{db}/backup-policy` | `admin` | Disable scheduled backups for `{db}` |
| `POST` | `/v1/databases/{db}/backups/run` | `admin` | Trigger an immediate backup run (snapshot + optional remote upload + prune) |
| `GET` | `/v1/databases/{db}/backups/status` | `read` | Get last backup run, last success, next scheduled run, and pending upload count |
| `POST` | `/v1/databases/{db}/backups/verify` | `admin` | Run a non-destructive restore drill on the latest local or remote backup |
| `POST` | `/v1/databases/{db}/restore` | `admin` | Restore `{db}` from `snapshot_id` or `remote_key` (optional `"as": "<new-db>"`) |
| `GET` | `/v1/databases/{db}/download` | `read` | Download a consistent `VACUUM INTO` copy of `{db}` (`litebase pull`) |
| `PUT` | `/v1/databases/{db}/upload?force=true` | `write` | Overwrite `{db}` with an uploaded SQLite 3 file after `integrity_check` (`litebase push --force`) |
| `POST` | `/v1/databases/{db}/branches` | `write` | Create an isolated branch copy from `{db}` |
| `GET` | `/v1/databases/{db}/branches` | `read` | List branches created from `{db}` |
| `DELETE` | `/v1/databases/{db}/branches/{branch}` | `write` | Delete a branch database |
| `GET` | `/v1/databases/{db}/branches/{branch}/diff` | `read` | Generate DDL statements needed to bring `{db}` to `{branch}`'s schema |
| `POST` | `/v1/databases/{db}/branches/{branch}/merge` | `write` | Apply the schema diff from `{branch}` onto `{db}` |
| `POST` | `/v1/tokens` | `admin` | Issue a scoped API token (`read`, `write`, `admin`) |
| `DELETE` | `/v1/tokens/{id}` | `admin` | Revoke an API token by ID |

---

### Value Encoding: Typed Mode vs. Plain Mode

Standard JSON numbers are IEEE-754 double-precision floats, which silently corrupt 64-bit integers above `2^53 - 1` (`9007199254740991`) and have no native binary `BLOB` representation.

By default, Litebase uses **Typed Value Encoding**:

| SQLite Affinity | JSON `type` | JSON `value` Encoding | Example |
|---|---|---|---|
| `NULL` | `"null"` | `null` | `{"type": "null", "value": null}` |
| `INTEGER` (`int64`) | `"integer"` | Base-10 string | `{"type": "integer", "value": "9223372036854775807"}` |
| `REAL` (`float64`) | `"float"` | Decimal string (finite only; `NaN`/`Inf` rejected) | `{"type": "float", "value": "3.14159"}` |
| `TEXT` (UTF-8) | `"text"` | UTF-8 string | `{"type": "text", "value": "Ada"}` |
| `BLOB` (`[]byte`) | `"blob"` | Standard Base64 string | `{"type": "blob", "value": "AAECAw=="}` |

#### Opt-In Plain Mode (`?format=plain` or `"plain": true`)

If your application does not use 64-bit integers beyond `2^53` and prefers native JSON scalars in responses, pass `"plain": true` in the request body or `?format=plain` in the URL query string:

```bash
curl -s -X POST "http://127.0.0.1:8080/v1/databases/main/query?format=plain" \
  -H "Authorization: Bearer $LITEBASE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sql": "SELECT id, email FROM users WHERE id = ?", "params": [{"type": "integer", "value": "1"}]}'
```

Plain response:

```json
{
  "columns": ["id", "email"],
  "rows": [
    [1, "ada@example.com"]
  ],
  "rows_affected": 0,
  "last_insert_rowid": null,
  "duration_ms": 0.31
}
```

---

### Atomic Batch Transactions (`POST /v1/databases/{db}/batch`)

Because interactive multi-request HTTP transactions (`BEGIN` ... `COMMIT` across separate HTTP calls) can stall the single writer, use `/batch` to run multiple statements atomically on the server. If any statement in the batch fails, the entire batch is rolled back cleanly:

```bash
curl -s -X POST http://127.0.0.1:8080/v1/databases/main/batch \
  -H "Authorization: Bearer $LITEBASE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "statements": [
      {
        "sql": "INSERT INTO users (email, created_at) VALUES (?, ?)",
        "params": [
          {"type": "text", "value": "grace@example.com"},
          {"type": "text", "value": "2026-10-29T16:00:00Z"}
        ]
      },
      {
        "sql": "SELECT id, email FROM users ORDER BY id ASC"
      }
    ]
  }'
```

---

### Error Handling (RFC 9457 Problem Details)

All error responses return `Content-Type: application/problem+json` with a machine-readable `code`:

```json
{
  "type": "https://litebase.tech/problems/forbidden",
  "title": "Forbidden",
  "status": 403,
  "detail": "statement denied by security policy: ATTACH statements are prohibited",
  "code": "forbidden"
}
```

| Error `code` | HTTP Status | When It Occurs |
|---|---|---|
| `invalid_request` | `400 Bad Request` | Invalid database/branch name, malformed JSON, empty SQL, checksum mismatch, or batch size over limit. |
| `sql_error` | `400 Bad Request` | SQLite syntax error, constraint violation (`UNIQUE`, `FOREIGN KEY`), or storage quota exceeded. |
| `unauthorized` | `401 Unauthorized` | Missing, malformed, expired, or revoked `lbk_` Bearer token. |
| `forbidden` | `403 Forbidden` | Insufficient token scope (`read` token attempting a write) or statement blocked by `sqlite3_set_authorizer`. |
| `not_found` | `404 Not Found` | Target database, branch, snapshot, or token ID does not exist. |
| `conflict` | `409 Conflict` | Database or branch with that name already exists, or snapshot SHA-256 checksum mismatch. |
| `SNAPSHOT_PROTECTED` | `409 Conflict` | Snapshot is pinned or referenced by an active branch and cannot be deleted. |
| `busy` | `503 Service Unavailable` | SQLite engine returned `SQLITE_BUSY` / lock contention. |
| `queue_full` | `429` or `503` | Per-token rate limit exceeded or writer actor queue is full (includes `Retry-After: 1` header). |
| `timeout` | `504 Gateway Timeout` | Query or batch exceeded the configured execution timeout (`sqlite3_interrupt`). |
| `internal_error` | `500` or `503` | Unexpected storage or control-plane error. |

---

## 6. Migration History & Upgrading from `0.0.1-beta`

Starting in `0.0.2-beta`, applied migration history is stored inside each SQLite database file in the internal `_litebase_migrations` table and recorded in the same atomic transaction as the migration DDL.

- **Automatic Upgrade**: When `0.0.2-beta` opens an existing `0.0.1-beta` database that does not yet have `_litebase_migrations`, it automatically creates `_litebase_migrations` and copies that database's applied migration records from `control.db`.
- **Portable State**: Because `_litebase_migrations` lives inside the `.sqlite` file, snapshots (`snapshot restore`), branches (`branch create`), and whole-file transfers (`litebase pull` / `litebase push --force`) carry their exact migration history with them.
- **Authorizer Protection**: User queries and batches cannot modify `_litebase_migrations` directly; only the internal migration runner can write to it.

---

## 7. Known Limitations

- **No interactive multi-request HTTP transactions**: Use `/v1/databases/{db}/batch` for atomic multi-statement transactions.
- **Copy-based branches**: `branch create` takes a single `VACUUM INTO` snapshot and copies it to the branch database path (`<parent>--<branch>`).
- **Three-way schema merge**: `branch merge` automatically merges new tables/indexes/triggers/views and additive `ALTER TABLE ... ADD COLUMN` changes. Non-additive column edits require a manual SQL migration (`litebase branch merge <branch> --as-migration`), and dropping tables requires `--allow-drop`.
- **Single-node architecture**: Designed for single-node vertical scale on local SSD/NVMe volumes.
- **Token granularity**: Tokens are scoped to `read`, `write`, or `admin` (project-wide or per-database).


