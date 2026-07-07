#!/usr/bin/env bash
set -euo pipefail

if [[ -t 1 ]]; then
    C_RESET=$'\033[0m'
    C_GREEN=$'\033[0;32m'
    C_CYAN=$'\033[0;36m'
    C_YELLOW=$'\033[0;33m'
    C_BOLD_RED=$'\033[1;31m'
    C_BOLD_GREEN=$'\033[1;32m'
    C_BOLD_CYAN=$'\033[1;36m'
    C_BOLD_WHITE=$'\033[1;37m'
else
    C_RESET='' C_GREEN='' C_CYAN='' C_YELLOW='' C_BOLD_RED='' C_BOLD_GREEN='' C_BOLD_CYAN='' C_BOLD_WHITE=''
fi

die() { printf -- "${C_BOLD_RED}ERROR: %s${C_RESET}\n" "$*" >&2; exit 1; }

if [[ "$(uname -s)" == "Darwin" ]]; then
    die "macOS is not supported as a Teamster hub — this installer only runs on apt-based Linux. macOS is a remote-only platform; do not run this on the Mac. From your Teamster server (the Linux hub) run 'teamster install-remote <user>@<this-mac>' to enroll this Mac over SSH. See docs/specs/REMOTE-INSTALL.md."
fi

# Bug #18: NFS-mounted repos (foreign uid) break git's VCS status, which
# Go's default -buildvcs=true requires. Disable VCS stamping for every
# downstream go/make invocation; we don't embed VCS info into Teamster
# binaries anyway.
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
BASEDIR="${HOME}/teamster"
BUILDDIR="$REPO/build"
GO_MIN_MINOR=22

# --- Round 0 debug-log tracing (locked format with install.sh) ---
# Line shape: "<RFC3339-UTC> <LEVEL5> <component> <msg>[ k=v ...]"
# Levels: TRACE DEBUG INFO  WARN  ERROR (all 5 chars; INFO and WARN pad with one space).
# Components: install.parse, install.apt, install.compile, install.bundle,
# install.hookd, install.subproc, install.unit-sync.
# Empty DEBUG_LOG = silent no-op; non-empty unwritable path = loud die.
DEBUG_LOG=""

_dlog_pad() {
    case "$1" in
        INFO|WARN) printf '%s ' "$1" ;;
        *)         printf '%s'  "$1" ;;
    esac
}

dlog() {
    # dlog <level> <component> <msg> [k=v ...]
    [[ -z "$DEBUG_LOG" ]] && return 0
    local level="$1" component="$2" msg="$3"; shift 3
    local ts lvl line pair
    ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    lvl="$(_dlog_pad "$level")"
    line="${ts} ${lvl} ${component} ${msg}"
    for pair in "$@"; do
        line+=" $pair"
    done
    printf '%s\n' "$line" >> "$DEBUG_LOG"
}

dtrace() {
    # dtrace <component> <marker> <fn> — marker is ">>" or "<<"
    [[ -z "$DEBUG_LOG" ]] && return 0
    dlog TRACE "$1" "$2 $3"
}

# sed_escape_replacement escapes a string for safe use on the REPLACEMENT side
# of a sed `s|...|...|` command that uses `|` as the delimiter. Without this, a
# value containing sed metachars (`\`, `&`), the `|` delimiter, or a newline
# corrupts the substitution. Today the grafana_ro password is pure lowercase hex
# (od -tx1), so escaping is a no-op on it — but a future generator change (e.g.
# base64 with `/ + &`) would break the YAML/SQL render silently without this.
# Escape order matters: backslash first, then & and the | delimiter; a literal
# newline becomes `\n` so the value stays on the replacement line.
sed_escape_replacement() {
    printf '%s' "$1" | sed -e 's/[\&|]/\\&/g' -e ':a' -e '$!N;s/\n/\\n/;ta'
}

# version_lt returns 0 (true) if $1 < $2 using dotted-decimal comparison.
version_lt() {
    [[ "$1" == "$2" ]] && return 1
    local IFS=.
    local -a a=($1) b=($2)
    local i ai bi max=$(( ${#a[@]} > ${#b[@]} ? ${#a[@]} : ${#b[@]} ))
    for (( i=0; i<max; i++ )); do
        ai="${a[i]:-0}"; bi="${b[i]:-0}"
        (( ai < bi )) && return 0
        (( ai > bi )) && return 1
    done
    return 1
}

# --- Flag parsing ---
# Modes:
#   hub mode (default):   --hookd-mode=systemd|supervisor  compile Go, run hookd locally
#   client mode:          --hookd-mode=external             Python hook client, point at hub
WIRE=""   # empty = "decide after flag parsing"
BASEDIR_EXPLICIT=0

# Per-service mode flags
HOOKD_MODE=""         # --hookd-mode=systemd|supervisor|external
STORE_MODE=""         # --store-mode=install|external|managed
OTELCOL_MODE=""       # --otelcol-mode=install|external|managed|none
PROMETHEUS_MODE=""    # --prometheus-mode=install|external|managed|none
GRAFANA_MODE=""       # --grafana-mode=install|external|managed|none
RELAY_MODE=""         # --relay-mode=none|install

# Per-service endpoint flags
STORE_DSN=""             # --store-dsn=<mysql://...>
HOOKD_ENDPOINT=""        # --hookd-endpoint=<host:port-or-URL>
OTELCOL_ENDPOINT=""      # --otelcol-endpoint=<host:port-or-URL>
PROMETHEUS_ENDPOINT=""   # --prometheus-endpoint=<host:port-or-URL>
GRAFANA_ENDPOINT=""      # --grafana-endpoint=<host:port-or-URL>

# Per-service build-from-source flags
OTELCOL_BUILD_FROM_SRC=0    # --otelcol-build-from-src
PROMETHEUS_BUILD_FROM_SRC=0 # --prometheus-build-from-src
GRAFANA_BUILD_FROM_SRC=0    # --grafana-build-from-src

# Per-service config flags
PROMETHEUS_RETENTION=""       # --prometheus-retention=<duration>
PROMETHEUS_RETENTION_SIZE=""  # --prometheus-retention-size=<size, e.g. 50GB>

# Backup config flags
BACKUP_DIR=""       # --backup-dir=<path>
BACKUP_SCHEDULE=""  # --backup-schedule=<duration>

# Relay / replication flags
RELAY_TARGET=""          # --relay-target=<URL>
REPL_PUSH_REMOTE=""      # --repl-push-remote=<user@host>
HOOKD_READ_ONLY=0        # --hookd-read-only (replica deployments)

# Retained flags
TEAMSTER_ENV=""    # --env=<name>

# Accept both --flag=VALUE and --flag VALUE forms for every value-taking flag.
# Unknown args die loudly (see [[no-silent-failures]]).
require_value() {
    local flag="$1" next="$2"
    [[ -z "$next" || "$next" == --* ]] && die "$flag requires a value"
    return 0
}
while [[ $# -gt 0 ]]; do
    case "$1" in
        # --- removed flags: die with migration guidance ---
        --bundle=*|--bundle)
            die "ERROR: --bundle is removed. Use per-service --<service>-mode flags instead.
  Migration: --bundle=all → --otelcol-mode=install --prometheus-mode=install --grafana-mode=install
  See: $0 --help" ;;
        --store=*|--store)
            die "--store is removed. Use --store-dsn instead." ;;
        --teamster-server=*|--teamster-server)
            die "--teamster-server is removed. Use --hookd-mode=external --hookd-endpoint=URL instead." ;;
        --build-otelcol)
            die "--build-otelcol is removed. Use --otelcol-build-from-src instead." ;;
        --build-prometheus)
            die "--build-prometheus is removed. Use --prometheus-build-from-src instead." ;;
        --build-grafana)
            die "--build-grafana is removed. Use --grafana-build-from-src instead." ;;
        --retention=*|--retention)
            die "--retention is removed. Use --prometheus-retention instead." ;;
        # --- per-service mode flags ---
        --hookd-mode=*)         HOOKD_MODE="${1#--hookd-mode=}"; shift ;;
        --hookd-mode)           require_value "$1" "${2-}"; HOOKD_MODE="$2"; shift 2 ;;
        --store-mode=*)         STORE_MODE="${1#--store-mode=}"; shift ;;
        --store-mode)           require_value "$1" "${2-}"; STORE_MODE="$2"; shift 2 ;;
        --otelcol-mode=*)       OTELCOL_MODE="${1#--otelcol-mode=}"; shift ;;
        --otelcol-mode)         require_value "$1" "${2-}"; OTELCOL_MODE="$2"; shift 2 ;;
        --prometheus-mode=*)    PROMETHEUS_MODE="${1#--prometheus-mode=}"; shift ;;
        --prometheus-mode)      require_value "$1" "${2-}"; PROMETHEUS_MODE="$2"; shift 2 ;;
        --grafana-mode=*)       GRAFANA_MODE="${1#--grafana-mode=}"; shift ;;
        --grafana-mode)         require_value "$1" "${2-}"; GRAFANA_MODE="$2"; shift 2 ;;
        --relay-mode=*)         RELAY_MODE="${1#--relay-mode=}"; shift ;;
        --relay-mode)           require_value "$1" "${2-}"; RELAY_MODE="$2"; shift 2 ;;
        # --- per-service endpoint flags ---
        --store-dsn=*)          STORE_DSN="${1#--store-dsn=}"; shift ;;
        --store-dsn)            require_value "$1" "${2-}"; STORE_DSN="$2"; shift 2 ;;
        --hookd-endpoint=*)     HOOKD_ENDPOINT="${1#--hookd-endpoint=}"; shift ;;
        --hookd-endpoint)       require_value "$1" "${2-}"; HOOKD_ENDPOINT="$2"; shift 2 ;;
        --otelcol-endpoint=*)   OTELCOL_ENDPOINT="${1#--otelcol-endpoint=}"; shift ;;
        --otelcol-endpoint)     require_value "$1" "${2-}"; OTELCOL_ENDPOINT="$2"; shift 2 ;;
        --prometheus-endpoint=*) PROMETHEUS_ENDPOINT="${1#--prometheus-endpoint=}"; shift ;;
        --prometheus-endpoint)  require_value "$1" "${2-}"; PROMETHEUS_ENDPOINT="$2"; shift 2 ;;
        --grafana-endpoint=*)   GRAFANA_ENDPOINT="${1#--grafana-endpoint=}"; shift ;;
        --grafana-endpoint)     require_value "$1" "${2-}"; GRAFANA_ENDPOINT="$2"; shift 2 ;;
        # --- per-service build-from-source flags ---
        --otelcol-build-from-src)    OTELCOL_BUILD_FROM_SRC=1; shift ;;
        --prometheus-build-from-src) PROMETHEUS_BUILD_FROM_SRC=1; shift ;;
        --grafana-build-from-src)    GRAFANA_BUILD_FROM_SRC=1; shift ;;
        # --- per-service config flags ---
        --prometheus-retention=*) PROMETHEUS_RETENTION="${1#--prometheus-retention=}"; shift ;;
        --prometheus-retention)   require_value "$1" "${2-}"; PROMETHEUS_RETENTION="$2"; shift 2 ;;
        --prometheus-retention-size=*) PROMETHEUS_RETENTION_SIZE="${1#--prometheus-retention-size=}"; shift ;;
        --prometheus-retention-size)   require_value "$1" "${2-}"; PROMETHEUS_RETENTION_SIZE="$2"; shift 2 ;;
        # --- backup config flags ---
        --backup-dir=*)           BACKUP_DIR="${1#--backup-dir=}"; shift ;;
        --backup-dir)             require_value "$1" "${2-}"; BACKUP_DIR="$2"; shift 2 ;;
        --backup-schedule=*)      BACKUP_SCHEDULE="${1#--backup-schedule=}"; shift ;;
        --backup-schedule)        require_value "$1" "${2-}"; BACKUP_SCHEDULE="$2"; shift 2 ;;
        # --- relay / replication flags ---
        --relay-target=*)       RELAY_TARGET="${1#--relay-target=}"; shift ;;
        --relay-target)         require_value "$1" "${2-}"; RELAY_TARGET="$2"; shift 2 ;;
        --repl-push-remote=*)   REPL_PUSH_REMOTE="${1#--repl-push-remote=}"; shift ;;
        --repl-push-remote)     require_value "$1" "${2-}"; REPL_PUSH_REMOTE="$2"; shift 2 ;;
        --hookd-read-only)      HOOKD_READ_ONLY=1; shift ;;
        # --- aliases for backward compat ---
        --systemd-hookd)      HOOKD_MODE="systemd"; shift ;;
        --supervisor-hookd)   HOOKD_MODE="supervisor"; shift ;;
        # --- retained flags ---
        --basedir=*)          BASEDIR="${1#--basedir=}"; BASEDIR_EXPLICIT=1; shift ;;
        --basedir)            require_value "$1" "${2-}"; BASEDIR="$2"; BASEDIR_EXPLICIT=1; shift 2 ;;
        --env=*)              TEAMSTER_ENV="${1#--env=}"; shift ;;
        --env)                require_value "$1" "${2-}"; TEAMSTER_ENV="$2"; shift 2 ;;
        --debug-log=*)        DEBUG_LOG="${1#--debug-log=}"; shift ;;
        --debug-log)          require_value "$1" "${2-}"; DEBUG_LOG="$2"; shift 2 ;;
        --wire)               WIRE=1; shift ;;
        --help|-h)
            echo "Usage: $0 [options]"
            echo ""
            echo "Hub mode (default — compiles Go, installs full Teamster hub):"
            echo "  $0                                         stage + wire to ~/teamster/ (default)"
            echo "  $0 --basedir=DIR                           stage to DIR only — safe, no global state touched"
            echo "  $0 --basedir=DIR --wire                    stage to DIR AND wire (touches systemd + settings.json)"
            echo ""
            echo "Full observability stack:"
            echo "  $0 --otelcol-mode=install --prometheus-mode=install --grafana-mode=install --hookd-mode=supervisor"
            echo ""
            echo "Client mode (no Go required — wires Claude Code to a running hub):"
            echo "  $0 --hookd-mode=external --hookd-endpoint=host:port [--basedir=DIR]"
            echo ""
            echo "Per-service mode flags:"
            echo "  --hookd-mode=MODE        hookd mode: systemd (default) | supervisor | external"
            echo "  --store-mode=MODE        store mode: managed (default) | install | external"
            echo "  --otelcol-mode=MODE      otelcol mode: install (default) | external | managed | none"
            echo "  --prometheus-mode=MODE   prometheus mode: install (default) | external | managed | none"
            echo "  --grafana-mode=MODE      grafana mode: none (default) | install | external | managed"
            echo "  --relay-mode=MODE        relay mode: none (default) | install"
            echo ""
            echo "Per-service endpoint flags:"
            echo "  --store-dsn=DSN              MySQL DSN (mysql://user:pass@host:port/db)"
            echo "  --hookd-endpoint=URL         Teamster hub URL. Sets TEAMSTER_HOOK_SERVER_URL."
            echo "  --otelcol-endpoint=URL        OTLP endpoint. Sets OTEL_EXPORTER_OTLP_ENDPOINT."
            echo "  --prometheus-endpoint=URL     Prometheus endpoint. Plumbed informationally."
            echo "  --grafana-endpoint=URL        Grafana endpoint. Plumbed informationally."
            echo "  --relay-target=URL            Target hookd URL for relay (required when --relay-mode=install)."
            echo "  --repl-push-remote=DEST       Repl-push SCP destination, user@host (required when --relay-mode=install)."
            echo "  --hookd-read-only             Run hookd in read-only mode (replica deployments: rejects MCP/telemetry/drain)."
            echo ""
            echo "Per-service build flags (only with mode=install):"
            echo "  --otelcol-build-from-src     Build otelcol-contrib from source instead of downloading"
            echo "  --prometheus-build-from-src  Build prometheus from source instead of downloading"
            echo "  --grafana-build-from-src     Build grafana from source instead of downloading"
            echo "  --prometheus-retention=DUR   Prometheus retention period (default: 365d)"
            echo "  --prometheus-retention-size=SIZE  Prometheus retention size cap (e.g. 50GB; default: none)"
            echo ""
            echo "Other flags:"
            echo "  --basedir=DIR            Installation target directory (default: ~/teamster)"
            echo "  --wire                   Wire global state (systemd unit, settings.json, MCP, CLAUDE.md)"
            echo "  --env=NAME               Deployment environment label (default: production)"
            echo "  --debug-log=PATH         Append structured trace to PATH."
            echo ""
            echo "Verification / cleanroom testing:"
            echo "  Run a clean install on a disposable test VM (reset it first), not on"
            echo "  the hub host. --basedir=DIR (without --wire) stages without touching"
            echo "  systemd or settings.json if you only need to inspect the layout."
            exit 0
            ;;
        *)
            die "Unknown argument: $1 (run with --help for usage)"
            ;;
    esac
