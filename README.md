# Litebase

**The open-source, developer-first control plane for SQLite.**

Run, manage, and self-host SQLite databases with a Git-like workflow—**migrations, database branches, online snapshots, and a 64-bit safe HTTP REST API**—packaged in a single Go binary.

Explore the full documentation in [`/docs`](./docs/README.md).

---

## Documentation

| Document | Contents |
|---|---|
| **[Overview & Architecture](./docs/README.md)** | Concurrency model, durability modes (`normal` vs. `full`), security model, and design boundaries. |
| **[Installation Guide](./docs/INSTALLATION.md)** | Source builds, Docker container setup, and verification commands. |
| **[Setup & Configuration](./docs/SETUP.md)** | `litebase serve` vs. `litebase dev`, `litebase.toml`, scoped tokens (`read`, `write`, `admin`), TLS, and storage layout. |
| **[Usage & API Reference](./docs/USAGE.md)** | End-to-end CLI workflows (`migrate`, `deploy`, `query`, `shell`, `snapshot`, `branch`) and `/v1` REST API reference. |
| **[Backups & Disaster Recovery](./docs/operations/backups.md)** | Scheduled local and S3 backups, retention policies, `backup verify` restore drills, and full-host recovery. |
| **[OpenAPI 3.1 Spec](./api/openapi.yaml)** | Full OpenAPI contract for all `/v1` endpoints and RFC 9457 Problem Details responses. |

---

## Architecture

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

---

## Durability & Backups

- **What is protected:**
  - **Process and OS crashes:** Every database runs in WAL mode with `PRAGMA foreign_keys = ON` and configurable `durability = "normal"` (`synchronous = NORMAL`) or `durability = "full"` (`synchronous = FULL`). Snapshot and restore file swaps use `fsync` on both files and parent directories, and interrupted restores roll back cleanly on startup.
  - **Bad deploys and operator mistakes:** `litebase push` and `litebase snapshot restore` automatically create `pre-push` and `pre-restore` snapshots before mutating a database.
  - **Full disk or host loss (when remote backups are enabled):** Per-database backup policies (`litebase backup policy set <db> --remote`) upload compressed, SHA-256-verified snapshots of user databases and `control.db` to S3-compatible storage, restorable onto an empty server via `litebase restore-control` and `litebase snapshot restore --from-remote`.
- **What is not protected:**
  - **Writes committed after the last backup:** Backups are periodic `VACUUM INTO` snapshots, not continuous WAL replication. If a host disk is permanently lost, transactions committed after the last completed remote backup are lost.

See the **[Backups & Disaster Recovery Guide](./docs/operations/backups.md)** for configuration, retention rules, and restore drills (`litebase backup verify`).

---

## Known Limitations

- **No interactive HTTP transactions**: `BEGIN`, `COMMIT`, `ROLLBACK`, `SAVEPOINT`, and `RELEASE` over `/query` are rejected so slow clients cannot hold the single SQLite writer lock. Use `/batch` for multi-statement atomic transactions.
- **Copy-based branches**: Branches are created from a consistent `VACUUM INTO` snapshot rather than page-level copy-on-write, which is ideal for small-to-medium SQLite databases.
- **Single-node control plane**: Litebase runs on a single host backed by local disk/NVMe and does not include distributed Raft/Paxos replication.
- **Token scopes**: Bearer tokens support `read`, `write`, and `admin` scopes (optionally bound to a single database), not row-level or table-level ACLs.

---

## Contributing & Security

- Read [CONTRIBUTING.md](./.github/CONTRIBUTING.md) before opening a pull request.
- Review [SECURITY.md](./.github/SECURITY.md) for our SQL security boundaries and private vulnerability reporting process.
