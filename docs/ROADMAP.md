# Roadmap

Litebase is a **developer-first control plane for SQLite**: run, manage and host SQLite databases with a Git-like workflow.

This roadmap states intent, not promises. Order and scope can change. Shipped work is listed in [CHANGELOG.md](CHANGELOG.md). The reasoning behind major choices is in [docs/adr](docs/adr/README.md).

## Where we are

Litebase is in **beta (0.0.x)**. APIs, file formats and CLI commands can still change between releases. Do not run it as your only copy of important data.

## Releases

| Version | Theme | Status |
|---|---|---|
| 0.0.0 | Inception: positioning, scope and design | Done (no release) |
| 0.0.1-beta | It works | Shipped |
| 0.0.2-beta | Trust | Shipped |
| 0.0.3-beta | Survive | Shipped |
| 0.0.4-beta | Control | In progress |

### 0.0.0: Inception

Defined the product (a control plane for SQLite, not another SQLite server), what is out of scope (replication, consensus, a hosted offering), and the core design: a single writer per database with queued group commits, WAL mode, SQL over HTTP, and a CLI built around migrations, snapshots and branches.

### 0.0.1-beta: It works

- Single-node server hosting multiple databases, with a single-writer queue and WAL mode.
- HTTP API: `/query`, `/batch`, schema, migrations, snapshots, restore, branches and tokens.
- Typed JSON values that keep 64-bit integers and blobs lossless.
- Scoped API tokens, rate limiting, request limits and an SQL authorizer.
- CLI: `serve`, `dev`, `init`, `link`, `migrate`, `deploy`, `query`, `shell`, `snapshot`, `branch`, `pull`, `push`.
- Consistent snapshots, restore, and copy-based branches with schema diff.
- Docker image, CI and release tooling.

### 0.0.2-beta: Trust

Correctness and safety, no new features.

- Migration history moved inside each database and applied in the same transaction.
- Branch merge became a three-way schema merge that no longer drops data.
- Triggers can be created.
- Memory and size limits on queries and responses.
- Safer restore that does not block other databases.
- Authenticated `/metrics`, a one-time bootstrap token, and a token cache.

### 0.0.3-beta: Survive

The goal: if the disk dies or a restore goes wrong, you get your data back.

- **Crash-safe snapshot and restore.** An interrupted restore recovers on startup; snapshot files are written atomically.
- **Snapshot lifecycle.** Delete, prune, pin, checksums, and retention that never removes a snapshot a branch still needs.
- **Scheduled backups** per database, with a policy and skip-when-idle.
- **Remote backups** to S3-compatible storage (or a local directory), with integrity checks.
- **Restore from remote**, including onto an empty server, and restore into a new database name for inspection.
- **`backup verify`**, a restore drill that never touches the live database.
- Backup metrics and an operator guide.

Release gate: crash tests for snapshot and restore pass, and a full disaster-recovery drill works from an empty data directory.

### 0.0.4-beta: Control

The goal: give a token to one service or tenant without giving it the whole database.

- **Policy-backed tokens** (`allow`/`deny` statements with actions and resource patterns).
- **Table-level SQL access control** enforced in the SQLite authorizer callback, not by parsing SQL.
- **`litebase token explain`** and detailed `403` errors so policies are debuggable.
- **Backward-compatible** with existing `read` / `write` / `admin` tokens.
- **Litestream sidecar evaluation** and **copy-on-write storage note** (documents only).

Release gate: policy fuzz tests pass, view/trigger/CTE bypass tests pass, and all existing token tests pass unchanged.

## Next: 0.0.5 (tentative)

- **Adopt:** a tiny zero-dependency TypeScript client and a 5-minute quickstart that hides the typed-value wire format.
- **Litestream sidecar recipe** in `deploy/` if the 0.0.4 evaluation passes.

## Later (unscheduled, in rough order)

- Client-side encryption of remote backups (age or Tink).
- Orphan snapshot cleanup enabled by default.
- Token update / rotation without recreation.
- API freeze and a stable 1.0, with a documented upgrade path.
- Teams: organizations, roles and a small web dashboard.
- Copy-on-write branching (per the 0.0.4 storage note).
- Publish a prebuilt container image (today you build it yourself with `docker build -t litebase:latest .`).

## Not planned

- **Support for other databases** (PostgreSQL, MySQL and others). See [ADR 0001](/docs/adr/0001-sqlite-only.md).
- **Replication, consensus or multi-node clustering.**
- **Interactive transactions over HTTP.** Use `/batch`. See [ADR 0004](/docs/adr/0004-no-interactive-transactions.md).
- **Merging data between branches.** Merge is schema-only. See [ADR 0011](/docs/adr/0011-branches-are-copies-schema-only-merge.md).

## Influence the roadmap

Open an issue describing the problem you are trying to solve. Concrete use cases move things up the list faster than feature requests.