done

# Decide wiring default: wire if using the default basedir, stage-only if --basedir was explicit.
if [[ -z "$WIRE" ]]; then
    if [[ "$BASEDIR_EXPLICIT" -eq 1 ]]; then
        WIRE=0
    else
        WIRE=1
    fi
fi

# Open --debug-log if requested. Unwritable path = loud die (no silent fallback).
if [[ -n "$DEBUG_LOG" ]]; then
    mkdir -p "$(dirname "$DEBUG_LOG")" \
        || die "--debug-log: cannot create parent dir for $DEBUG_LOG"
    if ! { : >> "$DEBUG_LOG"; } 2>/dev/null; then
        die "--debug-log: $DEBUG_LOG is not writable"
    fi
fi

dlog INFO install.parse "parsed flags" \
    "basedir=$BASEDIR" \
    "basedir_explicit=$BASEDIR_EXPLICIT" \
    "wire=$WIRE" \
    "hookd_mode=$HOOKD_MODE" \
    "store_mode=$STORE_MODE" \
    "otelcol_mode=$OTELCOL_MODE" \
    "prometheus_mode=$PROMETHEUS_MODE" \
    "grafana_mode=$GRAFANA_MODE" \
    "relay_mode=$RELAY_MODE" \
    "relay_target=$RELAY_TARGET" \
    "repl_push_remote=$REPL_PUSH_REMOTE" \
    "hookd_read_only=$HOOKD_READ_ONLY" \
    "store_dsn=$STORE_DSN" \
    "hookd_endpoint=$HOOKD_ENDPOINT" \
    "otelcol_endpoint=$OTELCOL_ENDPOINT" \
    "prometheus_endpoint=$PROMETHEUS_ENDPOINT" \
    "grafana_endpoint=$GRAFANA_ENDPOINT" \
    "prometheus_retention=$PROMETHEUS_RETENTION" \
    "prometheus_retention_size=$PROMETHEUS_RETENTION_SIZE" \
    "env=$TEAMSTER_ENV" \
    "debug_log=$DEBUG_LOG"

# --- Validation ---
[[ "$STORE_MODE" == "external" || "$STORE_MODE" == "managed" ]] && [[ -z "$STORE_DSN" ]] \
    && die "--store-mode=$STORE_MODE requires --store-dsn"
[[ "$HOOKD_MODE" == "external" ]] && [[ -z "$HOOKD_ENDPOINT" ]] \
    && die "--hookd-mode=external requires --hookd-endpoint"
[[ "$OTELCOL_MODE" == "external" ]] && [[ -z "$OTELCOL_ENDPOINT" ]] \
    && die "--otelcol-mode=external requires --otelcol-endpoint"
[[ "$PROMETHEUS_MODE" == "external" ]] && [[ -z "$PROMETHEUS_ENDPOINT" ]] \
    && die "--prometheus-mode=external requires --prometheus-endpoint"
[[ "$GRAFANA_MODE" == "external" ]] && [[ -z "$GRAFANA_ENDPOINT" ]] \
    && die "--grafana-mode=external requires --grafana-endpoint"
if [[ "${RELAY_MODE:-none}" == "install" ]]; then
    [[ -z "$RELAY_TARGET" ]]     && die "--relay-target is required when --relay-mode=install"
    [[ -z "$REPL_PUSH_REMOTE" ]] && die "--repl-push-remote is required when --relay-mode=install"
fi
[[ "$OTELCOL_BUILD_FROM_SRC" -eq 1 ]] && [[ "$OTELCOL_MODE" != "install" ]] \
    && die "--otelcol-build-from-src requires --otelcol-mode=install"
[[ "$PROMETHEUS_BUILD_FROM_SRC" -eq 1 ]] && [[ "$PROMETHEUS_MODE" != "install" ]] \
    && die "--prometheus-build-from-src requires --prometheus-mode=install"
[[ "$GRAFANA_BUILD_FROM_SRC" -eq 1 ]] && [[ "$GRAFANA_MODE" != "install" ]] \
    && die "--grafana-build-from-src requires --grafana-mode=install"
[[ "$HOOKD_MODE" == "external" ]] && { \
    [[ "$OTELCOL_MODE" == "install" || "$PROMETHEUS_MODE" == "install" || "$GRAFANA_MODE" == "install" ]] \
        && die "--hookd-mode=external cannot be combined with any --<service>-mode=install (no local hookd to supervise)"; \
}

# Bug #12 / tmp-relocation: keep Go's per-build tmp + cache out of /tmp
# (often a 2GB tmpfs that the otelcol source build exhausts mid-make).
# Use a basedir-local disposable area instead. Safe to rm -rf afterwards.
export GOTMPDIR="${BASEDIR}/.gotmp"
export GOCACHE="${BASEDIR}/.gocache"
mkdir -p "$GOTMPDIR" "$GOCACHE"

# download_prometheus fetches the prometheus release binary and verifies SHA256.
download_prometheus() {
    local builddir="$1"
    local version="2.51.2"
    # SHA256 of the release tarballs. Update when pinning a new version.
    # TODO(release): pin from actual release artifacts (https://github.com/prometheus/prometheus/releases)
    local sha256_linux_amd64="PLACEHOLDER_UPDATE_AT_PIN_TIME"
    local sha256_linux_arm64="PLACEHOLDER_UPDATE_AT_PIN_TIME"

    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        *)       echo "ERROR: unsupported arch $arch for prometheus download"; exit 1 ;;
    esac
    local os_name
    os_name="$(uname -s | tr '[:upper:]' '[:lower:]')"
    local tarball="prometheus-${version}.${os_name}-${arch}.tar.gz"
    local url="https://github.com/prometheus/prometheus/releases/download/v${version}/${tarball}"

    echo ""
    echo "--- Downloading prometheus v${version} ---"
    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN

    curl -fsSL --retry 3 -o "$tmp/$tarball" "$url"

    # SHA256 verify — skipped when placeholder is still in place.
    local _expected_sha=""
    case "$arch" in
        amd64) _expected_sha="$sha256_linux_amd64" ;;
        arm64) _expected_sha="$sha256_linux_arm64" ;;
    esac
    if [[ -n "$_expected_sha" && "$_expected_sha" != "PLACEHOLDER_UPDATE_AT_PIN_TIME" ]]; then
        echo "${_expected_sha}  $tmp/$tarball" | sha256sum --check --status \
            || { echo "ERROR: prometheus SHA256 mismatch" >&2; exit 1; }
    else
        echo "  WARNING: SHA256 not pinned for prometheus ${os_name}/${arch}; skipping integrity check"
    fi

    tar -xzf "$tmp/$tarball" -C "$tmp" --strip-components=1 "prometheus-${version}.${os_name}-${arch}/prometheus"
    cp "$tmp/prometheus" "$builddir/prometheus"
    chmod 0755 "$builddir/prometheus"
    echo "  prometheus v${version} staged to $builddir/prometheus"
}

# install_otelcol downloads or builds otelcol-contrib and stages it to builddir.
# --build-otelcol builds from source; default downloads the pinned release.
install_otelcol() {
    local builddir="$1"
    # Bumped from 0.95.0 to 0.156.0 (2026-07-07, otelcol-temporality-bump WU):
    # 0.95.0 has no delta-to-cumulative processor, so Codex's OTLP metrics
    # (all delta temporality — live-verified) were silently dropped by
    # prometheusremotewrite ("invalid temporality and type combination").
    # deltatocumulativeprocessor is confirmed present in this version's
    # manifest. Changelog reviewed for the 8 components this template uses
    # (otlp receiver, memory_limiter/batch/transform/resource/deltatocumulative
    # processors, debug/prometheusremotewrite exporters) across the whole
    # 0.95.0->0.156.0 span: no schema-breaking changes found for any of them.
    # prometheusremotewrite's "Remote Write 2.0" changelog entries all concern
    # an opt-in feature gate (exporter.prometheusremotewritexporter.enableSendingRW2)
    # that defaults off and remains flagged not-production-ready upstream — our
    # config never sets `protobuf_message`, so we stay on Remote Write 1.0
    # (`prometheus.WriteRequest`, still the default), compatible with the
    # bundled Prometheus 2.51.2 above. Full write-up + dual-runtime boot/traffic
    # proof: teamster-codex-kit research/evidence-round3/otelcol-temporality-bump/.
    local version="0.156.0"
    # SHA256 of the release tarballs, computed locally from the artifacts
    # downloaded over HTTPS, then cross-checked against the project's own
    # published (cosign-signed) checksums.txt asset on this release — both
    # match exactly. Resolves the prior PLACEHOLDER/TODO(release) pin.
    # https://github.com/open-telemetry/opentelemetry-collector-releases/releases/tag/v0.156.0
    # https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.156.0/opentelemetry-collector-releases_otelcol-contrib_checksums.txt
    # A stronger supply-chain option for later: verify that checksums.txt's
    # cosign signature (the accompanying .sig/.pem) instead of trusting a
    # self-downloaded tarball — not done here, flagged for a future pass.
    local sha256_amd64="ee70d7b1221be8a9cc4700f48bf985c04b1ab8aaeef24409fe79623849e2f9f2"
    local sha256_arm64="1f9afe1d245b4babbb4bcb7d6b57ba2836b3b23c5f61b38abc00ab461f049288"

    if [[ "$OTELCOL_BUILD_FROM_SRC" -eq 1 ]]; then
        echo ""
        echo "--- Building otelcol-contrib v${version} from source ---"
        if ! command -v go &>/dev/null; then
            echo "ERROR: --build-otelcol requires go in PATH"; exit 1
        fi
        local tmp
        tmp="$(mktemp -d)"
        trap 'rm -rf "$tmp"' RETURN
        git clone --depth 1 --branch "v${version}" \
            https://github.com/open-telemetry/opentelemetry-collector-contrib "$tmp/src"
        cd "$tmp/src"
        make otelcontribcol
        cp "$tmp/src/bin/otelcontribcol_linux_amd64" "$builddir/otelcol-contrib" \
            || cp "$tmp/src/otelcontribcol" "$builddir/otelcol-contrib"
        chmod 0755 "$builddir/otelcol-contrib"
        echo "  otelcol-contrib built and staged to $builddir/otelcol-contrib"
        return
    fi

    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        *)       echo "ERROR: unsupported arch $arch for otelcol download"; exit 1 ;;
    esac
    local os_name
    os_name="$(uname -s | tr '[:upper:]' '[:lower:]')"
    local tarball="otelcol-contrib_${version}_${os_name}_${arch}.tar.gz"
    local url="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${version}/${tarball}"

    echo ""
    echo "--- Downloading otelcol-contrib v${version} ---"
    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN

    curl -fsSL --retry 3 -o "$tmp/$tarball" "$url"

    # SHA256 verify — skipped when placeholder is still in place.
    local _expected_sha=""
    case "$arch" in
        amd64) _expected_sha="$sha256_amd64" ;;
        arm64) _expected_sha="$sha256_arm64" ;;
    esac
    if [[ -n "$_expected_sha" && "$_expected_sha" != "PLACEHOLDER_UPDATE_AT_PIN_TIME" ]]; then
        echo "${_expected_sha}  $tmp/$tarball" | sha256sum --check --status \
            || { echo "ERROR: otelcol-contrib SHA256 mismatch" >&2; exit 1; }
    else
        echo "  WARNING: SHA256 not pinned for otelcol-contrib ${os_name}/${arch}; skipping integrity check"
    fi

    tar -xzf "$tmp/$tarball" -C "$tmp" "otelcol-contrib"
    cp "$tmp/otelcol-contrib" "$builddir/otelcol-contrib"
    chmod 0755 "$builddir/otelcol-contrib"
    echo "  otelcol-contrib v${version} staged to $builddir/otelcol-contrib"
}

