# Ubuntu 24.04 official image, linux/arm64/v8 platform manifest (not a tag lookup).
FROM docker.io/library/ubuntu:24.04@sha256:18b33ff3829707f31ada654a963b9ee35f2e6f0241b964bd17a22a9b330cb63d

ARG SOURCE_REVISION
LABEL org.opencontainers.image.source="https://github.com/r-agatsuma/mcp-remote-workspace" \
      org.opencontainers.image.revision="${SOURCE_REVISION}" \
      org.opencontainers.image.title="MCP workspace base" \
      org.opencontainers.image.base.name="docker.io/library/ubuntu:24.04" \
      org.opencontainers.image.base.digest="sha256:18b33ff3829707f31ada654a963b9ee35f2e6f0241b964bd17a22a9b330cb63d"

# Fail rather than produce an artifact with missing/ambiguous source identity.
RUN test "${#SOURCE_REVISION}" -eq 40 && \
    case "$SOURCE_REVISION" in *[!0-9a-f]*) exit 1 ;; esac

RUN apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      bash \
      build-essential \
      ca-certificates \
      coreutils \
      curl \
      fd-find \
      file \
      git \
      git-lfs \
      jq \
      less \
      openssh-client \
      patch \
      procps \
      python3 \
      python3-pip \
      python3-venv \
      ripgrep \
      rsync \
      tree \
      unzip \
      wget \
      zip && \
    ln -s /usr/bin/fdfind /usr/local/bin/fd && \
    rm -rf /var/lib/apt/lists/*

USER 0
WORKDIR /workspace
# No custom ENTRYPOINT/CMD: the runtime supplies /usr/bin/sleep infinity.
