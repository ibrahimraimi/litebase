# Setup & Configuration

This guide covers how to configure a Litebase server (`litebase serve`), run a local development instance (`litebase dev`), manage scoped API tokens, and structure your project configuration (`litebase.toml`).

---

## 1. Starting the Server

Litebase provides two server modes in the same binary:

### Production / Self-Hosted Mode (`litebase serve`)

`litebase serve` starts the HTTP control-plane server and initializes `control.db` inside `--data-dir`.

```bash
export LITEBASE_BOOTSTRAP_TOKEN="lbk_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
litebase serve \
  --addr 127.0.0.1:8080 \
  --data-dir /var/lib/litebase
```

| Flag | Default | Description |
|---|---|---|
| `--addr` | `127.0.0.1:8080` | TCP address (`host:port`) the HTTP server binds to. |
| `--data-dir` | `./litebase-data` | Root directory for `control.db`, database files, and snapshots. |
| `--tls-cert` | `""` | Path to a PEM-encoded TLS certificate file (enables HTTPS when paired with `--tls-key`). |
| `--tls-key` | `""` | Path to a PEM-encoded TLS private key file. |

> [!TIP]
> If you omit `LITEBASE_BOOTSTRAP_TOKEN` on startup, Litebase automatically generates a fresh random `lbk_...` admin token and prints it once to standard output:
> ```text
> Litebase listening on 127.0.0.1:8080 (data-dir: ./litebase-data)
> Bootstrap admin token (shown once): lbk_9f8e7d6c5b4a...
> ```

### Local Development Mode (`litebase dev`)

For rapid local prototyping, `litebase dev` starts an embedded server bound to `127.0.0.1:8080`, stores state in `<work-dir>/.litebase/dev-data`, and automatically provisions the `main` database:

```bash
litebase dev --addr 127.0.0.1:8080
```

---

## 2. Environment Variables & Global Flags

Litebase separates **project configuration** (committed to Git in `litebase.toml`) from **credentials** (injected via environment variables or CLI flags).

### Environment Variables

| Variable | Used By | Description |
|---|---|---|
| `LITEBASE_BOOTSTRAP_TOKEN` | `litebase serve`, `litebase dev` | Pre-seeds a deterministic `admin`-scoped token on server startup. Must start with `lbk_` and be at least 13 characters long. |
| `LITEBASE_TOKEN` | All remote CLI commands | Bearer token (`lbk_<hex>`) used by the CLI to authenticate against the remote Litebase server. |

### Global CLI Flags

Every CLI subcommand accepts these persistent flags, which take precedence over `litebase.toml` and environment variables:

| Flag | Default | Description |
|---|---|---|
| `--work-dir` | `.` | Project working directory containing `litebase.toml` and `migrations/`. |
| `--url` | *(from `litebase.toml`)* | Override the remote Litebase server URL (e.g., `https://db.example.com`). |
| `--token` | *(from `LITEBASE_TOKEN`)* | Override the API Bearer token for a single command invocation. |
| `--database` | *(from `litebase.toml`)* | Override the target database or branch name (e.g., `main` or `feature-auth`). |

---

## 3. Authentication & Scoped Tokens

All `/v1/*` endpoints require a Bearer token in the `Authorization` header:

```http
Authorization: Bearer lbk_<token>
```

Tokens are 256-bit cryptographically random secrets prefixed with `lbk_`. Only the **SHA-256 hash** and a 12-character visible prefix (`lbk_xxxxxxxx`) are stored in `control.db`.

### Token Scopes

| Scope | Capabilities |
|---|---|
| `read` | Read database metadata, inspect schema (`/schema`), list applied migrations, and run read-only `SELECT` queries (`/query`). Write statements are rejected by both the HTTP handler and the SQLite connection authorizer. |
| `write` | All `read` permissions plus write queries (`INSERT`, `UPDATE`, `DELETE`, DDL), atomic batches (`/batch`), migration application (`/migrations/apply`), snapshots, and branches. |
| `admin` | Full control plane access: create/delete databases, issue/revoke tokens (`/v1/tokens`), plus all `write` and `read` permissions across all databases. |

### Issuing a Scoped Database Token

Use an `admin` token to create least-privilege tokens for your application services:

```bash
# Issue a write-scoped token bound strictly to the "main" database, expiring in 24 hours (86400s)
curl -s -X POST http://127.0.0.1:8080/v1/tokens \
  -H "Authorization: Bearer $LITEBASE_BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "database": "main",
    "scopes": ["read", "write"],
    "expires_in_seconds": 86400
  }'
```

