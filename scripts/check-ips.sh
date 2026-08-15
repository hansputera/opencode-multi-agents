#!/usr/bin/env bash
# check-ips.sh - Verify every WARP proxy container egresses from a unique public IP.
#
# Usage:
#   GATEWAY_PORT=18090 make check-ips
#   PROXY_BASE_PORT=10801 PROXY_POOL_SIZE=3 bash scripts/check-ips.sh
#
# Ports are discovered from the gateway's live proxy list (accurate even when
# a recreated container shifted ports); falls back to a sequential range when
# the gateway is unreachable.
#
# Exit code 1 if any container is unreachable, has no IP, or shares an IP.

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

  TRACE=$(curl -s --max-time 20 --socks5-hostname "${SOCKS}" "https://cloudflare.com/cdn-cgi/trace" 2>/dev/null || true)
  IP=$(printf '%s\n' "${TRACE}" | grep '^ip=' | cut -d= -f2)
  WARP=$(printf '%s\n' "${TRACE}" | grep '^warp=' | cut -d= -f2)

  # IPv4 view (WARP NAT64 maps v4 traffic onto the same per-account address)
  IP4=$(curl -4 -s --max-time 20 --socks5-hostname "${SOCKS}" "https://ifconfig.me/ip" 2>/dev/null || true)

  if [ -z "${IP}" ]; then
    echo "port ${PORT}: UNREACHABLE (no trace response) - check container is running"
    FAIL=1
    continue
  fi
  if [ "${WARP}" != "on" ]; then
    echo "port ${PORT}: WARP OFF (warp=${WARP:-none})"
    FAIL=1
  fi
  if [ -z "${IP4}" ]; then
    echo "port ${PORT}: ipv4 ifconfig.me UNREACHABLE"
    FAIL=1
  fi

  if [ -n "${SEEN[${IP}]:-}" ]; then
    echo "port ${PORT}: DUPLICATE IP ${IP} (also used by port ${SEEN[${IP}]})"
    FAIL=1
  else
    SEEN[${IP}]="${PORT}"
    echo "port ${PORT}: ip=${IP} ip4=${IP4} warp=${WARP}"
  fi
done

echo "---------------------------------------------------------------------------"
if [ "${FAIL}" = "0" ]; then
  echo "OK: all proxies egress from unique public IPs (${#SEEN[@]} total)"
else
  echo "FAIL: issues detected (see above)"
  exit 1
fi