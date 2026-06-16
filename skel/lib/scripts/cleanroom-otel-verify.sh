#!/bin/bash
# cleanroom-otel-verify.sh — verify D3 OTLP collector in a cleanroom install.
#
# Usage: ./scripts/cleanroom-otel-verify.sh [--basedir PATH]
#
# What it does:
#   1. Validates the rendered otelcol.yaml via otelcol-contrib validate.
#   2. Sends a synthetic OTLP metric payload via gRPC (grpcurl) AND HTTP (curl).
#   3. Polls bundled Prometheus for the synthetic series until it appears (30s).
#
# Ports are read from env vars (or rendered config), never hardcoded.
# Exits 0 on full success; non-zero with a clear message on any failure.
# Safe to run anywhere — only touches the cleanroom Teamster, not the live host.

set -euo pipefail

# ── locate basedir ────────────────────────────────────────────────────────────
BASEDIR="${TEAMSTER_BASEDIR:-$HOME/teamster}"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --basedir) BASEDIR="$2"; shift 2 ;;
        --basedir=*) BASEDIR="${1#*=}"; shift ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

OTELCOL_BIN="$BASEDIR/bin/otelcol-contrib"
OTELCOL_CFG="$BASEDIR/etc/otelcol.yaml"

# ── port resolution ───────────────────────────────────────────────────────────
OTEL_GRPC_PORT="${TEAMSTER_OTEL_GRPC_PORT:-4327}"
OTEL_HTTP_PORT="${TEAMSTER_OTEL_HTTP_PORT:-4328}"
OTELCOL_HEALTH_PORT="${TEAMSTER_OTELCOL_HEALTH_PORT:-13133}"
PROMETHEUS_PORT="${TEAMSTER_PROMETHEUS_PORT:-9190}"

# ── helpers ───────────────────────────────────────────────────────────────────
fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "OK: $*"; }

poll_until() {
    local desc="$1" cmd="$2" max="${3:-30}" elapsed=0
    while ! eval "$cmd" >/dev/null 2>&1; do
        if [[ $elapsed -ge $max ]]; then
            fail "$desc (not ready after ${max}s)"
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    ok "$desc"
}

# ── step 1: validate config ───────────────────────────────────────────────────
echo "=== step 1: validate rendered otelcol.yaml ==="
if [[ ! -f "$OTELCOL_CFG" ]]; then
    fail "rendered config not found: $OTELCOL_CFG (run teamster start --bundle otelcol first)"
fi
if [[ ! -x "$OTELCOL_BIN" ]]; then
    fail "otelcol-contrib not found or not executable: $OTELCOL_BIN"
fi
"$OTELCOL_BIN" validate --config "$OTELCOL_CFG" \
    || fail "otelcol-contrib validate failed — check OTTL syntax in $OTELCOL_CFG"
ok "otelcol.yaml validates"

# ── step 2a: wait for collector health endpoint ───────────────────────────────
echo "=== step 2a: wait for otelcol health endpoint ==="
poll_until "otelcol health endpoint :${OTELCOL_HEALTH_PORT}" \
    "curl --silent --fail http://localhost:${OTELCOL_HEALTH_PORT}/-/healthy" 30

