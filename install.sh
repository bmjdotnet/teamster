#!/usr/bin/env bash
# install.sh — interactive Teamster installer
#
# Purpose
# -------
# Walks the operator through every install choice with text prompts, pre-inspects
# the host for stale Claude Code config / port conflicts / prior installs, then
# builds and runs lib/installrunner.sh with the right flags.
#
# This file is intentionally verbose and readable. Optimize for clarity over
# compactness. Use only standard bash + jq (with python3 fallback). No
# whiptail/dialog/external menu libs. No Python script files.
#
# Usage:
#   cd /path/to/teamster && ./install.sh
#
# Scope:
#   - apt-based Linux only
#   - bash 4+
#   - touches NOTHING on the host — see [[wizard-installer-boundary]]. This
#     script inspects and guides; lib/installrunner.sh + teamster-install
#     mutate. Conflict decisions are emitted as --settings-remove/-keep/-replace
#     flags.
#   - does NOT touch live install state directly — it invokes lib/installrunner.sh

set -u
# NB: deliberately not 'set -e' — we want to keep going past non-fatal probes
# and surface each finding to the operator. See memory: no silent failures.

# ──────────────────────────────────────────────────────────────────────────
# Debug-log instrumentation (--debug-log=<path>)
# ──────────────────────────────────────────────────────────────────────────
# Empty DEBUG_LOG → all dlog/dtrace_* helpers are no-ops.
# Set by parse_flags() from --debug-log=<path>. When install.sh invokes
# lib/installrunner.sh it forwards a SIBLING path (<dir>/install.log) so
# both scripts write separate files in the same slug dir — operator tails
# both side-by-side.
DEBUG_LOG=""
DEBUG_LOG_DIR=""    # parent dir of DEBUG_LOG, used as side-file home for subprocess stdout/stderr

# Locked format with @installer:
#   <RFC3339-UTC>  <LEVEL5>  <component>  <freeform>[ k=v ...]
# Two-space field separators. LEVEL ∈ {TRACE, DEBUG, INFO , WARN , ERROR} padded to 5.
# Append-only.
dlog() {
    [[ -z "$DEBUG_LOG" ]] && return 0
    local level="$1"; shift
    local component="$1"; shift
    local ts
    ts="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
    printf '%s  %-5s  %s  %s\n' "$ts" "$level" "$component" "$*" >>"$DEBUG_LOG"
}

# dtrace_enter FN COMPONENT  → emit `>> FN` at TRACE level (component wizard.<scope>).
# Pair with `local _DTRACE_FN=FN; trap 'dtrace_exit "$_DTRACE_FN" <component>' RETURN`
# at top of each function so all return paths emit a closing `<<`.
dtrace_enter() {
    [[ -z "$DEBUG_LOG" ]] && return 0
    dlog TRACE "wizard.${2:-fn}" ">> $1"
}
dtrace_exit() {
    [[ -z "$DEBUG_LOG" ]] && return 0
    dlog TRACE "wizard.${2:-fn}" "<< $1"
}

# parse_flags ARGS...
# Parses install.sh's own CLI. Currently only --debug-log=<path>.
# Sets DEBUG_LOG and DEBUG_LOG_DIR globals. Dies loudly on unwritable path
# (per [[no-silent-failures]] — never swallow log-write failures).
parse_flags() {
    while (($#)); do
        case "$1" in
            --debug-log=*)
                DEBUG_LOG="${1#--debug-log=}"
                shift
                ;;
            --debug-log)
                if [[ $# -lt 2 ]]; then
                    printf 'ERROR: --debug-log requires a path argument\n' >&2
                    exit 1
                fi
                DEBUG_LOG="$2"
                shift 2
                ;;
            -h|--help)
                printf 'Usage: %s [--debug-log=<path>]\n' "$0"
                printf '\n'
                printf '  --debug-log=<path>   write structured instrumentation trace to <path>\n'
                printf '                       (append-only; parent dir is created if missing;\n'
                printf '                        forwarded to lib/installrunner.sh as --debug-log=<path>)\n'
                exit 0
                ;;
            *)
                printf 'ERROR: unknown flag: %s\n' "$1" >&2
                printf '       (run with --help for usage)\n' >&2
                exit 1
                ;;
        esac
    done

    [[ -z "$DEBUG_LOG" ]] && return 0

    DEBUG_LOG_DIR="$(dirname -- "$DEBUG_LOG")"
    if ! mkdir -p "$DEBUG_LOG_DIR" 2>/dev/null; then
        printf 'ERROR: --debug-log path %s unwritable: cannot mkdir parent %s\n' \
            "$DEBUG_LOG" "$DEBUG_LOG_DIR" >&2
        exit 1
    fi
    # Touch + write probe to surface permission errors loudly NOW, not at first event.
    if ! printf '' >>"$DEBUG_LOG" 2>/dev/null; then
        printf 'ERROR: --debug-log path %s unwritable: append failed\n' "$DEBUG_LOG" >&2
        exit 1
    fi
    dlog INFO  wizard.main "debug_log_opened path=$DEBUG_LOG pid=$$"
}

# run_subproc COMPONENT CMD [ARGS...]
# Run a subprocess with stdout/stderr teed to side-files; log cmd and rc.
# Side-files live next to DEBUG_LOG; named subproc.<N>.{stdout,stderr} so
# successive invocations don't clobber each other.
# Returns the subprocess rc. If rc != 0, also emits up to 20 stderr-tail lines
# inline at ERROR so the smoking gun is in-band per agreement w/ @installer.
__SUBPROC_SEQ=0
run_subproc() {
    local component="$1"; shift
    if [[ -z "$DEBUG_LOG" ]]; then
        "$@"
        return $?
    fi
    __SUBPROC_SEQ=$((__SUBPROC_SEQ + 1))
    local base="${DEBUG_LOG_DIR}/subproc.${__SUBPROC_SEQ}"
    local out="${base}.stdout" err="${base}.stderr"
    local quoted
    quoted="$(printf '%q ' "$@")"
    dlog INFO  "$component" "cmd_start cmd=\"${quoted% }\" stdout=$out stderr=$err"
    "$@" >"$out" 2>"$err"
    local rc=$?
    if [[ $rc -eq 0 ]]; then
        dlog INFO  "$component" "cmd_complete rc=0"
    else
        local nlines=0
        [[ -r "$err" ]] && nlines="$(wc -l <"$err" 2>/dev/null || echo 0)"
        local cap=$((nlines < 20 ? nlines : 20))
        dlog ERROR "$component" "cmd_complete rc=$rc stderr_file=$err stderr_tail_follows=$cap"
        if [[ $cap -gt 0 ]]; then
            local line
            while IFS= read -r line; do
                dlog ERROR "${component}.stderr" "line=\"$(printf '%s' "$line" | sed 's/"/\\"/g')\""
            done < <(tail -n "$cap" "$err" 2>/dev/null)
        fi
    fi
    return $rc
}

# ──────────────────────────────────────────────────────────────────────────
# Globals filled in by interview + probes
# ──────────────────────────────────────────────────────────────────────────
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETTINGS_FILE="${HOME}/.claude/settings.json"

# Interview outputs
INSTALL_MODE=""          # "hub" | "client"
HOOKD_ENDPOINT=""        # client mode: hub URL
BASEDIR=""               # install target
HOOKD_MODE=""            # "systemd" | "supervisor" | "external"

# Per-service mode decisions (populated by interview)
STORE_MODE=""            # install | external | managed
OTELCOL_MODE=""          # install | external | managed | none
PROMETHEUS_MODE=""       # install | external | managed | none
GRAFANA_MODE=""          # install | external | managed | none

STORE_DSN=""             # --store-dsn value
ENV_LABEL=""             # --env
RETENTION=""             # --prometheus-retention
OTELCOL_BUILD_FROM_SRC=0
PROMETHEUS_BUILD_FROM_SRC=0
GRAFANA_BUILD_FROM_SRC=0
AUTO_START=0             # run `teamster start` after install
WIRE=1                   # default: wire (the whole point of install.sh is a real install)

# Per-service endpoint URLs (only set when mode=external or managed with custom URL)
OTELCOL_ENDPOINT=""      # --otelcol-endpoint=URL
PROMETHEUS_ENDPOINT=""   # --prometheus-endpoint=URL
GRAFANA_ENDPOINT=""      # --grafana-endpoint=URL

# Relay / replication (opt-in, hub mode only). Empty → no relay flags emitted.
RELAY_MODE=""            # --relay-mode=none|install
RELAY_TARGET=""          # --relay-target=URL
REPL_PUSH_REMOTE=""      # --repl-push-remote=user@host
HOOKD_READ_ONLY=0        # --hookd-read-only (replica deployments)

# Plane integration (opt-in). Empty → no flags emitted → no Plane sync.
PLANE_URL=""             # --plane-url=URL    → settings.json TEAMSTER_PLANE_URL
PLANE_API_KEY=""         # --plane-api-key=KEY → settings.json TEAMSTER_PLANE_API_KEY

# Probe results (raw text, displayed in summary)
PROBE_SETTINGS=""
PROBE_BASEDIR=""
PROBE_PORTS=""
PROBE_SYSTEMD=""
PROBE_PREREQS=""
PROBE_SUDO=""

# Detected settings.json env values (only set if file exists + parses)
DETECTED_TEAMSTER_HOOK_URL=""
DETECTED_CLAUDE_HOOK_SERVER=""
DETECTED_OTEL_ENDPOINT=""
DETECTED_STORE_DSN=""
DETECTED_PROMETHEUS_URL=""
DETECTED_GRAFANA_URL=""
DETECTED_PROMETHEUS_MODE=""
DETECTED_GRAFANA_MODE=""
DETECTED_OTEL_MODE=""

# Detected relay config (from systemd units, staged files, or teamster.yaml)
DETECTED_RELAY_TARGET=""
DETECTED_REPL_PUSH_REMOTE=""

# JSON tool selection. Prefer jq, fall back to python3.
JSON_TOOL=""

# ANSI color constants — empty when stdout is not a terminal
if [[ -t 1 ]]; then
    C_RESET=$'\033[0m'
    C_BOLD=$'\033[1m'
    C_RED=$'\033[0;31m'
    C_GREEN=$'\033[0;32m'
    C_YELLOW=$'\033[0;33m'
    C_CYAN=$'\033[0;36m'
    C_BOLD_RED=$'\033[1;31m'
    C_BOLD_CYAN=$'\033[1;36m'
    C_BOLD_WHITE=$'\033[1;37m'
