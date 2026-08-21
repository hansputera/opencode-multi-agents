# ProtonVPN WireGuard client with gost SOCKS5 proxy sidecar.
#
# Extends the protonwire image with gost to provide a SOCKS5 proxy on port
# 1080, preserving the gateway's existing proxy routing architecture.
#
# The gateway builds this automatically at startup (DockerManager.ensureVPNImage).
# You can also build it yourself:
#
#   docker build -f internal/proxy/assets/protonwire-gost.Dockerfile \
#     -t opencode-multi-agents/protonvpn:latest .
FROM ghcr.io/tprasadtp/protonwire:latest

USER root

# Install gost for SOCKS5 proxy
RUN apk --no-cache add gost

# Copy custom entrypoint
COPY protonwire-entrypoint.sh /usr/local/bin/protonwire-entrypoint.sh
RUN chmod +x /usr/local/bin/protonwire-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/protonwire-entrypoint.sh"]