# install_mysql installs MySQL/MariaDB and creates the teamster database and user.
install_mysql() {
    local dsn="$1"

    if command -v mysql &>/dev/null && (systemctl is-active --quiet mysql 2>/dev/null || systemctl is-active --quiet mysqld 2>/dev/null || systemctl is-active --quiet mariadb 2>/dev/null); then
        echo "  MySQL already installed and running — skipping package install"
    else
        echo ""
        printf -- "${C_BOLD_CYAN}--- Installing MySQL ---${C_RESET}\n"
        sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y default-mysql-server
        sudo systemctl start mysql 2>/dev/null || sudo systemctl start mysqld 2>/dev/null || sudo systemctl start mariadb 2>/dev/null
    fi

    # Parse credentials from DSN: mysql://user:pass@host:port/dbname
    local _user _pass _db
    _user=$(echo "$dsn" | sed -n 's|mysql://\([^:]*\):.*|\1|p')
    _pass=$(echo "$dsn" | sed -n 's|mysql://[^:]*:\([^@]*\)@.*|\1|p')
    _db=$(echo "$dsn" | sed -n 's|mysql://[^/]*/\(.*\)|\1|p')

    if [[ -z "$_user" || -z "$_pass" || -z "$_db" ]]; then
        echo "  WARN: could not parse MySQL credentials from DSN — skipping DB setup"
        return
    fi

    # Create database and user (idempotent)
    sudo mysql -e "CREATE DATABASE IF NOT EXISTS \`$_db\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;" 2>/dev/null \
        || sudo mysql -e "CREATE DATABASE IF NOT EXISTS \`$_db\` CHARACTER SET utf8mb4;" 2>/dev/null

    # CREATE USER IF NOT EXISTS (idempotent across MySQL/MariaDB), falling back to
    # bare CREATE USER on engines without IF NOT EXISTS. SQL is piped via stdin,
    # not -e, so the password is never on argv (ps-visible).
    if ! printf "CREATE USER IF NOT EXISTS '%s'@'localhost' IDENTIFIED BY '%s';\n" "$_user" "$_pass" \
        | sudo mysql 2>/dev/null; then
        printf "CREATE USER '%s'@'localhost' IDENTIFIED BY '%s';\n" "$_user" "$_pass" \
            | sudo mysql 2>/dev/null
    fi
    sudo mysql -e "GRANT ALL PRIVILEGES ON \`$_db\`.* TO '$_user'@'localhost'; FLUSH PRIVILEGES;" 2>/dev/null

    # The backup config lists databases beyond the DSN database (e.g.
    # claude_telemetry). Grant the minimum privileges mysqldump needs so
    # pre-upgrade and scheduled backups don't fail with access-denied.
    for _extra_db in claude_telemetry; do
        if [[ "$_extra_db" != "$_db" ]]; then
            sudo mysql -e "GRANT SELECT, LOCK TABLES, SHOW VIEW, TRIGGER ON \`$_extra_db\`.* TO '$_user'@'localhost';" 2>/dev/null || true
        fi
    done

    # Verify. MYSQL_PWD keeps the password out of argv (ps-visible); same pattern
    # as skel/lib/scripts/wms-smoketest.sh.
    if MYSQL_PWD="$_pass" mysql -u "$_user" "$_db" -e "SELECT 1" >/dev/null 2>&1; then
        printf -- "${C_GREEN}  MySQL ready: database '%s', user '%s'${C_RESET}\n" "$_db" "$_user"
    else
        printf -- "${C_YELLOW}  WARN: MySQL setup may have issues — verify connectivity manually${C_RESET}\n"
    fi

    # When relay-mode=install, the hub must accept remote MySQL connections for
    # binlog replication (default bind-address is 127.0.0.1, so the replica's
    # CHANGE MASTER TO fails). Configure bind-address + binlog and create a
    # replication user. server-id + log-bin also satisfy the binlog prerequisite
    # on fresh installs. Debian/Ubuntu MariaDB conf path; RHEL would be /etc/my.cnf.d/.
    if [[ "${RELAY_MODE:-none}" == "install" ]]; then
        local _my_cnf="/etc/mysql/mariadb.conf.d/99-teamster-replication.cnf"
        if [[ ! -f "$_my_cnf" ]]; then
            sudo tee "$_my_cnf" >/dev/null <<'MYCNF'
[mysqld]
bind-address = 0.0.0.0
server-id = 1
log-bin = mysql-bin
MYCNF
            echo "  Configured MariaDB for replication (bind-address=0.0.0.0, log-bin)"
            dlog INFO install.mysql "replication config written" "cnf=$_my_cnf"
            sudo systemctl restart mariadb 2>/dev/null || sudo systemctl restart mysql 2>/dev/null || sudo systemctl restart mysqld 2>/dev/null
            echo "  Restarted MariaDB to apply replication config"
        fi

        local _repl_user="repl"
        local _repl_pass
        _repl_pass=$(openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
        # Password piped via stdin, not -e, so it is never on argv (ps-visible).
        printf "CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s';\n" "${_repl_user}" "${_repl_pass}" \
            | sudo mysql 2>/dev/null
        sudo mysql -e "GRANT REPLICATION SLAVE ON *.* TO '${_repl_user}'@'%'; FLUSH PRIVILEGES;" 2>/dev/null
        printf -- "${C_GREEN}  Replication user '%s' created (password in install log)${C_RESET}\n" "$_repl_user"
        dlog INFO install.mysql "replication user created" "user=$_repl_user"

        local _repl_creds="$BASEDIR/etc/repl-credentials.env"
        mkdir -p "$BASEDIR/etc"  # install_mysql runs before teamster-install creates etc/
        printf 'REPL_USER=%s\nREPL_PASSWORD=%s\n' "$_repl_user" "$_repl_pass" > "$_repl_creds"
        chmod 600 "$_repl_creds"
        echo "  Replication credentials saved to: $_repl_creds"
        dlog INFO install.mysql "replication credentials saved" "file=$_repl_creds"
    fi
}

# provision_grafana_ro creates the least-privilege read-only MySQL user the
# Grafana "Teamster MySQL" datasource connects as (grafana_ro). The privileged
# CREATE USER + GRANT must run as a DB admin: under store-mode=managed the
# StoreDSN is the app account, which lacks CREATE USER, so the supervisor can
# never do this from grafana.go. We use socket-root `sudo mysql` here — the same
# mechanism install_mysql uses for the app account — which always has the
# privilege on a locally-installed MySQL.
#
# Password ownership lives HERE: we generate it once, persist it 0600 to
# <basedir>/var/grafana/grafana_ro_password, and the supervisor only READS it to
# render the datasource. Idempotent: reuses an existing password file, and the
# SQL is CREATE USER IF NOT EXISTS + ALTER USER + additive GRANTs.
#
# No-silent-failure: if MySQL is not locally socket-reachable (e.g. a managed DB
# on another host) or the GRANT fails, we DO NOT silently skip — we print an
# actionable manual-step message. Args: $1=store DSN, $2=basedir.
provision_grafana_ro() {
    local dsn="$1" basedir="$2"
    local _db _ro_user="grafana_ro"
    _db=$(echo "$dsn" | sed -n 's|mysql://[^/]*/\([^?]*\).*|\1|p')

    local sql_tmpl="$basedir/etc/grafana/grafana-readonly-user.sql"
    local pw_dir="$basedir/var/grafana"
    local pw_file="$pw_dir/grafana_ro_password"

    # The password file is the marker `teamster status` reads. We clear it ONLY
    # on a CONFIRMED failure — the SQL ran and the GRANT errored (the user is
    # known-bad) — via _ro_fail() below. The "couldn't even attempt" early bails
    # (unparseable DSN, missing SQL, MySQL not locally administrable) do NOT clear
    # it: they don't disprove a grafana_ro provisioned by an earlier successful
    # run, and wiping a valid marker on a transient blip would falsely report
    # "Not provisioned" AND strip the password the supervisor's datasource reads.
    _ro_fail() {
        rm -f "$pw_file"
        printf -- "${C_YELLOW}    grafana_ro provisioning FAILED — D1-D4 MySQL panels will be unauthorized.${C_RESET}\n"
        printf -- "${C_YELLOW}    Apply manually as a DB admin: %s (substitute placeholders).${C_RESET}\n" "$sql_tmpl"
        dlog WARN install.grafana-ro "grant failed — cleared stale password marker" "db=$_db"
    }

    if [[ -z "$_db" ]]; then
        printf -- "${C_YELLOW}    WARN: could not parse store DB from DSN — skipping grafana_ro provisioning${C_RESET}\n"
        dlog WARN install.grafana-ro "no db parsed from dsn"
        return 1
    fi
    if [[ ! -f "$sql_tmpl" ]]; then
        printf -- "${C_YELLOW}    WARN: %s not found — skipping grafana_ro provisioning${C_RESET}\n" "$sql_tmpl"
        dlog WARN install.grafana-ro "sql template missing" "path=$sql_tmpl"
        return 1
    fi

    # Require a locally socket-reachable MySQL we can administer. `sudo mysql`
    # uses unix_socket root auth; if it can't run, this is a managed/remote DB we
    # cannot provision — surface the manual step rather than failing silently.
    if ! sudo mysql -e "SELECT 1" >/dev/null 2>&1; then
        printf -- "${C_YELLOW}    grafana_ro not provisioned — MySQL is not locally administrable from this host.${C_RESET}\n"
        printf -- "${C_YELLOW}    D1-D4 MySQL panels will be unauthorized until you apply, as a DB admin:${C_RESET}\n"
        printf -- "${C_YELLOW}      %s  (substitute grafana_ro user/password/db placeholders first)${C_RESET}\n" "$sql_tmpl"
        dlog WARN install.grafana-ro "mysql not locally administrable — manual step required" "db=$_db"
        return 1
    fi

    # Compute the grafana_ro password (reuse the persisted one if present so the
    # datasource the supervisor already rendered keeps working). The file is the
    # success marker — it is (re)written only AFTER the GRANT succeeds, so its
    # presence means grafana_ro was actually provisioned, not merely attempted.
    local _pw
    if [[ -s "$pw_file" ]]; then
        _pw=$(tr -d '\n' < "$pw_file")
    else
        _pw=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
    fi

    # Render placeholders and apply via socket-root. Pipe via stdin so the
    # password never lands in argv (ps/proc) or a rendered on-disk file. Escape
    # the password for the sed replacement side so a non-hex generator can't
    # corrupt the SQL (sed_escape_replacement is a no-op on today's hex).
    local _pw_esc
    _pw_esc=$(sed_escape_replacement "$_pw")
    if sed -e "s|__GRAFANA_DB_USER__|$_ro_user|g" \
           -e "s|__GRAFANA_DB_PASSWORD__|$_pw_esc|g" \
           -e "s|__STORE_DB__|$_db|g" "$sql_tmpl" \
       | sudo mysql >/dev/null 2>&1; then
        mkdir -p "$pw_dir"
        printf '%s' "$_pw" > "$pw_file"
        chmod 600 "$pw_file"
        printf -- "${C_GREEN}    grafana_ro read-only DB user provisioned (db '%s')${C_RESET}\n" "$_db"
        dlog INFO install.grafana-ro "provisioned" "user=$_ro_user" "db=$_db"
        return 0
    else
        _ro_fail
        return 1
    fi
}

# install_grafana downloads grafana-server and stages it to builddir.
# --build-grafana is accepted as a flag but Grafana's build chain is heavy;
# the default always downloads the pinned OSS release.
install_grafana() {
    local builddir="$1"
    local version="13.0.2"
    # v12+ tarballs use grafana-${version}/ (no "v" prefix); v10 used grafana-v${version}/.
    local inner_dir="grafana-${version}"

    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        *)       echo "ERROR: unsupported arch $arch for grafana download"; exit 1 ;;
    esac
    local tarball="grafana-${version}.linux-${arch}.tar.gz"
    local url="https://dl.grafana.com/oss/release/${tarball}"

    echo ""
    echo "--- Downloading Grafana v${version} ---"
    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN

    curl -fsSL --retry 3 -o "$tmp/$tarball" "$url"
    # Grafana requires its full asset tree (public/, conf/) at --homepath.
    # Extract the entire distribution; the installer will copy it to BASEDIR/var/grafana-home/.
    tar -xzf "$tmp/$tarball" -C "$tmp"
    # Stage binaries to builddir for teamster-install's binary copy step.
    cp "$tmp/${inner_dir}/bin/grafana" "$builddir/grafana"
    chmod 0755 "$builddir/grafana"
    # Grafana 13+ ships only bin/grafana (unified binary); grafana-server was
    # removed. Stage it only if present — teamster-install skips missing optionals.
    if [[ -f "$tmp/${inner_dir}/bin/grafana-server" ]]; then
        cp "$tmp/${inner_dir}/bin/grafana-server" "$builddir/grafana-server"
        chmod 0755 "$builddir/grafana-server"
    fi
    # Stage the full distribution tree to builddir/grafana-home/ so teamster-install
    # can copy it to BASEDIR/var/grafana-home/ (the --homepath value).
    rm -rf "$builddir/grafana-home"
    cp -a "$tmp/${inner_dir}" "$builddir/grafana-home"
    echo "  grafana v${version} staged to $builddir/grafana{,-server} + $builddir/grafana-home/"

    install_grafana_plugins "$builddir"

    # Copy custom fonts (e.g. Glass_TTY_VT220 for the Activity Feed panel)
    # from skel into the staged grafana-home's public/fonts/ dir.
    local fonts_src="$REPO/skel/etc/grafana/fonts"
    local fonts_dst="$builddir/grafana-home/public/fonts"
    if [[ -d "$fonts_src" ]]; then
        cp "$fonts_src"/*.ttf "$fonts_dst/" 2>/dev/null || true
        echo "  custom fonts staged to $fonts_dst/"
    fi
}

# install_grafana_plugins downloads the panel plugins Teamster's dashboards
# depend on and stages them to builddir/grafana-plugins/, from which
# teamster-install copies them into BASEDIR/var/grafana/plugins/ (the
# grafana.ini `plugins` dir). Only runs for grafana-mode=install — the Teamster-
# managed Grafana. external/managed Grafana is BYO and the operator owns its
# plugin set; we never touch a shared instance's plugins (see 176c562).
#
# volkovlabs-echarts-panel (Business Charts) backs the Entity Cost Treemap —
# core Grafana has no treemap. Pinned so the panel's pluginVersion is stable
# across every fresh install. The plugin ships a PGP-signed MANIFEST.txt, so
# Grafana loads it without allow_loading_unsigned_plugins.
#
# yesoreyeram-infinity-datasource (Infinity) backs the Activity Feed panel —
# it queries hookd's /api/events JSON endpoint.
#
# Teamster is a single-binary offline-ish distribution, but plugins (like the
# grafana-server tarball above) require network at install time. If the download
# fails we abort loudly rather than ship a Grafana that silently can't render the
# treemap — honoring no-silent-failures.
install_grafana_plugins() {
    local builddir="$1"
    local plugins_dir="$builddir/grafana-plugins"
    # id:version pairs. Add a line per plugin dependency.
    local plugins=(
        "volkovlabs-echarts-panel:7.2.2"
        "yesoreyeram-infinity-datasource:3.8.0"
        "grafana-pathfinder-app:2.12.1"
    )

    rm -rf "$plugins_dir"
    mkdir -p "$plugins_dir"

    local entry id ver url tmp
    for entry in "${plugins[@]}"; do
        id="${entry%%:*}"
        ver="${entry##*:}"
        url="https://grafana.com/api/plugins/${id}/versions/${ver}/download?os=any"
        echo "--- Downloading Grafana plugin ${id} v${ver} ---"
        tmp="$(mktemp -d)"
        if ! curl -fsSL --retry 3 -o "$tmp/plugin.zip" "$url"; then
            rm -rf "$tmp"
            die "failed to download Grafana plugin ${id} v${ver} from ${url} — network required at install time for grafana-mode=install; the Entity Cost Treemap panel cannot render without it"
        fi
        if ! unzip -q -o "$tmp/plugin.zip" -d "$plugins_dir"; then
            rm -rf "$tmp"
            die "failed to extract Grafana plugin ${id} v${ver}"
        fi
        rm -rf "$tmp"
        if [[ ! -f "$plugins_dir/${id}/plugin.json" ]]; then
            die "Grafana plugin ${id} v${ver} extracted without ${id}/plugin.json — unexpected archive layout"
        fi
        echo "  plugin ${id} v${ver} staged to $plugins_dir/${id}/"
    done
}

# install_go downloads the latest stable Go release and installs to /usr/local/go.
install_go() {
    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        *)       die "Unsupported architecture for Go install: $arch" ;;
    esac
    local os_name
    os_name="$(uname -s | tr '[:upper:]' '[:lower:]')"

    echo "  Querying go.dev for latest stable version..."
    local go_ver
    go_ver="$(curl -fsSL --retry 3 'https://go.dev/VERSION?m=text' | head -1)" \
        || die "Could not determine latest Go version from go.dev"
    [[ -n "$go_ver" ]] || die "Empty response from go.dev/VERSION"

    local ver="${go_ver#go}"
    local url="https://go.dev/dl/${go_ver}.${os_name}-${arch}.tar.gz"
    echo "  Downloading Go ${ver} from ${url} ..."
    dlog INFO install.go "downloading" "version=$ver" "url=$url"

    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN

    curl -fsSL --retry 3 -o "$tmp/go.tar.gz" "$url" \
        || die "Failed to download Go from $url"

    echo "  Installing to /usr/local/go ..."
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$tmp/go.tar.gz" \
        || die "Failed to extract Go tarball to /usr/local"

    export PATH="/usr/local/go/bin:$PATH"
    hash -r
    printf -- "  ${C_GREEN}Go %s installed successfully${C_RESET}\n" "$ver"
    dlog INFO install.go "installed" "version=$ver" "path=/usr/local/go"
}

# ============================================================
# Mode detection: --hookd-mode=external → client mode (Python hook client only).
# Default (no --hookd-mode, or systemd/supervisor) → hub mode.
# ============================================================
if [[ "$HOOKD_MODE" == "external" ]]; then
    # Client mode: Python hook client, no Go needed.
    if [[ -z "$HOOKD_ENDPOINT" ]]; then
        die "client-mode install (--hookd-mode=external) requires --hookd-endpoint=URL"
    fi
    printf -- "${C_BOLD_CYAN}=== Teamster Client Installer ===${C_RESET}\n"
    echo "  repo:   $REPO"
    echo "  target: $BASEDIR"
    echo "  hub:    $HOOKD_ENDPOINT"
    echo ""

    MISSING=""
    if ! command -v python3 &>/dev/null; then
        MISSING="$MISSING python3"
    fi
    if ! command -v claude &>/dev/null; then
        MISSING="$MISSING claude"
        echo ""
        echo "Claude Code not found. Install it first:"
        echo "  curl -fsSL https://claude.ai/install.sh | bash"
    fi
    if [[ -n "$MISSING" ]]; then
        echo "ERROR: Missing prerequisites:$MISSING"
        exit 1
    fi
    echo "Prerequisites OK: python3 $(python3 --version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+'), claude $(claude --version 2>/dev/null | head -1)"
    echo ""

    echo "--- Staging ---"
    mkdir -p "$BASEDIR/bin" "$BASEDIR/lib" "$BASEDIR/lib/scripts"

    cp "$REPO/skel/lib/hook/teamster.py" "$BASEDIR/bin/teamster"
    chmod +x "$BASEDIR/bin/teamster"
    cp "$REPO/skel/lib/scripts/token-scraper.py" "$BASEDIR/bin/token-scraper"
    chmod +x "$BASEDIR/bin/token-scraper"

    cp -r "$REPO/skel/lib/plugin"         "$BASEDIR/lib/"
    cp -r "$REPO/skel/lib/.claude-plugin"  "$BASEDIR/lib/"

    cp "$REPO/skel/lib/scripts/remote-setup.sh" "$BASEDIR/lib/scripts/remote-setup.sh"
    chmod +x "$BASEDIR/lib/scripts/remote-setup.sh"

    # Stamp VERSION so the Python hook client reports the correct version
    TEAMSTER_VERSION="$(cd "$REPO" && git describe --tags --dirty 2>/dev/null || cat "$REPO/VERSION" 2>/dev/null || echo dev)"
    printf '%s\n' "$TEAMSTER_VERSION" > "$BASEDIR/VERSION"
    # Clean stale skill directories from prior installs
    rm -rf "$BASEDIR/lib/plugin/skills/init" "$BASEDIR/lib/plugin/skills/muster" 2>/dev/null || true

    echo "  staged to $BASEDIR"

    echo ""
    echo "--- Configuring ---"
    bash "$BASEDIR/lib/scripts/remote-setup.sh" --server "$HOOKD_ENDPOINT"

    echo ""
    echo "--- Verifying hook path ---"
    SHORT_HOST="$(hostname -s 2>/dev/null || hostname)"
    echo '{"hook_event_name":"install-verify","session_id":"local-install"}' \
        | TEAMSTER_HOOK_SERVER_URL="http://$HOOKD_ENDPOINT/event" \
          TEAMSTER_HOST="$SHORT_HOST" \
          "$BASEDIR/bin/teamster"
    echo "  Verify on hub: feed | grep install-verify"

    echo ""
    printf -- "${C_BOLD_CYAN}=== Done ===${C_RESET}\n"
    echo "  Claude Code on this host is now wired to hub: $HOOKD_ENDPOINT"
    echo "  Restart Claude Code for settings.json changes to take effect."
    exit 0
fi

# ============================================================
# HUB MODE (existing behavior — unchanged)
# ============================================================
printf -- "${C_BOLD_CYAN}=== Teamster Hub Installer ===${C_RESET}\n"
echo "  repo:    $REPO"
echo "  target:  $BASEDIR"
echo ""

# Step 0: Install apt-installable pre-reqs if missing.
# claude is excluded — not an apt package; install instructions below.
# Detection probes the BINARY name (what command -v sees); installation uses
# the APT PACKAGE name. They differ for golang-go (binary is `go`) — without
# this split, command -v golang-go always failed and apt was invoked every
# run (a no-op, but misleading: "Pre-reqs installed: golang-go" on every run).
APT_PREREQS=(curl tar unzip git make golang-go)
declare -A APT_PKG_BIN=([golang-go]=go)
APT_MISSING=()
for _pkg in "${APT_PREREQS[@]}"; do
    _bin="${APT_PKG_BIN[$_pkg]:-$_pkg}"
    command -v "$_bin" &>/dev/null || APT_MISSING+=("$_pkg")
done
if [[ "${#APT_MISSING[@]}" -gt 0 ]]; then
    echo "Installing missing pre-reqs: ${APT_MISSING[*]}"
    dlog INFO install.apt "missing pre-reqs" "pkgs=${APT_MISSING[*]}"
    if sudo apt-get update -qq; then
        dlog INFO install.apt "apt-get update" "rc=0"
    else
        _rc=$?
        dlog ERROR install.apt "apt-get update failed" "rc=$_rc"
        die "apt-get update failed; install manually: ${APT_MISSING[*]}"
    fi
    if sudo apt-get install -y "${APT_MISSING[@]}"; then
        dlog INFO install.apt "apt-get install" "rc=0" "pkgs=${APT_MISSING[*]}"
    else
        _rc=$?
        dlog ERROR install.apt "apt-get install failed" "rc=$_rc" "pkgs=${APT_MISSING[*]}"
        die "apt-get install failed; install manually: ${APT_MISSING[*]}"
    fi
    echo "  Pre-reqs installed: ${APT_MISSING[*]}"
else
    echo "All apt pre-reqs satisfied (curl, tar, git, make, golang-go)"
    dlog INFO install.apt "all pre-reqs satisfied" "checked=${APT_PREREQS[*]}"
fi
unset _pkg _bin _rc

# Step 0b: Monitoring-stack prereqs (Node.js, ccusage, prometheus_client).
# token-scraper always installs; these prereqs are always needed on hub installs.
if true; then
    _NODE_MISSING=()
    command -v node  &>/dev/null || _NODE_MISSING+=(nodejs)
    command -v npm   &>/dev/null || _NODE_MISSING+=(npm)
    if [[ "${#_NODE_MISSING[@]}" -gt 0 ]]; then
        echo "Installing monitoring pre-reqs: ${_NODE_MISSING[*]}"
        dlog INFO install.apt "monitoring pre-reqs missing" "pkgs=${_NODE_MISSING[*]}"
        sudo apt-get update -qq || die "apt-get update failed"
        sudo apt-get install -y "${_NODE_MISSING[@]}" \
            || die "apt-get install failed for: ${_NODE_MISSING[*]}"
        dlog INFO install.apt "monitoring pre-reqs installed" "pkgs=${_NODE_MISSING[*]}"
    fi
    unset _NODE_MISSING
    if ! python3 -c "import prometheus_client" 2>/dev/null; then
        if apt-cache show python3-prometheus-client &>/dev/null; then
            echo "Installing prometheus_client via apt..."
            dlog INFO install.apt "prometheus_client via apt"
            sudo apt-get install -y python3-prometheus-client \
                || die "apt-get install python3-prometheus-client failed"
        elif command -v pip3 &>/dev/null; then
            echo "Installing prometheus_client via pip3..."
            dlog INFO install.apt "prometheus_client via pip3"
            pip3 install --quiet prometheus_client --break-system-packages 2>/dev/null \
                || pip3 install --quiet prometheus_client \
                || die "pip3 install prometheus_client failed"
        else
            die "Cannot install prometheus_client: no apt package and no pip3"
        fi
    fi
    echo "Installing ccusage (npm -g)..."
    dlog INFO install.apt "npm install -g ccusage"
    sudo npm install -g ccusage \
        || die "npm install -g ccusage failed"
    sudo find /usr/local/lib/node_modules/ccusage -name ccusage -path "*/bin/*" -exec chmod +x {} \; 2>/dev/null || true
    echo "  ccusage + prometheus_client installed"
fi

# Check prerequisites
MISSING=""

GO_NEED_INSTALL=0
if ! command -v go &>/dev/null; then
    printf -- "${C_YELLOW}Go not found.${C_RESET}\n"
    GO_NEED_INSTALL=1
elif go_ver_str="$(go version | grep -oP '\d+\.\d+' | head -1)"; then
    GO_CUR_MAJOR=$(echo "$go_ver_str" | cut -d. -f1)
    GO_CUR_MINOR=$(echo "$go_ver_str" | cut -d. -f2)
    if [ "$GO_CUR_MAJOR" -lt 1 ] || ([ "$GO_CUR_MAJOR" -eq 1 ] && [ "$GO_CUR_MINOR" -lt "$GO_MIN_MINOR" ]); then
        printf -- "${C_YELLOW}Go 1.%d+ required, found %s.${C_RESET}\n" "$GO_MIN_MINOR" "$go_ver_str"
        GO_NEED_INSTALL=1
    fi
fi

if command -v go &>/dev/null; then
    GO_VER=$(go version | grep -oP '\d+\.\d+' | head -1)
    GO_MAJOR=$(echo "$GO_VER" | cut -d. -f1)
    GO_MINOR=$(echo "$GO_VER" | cut -d. -f2)
    if [ "$GO_MAJOR" -lt 1 ] || ([ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -lt "$GO_MIN_MINOR" ]); then
        printf -- "${C_BOLD_RED}ERROR: Go 1.%d+ required, found %s${C_RESET}\n" "$GO_MIN_MINOR" "$GO_VER"
        echo "  Install from: https://go.dev/dl/"
        exit 1
    fi
fi

if ! command -v curl &>/dev/null; then
    MISSING="$MISSING curl"
fi

if ! command -v claude &>/dev/null; then
    MISSING="$MISSING claude"
    echo ""
    echo "Claude Code not found. Install it first:"
    echo "  curl -fsSL https://claude.ai/install.sh | bash"
fi

if [ -n "$MISSING" ]; then
    printf -- "${C_BOLD_RED}ERROR: Missing prerequisites:%s${C_RESET}\n" "$MISSING"
    exit 1
fi

printf -- "${C_GREEN}Prerequisites OK: go %s, claude %s${C_RESET}\n" "$(go version | grep -oP '\d+\.\d+\.\d+')" "$(claude --version 2>/dev/null | head -1)"
echo ""

# Step 0.5: Pre-upgrade backup (safety net). If an existing install is present,
# snapshot all stores to $BASEDIR/backups before touching anything. Uses the
# existing binary so the backup matches the schema of the running install.
# Best-effort: failure warns but never aborts the upgrade.
if [[ -x "$BASEDIR/bin/teamster" ]] && [[ -f "$BASEDIR/etc/teamster.yaml" ]]; then
    # Heal backup grants: older installs only granted on the DSN database,
    # but the backup config may list additional databases. Apply the missing
    # grants now so the pre-upgrade backup below doesn't fail with access-denied.
    if [[ -n "${STORE_DSN:-}" ]] && sudo mysql -e "SELECT 1" >/dev/null 2>&1; then
        _bu_user=$(echo "$STORE_DSN" | sed -n 's|mysql://\([^:]*\):.*|\1|p')
        _bu_db=$(echo "$STORE_DSN" | sed -n 's|mysql://[^/]*/\(.*\)|\1|p')
        if [[ -n "$_bu_user" ]]; then
            for _extra_db in claude_telemetry; do
                if [[ "$_extra_db" != "${_bu_db:-}" ]]; then
                    sudo mysql -e "GRANT SELECT, LOCK TABLES, SHOW VIEW, TRIGGER ON \`$_extra_db\`.* TO '$_bu_user'@'localhost';" 2>/dev/null || true
                fi
            done
        fi
        unset _bu_user _bu_db _extra_db
    fi

    echo ""
    printf -- "${C_BOLD_CYAN}--- Pre-upgrade backup ---${C_RESET}\n"
    dlog INFO install.backup "starting pre-upgrade backup"
    _pre_backup_dir="$BASEDIR/backups"
    mkdir -p "$_pre_backup_dir"

    _tmp_cfg=$(mktemp --suffix=.yaml)
    cp "$BASEDIR/etc/teamster.yaml" "$_tmp_cfg"
    if grep -q 'backup_dir:' "$_tmp_cfg"; then
        sed -i "s|backup_dir:.*|backup_dir: $_pre_backup_dir|" "$_tmp_cfg"
    elif grep -q '^backup:' "$_tmp_cfg"; then
        sed -i "/^backup:/a\\  backup_dir: $_pre_backup_dir" "$_tmp_cfg"
    else
        printf '\nbackup:\n  backup_dir: %s\n' "$_pre_backup_dir" >> "$_tmp_cfg"
    fi

    if "$BASEDIR/bin/teamster" backup --config="$_tmp_cfg" 2>&1; then
        printf -- "${C_GREEN}  Pre-upgrade backup saved to %s${C_RESET}\n" "$_pre_backup_dir"
        dlog INFO install.backup "pre-upgrade backup complete" "dir=$_pre_backup_dir"
    else
        printf -- "${C_YELLOW}  WARN: pre-upgrade backup failed — proceeding with upgrade${C_RESET}\n"
        dlog WARN install.backup "pre-upgrade backup failed — proceeding"
    fi
    rm -f "$_tmp_cfg"
    unset _pre_backup_dir _tmp_cfg
fi

# Step 1: Detect and stop running hookd
RUNNING_MODE="none"
RESTORE_HOOKD=0  # set to 1 once we stop hookd; cleared after successful restart.
                 # If install.sh exits abnormally between stop and restart, the
                 # EXIT trap below revives hookd so the user isn't left dark.
SCRIPT_OK=0      # set to 1 only at the very end of normal flow

restore_hookd_if_needed() {
    local rc=$?
    if [[ "$SCRIPT_OK" -eq 0 ]]; then
        echo ""
        printf -- "${C_BOLD_RED}================================================================\n"
        printf "  INSTALL FAILED (exit %s)\n" "$rc"
        printf "  Check the scrollback above for the error.\n"
        printf "================================================================${C_RESET}\n"
    fi
    [[ "$RESTORE_HOOKD" -eq 1 ]] || return 0
    echo ""
    echo "!!! install.sh exited before restarting hookd — attempting recovery."
    case "$RUNNING_MODE" in
        systemd)
            sudo systemctl start teamster-hookd 2>/dev/null \
                && printf -- "${C_GREEN}    hookd restored via systemd${C_RESET}\n" \
                || printf -- "${C_YELLOW}    WARN: recovery failed — run: sudo systemctl start teamster-hookd${C_RESET}\n"
            ;;
        manual)
            mkdir -p "$BASEDIR/var"
            nohup "$BASEDIR/bin/hookd" > "$BASEDIR/var/hookd.log" 2>&1 &
            disown
            echo "    hookd restored manually (PID $!)"
            ;;
    esac
}
trap restore_hookd_if_needed EXIT

