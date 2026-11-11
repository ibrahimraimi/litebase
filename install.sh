#!/bin/sh
# Litebase CLI installer (https://github.com/ibrahimraimi/litebase)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ibrahimraimi/litebase/main/install.sh | sh
#   sh install.sh [--version <tag>] [--dir <path>] [--dry-run] [--help]
#
# Environment variables:
#   LITEBASE_VERSION        Release tag to install (e.g. v0.0.3-beta)
#   LITEBASE_INSTALL_DIR    Target installation directory
#   LITEBASE_DOWNLOAD_BASE  Test-only override for release download base URL

set -eu

GITHUB_REPO="ibrahimraimi/litebase"
DEFAULT_DOWNLOAD_BASE="https://github.com/${GITHUB_REPO}/releases/download"
DEFAULT_FEED_URL="https://github.com/${GITHUB_REPO}/releases.atom"
DEFAULT_API_RELEASES_URL="https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=10"
BINARY_NAME="litebase"
CHECKSUM_FILE="checksums.txt"

TMP_DIR=""
STAGED_TARGET=""
USED_SUDO=0
LAST_HTTP_STATUS="unknown"
LAST_CURL_EXIT=0

err() {
	printf 'Error: %s\n' "$*" >&2
}

info() {
	printf '%s\n' "$*"
}

cleanup() {
	if [ -n "${STAGED_TARGET:-}" ]; then
		if [ "${USED_SUDO:-0}" -eq 1 ]; then
			sudo rm -f "$STAGED_TARGET" >/dev/null 2>&1 || true
		else
			rm -f "$STAGED_TARGET" >/dev/null 2>&1 || true
		fi
	fi
	if [ -n "${TMP_DIR:-}" ] && [ -d "$TMP_DIR" ]; then
		rm -rf "$TMP_DIR"
	fi
}

usage() {
	cat <<'EOF'
Usage: install.sh [options]

Install the litebase CLI from GitHub Releases.

Options:
  --version <tag>   Install a specific release tag (e.g. v0.0.3-beta)
  --dir <path>      Install binary into <path> (default: /usr/local/bin if writable, else ~/.local/bin)
  --dry-run         Print what would be downloaded, verified, and installed without changing anything
  -h, --help        Show this help message

Environment variables:
  LITEBASE_VERSION        Same as --version
  LITEBASE_INSTALL_DIR    Same as --dir
  LITEBASE_DOWNLOAD_BASE  Override download base URL (test-only)
EOF
}

print_supported_platforms() {
	err "Supported prebuilt platforms are: linux/amd64 (glibc), linux/arm64 (glibc), darwin/amd64, darwin/arm64."
	err "For other platforms, run the Docker image or build from source:"
	err "  https://github.com/${GITHUB_REPO}/blob/main/docs/INSTALLATION.md"
}

is_musl_linux() {
	if [ -n "${LITEBASE_TEST_IS_MUSL:-}" ]; then
		[ "$LITEBASE_TEST_IS_MUSL" = "1" ]
		return
	fi
	if [ -f /etc/alpine-release ]; then
		return 0
	fi
	if command -v ldd >/dev/null 2>&1; then
		if ldd --version 2>&1 | grep -qi musl; then
			return 0
		fi
	fi
	for musl_loader in /lib/ld-musl-*; do
		if [ -e "$musl_loader" ]; then
			return 0
		fi
	done
	return 1
}

detect_platform() {
	raw_os="${LITEBASE_TEST_UNAME_S:-$(uname -s 2>/dev/null || echo unknown)}"
	raw_arch="${LITEBASE_TEST_UNAME_M:-$(uname -m 2>/dev/null || echo unknown)}"

	case "$raw_os" in
		MINGW* | MSYS* | CYGWIN* | Windows_NT)
			err "Windows is not supported by install.sh."
			err "Download the release archive from https://github.com/${GITHUB_REPO}/releases or run Litebase under WSL2 / Docker."
			exit 1
			;;
		Linux)
			if is_musl_linux; then
				err "Detected musl libc (e.g. Alpine Linux). Prebuilt Litebase Linux binaries require glibc (CGO SQLite)."
				err "Use the official Docker image or build from source with CGO_ENABLED=1:"
				err "  https://github.com/${GITHUB_REPO}/blob/main/docs/INSTALLATION.md"
				exit 1
			fi
			DETECTED_OS="linux"
			;;
		Darwin)
			DETECTED_OS="darwin"
			;;
		*)
			err "Unsupported operating system: ${raw_os}."
			print_supported_platforms
			exit 1
			;;
	esac

	case "$raw_arch" in
		x86_64 | amd64)
			DETECTED_ARCH="amd64"
			;;
		aarch64 | arm64)
			DETECTED_ARCH="arm64"
			;;
		*)
			err "Unsupported CPU architecture: ${raw_arch} on ${DETECTED_OS}."
			print_supported_platforms
			exit 1
			;;
	esac
}

