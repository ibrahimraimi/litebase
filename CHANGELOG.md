# Changelog

All notable changes to Litebase will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [0.0.2-beta] - 2026-10-29

### Changed

- Migration history now lives inside each database (`_litebase_migrations`) and is applied in the same transaction as the migration. Existing data directories are upgraded on first open.
- `branch merge` is now a three-way schema merge and never drops tables without `--allow-drop`. Added `--as-migration` to emit the schema diff as a numbered migration file.
- Branch creation takes a single consistent snapshot; branches are namespaced per parent (`<parent>--<branch>`).

### Fixed

- Triggers (`CREATE [TEMP|TEMPORARY] TRIGGER ... BEGIN ...; END;`) and `[bracketed]` identifiers can be created via `/query`, `/batch`, and migrations.
- Restore no longer blocks unrelated databases, drains in-flight requests before closing, and rolls back `.pre-restore` on rename failure.
- `VACUUM INTO` snapshots run on a dedicated connection so active writes are not stalled.

### Security

- Per-connection SQLite limits (`SQLITE_LIMIT_LENGTH`, `SQL_LENGTH`, `VARIABLE_NUMBER`, `COMPOUND_SELECT`, `EXPR_DEPTH`, `LIKE_PATTERN_LENGTH`, `ATTACHED`) and response byte budget bound memory use.
- Database size is enforced with `PRAGMA max_page_count`.
- `/metrics` requires authentication.
- Bootstrap admin token is created once if no admin token exists, not on every start.

---

## [0.0.1-beta] - 2026-10-29

### Added

- **Storage Engine (`internal/engine`)**:
  - SQLite WAL connection management with per-connection pragmas (`synchronous = NORMAL|FULL`, `foreign_keys = ON`, `busy_timeout = 5000`, `temp_store = MEMORY`).
  - Single-writer actor goroutine with bounded queue backpressure, `BEGIN IMMEDIATE` group commits, and per-job `SAVEPOINT` isolation.
  - Concurrent read-only connection pool (`mode=ro`, `PRAGMA query_only = ON`) with automatic statement routing.
  - Defense-in-depth `sqlite3_set_authorizer` security callback blocking `ATTACH`, `DETACH`, `load_extension`, `fts3_tokenizer`, internal `_litebase_*` table writes, and non-allowlisted `PRAGMA` statements.
  - Lossless 64-bit `TypedValue` JSON codec (`null`, `integer`, `float`, `text`, `blob`) and opt-in `plain` mode (`?format=plain`).
- **Control Plane & Auth (`internal/control`, `internal/auth`, `internal/manager`)**:
  - `control.db` schema for projects, databases, tokens, applied migrations, snapshots, branches, and append-only audit logs.
  - `lbk_`-prefixed 256-bit API tokens stored as SHA-256 hashes with `read`, `write`, and `admin` scopes, database binding, and expiration.
  - Multi-database lifecycle manager with lazy open, reference-counted idle eviction, storage quotas, and WAL size monitoring.
- **HTTP REST API (`internal/api`, `api/openapi.yaml`)**:
  - `/v1` endpoints for databases, `/query`, `/batch`, `/schema`, `/migrations`, `/snapshots`, `/restore`, `/download`, `/upload`, `/branches`, and `/tokens`.
  - RFC 9457 Problem Details (`application/problem+json`) error responses, per-token rate limiting (`Retry-After`), `/healthz`, `/readyz`, and `/metrics` Prometheus exporter.
- **Migrations, Snapshots & Branching (`internal/migrate`, `internal/snapshot`, `internal/branch`)**:
  - Ordered `NNNN_<name>.sql` migration runner with SHA-256 checksum verification and tamper detection.
  - Consistent online snapshots via `VACUUM INTO` and atomic restore with automatic `.pre-restore` backup.
  - Copy-based database branching with `sqlite_master` DDL schema diff and atomic schema merge.
- **CLI & Client (`cmd/litebase`, `internal/cli`, `internal/client`, `internal/config`)**:
  - Single-binary CLI with `serve`, `dev`, `init`, `link`, `migrate new|status`, `deploy [--dry-run]`, `query`, `shell`, `db create|list`, `snapshot create|list|restore`, `branch create|list|delete|checkout|diff|merge`, `pull`, and `push --force`.
  - Project configuration loader and writer (`litebase.toml`) in `internal/config`.
