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
| **[Usage & API Reference](./docs/USEAGE.md)** | End-to-end CLI workflows (`migrate`, `deploy`, `query`, `shell`, `snapshot`, `branch`) and `/v1` REST API reference. |
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

## Contributing & Security

- Read [CONTRIBUTING.md](./.github/CONTRIBUTING.md) before opening a pull request.
- Review [SECURITY.md](./.github/SECURITY.md) for our SQL security boundaries and private vulnerability reporting process.
