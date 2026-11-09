# 0013: Backup credentials come from the environment and are never stored

- **Status:** Proposed (planned for v0.0.3-beta)
- **Date:** 2026-11-09

## Context

Off-box backups need credentials for S3-compatible storage. The control database currently holds only hashes (ADR 0008), so a leaked copy of it does not leak usable secrets. Storing storage credentials there would change that: they must be usable, so they must be recoverable.

## Decision

- Remote storage credentials are read from **environment variables** (`LITEBASE_S3_ENDPOINT`, `_REGION`, `_BUCKET`, `_PREFIX`, `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY`).
- They are **never** written to the control database, project files or logs.
- Per-database credentials are not supported.

## Consequences

- Operators manage secrets with their platform's own mechanism.
- Rotating credentials means restarting the server.
- The control database stays free of reversible secrets.
- A backup of the control database still contains token hashes and the audit log, so the bucket must be private. This must be stated in the docs.
