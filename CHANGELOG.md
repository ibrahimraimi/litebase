# Changelog

All notable changes to Litebase will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Added

- POSIX `sh` CLI installer (`install.sh`) with prerelease discovery, mandatory SHA-256 checksum verification (`checksums.txt`), musl/Windows safety checks, `--dry-run`, atomic binary replacement, and end-to-end installer test suite (`scripts/test-install.sh`).
- Installer operations guide (`docs/operations/install.md`) covering flags, environment variables, supported platforms, verification, trust model, and uninstallation.

### Changed

- Updated RFC 9457 Problem Details `type` URI prefix from `https://litebase.dev/problems/` to `https://litebase.tech/problems/`.

### Fixed

- `install.sh` now resolves the default release version from GitHub's `releases.atom` feed first (with SemVer comparison and automatic fallback to the REST API), avoiding HTTP `403` unauthenticated REST API rate-limit failures on shared IPs.

---

## [0.0.3-beta] - 2026-11-09

### Added

- Snapshot lifecycle: delete, prune (`--keep-last`, `--keep-days`, `--dry-run`), pin (`--pin`), SHA-256 checksums, and protection for snapshots referenced by active branches (`S5`, `S6`, `F1`).
- Per-database backup scheduler (`litebase backup policy set|show|disable`, `litebase backup run|status`) with skip-when-idle, jitter, exponential backoff, and concurrency cap (`F2`).
- Remote backups to S3-compatible object storage (`LITEBASE_S3_*`) or local directory (`LITEBASE_BACKUP_DIR`), with streaming gzip compression (`.sqlite.gz` default, `--backup-compress=none` to disable), `.meta.json` commit markers, orphan cleanup, and `control.db` backups (`F3`).
- Restore from remote storage (`litebase snapshot restore --from-remote <key|latest>`), restore into a new database (`--as <new-db>`), and control-plane disaster recovery (`litebase restore-control --from-remote <key|latest>`) (`F4`).
- `litebase backup verify` and `POST /v1/databases/{db}/backups/verify`: non-destructive restore drill (`PRAGMA integrity_check`, `PRAGMA foreign_key_check`, `_litebase_migrations` inspection, and optional `--sample` row counts) (`F5`).
- Backup and snapshot Prometheus metrics (`litebase_backup_last_success_timestamp_seconds`, `litebase_backup_last_duration_seconds`, `litebase_backup_failures_total`, `litebase_backup_upload_bytes_total`, `litebase_snapshots`, `litebase_snapshot_bytes`) (`F6`).

### Changed

- `snapshots` and `databases` paths are stored relative to `--data-dir` (`rel_path` in `control.db`, `user_version = 1`), so moving `--data-dir` works without rewriting metadata (`S4`).
- Automatic `pre-restore` and `pre-push` snapshots are capped (`keep_last = 5`, `keep_days = 7`) while `manual` and `branch` snapshots are never auto-pruned (`S5`).

### Fixed

- Startup recovers databases interrupted mid-restore (`.pre-restore`, `.restore-staging`) and records a `database.restore.recovered` audit event (`S1`).
- Snapshots write to `.tmp`, run `PRAGMA integrity_check` read-only, fsync the file and parent directory via `engine.AtomicReplace`, and rename into place before recording metadata (`S2`, `S3`).

### Security

- Snapshot and remote restore verify SHA-256 and `PRAGMA integrity_check` in a staging file before closing or replacing any live database (`S6`, `F4`).
- S3 credentials are read from environment variables only and never stored in `control.db`, `litebase.toml`, logs, or `/metrics`.

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