if [[ "$WIRE" -eq 1 ]]; then
    if command -v systemctl &>/dev/null && systemctl is-active --quiet teamster-hookd 2>/dev/null; then
        RUNNING_MODE="systemd"
        printf -- "${C_BOLD_WHITE}--> Stopping teamster-hookd via systemd...${C_RESET}\n"
        dlog INFO install.hookd "stop" "mode=systemd"
        sudo systemctl stop teamster-hookd
        dlog INFO install.hookd "stopped" "mode=systemd" "rc=$?"
        RESTORE_HOOKD=1
    elif HOOKD_PIDS=$(pgrep -f 'teamster/bin/hookd' 2>/dev/null); then
        RUNNING_MODE="manual"
        printf -- "${C_BOLD_WHITE}--> Stopping hookd (PIDs: %s, manual mode)...${C_RESET}\n" "$HOOKD_PIDS"
        dlog INFO install.hookd "stop" "mode=manual" "pids=$HOOKD_PIDS"
        # shellcheck disable=SC2086 — word-split intentional to pass multiple PIDs
        kill $HOOKD_PIDS 2>/dev/null || true
        for i in {1..10}; do
            kill -0 $HOOKD_PIDS 2>/dev/null || break
            sleep 1
        done
        kill -9 $HOOKD_PIDS 2>/dev/null || true
        dlog INFO install.hookd "stopped" "mode=manual" "pids=$HOOKD_PIDS"
        RESTORE_HOOKD=1
    else
        dlog INFO install.hookd "no running hookd found"
    fi
