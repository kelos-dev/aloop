FROM node:22-bookworm-slim

ARG CLAUDE_CODE_VERSION=2.1.332
ARG CODEX_VERSION=0.172.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends bubblewrap ca-certificates git tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force

ARG TARGETARCH
COPY aloop-linux-${TARGETARCH} /usr/local/bin/aloop
USER node
WORKDIR /home/node
ENTRYPOINT ["/usr/local/bin/aloop"]
