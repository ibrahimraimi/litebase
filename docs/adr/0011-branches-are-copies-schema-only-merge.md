# 0011: Branches are file copies; merge is a schema-only, three-way merge

- **Status:** Accepted
- **Date:** 2026-10-29 (v0.0.2-beta; supersedes the original two-way diff)

## Context

Git-like branching for a database is the headline idea of the project. True copy-on-write branching needs storage-level support and is a large piece of work. Merging **data** between two databases has no safe general answer. The first implementation compared the two schemas directly and emitted `DROP TABLE` plus `CREATE TABLE` for any changed table, which deleted every row in it.

## Decision

- A **branch is a consistent copy** of its parent, taken from a single snapshot. Cost is proportional to database size.
- Branches are registered under a **namespaced name** (`<parent>--<branch>`), so two parents can each have a branch called `dev`.
- The parent snapshot is recorded (`ParentSnapshotID`).
- **Merge is schema-only and three-way**: it compares the parent snapshot (base), the parent now (ours), and the branch (theirs), and applies only the branch's changes.
  - New tables and appended columns are applied. Indexes, triggers and views are recreated.
  - If the same object changed on both sides, the merge aborts and changes nothing.
  - Dropping a table requires an explicit `--allow-drop`.
  - Anything it cannot do safely (removing or retyping a column) fails with a message asking for a manual migration.
- `branch merge --as-migration` writes the change as a numbered migration file instead of applying it, so it can be reviewed in Git.
- **Data is never merged.**

## Consequences

- A branch of a very large database is slow and uses full disk space. Copy-on-write is future work.
- Anything that deletes snapshots must not delete one that a branch still needs as its base (see ADR 0015).
- Preview databases per pull request are possible, with schema changes shipped back as migrations.
