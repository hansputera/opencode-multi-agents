# ProtonVPN WireGuard client with gost SOCKS5 proxy sidecar.
#
# Uses wireguard-tools and gost to provide a SOCKS5 proxy on port 1080,
# preserving the gateway's existing proxy routing architecture.
#
# The gateway builds this automatically at startup (DockerManager.ensureVPNImage).
# You can also build it yourself:
#
#   docker build -f internal/proxy/assets/protonwire-gost.Dockerfile \
#     -t opencode-multi-agents/protonvpn:latest .
FROM alpine:3.19

RUN apk --no-cache add wireguard-tools gost bash

COPY protonwire-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
