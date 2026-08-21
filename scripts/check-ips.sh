#!/usr/bin/env bash
# check-ips.sh - Verify every VPN proxy container egresses from a public IP.
#
# Usage:
#   GATEWAY_PORT=18090 make check-ips
#   PROXY_BASE_PORT=10801 PROXY_POOL_SIZE=3 bash scripts/check-ips.sh
#
# Ports are discovered from the gateway's live proxy list (accurate even when
# a recreated container shifted ports); falls back to a sequential range when
# the gateway is unreachable.
#
# Exit code 1 if any container is unreachable or has no IP.

set -euo pipefail

GATEWAY_HOST="${GATEWAY_HOST:-localhost}"
GATEWAY_PORT="${GATEWAY_PORT:-8082}"
BASE_PORT="${PROXY_BASE_PORT:-10801}"
POOL_SIZE="${PROXY_POOL_SIZE:-3}"
API="http://${GATEWAY_HOST}:${GATEWAY_PORT}/api/metrics"

PORTS=""

METRICS=$(curl -s --max-time 10 "${API}" 2>/dev/null || true)
if [ -n "${METRICS}" ]; then
  # Extract socks5 ports from the proxy list, dedupe, and sort
  PORTS=$(printf '%s\n' "${METRICS}" \
    | grep -oE '127\.0\.0\.1:[0-9]+' \
    | cut -d: -f2 \
    | sort -n -u)
  if [ -z "${PORTS}" ]; then
    echo "WARN: gateway reachable but no proxies in pool (${API})" >&2
  fi
fi

if [ -z "${PORTS}" ]; then
  echo "WARN: gateway unreachable at ${API}, probing ports ${BASE_PORT}..$((BASE_PORT + POOL_SIZE - 1))" >&2
  PORTS=""
  for ((i = 0; i < POOL_SIZE; i++)); do
    PORTS="${PORTS}$((BASE_PORT + i))"$'\n'
  done
fi

echo "Checking egress IP of proxies on ports: $(echo "${PORTS}" | tr '\n' ' ')"
echo "---------------------------------------------------------------------------"

declare -A SEEN
FAIL=0

for PORT in ${PORTS}; do
  if [ -z "${PORT}" ]; then continue; fi
  SOCKS="127.0.0.1:${PORT}"

  # Use icanhazip.com to get the egress IP
  IP=$(curl -s --max-time 20 --socks5-hostname "${SOCKS}" "https://icanhazip.com/" 2>/dev/null || true)
  IP=$(echo "${IP}" | tr -d '[:space:]')

  if [ -z "${IP}" ]; then
    echo "port ${PORT}: UNREACHABLE (no IP response) - check container is running"
    FAIL=1
    continue
  fi

  # Basic IP format validation
  if ! echo "${IP}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "port ${PORT}: INVALID IP format: ${IP}"
    FAIL=1
    continue
  fi

  echo "port ${PORT}: ip=${IP}"
done

echo "---------------------------------------------------------------------------"
if [ "${FAIL}" = "0" ]; then
  echo "OK: all proxies are reachable"
else
  echo "FAIL: issues detected (see above)"
  exit 1
fi
