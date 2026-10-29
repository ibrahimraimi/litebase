# Security Policy

Executing user-supplied SQL over HTTP is the highest-risk surface in Litebase. We treat any bypass of our authorization, SQL authorizer, or filesystem isolation boundaries as a critical security issue.

---

## Security Architecture & Boundaries

Litebase enforces the following security invariants across all connections and requests:

1. **SQLite Authorizer (`sqlite3_set_authorizer`)**:
   - Denies `ATTACH`, `DETACH`, `load_extension`, and `fts3_tokenizer`.
   - Denies direct mutations of internal `_litebase_*` tables.
   - Restricts `PRAGMA` statements to a read-only inspection allow-list (`table_info`, `index_list`, `index_info`, `foreign_key_list`, `integrity_check`, `wal_checkpoint`, `page_count`, `page_size`, `freelist_count`).
2. **Read-Only Pool Enforcement**:
   - Reader pool connections open SQLite with `mode=ro` and `PRAGMA query_only = ON`, and install a read-only authorizer that rejects all write operations.
3. **Scoped Token Authentication**:
   - Bearer tokens (`lbk_<64-hex>`) contain 256 bits of cryptographic entropy (`crypto/rand`).
   - Only SHA-256 hashes are stored in `control.db` and compared in constant time (`crypto/subtle.ConstantTimeCompare`).
4. **Filesystem Path Containment**:
   - Database, branch, and snapshot names are validated against `^[a-z0-9][a-z0-9_-]{0,62}$` and checked with `engine.SafeJoinPath` to prevent path traversal (`../`, null bytes, or symlink escapes).

---

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Instead, report vulnerabilities privately via [GitHub Security Advisories](https://github.com/litebase/litebase/security/advisories/new) and include:

- The affected component (`internal/engine`, `internal/api`, `internal/auth`, `internal/cli`, etc.)
- A minimal proof-of-concept request or SQL payload reproducing the issue
- The potential impact (e.g., authorizer bypass, privilege escalation from `read` to `write`/`admin`, path traversal, or denial of service)

