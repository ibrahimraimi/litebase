# Litebase

> The open infrastructure layer for production-grade SQLite.

Litebase is an open source platform that brings a developer-first control plane to SQLite, enabling you to run, manage, and host SQLite databases with a Git-like workflow.

## Core Features

- **SQLite as a Service**: Safe concurrent writes via queueing and enabled WAL mode.
- **RESTful API**: Execute SQL queries securely via HTTP.
- **Developer-First CLI**: Manage your local and remote databases seamlessly (`litebase init`, `litebase link`, `litebase deploy`).
- **Branching & Snapshots (Upcoming)**: Git-like database branching and instant snapshot/restore functionality.

## Architecture

Litebase is built primarily in Go within a monorepo structure:

- `apps/data-plane`: The core execution engine handling isolated SQLite instances, connection pooling, and write serialization.
- `apps/control-plane`: The cloud API managing users, projects, database provisioning, and API keys.
- `packages/cli`: The `litebase` command-line tool for local development and cloud interaction.
- `packages/sdk-go`: The official Go SDK.
- `internal/`: Shared Go packages across the planes.

## Documentation

For a deeper dive into the architecture and capabilities, see our locally hosted docs:

- [Core Engine](docs/core-engine.md)
- [CLI Commands](docs/cli-commands.md)
- [Git Setup](docs/git-setup.md)