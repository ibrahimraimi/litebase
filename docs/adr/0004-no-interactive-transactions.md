# 0004: No interactive transactions over HTTP

- **Status:** Accepted
- **Date:** 2026-10-29

## Context

A client-held transaction (`BEGIN`, several HTTP calls, `COMMIT`) keeps SQLite's single write lock held across network round trips. A slow or disconnected client would block every other writer to that database. Other SQLite-over-HTTP systems make the same trade-off and decline explicit client-held transactions.

## Decision

No endpoint holds a transaction open across requests. Atomic multi-statement work is done with **`POST /v1/databases/{db}/batch`**, where all statements run in one transaction on the writer actor, and with the migration runner.

## Consequences

- The write lock is only held while the server runs the statements, never while waiting on a client.
- Read-modify-write logic that needs atomicity has to be expressed as one batch, or as a single statement (`UPDATE ... SET n = n + 1`).
- Clients cannot run a long, interactive transaction. This is deliberate and should be documented as a limitation.