else
    C_RESET='' C_BOLD='' C_RED='' C_GREEN='' C_YELLOW=''
    C_CYAN='' C_BOLD_RED='' C_BOLD_CYAN='' C_BOLD_WHITE=''
fi

# ──────────────────────────────────────────────────────────────────────────
# Output helpers
# ──────────────────────────────────────────────────────────────────────────
section()   { printf "\n${C_BOLD_CYAN}── %s " "$1"; printf '%.0s─' $(seq 1 $((60 - ${#1}))); printf "${C_RESET}\n"; }
info()      { printf '  %s\n' "$*"; }
warn()      { printf "${C_YELLOW}  WARN: %s${C_RESET}\n" "$*" >&2; }
err()       { printf "${C_BOLD_RED}  ERROR: %s${C_RESET}\n" "$*" >&2; }
banner()    { printf "\n${C_BOLD_CYAN}═══════════════════════════════════════════════════════════\n%s\n═══════════════════════════════════════════════════════════${C_RESET}\n" "$*"; }

# ──────────────────────────────────────────────────────────────────────────
# Prompt helpers
# ──────────────────────────────────────────────────────────────────────────

# ask VAR PROMPT DEFAULT
# Text input. Empty input → DEFAULT.
ask() {
    local __varname="$1" __prompt="$2" __default="${3:-}"
    local __reply
    if [[ -n "$__default" ]]; then
        read -r -p "$(printf "${C_BOLD_WHITE}  %s${C_RESET} [%s]: " "$__prompt" "$__default")" __reply
        __reply="${__reply:-$__default}"
    else
        read -r -p "$(printf "${C_BOLD_WHITE}  %s${C_RESET}: " "$__prompt")" __reply
    fi
    printf -v "$__varname" '%s' "$__reply"
    dlog INFO  wizard.interview "prompt key=$__varname prompt=\"$__prompt\" default=\"$__default\" parsed=\"$__reply\""
}

# ask_yn VAR PROMPT DEFAULT(Y|N)
# Yes/no. Result stored as 1 (yes) or 0 (no).
ask_yn() {
    local __varname="$1" __prompt="$2" __default="$3"
    local __hint __reply
    if [[ "$__default" == "Y" || "$__default" == "y" ]]; then
        __hint="Y/n"
    else
        __hint="y/N"
    fi
    while :; do
        read -r -p "$(printf "${C_BOLD_WHITE}  %s${C_RESET} (%s): " "$__prompt" "$__hint")" __reply
        __reply="${__reply:-$__default}"
        case "$__reply" in
            Y|y|yes|YES)
                printf -v "$__varname" '%s' 1
                dlog INFO  wizard.interview "prompt key=$__varname prompt=\"$__prompt\" default=$__default raw=\"$__reply\" parsed=1"
                return ;;
            N|n|no|NO)
                printf -v "$__varname" '%s' 0
                dlog INFO  wizard.interview "prompt key=$__varname prompt=\"$__prompt\" default=$__default raw=\"$__reply\" parsed=0"
                return ;;
            *) warn "answer y or n" ;;
        esac
    done
}

# ask_choice VAR PROMPT default-index opt1 opt2 ...
# Numbered menu. Operator types 1, 2, etc; empty = default. Stores the option
# *text* (not the index) in VAR.
ask_choice() {
    local __varname="$1" __prompt="$2" __default="$3"
    shift 3
    local __opts=("$@")
    local __i __reply
    printf "${C_BOLD_WHITE}  %s${C_RESET}\n" "$__prompt"
    for __i in "${!__opts[@]}"; do
        local __num=$((__i + 1))
        local __marker=""
        [[ "$__num" == "$__default" ]] && __marker=" [default]"
        printf '    %d) %s%s\n' "$__num" "${__opts[$__i]}" "$__marker"
    done
    while :; do
        read -r -p "$(printf "${C_BOLD_WHITE}  choose${C_RESET} [%s]: " "$__default")" __reply
        __reply="${__reply:-$__default}"
        if [[ "$__reply" =~ ^[0-9]+$ ]] && (( __reply >= 1 && __reply <= ${#__opts[@]} )); then
            local __chosen="${__opts[$((__reply - 1))]}"
            printf -v "$__varname" '%s' "$__chosen"
            dlog INFO  wizard.interview "prompt key=$__varname prompt=\"$__prompt\" default_index=$__default chosen_index=$__reply chosen=\"$__chosen\""
            return
        fi
        warn "enter a number 1-${#__opts[@]}"
    done
}

# ──────────────────────────────────────────────────────────────────────────
# JSON tool detection / wrappers
# ──────────────────────────────────────────────────────────────────────────
detect_json_tool() {
    local _DTRACE_FN="detect_json_tool"
    dtrace_enter "$_DTRACE_FN" json
    trap 'dtrace_exit "$_DTRACE_FN" json' RETURN
    if command -v jq >/dev/null 2>&1; then
        JSON_TOOL="jq"
    elif command -v python3 >/dev/null 2>&1; then
        JSON_TOOL="python3"
    else
        JSON_TOOL=""
    fi
    dlog INFO  wizard.json "selected tool=\"${JSON_TOOL:-<none>}\" jq=$(command -v jq 2>/dev/null || echo none) python3=$(command -v python3 2>/dev/null || echo none)"
}

# json_get FILE PATH
# PATH is a dotted env path like ".env.TEAMSTER_HOOK_SERVER_URL".
# Prints value or empty. Exits 0 always.
json_get() {
    local file="$1" path="$2"
    [[ -r "$file" ]] || return 0
    case "$JSON_TOOL" in
        jq)
            jq -r "$path // empty" "$file" 2>/dev/null
            ;;
        python3)
            # Convert ".env.FOO" → ["env","FOO"]
            local py_path="${path#.}"
            python3 - "$file" "$py_path" <<'PY' 2>/dev/null || true
import json, sys
fn, path = sys.argv[1], sys.argv[2]
try:
    with open(fn) as f:
        d = json.load(f)
except Exception:
    sys.exit(0)
for part in path.split("."):
    if not part:
        continue
    if isinstance(d, dict) and part in d:
        d = d[part]
    else:
        sys.exit(0)
if isinstance(d, (str, int, float, bool)):
    print(d)
PY
            ;;
        *)
            # Last-resort grep (imprecise — caller has been warned).
            local key="${path##*.}"
            grep -o "\"$key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$file" 2>/dev/null \
                | head -1 \
                | sed -E 's/.*: *"([^"]*)".*/\1/'
            ;;
    esac
}

# json_keys FILE PATH  → newline-separated key names under PATH (an object)
json_keys() {
    local file="$1" path="$2"
    [[ -r "$file" ]] || return 0
    case "$JSON_TOOL" in
        jq)
            jq -r "$path // {} | keys[]?" "$file" 2>/dev/null
            ;;
        python3)
            local py_path="${path#.}"
            python3 - "$file" "$py_path" <<'PY' 2>/dev/null || true
import json, sys
fn, path = sys.argv[1], sys.argv[2]
try:
    with open(fn) as f:
        d = json.load(f)
except Exception:
    sys.exit(0)
for part in path.split("."):
    if not part:
        continue
    if isinstance(d, dict) and part in d:
        d = d[part]
    else:
        sys.exit(0)
if isinstance(d, dict):
    for k in d:
        print(k)
PY
            ;;
        *)
            # No good fallback — return nothing.
            return 0
            ;;
    esac
}

# json_array_len FILE PATH  → length of array at PATH, or 0
json_array_len() {
    local file="$1" path="$2"
    [[ -r "$file" ]] || { echo 0; return 0; }
    case "$JSON_TOOL" in
        jq)
            jq -r "($path // []) | length" "$file" 2>/dev/null || echo 0
            ;;
        python3)
            local py_path="${path#.}"
            python3 - "$file" "$py_path" <<'PY' 2>/dev/null || echo 0
import json, sys
fn, path = sys.argv[1], sys.argv[2]
try:
    with open(fn) as f:
        d = json.load(f)
except Exception:
    print(0); sys.exit(0)
for part in path.split("."):
    if not part:
        continue
    if isinstance(d, dict) and part in d:
        d = d[part]
    else:
        print(0); sys.exit(0)
if isinstance(d, list):
    print(len(d))
else:
    print(0)
PY
            ;;
        *)
            echo 0
            ;;
    esac
}

# ──────────────────────────────────────────────────────────────────────────
# Pre-install probes
# ──────────────────────────────────────────────────────────────────────────

