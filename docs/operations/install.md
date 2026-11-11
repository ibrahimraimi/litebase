# Operations Guide: CLI Installer (`install.sh`)

`install.sh` is a POSIX `sh` script at the repository root that downloads, SHA-256 verifies, and atomically installs the `litebase` single-binary CLI from [GitHub Releases](https://github.com/ibrahimraimi/litebase/releases).

---

## 1. Quick Start

### Install the latest release (including `-beta` prereleases)

```sh
curl -fsSL https://raw.githubusercontent.com/ibrahimraimi/litebase/main/install.sh | sh
```

> **Note on `litebase.tech`:** Once DNS and hosting for `litebase.tech` are live, `curl -fsSL https://litebase.tech/install.sh | sh` will serve the same script. That URL is **not live yet**; use the `raw.githubusercontent.com` URL above.

### Inspect-first workflow

If you prefer not to pipe directly into `sh`:

```sh
curl -fsSL --proto '=https' --tlsv1.2 -o install.sh https://raw.githubusercontent.com/ibrahimraimi/litebase/main/install.sh
less install.sh
sh install.sh --dry-run
sh install.sh
```

---

## 2. Flags & Environment Variables

### Command-Line Flags

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific release tag (e.g. `v0.0.3-beta` or `0.0.3-beta`). Defaults to the newest non-draft release on GitHub (including prereleases). |
| `--dir <path>` | Target directory for the `litebase` binary. Defaults to `/usr/local/bin` if writable, otherwise `$HOME/.local/bin`. |
| `--dry-run` | Print the resolved version, archive URL, checksum URL, and target path without downloading or modifying the filesystem. |
| `-h`, `--help` | Print usage information and exit. |

Passing flags when piping to `sh`:

```sh
curl -fsSL https://raw.githubusercontent.com/ibrahimraimi/litebase/main/install.sh | sh -s -- --version v0.0.3-beta --dir ~/.local/bin
```

### Environment Variables

| Variable | Description |
|---|---|
| `LITEBASE_VERSION` | Release tag to install (overridden by `--version`). |
| `LITEBASE_INSTALL_DIR` | Target installation directory (overridden by `--dir`). |
| `LITEBASE_DOWNLOAD_BASE` | **Test-only.** Overrides `https://github.com/ibrahimraimi/litebase/releases/download` for local integration tests (`scripts/test-install.sh`). Non-HTTPS URLs are rejected unless they point to `http://127.0.0.1` or `http://localhost`. |

---

## 3. Supported Platforms

Prebuilt release archives (`litebase_<version>_<os>_<arch>.tar.gz`) are compiled with `CGO_ENABLED=1` (`github.com/mattn/go-sqlite3`) for the following platforms:

| OS (`uname -s`) | Architecture (`uname -m`) | Release Asset | Notes |
|---|---|---|---|
| `Linux` | `x86_64` / `amd64` | `litebase_<version>_linux_amd64.tar.gz` | Requires **glibc** |
| `Linux` | `aarch64` / `arm64` | `litebase_<version>_linux_arm64.tar.gz` | Requires **glibc** |
| `Darwin` (macOS) | `x86_64` / `amd64` | `litebase_<version>_darwin_amd64.tar.gz` | Intel macOS |
| `Darwin` (macOS) | `arm64` | `litebase_<version>_darwin_arm64.tar.gz` | Apple Silicon |

### Unsupported Platforms

- **Alpine Linux / musl libc:** Prebuilt Linux binaries are dynamically linked against glibc. When `install.sh` detects musl (`/etc/alpine-release`, `ldd --version`, or `/lib/ld-musl-*`), it exits non-zero and directs you to the [Docker image](../INSTALLATION.md#option-2-run-with-docker) or [building from source](../INSTALLATION.md#option-1-build-from-source).
- **Windows (`MINGW`, `MSYS`, `CYGWIN`):** `install.sh` refuses to run on Windows and never writes to `C:\Windows\System32`. Use WSL2, Docker, or download the release archive manually from the [Releases page](https://github.com/ibrahimraimi/litebase/releases).
- **Other architectures (`armv7l`, `riscv64`, `s390x`, etc.):** Build from source with `CGO_ENABLED=1 go build -trimpath -o bin/litebase ./cmd/litebase`.

---

## 4. How Checksum Verification Works

1. `install.sh` creates an isolated temporary directory via `mktemp -d` and registers an `EXIT`/`INT`/`TERM` cleanup trap.
2. It downloads both `checksums.txt` and `litebase_<version>_<os>_<arch>.tar.gz` over HTTPS (`--proto '=https' --tlsv1.2`).
3. **Before extracting the archive**, `install.sh` looks up the exact archive filename in `checksums.txt` and computes the local file's SHA-256 digest using `sha256sum` or `shasum -a 256`.
4. If neither hashing tool is installed, if the archive is missing from `checksums.txt`, or if the SHA-256 digest does not match, `install.sh` **aborts immediately**, removes all temporary files, and leaves existing installations untouched. There is no flag to skip verification.
5. Only after the SHA-256 check succeeds does `install.sh` extract `litebase`, stage it to `${INSTALL_DIR}/.litebase.tmp.$$`, `chmod 0755`, and atomically `mv -f` it to `${INSTALL_DIR}/litebase`.

---

## 5. Trust Model

- **What `curl | sh` trusts:** Piping a remote script into `sh` relies on TLS and the host serving `install.sh` (`raw.githubusercontent.com` or `litebase.tech`), as well as `api.github.com` and `github.com` for serving both `checksums.txt` and the release tarball.
- **What SHA-256 checksum verification protects against:** Truncated downloads, CDN/mirror corruption, or a mismatch between the release tarball and `checksums.txt`. Because `install.sh` places `main "$@"` on the final line of the file, a truncated download of `install.sh` itself will not execute partial commands.
- **Higher-assurance option:** Download `install.sh` to disk, inspect it before running, or build from a verified Git tag (`git clone` + `make build`).

---

## 6. Uninstalling

Litebase is a single static-asset binary and `install.sh` never modifies shell startup files (`~/.bashrc`, `~/.zshrc`, `~/.profile`). To uninstall, remove the binary from the directory where it was installed:

```sh
# If installed in ~/.local/bin:
rm -f "$HOME/.local/bin/litebase"

# If installed in /usr/local/bin:
sudo rm -f /usr/local/bin/litebase
```