Response (`201 Created`):

```json
{
  "id": "tok_a1b2c3d4e5f60718",
  "token": "lbk_4f9a2c8e1d7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a10",
  "prefix": "lbk_4f9a2c8e",
  "database": "main",
  "scopes": ["read", "write"],
  "expires_at": "2026-10-30T15:00:00Z"
}
```

> [!WARNING]
> Copy the `token` field immediately. Litebase never stores the plaintext token and cannot display it again.

### Revoking a Token

```bash
curl -s -X DELETE http://127.0.0.1:8080/v1/tokens/tok_a1b2c3d4e5f60718 \
  -H "Authorization: Bearer $LITEBASE_BOOTSTRAP_TOKEN"
```

---

## 4. Project Configuration (`litebase.toml`)

Running `litebase init` in your application repository creates `litebase.toml`, a `migrations/` folder, and a local development SQLite database at `.litebase/local.sqlite`.

```toml
# Litebase project configuration
# Set LITEBASE_TOKEN in your environment; never commit tokens to version control.

[project]
  name = "myapp"

[remote]
  url = "http://127.0.0.1:8080"
  database = "main"

[migrations]
  dir = "migrations"
```

| Key | Description |
|---|---|
| `project.name` | Logical name of your application project. |
| `remote.url` | Base URL of your remote Litebase server (updated via `litebase link <url>`). |
| `remote.database` | Active target database or branch name (updated via `litebase link --database` or `litebase branch checkout`). |
| `migrations.dir` | Relative path to the directory containing ordered `NNNN_<name>.sql` migration files. |

Add `.litebase/` to your project's `.gitignore` while committing `litebase.toml` and `migrations/` to Git:

```gitignore
.litebase/
litebase-data/
```

---

## 5. On-Disk Data Directory Layout

Litebase organizes all state inside `--data-dir` (e.g., `/var/lib/litebase`):

```text
/var/lib/litebase/
├── control.db                                         # Control-plane metadata & audit log
├── databases/
│   └── default/
│       ├── main/
│       │   ├── main.sqlite                            # Primary database file
│       │   ├── main.sqlite-wal                        # Write-Ahead Log
│       │   └── main.sqlite-shm                        # WAL shared-memory index
│       └── feature-auth/                              # Branch database (isolated actor & WAL)
│           └── main.sqlite
└── snapshots/
    └── main/
        └── 20261029T150405.000000000Z-pre-deploy.sqlite
```

---

## 6. Production Operations & Observability

### Direct TLS vs. Reverse Proxy

- **Reverse Proxy (Recommended)**: Run `litebase serve --addr 127.0.0.1:8080` behind Caddy, Nginx, Cloudflare Tunnel, or Fly Proxy for automatic TLS certificate management.
- **Built-in TLS**: Pass `--tls-cert /path/to/fullchain.pem --tls-key /path/to/privkey.pem` to terminate TLS directly in the Go server.

### Health, Readiness & Metrics Endpoints

Unauthenticated operational endpoints are exposed at the root path:

| Endpoint | Success Status | Purpose |
|---|---|---|
| `GET /healthz` | `200 OK` (`{"status":"ok"}`) | Liveness probe confirming the HTTP process is up. |
| `GET /readyz` | `200 OK` (`{"status":"ready"}`) | Readiness probe verifying `control.db` can execute queries (`503` on failure). |
| `GET /metrics` | `200 OK` (`text/plain; version=0.0.4`) | Prometheus metrics (`litebase_queries_total`, `litebase_batches_total`, `litebase_http_requests_total`, `litebase_db_size_bytes`, `litebase_wal_size_bytes`, `litebase_wal_checkpoint_alert`, `litebase_queue_depth`, `litebase_batch_size`, `litebase_commit_latency_seconds_total`). |

### Graceful Shutdown

When `litebase serve` receives `SIGINT` or `SIGTERM`:
1. The HTTP listener stops accepting new connections and waits up to 10 seconds for in-flight HTTP requests to finish.
2. Each open database drains its writer actor queue.
3. Every database runs `PRAGMA wal_checkpoint(TRUNCATE)` before closing its SQLite connections cleanly.

---

## Next Steps

- Continue to **[Usage & API Reference](./USEAGE.md)** for step-by-step CLI workflows (`migrate`, `deploy`, `snapshot`, `branch`) and full `/v1` HTTP endpoint examples.

