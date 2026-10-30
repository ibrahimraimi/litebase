# Litebase v0.0.2-beta: "Trust" release

> Base: `v0.0.1-beta` (commit `45a863b`). Theme: **correctness and safety, no new features.**
> Findings come from reading the code. The Go module proxy was unreachable when this was written, so nothing was built or run. **Reproduce each item with a failing test before fixing it.**

## 1. Goal and non-goals

**Goal:** a user can create, branch, migrate, snapshot and restore a database without losing data or ending up with silently wrong migration state.

**Release gate:** all P0 tests below pass under `go test -race ./...`, CI is green, and the Docker quickstart works from a clean machine.

**Non-goals (defer to 0.0.3+):** copy-on-write branching, replication, web UI, TypeScript SDK, Litestream integration, multi-project support.

## 2. Priority summary

| ID | Pri | Area | Problem | Files |
|---|---|---|---|---|
| A1 | P0 | Migrations | History lives in `control.db`, not in the database file | `internal/migrate/migrate.go`, `internal/control/artifacts.go` |
| A2 | P0 | Branching | `merge` emits `DROP TABLE`, which deletes data | `internal/migrate/migrate.go` (`GenerateSchemaDiffSQL`), `internal/branch/branch.go` |
| A3 | P0 | Branching | Branch is copied twice at different instants; name collisions | `internal/branch/branch.go` |
| A4 | P0 | SQL parsing | Triggers cannot be created (splitter breaks on `;` inside `BEGIN…END`) | `internal/engine/sqlparse.go` |
| B1 | P1 | Security | Unbounded blob/row size, soft quota | `internal/engine/engine.go`, `query.go`, `manager.go` |
| B2 | P1 | Performance | One `SELECT` + `UPDATE` on `control.db` per request | `internal/control/tokens.go`, `internal/api/middleware.go` |
| B3 | P1 | Availability | `VACUUM INTO` runs on the writer actor, stalling writes | `internal/engine/database.go` |
| B4 | P1 | Safety | Restore holds global lock, ignores in-flight requests, no rollback | `internal/manager/manager.go` |
| B5 | P1 | Security | New admin token on every `serve` start | `internal/cli/cmd_serve.go` |
| B6 | P1 | Security | `/metrics` unauthenticated, exposes DB names | `internal/api/metrics.go` |
| C1 | P2 | Release | Module path mismatch | `go.mod` |
| C2 | P2 | Release | CGO cross-compile in GoReleaser; no release workflow | `.goreleaser.yml`, `.github/workflows/` |
| C3 | P2 | Docker | Default bind address; volume ownership (verify) | `Dockerfile` |
| C4 | P2 | Docs | `USEAGE.md` typo; docs gaps | `docs/` |

Suggested order: **A1 → A2 → A3 → A4 → B1 → B2 → C1**, then cut the release. The rest can move to 0.0.3 unless you are demoing publicly soon.

---

## 3. P0: data loss and wrong state

### A1. Move migration history into each database

**Problem.**
- `ApplyPending` runs the SQL through `ExecuteBatch`, then calls `recordAppliedList`, which writes to `control.db`. These are two separate steps. A crash between them leaves the schema migrated but unrecorded, and the next `deploy` fails on `CREATE TABLE`.
- History is keyed by `database_id` in `control.db`, so it does not travel with the data:
  - A **branch** has no history, so `deploy` re-applies `0001` onto an already-migrated schema.
  - A **restore** to an older snapshot keeps the newer history, so `deploy` skips migrations the schema no longer has.
  - **`push`/`pull`** moves the file but not its history.

**Fix.**
1. Add a table inside every user database:
   ```sql
   CREATE TABLE IF NOT EXISTS _litebase_migrations (
     version    TEXT PRIMARY KEY,
     name       TEXT NOT NULL,
     checksum   TEXT NOT NULL,
     applied_at TEXT NOT NULL,
     applied_by TEXT NOT NULL
   );
   ```
2. The authorizer already denies writes to `_litebase_*`. Add a privileged internal path for the migration runner (the writer already has a `isPrivileged` flag used by `RunMaintenance`). The migration SQL and the history `INSERT` must run in **one transaction**.
3. `BuildPlan` reads history from the database itself.
4. Keep `control.db.migrations_applied` as a read-only audit copy, or drop it. Do not use it for decisions.
5. One-time upgrade: on open, if `_litebase_migrations` is missing but `control.db` has history for this database, copy it in. Document that 0.0.1-beta data dirs are upgraded in place.

