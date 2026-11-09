# 0016: Restore verifies before it replaces, and an interrupted restore rolls back

- **Status:** Proposed (planned for v0.0.3-beta)
- **Date:** 2026-11-09

## Context

Restore moves the live database aside and renames a staged copy into place (ADR 0012). If the process dies between those steps, only `.pre-restore` and `.restore-staging` remain, and the database file is missing. A restored file that is corrupt, or truncated by a missed `fsync`, is worse than no restore.

## Decision

- **Verify first.** Before replacing anything, check the SHA-256 checksum and run `PRAGMA integrity_check` on the staged file. A mismatch aborts and leaves the live database untouched.
- **Durable moves.** Every file moved into place is written, `fsync`ed, renamed, and the parent directory is `fsync`ed. This is one shared helper.
- **Atomic snapshot files.** Snapshots are written to a temporary name, verified, then renamed. Temporary files are cleaned up on startup.
- **Startup recovery rolls back, never forward.** On start, for each database path:
  - staging exists and the database exists: delete the staging file;
  - the database is missing and `.pre-restore` exists: restore `.pre-restore` and log a warning;
  - both exist: a restore completed, keep `.pre-restore`.
- `snapshot restore --as <new-db>` restores next to the live database without touching it, for inspection.

## Consequences

- An interrupted restore is simply "not done". The operator runs it again.
- Crash tests with named failpoints (kill after each swap step, during staging, during snapshot) are required, not optional.
- `backup verify` (restore the latest backup into a temp directory and check it) is the operational counterpart.