validate_url_scheme() {
	target_url="$1"
	case "$target_url" in
		https://*)
			return 0
			;;
		http://127.0.0.1:* | http://127.0.0.1/* | http://localhost:* | http://localhost/*)
			if [ -n "${LITEBASE_DOWNLOAD_BASE:-}" ]; then
				return 0
			fi
			err "Refusing insecure HTTP URL: ${target_url}"
			exit 1
			;;
		*)
			err "Refusing non-HTTPS URL: ${target_url}"
			exit 1
			;;
	esac
}

http_download() {
	src_url="$1"
	dest_file="$2"
	validate_url_scheme "$src_url"
	LAST_HTTP_STATUS="unknown"
	LAST_CURL_EXIT=0

	if command -v curl >/dev/null 2>&1; then
		http_code="000"
		curl_rc=0
		case "$src_url" in
			https://*)
				http_code=$(curl -sSL --proto '=https' --tlsv1.2 --retry 3 -w '%{http_code}' -o "$dest_file" "$src_url" 2>/dev/null) || curl_rc=$?
				;;
			*)
				http_code=$(curl -sSL --proto '=http,https' --retry 3 -w '%{http_code}' -o "$dest_file" "$src_url" 2>/dev/null) || curl_rc=$?
				;;
		esac
		LAST_HTTP_STATUS="${http_code:-000}"
		LAST_CURL_EXIT="$curl_rc"
		if [ "$curl_rc" -ne 0 ]; then
			rm -f "$dest_file"
			return 1
		fi
		case "$LAST_HTTP_STATUS" in
			2??)
				return 0
				;;
			*)
				rm -f "$dest_file"
				LAST_CURL_EXIT=22
				return 1
				;;
		esac
	fi

	if command -v wget >/dev/null 2>&1; then
		wget_rc=0
		case "$src_url" in
			https://*)
				wget -q --https-only -O "$dest_file" "$src_url" 2>/dev/null || wget_rc=$?
				;;
			*)
				wget -q -O "$dest_file" "$src_url" 2>/dev/null || wget_rc=$?
				;;
		esac
		LAST_CURL_EXIT="$wget_rc"
		if [ "$wget_rc" -ne 0 ]; then
			rm -f "$dest_file"
			return 1
		fi
		LAST_HTTP_STATUS="200"
		return 0
	fi

	err "Neither curl nor wget was found. Install curl or wget and try again."
	exit 1
}

select_newest_semver_tag() {
	awk '
		/^[vV]?[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z._-]+)?$/ {
			raw = $0
			ver = raw
			sub(/^[vV]/, "", ver)

			dash = index(ver, "-")
			if (dash > 0) {
				core = substr(ver, 1, dash - 1)
				pre = substr(ver, dash + 1)
				is_stable = 0
			} else {
				core = ver
				pre = ""
				is_stable = 1
			}

			n = split(core, parts, ".")
			if (n < 3) {
				next
			}
			maj = parts[1] + 0
			min = parts[2] + 0
			pat = parts[3] + 0

			better = 0
			if (!have_best) {
				better = 1
			} else if (maj != best_maj) {
				better = (maj > best_maj)
			} else if (min != best_min) {
				better = (min > best_min)
			} else if (pat != best_pat) {
				better = (pat > best_pat)
			} else if (is_stable != best_stable) {
				better = (is_stable > best_stable)
			} else if (pre > best_pre) {
				better = 1
			}

			if (better) {
				have_best = 1
				best_raw = raw
				best_maj = maj
				best_min = min
				best_pat = pat
				best_stable = is_stable
				best_pre = pre
			}
		}
		END {
			if (have_best) {
				print best_raw
			}
		}
	'
}