**Acceptance tests.**
- Crash injection (commit hook returns an error) leaves neither schema nor history applied.
- Create branch → `deploy` on the branch is a no-op when nothing is pending.
- Apply `0001..0003`, snapshot after `0002`, restore → `migrate status` reports `0003` pending.
- `pull` a database, open the file with plain `sqlite3`, and see `_litebase_migrations`.

### A2. Make `branch merge` safe

**Problem.** `GenerateSchemaDiffSQL(parent, branch)` compares `sqlite_master` text. When a table's SQL differs it emits `DROP TABLE IF EXISTS x; CREATE TABLE x (...)`, and `MergeSchema` applies that to the parent. A branch that adds one column therefore **deletes every row** in that parent table. The diff is also two-way: tables created in the parent after branching are dropped by a merge.

**Fix.**
1. Make it a three-way diff: `base` = the parent snapshot recorded in `ParentSnapshotID`, `ours` = parent now, `theirs` = branch.
2. Compute `delta = diff(base, theirs)` and apply only that to the parent.
3. Conflict rule: if the same object changed in both `base→ours` and `base→theirs`, abort with a clear error and change nothing.
4. For tables, generate only:
   - new table → its `CREATE TABLE`
   - column appended at the end → `ALTER TABLE … ADD COLUMN …`
   - index/trigger/view changes → drop and recreate (no data loss)
   - dropped table in the branch → require an explicit `--allow-drop` flag
   - anything else (column removed, retyped, constraint changed) → fail with "needs a manual migration"
5. Prefer `litebase branch merge --as-migration` which writes `migrations/NNNN_merge_<branch>.sql` instead of applying directly, so the change is reviewable in Git and recorded by A1.
6. `diff` and `merge` must use the same code path. Show the same output in both.

**Acceptance tests.**
- Add a column on the branch and insert rows in the parent, then merge. Rows survive and the column exists.
- Create a table in the parent after branching, then merge. The table survives.
- The same table altered differently on both sides: merge aborts, parent unchanged.
- Table dropped on the branch without `--allow-drop`: refused.

### A3. Branch creation: one consistent copy, and sane names

**Problem.**
- `branch.Create` runs `snapSvc.Create` (a `VACUUM INTO`) and then `VacuumInto` again to produce the branch file. Writes landing between the two mean the branch does not match `ParentSnapshotID`, which A2 relies on.
- A branch is registered as a top-level database named by the branch name. Two parents cannot both have a branch called `dev`, and a branch cannot share a name with an existing database.

**Fix.**
1. Take the snapshot once, then copy that file to the branch path.
2. Register branch databases under an internal name such as `<parent>--<branch>` (update `ValidateName` rules or store a separate `display_name`). Keep `litebase branch checkout <branch>` mapping to that name.
3. Reject branch names that collide with `--` handling.

**Acceptance tests.**
- Concurrent writer during `branch create`: the branch contents equal the snapshot contents byte for byte (`sqlite3 .dump` diff).
- Two parents each create branch `dev`. Both succeed and stay isolated.

### A4. Support triggers (statement splitter)

**Problem.** `SplitSQLStatements` splits on every `;` outside quotes and comments. `CREATE TRIGGER … BEGIN UPDATE …; END;` is cut into invalid pieces, and `ValidateSingleStatement` rejects the full text as "multiple statements". Migrations containing triggers fail. This splitter also guards against statement smuggling, so it is part of the security boundary.