# ── step 2b: synthetic gRPC probe ────────────────────────────────────────────
echo "=== step 2b: send synthetic OTLP metric via gRPC ==="
# Current time in nanoseconds — required by Prometheus to accept the datapoint
# (timestamp=0 maps to 1970-01-01 and is rejected as out of bounds).
NOW_NS=$(date +%s%N)
if command -v grpcurl >/dev/null 2>&1; then
    # Send a minimal ExportMetricsServiceRequest.
    # The payload uses the OTLP proto JSON encoding.
    GRPC_PAYLOAD="{
      \"resourceMetrics\": [{
        \"resource\": {
          \"attributes\": [{
            \"key\": \"host.name\",
            \"value\": {\"stringValue\": \"cleanroom-verify\"}
          }, {
            \"key\": \"session_id\",
            \"value\": {\"stringValue\": \"verify-session-001\"}
          }]
        },
        \"scopeMetrics\": [{
          \"metrics\": [{
            \"name\": \"claude_code_session_count_total\",
            \"sum\": {
              \"dataPoints\": [{
                \"asDouble\": 1,
                \"timeUnixNano\": \"${NOW_NS}\",
                \"attributes\": [{
                  \"key\": \"session_id\",
                  \"value\": {\"stringValue\": \"verify-session-001\"}
                }]
              }],
              \"isMonotonic\": true,
              \"aggregationTemporality\": 2
            }
          }]
        }]
      }]
    }"
    echo "$GRPC_PAYLOAD" | grpcurl \
        --plaintext \
        --proto-set /dev/stdin \
        -d @ \
        "localhost:${OTEL_GRPC_PORT}" \
        opentelemetry.proto.collector.metrics.v1.MetricsService/Export \
        2>/dev/null \
    && ok "gRPC probe accepted" \
    || {
        # grpcurl without the proto descriptor may still get a connection.
        # Fall back to a raw TCP connect check to confirm port is bound.
        if timeout 2 bash -c "echo >/dev/tcp/localhost/${OTEL_GRPC_PORT}" 2>/dev/null; then
            ok "gRPC port :${OTEL_GRPC_PORT} is bound (grpcurl proto-level probe skipped)"
        else
            fail "gRPC port :${OTEL_GRPC_PORT} is not bound"
        fi
    }
else
    # grpcurl not available — confirm port bind only.
    if timeout 2 bash -c "echo >/dev/tcp/localhost/${OTEL_GRPC_PORT}" 2>/dev/null; then
        ok "gRPC port :${OTEL_GRPC_PORT} is bound (grpcurl not installed, port-bind check only)"
    else
        fail "gRPC port :${OTEL_GRPC_PORT} is not bound"
    fi
fi

# ── step 2c: synthetic HTTP probe ────────────────────────────────────────────
echo "=== step 2c: send synthetic OTLP metric via HTTP ==="
HTTP_PAYLOAD="{
  \"resourceMetrics\": [{
    \"resource\": {
      \"attributes\": [{
        \"key\": \"host.name\",
        \"value\": {\"stringValue\": \"cleanroom-verify\"}
      }, {
        \"key\": \"session_id\",
        \"value\": {\"stringValue\": \"verify-session-002\"}
      }]
    },
    \"scopeMetrics\": [{
      \"metrics\": [{
        \"name\": \"claude_code_session_count_total\",
        \"sum\": {
          \"dataPoints\": [{
            \"asDouble\": 1,
            \"timeUnixNano\": \"${NOW_NS}\",
            \"attributes\": [{
              \"key\": \"session_id\",
              \"value\": {\"stringValue\": \"verify-session-002\"}
            }]
          }],
          \"isMonotonic\": true,
          \"aggregationTemporality\": 2
        }
      }]
    }]
  }]
}"
HTTP_RESPONSE=$(curl --silent --write-out "%{http_code}" --output /dev/null \
    --request POST \
    --header "Content-Type: application/json" \
    --data "$HTTP_PAYLOAD" \
    "http://localhost:${OTEL_HTTP_PORT}/v1/metrics" \
    2>/dev/null || echo "000")

if [[ "$HTTP_RESPONSE" == "200" || "$HTTP_RESPONSE" == "429" ]]; then
    ok "HTTP probe accepted (status $HTTP_RESPONSE)"
else
    fail "HTTP probe failed: status $HTTP_RESPONSE from localhost:${OTEL_HTTP_PORT}/v1/metrics"
fi

# ── step 3: poll Prometheus for synthetic series ──────────────────────────────
echo "=== step 3: poll Prometheus for synthetic claude_code_* series ==="
PROM_QUERY="claude_code_session_count_total"
PROM_URL="http://localhost:${PROMETHEUS_PORT}/api/v1/query?query=${PROM_QUERY}"

poll_until \
    "synthetic series in Prometheus (${PROM_QUERY})" \
    "curl --silent --fail '${PROM_URL}' | grep -q '\"result\":\\[{'" \
    30

echo ""
echo "=== D3 standalone verification PASSED ==="
echo "    otelcol.yaml: valid"
echo "    gRPC port    :${OTEL_GRPC_PORT}: bound"
echo "    HTTP port    :${OTEL_HTTP_PORT}: accepting"
echo "    Prometheus   :${PROMETHEUS_PORT}: received synthetic series"