probe_settings() {
    local _DTRACE_FN="probe_settings"
    dtrace_enter "$_DTRACE_FN" probe
    trap 'dtrace_exit "$_DTRACE_FN" probe' RETURN
    section "State check: settings.json"
    if [[ ! -f "$SETTINGS_FILE" ]]; then
        info "fresh — $SETTINGS_FILE does not exist (no existing Claude Code config)"
        PROBE_SETTINGS="fresh"
        dlog INFO  wizard.probe "settings result=fresh path=$SETTINGS_FILE"
        return
    fi
    info "found $SETTINGS_FILE"
    dlog INFO  wizard.probe "settings found path=$SETTINGS_FILE json_tool=\"${JSON_TOOL:-<none>}\""

    if [[ -z "$JSON_TOOL" ]]; then
        warn "neither jq nor python3 found — settings.json inspection will be imprecise"
        info "raw grep follows; treat as advisory only:"
        grep -E 'TEAMSTER_HOOK_SERVER_URL|CLAUDE_HOOK_SERVER|OTEL_EXPORTER_OTLP_ENDPOINT' \
            "$SETTINGS_FILE" 2>/dev/null | sed 's/^/    /'
        PROBE_SETTINGS="imprecise"
        dlog WARN  wizard.probe "settings result=imprecise reason=no_json_tool"
        return
    fi

    DETECTED_TEAMSTER_HOOK_URL="$(json_get "$SETTINGS_FILE" '.env.TEAMSTER_HOOK_SERVER_URL')"
    DETECTED_CLAUDE_HOOK_SERVER="$(json_get "$SETTINGS_FILE" '.env.CLAUDE_HOOK_SERVER')"
    DETECTED_OTEL_ENDPOINT="$(json_get "$SETTINGS_FILE" '.env.OTEL_EXPORTER_OTLP_ENDPOINT')"
    DETECTED_STORE_DSN="$(json_get "$SETTINGS_FILE" '.env.TEAMSTER_STORE_DSN')"
    dlog DEBUG wizard.probe "settings detected TEAMSTER_HOOK_SERVER_URL=\"$DETECTED_TEAMSTER_HOOK_URL\" CLAUDE_HOOK_SERVER=\"$DETECTED_CLAUDE_HOOK_SERVER\" OTEL_EXPORTER_OTLP_ENDPOINT=\"$DETECTED_OTEL_ENDPOINT\" TEAMSTER_STORE_DSN=\"$DETECTED_STORE_DSN\""

    if [[ -n "$DETECTED_TEAMSTER_HOOK_URL" ]]; then
        info "env.TEAMSTER_HOOK_SERVER_URL = ${C_GREEN}$DETECTED_TEAMSTER_HOOK_URL${C_RESET}"
        if [[ "$DETECTED_TEAMSTER_HOOK_URL" == http://localhost:*/event \
           || "$DETECTED_TEAMSTER_HOOK_URL" == http://127.0.0.1:*/event ]]; then
            warn "  ↑ pointing at localhost — remote clients will fail (hookd binds all interfaces; a hostname URL is the correct hub value)"
        fi
    else
        info "env.TEAMSTER_HOOK_SERVER_URL = (unset)"
    fi

    if [[ -n "$DETECTED_CLAUDE_HOOK_SERVER" ]]; then
        info "env.CLAUDE_HOOK_SERVER = ${C_GREEN}$DETECTED_CLAUDE_HOOK_SERVER${C_RESET}"
        if [[ "$DETECTED_CLAUDE_HOOK_SERVER" == http://localhost:* \
           || "$DETECTED_CLAUDE_HOOK_SERVER" == http://127.0.0.1:* ]]; then
            warn "  ↑ pointing at localhost — remote clients will fail (hookd binds all interfaces; a hostname URL is the correct hub value)"
        fi
    else
        info "env.CLAUDE_HOOK_SERVER = (unset)"
    fi

    if [[ -n "$DETECTED_OTEL_ENDPOINT" ]]; then
        info "env.OTEL_EXPORTER_OTLP_ENDPOINT = ${C_GREEN}$DETECTED_OTEL_ENDPOINT${C_RESET}"
        if [[ "$DETECTED_OTEL_ENDPOINT" != http://localhost:* \
           && "$DETECTED_OTEL_ENDPOINT" != http://127.0.0.1:* ]]; then
            warn "  ↑ not pointing at localhost — may block local otelcol"
        fi
    else
        info "env.OTEL_EXPORTER_OTLP_ENDPOINT = (unset)"
    fi

    if [[ -n "$DETECTED_STORE_DSN" ]]; then
        info "env.TEAMSTER_STORE_DSN = ${C_GREEN}$DETECTED_STORE_DSN${C_RESET}"
    fi

    local _prom_port _graf_port
    _prom_port="$(json_get "$SETTINGS_FILE" '.env.TEAMSTER_PROMETHEUS_PORT')"
    _graf_port="$(json_get "$SETTINGS_FILE" '.env.TEAMSTER_GRAFANA_PORT')"
    if [[ -n "$_prom_port" ]]; then
        DETECTED_PROMETHEUS_URL="http://localhost:$_prom_port"
        info "env.TEAMSTER_PROMETHEUS_PORT = ${C_GREEN}$_prom_port${C_RESET}"
        dlog INFO wizard.probe "settings detected TEAMSTER_PROMETHEUS_PORT=$_prom_port url=$DETECTED_PROMETHEUS_URL"
    fi
    if [[ -n "$_graf_port" ]]; then
        DETECTED_GRAFANA_URL="http://localhost:$_graf_port"
        info "env.TEAMSTER_GRAFANA_PORT = ${C_GREEN}$_graf_port${C_RESET}"
        dlog INFO wizard.probe "settings detected TEAMSTER_GRAFANA_PORT=$_graf_port url=$DETECTED_GRAFANA_URL"
    fi
    if [[ -z "$DETECTED_PROMETHEUS_URL" ]]; then
        if systemctl is-active --quiet prometheus 2>/dev/null; then
            DETECTED_PROMETHEUS_URL="http://localhost:9090"
            dlog INFO wizard.probe "prometheus_detected via=systemctl url=$DETECTED_PROMETHEUS_URL"
        elif timeout 2 bash -c 'echo >/dev/tcp/localhost/9090' 2>/dev/null; then
            DETECTED_PROMETHEUS_URL="http://localhost:9090"
            dlog INFO wizard.probe "prometheus_detected via=tcp url=$DETECTED_PROMETHEUS_URL"
        fi
    fi
    if [[ -z "$DETECTED_GRAFANA_URL" ]]; then
        if systemctl is-active --quiet grafana-server 2>/dev/null; then
            DETECTED_GRAFANA_URL="http://localhost:3000"
            dlog INFO wizard.probe "grafana_detected via=systemctl url=$DETECTED_GRAFANA_URL"
        elif timeout 2 bash -c 'echo >/dev/tcp/localhost/3000' 2>/dev/null; then
            DETECTED_GRAFANA_URL="http://localhost:3000"
            dlog INFO wizard.probe "grafana_detected via=tcp url=$DETECTED_GRAFANA_URL"
        fi
    fi
    if [[ -z "$DETECTED_OTEL_ENDPOINT" ]]; then
        if timeout 2 bash -c 'echo >/dev/tcp/localhost/4317' 2>/dev/null; then
            DETECTED_OTEL_ENDPOINT="http://localhost:4317"
            dlog INFO wizard.probe "otelcol_detected via=tcp port=4317 url=$DETECTED_OTEL_ENDPOINT"
        elif timeout 2 bash -c 'echo >/dev/tcp/localhost/4318' 2>/dev/null; then
            DETECTED_OTEL_ENDPOINT="http://localhost:4318"
            dlog INFO wizard.probe "otelcol_detected via=tcp port=4318 url=$DETECTED_OTEL_ENDPOINT"
        fi
    fi

    # Hook arrays — how many handlers in each event class
    local found_hooks=0
    while IFS= read -r evt; do
        [[ -z "$evt" ]] && continue
        local n
        n="$(json_array_len "$SETTINGS_FILE" ".hooks.${evt}")"
        if [[ "$n" != "0" && -n "$n" ]]; then
            info "hooks.$evt: $n entries"
            found_hooks=1
        fi
    done < <(json_keys "$SETTINGS_FILE" '.hooks')
    [[ $found_hooks -eq 0 ]] && info "no hook handlers registered"

    # MCP servers
    local mcp_names
    mcp_names="$(json_keys "$SETTINGS_FILE" '.mcpServers' | paste -sd, -)"
    if [[ -n "$mcp_names" ]]; then
        info "mcpServers: $mcp_names"
    else
        info "no MCP servers registered (this file — note global ~/.claude.json is separate)"
    fi
    dlog DEBUG wizard.probe "settings mcpServers=\"${mcp_names:-<none>}\""

    PROBE_SETTINGS="ok"
    dlog INFO  wizard.probe "settings result=ok"
}

probe_yaml() {
    local _DTRACE_FN="probe_yaml"
    dtrace_enter "$_DTRACE_FN" probe
    trap 'dtrace_exit "$_DTRACE_FN" probe' RETURN
    local dir="${1:-$HOME/teamster}"
    local yaml_file="$dir/etc/teamster.yaml"
    [[ -r "$yaml_file" ]] || return 0
    section "State check: prior install config ($yaml_file)"
    info "found $yaml_file — seeding defaults"
    dlog INFO wizard.probe "yaml path=$yaml_file"

    # Parse yaml with python3 (always available on systems that run Claude Code).
    # Falls back to grep-based parsing when python3 is absent.
    _yaml_get() {
        local key="$1"
        if command -v python3 >/dev/null 2>&1; then
            python3 - "$yaml_file" "$key" <<'PY' 2>/dev/null || true
import sys
try:
    data = open(sys.argv[1]).read()
    key = sys.argv[2]
    for line in data.splitlines():
        line = line.strip()
        if line.startswith(key + ':'):
            val = line[len(key)+1:].strip().strip('"').strip("'")
            if val and val != 'null' and val != '""' and val != "''":
                print(val)
                break
except Exception:
    pass
PY
        else
            grep -m1 "^[[:space:]]*${key}:" "$yaml_file" 2>/dev/null \
                | sed -E "s/^[[:space:]]*${key}:[[:space:]]*//" \
                | tr -d '"'"'" \
                | grep -v '^$'
        fi
    }

    local yaml_store_dsn yaml_hookd_port yaml_prom_mode yaml_prom_port
    local yaml_prom_health="" yaml_graf_health=""
    local yaml_graf_mode yaml_graf_port yaml_otel_mode yaml_otel_grpc yaml_otel_http
    local yaml_relay_mode="" yaml_relay_target="" yaml_repl_push=""

    yaml_store_dsn="$(_yaml_get dsn)"
    yaml_hookd_port="$(_yaml_get port)"
    yaml_prom_mode="$(_yaml_get mode)"   # first match = hookd; need context-aware parse below
    yaml_prom_port="$(_yaml_get port)"   # same issue — use python3 block for accuracy

    # Use python3 for structured parsing when available (more accurate than grep).
    # Write script to a tmpfile to avoid heredoc-inside-$() bash parser warning.
    if command -v python3 >/dev/null 2>&1; then
        local _pytmp _parsed
        _pytmp=$(mktemp)
        cat >"$_pytmp" <<'PY'
import sys, re
try:
    data = open(sys.argv[1]).read()
    sections = {}
    cur = None
    for line in data.splitlines():
        m = re.match(r'^(\w[\w-]*):', line)
        if m:
            cur = m.group(1)
            sections[cur] = {}
        elif cur and re.match(r'^\s+', line):
            km = re.match(r'^\s+([\w_]+):\s*(.*)', line)
            if km:
                sections[cur][km.group(1)] = km.group(2).strip().strip('"\'')
    def emit(k, v):
        if v and v not in ('null', '""', "''", ''):
            print(f'{k}={v}')
    emit('store_dsn',   sections.get('store', {}).get('dsn', ''))
    emit('hookd_port',  sections.get('hookd', {}).get('port', ''))
    emit('prom_mode',   sections.get('prometheus', {}).get('mode', ''))
    emit('prom_port',   sections.get('prometheus', {}).get('port', ''))
    emit('prom_health', sections.get('prometheus', {}).get('health', ''))
    emit('graf_mode',   sections.get('grafana', {}).get('mode', ''))
    emit('graf_port',   sections.get('grafana', {}).get('port', ''))
    emit('graf_health', sections.get('grafana', {}).get('health', ''))
    emit('otel_mode',   sections.get('otelcol', {}).get('mode', ''))
    emit('otel_grpc',   sections.get('otelcol', {}).get('grpc_port', ''))
    emit('otel_http',   sections.get('otelcol', {}).get('http_port', ''))
    emit('scraper_mode',sections.get('token-scraper', {}).get('mode', ''))
    emit('relay_mode',   sections.get('relay', {}).get('mode', ''))
    emit('relay_target', sections.get('relay', {}).get('target', ''))
    emit('repl_push',    sections.get('relay', {}).get('repl_push_remote', ''))
except Exception as e:
    import sys as _s; print(f'err={e}', file=_s.stderr)
PY
        _parsed=$(python3 "$_pytmp" "$yaml_file" 2>/dev/null || true)
        rm -f "$_pytmp"
        # Evaluate into local vars
        local _k _v
        while IFS='=' read -r _k _v; do
            [[ -z "$_k" ]] && continue
            case "$_k" in
                store_dsn)    yaml_store_dsn="$_v" ;;
                hookd_port)   yaml_hookd_port="$_v" ;;
                prom_mode)    yaml_prom_mode="$_v" ;;
                prom_port)    yaml_prom_port="$_v" ;;
                prom_health)  yaml_prom_health="$_v" ;;
                graf_mode)    yaml_graf_mode="$_v" ;;
                graf_port)    yaml_graf_port="$_v" ;;
                graf_health)  yaml_graf_health="$_v" ;;
                otel_mode)    yaml_otel_mode="$_v" ;;
                otel_grpc)    yaml_otel_grpc="$_v" ;;
                otel_http)    yaml_otel_http="$_v" ;;
                relay_mode)   yaml_relay_mode="$_v" ;;
                relay_target) yaml_relay_target="$_v" ;;
                repl_push)    yaml_repl_push="$_v" ;;
            esac
        done <<< "$_parsed"
    fi

    # _host_from_url URL — extract hostname from a URL, default to "localhost"
    _host_from_url() {
        local url="$1"
        if [[ -n "$url" ]]; then
            echo "$url" | sed -n 's|https\?://\([^:/]*\).*|\1|p'
        fi
    }

    # Seed DETECTED_* globals — yaml wins over probe results (more authoritative).
    # Use hostnames from health URLs when available, not hardcoded "localhost".
    if [[ -n "$yaml_store_dsn" && -z "$DETECTED_STORE_DSN" ]]; then
        DETECTED_STORE_DSN="$yaml_store_dsn"
        info "store DSN from yaml: $DETECTED_STORE_DSN"
        dlog INFO wizard.probe "yaml seeded DETECTED_STORE_DSN=\"$yaml_store_dsn\""
    fi
    if [[ -n "$yaml_prom_port" && -z "$DETECTED_PROMETHEUS_URL" ]]; then
        local _prom_host
        _prom_host="$(_host_from_url "$yaml_prom_health")"
        _prom_host="${_prom_host:-localhost}"
        DETECTED_PROMETHEUS_URL="http://${_prom_host}:${yaml_prom_port}"
        info "prometheus from yaml: $DETECTED_PROMETHEUS_URL (mode=${yaml_prom_mode:-?})"
        dlog INFO wizard.probe "yaml seeded DETECTED_PROMETHEUS_URL=\"$DETECTED_PROMETHEUS_URL\" mode=$yaml_prom_mode"
    fi
    if [[ -n "$yaml_prom_mode" && -z "$DETECTED_PROMETHEUS_MODE" ]]; then
        DETECTED_PROMETHEUS_MODE="$yaml_prom_mode"
        dlog INFO wizard.probe "yaml seeded DETECTED_PROMETHEUS_MODE=$yaml_prom_mode"
    fi
    if [[ -n "$yaml_graf_port" && -z "$DETECTED_GRAFANA_URL" ]]; then
        local _graf_host
        _graf_host="$(_host_from_url "$yaml_graf_health")"
        _graf_host="${_graf_host:-localhost}"
        DETECTED_GRAFANA_URL="http://${_graf_host}:${yaml_graf_port}"
        info "grafana from yaml: $DETECTED_GRAFANA_URL (mode=${yaml_graf_mode:-?})"
        dlog INFO wizard.probe "yaml seeded DETECTED_GRAFANA_URL=\"$DETECTED_GRAFANA_URL\" mode=$yaml_graf_mode"
    fi
    if [[ -n "$yaml_graf_mode" && -z "$DETECTED_GRAFANA_MODE" ]]; then
        DETECTED_GRAFANA_MODE="$yaml_graf_mode"
        dlog INFO wizard.probe "yaml seeded DETECTED_GRAFANA_MODE=$yaml_graf_mode"
    fi
    if [[ -z "$DETECTED_OTEL_ENDPOINT" ]]; then
        local _otel_port="${yaml_otel_grpc:-${yaml_otel_http}}"
        if [[ -n "$_otel_port" ]]; then
            DETECTED_OTEL_ENDPOINT="http://localhost:${_otel_port}"
            info "otelcol from yaml: $DETECTED_OTEL_ENDPOINT (mode=${yaml_otel_mode:-?})"
            dlog INFO wizard.probe "yaml seeded DETECTED_OTEL_ENDPOINT=\"$DETECTED_OTEL_ENDPOINT\" mode=$yaml_otel_mode"
        fi
    fi
    if [[ -n "$yaml_otel_mode" && -z "$DETECTED_OTEL_MODE" ]]; then
        DETECTED_OTEL_MODE="$yaml_otel_mode"
        dlog INFO wizard.probe "yaml seeded DETECTED_OTEL_MODE=$yaml_otel_mode"
    fi

    # Seed relay detected globals from yaml
    if [[ -n "$yaml_relay_target" && -z "$DETECTED_RELAY_TARGET" ]]; then
        DETECTED_RELAY_TARGET="$yaml_relay_target"
        info "relay target from yaml: $DETECTED_RELAY_TARGET"
        dlog INFO wizard.probe "yaml seeded DETECTED_RELAY_TARGET=\"$yaml_relay_target\""
    fi
    if [[ -n "$yaml_repl_push" && -z "$DETECTED_REPL_PUSH_REMOTE" ]]; then
        DETECTED_REPL_PUSH_REMOTE="$yaml_repl_push"
        info "repl-push remote from yaml: $DETECTED_REPL_PUSH_REMOTE"
        dlog INFO wizard.probe "yaml seeded DETECTED_REPL_PUSH_REMOTE=\"$yaml_repl_push\""
    fi

    dlog INFO wizard.probe "yaml done"
}

