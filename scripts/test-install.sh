#!/bin/sh
# End-to-end test suite for install.sh using a local python3 HTTP server.

set -eu

REPO_ROOT=$(CDPATH="" cd -- "$(dirname -- "$0")/.." && pwd)
INSTALL_SCRIPT="${REPO_ROOT}/install.sh"

WORK_DIR=""
HTTP_PID=""

cleanup() {
	if [ -n "${HTTP_PID:-}" ]; then
		kill "$HTTP_PID" >/dev/null 2>&1 || true
		wait "$HTTP_PID" 2>/dev/null || true
	fi
	if [ -n "${WORK_DIR:-}" ] && [ -d "$WORK_DIR" ]; then
		rm -rf "$WORK_DIR"
	fi
}

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

pass() {
	printf 'PASS: %s\n' "$*"
}

compute_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
		return 0
	fi
	shasum -a 256 "$1" | awk '{print $1}'
}

detect_host_pair() {
	case "$(uname -s)" in
		Linux) HOST_OS="linux" ;;
		Darwin) HOST_OS="darwin" ;;
		*) fail "test-install.sh must run on Linux or macOS" ;;
	esac
	case "$(uname -m)" in
		x86_64 | amd64) HOST_ARCH="amd64" ;;
		aarch64 | arm64) HOST_ARCH="arm64" ;;
		*) fail "test-install.sh requires amd64 or arm64 host" ;;
	esac
}

write_default_atom_feed() {
	cat >"${SERVER_ROOT}/releases.atom" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>tag:github.com,2008:Repository/1438563413/v0.0.1-beta</id>
    <link rel="alternate" type="text/html" href="https://github.com/ibrahimraimi/litebase/releases/tag/v0.0.1-beta"/>
    <title>v0.0.1-beta</title>
  </entry>
  <entry>
    <id>tag:github.com,2008:Repository/1438563413/v0.0.3-beta</id>
    <link rel="alternate" type="text/html" href="https://github.com/ibrahimraimi/litebase/releases/tag/v0.0.3-beta"/>
    <title>Litebase v0.0.3-beta</title>
  </entry>
  <entry>
    <id>tag:github.com,2008:Repository/1438563413/v0.0.2-beta</id>
    <link rel="alternate" type="text/html" href="https://github.com/ibrahimraimi/litebase/releases/tag/v0.0.2-beta"/>
    <title>v0.0.2-beta</title>
  </entry>
</feed>
EOF
}

write_default_releases_json() {
	cat >"${SERVER_ROOT}/releases.json" <<'EOF'
[
  {
    "tag_name": "v0.0.4-beta",
    "name": "Litebase v0.0.4-beta",
    "draft": true,
    "prerelease": true
  },
  {
    "tag_name": "v0.0.3-beta",
    "name": "Litebase v0.0.3-beta",
    "draft": false,
    "prerelease": true
  },
  {
    "tag_name": "v0.0.2-beta",
    "name": "Litebase v0.0.2-beta",
    "draft": false,
    "prerelease": true
  }
]
EOF
}

reset_lookup_fixtures() {
	rm -f "${SERVER_ROOT}/releases.atom.status" "${SERVER_ROOT}/releases.json.status"
	write_default_atom_feed
	write_default_releases_json
	: >"$REQUEST_LOG"
}

setup_fixture_server() {
	WORK_DIR=$(mktemp -d 2>/dev/null || mktemp -d -t litebase-test-install)
	trap 'cleanup' EXIT
	trap 'cleanup; exit 130' INT
	trap 'cleanup; exit 143' TERM

	detect_host_pair

	SERVER_ROOT="${WORK_DIR}/srv"
	STAGE_DIR="${WORK_DIR}/stage"
	REQUEST_LOG="${WORK_DIR}/requests.log"
	: >"$REQUEST_LOG"
	mkdir -p "${SERVER_ROOT}/v0.0.3-beta" "${SERVER_ROOT}/v0.0.2-beta" "${SERVER_ROOT}/v0.0.1-beta" "$STAGE_DIR"

	cat >"${STAGE_DIR}/litebase" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "--version" ]; then
	echo "litebase version 0.0.3-beta (commit test1234, built 2026-11-11T00:00:00Z)"
	exit 0
