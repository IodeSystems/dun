# dun container image — bundles the agent and all MCP servers.
# Build with: docker build -t dun:local .
#
# The image is minimal: just the binaries + a shell. The workspace is mounted
# at /work at runtime. No language toolchain is baked in — the agent installs
# what it needs (and it persists across exec calls since everything runs in
# the SAME container).

FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git curl && \
    rm -rf /var/lib/apt/lists/*

# Copy the dun toolchain binaries
COPY dun /usr/local/bin/dun
COPY poly-lsp-mcp /usr/local/bin/poly-lsp-mcp
COPY mcpshell /usr/local/bin/mcpshell
COPY raglit /usr/local/bin/raglit

WORKDIR /work

ENTRYPOINT ["dun"]
