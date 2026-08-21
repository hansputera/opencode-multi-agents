#!/bin/sh
set -e

# Start gost SOCKS5 proxy on port 1080 in background.
# Traffic entering port 1080 is forwarded through the WireGuard tunnel.
gost -L=socks5://:1080 &
GOST_PID=$!

# Wait briefly for gost to be ready
sleep 1

# Hand off to protonwire (runs in foreground, manages the WireGuard tunnel
# and optional health-checks). If PROTONVPN_SERVER is not set, protonwire
# will exit with an error, which is the correct behaviour.
exec protonwire "$@"
