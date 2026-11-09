# 0007: Typed JSON value encoding

- **Status:** Accepted
- **Date:** 2026-10-29

## Context

SQLite integers are 64-bit. JSON numbers are usually parsed as doubles, which lose precision above 2^53. SQLite also stores BLOBs, which JSON cannot represent directly. Whatever encoding the API uses becomes a public contract: changing it later breaks every client.

## Decision

Values in requests and responses are **typed**: `{"type": "integer" | "float" | "text" | "blob" | "null", "value": ...}`.

- Integers are encoded as **strings**, so no precision is lost.
- Blobs are encoded as **base64**.
- A compact **plain JSON mode** is available as an explicit opt-in (`?format=plain`) for convenience. It is lossy for large integers and blobs and is documented as such.

## Consequences

- Clients must encode and decode typed values. A client library should hide this.
- The `curl` experience is more verbose than plain JSON, which is why plain mode exists.
- New type tags may be added later, but existing tags and their encodings must not change.
- The codec needs fuzz tests (int64 extremes, NaN, large blobs, invalid UTF-8).
