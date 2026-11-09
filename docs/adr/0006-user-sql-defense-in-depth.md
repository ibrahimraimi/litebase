# 0006: Defense in depth for user-supplied SQL

- **Status:** Accepted
- **Date:** 2026-10-29 (limits and quota added in v0.0.2-beta)

## Context

Litebase executes SQL sent by API clients. That is the most dangerous surface in the project. A single mistake can expose the host filesystem (`ATTACH`, extension loading), corrupt internal state, or exhaust memory.

## Decision

Several independent layers, none of which is relied on alone:

1. **SQLite authorizer callback** denies `ATTACH`, `DETACH`, `load_extension`, `fts3_tokenizer`, writes to internal `_litebase_*` tables, and any `PRAGMA` outside an allow-list. Token scope is enforced here too: a read-scoped token cannot write.
2. **Single-statement validation.** The SQL splitter understands quotes, comments, `[bracketed]` identifiers and trigger bodies (`CREATE TRIGGER ... BEGIN ...; END;`), so a request to `/query` cannot smuggle a second statement.
3. **Per-connection SQLite limits** (`SQLITE_LIMIT_LENGTH`, `SQL_LENGTH`, `VARIABLE_NUMBER`, `COMPOUND_SELECT`, `EXPR_DEPTH`, `LIKE_PATTERN_LENGTH`, `ATTACHED`).
4. **Response byte budget** and a row cap, with a truncation flag, so a large result cannot exhaust server memory.
5. **Database size enforced with `PRAGMA max_page_count`**, so a single batch cannot overshoot a storage quota.
6. **Timeouts** per query, **request size limits**, and **per-token rate limiting**.
7. **Database names** are validated against a strict pattern, and file paths are always built through a safe join. A user-supplied name is never concatenated into a path.

## Consequences

- The splitter is part of the security boundary. Changes to it need differential fuzz tests against SQLite's own parsing.
- Each limit is configurable and must have a test that proves it is enforced.
- New features that run SQL (migrations, branches, merge) must go through the same authorizer or an explicitly privileged internal path that is reviewed as such.
