# 0014: Remote backup layout with a metadata file written last

- **Status:** Accepted
- **Date:** 2026-11-09

## Context

Backups must be restorable on an empty server with nothing but the bucket. They must also be safe against a crash or network failure part-way through an upload. We should not depend on every provider's atomicity guarantees.

## Decision

- Remote storage sits behind a small `BlobStore` interface (`Put`, `Get`, `List`, `Delete`) with two implementations: a **local directory** (`LocalDirStore`) and **S3-compatible** storage (`S3Store`).
- **S3 client (`github.com/minio/minio-go/v7`)**: We chose `minio-go/v7` over `aws-sdk-go-v2` to keep the single-binary dependency tree small while getting first-class streaming `PutObject`/`GetObject` and out-of-the-box compatibility with AWS S3, MinIO, Cloudflare R2, and Backblaze B2.
- **Object key layout** (produced by `buildObjectKeys` in `internal/backup/remote.go`):
  ```
  <prefix>/<db>/<YYYY>/<MM>/<unix-nano>-<label>.sqlite[.gz]
  <prefix>/<db>/<YYYY>/<MM>/<unix-nano>-<label>.meta.json
  <prefix>/_control/<YYYY>/<MM>/<unix-nano>-control.sqlite[.gz]
  <prefix>/_control/<YYYY>/<MM>/<unix-nano>-control.meta.json
  ```
- The **`.meta.json` file is written last** and acts as the commit marker. It records `snapshot_id`, `database`, `label`, `kind`, `data_key`, `meta_key`, `compression`, `sha256` (of the uncompressed SQLite file), `size_bytes`, `compressed_bytes`, `litebase_version`, `migration_versions`, and `created_at`. A data object without its `.meta.json` companion is treated as **incomplete** and is garbage-collected after 24 hours.
- **Compression (`gzip` on by default, `--backup-compress=none` to disable)**: Data is compressed with standard library `compress/gzip` (`.sqlite.gz`) by default so operators can inspect or decompress any backup with standard `gunzip` during disaster recovery without custom tooling. Operators on pre-compressed or local filesystem targets can pass `--backup-compress=none` (or `backup_compress = "none"`) to store raw `.sqlite` objects. Uploads are streamed, never buffered whole in memory, and retried with exponential backoff.
- **`control.db` backup and `restore-control`**: Whenever a scheduled remote backup completes, `control.db` is snapshotted via `VACUUM INTO` and uploaded under `<prefix>/_control/<YYYY>/<MM>/<unix-nano>-control.sqlite[.gz]` with its `.meta.json` commit marker. Operators can recover server metadata onto a fresh host using `litebase restore-control --from-remote <key|latest>` before restoring user databases, preserving database IDs, token hashes, branch lineage, and backup policies.
- **Opt-in backup policies**: Scheduled backups are opt-in per database (`litebase backup policy set <db> ...`) rather than auto-enabled when `LITEBASE_S3_*` environment variables are present.
- Two servers must not share a prefix. The prefix should include a server or environment name.

## Consequences

- Listing and restoring work from the bucket alone, with or without a pre-existing `control.db`.
- An interrupted upload is never mistaken for a valid backup because `ListRemoteSnapshots` only returns entries whose `.meta.json` commit marker is present.
- Client-side encryption is not part of this design. Operators should use bucket-side encryption until it exists.
