# 0001: SQLite only

- **Status:** Accepted
- **Date:** 2026-11-09

## Context

Litebase began as a developer-first control plane for SQLite: run, manage and host SQLite databases with a Git-like workflow (migrations, snapshots, branches).

We considered widening the scope to a control plane for any database (PostgreSQL, MySQL and others), with SQLite as the default. After researching the landscape we decided against it:

- Managed and open-source tools already serve PostgreSQL and MySQL branching, hosting and schema workflows. There is no clear reason for Litebase to compete there.
- With SQLite, Litebase owns the database file and the write path. That is what makes queued safe writes, consistent snapshots and cheap branching possible. For a server database, Litebase would only be a layer over something someone else runs, which is a different product.

## Decision

Litebase supports **SQLite only**, on stock SQLite (not a fork). It does not define an abstraction layer for other database engines.

## Consequences

- Code can use SQLite specifics freely (WAL, `VACUUM INTO`, file-level copy, the authorizer callback).
- No engine interface exists, and none should be added speculatively.
- The product tagline stays "Developer-first control plane for SQLite".
- Reopening this decision needs a concrete user need and a new ADR that supersedes this one.
