#!/usr/bin/env bash
# JMeter Perf Lab — run Apache JMeter non-GUI against a plan.jmx via Docker (default).
# Host bin is opt-in: OPA_PERF_ALLOW_HOST_JMETER=1.
# Usage:
#   ./scripts/jmeter-run.sh /path/to/plan.jmx [/path/to/results.jtl] [LOAD_RUN_ID]
set -euo pipefail
JMX="${1:?jmx path required}"
JTL="${2:-$(dirname "$JMX")/results.jtl}"
LOAD_RUN_ID="${3:-${OPA_LOAD_RUN_ID:-}}"
DIR="$(cd "$(dirname "$JMX")" && pwd)"
BASE="$(basename "$JMX")"
JTL_BASE="$(basename "$JTL")"
IMAGE="${OPA_JMETER_IMAGE:-justb4/jmeter:5.5}"
BIN="${OPA_JMETER_BIN:-jmeter}"
NETWORK_ARGS=()
if [[ -n "${OPA_JMETER_NETWORK:-}" ]]; then
  NETWORK_ARGS=(--network "$OPA_JMETER_NETWORK")
fi
LIMIT_ARGS=()
if [[ -n "${OPA_JMETER_CPUS:-}" ]]; then
  LIMIT_ARGS+=(--cpus "$OPA_JMETER_CPUS")
fi
if [[ -n "${OPA_JMETER_MEMORY:-}" ]]; then
  LIMIT_ARGS+=(--memory "$OPA_JMETER_MEMORY")
fi

ARGS=(-n -t "$JMX" -l "$JTL" -j "$(dirname "$JTL")/jmeter.log")
if [[ -n "$LOAD_RUN_ID" ]]; then
  ARGS+=(-JLOAD_RUN_ID="$LOAD_RUN_ID")
fi

# Production path: Docker
if command -v docker >/dev/null 2>&1; then
  exec docker run --rm \
    "${NETWORK_ARGS[@]}" \
    "${LIMIT_ARGS[@]}" \
    -v "$DIR:/jmeter" \
    -e JVM_ARGS=-Djava.awt.headless=true \
    "$IMAGE" \
    -n -t "/jmeter/$BASE" -l "/jmeter/$JTL_BASE" -j /jmeter/jmeter.log \
    ${LOAD_RUN_ID:+-JLOAD_RUN_ID=$LOAD_RUN_ID}
fi

# Dev-only host bin
if [[ "${OPA_PERF_ALLOW_HOST_JMETER:-}" == "1" ]]; then
  if [[ -n "${OPA_JMETER_BIN:-}" ]] || command -v jmeter >/dev/null 2>&1; then
    exec "$BIN" "${ARGS[@]}"
  fi
fi

echo "Apache JMeter Docker unavailable. Install docker for $IMAGE, or set OPA_PERF_ALLOW_HOST_JMETER=1 with OPA_JMETER_BIN." >&2
exit 127
