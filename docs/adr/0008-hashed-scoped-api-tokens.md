# 0008: Hashed, scoped API tokens

- **Status:** Accepted
- **Date:** 2026-10-29 (token cache and bootstrap behavior changed in v0.0.2-beta)

## Context

The API needs authentication that is simple for a self-hosted single binary, and that does not turn the control database into a store of usable secrets.

## Decision

- Tokens are **256-bit random values** with the prefix `lbk_`, shown **once** at creation.
- Only a **SHA-256 hash** is stored, together with a short visible prefix for identification.
- Each token has a **scope** (`read`, `write` or `admin`), may be **bound to one database**, and may **expire**.
- Validated tokens are cached in memory for a short time so the control database is not hit on every request. Revocation invalidates the cache.
- On first start, if no admin token exists, a bootstrap admin token is created **once** and printed. It is not recreated on every start.
- `/metrics` requires authentication.

## Consequences

- A leaked control database does not leak usable tokens.
- Lost tokens cannot be recovered, only replaced.
- Fine-grained permissions, organizations and roles are not part of this model and are a later concern.
- Anything that needs to call out to another service with a secret (for example remote backups) must not follow this pattern. See ADR 0013.