probe_basedir() {
    local _DTRACE_FN="probe_basedir"
    dtrace_enter "$_DTRACE_FN" probe
    trap 'dtrace_exit "$_DTRACE_FN" probe' RETURN
    local dir="${1:-$HOME/teamster}"
    section "State check: BASEDIR ($dir)"
    if [[ ! -e "$dir" ]]; then
        info "does not exist — clean target"
        PROBE_BASEDIR="clean"
        dlog INFO  wizard.probe "basedir dir=$dir result=clean"
        return
    fi
    if [[ ! -d "$dir" ]]; then
        warn "$dir exists but is not a directory"
        PROBE_BASEDIR="not-dir"
        dlog WARN  wizard.probe "basedir dir=$dir result=not-dir"
        return
    fi
    local count
    count="$(ls -A "$dir" 2>/dev/null | wc -l)"
    if [[ "$count" -eq 0 ]]; then
        info "exists, empty"
        PROBE_BASEDIR="empty"
        dlog INFO  wizard.probe "basedir dir=$dir result=empty"
        return
    fi
    info "exists, $count entries:"
    ls -1 "$dir" 2>/dev/null | sed 's/^/    /'
    local has_prior=0
    if [[ -x "$dir/bin/teamster" ]]; then
        warn "prior install detected — $dir/bin/teamster present"
        has_prior=1
    fi
    PROBE_BASEDIR="populated"
    dlog INFO  wizard.probe "basedir dir=$dir result=populated entries=$count prior_install=$has_prior"
}

probe_ports() {
    local _DTRACE_FN="probe_ports"
    dtrace_enter "$_DTRACE_FN" probe
    trap 'dtrace_exit "$_DTRACE_FN" probe' RETURN
    section "State check: ports"
    local ports=(9125 9128 9190 3000 3100 4317 4318 4327 4328 13133)
    if ! command -v ss >/dev/null 2>&1; then
        warn "ss(1) not available — skipping port probe"
        PROBE_PORTS="no-ss"
        dlog WARN  wizard.probe "ports result=no-ss"
        return
    fi
    local listening
    listening="$(ss -ltnp 2>/dev/null || ss -ltn 2>/dev/null)"
    local any_conflict=0 bound_list=""
    for p in "${ports[@]}"; do
        local line
        line="$(echo "$listening" | awk -v p=":$p\$" '$4 ~ p {print}')"
        if [[ -n "$line" ]]; then
            info "port $p: BOUND  → $line"
            any_conflict=1
            bound_list="${bound_list:+$bound_list,}$p"
        fi
    done
    if [[ $any_conflict -eq 0 ]]; then
        info "all teamster ports (9125, 9128, 9190, 3100, 4327/8, 13133) free"
    else
        warn "one or more teamster ports are already bound — service install may conflict"
    fi
    PROBE_PORTS="ok"
    dlog INFO  wizard.probe "ports result=ok bound=\"${bound_list:-<none>}\""
}