parse_release_tags_from_atom() {
	awk '
		/<link[^>]*\/releases\/tag\/[^"]+"/ {
			line = $0
			sub(/^.*\/releases\/tag\//, "", line)
			sub(/".*$/, "", line)
			print line
		}
	' | select_newest_semver_tag
}

parse_release_tags_from_json() {
	tr '{},' '\n\n\n' | awk '
		/"tag_name"[[:space:]]*:/ {
			line = $0
			sub(/^.*"tag_name"[[:space:]]*:[[:space:]]*"/, "", line)
			sub(/".*$/, "", line)
			tag = line
			if (draft_state == "false") {
				print tag
				tag = ""
				draft_state = ""
			} else if (draft_state == "true") {
				tag = ""
				draft_state = ""
			}
		}
		/"draft"[[:space:]]*:[[:space:]]*false/ {
			if (tag != "") {
				print tag
				tag = ""
				draft_state = ""
			} else {
				draft_state = "false"
			}
		}
		/"draft"[[:space:]]*:[[:space:]]*true/ {
			if (tag != "") {
				tag = ""
				draft_state = ""
			} else {
				draft_state = "true"
			}
		}
	' | select_newest_semver_tag
}

validate_version_tag() {
	candidate="$1"
	case "$candidate" in
		"" | *[!/0-9A-Za-z._-]* | */* | *..*)
			err "Invalid version tag: '${candidate}'."
			exit 1
			;;
	esac
	if ! printf '%s\n' "$candidate" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z._-]+)?$'; then
		err "Invalid semantic version tag: '${candidate}'."
		exit 1
	fi
}

set_resolved_version() {
	raw_tag="$1"
	validate_version_tag "$raw_tag"
	case "$raw_tag" in
		v*) RESOLVED_TAG="$raw_tag" ;;
		*) RESOLVED_TAG="v${raw_tag}" ;;
	esac
	RESOLVED_VERSION_NO_V="${RESOLVED_TAG#v}"
}

resolve_version() {
	requested_version="$1"
	download_base="$2"

	if [ -n "$requested_version" ]; then
		set_resolved_version "$requested_version"
		return 0
	fi

	feed_url="$DEFAULT_FEED_URL"
	api_url="$DEFAULT_API_RELEASES_URL"
	if [ -n "${LITEBASE_DOWNLOAD_BASE:-}" ]; then
		feed_url="${download_base}/releases.atom"
		api_url="${download_base}/releases.json"
	fi

	feed_tmp="${TMP_DIR}/releases.atom"
	feed_status="none"
	feed_exit=0
	if http_download "$feed_url" "$feed_tmp"; then
		feed_status="$LAST_HTTP_STATUS"
		parsed_tag=$(parse_release_tags_from_atom <"$feed_tmp" || true)
		if [ -n "$parsed_tag" ]; then
			set_resolved_version "$parsed_tag"
			return 0
		fi
	else
		feed_status="$LAST_HTTP_STATUS"
		feed_exit="$LAST_CURL_EXIT"
	fi

	releases_tmp="${TMP_DIR}/releases.json"
	api_status="none"
	api_exit=0
	if http_download "$api_url" "$releases_tmp"; then
		api_status="$LAST_HTTP_STATUS"
		parsed_tag=$(parse_release_tags_from_json <"$releases_tmp" || true)
		if [ -n "$parsed_tag" ]; then
			set_resolved_version "$parsed_tag"
			return 0
		fi
	else
		api_status="$LAST_HTTP_STATUS"
		api_exit="$LAST_CURL_EXIT"
	fi

	err "Failed to resolve the latest Litebase release version (feed HTTP ${feed_status}, curl exit ${feed_exit}; API HTTP ${api_status}, curl exit ${api_exit})."
	if [ "$feed_status" = "403" ] || [ "$feed_status" = "429" ] || [ "$api_status" = "403" ] || [ "$api_status" = "429" ]; then
		rate_code="$api_status"
		if [ "$rate_code" != "403" ] && [ "$rate_code" != "429" ]; then
			rate_code="$feed_status"
		fi
		err "HTTP ${rate_code} indicates a probable GitHub rate limit on your IP address."
	fi
	err "Pass --version <tag> or set LITEBASE_VERSION to install a specific release directly, for example:"
	err "  curl -fsSL https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh | LITEBASE_VERSION=v0.0.3-beta sh"
	err "  sh install.sh --version v0.0.3-beta"
	exit 1
}

compute_sha256() {
	target_file="$1"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$target_file" | awk '{print $1}'
		return 0
	fi
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$target_file" | awk '{print $1}'
		return 0
	fi
	err "No SHA-256 tool found (expected sha256sum or shasum). Aborting installation."
	exit 1
}

verify_archive_checksum() {
	archive_path="$1"
	archive_name="$2"
	checksums_path="$3"

	expected_hash=$(awk -v name="$archive_name" '
		NF >= 2 {
			file = $2
			sub(/^\*/, "", file)
			if (file == name) {
				print $1
				exit 0
			}
		}
	' "$checksums_path")

	if [ -z "$expected_hash" ]; then
		err "Archive ${archive_name} not found in ${CHECKSUM_FILE}. Aborting installation."
		exit 1
	fi

	actual_hash=$(compute_sha256 "$archive_path")
	if [ "$actual_hash" != "$expected_hash" ]; then
		err "SHA-256 checksum mismatch for ${archive_name}:"
		err "  expected: ${expected_hash}"
		err "  actual:   ${actual_hash}"
		err "Aborting installation."
		exit 1
	fi
}

select_install_dir() {
	explicit_dir="$1"
	USER_SPECIFIED_DIR=0

	if [ -n "$explicit_dir" ]; then
		INSTALL_DIR="$explicit_dir"
		USER_SPECIFIED_DIR=1
		return 0
	fi

	if [ -d "/usr/local/bin" ] && [ -w "/usr/local/bin" ]; then
		INSTALL_DIR="/usr/local/bin"
		return 0
	fi

	if [ -z "${HOME:-}" ]; then
		err "HOME is not set and /usr/local/bin is not writable. Pass --dir <path>."
		exit 1
	fi
	INSTALL_DIR="${HOME}/.local/bin"
}

is_under_home() {
	dir_path="$1"
	if [ -z "${HOME:-}" ]; then
		return 1
	fi
	case "$dir_path" in
		"$HOME" | "$HOME"/*) return 0 ;;
		*) return 1 ;;
	esac
}

ensure_install_dir_and_privilege() {
	target_dir="$1"
	user_picked="$2"
	NEED_SUDO=0

	if [ -d "$target_dir" ]; then
		if [ -w "$target_dir" ]; then
			return 0
		fi
	else
		parent_dir=$(dirname "$target_dir")
		while [ ! -d "$parent_dir" ] && [ "$parent_dir" != "/" ] && [ "$parent_dir" != "." ]; do
			parent_dir=$(dirname "$parent_dir")
		done
		if [ -w "$parent_dir" ]; then
			mkdir -p "$target_dir"
			return 0
		fi
	fi

	if [ "$user_picked" -eq 1 ] && is_under_home "$target_dir"; then
		err "Directory ${target_dir} is not writable."
		exit 1
	fi

	if [ "$(id -u)" -eq 0 ]; then
		mkdir -p "$target_dir"
		return 0
	fi

	if ! command -v sudo >/dev/null 2>&1; then
		err "Directory ${target_dir} is not writable and sudo is not available. Pass --dir ~/.local/bin."
		exit 1
	fi

	info "Directory ${target_dir} requires elevated permissions; using sudo to install."
	NEED_SUDO=1
	sudo mkdir -p "$target_dir"
}

install_binary_atomically() {
	src_bin="$1"
	target_dir="$2"
	dest_bin="${target_dir}/${BINARY_NAME}"
	STAGED_TARGET="${target_dir}/.${BINARY_NAME}.tmp.$$"

	if [ "${NEED_SUDO:-0}" -eq 1 ]; then
		USED_SUDO=1
		sudo cp "$src_bin" "$STAGED_TARGET"
		sudo chmod 0755 "$STAGED_TARGET"
		sudo mv -f "$STAGED_TARGET" "$dest_bin"
	else
		cp "$src_bin" "$STAGED_TARGET"
		chmod 0755 "$STAGED_TARGET"
		mv -f "$STAGED_TARGET" "$dest_bin"
	fi
	STAGED_TARGET=""
}

main() {
	opt_version="${LITEBASE_VERSION:-}"
	opt_dir="${LITEBASE_INSTALL_DIR:-}"
	opt_dry_run=0

	while [ $# -gt 0 ]; do
		case "$1" in
			-h | --help)
				usage
				exit 0
				;;
			--version)
				if [ $# -lt 2 ]; then
					err "--version requires a tag argument."
					exit 1
				fi
				opt_version="$2"
				shift 2
				;;
			--version=*)
				opt_version="${1#--version=}"
				shift
				;;
			--dir)
				if [ $# -lt 2 ]; then
					err "--dir requires a directory path."
					exit 1
				fi
				opt_dir="$2"
				shift 2
				;;
			--dir=*)
				opt_dir="${1#--dir=}"
				shift
				;;
			--dry-run)
				opt_dry_run=1
				shift
				;;
			*)
				err "Unknown argument: $1"
				usage >&2
				exit 1
				;;
		esac
	done

	detect_platform
	select_install_dir "$opt_dir"

	if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
		err "No SHA-256 tool found (expected sha256sum or shasum). Aborting installation."
		exit 1
	fi

	download_base="${LITEBASE_DOWNLOAD_BASE:-$DEFAULT_DOWNLOAD_BASE}"
	download_base="${download_base%/}"

	TMP_DIR=$(mktemp -d 2>/dev/null || mktemp -d -t litebase-install)
	trap 'cleanup' EXIT
	trap 'cleanup; exit 130' INT
	trap 'cleanup; exit 143' TERM

	resolve_version "$opt_version" "$download_base"

	archive_name="${BINARY_NAME}_${RESOLVED_VERSION_NO_V}_${DETECTED_OS}_${DETECTED_ARCH}.tar.gz"
	archive_url="${download_base}/${RESOLVED_TAG}/${archive_name}"
	checksums_url="${download_base}/${RESOLVED_TAG}/${CHECKSUM_FILE}"
	dest_binary="${INSTALL_DIR}/${BINARY_NAME}"

	info "Installing ${BINARY_NAME} ${RESOLVED_TAG} (${DETECTED_OS}/${DETECTED_ARCH})"
	case "$RESOLVED_TAG" in
		*-beta*)
			info "Note: ${RESOLVED_TAG} is a beta prerelease; APIs and file formats may change before 1.0."
			;;
	esac

	if [ "$opt_dry_run" -eq 1 ]; then
		validate_url_scheme "$archive_url"
		validate_url_scheme "$checksums_url"
		info "Dry run (no changes made):"
		info "  Download archive:   ${archive_url}"
		info "  Download checksums: ${checksums_url}"
		info "  Verify SHA-256 of:  ${archive_name}"
		info "  Install binary to:  ${dest_binary}"
		exit 0
	fi

	archive_tmp="${TMP_DIR}/${archive_name}"
	checksums_tmp="${TMP_DIR}/${CHECKSUM_FILE}"

	info "Downloading ${checksums_url}..."
	if ! http_download "$checksums_url" "$checksums_tmp"; then
		err "Failed to download ${CHECKSUM_FILE} from ${checksums_url} (HTTP ${LAST_HTTP_STATUS}, curl exit ${LAST_CURL_EXIT})."
		if [ "$LAST_HTTP_STATUS" = "403" ] || [ "$LAST_HTTP_STATUS" = "429" ]; then
			err "HTTP ${LAST_HTTP_STATUS} indicates a probable GitHub rate limit on your IP address."
		fi
		exit 1
	fi

	info "Downloading ${archive_url}..."
	if ! http_download "$archive_url" "$archive_tmp"; then
		err "Failed to download release archive from ${archive_url} (HTTP ${LAST_HTTP_STATUS}, curl exit ${LAST_CURL_EXIT})."
		if [ "$LAST_HTTP_STATUS" = "403" ] || [ "$LAST_HTTP_STATUS" = "429" ]; then
			err "HTTP ${LAST_HTTP_STATUS} indicates a probable GitHub rate limit on your IP address."
		fi
		exit 1
	fi

	info "Verifying SHA-256 checksum for ${archive_name}..."
	verify_archive_checksum "$archive_tmp" "$archive_name" "$checksums_tmp"

	extract_dir="${TMP_DIR}/extract"
	mkdir -p "$extract_dir"
	tar -xzf "$archive_tmp" -C "$extract_dir"

	extracted_bin="${extract_dir}/${BINARY_NAME}"
	if [ ! -f "$extracted_bin" ]; then
		err "Archive ${archive_name} did not contain '${BINARY_NAME}' binary."
		exit 1
	fi

	ensure_install_dir_and_privilege "$INSTALL_DIR" "$USER_SPECIFIED_DIR"
	install_binary_atomically "$extracted_bin" "$INSTALL_DIR"

	info "Installed ${BINARY_NAME} to ${dest_binary}"
	"$dest_binary" --version

	case ":${PATH:-}:" in
		*":${INSTALL_DIR}:"*) ;;
		*)
			info ""
			info "Note: ${INSTALL_DIR} is not in your PATH. Add it to your shell profile:"
			info "  export PATH=\"${INSTALL_DIR}:\$PATH\""
			;;
	esac
}

main "$@"
