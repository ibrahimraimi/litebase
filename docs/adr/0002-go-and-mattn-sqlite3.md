# 0002: Go, one binary, `mattn/go-sqlite3`

- **Status:** Accepted
- **Date:** 2026-10-29

## Context

Litebase needs to be easy to self-host: one artifact that runs the server and the CLI. It needs a SQLite driver that exposes what the control plane relies on (authorizer callback, per-connection settings, `VACUUM INTO`, connection hooks).

## Decision

- Implement Litebase in **Go** as a single binary with subcommands (`serve`, `dev`, and the CLI commands).
- Use the standard library `net/http` for the API, `cobra` for the CLI and TOML for project configuration.
- Use **`github.com/mattn/go-sqlite3`** as the SQLite driver.

## Alternatives considered

- `modernc.org/sqlite` (pure Go, no CGO) and `ncruces/go-sqlite3` (CGO-free, WASM-based). Both make cross-compilation and static builds easier. Published benchmarks for the Go SQLite drivers disagree with each other, and none resemble an API server workload, so we did not choose on speed claims.

## Consequences

- The build needs **CGO**. Release binaries are built on native runners for each OS and architecture, not cross-compiled from one machine.
- `CGO_ENABLED=0` builds are not supported.
- Driver choice should stay contained so it can be revisited if cross-compilation or static builds become a real problem. Any switch needs a new ADR and our own benchmark on a realistic workload.