probe_systemd() {
    local _DTRACE_FN="probe_systemd"
    dtrace_enter "$_DTRACE_FN" probe
    trap 'dtrace_exit "$_DTRACE_FN" probe' RETURN
    section "State check: systemd units"
    if ! command -v systemctl >/dev/null 2>&1; then
        info "systemctl not available — skipping"
        PROBE_SYSTEMD="no-systemctl"
        dlog INFO  wizard.probe "systemd result=no-systemctl"
        return
    fi
    local user_units sys_units
    user_units="$(systemctl --user list-unit-files 'teamster*' 2>/dev/null \
                  | awk 'NR>1 && $1 ~ /\.service$/ {print $1}' || true)"
    sys_units="$(systemctl list-unit-files 'teamster*' 2>/dev/null \
                  | awk 'NR>1 && $1 ~ /\.service$/ {print $1}' || true)"
    if [[ -z "$user_units" && -z "$sys_units" ]]; then
        info "no existing teamster-*.service units found"
    else
        [[ -n "$user_units" ]] && { info "user units:"; echo "$user_units" | sed 's/^/    /'; }
        [[ -n "$sys_units"  ]] && { info "system units:"; echo "$sys_units"  | sed 's/^/    /'; }
    fi
    PROBE_SYSTEMD="ok"
    dlog INFO  wizard.probe "systemd result=ok user_units=\"$(echo "$user_units" | tr '\n' ',' | sed 's/,$//')\" sys_units=\"$(echo "$sys_units" | tr '\n' ',' | sed 's/,$//')\""
}

probe_prereqs() {
    local _DTRACE_FN="probe_prereqs"
    dtrace_enter "$_DTRACE_FN" probe
    trap 'dtrace_exit "$_DTRACE_FN" probe' RETURN
    section "State check: build prerequisites (informational)"
    local missing=() found=()
    for cmd in go make curl tar git; do
        if command -v "$cmd" >/dev/null 2>&1; then
            info "$cmd: $(command -v "$cmd")"
            found+=("$cmd")
        else
            missing+=("$cmd")
        fi
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        warn "missing: ${missing[*]} — the installer will auto-install (apt) if needed"
    fi
    PROBE_PREREQS="ok"
    dlog INFO  wizard.probe "prereqs result=ok found=\"${found[*]}\" missing=\"${missing[*]:-<none>}\""
}

probe_sudo() {
    local _DTRACE_FN="probe_sudo"
    dtrace_enter "$_DTRACE_FN" probe
    trap 'dtrace_exit "$_DTRACE_FN" probe' RETURN
    section "State check: sudo"
    if sudo -n true 2>/dev/null; then
        info "passwordless sudo available"
        PROBE_SUDO="passwordless"
    else
        info "sudo will prompt for password (or unavailable)"
        PROBE_SUDO="prompt"
    fi
    dlog INFO  wizard.probe "sudo result=$PROBE_SUDO"
}

probe_relay() {
    local _DTRACE_FN="probe_relay"
    dtrace_enter "$_DTRACE_FN" probe
    trap 'dtrace_exit "$_DTRACE_FN" probe' RETURN
    local dir="${1:-$HOME/teamster}"

    # Already seeded from yaml — skip systemd extraction
    if [[ -n "$DETECTED_RELAY_TARGET" && -n "$DETECTED_REPL_PUSH_REMOTE" ]]; then
        dlog INFO  wizard.probe "relay already_seeded target=\"$DETECTED_RELAY_TARGET\" remote=\"$DETECTED_REPL_PUSH_REMOTE\""
        return
    fi

    # Try systemd unit file on disk
    local relay_svc="/etc/systemd/system/teamster-relay.service"
    if [[ -r "$relay_svc" && -z "$DETECTED_RELAY_TARGET" ]]; then
        local _target
        _target=$(grep -oP '(?<=--target )\S+' "$relay_svc" 2>/dev/null || true)
        if [[ -n "$_target" ]]; then
            DETECTED_RELAY_TARGET="$_target"
            info "relay target from systemd: $DETECTED_RELAY_TARGET"
            dlog INFO wizard.probe "relay seeded from systemd target=\"$_target\""
        fi
    fi

    local repl_svc="/etc/systemd/system/teamster-repl-push.service"
    if [[ -r "$repl_svc" && -z "$DETECTED_REPL_PUSH_REMOTE" ]]; then
        local _remote
        _remote=$(grep -oP '(?<=REPL_PUSH_REMOTE=)\S+' "$repl_svc" 2>/dev/null || true)
        if [[ -n "$_remote" ]]; then
            DETECTED_REPL_PUSH_REMOTE="$_remote"
            info "repl-push remote from systemd: $DETECTED_REPL_PUSH_REMOTE"
            dlog INFO wizard.probe "relay seeded from systemd repl_push_remote=\"$_remote\""
        fi
    fi

    # Fallback: staged service files in BASEDIR/etc/
    local staged_relay="$dir/etc/teamster-relay.service"
    if [[ -r "$staged_relay" && -z "$DETECTED_RELAY_TARGET" ]]; then
        local _target
        _target=$(grep -oP '(?<=--target )\S+' "$staged_relay" 2>/dev/null || true)
        if [[ -n "$_target" ]]; then
            DETECTED_RELAY_TARGET="$_target"
            info "relay target from staged config: $DETECTED_RELAY_TARGET"
            dlog INFO wizard.probe "relay seeded from staged target=\"$_target\""
        fi
    fi
    local staged_repl="$dir/etc/teamster-repl-push.service"
    if [[ -r "$staged_repl" && -z "$DETECTED_REPL_PUSH_REMOTE" ]]; then
        local _remote
        _remote=$(grep -oP '(?<=REPL_PUSH_REMOTE=)\S+' "$staged_repl" 2>/dev/null || true)
        if [[ -n "$_remote" ]]; then
            DETECTED_REPL_PUSH_REMOTE="$_remote"
            info "repl-push remote from staged config: $DETECTED_REPL_PUSH_REMOTE"
            dlog INFO wizard.probe "relay seeded from staged repl_push_remote=\"$_remote\""
        fi
    fi

    if [[ -n "$DETECTED_RELAY_TARGET" || -n "$DETECTED_REPL_PUSH_REMOTE" ]]; then
        section "State check: relay configuration"
        [[ -n "$DETECTED_RELAY_TARGET" ]] && info "relay target:     $DETECTED_RELAY_TARGET"
        [[ -n "$DETECTED_REPL_PUSH_REMOTE" ]] && info "repl-push remote: $DETECTED_REPL_PUSH_REMOTE"
    fi
    dlog INFO  wizard.probe "relay result target=\"${DETECTED_RELAY_TARGET:-<none>}\" remote=\"${DETECTED_REPL_PUSH_REMOTE:-<none>}\""
}

# ──────────────────────────────────────────────────────────────────────────
# Interview
# ──────────────────────────────────────────────────────────────────────────

