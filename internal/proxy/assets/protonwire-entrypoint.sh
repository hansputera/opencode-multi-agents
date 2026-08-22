#!/bin/bash
set -e

# Disable IPv6 to avoid wg-quick failures on containers without IPv6
sysctl -w net.ipv6.conf.all.disable_ipv6=1 > /dev/null 2>&1 || true
sysctl -w net.ipv6.conf.default.disable_ipv6=1 > /dev/null 2>&1 || true

# Configure WireGuard from environment variables
cat > /etc/wireguard/wg0.conf << EOF
[Interface]
PrivateKey = ${WIREGUARD_PRIVATE_KEY}
Address = ${WIREGUARD_ADDRESS}
DNS = ${WIREGUARD_DNS}

[Peer]
PublicKey = ${WIREGUARD_SERVER_PUBLIC_KEY}
Endpoint = ${WIREGUARD_ENDPOINT}
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
EOF

# Start WireGuard
wg-quick up wg0

# Start gost SOCKS5 proxy
gost -L=socks5://:1080 &
GOST_PID=$!

# Wait for gost to be ready
sleep 1

# Keep WireGuard running in foreground
wait $GOST_PID
