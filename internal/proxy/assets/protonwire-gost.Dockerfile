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

RUN apk --no-cache add wireguard-tools bash curl iptables

# Install gost from GitHub releases (go-gost/gost v3)
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then ARCH="amd64"; elif [ "$ARCH" = "aarch64" ]; then ARCH="arm64"; fi && \
    curl -fsSL "https://github.com/go-gost/gost/releases/download/v3.0.0/gost_3.0.0_linux_${ARCH}.tar.gz" | \
    tar xz -C /usr/local/bin gost

COPY protonwire-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