interview() {
    local _DTRACE_FN="interview"
    dtrace_enter "$_DTRACE_FN" interview
    trap 'dtrace_exit "$_DTRACE_FN" interview' RETURN
    section "Interview"

    # Q1 — install mode
    local mode_choice=""
    ask_choice mode_choice "Install mode:" 1 \
        "Hub (this host runs hookd + bundle locally)" \
        "Client (this host points at an existing hub elsewhere)"
    if [[ "$mode_choice" == Hub* ]]; then
        INSTALL_MODE="hub"
    else
        INSTALL_MODE="client"
    fi
    dlog INFO  wizard.interview "decided INSTALL_MODE=$INSTALL_MODE from mode_choice=\"$mode_choice\""

    if [[ "$INSTALL_MODE" == "client" ]]; then
        ask HOOKD_ENDPOINT "Hub URL (host:port or http://...)" "hub-host:9128"
        ask BASEDIR "Base directory" "$HOME/teamster"
        # No per-service / store / etc questions for client mode.
        dlog INFO  wizard.interview "client_mode_summary HOOKD_ENDPOINT=$HOOKD_ENDPOINT BASEDIR=$BASEDIR"
        return
    fi

    # Hub-mode questions
    ask BASEDIR "Base directory" "$HOME/teamster"

    local hookd_choice=""
    ask_choice hookd_choice "hookd supervision mode:" 1 \
        "systemd (default for hub installs)" \
        "supervisor (default for cleanroom / no-systemd hosts)"
    if [[ "$hookd_choice" == systemd* ]]; then
        HOOKD_MODE="systemd"
    else
        HOOKD_MODE="supervisor"
    fi
    dlog INFO  wizard.interview "decided HOOKD_MODE=$HOOKD_MODE from hookd_choice=\"$hookd_choice\""

    # Per-service interview — for each of otelcol/prometheus/grafana:
    # sets <SERVICE>_MODE and optionally <SERVICE>_ENDPOINT.
    # MySQL/store and ccusage handled separately below.
    resolve_services
    dlog DEBUG wizard.interview "service_decisions OTELCOL_MODE=$OTELCOL_MODE OTELCOL_ENDPOINT=\"$OTELCOL_ENDPOINT\" PROMETHEUS_MODE=$PROMETHEUS_MODE PROMETHEUS_ENDPOINT=\"$PROMETHEUS_ENDPOINT\" GRAFANA_MODE=$GRAFANA_MODE GRAFANA_ENDPOINT=\"$GRAFANA_ENDPOINT\""

    # Store backend — MySQL required; sqlite deadlocks under concurrent use.
    local mysql_default_dsn=""
    if [[ -n "$DETECTED_STORE_DSN" ]]; then
        mysql_default_dsn="$DETECTED_STORE_DSN"
        dlog INFO wizard.interview "mysql_existing_dsn=\"$DETECTED_STORE_DSN\""
    fi
    local mysql_running=0
    if systemctl is-active --quiet mysql 2>/dev/null || systemctl is-active --quiet mysqld 2>/dev/null || systemctl is-active --quiet mariadb 2>/dev/null; then
        mysql_running=1
        info "MySQL detected on this host"
        dlog INFO wizard.interview "mysql_detected running=yes"
    fi
    info ""
    if [[ $mysql_running -eq 1 || -n "$mysql_default_dsn" ]]; then
        # MySQL detected: offer managed (default) or fresh install
        local _dsn_default="${mysql_default_dsn:-mysql://teamster:CHANGEME@localhost:3306/teamster}"
        local mysql_choice=""
        ask_choice mysql_choice "MySQL / store backend:" 1 \
            "Managed — use the running MySQL, I will supply the DSN [default]" \
            "Install — re-install and re-provision MySQL (replaces existing)"
        case "$mysql_choice" in
            Install*)
                STORE_MODE="install"
                local _gen_pw
                _gen_pw=$(head -c 16 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 20)
                STORE_DSN="mysql://teamster:${_gen_pw}@localhost:3306/teamster"
                info "MySQL will be (re)installed. Generated DSN: $STORE_DSN"
                dlog INFO wizard.interview "decided STORE_MODE=install dsn=\"$STORE_DSN\""
                ;;
            *)
                STORE_MODE="managed"
                ask STORE_DSN "MySQL DSN" "$_dsn_default"
                dlog INFO wizard.interview "decided STORE_MODE=managed"
                ;;
        esac
    else
        warn "MySQL not detected on this host"
        local mysql_choice=""
        ask_choice mysql_choice "MySQL / store backend:" 1 \
            "Install — Teamster installs and manages MySQL [default]" \
            "External — I will supply a DSN for an existing MySQL server" \
            "Managed — MySQL is already running, I will supply the DSN"
        case "$mysql_choice" in
            Install*)
                STORE_MODE="install"
                local _gen_pw
                _gen_pw=$(head -c 16 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 20)
                STORE_DSN="mysql://teamster:${_gen_pw}@localhost:3306/teamster"
                info "MySQL will be installed. Generated DSN: $STORE_DSN"
                dlog INFO wizard.interview "decided STORE_MODE=install dsn=\"$STORE_DSN\""
                ;;
            External*)
                STORE_MODE="external"
                local _dsn_default="mysql://teamster:CHANGEME@localhost:3306/teamster"
                ask STORE_DSN "MySQL DSN" "$_dsn_default"
                dlog INFO wizard.interview "decided STORE_MODE=external"
                ;;
            Managed*)
                STORE_MODE="managed"
                local _dsn_default="mysql://teamster:CHANGEME@localhost:3306/teamster"
                ask STORE_DSN "MySQL DSN" "$_dsn_default"
                dlog INFO wizard.interview "decided STORE_MODE=managed"
                ;;
        esac
    fi
    dlog INFO  wizard.interview "decided STORE_DSN=\"$STORE_DSN\" STORE_MODE=$STORE_MODE"
    if [[ -n "$STORE_DSN" && "$STORE_MODE" != "install" ]]; then
        info "Checking MySQL connectivity..."
        local _dsn_host _dsn_port
        _dsn_host=$(echo "$STORE_DSN" | sed -n 's|mysql://[^@]*@\([^:]*\):\([0-9]*\)/.*|\1|p')
        _dsn_port=$(echo "$STORE_DSN" | sed -n 's|mysql://[^@]*@\([^:]*\):\([0-9]*\)/.*|\2|p')
        if [[ -n "$_dsn_host" ]] && [[ -n "$_dsn_port" ]]; then
            if timeout 3 bash -c "echo >/dev/tcp/$_dsn_host/$_dsn_port" 2>/dev/null; then
                info "  ${C_GREEN}✓${C_RESET} MySQL reachable at $_dsn_host:$_dsn_port"
            else
                warn "  MySQL not reachable at $_dsn_host:$_dsn_port — install will proceed but hookd may fail to start"
            fi
        fi
    fi

    ask ENV_LABEL "Environment label" "production"

    # Plane integration (opt-in)
    section "Plane integration (optional)"
    info "Plane is a project management tool. Connecting it lets Teamster sync"
    info "work items to/from a Plane board. Leave unset to disable Plane sync."
    local _plane_yn=0
    ask_yn _plane_yn "Connect a Plane board?" N
    if [[ "$_plane_yn" -eq 1 ]]; then
        ask PLANE_URL "Plane URL (e.g. https://plane.example.com)" ""
        ask PLANE_API_KEY "Plane API key" ""
        dlog INFO wizard.interview "plane_integration PLANE_URL=\"$PLANE_URL\" PLANE_API_KEY=<redacted>"
    else
        dlog INFO wizard.interview "plane_integration skipped"
    fi

    # Relay / replication (opt-in, hub mode only)
    section "Replication relay (optional)"
    info "Set up event relay to mirror this hub's data to a replica host."
    info "The relay tails events.jsonl and POSTs each line to a remote hookd."
    local _relay_yn=0
    local _relay_default="N"
    if [[ -n "$DETECTED_RELAY_TARGET" || -n "$DETECTED_REPL_PUSH_REMOTE" ]]; then
        _relay_default="Y"
        info ""
        info "Existing relay configuration detected:"
        [[ -n "$DETECTED_RELAY_TARGET" ]]     && info "  relay target:     ${C_GREEN}$DETECTED_RELAY_TARGET${C_RESET}"
        [[ -n "$DETECTED_REPL_PUSH_REMOTE" ]] && info "  repl-push remote: ${C_GREEN}$DETECTED_REPL_PUSH_REMOTE${C_RESET}"
        info ""
    fi
    ask_yn _relay_yn "Set up event relay to a replica?" "$_relay_default"
    if [[ "$_relay_yn" -eq 1 ]]; then
        RELAY_MODE="install"
        ask RELAY_TARGET "Replica hookd URL (e.g. http://replica:9125/event)" "${DETECTED_RELAY_TARGET}"
        ask REPL_PUSH_REMOTE "Repl-push SCP destination (e.g. user@replica)" "${DETECTED_REPL_PUSH_REMOTE}"
        dlog INFO wizard.interview "relay_integration RELAY_MODE=install RELAY_TARGET=\"$RELAY_TARGET\" REPL_PUSH_REMOTE=\"$REPL_PUSH_REMOTE\""
    else
        RELAY_MODE="none"
        dlog INFO wizard.interview "relay_integration skipped"
    fi

    # Replica mode (optional) — read-only hookd for hosts that receive data.
    # A host that relays events to a replica is a hub, not a replica itself.
    if [[ "$RELAY_MODE" == "install" ]]; then
        HOOKD_READ_ONLY=0
        dlog INFO wizard.interview "hookd_read_only=0 reason=relay_active (hub cannot be replica)"
    else
        section "Replica mode (optional)"
        info "If this host is a read-only replica (receives data from a hub),"
        info "hookd should run in read-only mode (rejects MCP/telemetry/drain)."
        local _ro_yn=0
        ask_yn _ro_yn "Run hookd in read-only mode?" N
        if [[ "$_ro_yn" -eq 1 ]]; then
            HOOKD_READ_ONLY=1
            dlog INFO wizard.interview "hookd_read_only=1"
        else
            HOOKD_READ_ONLY=0
            dlog INFO wizard.interview "hookd_read_only=0"
        fi
    fi

    # Build-from-source and retention — only when mode=install for that service
    if [[ "$PROMETHEUS_MODE" == "install" ]]; then
        ask RETENTION "Prometheus retention" "7d"
        ask_yn PROMETHEUS_BUILD_FROM_SRC "Build prometheus from source?" N
    fi
    if [[ "$OTELCOL_MODE" == "install" ]]; then
        ask_yn OTELCOL_BUILD_FROM_SRC "Build otelcol from source?" N
    fi
    if [[ "$GRAFANA_MODE" == "install" ]]; then
        ask_yn GRAFANA_BUILD_FROM_SRC "Build grafana from source? (heavy — usually no)" N
    fi

    ask_yn AUTO_START "After install, start managed services?" Y
    dlog INFO  wizard.interview "auto_start AUTO_START=$AUTO_START"
}

# ──────────────────────────────────────────────────────────────────────────
# Per-service resolution (operator-language)
# ──────────────────────────────────────────────────────────────────────────
#
# This script does NOT modify settings.json. For each service (otelcol,
# prometheus, grafana) the interview asks the operator what they want; answers
# are stored as <SERVICE>_MODE and optionally <SERVICE>_ENDPOINT globals, then
# emitted as --<service>-mode=<mode> flags to lib/installrunner.sh (the sole
# mutator).
# See [[wizard-installer-boundary]].

# default_for_service → sensible default URL for external/managed URL prompts
# Uses detected URL when available (from yaml/systemctl/TCP probe); falls back
# to standard system ports (not Teamster install-mode offset ports).
default_for_service() {
    local svc="$1"
    local detected
    detected="$(detected_value_for_service "$svc")"
    if [[ -n "$detected" ]]; then
        printf '%s' "$detected"
        return
    fi
    case "$svc" in
        otelcol)    printf 'http://localhost:4317'  ;;
        prometheus) printf 'http://localhost:9090'  ;;
        grafana)    printf 'http://localhost:3000'  ;;
        *)          printf ''                        ;;
    esac
}

# detected_value_for_service → detected URL, if any
detected_value_for_service() {
    case "$1" in
        otelcol)    printf '%s' "$DETECTED_OTEL_ENDPOINT" ;;
        prometheus) printf '%s' "$DETECTED_PROMETHEUS_URL" ;;
        grafana)    printf '%s' "$DETECTED_GRAFANA_URL" ;;
        *)          printf '' ;;
    esac
}

# detected_mode_for_service → prior mode from yaml, if any
detected_mode_for_service() {
    case "$1" in
        otelcol)    printf '%s' "$DETECTED_OTEL_MODE" ;;
        prometheus) printf '%s' "$DETECTED_PROMETHEUS_MODE" ;;
        grafana)    printf '%s' "$DETECTED_GRAFANA_MODE" ;;
        *)          printf '' ;;
    esac
}

# set_endpoint_var SERVICE VALUE — assigns the right endpoint global
set_endpoint_var() {
    case "$1" in
        otelcol)    OTELCOL_ENDPOINT="$2"    ;;
        prometheus) PROMETHEUS_ENDPOINT="$2" ;;
        grafana)    GRAFANA_ENDPOINT="$2"    ;;
    esac
}

# set_mode_var SERVICE MODE — assigns the right mode global
set_mode_var() {
    case "$1" in
        otelcol)    OTELCOL_MODE="$2"    ;;
        prometheus) PROMETHEUS_MODE="$2" ;;
        grafana)    GRAFANA_MODE="$2"    ;;
    esac
}

validate_service_url() {
    local svc="$1" url="$2"
    [[ -z "$url" ]] && return
    local health_path=""
    case "$svc" in
        prometheus) health_path="/-/ready" ;;
        grafana)    health_path="/api/health" ;;
        otelcol)    return ;;
    esac
    [[ -z "$health_path" ]] && return
    info "Validating $svc at $url..."
    if curl -sf --max-time 3 "${url}${health_path}" >/dev/null 2>&1; then
        info "  ${C_GREEN}✓${C_RESET} $svc healthy"
        dlog INFO wizard.validate "service=$svc url=\"$url\" result=ok"
    else
        warn "  $svc not responding at ${url}${health_path}"
        dlog WARN wizard.validate "service=$svc url=\"$url\" result=unreachable"
    fi
}

