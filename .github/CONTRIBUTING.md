# Contributing to Litebase

Thank you for contributing to Litebase! This guide covers how to set up your development environment, run the test suite, and follow our engineering rules.

---

## Development Setup

1. Ensure you have **Go 1.23+** and a C compiler (`gcc` or `clang`) installed.
2. Build the binary:
   ```bash
   make build
   ```
3. Run the verification suite before submitting a pull request:
   ```bash
   make fmt
   make vet
   make race
   ```

---

## Engineering Principles

1. **Write code for humans first**: Prefer small, single-purpose functions, descriptive names, early returns, and explicit error handling.
2. **Test against real SQLite**: Do not mock SQLite in unit or integration tests. Use `t.TempDir()` and verify behavior with `go test -race ./...`.
3. **Maintain the security boundary**:
   - Every database connection must register the SQLite authorizer callback (`sqlite3_set_authorizer`).
   - All user-supplied database, branch, migration, and snapshot names must pass `engine.ValidateName` and `engine.SafeJoinPath`.
   - Never log or persist plaintext `lbk_` Bearer tokens.
4. **Keep documentation in sync**: Any change to CLI flags, configuration keys (`litebase.toml`), or `/v1` HTTP endpoints must update [api/openapi.yaml](../api/openapi.yaml) and the guides in [docs/](../docs/).

