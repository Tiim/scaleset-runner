# Default Runner Image (FR-023), built on the Host by the Controller (FR-030).
# The Controller supplies the entrypoint (FR-032), so this image needs none.
FROM ghcr.io/actions/actions-runner:latest

USER root

ARG DOCKER_VERSION=28.5.2
ARG COMPOSE_VERSION=v2.40.3

# Inner Docker daemon (FR-011) needs iptables; the SSH client serves remote
# Docker builds (FR-012).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl iptables openssh-client \
 && rm -rf /var/lib/apt/lists/*

# Docker engine and CLI, static build (the base image only ships the CLI).
RUN curl -fsSL "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_VERSION}.tgz" \
   | tar -xz -C /tmp \
 && install -m 0755 /tmp/docker/* /usr/bin/ \
 && rm -rf /tmp/docker

# Compose plugin.
RUN mkdir -p /usr/local/lib/docker/cli-plugins \
 && curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-compose \
    "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-x86_64" \
 && chmod 0755 /usr/local/lib/docker/cli-plugins/docker-compose \
 && docker compose version

RUN groupadd -f docker && usermod -aG docker runner

USER runner
WORKDIR /home/runner
