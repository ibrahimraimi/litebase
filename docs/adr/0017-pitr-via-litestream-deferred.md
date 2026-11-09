# 0017: Point-in-time recovery will integrate Litestream, not reimplement it

- **Status:** Proposed (deferred beyond v0.0.3-beta)
- **Date:** 2026-11-09

## Context

Scheduled snapshots restore to a moment in time, at the schedule's granularity. Recovering to an arbitrary second needs continuous streaming of the write-ahead log to object storage. That is a hard problem that an existing, well-known tool already solves.

## Decision

- Litebase will **not** write its own continuous replication or point-in-time recovery.
- When this is built, Litestream will be integrated, first as a **managed sidecar process** (fewer coupling risks than linking it as a library).
- Before depending on it, check its current license, release stability and API.

## Consequences

- v0.0.3-beta documents plainly what it does not protect: there is no point-in-time recovery, and the most recent writes since the last backup can be lost.
- The backup layout (ADR 0014) must not make a later integration harder.
