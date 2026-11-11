# Architecture Decision Records

An ADR records one significant decision: what we chose, why, and what it costs us. They answer "why is it like this?" for contributors, and they stop old debates from being repeated.

## Index

| # | Decision | Status |
|---|---|---|
| [0001](0001-sqlite-only.md) | SQLite only | Accepted |
| [0002](0002-go-and-mattn-sqlite3.md) | Go, one binary, `mattn/go-sqlite3` | Accepted |
| [0003](0003-single-writer-actor-with-group-commit.md) | One writer goroutine per database, with group commit | Accepted |
| [0004](0004-no-interactive-transactions.md) | No interactive transactions over HTTP | Accepted |
| [0005](0005-wal-and-per-connection-pragmas.md) | WAL mode and per-connection pragmas | Accepted |
| [0006](0006-user-sql-defense-in-depth.md) | Defense in depth for user-supplied SQL | Accepted |
| [0007](0007-typed-json-value-encoding.md) | Typed JSON value encoding | Accepted |
| [0008](0008-hashed-scoped-api-tokens.md) | Hashed, scoped API tokens | Accepted |
| [0009](0009-deploy-applies-migrations-only.md) | `deploy` applies migrations, never uploads a database | Accepted |
| [0010](0010-migration-history-inside-the-database.md) | Migration history lives inside each database | Accepted |
| [0011](0011-branches-are-copies-schema-only-merge.md) | Branches are copies; merge is schema-only and three-way | Accepted |
| [0012](0012-snapshots-via-vacuum-into.md) | Snapshots use `VACUUM INTO`; restore keeps the original aside | Accepted |
| [0013](0013-backup-credentials-from-environment.md) | Backup credentials come from the environment | Accepted |
| [0014](0014-backup-remote-layout-and-commit-marker.md) | Remote backup layout with a metadata file written last | Accepted |
| [0015](0015-retention-protects-referenced-snapshots.md) | Retention never deletes a snapshot that is still needed | Accepted |
| [0016](0016-restore-verifies-and-rolls-back.md) | Restore verifies before it replaces; interrupted restore rolls back | Accepted |
| [0017](0017-pitr-via-litestream-deferred.md) | Point-in-time recovery will integrate Litestream | Proposed |

## Statuses

- **Proposed:** agreed direction, not built yet, or still open to change.
- **Accepted:** in effect, and reflected in the code.
- **Superseded by NNNN:** replaced. Keep the file; add the link.
- **Deprecated:** no longer relevant.

An ADR is never edited to change history. If a decision changes, write a new ADR and mark the old one superseded.

## When to write one

Write an ADR when a decision is hard to reverse, affects the public API or file formats, trades off security or durability, or when a contributor would otherwise ask "why did you do it this way?".

## Template

```markdown
# NNNN: Title

- **Status:** Proposed | Accepted | Superseded by NNNN
- **Date:** YYYY-MM-DD

## Context
What problem are we solving, and what constraints apply?

## Decision
What we chose, stated plainly.

## Alternatives considered
(Optional.) What else we looked at, and why not.

## Consequences
What gets easier, what gets harder, what we now must do or avoid.
```
