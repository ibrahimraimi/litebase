# Operations Guide: Backups & Disaster Recovery

Litebase combines online local snapshots (`VACUUM INTO`) with scheduled off-box backups to S3-compatible object storage (or a local backup directory), retention policies, automated restore verification (`backup verify`), and full disaster recovery onto an empty server.

---

## 1. What Is Protected (and What Is Not)

| Scenario | Protection | Recovery Point (RPO) |
|---|---|---|
| **Process crash (`SIGKILL`, panic)** | SQLite WAL mode + startup rollback of incomplete restores/snapshots | **Zero data loss** for committed transactions |
| **OS crash or power loss** | `durability = "full"` (`PRAGMA synchronous = FULL`) + `fsync` on file & directory renames | **Zero data loss** (`normal` mode may lose the last few milliseconds of WAL frames on power loss) |
| **Bad migration or operator mistake** | Automatic `pre-push` and `pre-restore` snapshots + `snapshot restore` | Point of the snapshot prior to the change |
| **Full disk or host loss** | Scheduled remote backups (`--remote`) of user databases and `control.db` | **Up to the last completed remote backup** (writes committed after the last backup are not replicated) |

> **Note:** Litebase `0.0.3-beta` / `0.0.4-beta` performs periodic snapshot backups rather than continuous WAL streaming. For workloads requiring sub-second point-in-time recovery (PITR), see [ADR 0017](../adr/0017-pitr-via-litestream-deferred.md).

---

## 2. Configuring Remote Storage

Remote storage credentials are read exclusively from **environment variables** ([ADR 0013](../adr/0013-backup-credentials-from-environment.md)) and are never stored in `control.db`, `litebase.toml`, or logs.

### S3-Compatible Storage (AWS S3, MinIO, Cloudflare R2, Backblaze B2)

```bash
export LITEBASE_S3_ENDPOINT="https://s3.us-east-1.amazonaws.com"
export LITEBASE_S3_REGION="us-east-1"
export LITEBASE_S3_BUCKET="my-litebase-backups"
export LITEBASE_S3_PREFIX="prod-node-01"
export LITEBASE_S3_ACCESS_KEY_ID="AKIA..."
export LITEBASE_S3_SECRET_ACCESS_KEY="..."
```

- **Unique prefix per server:** Two Litebase servers must never share the same `LITEBASE_S3_BUCKET` + `LITEBASE_S3_PREFIX`. Always include the server or environment name in `LITEBASE_S3_PREFIX`.
- **Bucket security:** `control.db` backups contain token hashes and the audit log. Keep the bucket private and enable server-side encryption (SSE-S3 / SSE-KMS).
- **Compression:** Backups are streamed through standard library `gzip` (`.sqlite.gz`) by default. Pass `--backup-compress=none` to `litebase serve` (or set `backup_compress = "none"`) to upload uncompressed `.sqlite` files.

### Local Directory Target (NFS / Mounted Volume / Local Testing)

```bash
export LITEBASE_BACKUP_DIR="/mnt/backup-volume/litebase"
export LITEBASE_S3_PREFIX="prod-node-01"
```

### Remote Object Layout ([ADR 0014](../adr/0014-backup-remote-layout-and-commit-marker.md))

```text
<prefix>/<db>/<YYYY>/<MM>/<unix-nano>-<label>.sqlite.gz
<prefix>/<db>/<YYYY>/<MM>/<unix-nano>-<label>.meta.json
<prefix>/_control/<YYYY>/<MM>/<unix-nano>-control.sqlite.gz
<prefix>/_control/<YYYY>/<MM>/<unix-nano>-control.meta.json
```

The `.meta.json` object is uploaded **only after** the data stream finishes. Any `.sqlite[.gz]` object without a matching `.meta.json` is treated as an incomplete upload and is garbage-collected after 24 hours.

---

## 3. Enabling Backup Policies (Opt-In per Database)

