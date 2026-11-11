# 0013: Backup credentials come from the environment and are never stored

- **Status:** Accepted
- **Date:** 2026-11-09

## Context

Off-box backups need credentials for S3-compatible storage. The control database currently holds only hashes (ADR 0008), so a leaked copy of it does not leak usable secrets. Storing storage credentials there would change that: they must be usable, so they must be recoverable.

## Decision

- Remote storage configuration and credentials are read from **environment variables** (`LITEBASE_S3_ENDPOINT`, `LITEBASE_S3_REGION`, `LITEBASE_S3_BUCKET`, `LITEBASE_S3_PREFIX`, `LITEBASE_S3_ACCESS_KEY_ID`, `LITEBASE_S3_SECRET_ACCESS_KEY`, or `LITEBASE_BACKUP_DIR` for a local directory store).
- Setting `LITEBASE_S3_*` or `LITEBASE_BACKUP_DIR` configures the server's `BlobStore`, **not** an automatic backup schedule on every database. Scheduled backups are explicitly **opt-in per database** via `litebase backup policy set` (or `PUT /v1/databases/{db}/backup-policy`) so scratch and branch databases do not incur unexpected background I/O or bucket egress.
- Credentials are **never** written to the control database, project files, error responses, or structured logs.
- Per-database credentials are not supported.

## Consequences

- Operators manage secrets with their platform's own mechanism.
- Rotating credentials means restarting the server.
- The control database stays free of reversible secrets.
- A backup of the control database still contains token hashes and the audit log, so the bucket must be private. This must be stated in the docs.