**Fix.**
1. Track whether the current statement began with `CREATE [TEMP|TEMPORARY] TRIGGER`. If so, only terminate on a `;` whose preceding token is `END` (at the trigger's own nesting level).
2. Also handle `[bracketed]` identifiers, which SQLite accepts and the splitter currently ignores.
3. Add a differential fuzz test: for random inputs, compare `hasMultipleStatements` against what SQLite actually accepts, and assert the splitter never says "single" for input SQLite would run as multiple statements.

**Acceptance tests.**
- A migration with a trigger applies.
- `/query` and `/batch` accept one `CREATE TRIGGER` statement.
- `SELECT [a;b] FROM t` is treated as one statement.
- Fuzz corpus added to the nightly job.

---

## 4. P1: security and robustness

### B1. Bound memory and size

**Problem.** Only `SQLITE_LIMIT_ATTACHED` is set. `SELECT zeroblob(1000000000)` from a read-only token builds a 1 GB value, then its base64 JSON. The row cap (`MaxRows`) has no byte cap. `CheckStorageQuota` is a soft pre-check on file size, so a single batch can overshoot it.

**Fix.**
- In both connection hooks, call `conn.SetLimit(...)` for `SQLITE_LIMIT_LENGTH` (suggest 16 MiB), `SQL_LENGTH`, `VARIABLE_NUMBER`, `COMPOUND_SELECT`, `EXPR_DEPTH`, `LIKE_PATTERN_LENGTH`. Make the values configurable.
- Add a response byte budget in `scanRowsWithCap` that sets `truncated=true` when exceeded.
- Enforce database size with `PRAGMA max_page_count`, derived from `MaxDBSizeBytes` and the page size, on the writer connection (privileged).

**Tests.** `SELECT zeroblob(1<<30)` returns a clean error with bounded memory. A batch that would exceed the quota fails and rolls back. A recursive CTE that never ends is cancelled by `QueryTimeout`.

### B2. Stop hitting `control.db` on every request

**Problem.** `AuthenticateToken` does a `SELECT`, then `UPDATE tokens SET last_used_at` on every request, with the error ignored. The control database's write path caps API throughput and undermines the engine's group commit.

**Fix.** Cache validated tokens in memory (about 30 s TTL, invalidated on revoke). Buffer `last_used_at` updates and flush every few seconds.

**Test.** Benchmark `/query` before and after. Revoking a token takes effect within the TTL (or immediately via invalidation).

### B3. Snapshots should not stall writes

**Problem.** `VacuumInto` runs through `RunMaintenance`, which occupies the single writer for the whole copy. Snapshots, branch creation and `/download` all block every write to that database.

**Fix (verify first).** `VACUUM INTO` reads from a consistent snapshot, so try running it on a dedicated connection rather than the writer actor. Confirm it works with your authorizer and `query_only` settings, and confirm the result is consistent under concurrent writes. If it does not work on a read connection, add a bounded "pause between batches" instead.

**Test.** Measure p99 write latency during a snapshot of a 500 MB database, before and after.

### B4. Make restore safe

**Problem.** `ReplaceDatabaseFile` holds `Manager.mu` (the global lock) while copying, so every database blocks. It closes the open database even if requests are in flight. If the final rename fails, the original has already been moved to `.pre-restore` and the database is missing.

**Fix.**
- Stage the file first, outside the lock.
- Mark the entry as "restoring" so new `Acquire` calls wait or return 503, wait for `inFlight == 0` with a timeout, then close and swap.
- On rename failure, move `.pre-restore` back.
- Keep more than one `.pre-restore` generation, or rely on the automatic pre-restore snapshot and document it.

**Tests.** Restore during concurrent reads and writes. Injected rename failure restores the original. Other databases stay responsive during a large restore.

### B5. Bootstrap token

**Problem.** Without `LITEBASE_BOOTSTRAP_TOKEN`, every `serve` start creates and prints a new non-expiring admin token. They accumulate.

**Fix.** Create the bootstrap token only if no admin token exists, and print it once. Add `litebase token create|list|revoke` for later. If the env token is removed from the environment, revoke or expire the previously seeded token.

### B6. Protect `/metrics`

**Problem.** `/metrics` is unauthenticated and exports database names, sizes and writer stats.

**Fix.** Require an admin or dedicated `metrics` scope, or bind metrics to a separate listener (`--metrics-addr`). Document which one you chose.

---

## 5. P2: release and hygiene

### C1. Module path
`go.mod` says `github.com/litebase/litebase`, but the repository is `github.com/ibrahimraimi/litebase`, so `go install github.com/ibrahimraimi/litebase/cmd/litebase@latest` fails. Choose one before anyone depends on it:
- rename the module: `go mod edit -module github.com/ibrahimraimi/litebase` plus a repo-wide import rewrite and the GoReleaser `ldflags`, or
- create the `litebase` GitHub org and transfer the repo.

### C2. Release pipeline
`.goreleaser.yml` builds linux/darwin on amd64/arm64 with `CGO_ENABLED=1` from one runner. That needs cross C toolchains and will fail without them. There is no release workflow in `.github/workflows/`.
- Build on a runner matrix (linux amd64, linux arm64, macOS arm64, macOS amd64) with plain `go build`.
- Upload binaries and a `checksums.txt` to the GitHub release on tag push.
- Alternative: move to a CGO-free driver (see `ncruces/go-sqlite3` in the original plan). That is a larger change, so do it only if the matrix proves painful.

### C3. Docker (verify before fixing)
- The image's default `CMD` binds `127.0.0.1:8080`, so `docker run -p 8080:8080 litebase` is unreachable. CI only passes because it overrides `--addr`. Default to `0.0.0.0:8080` inside the image.
- `VOLUME /var/lib/litebase` is declared before `USER nonroot`. The directory is likely root-owned, so `serve` may be unable to create `control.db`. Check your latest CI run. If it is green, drop this item. Otherwise create the directory in the builder stage and `COPY --chown=nonroot:nonroot`.

### C4. Docs
- Rename `docs/USEAGE.md` to `docs/USAGE.md` and fix the README link.
- Add a "Known limitations" section: no interactive transactions, copy-based branches, single node, tokens are admin/read/write scoped only.
- Document the migration-history change from A1 and the upgrade path.

---

## 6. Test plan additions

| Area | New tests |
|---|---|
| Migrations | crash between apply and record; branch/restore/push history; trigger migration |
| Merge | column added on branch with parent data; parent-only table; conflicting edits; drop refused |
| Branching | consistent copy under concurrent writes; same branch name under two parents |
| Parser | trigger statements; bracketed identifiers; differential fuzz vs SQLite |
| Limits | `zeroblob(1<<30)`; oversized SQL; page-count quota; infinite recursive CTE |
| Restore | concurrent traffic; injected rename failure; other databases unaffected |
| Crash | SIGKILL during snapshot and during restore (the original plan required both) |
| Shutdown | queue drains on SIGTERM; no acknowledged write lost |
| Auth | token cache invalidation on revoke; metrics requires auth |

Run the existing crash loop (`-count=50`) after the A1 and B4 changes, since both touch durability paths.

## 7. Suggested milestones

| Step | Work | Done when |
|---|---|---|
| 1 | Write failing tests for A1–A4 | tests red for the right reasons |
| 2 | A1 (history in database) | A1 tests green; upgrade path tested |
| 3 | A2 + A3 (merge, branch copy) | A2/A3 tests green |
| 4 | A4 (trigger splitter + fuzz) | trigger migration applies |
| 5 | B1 + B2 | limits and benchmark in place |
| 6 | C1, C3, C4 | module path fixed; quickstart works in Docker |
| 7 | Cut `v0.0.2-beta` | CI green, changelog updated, tag pushed |

## 8. Changelog draft (`CHANGELOG.md`)

```markdown
## [0.0.2-beta] - YYYY-MM-DD

### Changed
- Migration history now lives inside each database (`_litebase_migrations`) and is applied in the same transaction as the migration. Existing data directories are upgraded on first open.
- `branch merge` is now a three-way schema merge and never drops tables without `--allow-drop`.
- Branch creation takes a single consistent snapshot; branches are namespaced per parent.

### Fixed
- Triggers can be created via `/query`, `/batch` and migrations.
- Restore no longer blocks unrelated databases and rolls back on failure.

### Security
- Per-connection SQLite limits and response byte budget bound memory use.
- Database size is enforced with `max_page_count`.
- `/metrics` requires authentication.
- Bootstrap admin token is created once, not on every start.
```

## 9. Open decisions

| Decision | Options | Recommendation |
|---|---|---|
| Merge output | apply directly vs write a migration file | write a migration file by default |
| Branch internal naming | `parent--branch` vs separate display name | separate `display_name` column, safer long term |
| Module path | rename module vs create org | org if you plan a hosted product, rename otherwise |
| Metrics auth | scope vs separate listener | separate listener for production, scope for simplicity |