Providing `LITEBASE_S3_*` environment variables configures the remote store, but **does not automatically schedule backups** on every database. Policies are opt-in per database:

```bash
# Run a backup every 1 hour, retain the newest 24 scheduled snapshots and 7 days of history, and upload to remote storage:
litebase backup policy set app \
  --interval 1h \
  --keep-last 24 \
  --keep-days 7 \
  --remote=true \
  --enabled=true

# Inspect the policy and current scheduler status:
litebase backup status app

# Trigger an immediate backup (and control.db remote backup):
litebase backup run app
```

- **Skip-when-idle:** If a database has had zero write commits since the previous scheduled backup and has no pending remote uploads, the scheduler records a `skipped_idle` run without writing a duplicate snapshot.
- **`control.db` backup:** Whenever a remote backup run succeeds, Litebase also snapshots and uploads `control.db` under `<prefix>/_control/`.

---

## 4. Retention & Snapshot Protection ([ADR 0015](../adr/0015-retention-protects-referenced-snapshots.md))

Every snapshot has a `kind`: `manual`, `scheduled`, `pre-restore`, `pre-push`, or `branch`.

- **`manual` snapshots** are **never** deleted automatically by retention.
- **Pinned snapshots** (`litebase snapshot pin <db> <id>`) are protected from both automatic pruning and manual deletion until unpinned (`litebase snapshot unpin <db> <id>`).
- **Branch parent snapshots** are protected while any branch still references them as `parent_snapshot_id`, preserving three-way schema merge bases.
- **`scheduled` snapshots** are pruned only when they fall outside **both** `keep_last` and `keep_days` (and are neither pinned nor referenced by a branch).
- **`pre-restore` and `pre-push` snapshots** keep the newest 5 per database.

Preview or run retention manually at any time:

```bash
litebase snapshot prune app --dry-run
litebase snapshot prune app
```

---

## 5. Restore Verification Drills (`backup verify`)

A backup is only trustworthy if it can be restored. `litebase backup verify` downloads (or reads) the latest backup into an isolated temporary directory, verifies its SHA-256 checksum, opens it with SQLite, runs `PRAGMA integrity_check`, and checks `_litebase_migrations`—without touching the live database:

```bash
# Verify the latest local snapshot:
litebase backup verify app

# Verify the latest remote backup in S3:
litebase backup verify app --remote
```

---

## 6. Disaster Recovery Runbook

### Scenario A: Restore a Database in Place (or Side-by-Side) on a Running Server

```bash
# Inspect available local and remote snapshots:
litebase snapshot list app --include-remote

# Restore into a new database name for side-by-side inspection without touching live traffic:
litebase snapshot restore app <snapshot-id> --as app_inspect

# Or restore in place (automatically creates a pre-restore snapshot first):
litebase snapshot restore app <snapshot-id>
```

### Scenario B: Full Host Loss — Recover onto an Empty Server from S3

1. **Provision the new host** and set the `LITEBASE_S3_*` environment variables pointing to the existing bucket and prefix.
2. **Restore `control.db` before starting the server** (recovers database IDs, API token hashes, branch lineage, and backup policies):
   ```bash
   litebase restore-control --data-dir /var/lib/litebase --from-remote latest
   ```
3. **Start `litebase serve`** and restore each user database from its latest remote backup:
   ```bash
   litebase backup remote-list app
   litebase snapshot restore app --from-remote <data-key>
   ```
4. **Verify health and integrity:**
   ```bash
   litebase backup verify app
   ```

---

## 7. Prometheus Metrics

All backup metrics are exposed on `GET /metrics` (authenticated with an admin or `metrics:read` token):

- `litebase_backup_last_success_timestamp_seconds{db="<name>"}`
- `litebase_backup_last_duration_seconds{db="<name>"}`
- `litebase_backup_failures_total{db="<name>"}`
- `litebase_snapshots_total{db="<name>",kind="<kind>"}`
- `litebase_snapshots_bytes{db="<name>"}`
- `litebase_remote_upload_bytes_total`

