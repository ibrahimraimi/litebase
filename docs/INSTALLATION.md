# Installation

Litebase ships as a **single binary** (`litebase`) that includes both the HTTP control-plane server (`litebase serve`, `litebase dev`) and the developer CLI (`init`, `link`, `migrate`, `deploy`, `query`, `shell`, `db`, `snapshot`, `branch`).

---

## System Requirements

| Requirement | Minimum Version / Notes |
|---|---|
| **Operating System** | Linux (`amd64`, `arm64`), macOS (`amd64`, `arm64`), or Windows via WSL2 |
| **Go Toolchain** *(source builds)* | Go `1.23` or newer |
| **C Compiler** *(CGO)* | `gcc` or `clang` (required by `github.com/mattn/go-sqlite3`) |
| **Storage** | Local POSIX filesystem (SSD/NVMe recommended). **Do not** place the data directory on NFS or SMB network shares, as SQLite WAL requires host-local shared memory (`-shm`). |

---

## Option 1: Build from Source

### 1. Clone the repository

```bash
git clone https://github.com/litebase/litebase.git
cd litebase
```

### 2. Build the binary

Use the provided `Makefile` target to compile `./bin/litebase` with `CGO_ENABLED=1` and stripped build paths:

```bash
make build
```

Or run `go build` directly:

```bash
CGO_ENABLED=1 go build -trimpath -o bin/litebase ./cmd/litebase
```

### 3. Install into your `$PATH` *(optional)*

```bash
sudo install -m 0755 bin/litebase /usr/local/bin/litebase
```

### 4. Verify the installation

```bash
litebase --help
```

Expected output:

```text
Developer-first control plane for SQLite

Usage:
  litebase [command]

Available Commands:
  branch      Manage Git-like database branches (create, list, delete, checkout, diff, merge)
  completion  Generate the autocompletion script for the specified shell
  db          Manage remote SQLite databases
  deploy      Apply pending local migrations to the remote database
  dev         Run an embedded local development server on 127.0.0.1
  help        Help about any command
  init        Initialize litebase.toml, ./migrations/, and a local SQLite database
  link        Link local project to a remote Litebase server URL
  migrate     Manage SQL schema migrations
  query       Execute an ad-hoc SQL statement against the local or remote database
  serve       Run the Litebase HTTP API server
  shell       Start an interactive SQL REPL
  snapshot    Create, list, and restore consistent database snapshots

Flags:
      --database string   Override target database name
  -h, --help              help for litebase
      --token string      Override Litebase API token
      --url string        Override remote Litebase server URL
      --work-dir string   Project working directory (default ".")
```

---

## Option 2: Run with Docker

Litebase provides a multi-stage `Dockerfile` based on `gcr.io/distroless/base-debian12:nonroot`. It runs as a non-root user (`nonroot:nonroot`), exposes port `8080`, and persists database state in `/var/lib/litebase`.

### 1. Build the Docker image

```bash
docker build -t litebase:latest .
```

### 2. Create a persistent Docker volume

```bash
docker volume create litebase-data
```

### 3. Start the container

> [!IMPORTANT]
> Inside a container, pass `--addr 0.0.0.0:8080` so the server accepts connections across the Docker bridge network.

```bash
docker run -d \
  --name litebase \
  -p 127.0.0.1:8080:8080 \
  -v litebase-data:/var/lib/litebase \
  -e LITEBASE_BOOTSTRAP_TOKEN="lbk_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" \
  litebase:latest \
  serve --addr 0.0.0.0:8080 --data-dir /var/lib/litebase
```

### 4. Verify container health

```bash
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}

curl -s http://127.0.0.1:8080/readyz
# {"status":"ready"}
```

---

## Running the Verification Suite

If you are building Litebase from source or contributing changes, run the built-in verification targets in `Makefile`:

| Target | Command | Purpose |
|---|---|---|
| `make vet` | `go vet ./...` | Static analysis across all packages |
| `make test` | `go test ./...` | Unit, integration, crash (`kill -9`), and end-to-end tests |
| `make race` | `go test -race ./...` | Full test suite with the Go race detector enabled |
| `make bench` | `go test -bench=. -benchmem ./internal/engine/...` | Group commit vs. single-tx write benchmarks and concurrent WAL read/write benchmarks |
| `make fmt` | `gofmt -s -w cmd/ internal/ test/` | Format all Go source files |

---

## Next Steps

- Proceed to **[Setup & Configuration](./SETUP.md)** to configure authentication tokens, project files (`litebase.toml`), TLS, and storage layout.
- Or jump straight to **[Usage & API Reference](./USEAGE.md)** to create databases, run migrations, and branch schemas.