# resolve_services — runs the per-service mode interview for hub mode
resolve_services() {
    local _DTRACE_FN="resolve_services"
    dtrace_enter "$_DTRACE_FN" merge
    trap 'dtrace_exit "$_DTRACE_FN" merge' RETURN
    info ""
    info "Monitoring services — for each: install, point at existing, or skip."

    local svc
    for svc in otelcol prometheus grafana; do
        _resolve_one_service "$svc"
    done
}

# _resolve_one_service SERVICE — interview for one monitoring service.
# Sets <SERVICE>_MODE and optionally <SERVICE>_ENDPOINT.
_resolve_one_service() {
    local svc="$1"
    local detected
    detected="$(detected_value_for_service "$svc")"
    local prior_mode
    prior_mode="$(detected_mode_for_service "$svc")"
    local default_url
    default_url="$(default_for_service "$svc")"

    # Default for no-detected branch: install=1, external=2, managed=3, none=4
    # Adjust based on prior mode from yaml so re-runs don't change the mode.
    local install_default=1
    case "$prior_mode" in
        external) install_default=2 ;;
        managed)  install_default=3 ;;
        none)     install_default=4 ;;
    esac

    dlog INFO  wizard.merge "service_probe svc=$svc detected=\"$detected\" prior_mode=\"$prior_mode\" default_url=\"$default_url\" install_default=$install_default"

    info ""
    if [[ -n "$detected" ]]; then
        info "Detected existing $svc at $detected"
        # detected branch: managed=1, install=2, external=3, none=4
        # Pick default based on prior mode.
        local detected_default=1
        case "$prior_mode" in
            install)  detected_default=2 ;;
            external) detected_default=3 ;;
            none)     detected_default=4 ;;
        esac
        local choice=""
        ask_choice choice "$svc mode:" "$detected_default" \
            "Managed — Teamster tracks it at this address" \
            "Install — replace with Teamster-managed $svc" \
            "External — point at a different server I'll supply the URL for" \
            "None — ignore it"
        case "$choice" in
            Managed*)
                set_mode_var "$svc" "managed"
                set_endpoint_var "$svc" "$detected"
                dlog INFO  wizard.merge "service_decision svc=$svc mode=managed endpoint=\"$detected\""
                validate_service_url "$svc" "$detected"
                ;;
            Install*)
                set_mode_var "$svc" "install"
                dlog INFO  wizard.merge "service_decision svc=$svc mode=install"
                ;;
            External*)
                local newurl=""
                ask newurl "$svc URL" "$default_url"
                if [[ -z "$newurl" ]]; then
                    err "External mode requires a URL."
                    dlog ERROR wizard.merge "service_decision svc=$svc mode=external rejected reason=\"empty url\""
                    exit 1
                fi
                set_mode_var "$svc" "external"
                set_endpoint_var "$svc" "$newurl"
                dlog INFO  wizard.merge "service_decision svc=$svc mode=external endpoint=\"$newurl\""
                validate_service_url "$svc" "$newurl"
                ;;
            None*)
                set_mode_var "$svc" "none"
                dlog INFO  wizard.merge "service_decision svc=$svc mode=none"
                ;;
        esac
    else
        info "No $svc server configured."
        local choice=""
        ask_choice choice "$svc mode:" "$install_default" \
            "Install — Teamster downloads and manages $svc [default]" \
            "External — point at an existing $svc I'll supply the URL for" \
            "Managed — I installed $svc myself, Teamster just tracks it" \
            "None — skip $svc entirely"
        case "$choice" in
            Install*)
                set_mode_var "$svc" "install"
                dlog INFO  wizard.merge "service_decision svc=$svc mode=install"
                ;;
            External*)
                local newurl=""
                ask newurl "$svc URL" "$default_url"
                if [[ -z "$newurl" ]]; then
                    err "External mode requires a URL."
                    dlog ERROR wizard.merge "service_decision svc=$svc mode=external rejected reason=\"empty url\""
                    exit 1
                fi
                set_mode_var "$svc" "external"
                set_endpoint_var "$svc" "$newurl"
                dlog INFO  wizard.merge "service_decision svc=$svc mode=external endpoint=\"$newurl\""
                validate_service_url "$svc" "$newurl"
                ;;
            Managed*)
                local newurl=""
                ask newurl "$svc URL" "$default_url"
                set_mode_var "$svc" "managed"
                [[ -n "$newurl" ]] && set_endpoint_var "$svc" "$newurl"
                dlog INFO  wizard.merge "service_decision svc=$svc mode=managed endpoint=\"${newurl:-<empty>}\""
                [[ -n "$newurl" ]] && validate_service_url "$svc" "$newurl"
                ;;
            None*)
                set_mode_var "$svc" "none"
                dlog INFO  wizard.merge "service_decision svc=$svc mode=none"
                ;;
        esac
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# Command generation
# ──────────────────────────────────────────────────────────────────────────

build_install_args() {
    local _DTRACE_FN="build_install_args"
    dtrace_enter "$_DTRACE_FN" build
    trap 'dtrace_exit "$_DTRACE_FN" build' RETURN
    INSTALL_ARGS=()

    dlog DEBUG wizard.build "inputs INSTALL_MODE=$INSTALL_MODE BASEDIR=$BASEDIR HOOKD_MODE=\"$HOOKD_MODE\" HOOKD_ENDPOINT=\"$HOOKD_ENDPOINT\" STORE_MODE=$STORE_MODE STORE_DSN=\"$STORE_DSN\" OTELCOL_MODE=$OTELCOL_MODE PROMETHEUS_MODE=$PROMETHEUS_MODE GRAFANA_MODE=$GRAFANA_MODE ENV_LABEL=$ENV_LABEL RETENTION=\"$RETENTION\" WIRE=$WIRE"

    if [[ "$INSTALL_MODE" == "client" ]]; then
        INSTALL_ARGS+=("--hookd-mode=external")
        INSTALL_ARGS+=("--hookd-endpoint=$HOOKD_ENDPOINT")
        INSTALL_ARGS+=("--basedir=$BASEDIR")
        [[ $WIRE -eq 1 ]] && INSTALL_ARGS+=("--wire")
        dlog INFO  wizard.build "args_built mode=client argv=[${INSTALL_ARGS[*]}]"
        return
    fi

    # Hub mode
    INSTALL_ARGS+=("--hookd-mode=$HOOKD_MODE")
    INSTALL_ARGS+=("--basedir=$BASEDIR")

    [[ -n "$STORE_MODE" ]]      && INSTALL_ARGS+=("--store-mode=$STORE_MODE")
    [[ -n "$STORE_DSN" ]]       && INSTALL_ARGS+=("--store-dsn=$STORE_DSN")
    [[ -n "$OTELCOL_MODE" ]]    && INSTALL_ARGS+=("--otelcol-mode=$OTELCOL_MODE")
    [[ -n "$PROMETHEUS_MODE" ]] && INSTALL_ARGS+=("--prometheus-mode=$PROMETHEUS_MODE")
    [[ -n "$GRAFANA_MODE" ]]    && INSTALL_ARGS+=("--grafana-mode=$GRAFANA_MODE")
    [[ -n "$OTELCOL_ENDPOINT" ]]    && INSTALL_ARGS+=("--otelcol-endpoint=$OTELCOL_ENDPOINT")
    [[ -n "$PROMETHEUS_ENDPOINT" ]] && INSTALL_ARGS+=("--prometheus-endpoint=$PROMETHEUS_ENDPOINT")
    [[ -n "$GRAFANA_ENDPOINT" ]]    && INSTALL_ARGS+=("--grafana-endpoint=$GRAFANA_ENDPOINT")

    [[ "$OTELCOL_BUILD_FROM_SRC" -eq 1 ]]    && INSTALL_ARGS+=("--otelcol-build-from-src")
    [[ "$PROMETHEUS_BUILD_FROM_SRC" -eq 1 ]] && INSTALL_ARGS+=("--prometheus-build-from-src")
    [[ "$GRAFANA_BUILD_FROM_SRC" -eq 1 ]]    && INSTALL_ARGS+=("--grafana-build-from-src")

    [[ -n "$RELAY_MODE" && "$RELAY_MODE" != "none" ]] && INSTALL_ARGS+=("--relay-mode=$RELAY_MODE")
    [[ -n "$RELAY_TARGET" ]]      && INSTALL_ARGS+=("--relay-target=$RELAY_TARGET")
    [[ -n "$REPL_PUSH_REMOTE" ]]  && INSTALL_ARGS+=("--repl-push-remote=$REPL_PUSH_REMOTE")
    [[ "$HOOKD_READ_ONLY" -eq 1 ]] && INSTALL_ARGS+=("--hookd-read-only")

    [[ -n "$RETENTION" ]]     && INSTALL_ARGS+=("--prometheus-retention=$RETENTION")
    [[ -n "$ENV_LABEL" ]]     && INSTALL_ARGS+=("--env=$ENV_LABEL")
    [[ -n "$PLANE_URL" ]]     && INSTALL_ARGS+=("--plane-url=$PLANE_URL")
    [[ -n "$PLANE_API_KEY" ]] && INSTALL_ARGS+=("--plane-api-key=$PLANE_API_KEY")
    [[ $WIRE -eq 1 ]]         && INSTALL_ARGS+=("--wire")
    dlog INFO  wizard.build "args_built mode=hub argv=[${INSTALL_ARGS[*]}]"
}

# ──────────────────────────────────────────────────────────────────────────
# Summary + execution
# ──────────────────────────────────────────────────────────────────────────

