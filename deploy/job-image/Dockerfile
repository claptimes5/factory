# Job image: a repo's toolchain plus pinned agent CLIs.
# Build: docker build -t factory-job:latest deploy/job-image
# For another stack, build it on that stack's Debian or Ubuntu image (this
# is what `factory images` does for a repo's image_from):
#   docker build --build-arg BASE=ruby:3.4-bookworm -t factory-job-you__api deploy/job-image
ARG BASE=golang:1.27-bookworm
FROM ${BASE}
# Devcontainer images often switch to a non-root user; installs need root.
USER root

ARG CLAUDE_CODE_VERSION=2.1.302
ARG CODEX_VERSION=0.163.0
ARG GOLANGCI_LINT_VERSION=v2.14.0
ARG NODE_MAJOR=24
# Set to 1 for repos with Playwright scenarios (adds Chromium, ~400 MB).
ARG PLAYWRIGHT=0
ARG PLAYWRIGHT_VERSION=1.63.0

# Node is installed unless the base already has a recent one (the agent
# CLIs need it).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git gnupg jq ripgrep \
 && if ! node -e 'process.exit(+process.versions.node.split(".")[0] < 20)' 2>/dev/null; then \
      curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
      && apt-get install -y --no-install-recommends nodejs; \
    fi \
 && rm -rf /var/lib/apt/lists/*

RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
 && npm cache clean --force

# Playwright scenarios resolve `playwright/test` through NODE_PATH.
ENV NODE_PATH=/usr/lib/node_modules PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers
RUN if [ "$PLAYWRIGHT" = 1 ]; then \
      npm install -g "playwright@${PLAYWRIGHT_VERSION}" \
      && playwright install --with-deps chromium \
      && chmod -R a+rX /opt/pw-browsers \
      && npm cache clean --force && rm -rf /var/lib/apt/lists/*; \
    fi

# Rust images come with the minimal profile, which lacks clippy and rustfmt.
RUN if command -v rustup >/dev/null; then rustup component add clippy rustfmt; fi

RUN if command -v go >/dev/null; then \
      curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
      | sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"; \
    fi

# Jobs run as the daemon's uid, not root. /cache is where the daemon mounts
# each repo's dependency cache; a Ruby image's gem home must be writable so
# builders can bundle install.
RUN mkdir -m 1777 -p /cache \
 && if [ -d /usr/local/bundle ]; then chmod -R a+rwX /usr/local/bundle; fi

# The daemon runs containers as its own uid with a tmpfs home, so caches
# must live somewhere any uid can write.
ENV GOPATH=/tmp/go GOCACHE=/tmp/go-cache GOLANGCI_LINT_CACHE=/tmp/golangci-cache \
    CLAUDE_CODE_DISABLE_AUTOUPDATER=1 DISABLE_AUTOUPDATER=1
WORKDIR /work

# A base built from a repo's own Dockerfile may start its app through an
# entrypoint script; jobs always run their own command, through this one.
# It seeds a checkout's gems from the base's (its BUNDLE_PATH), if any.
ENV FACTORY_IMAGE_GEMS=${BUNDLE_PATH}
COPY --chmod=755 entry.sh /usr/local/bin/factory-entry
ENTRYPOINT ["/usr/local/bin/factory-entry"]