fi

# Auto-generate DSN when store-mode=install and no DSN was provided.
# store-mode=install is the "do everything for me" mode — the wizard fills in a
# DSN during the interview, but a direct installrunner invocation may not. Without
# this, install_mysql is skipped and hookd starts with no database. The DSN must
# use the mysql:// URI scheme (hookd's config uses url.Parse, rejecting bare DSNs)
# and 127.0.0.1 (avoids socket-vs-TCP ambiguity on some distros).
if [[ "${STORE_MODE}" == "install" ]] && [[ -z "$STORE_DSN" ]]; then
    _gen_pw=$(openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
    STORE_DSN="mysql://teamster:${_gen_pw}@127.0.0.1:3306/teamster"
    printf -- "${C_GREEN}  Auto-generated MySQL DSN for --store-mode=install${C_RESET}\n"
    dlog INFO install.store "auto-generated DSN" "dsn_user=teamster" "dsn_db=teamster"
fi

# Step 1.5: Install MySQL if store-mode=install
if [[ "${STORE_MODE}" == "install" ]] && [[ -n "$STORE_DSN" ]]; then
    dtrace install.bundle ">>" install_mysql
    install_mysql "$STORE_DSN"
    dtrace install.bundle "<<" install_mysql
fi

# Step 2: Compile
printf -- "${C_BOLD_CYAN}--- Compiling ---${C_RESET}\n"
dlog INFO install.compile "start" "builddir=$BUILDDIR"
mkdir -p "$BUILDDIR"

# Derive the single version source. A tagged tree: git describe decorates the
# nearest tag (e.g. 0.1.0, or 0.1.0-3-gabc123-dirty) and that wins. A tagless
# tree (a fresh public clone): git describe --tags WITHOUT --always returns
# empty, so we fall back to the committed VERSION floor (e.g. 0.1.0), then "dev".
# --always is deliberately omitted: it would emit a bare commit hash on a tagless
# tree, never empty, silently bypassing the VERSION floor. The commit hash is
# stamped separately from git rev-parse below, so dropping --always loses nothing.
# The `|| true` is required: git describe exits 128 ("No names found") on a
# tagless tree, which under `set -euo pipefail` would otherwise abort the install.
# Stamped into internal/version via -ldflags -X so every binary reports the same build.
TEAMSTER_VERSION="$(cd "$REPO" && git describe --tags --dirty 2>/dev/null || true)"
[[ -z "$TEAMSTER_VERSION" ]] && TEAMSTER_VERSION="$(cat "$REPO/VERSION" 2>/dev/null || echo dev)"
TEAMSTER_COMMIT="$(cd "$REPO" && git rev-parse --short HEAD 2>/dev/null || echo none)"
TEAMSTER_BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
_vpkg="github.com/bmjdotnet/teamster/internal/version"
LDFLAGS="-X ${_vpkg}.Version=${TEAMSTER_VERSION} -X ${_vpkg}.Commit=${TEAMSTER_COMMIT} -X ${_vpkg}.BuildTime=${TEAMSTER_BUILD_TIME}"
dlog INFO install.compile "version" "version=$TEAMSTER_VERSION" "commit=$TEAMSTER_COMMIT" "build_time=$TEAMSTER_BUILD_TIME"

cd "$REPO/src"
for _target in teamster hookd feed activity-mcp wms-mcp teamster-install token-scraper codex-scraper rollup classify demogen relay backup; do
    if go build -trimpath -ldflags "$LDFLAGS" -o "$BUILDDIR/$_target" "./cmd/$_target"; then
        dlog INFO install.compile "built" "target=$_target" "rc=0"
    else
        _rc=$?
        dlog ERROR install.compile "build failed" "target=$_target" "rc=$_rc"
        die "go build failed for $_target"
    fi
done
unset _target _rc
printf -- "${C_GREEN}  12 binaries compiled to %s${C_RESET}\n" "$BUILDDIR"

# Step 2.5: Download/build service binaries for install-mode services.
if [[ "${PROMETHEUS_MODE:-install}" == "install" ]]; then
    dtrace install.bundle ">>" download_prometheus
    download_prometheus "$BUILDDIR"
    dtrace install.bundle "<<" download_prometheus
fi
if [[ "${OTELCOL_MODE:-install}" == "install" ]]; then
    dtrace install.bundle ">>" install_otelcol
    install_otelcol "$BUILDDIR"
    dtrace install.bundle "<<" install_otelcol
fi
if [[ "${GRAFANA_MODE:-install}" == "install" ]]; then
    dtrace install.bundle ">>" install_grafana
    install_grafana "$BUILDDIR"
    dtrace install.bundle "<<" install_grafana
fi

# Detect whether the Teamster supervisor (teamster start — owns the managed
# grafana/prometheus/otelcol bundle) is running BEFORE we stop it. On an upgrade
# we must bring the managed bundle back up afterward so a newly-staged grafana
# plugin actually loads (plugins load only at grafana start). We only restart if
# it was running here — never auto-start a supervisor the operator hadn't.
# The supervisor authoritatively writes var/pids/teamster.pid on start; a
# non-empty file naming a live pid is the ground-truth signal. (We avoid a
# pgrep fallback: matching 'teamster start' on a command line incidentally hits
# install.sh's own process and other false positives.)
SUPERVISOR_WAS_RUNNING=0
_sup_pid_file="$BASEDIR/var/pids/teamster.pid"
if [[ -s "$_sup_pid_file" ]] && kill -0 "$(cat "$_sup_pid_file" 2>/dev/null)" 2>/dev/null; then
    SUPERVISOR_WAS_RUNNING=1
fi
unset _sup_pid_file

# Stop existing services before re-install so port scan finds default ports free.
if [[ -x "$BASEDIR/bin/teamster" ]]; then
    echo "Stopping existing services before re-install..."
    dlog INFO install.pre "stopping existing services"
    "$BASEDIR/bin/teamster" stop 2>/dev/null || true
fi
if systemctl is-active --quiet teamster-hookd 2>/dev/null; then
    sudo systemctl stop teamster-hookd 2>/dev/null || true
fi

# Detect and disable legacy pre-Teamster systemd units that conflict with
# Teamster services (port 9125, port 9123). These are from the Python-era
# hook server and will fight for ports if left enabled.
LEGACY_UNITS="claude-hook-server claude-hook-exporter claude-quota-scraper claude-dashboard"
LEGACY_FOUND=""
for _unit in $LEGACY_UNITS; do
    if systemctl list-unit-files "${_unit}.service" 2>/dev/null | grep -q "${_unit}"; then
        if systemctl is-active --quiet "${_unit}" 2>/dev/null; then
            LEGACY_FOUND="${LEGACY_FOUND} ${_unit}"
            sudo systemctl stop "${_unit}" 2>/dev/null || true
        fi
        if systemctl is-enabled --quiet "${_unit}" 2>/dev/null; then
            LEGACY_FOUND="${LEGACY_FOUND:+$LEGACY_FOUND }${_unit}"
            sudo systemctl disable "${_unit}" 2>/dev/null || true
        fi
    fi
done
if [[ -n "$LEGACY_FOUND" ]]; then
    printf -- "${C_YELLOW}--> Disabled legacy units (conflict with Teamster):${C_RESET}\n"
    for _unit in $LEGACY_FOUND; do
        printf -- "${C_YELLOW}    %s.service${C_RESET}\n" "$_unit"
    done
    dlog INFO install.pre "disabled legacy units" "units=$LEGACY_FOUND"
fi
unset _unit LEGACY_FOUND

# Wait for service ports to be released before port scan
if [[ -x "$BASEDIR/bin/teamster" ]]; then
    echo "Waiting for service ports to release..."
    dlog INFO install.pre "waiting for ports to release"
    for port in 9125 9190 3100 4327 4328 9123; do
        for i in $(seq 1 25); do
            if ! ss -tln | grep -q ":${port} "; then
                break
            fi
            if [[ $i -eq 25 ]]; then
                printf -- "${C_YELLOW}WARNING: port %s still bound after 5s${C_RESET}\n" "$port"
                dlog WARN install.pre "port still bound after 5s" "port=$port"
                sudo fuser -k "${port}/tcp" 2>/dev/null || true
                sleep 1
            fi
            sleep 0.2
        done
    done
    echo "  ports released"
    dlog INFO install.pre "ports released"
fi

# Step 3: Run the Go installer
echo ""
printf -- "${C_BOLD_CYAN}--- Installing ---${C_RESET}\n"
INSTALL_FLAGS=(--basedir="$BASEDIR" --repo="$REPO" --builddir="$BUILDDIR")
[[ "$WIRE" -eq 1 ]] && INSTALL_FLAGS+=(--wire)
[[ -n "$HOOKD_MODE" ]]           && INSTALL_FLAGS+=(--hookd-mode="$HOOKD_MODE")
[[ -n "$STORE_MODE" ]]           && INSTALL_FLAGS+=(--store-mode="$STORE_MODE")
[[ -n "$OTELCOL_MODE" ]]         && INSTALL_FLAGS+=(--otelcol-mode="$OTELCOL_MODE")
[[ -n "$PROMETHEUS_MODE" ]]      && INSTALL_FLAGS+=(--prometheus-mode="$PROMETHEUS_MODE")
[[ -n "$GRAFANA_MODE" ]]         && INSTALL_FLAGS+=(--grafana-mode="$GRAFANA_MODE")
[[ -n "$STORE_DSN" ]]            && INSTALL_FLAGS+=(--store-dsn="$STORE_DSN")
[[ -n "$HOOKD_ENDPOINT" ]]       && INSTALL_FLAGS+=(--hookd-endpoint="$HOOKD_ENDPOINT")
[[ -n "$OTELCOL_ENDPOINT" ]]     && INSTALL_FLAGS+=(--otelcol-endpoint="$OTELCOL_ENDPOINT")
[[ -n "$PROMETHEUS_ENDPOINT" ]]  && INSTALL_FLAGS+=(--prometheus-endpoint="$PROMETHEUS_ENDPOINT")
[[ -n "$GRAFANA_ENDPOINT" ]]     && INSTALL_FLAGS+=(--grafana-endpoint="$GRAFANA_ENDPOINT")
[[ -n "$PROMETHEUS_RETENTION" ]] && INSTALL_FLAGS+=(--prometheus-retention="$PROMETHEUS_RETENTION")
[[ -n "$PROMETHEUS_RETENTION_SIZE" ]] && INSTALL_FLAGS+=(--prometheus-retention-size="$PROMETHEUS_RETENTION_SIZE")
[[ -n "$TEAMSTER_ENV" ]]         && INSTALL_FLAGS+=(--env="$TEAMSTER_ENV")
[[ -n "$BACKUP_DIR" ]]           && INSTALL_FLAGS+=(--backup-dir="$BACKUP_DIR")
[[ -n "$BACKUP_SCHEDULE" ]]      && INSTALL_FLAGS+=(--backup-schedule="$BACKUP_SCHEDULE")
[[ -n "$DEBUG_LOG" ]]            && INSTALL_FLAGS+=(--debug-log="$DEBUG_LOG")
[[ "$HOOKD_READ_ONLY" -eq 1 ]]   && INSTALL_FLAGS+=(--hookd-read-only)
[[ -n "$RELAY_MODE" && "$RELAY_MODE" != "none" ]] && INSTALL_FLAGS+=(--relay-mode="$RELAY_MODE")
[[ -n "$RELAY_TARGET" ]]         && INSTALL_FLAGS+=(--relay-target="$RELAY_TARGET")
[[ -n "$REPL_PUSH_REMOTE" ]]    && INSTALL_FLAGS+=(--repl-push-remote="$REPL_PUSH_REMOTE")
dlog INFO install.subproc "exec teamster-install" "cmd=$(printf '%q ' "$BUILDDIR/teamster-install" "${INSTALL_FLAGS[@]}")"
if "$BUILDDIR/teamster-install" "${INSTALL_FLAGS[@]}"; then
    dlog INFO install.subproc "teamster-install done" "rc=0"
else
    _rc=$?
    dlog ERROR install.subproc "teamster-install failed" "rc=$_rc"
    exit $_rc
fi
unset _rc

# Grafana 13+ has no separate grafana-server binary. The supervisor expects
# bin/grafana-server, so create a wrapper that inserts the `server` subcommand.
if [[ -f "$BASEDIR/bin/grafana" ]] && [[ ! -f "$BASEDIR/bin/grafana-server" ]]; then
    cat > "$BASEDIR/bin/grafana-server" << 'WRAPPER'
#!/bin/sh
exec "$(dirname "$0")/grafana" server "$@"
WRAPPER
    chmod +x "$BASEDIR/bin/grafana-server"
fi

# Stamp the resolved version into BASEDIR so the Python hook client (which has
# no Go build to receive -ldflags) can read it. Mirrors the Go binaries' version.
printf '%s\n' "$TEAMSTER_VERSION" > "$BASEDIR/VERSION"

# Clean up renamed/removed skill directories from prior installs.
rm -rf "$BASEDIR/lib/plugin/skills/init" 2>/dev/null || true
rm -rf "$BASEDIR/lib/plugin/skills/muster" 2>/dev/null || true

# Add Teamster env vars to user's shell profile. ~/.bashrc is global user state,
# so this only runs when wiring a real install — stage-only (--basedir without
# --wire) must mutate nothing outside BASEDIR, matching the stage-only banner and
# the systemd-sync gate below.
if [[ "$WIRE" -eq 1 ]]; then
    _SHELLRC=""
    if [[ -f "$HOME/.bashrc" ]]; then
        _SHELLRC="$HOME/.bashrc"
    elif [[ -f "$HOME/.zshrc" ]]; then
        _SHELLRC="$HOME/.zshrc"
    fi
    _MARKER="# >>> teamster >>>"
    _END_MARKER="# <<< teamster <<<"
    if [[ -n "$_SHELLRC" ]] && ! grep -q "$_MARKER" "$_SHELLRC"; then
        cat >> "$_SHELLRC" << SHELLRC

$_MARKER
export TEAMSTER_BASEDIR="$BASEDIR"
export PATH="\$TEAMSTER_BASEDIR/bin:\$PATH"
$_END_MARKER
SHELLRC
        echo "  Added Teamster env to $_SHELLRC"
        dlog INFO install.bashrc "appended teamster block" "file=$_SHELLRC"
    elif [[ -n "$_SHELLRC" ]] && grep -q "$_MARKER" "$_SHELLRC"; then
        sed -i "/$_MARKER/,/$_END_MARKER/c\\
$_MARKER\\
export TEAMSTER_BASEDIR=\"$BASEDIR\"\\
export PATH=\"\\\$TEAMSTER_BASEDIR/bin:\\\$PATH\"\\
$_END_MARKER" "$_SHELLRC"
        echo "  Updated Teamster env in $_SHELLRC"
        dlog INFO install.bashrc "updated teamster block" "file=$_SHELLRC"
    fi
    unset _SHELLRC _MARKER _END_MARKER
fi

# Step 3.5: Sync systemd unit. teamster-install materializes the unit at
# $BASEDIR/etc/teamster-hookd.service with the right BASEDIR/PORT/USER, but
# systemd loads from /etc/systemd/system/. If they differ — drift from a
# previous install or a fresh install with no unit yet — copy and reload.
# Skipped in stage-only mode (WIRE=0) or supervisor mode (hookd managed by supervisor, not systemd).
if [[ "$WIRE" -eq 1 ]] && [[ "$HOOKD_MODE" != "supervisor" ]] && [[ "$HOOKD_MODE" != "external" ]] && command -v systemctl &>/dev/null; then
    SRC_UNIT="$BASEDIR/etc/teamster-hookd.service"
    DST_UNIT="/etc/systemd/system/teamster-hookd.service"
    if [[ -f "$SRC_UNIT" ]] && ! sudo cmp -s "$SRC_UNIT" "$DST_UNIT" 2>/dev/null; then
        echo ""
        printf -- "${C_BOLD_CYAN}--- Syncing systemd unit ---${C_RESET}\n"
        if [[ -f "$DST_UNIT" ]]; then
            sudo cp "$DST_UNIT" "$DST_UNIT.bak"
            echo "    backed up old unit → $DST_UNIT.bak"
            dlog INFO install.unit-sync "backed up" "from=$DST_UNIT" "to=$DST_UNIT.bak"
        fi
        sudo install -m 0644 "$SRC_UNIT" "$DST_UNIT"
        sudo systemctl daemon-reload
        echo "    installed $DST_UNIT (daemon-reloaded)"
        dlog INFO install.unit-sync "installed" "src=$SRC_UNIT" "dst=$DST_UNIT"
    elif [[ -f "$SRC_UNIT" ]]; then
        dlog INFO install.unit-sync "unchanged" "dst=$DST_UNIT"
    else
        dlog WARN install.unit-sync "src unit not present" "src=$SRC_UNIT"
    fi
fi

# Sync the rollup service + timer (attribution aggregation, every 5 min) into
# systemd and enable the timer. Same guards as the hookd unit: only when wiring
# locally-managed systemd. The service is Type=oneshot driven by the timer.
if [[ "$WIRE" -eq 1 ]] && [[ "$HOOKD_READ_ONLY" -eq 0 ]] && [[ "$HOOKD_MODE" != "supervisor" ]] && [[ "$HOOKD_MODE" != "external" ]] && command -v systemctl &>/dev/null; then
    ROLLUP_SVC_SRC="$BASEDIR/etc/teamster-rollup.service"
    ROLLUP_TIMER_SRC="$BASEDIR/etc/teamster-rollup.timer"
    if [[ -f "$ROLLUP_SVC_SRC" ]] && [[ -f "$ROLLUP_TIMER_SRC" ]]; then
        printf -- "${C_BOLD_CYAN}--- Syncing rollup timer ---${C_RESET}\n"
        sudo install -m 0644 "$ROLLUP_SVC_SRC" /etc/systemd/system/teamster-rollup.service
        sudo install -m 0644 "$ROLLUP_TIMER_SRC" /etc/systemd/system/teamster-rollup.timer
        sudo systemctl daemon-reload
        sudo systemctl enable --now teamster-rollup.timer 2>/dev/null \
            && echo "    enabled teamster-rollup.timer" \
            || printf -- "${C_YELLOW}    WARN: could not enable teamster-rollup.timer${C_RESET}\n"
        dlog INFO install.rollup-timer "installed" "svc=$ROLLUP_SVC_SRC" "timer=$ROLLUP_TIMER_SRC"
    else
        dlog WARN install.rollup-timer "src units not present" "svc=$ROLLUP_SVC_SRC"
    fi
fi

# Sync the classify service + timer (phase + work-type classifier, every 5 min)
# into systemd and enable the timer. Same guards as the rollup unit: only when
# wiring locally-managed systemd. The service is Type=oneshot driven by the timer.
if [[ "$WIRE" -eq 1 ]] && [[ "$HOOKD_READ_ONLY" -eq 0 ]] && [[ "$HOOKD_MODE" != "supervisor" ]] && [[ "$HOOKD_MODE" != "external" ]] && command -v systemctl &>/dev/null; then
    CLASSIFY_SVC_SRC="$BASEDIR/etc/teamster-classify.service"
    CLASSIFY_TIMER_SRC="$BASEDIR/etc/teamster-classify.timer"
    if [[ -f "$CLASSIFY_SVC_SRC" ]] && [[ -f "$CLASSIFY_TIMER_SRC" ]]; then
        printf -- "${C_BOLD_CYAN}--- Syncing classify timer ---${C_RESET}\n"
        sudo install -m 0644 "$CLASSIFY_SVC_SRC" /etc/systemd/system/teamster-classify.service
        sudo install -m 0644 "$CLASSIFY_TIMER_SRC" /etc/systemd/system/teamster-classify.timer
        sudo systemctl daemon-reload
        sudo systemctl enable --now teamster-classify.timer 2>/dev/null \
            && echo "    enabled teamster-classify.timer" \
            || printf -- "${C_YELLOW}    WARN: could not enable teamster-classify.timer${C_RESET}\n"
        dlog INFO install.classify-timer "installed" "svc=$CLASSIFY_SVC_SRC" "timer=$CLASSIFY_TIMER_SRC"
    else
        dlog WARN install.classify-timer "src units not present" "svc=$CLASSIFY_SVC_SRC"
    fi
fi

# Sync the codex-scraper service + timer (Codex rollout-JSONL tailer — sole
# writer of Codex cost/ledger data and the Codex sessions row) into systemd
# and enable the timer. Same guards as the classify unit: only when wiring
# locally-managed systemd. The service is Type=oneshot driven by the timer.
# Graceful on a host with no `codex` CLI: the binary finds no rollout files
# and exits 0 every run.
if [[ "$WIRE" -eq 1 ]] && [[ "$HOOKD_READ_ONLY" -eq 0 ]] && [[ "$HOOKD_MODE" != "supervisor" ]] && [[ "$HOOKD_MODE" != "external" ]] && command -v systemctl &>/dev/null; then
    CODEX_SCRAPER_SVC_SRC="$BASEDIR/etc/teamster-codex-scraper.service"
    CODEX_SCRAPER_TIMER_SRC="$BASEDIR/etc/teamster-codex-scraper.timer"
    if [[ -f "$CODEX_SCRAPER_SVC_SRC" ]] && [[ -f "$CODEX_SCRAPER_TIMER_SRC" ]]; then
        printf -- "${C_BOLD_CYAN}--- Syncing codex-scraper timer ---${C_RESET}\n"
        sudo install -m 0644 "$CODEX_SCRAPER_SVC_SRC" /etc/systemd/system/teamster-codex-scraper.service
        sudo install -m 0644 "$CODEX_SCRAPER_TIMER_SRC" /etc/systemd/system/teamster-codex-scraper.timer
        sudo systemctl daemon-reload
        sudo systemctl enable --now teamster-codex-scraper.timer 2>/dev/null \
            && echo "    enabled teamster-codex-scraper.timer" \
            || printf -- "${C_YELLOW}    WARN: could not enable teamster-codex-scraper.timer${C_RESET}\n"
        dlog INFO install.codex-scraper-timer "installed" "svc=$CODEX_SCRAPER_SVC_SRC" "timer=$CODEX_SCRAPER_TIMER_SRC"
    else
        dlog WARN install.codex-scraper-timer "src units not present" "svc=$CODEX_SCRAPER_SVC_SRC"
    fi
fi

# Sync the sweep service + timer (deep-clean attribution pipeline) into
# systemd and enable the timer. Same guards as the rollup unit: only when wiring
# locally-managed systemd. The service is Type=oneshot driven by the timer.
if [[ "$WIRE" -eq 1 ]] && [[ "$HOOKD_READ_ONLY" -eq 0 ]] && [[ "$HOOKD_MODE" != "supervisor" ]] && [[ "$HOOKD_MODE" != "external" ]] && command -v systemctl &>/dev/null; then
    SWEEP_SVC_SRC="$BASEDIR/etc/teamster-sweep.service"
    SWEEP_TIMER_SRC="$BASEDIR/etc/teamster-sweep.timer"
    if [[ -f "$SWEEP_SVC_SRC" ]] && [[ -f "$SWEEP_TIMER_SRC" ]]; then
        printf -- "${C_BOLD_CYAN}--- Syncing sweep timer ---${C_RESET}\n"
        sudo install -m 0644 "$SWEEP_SVC_SRC" /etc/systemd/system/teamster-sweep.service
        sudo install -m 0644 "$SWEEP_TIMER_SRC" /etc/systemd/system/teamster-sweep.timer
        sudo systemctl daemon-reload
        sudo systemctl enable --now teamster-sweep.timer 2>/dev/null \
            && echo "    enabled teamster-sweep.timer" \
            || printf -- "${C_YELLOW}    WARN: could not enable teamster-sweep.timer${C_RESET}\n"
        dlog INFO install.sweep-timer "installed" "svc=$SWEEP_SVC_SRC" "timer=$SWEEP_TIMER_SRC"
    else
        dlog WARN install.sweep-timer "src units not present" "svc=$SWEEP_SVC_SRC"
    fi
fi

# Sync the backup service + timer (hourly data backup) into systemd and enable
# the timer. Same guards as the other oneshot timers: only when wiring
# locally-managed systemd and not in read-only replica mode.
if [[ "$WIRE" -eq 1 ]] && [[ "$HOOKD_READ_ONLY" -eq 0 ]] && [[ "$HOOKD_MODE" != "supervisor" ]] && [[ "$HOOKD_MODE" != "external" ]] && command -v systemctl &>/dev/null; then
    BACKUP_SVC_SRC="$BASEDIR/etc/teamster-backup.service"
    BACKUP_TIMER_SRC="$BASEDIR/etc/teamster-backup.timer"
    if [[ -f "$BACKUP_SVC_SRC" ]] && [[ -f "$BACKUP_TIMER_SRC" ]]; then
        printf -- "${C_BOLD_CYAN}--- Syncing backup timer ---${C_RESET}\n"
        sudo install -m 0644 "$BACKUP_SVC_SRC" /etc/systemd/system/teamster-backup.service
        sudo install -m 0644 "$BACKUP_TIMER_SRC" /etc/systemd/system/teamster-backup.timer
        sudo systemctl daemon-reload
        # The timer is enabled but NOT started yet — the operator must set
        # backup_dir in $BASEDIR/etc/teamster.yaml (backup.backup_dir) first.
        sudo systemctl enable teamster-backup.timer 2>/dev/null \
            && echo "    enabled teamster-backup.timer (edit $BASEDIR/etc/teamster.yaml to set backup.backup_dir, then: sudo systemctl start teamster-backup.timer)" \
            || printf -- "${C_YELLOW}    WARN: could not enable teamster-backup.timer${C_RESET}\n"
        dlog INFO install.backup-timer "installed" "svc=$BACKUP_SVC_SRC" "timer=$BACKUP_TIMER_SRC"
    else
        dlog WARN install.backup-timer "src units not present" "svc=$BACKUP_SVC_SRC"
    fi
fi

# Sync the logrotate config into /etc/logrotate.d/ so events.jsonl gets rotated.
# teamster-install already materialized __BASEDIR__ in the config file.
if [[ "$WIRE" -eq 1 ]] && [[ -f "$BASEDIR/etc/teamster-logrotate.conf" ]]; then
    printf -- "${C_BOLD_CYAN}--- Syncing logrotate config ---${C_RESET}\n"
    sudo install -m 0644 "$BASEDIR/etc/teamster-logrotate.conf" /etc/logrotate.d/teamster
    echo "    installed /etc/logrotate.d/teamster"
    dlog INFO install.logrotate "installed" "src=$BASEDIR/etc/teamster-logrotate.conf" "dst=/etc/logrotate.d/teamster"
fi

# Materialize + sync the relay + repl-push services when --relay-mode=install.
# Unlike the other units, teamster-install does NOT materialize these templates
# (they carry relay-specific markers), so we do the marker replacement here on the
# skel-copied .tmpl files in $BASEDIR/etc/, then sync into systemd when wiring.
if [[ "${RELAY_MODE:-none}" == "install" ]]; then
    RELAY_SVC_SRC="$BASEDIR/etc/teamster-relay.service"
    if [[ -f "${RELAY_SVC_SRC}.tmpl" ]]; then
        sed -e "s|__BASEDIR__|$BASEDIR|g" \
            -e "s|__USER__|$(id -un)|g" \
            -e "s|__SOURCE_JSONL__|${BASEDIR}/var/events.jsonl|g" \
            -e "s|__TARGET_URL__|${RELAY_TARGET}|g" \
            "${RELAY_SVC_SRC}.tmpl" > "$RELAY_SVC_SRC"
        dlog INFO install.relay "materialized" "out=$RELAY_SVC_SRC" "target=$RELAY_TARGET"
    else
        dlog WARN install.relay "template not present" "tmpl=${RELAY_SVC_SRC}.tmpl"
    fi

    REPL_PUSH_SVC_SRC="$BASEDIR/etc/teamster-repl-push.service"
    if [[ -f "${REPL_PUSH_SVC_SRC}.tmpl" ]]; then
        sed -e "s|__BASEDIR__|$BASEDIR|g" \
            -e "s|__USER__|$(id -un)|g" \
            -e "s|__REPL_PUSH_REMOTE__|${REPL_PUSH_REMOTE}|g" \
            "${REPL_PUSH_SVC_SRC}.tmpl" > "$REPL_PUSH_SVC_SRC"
        dlog INFO install.repl-push "materialized" "out=$REPL_PUSH_SVC_SRC" "remote=$REPL_PUSH_REMOTE"
    else
        dlog WARN install.repl-push "template not present" "tmpl=${REPL_PUSH_SVC_SRC}.tmpl"
    fi

    if [[ "$WIRE" -eq 1 ]] && command -v systemctl &>/dev/null; then
        printf -- "${C_BOLD_CYAN}--- Syncing relay services ---${C_RESET}\n"
        if [[ -f "$RELAY_SVC_SRC" ]]; then
            sudo install -m 0644 "$RELAY_SVC_SRC" /etc/systemd/system/teamster-relay.service
            dlog INFO install.relay "installed" "src=$RELAY_SVC_SRC"
        fi
        if [[ -f "$REPL_PUSH_SVC_SRC" ]]; then
            sudo install -m 0644 "$REPL_PUSH_SVC_SRC" /etc/systemd/system/teamster-repl-push.service
            dlog INFO install.repl-push "installed" "src=$REPL_PUSH_SVC_SRC"
        fi
        sudo systemctl daemon-reload
        sudo systemctl enable --now teamster-relay.service 2>/dev/null \
            && echo "    enabled + started teamster-relay.service" \
            || printf -- "${C_YELLOW}    WARN: could not enable teamster-relay.service${C_RESET}\n"
        sudo systemctl enable --now teamster-repl-push.service 2>/dev/null \
            && echo "    enabled + started teamster-repl-push.service" \
            || printf -- "${C_YELLOW}    WARN: could not enable teamster-repl-push.service${C_RESET}\n"
    fi
fi

# Materialize replica-side observability configs when installing a read-only
# replica. teamster-install already copied skel/etc/ into $BASEDIR/etc/, so the
# templates are present; here we fill the port marker and point the operator at
# the resulting files. These are replica-scoped, so gate on --hookd-read-only.
if [[ "$HOOKD_READ_ONLY" -eq 1 ]]; then
    REPL_PROM_TMPL="$BASEDIR/etc/teamster-prometheus-replica.yml.tmpl"
    REPL_PROM_OUT="$BASEDIR/etc/teamster-prometheus-replica.yml"
    if [[ -f "$REPL_PROM_TMPL" ]]; then
        _hookd_port=$(grep -oP 'TEAMSTER_HOOK_SERVER_PORT=\K[0-9]+' \
            "$BASEDIR/etc/teamster-hookd.service" 2>/dev/null || echo 9125)
        sed "s|__HOOK_SERVER_PORT__|${_hookd_port}|g" \
            "$REPL_PROM_TMPL" > "$REPL_PROM_OUT"
        dlog INFO install.replica-config "materialized prometheus replica config" \
            "out=$REPL_PROM_OUT" "port=$_hookd_port"
        echo "    Replica Prometheus config: $REPL_PROM_OUT"
        echo "    Copy to your Prometheus config dir (e.g. /etc/prometheus/)"
    else
        dlog WARN install.replica-config "prometheus replica template not present" "tmpl=$REPL_PROM_TMPL"
    fi

    REPL_GRAFANA_INI="$BASEDIR/etc/grafana-anonymous.ini"
    if [[ -f "$REPL_GRAFANA_INI" ]]; then
        echo "    Replica Grafana config: $REPL_GRAFANA_INI"
        echo "    Merge into your Grafana config (e.g. /etc/grafana/grafana.ini)"
    fi
fi

# Step 4: Restart hookd based on how it was running before.
# Skipped in stage-only mode (WIRE=0).
if [[ "$WIRE" -eq 0 ]]; then
    echo ""
    printf -- "${C_BOLD_CYAN}--- Stage-only: no global state modified ---${C_RESET}\n"
    echo "    Binaries and skel staged to $BASEDIR."
    echo ""
    echo "    To wire this install (touches systemd + ~/.claude/settings.json):"
    echo "      $0 --basedir='$BASEDIR' --wire"
    echo ""
    echo "    Or wire manually:"
    echo "      sudo install -m 0644 '$BASEDIR'/etc/teamster-hookd.service /etc/systemd/system/teamster-hookd.service"
    echo "      sudo systemctl daemon-reload && sudo systemctl restart teamster-hookd"
    echo "      # Then merge '$BASEDIR'/etc/settings.fragment.json into ~/.claude/settings.json"
    echo "      # and register MCP servers via: claude mcp add-json --scope user ..."
else
echo ""
printf -- "${C_BOLD_CYAN}--- Restarting hookd ---${C_RESET}\n"

# Read the hookd port from settings.json (written by teamster-install; starts at 9125)
HOOKD_PORT=$(python3 -c "
import json, sys
try:
    s = json.load(open('$HOME/.claude/settings.json'))
    url = s.get('env', {}).get('TEAMSTER_HOOK_SERVER_URL', '')
    port = url.split(':')[-1].split('/')[0]
    print(port if port.isdigit() else '9125')
except Exception:
    print('9125')
" 2>/dev/null || echo "9125")

case "$RUNNING_MODE" in
    systemd)
        printf -- "${C_BOLD_WHITE}--> Starting teamster-hookd via systemd...${C_RESET}\n"
        dlog INFO install.hookd "start" "mode=systemd"
        sudo systemctl start teamster-hookd
        sleep 1
        if curl -fsS "http://localhost:${HOOKD_PORT}/health" >/dev/null; then
            printf -- "${C_GREEN}    hookd healthy (port %s)${C_RESET}\n" "$HOOKD_PORT"
            dlog INFO install.hookd "health ok" "mode=systemd" "port=$HOOKD_PORT"
        else
            printf -- "${C_YELLOW}    WARN: health check failed — check: sudo systemctl status teamster-hookd${C_RESET}\n"
            dlog WARN install.hookd "health failed" "mode=systemd" "port=$HOOKD_PORT"
        fi
        RESTORE_HOOKD=0
        ;;
    manual)
        printf -- "${C_BOLD_WHITE}--> Restarting hookd as background process...${C_RESET}\n"
        mkdir -p "$BASEDIR/var"
        nohup "$BASEDIR/bin/hookd" > "$BASEDIR/var/hookd.log" 2>&1 &
        _hpid=$!
        disown
        dlog INFO install.hookd "start" "mode=manual" "pid=$_hpid"
        sleep 1
        if curl -fsS "http://localhost:${HOOKD_PORT}/health" >/dev/null; then
            printf -- "${C_GREEN}    hookd healthy (port %s)${C_RESET}\n" "$HOOKD_PORT"
            dlog INFO install.hookd "health ok" "mode=manual" "port=$HOOKD_PORT" "pid=$_hpid"
        else
            printf -- "${C_YELLOW}    WARN: health check failed — check: %s/var/hookd.log${C_RESET}\n" "$BASEDIR"
            dlog WARN install.hookd "health failed" "mode=manual" "port=$HOOKD_PORT" "pid=$_hpid"
        fi
        echo "    (If you previously ran hookd with custom env/flags, kill this and respawn yours.)"
        RESTORE_HOOKD=0
        unset _hpid
        ;;
    none)
        echo "    Note: hookd was not running before install."
        echo "    Start via: sudo systemctl start teamster-hookd"
        echo "    Or manually: $BASEDIR/bin/hookd &"
        dlog INFO install.hookd "not previously running"
        ;;