fi
echo "fake litebase"
EOF
	chmod 0755 "${STAGE_DIR}/litebase"
	printf '# Litebase\n' >"${STAGE_DIR}/README.md"
	printf '# Changelog\n' >"${STAGE_DIR}/CHANGELOG.md"

	ARCHIVE_NAME="litebase_0.0.3-beta_${HOST_OS}_${HOST_ARCH}.tar.gz"
	tar -C "$STAGE_DIR" -czf "${SERVER_ROOT}/v0.0.3-beta/${ARCHIVE_NAME}" litebase README.md CHANGELOG.md
	GOOD_SHA=$(compute_sha256 "${SERVER_ROOT}/v0.0.3-beta/${ARCHIVE_NAME}")
	printf '%s  %s\n' "$GOOD_SHA" "$ARCHIVE_NAME" >"${SERVER_ROOT}/v0.0.3-beta/checksums.txt"

	# v0.0.2-beta has a deliberately corrupted checksum in checksums.txt
	ARCHIVE_V2="litebase_0.0.2-beta_${HOST_OS}_${HOST_ARCH}.tar.gz"
	cp "${SERVER_ROOT}/v0.0.3-beta/${ARCHIVE_NAME}" "${SERVER_ROOT}/v0.0.2-beta/${ARCHIVE_V2}"
	printf '0000000000000000000000000000000000000000000000000000000000000000  %s\n' "$ARCHIVE_V2" >"${SERVER_ROOT}/v0.0.2-beta/checksums.txt"

	# v0.0.1-beta is missing our archive entry from checksums.txt
	ARCHIVE_V1="litebase_0.0.1-beta_${HOST_OS}_${HOST_ARCH}.tar.gz"
	cp "${SERVER_ROOT}/v0.0.3-beta/${ARCHIVE_NAME}" "${SERVER_ROOT}/v0.0.1-beta/${ARCHIVE_V1}"
	printf '%s  litebase_0.0.1-beta_other_arch.tar.gz\n' "$GOOD_SHA" >"${SERVER_ROOT}/v0.0.1-beta/checksums.txt"

	reset_lookup_fixtures

	PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
	python3 - "$PORT" "$SERVER_ROOT" "$REQUEST_LOG" >/dev/null 2>&1 <<'PYEOF' &
import http.server
import os
import sys

port = int(sys.argv[1])
root = sys.argv[2]
req_log = sys.argv[3]

class FixtureHandler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=root, **kwargs)

    def log_message(self, format, *args):
        pass

    def do_GET(self):
        clean_path = self.path.split("?", 1)[0].lstrip("/")
        with open(req_log, "a", encoding="utf-8") as lf:
            lf.write(clean_path + "\n")
        status_override = os.path.join(root, clean_path + ".status")
        if os.path.isfile(status_override):
            with open(status_override, "r", encoding="utf-8") as sf:
                code = int(sf.read().strip())
            body = b'{"message":"SECRET_ERROR_BODY_DO_NOT_PRINT"}\n'
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        super().do_GET()

httpd = http.server.HTTPServer(("127.0.0.1", port), FixtureHandler)
httpd.serve_forever()
PYEOF
	HTTP_PID=$!

	i=0
	while [ "$i" -lt 30 ]; do
		if curl -fsS "http://127.0.0.1:${PORT}/releases.atom" >/dev/null 2>&1; then
			break
		fi
		i=$((i + 1))
		sleep 0.1
	done
	BASE_URL="http://127.0.0.1:${PORT}"
}

