# opencode CLI baked into the WARP container image.
#
# Keeps the base image's /entrypoint.sh (WARP + gost SOCKS5) and adds Node.js
# with the opencode CLI, so every WARP container is also a full opencode agent
# egressing through its own unique WARP IP.
#
# The gateway builds this automatically at startup when
# UPSTREAM_PROVIDER=opencode-cli (DockerManager.ensureCLIImage). You can also
# build it yourself:
#
#   docker build -f internal/proxy/assets/opencode-warp.Dockerfile \
#     -t opencode-multi-agents/warp-opencode:latest .
FROM caomingjun/warp:latest

USER root

# Node.js 20 (LTS) from the official prebuilt binary tarball, then the CLI.
# opencode needs >= Node 20.
ARG NODE_VERSION=v20.19.4
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates xz-utils \
 && curl -fsSL "https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-x64.tar.xz" \
      | tar -xJ -C /usr/local --strip-components=1 \
 && npm install -g opencode-ai \
 && apt-get purge -y curl \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/* /root/.npm

# Make a home for the unprivileged warp user so opencode can write its
# config/session dirs when the gateway docker-execs it.
RUN mkdir -p /home/warp && chown warp:warp /home/warp

USER warp
ENV HOME=/home/warp