show_summary() {
    local _DTRACE_FN="show_summary"
    dtrace_enter "$_DTRACE_FN" exec
    trap 'dtrace_exit "$_DTRACE_FN" exec' RETURN
    # Generated install command verbatim — the canonical record of what wizard
    # decided. Bug #20 smoking-gun lives in this line for the no-tee case.
    dlog INFO  wizard.build "generated_install_command argv=[${INSTALL_ARGS[*]}]"
    banner "Wizard summary"
    info "Install mode:    $INSTALL_MODE"
    [[ "$INSTALL_MODE" == "client" ]] && info "Hub endpoint:    $HOOKD_ENDPOINT"
    info "Basedir:         $BASEDIR"
    if [[ "$INSTALL_MODE" == "hub" ]]; then
        info "hookd mode:      $HOOKD_MODE"
        info ""
        local _store_display="${STORE_MODE:-<unset>}"
        if [[ -n "$STORE_DSN" ]]; then
            # Mask password in DSN for display
            local _dsn_masked
            _dsn_masked="$(printf '%s' "$STORE_DSN" | sed 's|\(://[^:]*:\)[^@]*\(@\)|\1***\2|')"
            _store_display="${STORE_MODE} (${_dsn_masked})"
        fi
        info "  hookd mode:      $HOOKD_MODE"
        info "  store mode:      $_store_display"
        info "  otelcol mode:    ${OTELCOL_MODE:-<unset>}${OTELCOL_ENDPOINT:+ ($OTELCOL_ENDPOINT)}"
        info "  prometheus mode: ${PROMETHEUS_MODE:-<unset>}${PROMETHEUS_ENDPOINT:+ ($PROMETHEUS_ENDPOINT)}"
        info "  grafana mode:    ${GRAFANA_MODE:-<unset>}${GRAFANA_ENDPOINT:+ ($GRAFANA_ENDPOINT)}"
        info "  relay mode:      ${RELAY_MODE:-none}${RELAY_TARGET:+ ($RELAY_TARGET)}"
        info "  hookd read-only: $([[ "$HOOKD_READ_ONLY" -eq 1 ]] && echo yes || echo no)"
        info ""
        info "Env label:       $ENV_LABEL"
        [[ -n "$RETENTION" ]] && info "Retention:       $RETENTION"
        info "Auto-start:      $AUTO_START"
        if [[ -n "$PLANE_URL" ]]; then
            info "Plane URL:       $PLANE_URL"
            info "Plane API key:   <set>"
        else
            info "Plane:           (not configured)"
        fi
    fi
    echo
    info "Generated installrunner command:"
    info ""
    # Pretty-print the args, one per line after lib/installrunner.sh
    printf '    ./lib/installrunner.sh'
    for a in "${INSTALL_ARGS[@]}"; do
        printf ' \\\n        %s' "$a"
    done
    printf '\n\n'
}

execute_or_print() {
    local _DTRACE_FN="execute_or_print"
    dtrace_enter "$_DTRACE_FN" exec
    trap 'dtrace_exit "$_DTRACE_FN" exec' RETURN

    local tm_bin="${BASEDIR}/bin/teamster"
    local tm_bin_state="missing"
    [[ -e "$tm_bin" ]] && tm_bin_state="present"
    [[ -x "$tm_bin" ]] && tm_bin_state="executable"
    dlog DEBUG wizard.exec "state INSTALL_MODE=$INSTALL_MODE AUTO_START=$AUTO_START BASEDIR=$BASEDIR tm_bin=$tm_bin tm_bin_state=$tm_bin_state REPO_DIR=$REPO_DIR INSTALL_ARGS=[${INSTALL_ARGS[*]}] DEBUG_LOG=\"$DEBUG_LOG\""

    local _do_run=0
    ask_yn _do_run "Run it now?" Y
    if [[ "$_do_run" == "0" ]]; then
        info ""
        info "Copy-pasteable command:"
        local line="./lib/installrunner.sh"
        for a in "${INSTALL_ARGS[@]}"; do
            line="$line $a"
        done
        echo "  $line"
        info ""
        info "Done — install not executed."
        dlog INFO  wizard.exec "branch=print reason=\"operator declined run\""
        exit 0
    fi

    cd "$REPO_DIR" || { err "cannot cd to repo dir $REPO_DIR"; dlog ERROR wizard.exec "cd_failed path=$REPO_DIR"; exit 1; }

    # If --debug-log is set, forward a SIBLING path to lib/installrunner.sh so
    # they write separate files (`wizard.log` + `install.log`) in the same slug
    # dir. Coordinated with @installer: lib/installrunner.sh AND teamster-install
    # both append to install.log; this script writes only to its own DEBUG_LOG.
    local install_argv=("${INSTALL_ARGS[@]}")
    local install_debug_log=""
    if [[ -n "$DEBUG_LOG" ]]; then
        install_debug_log="${DEBUG_LOG_DIR}/install.log"
        install_argv+=("--debug-log=$install_debug_log")
        dlog DEBUG wizard.exec "forwarding --debug-log=$install_debug_log to lib/installrunner.sh (sibling of $DEBUG_LOG)"
    fi

    if [[ "$AUTO_START" == "1" ]]; then
        # Non-exec branch: install.sh stays alive so it can run `teamster start`
        # after lib/installrunner.sh returns.
        dlog INFO  wizard.exec "branch=non-exec reason=\"AUTO_START=1\" argv=[${install_argv[*]}]"
        info ""
        info "Running lib/installrunner.sh (auto-start enabled — will follow up with \`teamster start\`)..."
        info ""
        run_subproc wizard.subproc "$REPO_DIR/lib/installrunner.sh" "${install_argv[@]}"
        local rc=$?
        if [[ $rc -ne 0 ]]; then
            err "lib/installrunner.sh failed (rc=$rc) — not running auto-start"
            dlog ERROR wizard.exec "installrunner_failed rc=$rc — not running auto-start"
            exit 1
        fi
        # Load port env vars written by lib/installrunner.sh into this process so teamster
        # start inherits dynamic port assignments from settings.json.
        if [[ -f "$HOME/.claude/settings.json" ]]; then
            local _port_vars
            _port_vars=$(python3 -c "
import json, sys
try:
    d = json.load(open('$HOME/.claude/settings.json'))
    env = d.get('env', {})
    for k in ['TEAMSTER_PROMETHEUS_PORT', 'TEAMSTER_GRAFANA_PORT', 'TEAMSTER_OTEL_GRPC_PORT', 'TEAMSTER_OTEL_HTTP_PORT', 'TEAMSTER_HOOK_SERVER_PORT']:
        v = env.get(k, '')
        if v:
            print(f'export {k}={v}')
except: pass
" 2>/dev/null)
            if [[ -n "$_port_vars" ]]; then
                eval "$_port_vars"
                dlog DEBUG wizard.exec "loaded port env vars from settings.json"
            fi
        fi
        # Re-probe tm_bin after lib/installrunner.sh has had a chance to write it.
        tm_bin_state="missing"
        [[ -e "$tm_bin" ]] && tm_bin_state="present"
        [[ -x "$tm_bin" ]] && tm_bin_state="executable"
        dlog DEBUG wizard.exec "post_install state tm_bin=$tm_bin tm_bin_state=$tm_bin_state"
        if [[ -x "$tm_bin" ]]; then
            local start_args=("start")
            [[ -n "$ENV_LABEL" ]]  && start_args+=("--env=$ENV_LABEL")
            [[ -n "$RETENTION" ]]  && start_args+=("--prometheus-retention=$RETENTION")
            info ""
            info "Running: $tm_bin ${start_args[*]}"
            run_subproc wizard.subproc "$tm_bin" "${start_args[@]}"
            local trc=$?
            if [[ $trc -ne 0 ]]; then
                warn "teamster start returned non-zero (rc=$trc)"
                dlog WARN  wizard.exec "teamster_start_nonzero rc=$trc"
            else
                dlog INFO  wizard.exec "teamster_start ok rc=0"
            fi
        else
            warn "$tm_bin not found — skipping auto-start"
            dlog ERROR wizard.exec "teamster_start_skipped reason=\"$tm_bin not executable\" tm_bin_state=$tm_bin_state"
        fi
        info ""
        info "Done."
        dlog INFO  wizard.exec "done branch=non-exec"
    else
        # Exec branch — this PID is replaced by lib/installrunner.sh. No more
        # log lines after this point; lib/installrunner.sh writes to its own
        # sibling log file ($install_debug_log) if --debug-log was forwarded.
        dlog INFO  wizard.exec "branch=exec reason=\"AUTO_START=$AUTO_START\" exec_handoff_to=lib/installrunner.sh install_debug_log=\"$install_debug_log\" argv=[${install_argv[*]}]"
        info ""
        info "Handing off to lib/installrunner.sh..."
        exec "$REPO_DIR/lib/installrunner.sh" "${install_argv[@]}"
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# Main
# ──────────────────────────────────────────────────────────────────────────

main() {
    parse_flags "$@"
    local _DTRACE_FN="main"
    dtrace_enter "$_DTRACE_FN" main
    trap 'dtrace_exit "$_DTRACE_FN" main' RETURN
    dlog INFO  wizard.main "start REPO_DIR=$REPO_DIR SETTINGS_FILE=$SETTINGS_FILE bash=$BASH_VERSION user=${USER:-?} host=$(hostname 2>/dev/null || echo ?)"

    if [[ "$(uname -s)" == "Darwin" ]]; then
        printf -- '\033[1;31mERROR: macOS is not supported as a Teamster hub.\033[0m\n' >&2
        printf -- 'This installer only runs on apt-based Linux (Debian/Ubuntu); do not run it on the Mac.\n' >&2
        printf -- '\n' >&2
        printf -- 'macOS is supported as a Teamster *remote* only. To enroll this Mac, go to your\n' >&2
        printf -- 'Teamster server (the Linux hub) and run the remote installer there, pointing it\n' >&2
        printf -- 'at this Mac over SSH:\n' >&2
        printf -- '\n' >&2
        printf -- '    # on the Teamster server (NOT on the Mac):\n' >&2
        printf -- '    teamster install-remote <user>@<this-mac>\n' >&2
        printf -- '\n' >&2
        printf -- 'See docs/specs/REMOTE-INSTALL.md for details.\n' >&2
        exit 1
    fi

    banner "Teamster installer"
    info "Repo:      $REPO_DIR"
    info "Settings:  $SETTINGS_FILE"
    info ""
    info "This installer probes local state, walks you through install choices,"
    info "and then runs (or prints) the lib/installrunner.sh command. This script"
    info "mutates NOTHING on the host — your settings.json conflict decisions are"
    info "emitted as --settings-* flags for lib/installrunner.sh to enact under --wire."

    detect_json_tool
    probe_settings
    probe_basedir "$HOME/teamster"
    probe_yaml "$HOME/teamster"
    probe_relay "$HOME/teamster"
    probe_ports
    probe_systemd
    probe_prereqs
    probe_sudo

    interview

    # If the operator chose a different basedir, re-probe that location too so
    # they know what's there.
    if [[ -n "$BASEDIR" && "$BASEDIR" != "$HOME/teamster" ]]; then
        probe_basedir "$BASEDIR"
    fi

    build_install_args
    show_summary
    execute_or_print
}

main "$@"