run_suite_for_shell() {
	shell_bin="$1"
	info_prefix="[${shell_bin}]"

	"$shell_bin" -n "$INSTALL_SCRIPT"
	pass "${info_prefix} syntax check (-n)"

	# 1. Atom feed returns multiple tags out of order: newest (v0.0.3-beta) picked without hitting REST API
	reset_lookup_fixtures
	dest_ok="${WORK_DIR}/bin-ok-${shell_bin##*/}"
	out_ok=$(LITEBASE_DOWNLOAD_BASE="$BASE_URL" "$shell_bin" "$INSTALL_SCRIPT" --dir "$dest_ok" 2>&1)
	if [ ! -x "${dest_ok}/litebase" ]; then
		fail "${info_prefix} expected ${dest_ok}/litebase to be executable after install"
	fi
	case "$out_ok" in
		*"Installing litebase v0.0.3-beta"* ) ;;
		*) fail "${info_prefix} expected output to mention v0.0.3-beta, got: ${out_ok}" ;;
	esac
	case "$out_ok" in
		*"beta prerelease"* ) ;;
		*) fail "${info_prefix} expected beta prerelease note in output, got: ${out_ok}" ;;
	esac
	case "$out_ok" in
		*"litebase version 0.0.3-beta"* ) ;;
		*) fail "${info_prefix} expected litebase --version output after install, got: ${out_ok}" ;;
	esac
	if grep -qx "releases.json" "$REQUEST_LOG"; then
		fail "${info_prefix} expected Atom feed resolution not to call releases.json"
	fi
	pass "${info_prefix} Atom feed resolves newest tag (v0.0.3-beta) & installs cleanly"

	# 2. Atom feed unavailable (404), REST API works: fallback succeeds
	reset_lookup_fixtures
	rm -f "${SERVER_ROOT}/releases.atom"
	dest_fallback="${WORK_DIR}/bin-fallback-${shell_bin##*/}"
	out_fallback=$(LITEBASE_DOWNLOAD_BASE="$BASE_URL" "$shell_bin" "$INSTALL_SCRIPT" --dir "$dest_fallback" 2>&1)
	if [ ! -x "${dest_fallback}/litebase" ]; then
		fail "${info_prefix} expected fallback install to produce ${dest_fallback}/litebase"
	fi
	case "$out_fallback" in
		*"Installing litebase v0.0.3-beta"* ) ;;
		*) fail "${info_prefix} expected fallback to resolve v0.0.3-beta, got: ${out_fallback}" ;;
	esac
	if ! grep -qx "releases.json" "$REQUEST_LOG"; then
		fail "${info_prefix} expected fallback to query releases.json when releases.atom fails"
	fi
	pass "${info_prefix} Atom feed unavailable -> REST API fallback works"

	# 3. Both feed and API fail (500): clear error, nothing installed, non-zero exit
	reset_lookup_fixtures
	printf '500\n' >"${SERVER_ROOT}/releases.atom.status"
	printf '500\n' >"${SERVER_ROOT}/releases.json.status"
	dest_both_fail="${WORK_DIR}/bin-both-fail-${shell_bin##*/}"
	if out_both_fail=$(LITEBASE_DOWNLOAD_BASE="$BASE_URL" "$shell_bin" "$INSTALL_SCRIPT" --dir "$dest_both_fail" 2>&1); then
		fail "${info_prefix} expected failure when both feed and API return 500"
	fi
	if [ -e "${dest_both_fail}/litebase" ]; then
		fail "${info_prefix} expected nothing installed when version lookup fails"
	fi
	case "$out_both_fail" in
		*"feed HTTP 500"*"API HTTP 500"* ) ;;
		*) fail "${info_prefix} expected HTTP 500 status in error message, got: ${out_both_fail}" ;;
	esac
	case "$out_both_fail" in
		*"LITEBASE_VERSION=v0.0.3-beta"* ) ;;
		*) fail "${info_prefix} expected LITEBASE_VERSION workaround in error message, got: ${out_both_fail}" ;;
	esac
	case "$out_both_fail" in
		*"SECRET_ERROR_BODY_DO_NOT_PRINT"* )
			fail "${info_prefix} installer must never print the error response body"
			;;
	esac
	pass "${info_prefix} both feed and API fail -> clear error, no body leak, nothing installed"

	# 4. HTTP 403 from both sources prints the rate-limit hint
	reset_lookup_fixtures
	printf '403\n' >"${SERVER_ROOT}/releases.atom.status"
	printf '403\n' >"${SERVER_ROOT}/releases.json.status"
	dest_403="${WORK_DIR}/bin-403-${shell_bin##*/}"
	if out_403=$(LITEBASE_DOWNLOAD_BASE="$BASE_URL" "$shell_bin" "$INSTALL_SCRIPT" --dir "$dest_403" 2>&1); then
		fail "${info_prefix} expected failure when both feed and API return 403"
	fi
	case "$out_403" in
		*"HTTP 403 indicates a probable GitHub rate limit"* ) ;;
		*) fail "${info_prefix} expected HTTP 403 rate-limit hint, got: ${out_403}" ;;
	esac
	case "$out_403" in
		*"SECRET_ERROR_BODY_DO_NOT_PRINT"* )
			fail "${info_prefix} installer must never print 403 response body"
			;;
	esac
	pass "${info_prefix} HTTP 403 prints rate-limit hint and --version workaround"

	# 5. LITEBASE_VERSION skips lookups entirely (server receives no feed or API request)
	reset_lookup_fixtures
	dest_env="${WORK_DIR}/bin-env-${shell_bin##*/}"
	LITEBASE_DOWNLOAD_BASE="$BASE_URL" LITEBASE_VERSION="v0.0.3-beta" \
		"$shell_bin" "$INSTALL_SCRIPT" --dir "$dest_env" >/dev/null 2>&1
	if [ ! -x "${dest_env}/litebase" ]; then
		fail "${info_prefix} expected LITEBASE_VERSION install to succeed"
	fi
	if grep -Eq '^releases\.(atom|json)$' "$REQUEST_LOG"; then
		fail "${info_prefix} expected LITEBASE_VERSION to make zero requests to releases.atom or releases.json, got: $(cat "$REQUEST_LOG")"
	fi
	pass "${info_prefix} LITEBASE_VERSION skips feed and API lookups completely"

	# 6. Checksum mismatch aborts and leaves nothing installed
	reset_lookup_fixtures
	dest_bad_sha="${WORK_DIR}/bin-bad-sha-${shell_bin##*/}"
	if LITEBASE_DOWNLOAD_BASE="$BASE_URL" "$shell_bin" "$INSTALL_SCRIPT" --version v0.0.2-beta --dir "$dest_bad_sha" >/dev/null 2>&1; then
		fail "${info_prefix} expected checksum mismatch to fail non-zero"
	fi
	if [ -e "${dest_bad_sha}/litebase" ]; then
		fail "${info_prefix} expected no binary installed on checksum mismatch"
	fi
	pass "${info_prefix} checksum mismatch aborts cleanly"

	# 7. Missing checksum entry aborts and leaves nothing installed
	dest_missing_sha="${WORK_DIR}/bin-missing-sha-${shell_bin##*/}"
	if LITEBASE_DOWNLOAD_BASE="$BASE_URL" "$shell_bin" "$INSTALL_SCRIPT" --version v0.0.1-beta --dir "$dest_missing_sha" >/dev/null 2>&1; then
		fail "${info_prefix} expected missing checksum entry to fail non-zero"
	fi
	if [ -e "${dest_missing_sha}/litebase" ]; then
		fail "${info_prefix} expected no binary installed when archive checksum entry is missing"
	fi
	pass "${info_prefix} missing checksum entry aborts cleanly"

	# 8. --dry-run changes nothing on disk
	dest_dry="${WORK_DIR}/bin-dry-${shell_bin##*/}"
	out_dry=$(LITEBASE_DOWNLOAD_BASE="$BASE_URL" "$shell_bin" "$INSTALL_SCRIPT" --version 0.0.3-beta --dir "$dest_dry" --dry-run 2>&1)
	if [ -e "$dest_dry" ]; then
		fail "${info_prefix} expected --dry-run not to create target directory ${dest_dry}"
	fi
	case "$out_dry" in
		*"Dry run (no changes made):"* ) ;;
		*) fail "${info_prefix} expected dry-run banner, got: ${out_dry}" ;;
	esac
	pass "${info_prefix} --dry-run makes no changes"

	# 9. Unsupported architecture message
	if out_arch=$(LITEBASE_TEST_UNAME_S="Linux" LITEBASE_TEST_UNAME_M="armv7l" LITEBASE_TEST_IS_MUSL="0" "$shell_bin" "$INSTALL_SCRIPT" --version v0.0.3-beta 2>&1); then
		fail "${info_prefix} expected unsupported arch armv7l to exit non-zero"
	fi
	case "$out_arch" in
		*"Unsupported CPU architecture: armv7l"*"docker build -t litebase:latest ."* ) ;;
		*) fail "${info_prefix} expected unsupported arch message with docker build command, got: ${out_arch}" ;;
	esac
	pass "${info_prefix} unsupported architecture rejected with message"

	# 10. Linux musl detection message
	if out_musl=$(LITEBASE_TEST_UNAME_S="Linux" LITEBASE_TEST_UNAME_M="x86_64" LITEBASE_TEST_IS_MUSL="1" "$shell_bin" "$INSTALL_SCRIPT" --version v0.0.3-beta 2>&1); then
		fail "${info_prefix} expected musl Linux to exit non-zero"
	fi
	case "$out_musl" in
		*"Detected musl libc"*"docker build -t litebase:latest ."* ) ;;
		*) fail "${info_prefix} expected musl message with docker build command, got: ${out_musl}" ;;
	esac
	pass "${info_prefix} musl Linux rejected with Dockerfile build/source guidance"

	# 11. Windows detection message
	if out_win=$(LITEBASE_TEST_UNAME_S="MINGW64_NT-10.0" LITEBASE_TEST_UNAME_M="x86_64" "$shell_bin" "$INSTALL_SCRIPT" --version v0.0.3-beta 2>&1); then
		fail "${info_prefix} expected Windows MINGW64 to exit non-zero"
	fi
	case "$out_win" in
		*"Windows is not supported by install.sh"*"docker build -t litebase:latest ."* ) ;;
		*) fail "${info_prefix} expected Windows rejection message with docker build command, got: ${out_win}" ;;
	esac
	pass "${info_prefix} Windows rejected without touching System32"

	# 12. Truncated script guard (head -c 500 install.sh | sh) executes nothing
	trunc_dir="${WORK_DIR}/trunc-${shell_bin##*/}"
	mkdir -p "$trunc_dir"
	head -c 500 "$INSTALL_SCRIPT" | "$shell_bin" >/dev/null 2>&1 || true
	if [ -e "${trunc_dir}/litebase" ]; then
		fail "${info_prefix} truncated script should not install anything"
	fi
	pass "${info_prefix} truncated script (head -c 500) executes nothing harmful"
}

main() {
	setup_fixture_server
	run_suite_for_shell "sh"
	if command -v dash >/dev/null 2>&1; then
		run_suite_for_shell "dash"
	fi
	if command -v bash >/dev/null 2>&1; then
		run_suite_for_shell "bash"
	fi
	printf '\nAll install.sh tests passed.\n'
}

main "$@"
