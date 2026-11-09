# 0014: Remote backup layout with a metadata file written last

- **Status:** Proposed (planned for v0.0.3-beta)
- **Date:** 2026-11-09

## Context

Backups must be restorable on an empty server with nothing but the bucket. They must also be safe against a crash or network failure part-way through an upload. We should not depend on every provider's atomicity guarantees.

## Decision

- Remote storage sits behind a small `BlobStore` interface (put, get, list, delete) with two implementations: a **local directory** and **S3-compatible** storage.
- Layout:
  ```
  <prefix>/<db>/<YYYY>/<MM>/<ts>-<label>.sqlite.gz
  <prefix>/<db>/<YYYY>/<MM>/<ts>-<label>.meta.json
  <prefix>/_control/<ts>.control.sqlite.gz   (+ meta)
  ```
- The **`.meta.json` file is written last** and acts as the commit marker. It records database name, label, kind, SHA-256 of the uncompressed file, sizes, Litebase version, migration versions and creation time. A data object without its meta file is treated as **incomplete** and is garbage-collected after 24 hours.
- Data is gzip-compressed with the standard library. Uploads are streamed, never buffered whole, and are retried with backoff.
- The control database is backed up with the same mechanism.
- Two servers must not share a prefix. The prefix should include a server name.

## Consequences

- Listing and restoring work from the bucket alone, with no control database.
- An interrupted upload is never mistaken for a valid backup.
- The S3 client library is a dependency decision to be made explicitly before implementation.
- Client-side encryption is not part of this design. Operators should use bucket-side encryption until it exists.