esac

# Run MySQL migrations when store backend is MySQL or dual+MySQL.
# The migrate subcommand rejects dual: DSNs, so extract the mysql:// portion.
if [[ "$STORE_DSN" == mysql://* ]]; then
    MIGRATE_DSN="$STORE_DSN"
elif [[ "$STORE_DSN" == dual:*mysql://* ]]; then
    MIGRATE_DSN="${STORE_DSN##*,}"
fi
if [[ -n "${MIGRATE_DSN:-}" ]]; then
    echo ""
    printf -- "${C_BOLD_WHITE}--> Running MySQL schema migrations...${C_RESET}\n"
    "$BASEDIR/bin/teamster" store migrate --dsn "$MIGRATE_DSN" 2>&1 \
        && printf -- "${C_GREEN}    migrations applied${C_RESET}\n" \
        || printf -- "${C_YELLOW}    WARN: migration failed — check MySQL connectivity${C_RESET}\n"

    # Interactive tag vocabulary setup. Runs only when stdin is a terminal so
    # non-interactive installs (CI, --basedir staging, piped scripts) are never
    # blocked. The binary auto-detects first-run vs editor mode; on subsequent
    # installs it goes straight to the editor menu for any adjustments.
    if [[ -t 0 ]]; then
        echo ""
        printf -- "${C_BOLD_WHITE}--> Tag vocabulary setup...${C_RESET}\n"
        TEAMSTER_STORE_DSN="$MIGRATE_DSN" "$BASEDIR/bin/teamster" setup tags || true
    fi
fi

# Provision the Grafana read-only DB user (grafana_ro) when Grafana will connect
# to this host's MySQL — whether Grafana is bundled (install) or the system
# instance (external). The privileged GRANT runs here (socket root), not in the
# supervisor — the StoreDSN account lacks CREATE USER under managed mode. Runs
# after migrations so the schema/tables the GRANT targets exist. Non-fatal: a
# failure prints an actionable manual step (no silent skip).
if [[ "$GRAFANA_MODE" == "install" || "$GRAFANA_MODE" == "external" ]] && [[ -n "${MIGRATE_DSN:-}" ]] && [[ "$WIRE" -eq 1 ]]; then
    echo ""
    printf -- "${C_BOLD_WHITE}--> Provisioning Grafana read-only DB user...${C_RESET}\n"
    provision_grafana_ro "$MIGRATE_DSN" "$BASEDIR" || true
fi

# Deploy Grafana dashboard provisioning if Grafana is running on this host.
# Grafana runs as the grafana user and cannot traverse ~/teamster/, so we
# copy dashboards to /var/lib/grafana/dashboards/teamster/ and point the
# provisioner there.
if command -v systemctl &>/dev/null && { systemctl is-active --quiet grafana-server 2>/dev/null || systemctl is-enabled --quiet grafana-server 2>/dev/null; }; then
    echo ""
    printf -- "${C_BOLD_WHITE}--> Deploying Grafana dashboard provisioning...${C_RESET}\n"
    GRAFANA_PROV="/etc/grafana/provisioning"
    GRAFANA_DASH_DIR="/var/lib/grafana/dashboards/teamster"
    TEAMSTER_GRAFANA="$BASEDIR/etc/grafana"

    sudo mkdir -p "$GRAFANA_DASH_DIR" 2>/dev/null
    sudo cp "$TEAMSTER_GRAFANA"/dashboards/*.json "$GRAFANA_DASH_DIR/" 2>/dev/null \
        && printf -- "${C_GREEN}    dashboard JSON files deployed to %s${C_RESET}\n" "$GRAFANA_DASH_DIR" \
        || printf -- "${C_YELLOW}    WARN: could not copy dashboard JSON files${C_RESET}\n"

    # cp preserves the root-owned source perms (0750, no world-read), so the
    # grafana service user cannot read the dashboards. Hand them to grafana.
    sudo chown -R grafana:grafana "$GRAFANA_DASH_DIR" 2>/dev/null \
        && sudo chmod 644 "$GRAFANA_DASH_DIR"/*.json 2>/dev/null \
        && printf -- "${C_GREEN}    dashboard files chowned grafana:grafana 0644${C_RESET}\n" \
        || printf -- "${C_YELLOW}    WARN: could not set dashboard ownership/permissions${C_RESET}\n"

    if [[ -f "$TEAMSTER_GRAFANA/provisioning/dashboards/teamster.yaml.tmpl" ]]; then
        sed "s|{{ .GrafanaDir }}/dashboards|$GRAFANA_DASH_DIR|g" \
            "$TEAMSTER_GRAFANA/provisioning/dashboards/teamster.yaml.tmpl" \
            > "$TEAMSTER_GRAFANA/provisioning/dashboards/teamster.yaml"
        sudo cp "$TEAMSTER_GRAFANA/provisioning/dashboards/teamster.yaml" \
            "$GRAFANA_PROV/dashboards/teamster.yaml" 2>/dev/null \
            && printf -- "${C_GREEN}    dashboard provisioner deployed${C_RESET}\n" \
            || printf -- "${C_YELLOW}    WARN: could not deploy dashboard provisioner (need sudo?)${C_RESET}\n"
    fi

    PROM_PORT="${PROMETHEUS_PORT:-9090}"
    if [[ -f "$TEAMSTER_GRAFANA/provisioning/datasources/teamster.yaml.tmpl" ]]; then
        # Decompose the store DSN and read the grafana_ro password so we can
        # substitute ALL template vars — not just PrometheusPort. Leaving Go
        # template placeholders in the output produces invalid YAML that
        # crashes the external Grafana instance.
        _ds_host="" _ds_port="3306" _ds_db="" _ds_user="grafana_ro" _ds_pw=""
        if [[ -n "${MIGRATE_DSN:-}" ]]; then
            _ds_host=$(echo "$MIGRATE_DSN" | sed -n 's|mysql://[^@]*@\([^:/]*\).*|\1|p')
            _ds_port=$(echo "$MIGRATE_DSN" | sed -n 's|mysql://[^@]*@[^:]*:\([0-9]*\)/.*|\1|p')
            _ds_db=$(echo "$MIGRATE_DSN" | sed -n 's|mysql://[^/]*/\([^?]*\).*|\1|p')
            [[ -z "$_ds_port" ]] && _ds_port="3306"
        fi
        _pw_file="$BASEDIR/var/grafana/grafana_ro_password"
        [[ -s "$_pw_file" ]] && _ds_pw=$(tr -d '\n' < "$_pw_file")

        # Escape the password for the sed replacement side so a non-hex
        # generator can't corrupt the rendered YAML (no-op on today's hex).
        _ds_pw_esc=$(sed_escape_replacement "$_ds_pw")
        sed -e "s|{{ .PrometheusPort }}|$PROM_PORT|g" \
            -e "s|{{ .HookdPort }}|$HOOKD_PORT|g" \
            -e "s|{{ .StoreHost }}|$_ds_host|g" \
            -e "s|{{ .StorePort }}|$_ds_port|g" \
            -e "s|{{ .StoreDB }}|$_ds_db|g" \
            -e "s|{{ .GrafanaDBUser }}|$_ds_user|g" \
            -e "s|{{ .GrafanaDBPassword }}|$_ds_pw_esc|g" \
            "$TEAMSTER_GRAFANA/provisioning/datasources/teamster.yaml.tmpl" \
            > "$TEAMSTER_GRAFANA/provisioning/datasources/teamster.yaml"
        sudo cp "$TEAMSTER_GRAFANA/provisioning/datasources/teamster.yaml" \
            "$GRAFANA_PROV/datasources/teamster.yaml" 2>/dev/null \
            && printf -- "${C_GREEN}    datasource provisioner deployed${C_RESET}\n" \
            || printf -- "${C_YELLOW}    WARN: could not deploy datasource provisioner (need sudo?)${C_RESET}\n"
    fi

    # Advisory: check Grafana version meets minimum for Pathfinder and groupings.
    GRAFANA_MIN_VERSION="13.0.0"
    _grafana_ver=""
    if command -v grafana-server &>/dev/null; then
        _grafana_ver=$(grafana-server -v 2>/dev/null | grep -oP 'Version \K[0-9]+\.[0-9]+\.[0-9]+' || true)
    fi
    if [[ -z "$_grafana_ver" ]] && command -v grafana &>/dev/null; then
        _grafana_ver=$(grafana server -v 2>/dev/null | grep -oP 'Version \K[0-9]+\.[0-9]+\.[0-9]+' || true)
    fi
    if [[ -n "$_grafana_ver" ]] && version_lt "$_grafana_ver" "$GRAFANA_MIN_VERSION"; then
        _arch="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
        printf -- "${C_YELLOW}    NOTE: Grafana %s is below the minimum %s required for Pathfinder and dashboard groupings.${C_RESET}\n" "$_grafana_ver" "$GRAFANA_MIN_VERSION"
        printf -- "${C_YELLOW}          Upgrade manually (Teamster will not upgrade a shared Grafana):${C_RESET}\n"
        printf -- "${C_YELLOW}            curl -fsSLO https://dl.grafana.com/oss/release/grafana_%s_${_arch}.deb && sudo dpkg -i grafana_%s_${_arch}.deb${C_RESET}\n" "$GRAFANA_MIN_VERSION" "$GRAFANA_MIN_VERSION"
        dlog WARN install.grafana-version "grafana $_grafana_ver below minimum $GRAFANA_MIN_VERSION — manual upgrade required"
    fi

    # Advisory: check Pathfinder plugin is installed.
    if ! sudo grafana cli plugins ls 2>/dev/null | grep -q "grafana-pathfinder-app" \
        && ! ls /var/lib/grafana/plugins/grafana-pathfinder-app/plugin.json &>/dev/null; then
        printf -- "${C_YELLOW}    NOTE: the Pathfinder interactive learning feature requires the 'grafana-pathfinder-app' plugin,${C_RESET}\n"
        printf -- "${C_YELLOW}          which is not installed on this shared Grafana. Install it manually:${C_RESET}\n"
        printf -- "${C_YELLOW}            sudo grafana cli plugins install grafana-pathfinder-app && sudo systemctl restart grafana-server${C_RESET}\n"
        dlog WARN install.grafana-plugin "pathfinder plugin missing on external grafana — manual install required"
    fi

    # Advisory only: the Entity Cost Treemap needs the volkovlabs-echarts-panel
    # plugin. On an external/shared Grafana we must NOT install plugins or
    # restart it — the operator owns its plugin set. Check whether it's already
    # present and, if not, tell the operator the one manual step.
    if ! sudo grafana cli plugins ls 2>/dev/null | grep -q "volkovlabs-echarts-panel" \
        && ! ls /var/lib/grafana/plugins/volkovlabs-echarts-panel/plugin.json &>/dev/null; then
        printf -- "${C_YELLOW}    NOTE: the Entity Cost Treemap requires the 'volkovlabs-echarts-panel' Grafana plugin,${C_RESET}\n"
        printf -- "${C_YELLOW}          which is not installed on this shared Grafana. Install it manually (Teamster${C_RESET}\n"
        printf -- "${C_YELLOW}          will not touch a shared instance):${C_RESET}\n"
        printf -- "${C_YELLOW}            sudo grafana cli plugins install volkovlabs-echarts-panel 7.2.2 && sudo systemctl restart grafana-server${C_RESET}\n"
        dlog WARN install.grafana-plugin "echarts panel plugin missing on external grafana — manual install required"
    fi

    if ! sudo grafana cli plugins ls 2>/dev/null | grep -q "yesoreyeram-infinity-datasource" \
        && ! ls /var/lib/grafana/plugins/yesoreyeram-infinity-datasource/plugin.json &>/dev/null; then
        printf -- "${C_YELLOW}    NOTE: the Activity Feed panel requires the 'yesoreyeram-infinity-datasource' Grafana plugin,${C_RESET}\n"
        printf -- "${C_YELLOW}          which is not installed on this shared Grafana. Install it manually:${C_RESET}\n"
        printf -- "${C_YELLOW}            sudo grafana cli plugins install yesoreyeram-infinity-datasource && sudo systemctl restart grafana-server${C_RESET}\n"
        dlog WARN install.grafana-plugin "infinity datasource plugin missing on external grafana — manual install required"
    fi

    # Reload only — never restart. Grafana has no reload handler (CanReload=no),
    # so this exits non-zero and we fall through to INFO. We must NOT restart:
    # an external-mode Grafana is shared with the operator's other tenants, and
    # its file provisioner rescans on its own (~30s) so our provisioning applies
    # without a restart anyway.
    sudo systemctl reload grafana-server 2>/dev/null \
        || printf -- "${C_YELLOW}    INFO: grafana has no reload handler — dashboard/datasource provisioning applies on the next poll (~30s); if a datasource change must take effect immediately, restart grafana-server manually at a convenient time${C_RESET}\n"
fi

fi

# Restart the Teamster-MANAGED grafana bundle on upgrade so a freshly-staged
# grafana plugin actually loads. Plugins are read only at grafana START, and the
# `teamster stop` above killed the managed grafana along with the rest of the
# supervisor's bundle — without this, an upgrade would stage the new plugin but
# leave grafana down (or, if restarted out-of-band, only then pick it up), so the
# treemap would be broken-until-manual-restart. `teamster start` is idempotent
# (each component is guarded by processAlive) and re-launches grafana fresh.
#
# Gated strictly to grafana-mode=install (the supervisor owns this grafana) and
# to the supervisor having been running before we stopped it — we never touch an
# external/shared grafana (per 176c562) and never auto-start a supervisor the
# operator hadn't running. hookd is restarted separately above; under hookd
# systemd mode `teamster start` skips re-launching hookd but still brings up the
# managed monitoring bundle.
if [[ "$WIRE" -eq 1 ]] && [[ "${GRAFANA_MODE:-install}" == "install" ]] && [[ "$SUPERVISOR_WAS_RUNNING" -eq 1 ]]; then
    echo ""
    printf -- "${C_BOLD_WHITE}--> Restarting managed monitoring bundle (loads staged grafana plugins)...${C_RESET}\n"
    dlog INFO install.supervisor "restart managed bundle for plugin load"
    if "$BASEDIR/bin/teamster" start >/dev/null 2>&1; then
        printf -- "${C_GREEN}    managed grafana restarted — staged plugins loaded${C_RESET}\n"
        dlog INFO install.supervisor "managed bundle restarted" "rc=0"
    else
        printf -- "${C_YELLOW}    WARN: 'teamster start' failed — run it manually so the new grafana plugin loads:${C_RESET}\n"
        printf -- "${C_YELLOW}            %s/bin/teamster start${C_RESET}\n" "$BASEDIR"
        dlog WARN install.supervisor "managed bundle restart failed — manual teamster start required"
    fi
fi

SCRIPT_OK=1
dlog INFO install.parse "script complete" "rc=0"
echo ""
printf -- "${C_BOLD_CYAN}=== Install complete ===${C_RESET}\n"

# --- Next steps guide ---
GRAFANA_PORT="${GRAFANA_PORT:-3000}"
SHORT_HOST="${SHORT_HOST:-$(hostname -s 2>/dev/null || hostname)}"
echo ""
printf -- "${C_BOLD_GREEN}━━━ Next Steps ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${C_RESET}\n"
echo ""
printf -- "${C_BOLD_WHITE}  1. Restart Claude Code${C_RESET} to activate the hooks and MCP servers.\n"
echo ""
printf -- "${C_BOLD_WHITE}  2. Use /teamster:start${C_RESET} at the beginning of each work session.\n"
printf -- "     It sets up cost tracking and picks team vs solo mode.\n"
echo ""
if [[ "$GRAFANA_MODE" == "install" || "$GRAFANA_MODE" == "managed" ]]; then
    printf -- "${C_BOLD_WHITE}  3. View your dashboards${C_RESET} at:\n"
    printf -- "     ${C_CYAN}http://${SHORT_HOST}:${GRAFANA_PORT}/dashboards${C_RESET}\n"
    printf -- "     Login with ${C_BOLD_WHITE}admin/admin${C_RESET} — change the password on first login.\n"
    printf -- "     ${C_YELLOW}Note:${C_RESET} dashboards will be empty until Teamster captures session data.\n"
    echo ""
fi
printf -- "${C_BOLD_WHITE}  4. Configure your tag vocabulary:${C_RESET}\n"
printf -- "     ${C_CYAN}teamster setup tags${C_RESET}     — guided TUI wizard (first time)\n"
printf -- "     ${C_CYAN}/teamster:tags${C_RESET}          — conversational refinement (day two+)\n"
echo ""
printf -- "${C_BOLD_WHITE}  5. Add other hosts${C_RESET} where Claude Code runs:\n"
printf -- "     ${C_CYAN}teamster install-remote user@host${C_RESET}\n"
printf -- "     Installs the lightweight client — no Go or daemons needed on remotes.\n"
echo ""
printf -- "${C_BOLD_GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${C_RESET}\n"
