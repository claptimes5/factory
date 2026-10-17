# Job image: one per repo. Toolchain plus pinned agent CLIs.
# Build: docker build -t factory-job:latest deploy/job-image
FROM golang:1.24-bookworm

ARG CLAUDE_CODE_VERSION=2.1.302
ARG CODEX_VERSION=0.163.0
ARG GOLANGCI_LINT_VERSION=v2.5.0
ARG NODE_MAJOR=22

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git gnupg jq ripgrep \
 && curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
 && npm cache clean --force

RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
    | sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"

# The daemon runs containers as its own uid with a tmpfs home, so caches
# must live somewhere any uid can write.
ENV GOPATH=/tmp/go GOCACHE=/tmp/go-cache GOLANGCI_LINT_CACHE=/tmp/golangci-cache \
    CLAUDE_CODE_DISABLE_AUTOUPDATER=1 DISABLE_AUTOUPDATER=1
WORKDIR /work
