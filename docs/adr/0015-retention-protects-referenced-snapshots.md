# 0015: Retention never deletes a snapshot that is still needed

- **Status:** Accepted
- **Date:** 2026-11-09

## Context

Snapshots grow without bound today: every restore and every push creates one, and nothing removes any. Retention is needed. But a branch merge reads the branch's parent snapshot as its merge base (ADR 0011), so deleting that snapshot would break the merge.

## Decision

- Snapshots have a **kind**: `manual`, `scheduled`, `pre-restore`, `pre-push`, `branch`.
- Retention automatically deletes only `scheduled` snapshots, plus a separate small cap for `pre-restore` and `pre-push`. **`manual` snapshots are never deleted automatically.**
- A scheduled snapshot is deleted only if it is outside the newest `keep_last`, older than `keep_days`, **not pinned**, and **not the parent snapshot of any branch**. Branch references are computed with a join on the branches table, not stored in a flag that can go stale.
- Deleting a snapshot also deletes its remote copy when the policy uses remote storage.
- `snapshot prune --dry-run` shows what would be deleted.

## Consequences

- Disk use stays bounded without risking merges or manual restore points.
- Retention logic needs table-driven tests that cover branches, pins, mixed kinds and dry runs.
